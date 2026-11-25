package llm

// Reservation-related tests on the llm layer. The money movement itself is
// covered by the Postgres-backed tests in internal/biz/billing_reserve_test.go;
// these cover the glue: the WS pump's per-turn billing gate, and the pure
// helpers that decide what a cancelled stream is charged for and what a
// request's reserve estimate is priced with.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgentask "github.com/jucode/jucode/internal/data/ent/generationtask"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

func TestBillingHoldKeysAreServerOwned(t *testing.T) {
	first := newBillingHoldKey("llm")
	second := newBillingHoldKey("llm")
	if first == second {
		t.Fatalf("two requests shared hold key %q", first)
	}
	if !strings.HasPrefix(first, "llm:") || !strings.HasPrefix(second, "llm:") {
		t.Fatalf("unexpected hold keys %q and %q", first, second)
	}

	taskID := uuid.New()
	if got, want := videoHoldKey(taskID), "video:"+taskID.String(); got != want {
		t.Fatalf("video hold key = %q, want %q", got, want)
	}
}

// A gate error must (a) keep the frame from ever reaching the upstream and
// (b) end the session — that is the "reserve failed, stop generating" path of
// the per-turn WS billing gate.
func TestPumpWebSocketsGateStopsSessionWithoutForwarding(t *testing.T) {
	var mu sync.Mutex
	var upstreamGot []string
	upstream := upstreamWSServer(t, func(c *websocket.Conn) {
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			mu.Lock()
			upstreamGot = append(upstreamGot, string(msg))
			mu.Unlock()
		}
	})
	defer upstream.Close()

	gate := func(frame []byte) error {
		if strings.Contains(string(frame), "response.create") {
			return errors.New("insufficient balance")
		}
		return nil
	}

	done := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		client, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer client.Close()
		up, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
		if err != nil {
			return
		}
		defer up.Close()
		pumpWebSockets(r.Context(), newWSConn(client), newWSConn(up), gate, nil)
	}))
	defer gateway.Close()

	cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer cli.Close()

	if err := cli.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update"}`)); err != nil {
		t.Fatalf("write allowed frame: %v", err)
	}
	if err := cli.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write gated frame: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not stop after the gate rejected a turn")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, msg := range upstreamGot {
		if strings.Contains(msg, "response.create") {
			t.Fatalf("gated frame reached the upstream: %q", msg)
		}
	}
}

// 流取消仍结算 (glue half): exact upstream-reported usage settles as-is —
// cache_read / cache_write columns included, never flattened into input; a
// disconnect before any usage frame reconstructs from observed output only
// (visible text + relayed tool-call arguments, never the reserve-time input
// overestimate) and flags the result as estimated so the settle caps at the
// hold. No observed work is not billable.
func TestCancelSettlementUsage(t *testing.T) {
	exact := &Accumulator{
		HasUsage: true, TokensIn: 100, TokensOut: 40,
		CachedTokensIn: 10, CacheCreationTokensIn: 7,
	}
	usage, estimated := cancelSettlementUsage(exact)
	if estimated {
		t.Fatal("exact usage must not be flagged estimated")
	}
	if usage.TokensIn != 100 || usage.TokensOut != 40 ||
		usage.CachedTokensIn != 10 || usage.CacheCreationTokensIn != 7 {
		t.Fatalf("exact usage mangled (cache split must survive): %+v", usage)
	}
	if !usageHasBillableWork(usage) {
		t.Fatal("exact usage must be billable")
	}

	partial := &Accumulator{}
	partial.AssistantText.WriteString(strings.Repeat("x", 300))
	usage, estimated = cancelSettlementUsage(partial)
	if !estimated {
		t.Fatal("reconstructed usage must be flagged estimated (settle caps at hold)")
	}
	if usage.TokensIn != 0 {
		t.Fatalf("reconstructed TokensIn = %d, want 0 (do not bill the reserve overestimate)", usage.TokensIn)
	}
	if usage.TokensOut != 100 { // 300 bytes / 3
		t.Fatalf("estimated TokensOut = %d, want 100", usage.TokensOut)
	}
	if !usageHasBillableWork(usage) {
		t.Fatal("observed output must be billable")
	}

	// Tool-call arguments the gateway already relayed are observed output
	// too: one finalized call and one still-open buffer (a cancel arrives
	// mid-call, before content_block_stop) both count, without double-count.
	withTools := &Accumulator{}
	withTools.AssistantText.WriteString(strings.Repeat("x", 90))
	finalized := &ToolCallSummary{Name: "search", argsBuf: []byte(strings.Repeat("a", 120))}
	finalized.finalize()
	open := &ToolCallSummary{Name: "edit", argsBuf: []byte(strings.Repeat("b", 90))}
	withTools.ToolCalls = []*ToolCallSummary{finalized, open}
	usage, estimated = cancelSettlementUsage(withTools)
	if !estimated {
		t.Fatal("tool-output reconstruction must be flagged estimated")
	}
	if usage.TokensOut != 100 { // (90 text + 120 finalized + 90 open) / 3
		t.Fatalf("estimated TokensOut with tool args = %d, want 100", usage.TokensOut)
	}
	if usage.TokensIn != 0 {
		t.Fatalf("tool-output reconstruction invented TokensIn = %d", usage.TokensIn)
	}

	empty, estimated := cancelSettlementUsage(&Accumulator{})
	if !estimated {
		t.Fatal("empty cancel must still be flagged estimated")
	}
	if usageHasBillableWork(empty) {
		t.Fatalf("empty cancel must not be billable: %+v", empty)
	}
	if usageHasBillableWork(biz.TokenUsage{}) {
		t.Fatal("zero TokenUsage must not be billable")
	}
}

// 499 + 真实 usage 必须与成功路径同口径：cache 三分原样结算，且对
// estimate_cache_write 渠道跑同一套缓存写入改判（ApplyCacheWriteEstimate）。
// 无 usage 的 499 绝不改判——没有观察到的输入不允许被编造成 cache_write。
func TestCancelBillableUsage(t *testing.T) {
	estProv := &ent.UpstreamAccount{EstimateCacheWrite: true, CacheWriteMinContextTokens: 1024}
	plainProv := &ent.UpstreamAccount{}

	// Exact usage on an estimating provider: the uncached input remainder is
	// reclassified to cache_write, exactly like chargeAndLog would.
	acc := &Accumulator{HasUsage: true, TokensIn: 5000, CachedTokensIn: 90000, TokensOut: 12}
	usage, estimated, cw := cancelBillableUsage(estProv, acc)
	if estimated {
		t.Fatal("HasUsage must not be treated as an estimate")
	}
	if !cw {
		t.Fatal("cache-write estimate must fire on the exact-usage cancel path")
	}
	if usage.TokensIn != 0 || usage.CacheCreationTokensIn != 5000 || usage.CachedTokensIn != 90000 || usage.TokensOut != 12 {
		t.Fatalf("cache-write reclassification wrong: %+v", usage)
	}

	// Upstream-reported cache_creation always wins — never overwritten.
	real := &Accumulator{HasUsage: true, TokensIn: 100, CachedTokensIn: 500, CacheCreationTokensIn: 200, TokensOut: 3}
	usage, estimated, cw = cancelBillableUsage(estProv, real)
	if estimated || cw {
		t.Fatalf("real cache_creation must not be re-estimated (estimated=%v cw=%v)", estimated, cw)
	}
	if usage.TokensIn != 100 || usage.CachedTokensIn != 500 || usage.CacheCreationTokensIn != 200 {
		t.Fatalf("real cache split mangled: %+v", usage)
	}

	// Non-estimating provider: exact usage passes through untouched.
	usage, estimated, cw = cancelBillableUsage(plainProv, acc)
	if estimated || cw {
		t.Fatalf("plain provider must not estimate (estimated=%v cw=%v)", estimated, cw)
	}
	if usage.TokensIn != 5000 || usage.CachedTokensIn != 90000 {
		t.Fatalf("plain provider usage mangled: %+v", usage)
	}

	// No usage frame: output-only reconstruction, and the cache-write
	// estimate must NOT run — there is no observed input to reclassify.
	partial := &Accumulator{}
	partial.AssistantText.WriteString(strings.Repeat("x", 3000))
	usage, estimated, cw = cancelBillableUsage(estProv, partial)
	if !estimated {
		t.Fatal("no-usage cancel must be flagged estimated")
	}
	if cw {
		t.Fatal("no-usage cancel must never run the cache-write estimate")
	}
	if usage.TokensIn != 0 || usage.CacheCreationTokensIn != 0 {
		t.Fatalf("no-usage cancel invented input-side tokens: %+v", usage)
	}
	if usage.TokensOut != 1000 {
		t.Fatalf("no-usage cancel TokensOut = %d, want 1000", usage.TokensOut)
	}

	// Nil provider (exhausted-path synthetic) is safe and a no-op.
	usage, estimated, cw = cancelBillableUsage(nil, acc)
	if estimated || cw || usage.TokensIn != 5000 {
		t.Fatalf("nil provider handling wrong: estimated=%v cw=%v usage=%+v", estimated, cw, usage)
	}
}

// logFinal is only reached after failover cannot continue. A last-hop
// failure that already observed usage (Anthropic message_start cache
// split, streamed output, response.failed after tokens) must settle the
// same way a 499 hang-up does — otherwise the upstream bills the hop
// and we Release the hold. Zero-usage failures still Release.
func TestFailureShouldSettle(t *testing.T) {
	plain := &ent.UpstreamAccount{}
	route := &biz.Route{}

	acc := &Accumulator{
		HasUsage: true, TokensIn: 80, CachedTokensIn: 900,
		CacheCreationTokensIn: 400, TokensOut: 6,
	}
	usage, estimated, cw, settle := failureShouldSettle(route, plain, acc)
	if !settle {
		t.Fatal("stream_incomplete with message_start usage must settle")
	}
	if estimated || cw {
		t.Fatalf("exact usage must not be estimated: estimated=%v cw=%v", estimated, cw)
	}
	if usage.TokensIn != 80 || usage.CachedTokensIn != 900 ||
		usage.CacheCreationTokensIn != 400 || usage.TokensOut != 6 {
		t.Fatalf("cache split mangled: %+v", usage)
	}

	if _, _, _, settle = failureShouldSettle(route, plain, &Accumulator{}); settle {
		t.Fatal("zero-usage last-hop failure must release")
	}
	if _, _, _, settle = failureShouldSettle(nil, plain, acc); settle {
		t.Fatal("nil route must release")
	}

	partial := &Accumulator{}
	partial.AssistantText.WriteString(strings.Repeat("x", 300))
	usage, estimated, cw, settle = failureShouldSettle(route, plain, partial)
	if !settle || !estimated || cw {
		t.Fatalf("visible output without usage frame must settle estimated: settle=%v estimated=%v cw=%v", settle, estimated, cw)
	}
	if usage.TokensOut != 100 || usage.TokensIn != 0 {
		t.Fatalf("reconstructed usage wrong: %+v", usage)
	}

	estProv := &ent.UpstreamAccount{EstimateCacheWrite: true, CacheWriteMinContextTokens: 1024}
	rewrite := &Accumulator{HasUsage: true, TokensIn: 5000, CachedTokensIn: 90000, TokensOut: 12}
	usage, estimated, cw, settle = failureShouldSettle(route, estProv, rewrite)
	if !settle || estimated || !cw {
		t.Fatalf("estimate_cache_write must fire on exact last-hop usage: settle=%v estimated=%v cw=%v", settle, estimated, cw)
	}
	if usage.TokensIn != 0 || usage.CacheCreationTokensIn != 5000 {
		t.Fatalf("cache-write reclassification wrong: %+v", usage)
	}
}

func TestRequestedMaxOutputTokens(t *testing.T) {
	cases := []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"max_tokens": float64(2048)}, 2048},
		{map[string]any{"max_completion_tokens": float64(1024)}, 1024},
		{map[string]any{"max_output_tokens": float64(512)}, 512},
		// max_output_tokens (responses dialect) wins over legacy max_tokens.
		{map[string]any{"max_output_tokens": float64(512), "max_tokens": float64(2048)}, 512},
		{map[string]any{}, 0},
		{map[string]any{"max_tokens": "not-a-number"}, 0},
	}
	for _, c := range cases {
		if got := requestedMaxOutputTokens(c.body); got != c.want {
			t.Errorf("requestedMaxOutputTokens(%v) = %d, want %d", c.body, got, c.want)
		}
	}
}

func TestRequestedVideoSeconds(t *testing.T) {
	cases := []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"seconds": "8"}, 8},   // Sora string enum
		{map[string]any{"seconds": 12.0}, 12}, // bare number
		{map[string]any{"duration": 6.0}, 6},
		{map[string]any{}, 0},
		{map[string]any{"seconds": "fast"}, 0},
	}
	for _, c := range cases {
		if got := requestedVideoSeconds(c.body); got != c.want {
			t.Errorf("requestedVideoSeconds(%v) = %d, want %d", c.body, got, c.want)
		}
	}
}

// The reserve estimator must over-count, never under-count, and a model with
// no pricing must estimate zero (fail open on estimation, closed on charge).
func TestEstimateInputTokensBounds(t *testing.T) {
	if got := biz.EstimateInputTokens(0); got != 64 {
		t.Fatalf("floor = %d, want 64", got)
	}
	if got := biz.EstimateInputTokens(10_000); got != 5_000 {
		t.Fatalf("bytes/2 = %d, want 5000", got)
	}
	if got := biz.EstimateInputTokens(1 << 30); got != 262144 {
		t.Fatalf("cap = %d, want 262144", got)
	}
}

// Postgres-backed (same JUCODE_TEST_PG_DSN opt-in as internal/biz): a
// per-second video model whose completion carries no duration must NOT be
// billed at zero — the task stays pending (billed=false, non-terminal), so a
// later poll with the real duration settles it, and a retry with the duration
// completes normally.
func TestVideoSettleMissingDurationLeavesTaskPending(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed video settle test")
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
	d := data.NewWith(client, nil, db)
	settings := biz.NewSettingsSvc(d)
	billing := biz.NewBillingSvc(d, biz.NewPlanSvc(d, settings), settings)
	tasks := biz.NewTaskSvc(d)
	g := &Gateway{billing: billing, tasks: tasks}

	user, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		SetBalance(decimal.RequireFromString("100")).
		Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	group, err := client.ModelGroup.Create().
		SetName("vid-grp-" + uuid.NewString()).
		SetBillingSource(entgroup.BillingSourceBalanceOnly).
		Save(ctx)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	modelName := "video-model-" + uuid.NewString()
	if _, err := client.Model.Create().
		SetName(modelName).
		SetBillingMode(entmodel.BillingModePerRequest).
		SetPricePerRequest(decimal.RequireFromString("1")).
		SetOutputSecondPrice(decimal.RequireFromString("2")).
		Save(ctx); err != nil {
		t.Fatalf("create model: %v", err)
	}
	task, err := client.GenerationTask.Create().
		SetUserID(user.ID).
		SetRequestID(uuid.NewString()).
		SetModel(modelName).
		SetGroupID(group.ID).
		SetStatus(entgentask.StatusRunning).
		Save(ctx)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Missing duration → refuse to settle, no billed flag, no charge.
	if ok := g.settleVideoCompletion(ctx, task, 0); ok {
		t.Fatal("settle with missing duration reported success")
	}
	got, _ := client.GenerationTask.Get(ctx, task.ID)
	if got.Billed {
		t.Fatal("task was marked billed despite missing duration")
	}
	u, _ := client.User.Get(ctx, user.ID)
	if !u.Balance.Equal(decimal.RequireFromString("100")) {
		t.Fatalf("balance moved on refused settle: %s", u.Balance)
	}

	// Retry with the duration → settles, charges 1 + 2*3 = 7, billed flips.
	if ok := g.settleVideoCompletion(ctx, task, 3); !ok {
		t.Fatal("settle with duration failed")
	}
	got, _ = client.GenerationTask.Get(ctx, task.ID)
	if !got.Billed {
		t.Fatal("task not billed after retry with duration")
	}
	u, _ = client.User.Get(ctx, user.ID)
	if !u.Balance.Equal(decimal.RequireFromString("93")) {
		t.Fatalf("balance after settle = %s, want 93", u.Balance)
	}
}
