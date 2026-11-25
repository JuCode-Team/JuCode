// model_probe.go: synthetic availability probing at (model_group, model)
// resolution.
//
// This is the public-availability probe. It asks the question the public status
// page actually
// shows — "can a customer get an answer out of this model through this group" —
// and the only honest way to answer it is to be a customer: a real HTTP request
// to our own gateway, through ClientAuth, routing, plan checks, provider
// selection, failover and billing, exactly as anyone else's traffic goes.
//
// Consequences of that choice, all intentional:
//   - The probe costs real money. It is billed to the probe account like any
//     other request; nothing here bypasses billing, and no code in the charge
//     path knows this package exists.
//   - The availability observation is written by the gateway, not by us. We
//     fire the request and throw the response away; WriteUsageLog files the row.
//     The only thing distinguishing it is traffic_class=probe, set from the
//     context marker that middleware.resolveApiKey attaches to internal keys.
//   - It only measures the instance it runs on, since it calls its own listen
//     address over loopback. With the prober behind the leader election that is
//     one instance out of the pool.
package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entgroupaccount "github.com/jucode/jucode/internal/data/ent/groupaccount"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entgroupmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entaccountmodel "github.com/jucode/jucode/internal/data/ent/upstreamaccountmodel"

	"github.com/jucode/jucode/internal/data"
)

const (
	// modelProbeReconcileInterval is how often enabled/interval changes made in
	// the admin UI are picked up.
	modelProbeReconcileInterval = 30 * time.Second
	// modelProbeTimeout bounds one probe. Generous next to the payload — some
	// models sit behind a cold pool and a slow first token is still a success
	// from the page's point of view — but well under the gateway's own stream
	// deadlines so we never sit on a connection the gateway has given up on.
	modelProbeTimeout = 90 * time.Second
	// modelProbeMinInterval floors whatever an admin types in. Probing faster
	// than this buys no resolution the 30-minute state window can show, and
	// every tick is a paid upstream call.
	modelProbeMinInterval = 60 * time.Second
)

// ModelProbeSvc drives one goroutine per enabled (group, model) link.
type ModelProbeSvc struct {
	d            *data.Data
	apiKeys      *ApiKeySvc
	siteSettings *SettingsSvc
	logger       *slog.Logger
	baseURL      string
	hc           *http.Client

	mu       sync.Mutex
	stop     map[groupModelKey]chan struct{}
	settings map[groupModelKey]modelProbeSettings
}

// modelProbeSettings is everything about a link that, when changed, has to
// restart its loop. Compared by value in reconcile, so adding a field here is
// enough to make the watcher notice it.
type modelProbeSettings struct {
	interval        time.Duration
	disableThinking bool
	protocol        RequestProtocol
}

func NewModelProbeSvc(d *data.Data, apiKeys *ApiKeySvc, listenAddr string, logger *slog.Logger) *ModelProbeSvc {
	return &ModelProbeSvc{
		d:       d,
		apiKeys: apiKeys,
		logger:  logger,
		baseURL: loopbackBaseURL(listenAddr),
		hc: &http.Client{
			Transport: &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second},
			Timeout:   modelProbeTimeout,
		},
		stop:     map[groupModelKey]chan struct{}{},
		settings: map[groupModelKey]modelProbeSettings{},
	}
}

// loopbackBaseURL turns a listen address into something dialable. A config like
// ":8090", "0.0.0.0:8090" or "[::]:8090" means "every interface", which is not
// an address a client can connect to, so those become loopback.
//
// Parsed with net.SplitHostPort rather than a manual split on ":" — the IPv6
// wildcard form has colons inside the host, and a naive split happens to
// reassemble into a string that looks right while carrying a mangled host.
func loopbackBaseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1" + listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Start runs until ctx is cancelled. Mount it under the leader election with
// the other background loops: two instances probing the same links would double
// the spend and file duplicate observations.
func (s *ModelProbeSvc) Start(ctx context.Context) {
	s.reconcile(ctx)
	go func() {
		t := time.NewTicker(modelProbeReconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.stopAll()
				return
			case <-t.C:
				s.reconcile(ctx)
			}
		}
	}()
}

type modelProbeTarget struct {
	key      groupModelKey
	settings modelProbeSettings
}

// desiredTargets lists the links that should currently be probed. A link counts
// only when the link, its group and its model are all enabled — a disabled
// model still has rows, and probing it would bill us for something no customer
// can reach.
func (s *ModelProbeSvc) desiredTargets(ctx context.Context) ([]modelProbeTarget, error) {
	links, err := s.d.Ent.ModelGroupModel.Query().
		Where(
			entgroupmodel.ProbeEnabledEQ(true),
			entgroupmodel.HasGroupWith(entgroup.EnabledEQ(true)),
			entgroupmodel.HasModelWith(entmodel.EnabledEQ(true)),
		).
		WithModel().
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]modelProbeTarget, 0, len(links))
	for _, link := range links {
		if link.Edges.Model == nil {
			continue
		}
		interval := time.Duration(link.ProbeIntervalSeconds) * time.Second
		if interval < modelProbeMinInterval {
			interval = modelProbeMinInterval
		}
		protocol, err := s.probeProtocol(ctx, link.GroupID, link.Edges.Model.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, modelProbeTarget{
			key: groupModelKey{GroupID: link.GroupID, Model: link.Edges.Model.Name},
			settings: modelProbeSettings{
				interval:        interval,
				disableThinking: link.ProbeDisableThinking,
				protocol:        protocol,
			},
		})
	}
	return out, nil
}

// probeProtocol chooses the client-facing wire format that can actually reach
// this model through the selected group. OpenAI is preferred when both formats
// are available; Anthropic is used for groups whose only enabled routes are
// Anthropic providers. Without this selection, a healthy Anthropic-only group
// is incorrectly reported as 503 because the probe always entered the OpenAI
// resolver.
func (s *ModelProbeSvc) probeProtocol(ctx context.Context, groupID uuid.UUID, model string) (RequestProtocol, error) {
	links, err := s.d.Ent.GroupAccount.Query().
		Where(entgroupaccount.GroupIDEQ(groupID), entgroupaccount.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return RequestProtocolOpenAI, err
	}
	conversionEnabled := false
	if s.siteSettings != nil {
		if site, err := s.siteSettings.GetSite(ctx); err == nil && site != nil {
			conversionEnabled = site.FormatConversionEnabled
		}
	}
	hasAnthropic := false
	for _, link := range links {
		acct, err := s.d.Ent.UpstreamAccount.Get(ctx, link.AccountID)
		if err != nil || acct.DeletedAt != nil || acct.Status != entaccount.StatusActive || !acct.Schedulable {
			continue
		}
		exists, err := s.d.Ent.UpstreamAccountModel.Query().
			Where(
				entaccountmodel.AccountIDEQ(acct.ID),
				entaccountmodel.ModelNameEQ(model),
				entaccountmodel.EnabledEQ(true),
			).
			Exist(ctx)
		if err != nil {
			return RequestProtocolOpenAI, err
		}
		if !exists {
			continue
		}
		if AccountSupportsRequestProtocol(acct, RequestProtocolOpenAI, conversionEnabled) {
			return RequestProtocolOpenAI, nil
		}
		if AccountSupportsRequestProtocol(acct, RequestProtocolAnthropic, conversionEnabled) {
			hasAnthropic = true
		}
	}
	if hasAnthropic {
		return RequestProtocolAnthropic, nil
	}
	// Keep the old failure semantics when no enabled mapping exists: the
	// request will enter the OpenAI resolver and record the useful 503 body.
	return RequestProtocolOpenAI, nil
}

func (s *ModelProbeSvc) reconcile(ctx context.Context) {
	targets, err := s.desiredTargets(ctx)
	if err != nil {
		s.logger.Warn("model probe reconcile failed", "err", err)
		return
	}
	desired := make(map[groupModelKey]modelProbeSettings, len(targets))
	for _, t := range targets {
		desired[t.key] = t.settings
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Drop links that were disabled or whose settings changed; the changed ones
	// are re-armed below with the new settings.
	for key, stop := range s.stop {
		if want, ok := desired[key]; !ok || want != s.settings[key] {
			close(stop)
			delete(s.stop, key)
			delete(s.settings, key)
		}
	}
	for key, set := range desired {
		if _, running := s.stop[key]; running {
			continue
		}
		stop := make(chan struct{})
		s.stop[key] = stop
		s.settings[key] = set
		go s.runLoop(ctx, key, set, stop)
	}
}

func (s *ModelProbeSvc) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, stop := range s.stop {
		close(stop)
		delete(s.stop, key)
		delete(s.settings, key)
	}
}

func (s *ModelProbeSvc) runLoop(ctx context.Context, key groupModelKey, set modelProbeSettings, stop chan struct{}) {
	// Stagger the first tick across links so a restart does not fire every
	// probe at once — that would spike the upstreams and bunch all the samples
	// into one corner of the state window.
	jitter := time.Duration(uuid.New().ID()%uint32(set.interval/time.Millisecond)) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-stop:
		return
	case <-time.After(jitter):
	}

	t := time.NewTicker(set.interval)
	defer t.Stop()
	s.runOne(ctx, key, set)
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			s.runOne(ctx, key, set)
		}
	}
}

// probeBody is the smallest request that still exercises the whole path.
//
// max_tokens=1 does not bound a reasoning model: measured against production,
// claude honours it and returns one token, while glm-5.2 and kimi-k3 ignore it
// for their thinking block and return 126-129. That block is where essentially
// all of a probe's cost goes.
//
// disableThinking adds the vendor switches that suppress it. They are opt-in
// per link rather than always-on because they are neither universally
// understood nor universally tolerated — measured: glm-5.2 drops 129 output
// tokens to 14 and kimi-k3 126 to 56, deepseek-v4-flash ignores them entirely,
// and xAI answers 400. A 400 would file the channel as bad and redden the
// status page, so the flag stays off until a link has been measured.
func probeBody(model string, disableThinking bool) ([]byte, error) {
	body := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	}
	if disableThinking {
		// Four spellings for the same intent, one per vendor family. Unknown
		// keys are ignored by the vendors that do not use them — which is
		// exactly why the flag must be measured per link before being set.
		body["thinking"] = map[string]any{"type": "disabled"}
		body["enable_thinking"] = false
		body["reasoning_effort"] = "none"
		body["reasoning"] = map[string]any{"enabled": false, "effort": "none"}
	}
	return json.Marshal(body)
}

func (s *ModelProbeSvc) runOne(ctx context.Context, key groupModelKey, set modelProbeSettings) {
	secret, err := s.probeSecret(ctx)
	if err != nil {
		// Without a key there is nothing to probe with. Loud, because the
		// symptom on the page is silence: state simply stays unknown.
		s.logger.Error("model probe has no usable internal api key", "err", err)
		return
	}
	body, err := probeBody(key.Model, set.disableThinking)
	if err != nil {
		s.logger.Error("model probe body", "model", key.Model, "err", err)
		return
	}

	reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelProbeTimeout)
	defer cancel()
	path := "/v1/chat/completions"
	if set.protocol == RequestProtocolAnthropic {
		path = "/anthropic/v1/messages"
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		s.logger.Error("model probe request", "group_id", key.GroupID, "model", key.Model, "protocol", set.protocol, "path", path, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	// Pins routing to this exact group, so a model shared by several groups is
	// measured once per group rather than collapsing onto whichever the router
	// happened to pick. Same header the admin group-override uses.
	req.Header.Set("X-JuCode-Group", key.GroupID.String())

	started := time.Now()
	resp, err := s.hc.Do(req)
	if err != nil {
		// The gateway has already filed an observation for anything that got
		// far enough to be routed; a transport error here usually means we
		// never reached ourselves at all, which is worth saying out loud.
		s.logger.Warn("model probe transport error",
			"group_id", key.GroupID, "model", key.Model, "protocol", set.protocol, "path", path,
			"err", err, "elapsed", time.Since(started))
		return
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusOK {
		return
	}
	// 402/403/429 on the probe account means WE are broken, not the channel:
	// an empty balance, a plan that excludes the group, a cost limit. The
	// gateway records those as excluded rather than bad, so they do not turn
	// the page red — they make it go quiet instead, which is harder to notice.
	if isProbeAccountFault(resp.StatusCode) {
		s.logger.Error("model probe rejected by our own billing — the probe account needs attention",
			"group_id", key.GroupID, "model", key.Model,
			"protocol", set.protocol, "path", path,
			"status", resp.StatusCode, "body", string(payload))
		return
	}
	s.logger.Info("model probe failed",
		"group_id", key.GroupID, "model", key.Model,
		"protocol", set.protocol, "path", path,
		"status", resp.StatusCode, "elapsed_ms", time.Since(started).Milliseconds(),
		"body", strings.TrimSpace(string(payload)))
}

func isProbeAccountFault(status int) bool {
	switch status {
	case http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	}
	return false
}

// probeSecret finds the platform's own API key. Looked up rather than
// configured so there is no second place for the secret to live: mark exactly
// one key internal and the prober adopts it.
func (s *ModelProbeSvc) probeSecret(ctx context.Context) (string, error) {
	rec, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.InternalEQ(true), entapikey.RevokedAtIsNil()).
		Order(ent.Asc(entapikey.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		return "", err
	}
	return s.apiKeys.Reveal(ctx, rec.ID, uuid.Nil)
}
