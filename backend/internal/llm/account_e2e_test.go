package llm

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/middleware"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/providerkit"
	"github.com/jucode/jucode/internal/providerkit/catalog"
)

// This file is the account layer's end-to-end smoke test: a real group bound to
// a real account row, driven through the real HTTP handlers, against a fake
// upstream. It covers the things that break in production:
//
//   - a Messages-only account must not be handed an OpenAI-format body — with
//     conversion off the request fails cleanly and leaks nothing, with the site
//     switch on the body is converted and served;
//   - a 429 must park that account (not the group) so the next request skips it.

type accountE2EEnv struct {
	ctx      context.Context
	t        *testing.T
	client   *ent.Client
	db       *sql.DB
	gw       *Gateway
	settings *biz.SettingsSvc
	cipher   *cryptopkg.Cipher
	accounts *biz.AccountPoolSvc
	user     *ent.User
	apiKey   *ent.ApiKey
	model    *ent.Model
	group    *ent.ModelGroup
}

func newAccountE2EEnv(t *testing.T) *accountE2EEnv {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping account-layer end-to-end test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("schema: %v", err)
	}
	redisAddr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unreachable at %s (%v)", redisAddr, err)
	}
	cipher, err := cryptopkg.New(strings.Repeat("ef", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	d := data.NewWith(client, rdb, db)
	settings := biz.NewSettingsSvc(d)
	plans := biz.NewPlanSvc(d, settings)
	billing := biz.NewBillingSvc(d, plans, settings)
	models := biz.NewModelSvc(d)
	models.SetSettings(settings)
	gw := NewGateway(
		models, billing,
		biz.NewConcurrencyGate(d, plans, settings),
		biz.NewStickySvc(d, settings),
		biz.NewPolicySvc(d),
		biz.NewIncidentSvc(d),
		settings, cipher,
		3, /*maxAttempts*/
		0, /*maxBodyBytes*/
		30*time.Second,
	)
	accounts := biz.NewAccountPoolSvc(d, cipher, settings)
	gw.SetAccounts(accounts)
	// Route on accounts regardless of the site's auto-route setting: these
	// tests assert protocol and health behavior, not group selection.
	baseURL := "http://" + "127.0.0.1:1"
	gw.models = models
	_ = baseURL

	user, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		SetBalance(decimal.RequireFromString("100")).
		Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apiKey, err := client.ApiKey.Create().
		SetUserID(user.ID).
		SetPrefix("sk-e2e").
		SetHash(uuid.NewString()).
		Save(ctx)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	model, err := client.Model.Create().
		SetName("e2e-model-" + uuid.NewString()).
		SetBillingMode("per_token").
		SetInputUnitPrice(decimal.RequireFromString("0.001")).
		SetOutputUnitPrice(decimal.RequireFromString("0.002")).
		Save(ctx)
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	group, err := client.ModelGroup.Create().
		SetName("e2e-group-" + uuid.NewString()).
		SetBillingSource("balance_only").
		SetRateMultiplier(1).
		Save(ctx)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := client.ModelGroupModel.Create().
		SetGroupID(group.ID).SetModelID(model.ID).Save(ctx); err != nil {
		t.Fatalf("link model: %v", err)
	}
	env := &accountE2EEnv{ctx: ctx, t: t, client: client, db: db, gw: gw, settings: settings, cipher: cipher, accounts: accounts, user: user, apiKey: apiKey, model: model, group: group}
	t.Cleanup(func() {
		_ = client.Close()
		_ = db.Close()
	})
	return env
}

// addAccount binds one account to the test group.
func (e *accountE2EEnv) addAccount(name, baseURL string, native []string, opts ...func(*ent.UpstreamAccountCreate)) *ent.UpstreamAccount {
	return e.addAccountWithLink(name, baseURL, native, 0, 0, opts...)
}

// addAccountWithLink also sets the group link's scheduling inputs, which are
// what the router actually orders by inside a group.
func (e *accountE2EEnv) addAccountWithLink(name, baseURL string, native []string, linkPriority, linkWeight int, opts ...func(*ent.UpstreamAccountCreate)) *ent.UpstreamAccount {
	e.t.Helper()
	q := e.client.UpstreamAccount.Create().
		SetName(name).
		SetBaseURL(baseURL).
		SetNativeProtocols(native).
		SetStatus(entaccount.StatusActive).
		SetSchedulable(true)
	for _, opt := range opts {
		opt(q)
	}
	acct, err := q.Save(e.ctx)
	if err != nil {
		e.t.Fatalf("create account: %v", err)
	}
	link := e.client.GroupAccount.Create().
		SetGroupID(e.group.ID).SetAccountID(acct.ID).SetEnabled(true)
	if linkPriority > 0 {
		link = link.SetPriority(linkPriority)
	}
	if linkWeight > 0 {
		link = link.SetWeight(linkWeight)
	}
	if _, err := link.Save(e.ctx); err != nil {
		e.t.Fatalf("link account: %v", err)
	}
	if _, err := e.client.UpstreamAccountModel.Create().
		SetAccountID(acct.ID).SetModelName(e.model.Name).SetEnabled(true).Save(e.ctx); err != nil {
		e.t.Fatalf("bind model: %v", err)
	}
	return acct
}

func (e *accountE2EEnv) withAPIKeyCredential(key string) func(*ent.UpstreamAccountCreate) {
	e.t.Helper()
	plain, err := providerkit.NewAPIKeyCredential(key).Encode()
	if err != nil {
		e.t.Fatalf("encode: %v", err)
	}
	enc, err := e.cipher.Encrypt(plain)
	if err != nil {
		e.t.Fatalf("encrypt: %v", err)
	}
	return func(q *ent.UpstreamAccountCreate) {
		q.SetCredentialEnc(enc)
	}
}

func (e *accountE2EEnv) setConversion(enabled bool) {
	e.t.Helper()
	// The site row is not created by the schema defaults in tests, so write it
	// through the settings service's own store.
	st, err := e.settings.GetSite(e.ctx)
	if err != nil || st == nil {
		e.t.Fatalf("get site: %v", err)
	}
	st.FormatConversionEnabled = enabled
	if err := e.settings.UpdateSite(e.ctx, *st); err != nil {
		e.t.Fatalf("update site: %v", err)
	}
}

// do sends one request through the real handler and returns the recorder.
func (e *accountE2EEnv) do(handler func(*gin.Context), path string, body map[string]any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	gin.SetMode(gin.TestMode)
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	c.Set(middleware.CtxUser, e.user)
	c.Set(middleware.CtxApiKey, e.apiKey)
	handler(c)
	return w
}

func (e *accountE2EEnv) chat(body map[string]any, headers map[string]string) *httptest.ResponseRecorder {
	return e.do(e.gw.HandleChatCompletions, "/v1/chat/completions", body, headers)
}

// TestAccountLayerProtocolGating drives an OpenAI-format request against a
// Messages-only account: refused while conversion is off, converted and served
// once the site switch is on.
func TestAccountLayerProtocolGating(t *testing.T) {
	env := newAccountE2EEnv(t)
	var hits int
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	env.addAccount("messages-only", upstream.URL, []string{biz.NativeProtocolMessages},
		env.withAPIKeyCredential("sk-messages"), func(q *ent.UpstreamAccountCreate) {
			q.SetAllowConversion(true)
		})

	body := map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}

	// Conversion off: no account can serve this protocol. The client gets a
	// clean error naming neither the account nor the upstream.
	env.setConversion(false)
	w := env.chat(body, nil)
	if w.Code == http.StatusOK {
		t.Fatalf("messages-only account must not serve chat with conversion off: %s", w.Body.String())
	}
	if hits != 0 {
		t.Fatalf("upstream was contacted %d times with conversion off", hits)
	}
	if strings.Contains(w.Body.String(), "messages-only") || strings.Contains(w.Body.String(), upstream.URL) {
		t.Fatalf("error leaks upstream/account detail: %s", w.Body.String())
	}

	// Conversion on: the chat body is rewritten to Messages and served.
	env.setConversion(true)
	w = env.chat(body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if hits != 1 || gotPath != "/v1/messages" {
		t.Fatalf("hits=%d path=%q, want one request to /v1/messages", hits, gotPath)
	}
	if !strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("client did not receive a Chat Completions envelope: %s", w.Body.String())
	}
}

// TestAccountLayerUnsupportedProtocolIsClientError: with the site switch on
// but the account's own conversion off, every candidate is skipped. The client
// must learn its API format is the problem (400, naming the endpoint that
// works), not be told the service is unreachable and to retry.
func TestAccountLayerUnsupportedProtocolIsClientError(t *testing.T) {
	env := newAccountE2EEnv(t)
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env.addAccount("messages-only", upstream.URL, []string{biz.NativeProtocolMessages},
		env.withAPIKeyCredential("sk-messages"))
	env.setConversion(true)

	w := env.chat(map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if hits != 0 {
		t.Fatalf("upstream was contacted %d times", hits)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"code":"unsupported_protocol"`) || !strings.Contains(body, "/anthropic/v1/messages") {
		t.Fatalf("error does not name the problem and the working endpoint: %s", body)
	}
	if strings.Contains(body, "messages-only") || strings.Contains(body, upstream.URL) {
		t.Fatalf("error leaks upstream/account detail: %s", body)
	}
}

// TestAccountLayerRateLimitCoolsOnlyTheAccount pins the pool's core promise: a
// 429 parks the account that took it, and the group keeps serving through its
// other account.
func TestAccountLayerRateLimitCoolsOnlyTheAccount(t *testing.T) {
	env := newAccountE2EEnv(t)
	var limited, healthy int
	limitedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limited++
		w.Header().Set("anthropic-ratelimit-unified-reset", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer limitedUpstream.Close()
	healthyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthy++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer healthyUpstream.Close()

	limitedAcct := env.addAccountWithLink("limited", limitedUpstream.URL, []string{biz.NativeProtocolChat}, 10, 100,
		env.withAPIKeyCredential("sk-limited"))
	if mode := env.accounts.ResolvePoolMode(env.ctx, limitedAcct); mode != biz.PoolModeBalanced {
		t.Fatalf("pool mode = %q, want balanced (the fixture must not inherit another test's policy)", mode)
	}
	env.addAccountWithLink("healthy", healthyUpstream.URL, []string{biz.NativeProtocolChat}, 20, 100,
		env.withAPIKeyCredential("sk-healthy"))

	body := map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}

	w := env.chat(body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the group must fail over to its healthy account, got %d body=%s", w.Code, w.Body.String())
	}
	if healthy == 0 {
		t.Fatal("healthy account never served the failover")
	}

	// The limited account is now parked...
	acct, err := env.client.UpstreamAccount.Get(env.ctx, limitedAcct.ID)
	if err != nil {
		t.Fatalf("reload account: %v", err)
	}
	if acct.RateLimitResetAt == nil || !acct.RateLimitResetAt.After(time.Now()) {
		t.Fatalf("rate_limit_reset_at = %v, want a future instant from the upstream header; last_error_class=%q consecutive_failures=%d temp_until=%v status=%s schedulable=%v limited_hits=%d healthy_hits=%d first_status=%d",
			acct.RateLimitResetAt, acct.LastErrorClass, acct.ConsecutiveFailures, acct.TempUnschedulableUntil, acct.Status, acct.Schedulable, limited, healthy, w.Code)
	}
	if biz.AccountSchedulable(acct, env.model.Name, time.Now()) {
		t.Fatal("a cooling account must not be schedulable")
	}

	// ...and the next request goes straight to the healthy one.
	before := limited
	w = env.chat(body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if limited != before {
		t.Fatalf("the cooling account was retried %d times", limited-before)
	}
}

// TestAccountLayerRotationSkipsDisabledAccounts guards the scheduler's other
// half: an account parked by an operator stays out of rotation while its
// sibling keeps serving.
func TestAccountLayerRotationSkipsDisabledAccounts(t *testing.T) {
	env := newAccountE2EEnv(t)
	var parked, active int
	parkedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parked++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer parkedUpstream.Close()
	activeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer activeUpstream.Close()

	env.addAccountWithLink("parked", parkedUpstream.URL, []string{biz.NativeProtocolChat}, 5, 100,
		env.withAPIKeyCredential("sk-parked"), func(q *ent.UpstreamAccountCreate) {
			q.SetSchedulable(false)
		})
	env.addAccountWithLink("active", activeUpstream.URL, []string{biz.NativeProtocolChat}, 50, 100,
		env.withAPIKeyCredential("sk-active"))

	w := env.chat(map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if parked != 0 {
		t.Fatalf("a non-schedulable account was contacted %d times", parked)
	}
	if active == 0 {
		t.Fatal("the schedulable account never served")
	}
}

// TestCatalogTemplatesMapToAccountProtocols keeps the wizard honest: every
// catalog template the admin can pick maps onto a dialect the gateway speaks
// with, and the ones it does not support are refused rather than half-wired.
func TestCatalogTemplatesMapToAccountProtocols(t *testing.T) {
	cat := catalog.MustLoad()
	supported := map[string]bool{}
	for _, p := range cat.AuthProviders() {
		baseURL, kind, protocols, ok := biz.CatalogProviderDefaults(p.ID)
		if !ok {
			continue
		}
		supported[p.ID] = true
		if baseURL == "" || kind == "" {
			t.Fatalf("%s: incomplete defaults base=%q kind=%q", p.ID, baseURL, kind)
		}
		if len(protocols) == 0 {
			t.Fatalf("%s: no native protocol", p.ID)
		}
		for _, proto := range protocols {
			switch proto {
			case biz.NativeProtocolChat, biz.NativeProtocolResponses, biz.NativeProtocolMessages:
			default:
				t.Fatalf("%s: unsupported native protocol %q", p.ID, proto)
			}
		}
	}
	if len(supported) == 0 {
		t.Fatal("no catalog provider maps onto the gateway's dialects")
	}
}
