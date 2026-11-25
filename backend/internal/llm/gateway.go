package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/metrics"
	"github.com/jucode/jucode/internal/middleware"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
)

// promoteSticky moves the provider with id == pinned to the front of its
// priority tier, preserving the relative order of the rest. A pinned fallback
// tier must not jump ahead of a currently higher-priority provider.
func promoteSticky(ps []biz.RouteAccount, pinned uuid.UUID) []biz.RouteAccount {
	if len(ps) == 0 {
		return ps
	}
	topPriority := ps[0].Priority
	idx := -1
	for i := range ps {
		if ps[i].Account != nil && ps[i].Account.ID == pinned {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return ps
	}
	if ps[idx].Priority != topPriority {
		return ps
	}
	out := make([]biz.RouteAccount, 0, len(ps))
	out = append(out, ps[idx])
	out = append(out, ps[:idx]...)
	out = append(out, ps[idx+1:]...)
	return out
}

// Gateway is the request handler for the OpenAI-compatible endpoints.
type Gateway struct {
	models         *biz.ModelSvc
	billing        *biz.BillingSvc
	concur         *biz.ConcurrencyGate
	sticky         *biz.StickySvc
	policy         *biz.PolicySvc
	incidents      *biz.IncidentSvc
	settings       *biz.SettingsSvc // for AllowImageOutput gating
	cipher         *cryptopkg.Cipher
	maxAttempts    int
	maxBodyBytes   int64
	requestTimeout time.Duration
	moderation     *biz.ModerationSvc // optional: nil when llm.moderation.enabled = false
	tasks          *biz.TaskSvc       // optional: async generation (video) task store; nil = video 501s
	rateLimit      *biz.RateLimitGate // optional: nil disables RPM/TPM caps
	// accounts is the account pool: the routing entity, and the owner of every
	// upstream credential, health signal and scheduling input.
	accounts *biz.AccountPoolSvc
	// webTools serves /tools/v1 (see web_tools.go); nil answers 503.
	webTools *biz.WebToolSvc

	providerMu     sync.Mutex
	accountClients map[uuid.UUID]accountClientEntry
}

// accountClientEntry is a cached transport for one account, invalidated when
// the account row changes (protocol, base URL).
type accountClientEntry struct {
	updatedAt time.Time
	client    *http.Client
}

// SetAccounts wires the account pool. Kept as a setter for the same reason as
// SetTaskSvc: NewGateway already takes many arguments.
func (g *Gateway) SetAccounts(svc *biz.AccountPoolSvc) { g.accounts = svc }

// accountClient returns the HTTP client shared by one account: the transport
// (and its connection pool) is per account, while the credential is attached
// per attempt. The cache follows the account row, so an edit takes effect on
// the next request rather than at the next restart.
func (g *Gateway) accountClient(a *ent.UpstreamAccount) *http.Client {
	g.providerMu.Lock()
	defer g.providerMu.Unlock()
	if g.accountClients == nil {
		g.accountClients = make(map[uuid.UUID]accountClientEntry)
	}
	if entry, ok := g.accountClients[a.ID]; ok && entry.updatedAt.Equal(a.UpdatedAt) {
		return entry.client
	}
	client := &http.Client{Transport: newProviderTransport()}
	g.accountClients[a.ID] = accountClientEntry{updatedAt: a.UpdatedAt, client: client}
	return client
}

// SetTaskSvc wires the async generation task store (for /v1/videos). Kept as
// a setter so NewGateway's signature — and its single caller — are untouched;
// when nil, video endpoints return 501.
func (g *Gateway) SetTaskSvc(t *biz.TaskSvc) { g.tasks = t }

// SetModeration wires the optional async LLM moderation service. nil leaves
// moderation disabled (MaybeReview is nil-safe).
func (g *Gateway) SetModeration(m *biz.ModerationSvc) { g.moderation = m }

// SetRateLimitGate wires the per-user RPM/TPM caps. Kept as a setter for the
// same reason as SetTaskSvc: NewGateway already takes 14 arguments. nil (or a
// gate in "off" mode) leaves the generation endpoints governed by the
// concurrency gate alone, which is the behavior that predates this.
func (g *Gateway) SetRateLimitGate(rl *biz.RateLimitGate) { g.rateLimit = rl }

// providerCacheKey and providerCacheEntry used to cache one upstream client
// per (channel, wire protocol). Clients are now built per attempt from the
// account's own credential (see buildProviderForAttempt), so there is nothing
// to cache but the transport: accountClient keeps one per account.

const (
	// 90s sits above observed TTFT p95 (~53s) so legitimate slow first
	// tokens aren't killed, while still bounding pre-first-byte hangs.
	defaultStreamFirstTextTimeout       = 90 * time.Second
	minImageGenerationStreamIdleTimeout = 300 * time.Second
	// maxSameAccountRetries bounds the lenient-pool-mode "retry the same
	// account" loop per candidate, so a sick account cannot monopolize the
	// request's attempt budget.
	maxSameAccountRetries = 2
)

func NewGateway(models *biz.ModelSvc, billing *biz.BillingSvc, concur *biz.ConcurrencyGate, sticky *biz.StickySvc, policy *biz.PolicySvc, incidents *biz.IncidentSvc, settings *biz.SettingsSvc, cipher *cryptopkg.Cipher, maxAttempts int, maxBodyBytes int64, requestTimeout time.Duration) *Gateway {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return &Gateway{models: models, billing: billing, concur: concur, sticky: sticky, policy: policy, incidents: incidents, settings: settings, cipher: cipher, maxAttempts: maxAttempts, maxBodyBytes: maxBodyBytes, requestTimeout: requestTimeout}
}

// HandleChatCompletions implements POST /v1/chat/completions.
func (g *Gateway) HandleChatCompletions(c *gin.Context) {
	g.handle(c, "/v1/chat/completions", entusagelog.EndpointChatCompletions, true /*injectIncludeUsage*/)
}

// HandleResponses implements POST /v1/responses. Upstream always streams;
// clients that pass stream:false get the stream aggregated back into a
// single JSON response object.
func (g *Gateway) HandleResponses(c *gin.Context) {
	g.handle(c, "/v1/responses", entusagelog.EndpointResponses, false /*injectIncludeUsage*/)
}

// HandleResponsesCompact implements POST /v1/responses/compact. Same wire
// format as /responses (SSE in, SSE out), but the upstream is Codex's
// summarization variant — it compresses long conversation histories into
// a shorter response.
// KindResponsesCompact; we just forward the path so the upstream mux hits
// the right handler. Logged with endpoint=responses to keep the
// usage_logs enum stable (one of "chat_completions" / "responses"); the
// compact distinction is recoverable from the request URL in the access
// log if needed.
func (g *Gateway) HandleResponsesCompact(c *gin.Context) {
	g.handle(c, "/v1/responses/compact", entusagelog.EndpointResponses, false /*injectIncludeUsage*/)
}

func (g *Gateway) handle(c *gin.Context, upstreamPath string, ep entusagelog.Endpoint, injectChatIncludeUsage bool) {
	startedAt := time.Now()
	requestID := gatewayRequestID(c)
	// A streaming reply commits its 200 status line on the first accepted
	// attempt; model output starting (streamBodyStarted) is what ends
	// failover, not the header (see streamPrelude).
	downstreamCommitted, streamBodyStarted := false, false
	writeErr := func(status int, kind, code, message string) {
		if downstreamCommitted {
			// An earlier attempt already sent the SSE status line; a later
			// one failed before any output. End the stream in-band.
			writeStreamAbortTermination(c.Writer, string(ep), requestID, "")
			return
		}
		c.JSON(status, openAIError(kind, code, message, requestID))
	}
	user, ok := middleware.UserFrom(c)
	if !ok {
		writeErr(http.StatusUnauthorized, "authentication_error", "missing_user", "no user")
		return
	}
	apiKey, _ := middleware.ApiKeyFrom(c)
	lc := logContext{
		IP:              c.ClientIP(),
		UserAgent:       c.Request.UserAgent(),
		ApiKeyName:      keyName(apiKey),
		AuthorizationID: middleware.AuthzIDFrom(c),
		TurnID:          agentTurnIDFrom(c),
		Latency:         newLatencyTracker(startedAt),
	}
	c.Writer = newTTFBResponseWriter(c.Writer, func() {
		if lc.TtfbMs != nil {
			return
		}
		d := int(time.Since(startedAt).Milliseconds())
		lc.TtfbMs = &d
	})

	var raw []byte
	var err error
	stageStart := time.Now()
	if g.maxBodyBytes > 0 {
		raw, err = io.ReadAll(io.LimitReader(c.Request.Body, g.maxBodyBytes+1))
		if err == nil && int64(len(raw)) > g.maxBodyBytes {
			writeErr(http.StatusRequestEntityTooLarge, "invalid_request_error", "request_body_too_large", "request body too large")
			return
		}
	} else {
		raw, err = io.ReadAll(c.Request.Body)
	}
	if err != nil {
		writeErr(http.StatusBadRequest, "invalid_request_error", "read_body_failed", "failed to read request body")
		return
	}
	lc.Latency.Duration("body_read_ms", time.Since(stageStart))
	lc.Latency.Value("body_bytes", len(raw))
	if c.Request.ContentLength >= 0 {
		lc.Latency.Value("content_length_bytes", int(c.Request.ContentLength))
	}
	var body map[string]any
	stageStart = time.Now()
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(http.StatusBadRequest, "invalid_request_error", "invalid_json", "body must be valid JSON")
		return
	}
	lc.Latency.Duration("json_decode_ms", time.Since(stageStart))
	modelName, _ := body["model"].(string)
	clientStream, _ := body["stream"].(bool)
	stream := clientStream
	// Responses endpoint is always streamed upstream.
	// Force streaming in the forwarded body too, not just locally: passing
	// the client's original stream:false through made providers
	// answer with a plain JSON body, which the streaming pipe below misread
	// as an incomplete stream (and tacked a bogus stream-abort frame onto).
	if ep == entusagelog.EndpointResponses {
		stream = true
		body["stream"] = true
	}
	// Client asked /v1/responses with stream:false (or omitted it): we still
	// stream upstream, but buffer the terminal response frame and return it
	// as a single application/json object instead of SSE.
	aggregateResponses := ep == entusagelog.EndpointResponses && !clientStream
	if modelName == "" {
		writeErr(http.StatusBadRequest, "invalid_request_error", "missing_model", "missing model")
		return
	}
	reqCtx := c.Request.Context()
	if !stream && g.requestTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, g.requestTimeout)
		defer cancel()
	}

	lc.ReasoningEffort = g.extractReasoningEffort(reqCtx, body)
	lc.Stream = stream

	// Policy moderation. The Aho-Corasick scan over the raw body is
	// O(n+matches) where n is the request bytes; empty dictionary
	// short-circuits with no allocation, so the cost is paid only when
	// operators have actually loaded a wordlist. We scan the JSON-
	// encoded body but replace every image payload with a single
	// "<image>" sentinel first — base64 image data can be MB-scale
	// (wastes scanner time) and has a non-zero chance of containing
	// substrings that happen to match a policy keyword by accident.
	// Text content (messages / system / tool args) is preserved
	// verbatim; JSON punctuation between fields can't itself match a
	// keyword.
	stageStart = time.Now()
	scan := g.policy.Scan(biz.RedactImagesForScan(raw))
	lc.Latency.Duration("policy_scan_ms", time.Since(stageStart))
	// Graded decision: a single low/medium keyword no longer hard-blocks
	// (see PolicyScanResult.BlockDecision) — it is recorded as "logged" and,
	// together with soft suspects + sampled clean traffic, routed to async
	// LLM review which can confirm it and drive account sanctions.
	block, topSev := scan.BlockDecision()
	uid := user.ID
	if len(scan.Hits) > 0 {
		cats := make([]string, 0, len(scan.Hits))
		for _, h := range scan.Hits {
			cats = append(cats, h.Slug)
		}
		action := "logged"
		msg := "policy categories (logged): " + strings.Join(cats, ",")
		payload := biz.PolicyIncidentPayload(scan.Hits, scan.Truncated)
		if block {
			action = "blocked"
			msg = "blocked by policy categories: " + strings.Join(cats, ",")
		}
		g.incidents.RecordAsync(biz.IncidentInput{
			UserID:    &uid,
			ApiKeyID:  apiKeyID(apiKey),
			Kind:      "policy_keyword",
			Severity:  topSev,
			Action:    action,
			Model:     modelName,
			Endpoint:  string(ep),
			RequestID: requestID,
			IP:        c.ClientIP(),
			Message:   msg,
			Payload:   payload,
		})
		if block {
			body := openAIError("content_policy_violation", "input_policy_block", "blocked by input moderation policy", requestID)
			body["error"].(map[string]any)["categories"] = cats
			c.JSON(http.StatusUnprocessableEntity, body)
			return
		}
	}
	// Async LLM moderation: confirm suspects/logged hits and sample clean
	// traffic to catch dictionary misses. Never blocks the hot path.
	g.moderation.MaybeReview(biz.ModerationRequest{
		UserID:    uid,
		ApiKeyID:  apiKeyID(apiKey),
		Model:     modelName,
		Endpoint:  string(ep),
		RequestID: requestID,
		IP:        c.ClientIP(),
		Text:      biz.RedactImagesForScan(raw),
		Scan:      scan,
	})

	// Route -> group -> accounts.
	// When format conversion is on, any account that can be rewritten to
	// is eligible; when off, keep today's protocol-filtered routing.
	clientDialect := DialectCompatible
	clientProtocol := biz.RequestProtocolCompatible
	if ep == entusagelog.EndpointResponses {
		clientDialect = DialectOpenAI
		clientProtocol = biz.RequestProtocolResponses
	}
	resolveProtocol := biz.RequestProtocolOpenAI
	convEnabled := g.formatConversionEnabled(c)
	if convEnabled {
		resolveProtocol = biz.RequestProtocolAny
	}
	stageStart = time.Now()
	convID := ClientSessionAnchor(c.Request.Header, body)
	reqCtx = g.withStickyGroup(reqCtx, user.ID, modelName, convID)
	route, err := g.models.Resolve(reqCtx, modelName, user, apiKey, resolveProtocol)
	lc.Latency.Duration("route_ms", time.Since(stageStart))
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrModelNotFound):
			writeErr(http.StatusBadRequest, "invalid_request_error", "model_not_found", "model not found: "+modelName)
		case errors.Is(err, biz.ErrModelNotInGroup):
			writeErr(http.StatusBadRequest, "invalid_request_error", "model_group_unavailable", routeErrorMessage(modelName, err))
		case errors.Is(err, biz.ErrGroupDeniedForUser):
			writeErr(http.StatusForbidden, "permission_error", "access_denied", err.Error())
		case errors.Is(err, biz.ErrNoProvider):
			g.recordNoProviderAvailability(reqCtx, err, requestID, modelName, ep, startedAt)
			writeErr(http.StatusServiceUnavailable, "service_unavailable", "no_available_provider", routeErrorMessage(modelName, err))
		default:
			writeErr(http.StatusInternalServerError, "api_error", "internal_error", "internal server error")
		}
		return
	}

	// Routing shape for the usage log: how many groups were on the table and
	// which one auto-route took. Only written when there was a real choice,
	// so single-group traffic adds no noise.
	if route.CandidateGroups > 1 {
		lc.Latency.Value("route_candidate_groups", route.CandidateGroups)
		lc.Latency.Value("route_group_index", route.GroupIndex)
	}

	// Access gates: user_group → model_group + api_key scope → model_group.
	stageStart = time.Now()
	if err := g.models.CheckAccess(reqCtx, user, apiKey, route.Group); err != nil {
		lc.Latency.Duration("access_check_ms", time.Since(stageStart))
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, biz.AvailabilityFailureAccess, http.StatusForbidden, startedAt)
		writeErr(http.StatusForbidden, "permission_error", "access_denied", err.Error())
		return
	}
	lc.Latency.Duration("access_check_ms", time.Since(stageStart))

	// Billing pre-check.
	stageStart = time.Now()
	planState, err := g.billing.ResolvePlanState(reqCtx, user, apiKey, route.Group)
	lc.Latency.Duration("billing_precheck_ms", time.Since(stageStart))
	if err != nil {
		status := http.StatusPaymentRequired
		switch {
		case errors.Is(err, biz.ErrUnsupportedBillingSource):
			status = http.StatusServiceUnavailable
		case errors.Is(err, biz.ErrPlanNotAllowed):
			status = http.StatusForbidden
		case errors.Is(err, biz.ErrPlanQuotaExhausted):
			status = http.StatusTooManyRequests
		case errors.Is(err, biz.ErrMonthlySpendLimitExceeded), errors.Is(err, biz.ErrApiKeyCostLimitExceeded):
			status = http.StatusTooManyRequests
		case errors.Is(err, biz.ErrOrganizationLimit), errors.Is(err, biz.ErrMemberLimit):
			status = http.StatusTooManyRequests
		}
		writeErr(status, "billing_error", "billing_rejected", err.Error())
		// Record a quota_exceeded log so admin can see the rejection.
		var groupID *uuid.UUID
		if route != nil {
			groupID = &route.Group.ID
		}
		// Classify off the status just computed, not a second list of error
		// values. The two lists drifted: the switch above routes every
		// user-side rejection to a 4xx, but this whitelist named only three
		// errors, so ErrInsufficientBalance — by far the most common one —
		// fell through and logged a user with no money as an internal_error.
		// Measured at 465 rows in 6h, 1.6% of all traffic, which is also why
		// availability.go carries an explicit 4xx workaround for this status.
		// A 4xx here is the caller; only a 5xx is ours.
		st := entusagelog.StatusQuotaExceeded
		if status >= 500 {
			st = entusagelog.StatusInternalError
		}
		if _, werr := g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
			UserID: user.ID, ApiKeyID: apiKeyID(apiKey), RequestID: requestID,
			OrganizationID: organizationID(apiKey),
			Endpoint:       ep, Model: modelName, ModelID: &route.Model.ID, GroupID: groupID,
			Status: st, HTTPStatus: status, LatencyMs: int(time.Since(startedAt).Milliseconds()),
			ErrorMessage: err.Error(), StartedAt: startedAt,
			IP: lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
			Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
			LatencyDetail: lc.Latency.Snapshot(),
		}); werr != nil {
			slog.Warn("precheck-reject log write failed", "err", werr)
		}
		return
	}

	// Concurrency gate. Pick limit source based on group.BillingSource:
	// balance_only → user_group limit, plan_only → active-plan limit.
	// release is never nil; deferred so retries across providers count as
	// one in-flight slot (not N).
	stageStart = time.Now()
	release, err := g.concur.AcquireWithPlanState(reqCtx, user, route.Group, planState)
	lc.Latency.Duration("concurrency_ms", time.Since(stageStart))
	if err != nil {
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, "concurrency_limit", http.StatusTooManyRequests, startedAt)
		writeErr(http.StatusTooManyRequests, "rate_limit_error", "concurrency_limit", err.Error())
		return
	}
	defer release()

	// Peek phase of the rate-limit protocol: read-only, consumes nothing.
	// Commit happens at the "2xx success" point below, settle in chargeAndLog.
	stageStart = time.Now()
	ticket, rlErr := g.rateLimit.Peek(reqCtx, user, route.Group, planState)
	lc.Latency.Duration("rate_limit_ms", time.Since(stageStart))
	lc.RateTicket = ticket
	if rlErr != nil {
		code := "rpm_limit"
		if errors.Is(rlErr, biz.ErrTPMLimit) {
			code = "tpm_limit"
		}
		// No upstream header to honor here — the cap is ours, so hint the
		// time until the current 60s window rolls over.
		setRetryAfter(c, nil, rateLimitRetryAfter)
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, code, http.StatusTooManyRequests, startedAt)
		writeErr(http.StatusTooManyRequests, "rate_limit_error", code, rlErr.Error())
		return
	}

	// Reserve BEFORE any provider is contacted. Above the 0.25 water-line
	// with enough cash this freezes the estimate; otherwise last-request
	// freezes remaining cash and serializes llm:/ws: holds. Exact settle
	// may take the balance negative; CapAtHold (cancel reconstruct) does
	// not. Plan-quota groups get a pseudo-reservation that preserves their
	// "last request may cross the line" contract. Every terminal path after
	// this point finishes the hold: chargeAndLog settles it, logFinal
	// settles it on last-hop observed usage (499, truncated stream,
	// timeout, response.failed) and releases it otherwise, and the two
	// direct-return paths in the provider loop release explicitly.
	stageStart = time.Now()
	estInput := biz.CostEstimateInput{
		BodyBytes:       len(raw),
		MaxOutputTokens: requestedMaxOutputTokens(body),
	}
	estCost := g.billing.EstimateMaxCost(reqCtx, route.Model, route.Group, estInput)
	reservation, resErr := g.billing.Reserve(reqCtx, biz.ReserveInput{
		User: user, ApiKey: apiKey, Group: route.Group, PlanState: planState,
		Key: newBillingHoldKey("llm"), Amount: estCost, TTL: biz.ReserveTTLRequest,
		Note: "llm gateway reserve " + requestID,
	})
	lc.Latency.Duration("billing_reserve_ms", time.Since(stageStart))
	if resErr != nil {
		// Fail closed: without a hold the last-request / overdraft rules
		// do not apply and the request must not contact upstream.
		// User-side rejections get a 402/429; anything else (DB fault)
		// is a 503.
		status := http.StatusPaymentRequired
		kind, code := "billing_error", "insufficient_balance"
		switch {
		case errors.Is(resErr, biz.ErrInsufficientBalance):
			// keep 402
		case errors.Is(resErr, biz.ErrOrganizationLimit), errors.Is(resErr, biz.ErrMemberLimit):
			status, code = http.StatusTooManyRequests, "spend_limit_exceeded"
		default:
			status, kind, code = http.StatusServiceUnavailable, "api_error", "billing_reserve_failed"
		}
		writeErr(status, kind, code, resErr.Error())
		st := entusagelog.StatusQuotaExceeded
		if status >= 500 {
			st = entusagelog.StatusInternalError
		}
		if _, werr := g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
			UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID,
			OrganizationID: organizationID(apiKey),
			Endpoint:       ep, Model: modelName, ModelID: &route.Model.ID, GroupID: &route.Group.ID,
			Status: st, HTTPStatus: status, LatencyMs: int(time.Since(startedAt).Milliseconds()),
			ErrorMessage: "billing reserve: " + resErr.Error(), StartedAt: startedAt,
			IP: lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
			Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
			LatencyDetail: lc.Latency.Snapshot(),
		}); werr != nil {
			slog.Warn("reserve-reject log write failed", "err", werr)
		}
		return
	}
	lc.Reservation = reservation

	// For Chat stream requests, inject stream_options.include_usage = true
	// so the upstream's final non-data frame contains the usage block.
	if stream && injectChatIncludeUsage {
		if opts, ok := body["stream_options"].(map[string]any); ok {
			opts["include_usage"] = true
		} else {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	acc := &Accumulator{}
	providerChain := make([]string, 0, len(route.Providers))
	var (
		usedRP       biz.RouteAccount
		usedUpstream string
		retryCount   int
		lastErr      error
		lastStatus   int
		ttftMs       *int // populated when the first semantic output token arrives
	)
	attempts := &usageAttemptRecorder{endpoint: string(ep), group: route.Group.ID.String()}
	syncAttempts := func() {
		providerChain = attempts.Chain()
		retryCount = attempts.RetryCount()
	}
	markFirstTextTimeout := func() {
		d := int(time.Since(startedAt).Milliseconds())
		lc.Latency.Value("first_token_timeout_ms", d)
	}

	// Sticky / bound account affinity: if (user, group) or a client session
	// header / prompt_cache_key was pinned to an account in the recent past,
	// try that one first. This makes short bursts of follow-up requests land on
	// the same account so its upstream prompt cache stays warm. Codex clients
	// usually send session_id rather than conversation_id.
	route.Providers = g.orderRouteAccounts(reqCtx, user, apiKey, route.Group.ID, convID, modelName, route.Providers)

	// capacityDrops counts candidates that could not even be attempted because
	// the account was at its concurrency cap. If the request ends with no
	// upstream attempt and no other error, that count turns the answer into
	// backpressure instead of "unreachable".
	var capacityDrops int
	// protocolSkips counts candidates skipped because they cannot speak the
	// client's API format. If nothing else happened, the request is answered
	// as a client error naming the endpoints that do work: retrying the same
	// format can never succeed.
	var protocolSkips int

	recordStickyFailure := func(rp biz.RouteAccount, reason string) {
		if rp.Account == nil || g.accounts == nil {
			return
		}
		detached, err := g.accounts.RecordStickyFailure(c.Request.Context(), user.ID, route.Group.ID, rp.Account.ID, convID, 2)
		if err != nil {
			slog.Debug("sticky.account_failure_record_failed", "err", err.Error())
			return
		}
		if detached {
			slog.Info("sticky.account_detached_after_failures",
				"account_id", rp.Account.ID.String(),
				"reason", reason,
			)
		}
	}
	// recordAccountFailure feeds a failure class into the account health state
	// machine, which picks cooldown vs disable from the class and the pool mode
	// (and may decide to do nothing at all). It never cools anything above the
	// account: a sibling account is still healthy, and one bad credential must
	// not take the whole pool down.
	recordAccountFailure := func(rp biz.RouteAccount, reason string) biz.AccountFailureOutcome {
		if rp.Account == nil || g.accounts == nil {
			return biz.AccountFailureOutcome{}
		}
		outcome := g.accounts.ReportAccountFailure(c.Request.Context(), rp.Account, biz.AccountFailure{
			Class:  accountFailureClass(reason),
			Reason: reason,
			Model:  modelName,
		})
		if outcome.Action != biz.ActionNone {
			g.accounts.RecordOutcome(c.Request.Context(), rp.Account.ID, false)
		}
		return outcome
	}
	// recordUpstreamFailure carries the full upstream signal (class, status,
	// headers, body) into the account state machine, so a 429 parks the account
	// until the window it advertises actually resets, and a rejected credential
	// disables it instead of cooling it for two minutes.
	//
	// It returns whether the state machine asked for the same account to be
	// retried (lenient pool mode): the attempt loop uses that to try the same
	// account again before moving on.
	recordUpstreamFailure := func(rp biz.RouteAccount, cls errorClass, status int, h http.Header, body string) biz.AccountFailureOutcome {
		if rp.Account == nil || g.accounts == nil {
			return biz.AccountFailureOutcome{}
		}
		outcome := g.accounts.ReportAccountFailure(c.Request.Context(), rp.Account, biz.AccountFailure{
			Class:   string(cls),
			Status:  status,
			Reason:  fmt.Sprintf("status_%d", status),
			Headers: h,
			Body:    body,
			Model:   modelName,
		})
		if outcome.Action != biz.ActionNone {
			g.accounts.RecordOutcome(c.Request.Context(), rp.Account.ID, false)
		}
		return outcome
	}
	streamCtx := reqCtx
	var firstTextTimeout *streamFirstTextTimeout
	var totalTimeout *streamTotalTimeout
	if stream {
		// Total wall-clock cap wraps the whole request (shared across all
		// attempts); first-token timeout layers on top of it.
		streamCtx, totalTimeout = withStreamTotalTimeout(streamCtx, g.streamTotalTimeout(c.Request.Context()))
		defer totalTimeout.Stop()
		streamCtx, firstTextTimeout = withStreamFirstTextTimeout(streamCtx, g.streamFirstTextTimeout(c.Request.Context()))
		defer firstTextTimeout.Stop()
	}

	// Cross-group retry (product rule D8=B): when every provider of the
	// preferred group fails before anything reached the client, the request
	// may move to the next group with the SAME billing_source within the
	// key's auto_route_max_rate cap. Image-generation-style requests are
	// excluded by default. Candidates load lazily on the failure path only.
	mediaRequest := isLikelyImageGenerationRequest(modelName, "", body)
	failover := newGroupFailover(g, user, apiKey, resolveProtocol, requestID, estInput, mediaRequest)
	// moreProviders is the "somewhere left to try" lookahead used by the
	// mid-stream failure paths: either another provider in the current
	// group, or an untried cross-group fallback.
	moreProviders := func(i int) bool {
		return i+1 < len(route.Providers) || failover.available(reqCtx, route)
	}

	// upstreamAttempts counts only attempts that actually reached an
	// upstream — local pre-flight failures (body re-encode, provider
	// build) don't consume the retry budget, so a couple of bad-config
	// candidates can't starve healthy providers further down the list.
	//
	// The budget is PER GROUP: it resets on every cross-group switch, so a
	// group with many channels cannot swallow the whole request's budget and
	// leave nothing for the group it fails over to. maxGroupSwitches caps the
	// total (see cross_group.go).
	upstreamAttempts := 0
	// The RPM counter moves once per request, not once per accepted attempt.
	// An upstream can answer 2xx and then break — a stream that dies before
	// its terminal frame, a body that fails to read — and the request fails
	// over to the next account from there. Committing again would spend the
	// caller's window twice on one call and 429 their next one early.
	rateCommitted := false
	commitRate := func() {
		if rateCommitted {
			return
		}
		rateCommitted = true
		lc.RateTicket.Commit(reqCtx)
	}
	// In lenient pool mode the health state machine asks for the same account
	// to be retried instead of moving on. sameRetries bounds that per candidate
	// so one sick account cannot monopolize the attempt budget.
	sameRetries := make([]int, len(route.Providers))
	shouldRetrySame := func(i int, outcome biz.AccountFailureOutcome) bool {
		if !outcome.RetrySame || i < 0 || i >= len(sameRetries) || sameRetries[i] >= maxSameAccountRetries {
			return false
		}
		sameRetries[i]++
		return true
	}
	// ctxFloor is set after a context-too-long rejection: only accounts with a
	// larger window are still worth trying (see contextRetryWindow).
	ctxFloor := 0
	// relayRetries counts waits for a restarting local relay (relay_retry.go).
	relayRetries := 0
	clientSawBody := func() bool {
		return c.Writer.Written() && (!downstreamCommitted || streamBodyStarted)
	}
attemptLoop:
	for {
		for i := 0; i < len(route.Providers); i++ {
			rp := route.Providers[i]
			if upstreamAttempts >= g.maxAttempts {
				// Per-group budget spent. Leave the inner loop rather than the
				// whole request: cross-group retry below still gets its turn,
				// and the budget resets for the group it moves to. Breaking
				// all the way out here is why a group with max_attempts-or-more
				// channels could never fail over.
				break
			}
			if ctxFloor > 0 && rp.ContextWindow <= ctxFloor {
				continue
			}
			usedRP = rp
			usedUpstream = rp.UpstreamModel
			// Attribute the eventual usage row to the account serving this
			// attempt. Internal only.
			lc.AccountID = accountIDRef(rp.Account)
			attemptIdx := attempts.Start(rp)
			syncAttempts()

			// Re-encode the client request with the per-account upstream
			// model name. Only deep-clone when account-specific mutations are
			// needed; the common path temporarily swaps model in place to avoid
			// an extra marshal+unmarshal of large request bodies.
			attemptStage := time.Now()
			var blockImageTool bool
			if rp.Account != nil {
				blockImageTool = rp.Account.BlockImageGenerationTool
			}
			fwdBody, omitted, err := trimRequestImages(body, learnedImageLimit(metricOwner(rp)))
			attempts.SetImagesOmitted(attemptIdx, omitted)
			var encoded []byte
			if err == nil {
				encoded, err = prepareForwardBody(fwdBody, rp.UpstreamModel, blockImageTool)
			}
			if err != nil {
				attempts.SetLatency(attemptIdx, "request_prepare_ms", time.Since(attemptStage))
				lastErr = fmt.Errorf("re-encode body: %w", err)
				attempts.SetResult(attemptIdx, 0, lastErr.Error(), "request_prepare_error")
				syncAttempts()
				continue
			}
			// Pick the upstream dialect for this account. An account that
			// cannot serve the request (no native protocol match and conversion
			// not allowed) is skipped without being contacted.
			encoded, fwdPath, fwdDialect, rewriteErr := RewriteForAccount(convEnabled, clientDialect, clientProtocol, rp.Account, rp.UpstreamProtocol, encoded)
			if rewriteErr != nil {
				attempts.SetLatency(attemptIdx, "request_prepare_ms", time.Since(attemptStage))
				if errors.Is(rewriteErr, errAccountProtocolUnsupported) {
					// Counted rather than kept as lastErr: a real upstream
					// failure on another candidate stays the reported cause.
					protocolSkips++
					attempts.SetResult(attemptIdx, 0, "account cannot serve request protocol", "protocol_unsupported")
					syncAttempts()
					continue
				}
				lastErr = fmt.Errorf("format conversion: %w", rewriteErr)
				attempts.SetResult(attemptIdx, 0, lastErr.Error(), "request_prepare_error")
				syncAttempts()
				continue
			}
			attempts.SetLatency(attemptIdx, "request_prepare_ms", time.Since(attemptStage))

			attemptStage = time.Now()
			buildProto := biz.RequestProtocolOpenAI
			if fwdDialect == DialectAnthropic {
				buildProto = biz.RequestProtocolAnthropic
			}
			if !g.acquireAccountSlot(reqCtx, rp) {
				// The account is at its concurrency cap: try the next
				// candidate (a sibling account or the next channel).
				attempts.SetLatency(attemptIdx, "provider_build_ms", time.Since(attemptStage))
				attempts.SetResult(attemptIdx, 0, "account at concurrency cap", "account_busy")
				syncAttempts()
				capacityDrops++
				continue
			}
			// Held until the request handler returns, which covers the
			// streaming copy as well.
			defer g.releaseAccountSlot(c.Request.Context(), rp)
			prov, err := g.buildProviderForAttempt(reqCtx, rp, buildProto)
			attempts.SetLatency(attemptIdx, "provider_build_ms", time.Since(attemptStage))
			if err != nil {
				lastErr = err
				attempts.SetResult(attemptIdx, 0, err.Error(), "build_provider_error")
				syncAttempts()
				recordStickyFailure(rp, "build_provider_error")
				continue
			}

			attemptCtx := reqCtx
			var idleTimeout *streamIdleTimeout
			if stream {
				attemptCtx, idleTimeout = withStreamIdleTimeout(streamCtx, g.streamIdleTimeoutForRequest(c.Request.Context(), modelName, rp.UpstreamModel, body))
				defer idleTimeout.Stop()
			}
			attemptStage = time.Now()
			resp, err := prov.Forward(attemptCtx, fwdPath, encoded, stream, c.Request.Header)
			attempts.SetLatency(attemptIdx, "upstream_header_ms", time.Since(attemptStage))
			metrics.LLMAttemptDuration.WithLabelValues(metricOwner(rp), string(ep)).Observe(time.Since(attemptStage).Seconds())
			upstreamAttempts++
			if err != nil {
				lastErr = err
				lastStatus = 0
				// No upstream response on a transport error — clear any
				// request id carried over from a prior attempt.
				lc.UpstreamRequestID = ""
				if streamFirstTextTimedOut(attemptCtx) {
					markFirstTextTimeout()
					lastErr = errUpstreamFirstTextTimeout
					attempts.SetResult(attemptIdx, 0, lastErr.Error(), "first_text_timeout")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
					syncAttempts()
					metrics.LLMStreamFirstTextTimeout.WithLabelValues(metricOwner(rp)).Inc()
					recordAccountFailure(rp, "first_text_timeout")
					recordStickyFailure(rp, "first_text_timeout")
					break attemptLoop
				}
				if streamIdleTimedOut(attemptCtx) {
					lastErr = errUpstreamStreamIdleTimeout
					attempts.SetResult(attemptIdx, 0, lastErr.Error(), "stream_idle_timeout")
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
					syncAttempts()
					metrics.LLMStreamIdleTimeout.WithLabelValues(metricOwner(rp)).Inc()
					outcome := recordAccountFailure(rp, "stream_idle_timeout")
					recordStickyFailure(rp, "stream_idle_timeout")
					if shouldRetrySame(i, outcome) {
						i--
					}
					continue
				}
				if reqCtx.Err() != nil {
					lastErr = reqCtx.Err()
					attempts.SetResult(attemptIdx, 0, lastErr.Error(), "client_canceled")
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
					syncAttempts()
					break attemptLoop
				}
				if d, ok := relayRestartDelay(rp, err, relayRetries); ok {
					// Not an account fault and not a spent attempt: wait out
					// the relay restart, then retry the same account.
					relayRetries++
					upstreamAttempts--
					attempts.SetResult(attemptIdx, 0, err.Error(), "relay_restart_retry")
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(err.Error(), 8192)
					syncAttempts()
					sleepCtx(reqCtx, d)
					i--
					continue
				}
				attempts.SetResult(attemptIdx, 0, err.Error(), "forward_error")
				lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(err.Error(), 8192)
				syncAttempts()
				// Transport failure is an account-side fault — report it so the
				// health state machine decides cooldown/disable/retry-same.
				outcome := recordAccountFailure(rp, "forward_error")
				recordStickyFailure(rp, "forward_error")
				if shouldRetrySame(i, outcome) {
					i--
				}
				continue
			}
			lastStatus = resp.StatusCode
			idleTimeout.Reset()
			lc.UpstreamRequestID = captureUpstreamRequestID(resp.Header)

			// Treat 5xx + 429 as retryable; everything else either succeeds
			// or is a client-error worth forwarding.
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				cls := classifyUpstreamResponse(resp.StatusCode, b)
				lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
				lc.UpstreamRawError = truncate(string(b), 8192)
				// Account-pool exhaustion is a GLOBAL capacity condition: failing
				// over to a sibling channel just fans out onto the same dry pool
				// (the ~3.5x amplification we observed). Stop now, surface a clean
				// 503 + Retry-After, don't cooldown (the channel isn't bad).
				if cls == classPoolExhausted {
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "pool_exhausted")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					setRetryAfter(c, resp.Header, 30*time.Second)
					writeErr(http.StatusServiceUnavailable, "upstream_error", "pool_exhausted", upstreamUnavailableMessage())
					g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount,
						entusagelog.StatusUpstreamError, http.StatusServiceUnavailable, lastErr.Error(), lc)
					return
				}
				attempts.SetResult(attemptIdx, resp.StatusCode, string(b), switchStrategyForStatus(resp.StatusCode))
				lc.ErrorClass = string(attempts.FinalClass())
				syncAttempts()
				// Provider-side failure — report it so the health state machine
				// cools (or disables) this account per its failure class and
				// pool mode.
				outcome := recordUpstreamFailure(rp, cls, resp.StatusCode, resp.Header, string(b))
				recordStickyFailure(rp, fmt.Sprintf("status_%d", resp.StatusCode))
				if shouldRetrySame(i, outcome) {
					i--
				}
				continue
			}
			// Remaining-quota sampling: the vendor's own rate-limit
			// headers are the cheapest source of window utilization, and they
			// arrive on success as well as on 429. Detached from the request
			// (the caller may already be gone) and throttled inside the
			// service, so ordinary traffic costs nothing.
			g.sampleAccountQuota(c.Request.Context(), rp, resp.Header)
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				// Single structured classification drives retry, safety and
				// health. cyber_block (any status) and request-level 4xx
				// (400/413/422) are non-retryable; channel-level 4xx
				// (401/402/403/404/408) fail over to the next provider.
				cls := classifyUpstreamResponse(resp.StatusCode, b)
				cyberBlocked := cls.IsSafetyBlock()
				lc.UpstreamRawError = truncate(string(b), 8192)
				// Over this account's image cap: retry the same account with the
				// oldest images dropped. Not a health sample.
				if n := upstreamImageLimit(b); n > 0 && upstreamAttempts < g.maxAttempts &&
					countRequestImages(body) > n && learnImageLimit(metricOwner(rp), n) {
					lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "image_limit_retry")
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					i--
					continue
				}
				// Over this account's window but within a later one's: fail over
				// instead of returning the rejection. Not a health sample.
				if w := contextRetryWindow(route.Providers, i, rp, b, upstreamAttempts, g.maxAttempts); w > 0 {
					ctxFloor = w
					lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "context_window_retry")
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					continue
				}
				if cls.Retryable() {
					lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), switchStrategyForStatus(resp.StatusCode))
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					// Account-level auth/quota failure — report it; the state
					// machine decides cooldown vs disable.
					outcome := recordUpstreamFailure(rp, cls, resp.StatusCode, resp.Header, string(b))
					recordStickyFailure(rp, fmt.Sprintf("status_%d", resp.StatusCode))
					if shouldRetrySame(i, outcome) {
						i--
					}
					continue
				}
				attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "")
				attempts.MarkFinal(attemptIdx)
				lc.ErrorClass = string(attempts.FinalClass())
				syncAttempts()
				if cyberBlocked {
					uid := user.ID
					g.incidents.RecordAsync(biz.IncidentInput{
						UserID:    &uid,
						ApiKeyID:  apiKeyID(apiKey),
						Kind:      "cyber_security",
						Severity:  "high",
						Action:    "blocked",
						Model:     modelName,
						Endpoint:  string(ep),
						RequestID: requestID,
						IP:        c.ClientIP(),
						Message:   "upstream cyber-security/moderation block",
						Payload: map[string]any{
							"upstream_status": resp.StatusCode,
						},
					})
				}
				// Terminal upstream rejection — the user got no output, return
				// the hold (this path writes its usage row directly instead of
				// via logFinal).
				g.releaseReservation(c.Request.Context(), lc.Reservation)
				if cyberBlocked {
					writeErr(http.StatusForbidden, "content_policy_violation", "cyber_safety_block", cyberSafetyMessage())
				} else if msg, code, ok := actionableUpstreamError(b); ok {
					// Actionable upstream rejection (wrong client version, input
					// over the context window, a disabled feature, ...) — echo the
					// upstream's own message so the user knows what to fix, instead
					// of masking it behind the generic rejection text.
					writeErr(resp.StatusCode, "invalid_request_error", code, msg)
				} else {
					st, code, msg := upstreamRejectedReply(resp.StatusCode, b)
					writeErr(st, "upstream_error", code, msg)
				}
				_, _ = g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
					UserID:            user.ID,
					ApiKeyID:          apiKeyID(apiKey),
					OrganizationID:    organizationID(apiKey),
					RequestID:         requestID,
					Endpoint:          ep,
					Model:             modelName,
					ModelID:           &route.Model.ID,
					GroupID:           &route.Group.ID,
					ProviderID:        rp.LegacyProviderID(),
					ProviderAccountID: accountIDRef(rp.Account),
					UpstreamModel:     rp.UpstreamModel,
					Status:            entusagelog.StatusUpstreamError,
					HTTPStatus:        resp.StatusCode,
					LatencyMs:         int(time.Since(startedAt).Milliseconds()),
					RetryCount:        retryCount,
					ProviderChain:     providerChain,
					ErrorMessage:      fmt.Sprintf("upstream %d", resp.StatusCode),
					ErrorClass:        lc.ErrorClass,
					UpstreamRequestID: lc.UpstreamRequestID,
					UpstreamRawError:  truncate(string(b), 8192),
					StartedAt:         startedAt,
					IP:                lc.IP,
					UserAgent:         lc.UserAgent,
					ReasoningEffort:   lc.ReasoningEffort,
					Stream:            lc.Stream,
					ApiKeyName:        lc.ApiKeyName,
					LatencyDetail:     lc.Latency.Snapshot(),
				})
				return
			}

			// 2xx success
			//
			// This is the commit point of the rate-limit protocol: the request is
			// only counted against RPM once an upstream has actually accepted it,
			// so provider 5xx / connect failures and the retries they trigger stay
			// free for the caller.
			commitRate()
			if convEnabled && fwdDialect != clientDialect {
				resp.Body = wrapConvertedBody(fwdDialect, clientDialect, resp.Body, stream)
			}
			if stream {
				// Aggregate mode (client asked /v1/responses with stream:false):
				// sink the upstream SSE into an aggregator and answer with a
				// single JSON object once the terminal frame arrives. Nothing is
				// written to the client until then, so failover stays available.
				var agg *responsesAggregator
				var prelude *streamPrelude
				var dst io.Writer = c.Writer
				if aggregateResponses {
					agg = newResponsesAggregator()
					dst = agg
				} else {
					// Commit the status line now. Waiting for the first SSE
					// line made the client's first packet track model prefill.
					// Frames before the first model output are held back, so an
					// attempt that fails before then can still fail over.
					if !downstreamCommitted {
						commitDownstreamSSE(c.Writer)
						observeStreamTTFB(metricOwner(rp), string(ep), startedAt)
						downstreamCommitted = true
					}
					prelude = &streamPrelude{w: c.Writer}
					dst = prelude
				}
				releasePrelude := func() {
					if prelude != nil {
						prelude.release()
						streamBodyStarted = true
					}
				}
				// Output-side policy: only installed when at least one
				// enabled group has output scope. Block hits close the
				// upstream connection and surface ErrPolicyBlocked.
				var policyHit *biz.PolicyMatch
				onFrame := g.policyStreamHook(func(h *biz.PolicyMatch) {
					policyHit = h
					writePolicyBlockedSSE(dst, h, string(ep))
					g.recordPolicyOutputIncident(c, h, &user.ID, apiKeyID(apiKey), requestID, modelName, string(ep))
				})
				// Image-output gate: when AllowImageOutput is off, rewrite
				// every image block in flight so the client receives a
				// text notice instead. Acc still sees the original frame
				// for token-counting, but acc.OutputImages stays empty by
				// construction because the transform happens *after*
				// Inspect — see further down: we reset it.
				stageStart = time.Now()
				imageOutputAllowed := g.imageOutputAllowed(c)
				lc.Latency.Duration("image_output_policy_ms", time.Since(stageStart))
				imageBlockedThisRequest := false
				transformFn := func(payload []byte) []byte {
					if imageOutputAllowed {
						return nil
					}
					var obj map[string]any
					if err := json.Unmarshal(payload, &obj); err != nil {
						return nil
					}
					if biz.RewriteImageOutputsWithNotice(obj) == 0 {
						return nil
					}
					imageBlockedThisRequest = true
					out, err := json.Marshal(obj)
					if err != nil {
						return nil
					}
					return out
				}
				upstreamFailed := false
				var upstreamFailedRaw []byte
				// Whether the upstream produced any SSE frame at all. The pipe
				// only reports streams it can judge — one that never started
				// is a different condition, handled after the pipe returns.
				sawUpstreamFrame := false
				pipeErr := PipeAndExtractOpts(dst, resp.Body, acc, PipeOptions{
					OnFirstFrame: func() {
						lc.Latency.SinceStart("first_sse_frame_ms")
						// Aggregate mode has not committed a downstream status
						// line; the first upstream frame is the first byte we
						// can attribute. Streaming mode already recorded TTFB
						// when the headers were flushed.
						if aggregateResponses {
							observeStreamTTFB(metricOwner(rp), string(ep), startedAt)
						}
						// Any first content frame (response.created, a reasoning
						// delta, ...) proves the upstream is alive — hand stall
						// detection to the idle timer from here. Reasoning models
						// can legitimately stream minutes of non-text frames
						// before the first counted token, so the first-token
						// guard must not keep ticking past this point.
						firstTextTimeout.Stop()
					},
					OnFirstText: func() {
						releasePrelude()
						firstTextTimeout.Stop()
						d := int(time.Since(startedAt).Milliseconds())
						ttftMs = &d
						lc.Latency.Duration("first_token_delta_ms", time.Duration(d)*time.Millisecond)
					},
					// The model started producing, readable or not. This, not
					// the first frame, is where the decode window begins for a
					// reasoning model whose thinking arrives encrypted: its
					// reasoning tokens are in tokens_out but its first readable
					// token is hundreds of seconds later.
					OnFirstOutput: func() {
						releasePrelude()
						lc.Latency.SinceStart("first_output_ms")
					},
					OnFrame: func(payload []byte, acc *Accumulator) error {
						sawUpstreamFrame = true
						// Whether the stream finished is the pipe's business
						// now. This hook only singles out the one terminal
						// event that means "finished, and it failed", so the
						// request is logged as an upstream error rather than
						// charged as a completed response.
						if ep == entusagelog.EndpointResponses {
							var ev struct {
								Type string `json:"type"`
							}
							if err := json.Unmarshal(payload, &ev); err == nil && ev.Type == "response.failed" {
								upstreamFailed = true
								upstreamFailedRaw = append([]byte(nil), payload...)
							}
						}
						if onFrame != nil {
							if err := onFrame(payload, acc); err != nil {
								return err
							}
						}
						return nil
					},
					TransformPayload: transformFn,
					// Reset the chunk-idle (stall) timer only on real content
					// frames — upstream keepalive comments must NOT keep a stalled
					// stream alive.
					AfterContentFrame: idleTimeout.Reset,
				})
				// When images were blocked, scrub acc.OutputImages so
				// billing sees "no images" (we discarded them per operator
				// policy).
				if !imageOutputAllowed && imageBlockedThisRequest {
					acc.OutputImages = nil
					g.recordImageBlockedIncident(c, &user.ID, apiKeyID(apiKey), requestID, modelName, string(ep))
				}
				_ = resp.Body.Close()
				// Some relays ignore stream=true and answer with one plain JSON
				// object. There is no `data:` frame in that, so the pipe stays
				// quiet — it cannot tell a non-stream from a finished one. Only
				// aggregate mode can make use of such a body; for a streaming
				// client it is an unusable response, and either way an
				// unusable response is the same outcome as a truncated one.
				if pipeErr == nil && !sawUpstreamFrame && !upstreamFailed {
					if !aggregateResponses || agg.final() == nil {
						pipeErr = errUpstreamStreamIncomplete
					}
				}
				// Nothing of this attempt has reached the client: it can still
				// fail over. Aggregate mode writes only after the pipe returns.
				canFailover := !clientSawBody() && (prelude == nil || !prelude.released)
				if errors.Is(pipeErr, ErrPolicyBlocked) {
					releasePrelude()
					// Clean termination on policy block — don't retry,
					// log a dedicated reason so admins can tell it apart
					// from upstream errors.
					slug := ""
					if policyHit != nil {
						slug = policyHit.Slug
					}
					attempts.SetResult(attemptIdx, resp.StatusCode, "output_policy_block:"+slug, "output_policy_block")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					if aggregateResponses {
						// The blocked-termination frame went into the aggregator —
						// hand its response object to the client as JSON.
						if payload := agg.final(); payload != nil {
							c.Data(http.StatusOK, "application/json", payload)
						} else {
							body := openAIError("content_policy_violation", "output_policy_block", outputPolicyMessage(slug), requestID)
							body["error"].(map[string]any)["category"] = slug
							c.JSON(http.StatusUnprocessableEntity, body)
						}
					}
					g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount,
						entusagelog.StatusInternalError, resp.StatusCode,
						"output_policy_block:"+slug, lc)
					return
				}
				// A failed write to our own socket means the caller hung up mid-stream.
				// It is indistinguishable from the observed-cancel case below as far as
				// the service is concerned, so it takes the same path: logged at 499,
				// never cooled against the provider, never counted as unavailability.
				// Checked before the timeout branches because a dead client socket is a
				// direct syscall result, whereas the timeouts are inferred from ctx.
				if errors.Is(pipeErr, errClientWrite) {
					lastErr = pipeErr
					attempts.SetResult(attemptIdx, resp.StatusCode, "client_canceled: "+pipeErr.Error(), "client_canceled")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, 499, "client_canceled", lc)
					return
				}
				// An upstream that stops mid-answer looks like a clean EOF, so
				// the pipe reports it. A client that hangs up produces the same
				// shape — and when it does, the read is usually what fails
				// first, so the error is a wrapped context.Canceled rather than
				// the sentinel. Either way it is the caller's doing and must
				// not cool the channel or count against availability. Note the
				// stream timeouts cancel a derived context, never reqCtx, so
				// they cannot be mistaken for a hang-up here.
				if streamEndedOnClientHangUp(pipeErr, reqCtx) {
					attempts.SetResult(attemptIdx, resp.StatusCode, "client_canceled", "client_canceled")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, 499, "client_canceled", lc)
					return
				}
				if pipeErr != nil && (!errors.Is(pipeErr, context.Canceled) || streamIdleTimedOut(attemptCtx) || streamFirstTextTimedOut(attemptCtx) || streamTotalTimedOut(attemptCtx)) {
					lastErr = pipeErr
					reason := "stream_error"
					if errors.Is(pipeErr, errUpstreamStreamIncomplete) {
						reason = "stream_incomplete"
					}
					// Check total BEFORE idle: both surface as a cancelled ctx, so
					// order decides the recorded reason.
					if streamFirstTextTimedOut(attemptCtx) {
						markFirstTextTimeout()
						lastErr = errUpstreamFirstTextTimeout
						reason = "first_text_timeout"
					} else if streamTotalTimedOut(attemptCtx) {
						lastErr = errUpstreamStreamTotalTimeout
						reason = "stream_total_timeout"
					} else if streamIdleTimedOut(attemptCtx) {
						lastErr = errUpstreamStreamIdleTimeout
						reason = "stream_idle_timeout"
					}
					// Total timeout is a request-level deadline, not a channel
					// fault. stream_incomplete is a channel fault but a single
					// one must not take the channel out of rotation: the
					// cooldown has no threshold, and this condition became
					// visible on the busiest endpoint only with this change, so
					// one truncation per minute would keep a healthy provider
					// permanently cooled and bounce traffic between channels.
					// The health window still catches a provider that truncates
					// systematically.
					if reason != "stream_total_timeout" && reason != "stream_incomplete" {
						recordAccountFailure(rp, reason)
					}
					recordStickyFailure(rp, reason)
					attempts.SetResult(attemptIdx, resp.StatusCode, lastErr.Error(), reason)
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
					syncAttempts()
					// Neither first-token nor total timeout should fail over (the
					// former re-enters a cancelled ctx; the latter is terminal).
					if canFailover && reason != "first_text_timeout" && reason != "stream_total_timeout" && moreProviders(i) && upstreamAttempts < g.maxAttempts {
						*acc = Accumulator{}
						continue
					}
					releasePrelude()
					// Nothing written yet → answer with a real error status so
					// the client fails fast instead of hanging on an empty 200.
					logHTTPStatus := resp.StatusCode
					if c.Writer.Written() {
						writeStreamAbortTermination(c.Writer, string(ep), requestID, acc.AssistantText.String())
					} else {
						status := http.StatusBadGateway
						msg := upstreamUnavailableMessage()
						switch reason {
						case "first_text_timeout", "stream_total_timeout":
							status, msg = http.StatusGatewayTimeout, upstreamTimeoutMessage()
						case "stream_idle_timeout":
							status, msg = http.StatusGatewayTimeout, upstreamStalledMessage()
						case "stream_incomplete":
							msg = streamInterruptedMessage()
						}
						writeErr(status, "upstream_error", reason, msg)
						logHTTPStatus = status
					}
					attempts.MarkFinal(attemptIdx)
					syncAttempts()
					g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, logHTTPStatus, lastErr.Error(), lc)
					return
				}
				if upstreamFailed {
					// Upstream explicitly terminated with response.failed. The failed
					// frame has already been forwarded (or captured for aggregate mode).
					// logFinal settles observed usage if the hop reported tokens.
					reason := "response_failed"
					msg := "upstream response.failed"
					// Extract the upstream error code. It is the ONLY signal available
					// here — the HTTP status is the 200 already on the wire — so it is
					// appended to the strategy (not just the message) to let
					// deriveErrorClass classify structurally instead of by substring.
					var failedDetail struct {
						Type     string `json:"type"`
						Response *struct {
							Error *struct {
								Code    string `json:"code"`
								Message string `json:"message"`
							} `json:"error"`
						} `json:"response"`
					}
					if err := json.Unmarshal(upstreamFailedRaw, &failedDetail); err == nil && failedDetail.Response != nil && failedDetail.Response.Error != nil {
						if c := failedDetail.Response.Error.Code; c != "" {
							msg = fmt.Sprintf("upstream response.failed: %s", c)
							reason = "response_failed:" + c
						}
					}
					attempts.SetResult(attemptIdx, resp.StatusCode, msg, reason)
					// Only genuine channel-level aborts cool the channel. A content
					// block or an over-long prompt fails identically on every provider,
					// so cooling one for it just shrinks the pool for no reason.
					failedClass := attempts.FinalClass()
					if failedClass.CountsForCooldown() {
						recordAccountFailure(rp, reason)
						recordStickyFailure(rp, reason)
					}
					// Failed before any output (the busy/server_error family
					// lands here a frame or two in): another account may serve it.
					if canFailover && failedClass.Retryable() && moreProviders(i) && upstreamAttempts < g.maxAttempts {
						lastErr = errors.New(msg)
						lc.ErrorClass = string(failedClass)
						lc.UpstreamRawError = truncate(string(upstreamFailedRaw), 8192)
						syncAttempts()
						*acc = Accumulator{}
						continue
					}
					releasePrelude()
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass = string(attempts.FinalClass())
					lc.UpstreamRawError = truncate(string(upstreamFailedRaw), 8192)
					syncAttempts()
					if aggregateResponses {
						if payload := agg.final(); payload != nil {
							c.Data(resp.StatusCode, "application/json", payload)
						} else {
							writeErr(http.StatusBadGateway, "upstream_error", reason, upstreamUnavailableMessage())
						}
					}
					g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, resp.StatusCode, msg, lc)
					return
				}
				releasePrelude()
				if aggregateResponses {
					// Terminal frame captured — answer the non-streaming client
					// with the complete response object as one JSON body.
					c.Data(resp.StatusCode, "application/json", agg.final())
				}
			} else {
				rawResp, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				// A truncated body — or a failed format conversion, which arrives
				// here as a pipe error because the converter runs inside
				// resp.Body — leaves nothing worth forwarding. Answering 200 with
				// the partial bytes hands the client a body it cannot parse and
				// still charges for it, so treat it like any other attempt
				// failure: fail over while the response is still unwritten, or
				// surface a real error status.
				if readErr != nil {
					if errors.Is(reqCtx.Err(), context.Canceled) {
						attempts.SetResult(attemptIdx, resp.StatusCode, "client_canceled", "client_canceled")
						attempts.MarkFinal(attemptIdx)
						lc.ErrorClass = string(attempts.FinalClass())
						syncAttempts()
						g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
							acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, 499, "client_canceled", lc)
						return
					}
					lastErr = fmt.Errorf("read upstream response: %w", readErr)
					msg := lastErr.Error()
					recordAccountFailure(rp, "response_read_error")
					recordStickyFailure(rp, "response_read_error")
					attempts.SetResult(attemptIdx, resp.StatusCode, msg, "response_read_error")
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(msg, 8192)
					syncAttempts()
					if moreProviders(i) && upstreamAttempts < g.maxAttempts {
						continue
					}
					attempts.MarkFinal(attemptIdx)
					syncAttempts()
					writeErr(http.StatusBadGateway, "upstream_error", "response_read_error", upstreamUnavailableMessage())
					g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, http.StatusBadGateway, msg, lc)
					return
				}
				acc.Inspect(rawResp)
				// Image-output gate (non-stream): rewrite image blocks in
				// place before forwarding the body to the client.
				stageStart = time.Now()
				imageOutputAllowed := g.imageOutputAllowed(c)
				lc.Latency.Duration("image_output_policy_ms", time.Since(stageStart))
				if !imageOutputAllowed {
					var obj map[string]any
					if err := json.Unmarshal(rawResp, &obj); err == nil {
						if biz.RewriteImageOutputsWithNotice(obj) > 0 {
							if rewritten, err := json.Marshal(obj); err == nil {
								rawResp = rewritten
							}
							acc.OutputImages = nil
							g.recordImageBlockedIncident(c, &user.ID, apiKeyID(apiKey), requestID, modelName, string(ep))
						}
					}
				}
				// Output-side policy on non-stream JSON: scan the full
				// response body once. We don't need a streaming scanner
				// here — full text is available before we forward to
				// client, so we can swap in a 422 if blocked.
				if g.policy != nil {
					hasText := g.policy.HasAnyScope(biz.ScopeInputOutput, biz.ScopeInputOutputTools)
					hasTools := g.policy.HasAnyScope(biz.ScopeInputOutputTools)
					if hasText || hasTools {
						sc := g.policy.NewStreamScanner()
						filter := biz.ScopeInputOutput
						if hasTools && !hasText {
							filter = biz.ScopeInputOutputTools
						}
						if hit := sc.Feed(rawResp, filter); hit != nil && hit.Action == "block" {
							g.recordPolicyOutputIncident(c, hit, &user.ID, apiKeyID(apiKey), requestID, modelName, string(ep))
							body := openAIError("content_policy_violation", "output_policy_block", outputPolicyMessage(hit.Slug), requestID)
							body["error"].(map[string]any)["category"] = hit.Slug
							c.JSON(http.StatusUnprocessableEntity, body)
							attempts.SetResult(attemptIdx, http.StatusUnprocessableEntity, "output_policy_block:"+hit.Slug, "output_policy_block")
							attempts.MarkFinal(attemptIdx)
							lc.ErrorClass = string(attempts.FinalClass())
							syncAttempts()
							g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
								acc, ttftMs, startedAt, providerChain, retryCount,
								entusagelog.StatusInternalError, http.StatusUnprocessableEntity,
								"output_policy_block:"+hit.Slug, lc)
							recordStickyFailure(rp, "output_policy_block")
							return
						}
					}
				}
				ct := resp.Header.Get("Content-Type")
				if ct == "" {
					ct = "application/json"
				}
				c.Data(resp.StatusCode, ct, rawResp)
			}
			attempts.SetResult(attemptIdx, resp.StatusCode, "", "")
			attempts.MarkFinal(attemptIdx)
			lc.ErrorClass = string(attempts.FinalClass())
			syncAttempts()

			// Pin this (user, group, conversation) to the winning account for
			// the sticky_ttl_seconds window so follow-up requests land here.
			// Same pin, one level down: the account that served this attempt.
			g.rememberAccountOutcome(c.Request.Context(), user.ID, route.Group.ID, convID, rp)
			g.rememberRouteGroup(c.Request.Context(), user.ID, route.Group.ID, modelName, convID, failover.switches > 0)
			// Charge + log.
			g.chargeAndLog(c.Request.Context(), user, apiKey, requestID, ep, modelName, route, rp, usedUpstream,
				acc, ttftMs, startedAt, providerChain, retryCount, resp.StatusCode, lc, planState)
			return
		}

		// Every provider of the current group failed with nothing written to
		// the client (terminal failures returned or broke out of attemptLoop
		// above). Cross-group retry: same billing_source only, rate within
		// the key's auto_route_max_rate, hold re-priced inside next().
		nextRoute, nextState := failover.next(reqCtx, route, clientSawBody(), &lc)
		if nextRoute == nil {
			break
		}
		route = nextRoute
		planState = nextState
		// The fallback route is built by the biz resolver and already carries
		// accounts; order them the same way the primary path does.
		route.Providers = g.orderRouteAccounts(reqCtx, user, apiKey, route.Group.ID, convID, modelName, route.Providers)
		lc.Latency.Value("cross_group_switches", failover.switches)
		// Fresh group, fresh attempt budget — see the per-group note above.
		upstreamAttempts = 0
	}

	// All attempts exhausted.
	if clientSawBody() {
		// Stream may have partially written; just release the hold and stop
		// (defensive — the streaming failure paths above normally return
		// through logFinal before reaching here).
		g.releaseReservation(c.Request.Context(), lc.Reservation)
		return
	}
	msg := "upstream unavailable"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	// Nothing ever reached an upstream and the reason was capacity: answer as
	// backpressure so clients back off instead of retrying a "broken" upstream.
	if lastErr == nil && capacityDrops > 0 {
		lastErr = errNoUpstreamCapacity
		msg = lastErr.Error()
	}
	if lastErr == nil && protocolSkips > 0 {
		lastErr = errAccountProtocolUnsupported
		msg = lastErr.Error()
	}

	// Derive a client-facing reason (idle/total timeout, incomplete stream,
	// unreachable upstream, or an actionable upstream body) instead of a
	// blanket "service temporarily unavailable".
	errCode, clientMsg := failureReason(lastErr, lastStatus, lc.UpstreamRawError)
	// 502 is reserved for a genuine transport failure (no upstream HTTP
	// status). An upstream 5xx/429 becomes a 503 + Retry-After so clients
	// back off instead of hammering (a bare 502 invites aggressive retry).
	status := http.StatusBadGateway
	errKind := "upstream_error"
	if errors.Is(lastErr, errAccountProtocolUnsupported) {
		status = http.StatusBadRequest
		errKind = "invalid_request_error"
		errCode, clientMsg = "unsupported_protocol", unsupportedProtocolMessage(modelName, c.Request.URL.Path, route.Providers)
		lc.ErrorClass = string(classUpstreamConfig)
	} else if isTimeoutErr(lastErr) {
		status = http.StatusGatewayTimeout
	} else if errors.Is(lastErr, errNoUpstreamCapacity) {
		// Backpressure, not a broken upstream: 503 with Retry-After, the same
		// shape the exhausted-pool path uses.
		status = http.StatusServiceUnavailable
		setRetryAfter(c, nil, 5*time.Second)
	} else if lastStatus == http.StatusTooManyRequests {
		status = http.StatusServiceUnavailable
		setRetryAfter(c, nil, cooldownRateMin)
	} else if lastStatus >= 500 {
		status = http.StatusServiceUnavailable
		setRetryAfter(c, nil, 5*time.Second)
	} else if lastStatus >= 400 && lastStatus < 500 {
		// Channel-level 4xx — surface the status; the body is masked unless
		// failureReason recognized it as actionable.
		status = lastStatus
	}
	writeErr(status, errKind, errCode, clientMsg)
	logStatus := lastStatus
	if status == http.StatusGatewayTimeout || status == http.StatusBadRequest {
		logStatus = status
	}
	g.logFinal(c, user, apiKey, requestID, ep, modelName, route, usedRP, usedUpstream,
		acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, logStatus, msg, lc)
}

// --- helpers ---

// imageOutputAllowed reads SiteSettings.AllowImageOutput. Cached
// nowhere — settings.GetSite() is itself one DB round-trip with a
// small fixed cost, and we want admin toggles to take effect
// immediately without TTL games. On any error we fail closed
// (default to "not allowed") so a transient DB blip doesn't let
// images leak through.
func (g *Gateway) imageOutputAllowed(c *gin.Context) bool {
	if g.settings == nil {
		return false
	}
	st, err := g.settings.GetSite(c.Request.Context())
	if err != nil || st == nil {
		return false
	}
	return st.AllowImageOutput
}

func (g *Gateway) streamFirstTextTimeout(ctx context.Context) time.Duration {
	if g.settings == nil {
		return defaultStreamFirstTextTimeout
	}
	st, err := g.settings.GetSite(ctx)
	if err != nil || st == nil {
		return defaultStreamFirstTextTimeout
	}
	if st.StreamFirstTextTimeoutSeconds < 0 {
		return defaultStreamFirstTextTimeout
	}
	return time.Duration(st.StreamFirstTextTimeoutSeconds) * time.Second
}

// streamTotalTimeout is the wall-clock cap on a whole streamed request. 0
// disables. Defaults to 0 (disabled) when settings are unavailable so a
// settings outage can't start truncating long streams.
func (g *Gateway) streamTotalTimeout(ctx context.Context) time.Duration {
	if g.settings == nil {
		return 0
	}
	st, err := g.settings.GetSite(ctx)
	if err != nil || st == nil || st.StreamTotalTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(st.StreamTotalTimeoutSeconds) * time.Second
}

// streamIdleTimeout is the cap on the gap between streamed bytes. Falls back
// to the request timeout when unset/unavailable (the prior behavior).
func (g *Gateway) streamIdleTimeout(ctx context.Context) time.Duration {
	if g.settings == nil {
		return g.requestTimeout
	}
	st, err := g.settings.GetSite(ctx)
	if err != nil || st == nil || st.StreamIdleTimeoutSeconds <= 0 {
		return g.requestTimeout
	}
	return time.Duration(st.StreamIdleTimeoutSeconds) * time.Second
}

func (g *Gateway) streamIdleTimeoutForRequest(ctx context.Context, modelName, upstreamModel string, body map[string]any) time.Duration {
	timeout := g.streamIdleTimeout(ctx)
	if isLikelyImageGenerationRequest(modelName, upstreamModel, body) && timeout < minImageGenerationStreamIdleTimeout {
		return minImageGenerationStreamIdleTimeout
	}
	return timeout
}

// recordImageBlockedIncident persists a user_incident row so admins
// can see "this user tried to generate images while the feature was
// off". Action=blocked, severity=low — it's policy-driven, not an
// abuse signal.
func (g *Gateway) recordImageBlockedIncident(c *gin.Context, userID *uuid.UUID, apiKeyID *uuid.UUID, requestID, modelName, endpointStr string) {
	if g.incidents == nil {
		return
	}
	g.incidents.RecordAsync(biz.IncidentInput{
		UserID:    userID,
		ApiKeyID:  apiKeyID,
		Kind:      "image_output_blocked",
		Severity:  "low",
		Action:    "blocked",
		Model:     modelName,
		Endpoint:  endpointStr,
		RequestID: requestID,
		IP:        c.ClientIP(),
		Message:   "image output blocked by site policy (AllowImageOutput=false)",
	})
}
