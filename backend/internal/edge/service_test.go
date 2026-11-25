package edge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/region"
)

// newTestService wires a Service around miniredis and a temp-dir outbox,
// operating in standalone mode (no control plane): reservations are seeded
// from the account snapshot balance, which is exactly the local reserve/settle
// hot path under audit.
func newTestService(t *testing.T, upstreamURL string) (*Service, *region.AccountSnapshot, string, string) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	outboxDir := filepath.Join(t.TempDir(), "outbox")
	outbox, err := NewFileOutbox(outboxDir)
	if err != nil {
		t.Fatal(err)
	}

	secret := "sk-juc-test-secret"
	hash := hashSecret(secret)
	snapshot := &region.AccountSnapshot{
		UserID: uuid.New(), APIKeyID: uuid.New(), APIKeyHash: hash,
		APIKeyName: "test-key", Status: "active",
		AllowedModels: []string{"test-model"},
		ModelPrices: map[string]region.ModelPrice{
			"test-model": {BillingMode: "per_request", PerRequestCNY: "0.5"},
		},
		BalanceCNY: "10",
	}

	cfg := Config{
		RegionID: "test-1", MaxBodyBytes: 1 << 20,
		AccountTTL: time.Minute, RequestTTL: time.Minute, SyncInterval: time.Second,
	}
	s := &Service{
		cfg:    cfg,
		ledger: NewLedger(rdb, "jucode:edge:"+cfg.RegionID, cfg.RequestTTL),
		rdb:    rdb, outbox: outbox,
		hc: &http.Client{}, streamHC: &http.Client{},
		lease: 10_000_000, reserve: 1_000_000, overdraft: 0,
		accounts: map[string]cachedAccount{
			hash: {account: snapshot, expires: time.Now().Add(time.Hour)},
		},
		leases: map[uuid.UUID]localLease{},
		upstreams: []cachedUpstream{{
			snapshot: region.ProviderSnapshot{
				ID: uuid.New(), Enabled: true, BaseURL: upstreamURL,
				Models: []string{"test-model"},
			},
			apiKey: "sk-upstream",
		}},
	}
	return s, snapshot, secret, outboxDir
}

func newUpstream(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","usage":{"prompt_tokens":10,"completion_tokens":10}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func doProxyRequest(handler http.Handler, secret, clientRequestID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	if clientRequestID != "" {
		req.Header.Set("X-Request-ID", clientRequestID)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// A replayed client X-Request-ID must not reuse a settled reservation: every
// request gets a fresh server-side billing ID, so both calls hit upstream AND
// both are charged. Before the fix, the second call reused the settled
// request key and was never charged locally.
func TestProxyReplayedClientRequestIDIsChargedEveryTime(t *testing.T) {
	calls := 0
	upstream := newUpstream(t, &calls)
	s, snapshot, secret, _ := newTestService(t, upstream.URL)
	handler := s.Handler()
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if rec := doProxyRequest(handler, secret, "replayed-client-id"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d; want 2", calls)
	}

	state, err := s.ledger.Account(ctx, snapshot.UserID)
	if err != nil {
		t.Fatal(err)
	}
	// 10 CNY - 2 x 0.5 CNY per-request price.
	if state.Available != 9_000_000 || state.Reserved != 0 {
		t.Fatalf("available = %d reserved = %d; want 9000000 / 0 (both requests charged)", state.Available, state.Reserved)
	}

	pending, err := s.outbox.Pending(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("outbox pending = %d, %v; want 2 events", len(pending), err)
	}
	if pending[0].RequestID == pending[1].RequestID {
		t.Fatalf("both events share billing id %q; server must mint a fresh one per request", pending[0].RequestID)
	}
	for _, event := range pending {
		if event.RequestID == "replayed-client-id" {
			t.Fatal("client X-Request-ID leaked into the accounting event")
		}
		if event.CostCNY != "0.500000" {
			t.Fatalf("event cost = %q; want 0.500000", event.CostCNY)
		}
	}
}

// A failed outbox write (disk full / directory gone) must not be treated as
// success: the reservation stays held (money frozen, not refunded) and the
// retry loop later lands both the durable event and the settlement.
func TestOutboxFailureHoldsReservationAndRecovers(t *testing.T) {
	calls := 0
	upstream := newUpstream(t, &calls)
	s, snapshot, secret, outboxDir := newTestService(t, upstream.URL)
	handler := s.Handler()
	ctx := context.Background()

	if err := os.RemoveAll(outboxDir); err != nil {
		t.Fatal(err)
	}
	if rec := doProxyRequest(handler, secret, ""); rec.Code != http.StatusOK {
		t.Fatalf("request = %d: %s", rec.Code, rec.Body.String())
	}

	state, err := s.ledger.Account(ctx, snapshot.UserID)
	if err != nil {
		t.Fatal(err)
	}
	// The reserve (1 CNY) must still be held: settling before the event is
	// durable would be exactly the loss mode under audit.
	if state.Available != 9_000_000 || state.Reserved != 1_000_000 {
		t.Fatalf("available = %d reserved = %d; want 9000000 / 1000000 (reservation held)", state.Available, state.Reserved)
	}

	// Disk comes back: the retry drain writes the event and settles.
	if err := os.MkdirAll(outboxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.drainSettlementRetries(ctx)

	pending, err := s.outbox.Pending(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("outbox pending = %d, %v; want the recovered event", len(pending), err)
	}
	if pending[0].CostCNY != "0.500000" {
		t.Fatalf("recovered event cost = %q; want 0.500000", pending[0].CostCNY)
	}
	state, err = s.ledger.Account(ctx, snapshot.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Available != 9_500_000 || state.Reserved != 0 {
		t.Fatalf("available = %d reserved = %d; want 9500000 / 0 after recovery", state.Available, state.Reserved)
	}
}

// The API key cost limits synchronized in the account snapshot must trip at
// the edge once the locally settled spend reaches the limit.
func TestEdgeEnforcesKeyCostLimit(t *testing.T) {
	calls := 0
	upstream := newUpstream(t, &calls)
	s, snapshot, secret, _ := newTestService(t, upstream.URL)
	snapshot.TotalCostLimitCNY = "0.5"
	handler := s.Handler()

	if rec := doProxyRequest(handler, secret, ""); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d: %s", rec.Code, rec.Body.String())
	}
	rec := doProxyRequest(handler, secret, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("second request = %d; want 403 once the total limit is reached", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "cost_limit_exceeded" {
		t.Fatalf("error code = %q, %v; want cost_limit_exceeded", body.Error.Code, err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d; the limited request must not reach upstream", calls)
	}
	// Daily and monthly limits use the same counters and comparator.
	snapshot.TotalCostLimitCNY = ""
	snapshot.DailyCostLimitCNY = "0.3"
	if rec := doProxyRequest(handler, secret, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("daily-limited request = %d; want 403", rec.Code)
	}
}

// Settling the same billing ID twice must be idempotent: the second settle
// reports the already-settled state and moves no money.
func TestLedgerSettleIsIdempotent(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ledger := NewLedger(rdb, "jucode:edge:test-ledger", time.Minute)
	ctx := context.Background()
	uid := uuid.New()

	if err := ledger.SeedAccount(ctx, uid, 10_000_000, 0); err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(ctx, uid, "req-1", 1_000_000, uuid.Nil)
	if err != nil || reservation.Status != "reserved" {
		t.Fatalf("reserve = %#v, %v", reservation, err)
	}
	// A second reserve for the same ID is NOT fresh; the proxy refuses to
	// forward those.
	replay, err := ledger.Reserve(ctx, uid, "req-1", 1_000_000, uuid.Nil)
	if err != nil || replay.Status == "reserved" {
		t.Fatalf("replayed reserve status = %q, %v; must not be treated as fresh", replay.Status, err)
	}

	first, err := ledger.Settle(ctx, uid, "req-1", 500_000)
	if err != nil || first.Status != "settled" || first.Available != 9_500_000 {
		t.Fatalf("first settle = %#v, %v", first, err)
	}
	second, err := ledger.Settle(ctx, uid, "req-1", 500_000)
	if err != nil || second.Status != "settled" {
		t.Fatalf("second settle = %#v, %v", second, err)
	}
	state, err := ledger.Account(ctx, uid)
	if err != nil || state.Available != 9_500_000 || state.Reserved != 0 {
		t.Fatalf("account after double settle = %#v, %v; money moved twice", state, err)
	}
}
