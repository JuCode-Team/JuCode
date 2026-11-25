package llm

// Postgres-backed tests for cross-group retry (product rule D8=B), behind
// the same JUCODE_TEST_PG_DSN opt-in as internal/biz. They drive the real
// gateway handler against httptest upstreams and assert the five product
// rules end to end:
//
//	同 source 可切         — TestCrossGroupRetrySwitchesToSameSourceGroup
//	不同 source 不可切     — TestCrossGroupRetryDoesNotCrossBillingSource
//	超倍率不可切           — TestCrossGroupRetryRespectsRateCap
//	已写下游字节后不可切   — TestCrossGroupRetryStopsAfterBytesWritten
//	媒体不切               — TestCrossGroupRetrySkipsMediaModels
//
// plus the hold re-pricing contract (keep / top-up / stop, never two live
// holds) in TestCrossGroupReholdRepricesSingleHold.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entbillinghold "github.com/jucode/jucode/internal/data/ent/billinghold"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/middleware"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/providerkit"
)

type crossGroupEnv struct {
	t      *testing.T
	ctx    context.Context
	client *ent.Client
	gw     *Gateway
	cipher *cryptopkg.Cipher
	user   *ent.User
	apiKey *ent.ApiKey
	model  *ent.Model
}

func newCrossGroupEnv(t *testing.T, autoRoute bool, maxRate float64) *crossGroupEnv {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed cross-group test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	// The full gateway path touches Redis (sticky sessions, runtime
	// cooldowns), so these tests also need a disposable Redis alongside the
	// Postgres DSN.
	redisAddr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unreachable at %s (%v); skipping gateway cross-group test", redisAddr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	d := data.NewWith(client, rdb, db)

	cipher, err := cryptopkg.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	settings := biz.NewSettingsSvc(d)
	plans := biz.NewPlanSvc(d, settings)
	billing := biz.NewBillingSvc(d, plans, settings)
	accounts := biz.NewAccountPoolSvc(d, cipher, settings)
	accounts.SetDefinitions(biz.NewProviderDefinitionSvc(d))
	gw := NewGateway(
		biz.NewModelSvc(d),
		billing,
		biz.NewConcurrencyGate(d, plans, settings),
		biz.NewStickySvc(d, settings),
		biz.NewPolicySvc(d),
		biz.NewIncidentSvc(d),
		settings,
		cipher,
		4, /*maxAttempts*/
		0, /*maxBodyBytes*/
		30*time.Second,
	)
	gw.SetAccounts(accounts)
	gw.SetWebTools(biz.NewWebToolSvc(d))

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
		SetPrefix("sk-test").
		SetHash(uuid.NewString()).
		SetAutoRoute(autoRoute).
		SetAutoRouteMaxRate(maxRate).
		Save(ctx)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return &crossGroupEnv{t: t, ctx: ctx, client: client, gw: gw, cipher: cipher, user: user, apiKey: apiKey}
}

func (e *crossGroupEnv) createModel(name string) {
	e.t.Helper()
	m, err := e.client.Model.Create().
		SetName(name).
		SetBillingMode(entmodel.BillingModePerToken).
		SetInputUnitPrice(decimal.RequireFromString("0.001")).
		SetOutputUnitPrice(decimal.RequireFromString("0.002")).
		Save(e.ctx)
	if err != nil {
		e.t.Fatalf("create model: %v", err)
	}
	e.model = m
}

// addGroup wires model → group → account → upstream URL in one call.
func (e *crossGroupEnv) addGroup(name string, source entgroup.BillingSource, rate float64, upstreamURL string) *ent.ModelGroup {
	e.t.Helper()
	group, err := e.client.ModelGroup.Create().
		SetName(name).
		SetBillingSource(source).
		SetRateMultiplier(rate).
		Save(e.ctx)
	if err != nil {
		e.t.Fatalf("create group: %v", err)
	}
	if _, err := e.client.ModelGroupModel.Create().
		SetGroupID(group.ID).SetModelID(e.model.ID).Save(e.ctx); err != nil {
		e.t.Fatalf("link model: %v", err)
	}
	plain, err := providerkit.NewAPIKeyCredential("upstream-key").Encode()
	if err != nil {
		e.t.Fatalf("encode credential: %v", err)
	}
	enc, err := e.cipher.Encrypt(plain)
	if err != nil {
		e.t.Fatalf("encrypt: %v", err)
	}
	acct, err := e.client.UpstreamAccount.Create().
		SetName(name + "-account").
		SetBaseURL(upstreamURL).
		SetCredentialEnc(enc).
		SetNativeProtocols([]string{biz.NativeProtocolChat}).
		SetStatus(entaccount.StatusActive).
		SetSchedulable(true).
		Save(e.ctx)
	if err != nil {
		e.t.Fatalf("create account: %v", err)
	}
	if _, err := e.client.GroupAccount.Create().
		SetGroupID(group.ID).SetAccountID(acct.ID).SetEnabled(true).Save(e.ctx); err != nil {
		e.t.Fatalf("link account: %v", err)
	}
	if _, err := e.client.UpstreamAccountModel.Create().
		SetAccountID(acct.ID).SetModelName(e.model.Name).SetEnabled(true).Save(e.ctx); err != nil {
		e.t.Fatalf("account model: %v", err)
	}
	return group
}

func (e *crossGroupEnv) doChat(requestID string, body map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	gin.SetMode(gin.TestMode)
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID)
	c.Request = req
	c.Set(middleware.CtxUser, e.user)
	c.Set(middleware.CtxApiKey, e.apiKey)
	e.gw.HandleChatCompletions(c)
	return w
}

func (e *crossGroupEnv) balance() decimal.Decimal {
	e.t.Helper()
	u, err := e.client.User.Get(e.ctx, e.user.ID)
	if err != nil {
		e.t.Fatalf("reload user: %v", err)
	}
	return u.Balance
}

func (e *crossGroupEnv) deductCount() int {
	e.t.Helper()
	n, err := e.client.BalanceTx.Query().
		Where(entbalancetx.UserIDEQ(e.user.ID), entbalancetx.KindEQ("deduct")).
		Count(e.ctx)
	if err != nil {
		e.t.Fatalf("count deducts: %v", err)
	}
	return n
}

func (e *crossGroupEnv) liveHolds() int {
	e.t.Helper()
	n, err := e.client.BillingHold.Query().
		Where(entbillinghold.UserIDEQ(e.user.ID), entbillinghold.StatusEQ("held")).
		Count(e.ctx)
	if err != nil {
		e.t.Fatalf("count holds: %v", err)
	}
	return n
}

func upstream500(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func upstream200(t *testing.T, hits *int32, marker string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"` + marker + `"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// upstreamStreamThenDie answers 200, streams one SSE frame (so bytes reach
// the client) and then kills the connection mid-stream.
func upstreamStreamThenDie(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("recorder not hijackable")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		frame := "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		chunk := func(s string) string {
			return strings.ToLower(strings.TrimLeft(intToHex(len(s)), "0x")) + "\r\n" + s + "\r\n"
		}
		_, _ = buf.WriteString(chunk(frame))
		_ = buf.Flush()
		// Close without the terminal 0-length chunk → the gateway sees an
		// unexpected EOF after having relayed real bytes downstream.
	}))
	t.Cleanup(s.Close)
	return s
}

func intToHex(n int) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(digits[n%16]) + out
		n /= 16
	}
	return out
}

// 同 source 可切: preferred group fails on every provider before anything is
// written → the request retries on the same-billing-source sibling, the
// client gets that group's 200, and exactly one deduct row lands (the
// re-priced hold settled once — never two charges).
func TestCrossGroupRetrySwitchesToSameSourceGroup(t *testing.T) {
	env := newCrossGroupEnv(t, true /*autoRoute*/, 0 /*no rate cap*/)
	env.createModel("cg-model-" + uuid.NewString())
	var aHits, bHits int32
	groupA := env.addGroup("cg-a-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1.0, upstream500(t, &aHits).URL)
	groupB := env.addGroup("cg-b-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 2.0, upstream200(t, &bHits, "ok-from-b").URL)
	_ = groupA

	rid := "cg-switch-" + uuid.NewString()
	w := env.doChat(rid, map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ok-from-b") {
		t.Fatalf("response did not come from the fallback group: %s", w.Body.String())
	}
	if atomic.LoadInt32(&aHits) == 0 || atomic.LoadInt32(&bHits) != 1 {
		t.Fatalf("hits a=%d b=%d, want preferred tried first then exactly one fallback attempt", aHits, bHits)
	}
	// 禁止重复扣两份: one deduct row, no hold left frozen, and the balance
	// moved by exactly that single deduct.
	if n := env.deductCount(); n != 1 {
		t.Fatalf("deduct rows = %d, want 1", n)
	}
	if n := env.liveHolds(); n != 0 {
		t.Fatalf("live holds = %d, want 0", n)
	}
	if !env.balance().LessThan(decimal.RequireFromString("100")) {
		t.Fatal("success on fallback group did not charge")
	}
	// The usage row must carry the group that actually served the request.
	row, err := env.client.UsageLog.Query().Where(entusagelog.RequestIDEQ(rid)).Only(env.ctx)
	if err != nil {
		t.Fatalf("usage log: %v", err)
	}
	if row.GroupID == nil || *row.GroupID != groupB.ID {
		t.Fatalf("usage log group = %v, want fallback group %s", row.GroupID, groupB.ID)
	}
}

// 不同 source 不可切: the only sibling bills from a different source →
// no failover, the client sees the upstream failure, nothing is charged.
func TestCrossGroupRetryDoesNotCrossBillingSource(t *testing.T) {
	env := newCrossGroupEnv(t, true, 0)
	env.createModel("cg-model-" + uuid.NewString())
	var aHits, bHits int32
	env.addGroup("cg-a-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1.0, upstream500(t, &aHits).URL)
	env.addGroup("cg-b-"+uuid.NewString(), entgroup.BillingSourcePlanOnly, 1.0, upstream200(t, &bHits, "must-not-serve").URL)

	w := env.doChat("cg-src-"+uuid.NewString(), map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&bHits) != 0 {
		t.Fatalf("plan-only sibling served %d cross-billing-source attempts", bHits)
	}
	if n := env.deductCount(); n != 0 {
		t.Fatalf("deduct rows = %d, want 0 (nothing delivered)", n)
	}
	if n := env.liveHolds(); n != 0 {
		t.Fatalf("live holds = %d, want 0 (failure path must release)", n)
	}
	if !env.balance().Equal(decimal.RequireFromString("100")) {
		t.Fatalf("balance moved on a failed request: %s", env.balance())
	}
}

// 超倍率不可切: the sibling's rate_multiplier exceeds the key's
// auto_route_max_rate → it is not even a candidate.
func TestCrossGroupRetryRespectsRateCap(t *testing.T) {
	env := newCrossGroupEnv(t, true, 2.0 /*cap*/)
	env.createModel("cg-model-" + uuid.NewString())
	var aHits, bHits int32
	env.addGroup("cg-a-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1.0, upstream500(t, &aHits).URL)
	env.addGroup("cg-b-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 3.0, upstream200(t, &bHits, "too-expensive").URL)

	w := env.doChat("cg-rate-"+uuid.NewString(), map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&bHits) != 0 {
		t.Fatalf("over-cap group served %d attempts", bHits)
	}
	if n := env.liveHolds(); n != 0 {
		t.Fatalf("live holds = %d, want 0", n)
	}
}

// 已写下游字节后不可切: the preferred group's provider dies mid-stream AFTER
// relaying bytes → the request terminates on that group (abort frame), the
// sibling is never contacted.
func TestCrossGroupRetryStopsAfterBytesWritten(t *testing.T) {
	env := newCrossGroupEnv(t, true, 0)
	env.createModel("cg-model-" + uuid.NewString())
	var aHits, bHits int32
	env.addGroup("cg-a-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1.0, upstreamStreamThenDie(t, &aHits).URL)
	env.addGroup("cg-b-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 2.0, upstream200(t, &bHits, "must-not-serve").URL)

	w := env.doChat("cg-written-"+uuid.NewString(), map[string]any{
		"model":    env.model.Name,
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if atomic.LoadInt32(&aHits) != 1 {
		t.Fatalf("preferred group hits = %d, want 1", aHits)
	}
	if atomic.LoadInt32(&bHits) != 0 {
		t.Fatalf("fallback group served %d attempts after bytes were written", bHits)
	}
	body := w.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("relayed frame missing from response: %s", body)
	}
	if n := env.liveHolds(); n != 0 {
		t.Fatalf("live holds = %d, want 0", n)
	}
}

// 媒体不切: image-generation-style models never fail over across groups.
func TestCrossGroupRetrySkipsMediaModels(t *testing.T) {
	env := newCrossGroupEnv(t, true, 0)
	env.createModel("gpt-image-cg-" + uuid.NewString())
	var aHits, bHits int32
	env.addGroup("cg-a-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1.0, upstream500(t, &aHits).URL)
	env.addGroup("cg-b-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 2.0, upstream200(t, &bHits, "must-not-serve").URL)

	w := env.doChat("cg-media-"+uuid.NewString(), map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "draw a cat"}},
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(&bHits) != 0 {
		t.Fatalf("media request failed over across groups (%d hits)", bHits)
	}
}

// Hold re-pricing contract: cheaper/equal keeps the original hold, pricier
// releases it and takes exactly one bigger one, a top-up beyond the balance
// falls back to the last-request hold, and a re-reserve the account cannot
// admit stops the failover with every hold of this request returned.
func TestCrossGroupReholdRepricesSingleHold(t *testing.T) {
	env := newCrossGroupEnv(t, true, 0)
	env.createModel("cg-model-" + uuid.NewString())
	groupA := env.addGroup("cg-a-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1.0, "http://unused.invalid")
	groupCheap := env.addGroup("cg-cheap-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 0.5, "http://unused.invalid")
	groupPricey := env.addGroup("cg-pricey-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 10.0, "http://unused.invalid")
	groupHuge := env.addGroup("cg-huge-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1e9, "http://unused.invalid")

	est := biz.CostEstimateInput{BodyBytes: 128, MaxOutputTokens: 1000}
	ctx := env.ctx
	billing := env.gw.billing

	amountA := billing.EstimateMaxCost(ctx, env.model, groupA, est)
	res, err := billing.Reserve(ctx, biz.ReserveInput{
		User: env.user, ApiKey: env.apiKey, Group: groupA,
		Key: newBillingHoldKey("llm"), Amount: amountA, TTL: biz.ReserveTTLRequest,
		Note: "test primary reserve",
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	lc := logContext{Reservation: res}
	f := newGroupFailover(env.gw, env.user, env.apiKey, biz.RequestProtocolOpenAI,
		"req-rehold", est, false)

	// Cheaper group → original hold kept untouched.
	if !f.rehold(ctx, &biz.Route{Model: env.model, Group: groupCheap}, nil, &lc) {
		t.Fatal("rehold to cheaper group failed")
	}
	if lc.Reservation.HoldID != res.HoldID {
		t.Fatal("cheaper group replaced the hold; it should have kept it")
	}

	// Pricier group → old hold released, ONE bigger hold live, balance
	// reflects exactly that single hold.
	if !f.rehold(ctx, &biz.Route{Model: env.model, Group: groupPricey}, nil, &lc) {
		t.Fatal("rehold to pricier group failed")
	}
	if lc.Reservation.HoldID == res.HoldID {
		t.Fatal("pricier group kept the too-small hold")
	}
	if n := env.liveHolds(); n != 1 {
		t.Fatalf("live holds after top-up = %d, want exactly 1 (no double hold)", n)
	}
	wantHold := billing.EstimateMaxCost(ctx, env.model, groupPricey, est)
	if !lc.Reservation.Amount.Equal(wantHold) {
		t.Fatalf("topped-up hold = %s, want %s", lc.Reservation.Amount, wantHold)
	}
	if !env.balance().Equal(decimal.RequireFromString("100").Sub(wantHold)) {
		t.Fatalf("balance = %s, want 100 - %s", env.balance(), wantHold)
	}

	// A top-up above the whole balance, nothing else in flight → the
	// last-request path freezes every remaining cent instead of refusing
	// (biz.decideMonetaryReserve). A direct request to this group would be
	// admitted the same way, so failover must not be stricter. Still one hold.
	if !f.rehold(ctx, &biz.Route{Model: env.model, Group: groupHuge}, nil, &lc) {
		t.Fatal("rehold above the balance should take the last-request hold")
	}
	if n := env.liveHolds(); n != 1 {
		t.Fatalf("live holds after last-request top-up = %d, want exactly 1 (no double hold)", n)
	}
	if !lc.Reservation.Amount.Equal(decimal.RequireFromString("100")) {
		t.Fatalf("last-request hold = %s, want the whole 100 balance", lc.Reservation.Amount)
	}
	if !env.balance().Equal(decimal.Zero) {
		t.Fatalf("balance = %s, want 0 (last-request freezes everything)", env.balance())
	}

	// Same top-up while another llm: request is in flight: last-request may
	// not run in parallel, so the re-reserve is refused — and the hold this
	// failover released is not taken back. Only the concurrent request stays
	// frozen.
	if err := billing.ReleaseReservation(ctx, lc.Reservation); err != nil {
		t.Fatalf("release last-request hold: %v", err)
	}
	if _, err := billing.Reserve(ctx, biz.ReserveInput{
		User: env.user, ApiKey: env.apiKey, Group: groupA,
		Key: newBillingHoldKey("llm"), Amount: decimal.RequireFromString("1"),
		TTL: biz.ReserveTTLRequest, Note: "test concurrent request",
	}); err != nil {
		t.Fatalf("concurrent reserve: %v", err)
	}
	own, err := billing.Reserve(ctx, biz.ReserveInput{
		User: env.user, ApiKey: env.apiKey, Group: groupA,
		Key: newBillingHoldKey("llm"), Amount: amountA, TTL: biz.ReserveTTLRequest,
		Note: "test reserve alongside an in-flight request",
	})
	if err != nil {
		t.Fatalf("reserve alongside in-flight request: %v", err)
	}
	lc.Reservation = own
	if f.rehold(ctx, &biz.Route{Model: env.model, Group: groupHuge}, nil, &lc) {
		t.Fatal("rehold to an unaffordable group succeeded while another request was in flight")
	}
	if n := env.liveHolds(); n != 1 {
		t.Fatalf("live holds after refused top-up = %d, want 1 (only the concurrent request)", n)
	}
	if !env.balance().Equal(decimal.RequireFromString("99")) {
		t.Fatalf("balance after refused top-up = %s, want 99 (only the concurrent hold frozen)", env.balance())
	}
}

// The hold a cross-group top-up takes must keep counting as this account's
// one in-flight interactive request. Last-request serialization only sees
// llm:/ws: request keys, so a re-reserve filed under any other prefix would
// go invisible and let a second request drain the same balance beside it —
// which is exactly what /v1/messages did while it passed its own hold scope.
func TestCrossGroupReholdHoldStillSerializesLastRequest(t *testing.T) {
	env := newCrossGroupEnv(t, true, 0)
	env.createModel("cg-model-" + uuid.NewString())
	groupA := env.addGroup("cg-a-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 1.0, "http://unused.invalid")
	groupPricey := env.addGroup("cg-pricey-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 10.0, "http://unused.invalid")

	est := biz.CostEstimateInput{BodyBytes: 128, MaxOutputTokens: 1000}
	ctx := env.ctx
	billing := env.gw.billing

	res, err := billing.Reserve(ctx, biz.ReserveInput{
		User: env.user, ApiKey: env.apiKey, Group: groupA,
		Key: newBillingHoldKey("llm"), Amount: billing.EstimateMaxCost(ctx, env.model, groupA, est),
		TTL: biz.ReserveTTLRequest, Note: "test primary reserve",
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	lc := logContext{Reservation: res}
	f := newGroupFailover(env.gw, env.user, env.apiKey, biz.RequestProtocolOpenAI,
		"req-serialize", est, false)
	if !f.rehold(ctx, &biz.Route{Model: env.model, Group: groupPricey}, nil, &lc) {
		t.Fatal("rehold to pricier group failed")
	}
	frozen := lc.Reservation.Amount

	// A second request that can only go through as a last request: the
	// top-up hold above is the other in-flight one, so it must be refused.
	_, err = billing.Reserve(ctx, biz.ReserveInput{
		User: env.user, ApiKey: env.apiKey, Group: groupA,
		Key: newBillingHoldKey("llm"), Amount: decimal.RequireFromString("1000"),
		TTL: biz.ReserveTTLRequest, Note: "test second request",
	})
	if !errors.Is(err, biz.ErrInsufficientBalance) {
		t.Fatalf("second last-request reserve err = %v, want ErrInsufficientBalance "+
			"(the cross-group hold is invisible to serialization)", err)
	}
	if n := env.liveHolds(); n != 1 {
		t.Fatalf("live holds = %d, want 1 (only the cross-group top-up)", n)
	}
	if want := decimal.RequireFromString("100").Sub(frozen); !env.balance().Equal(want) {
		t.Fatalf("balance = %s, want %s (refused reserve froze nothing)", env.balance(), want)
	}
}

// X-JuCode-Billing-Source narrows auto-routing to one billing source: the
// cheaper plan group is skipped when the caller asked for balance billing.
func TestAutoRouteHonorsBillingSourceFilter(t *testing.T) {
	env := newCrossGroupEnv(t, true /*autoRoute*/, 0 /*no rate cap*/)
	env.createModel("cg-model-" + uuid.NewString())
	var planHits, balanceHits int32
	env.addGroup("cg-plan-"+uuid.NewString(), entgroup.BillingSourcePlanOnly, 0.5, upstream200(t, &planHits, "from-plan").URL)
	env.addGroup("cg-balance-"+uuid.NewString(), entgroup.BillingSourceBalanceOnly, 2.0, upstream200(t, &balanceHits, "from-balance").URL)

	gin.SetMode(gin.TestMode)
	raw, _ := json.Marshal(map[string]any{
		"model":    env.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(biz.WithBillingSourceFilter(req.Context(), entgroup.BillingSourceBalanceOnly))
	c.Set(middleware.CtxUser, env.user)
	c.Set(middleware.CtxApiKey, env.apiKey)
	env.gw.HandleChatCompletions(c)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from-balance") {
		t.Fatalf("status = %d body = %s, want the balance group", w.Code, w.Body.String())
	}
	if planHits != 0 {
		t.Fatalf("plan group was tried %d times despite the balance filter", planHits)
	}
}
