package biz

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/conf"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entmodelgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// The batch writer inserts through CreateBulk inside a transaction, so its
// behavior (column coverage, ON CONFLICT, dedupe) cannot be checked without a
// real Postgres. Point JUCODE_TEST_PG_DSN at a throwaway instance to run these:
//
//	docker run -d --rm -e POSTGRES_PASSWORD=test -e POSTGRES_DB=jucode_test \
//	  -p 5499:5432 --name jucode-pg-test postgres:16-alpine
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/biz -run UsageLogWriter
func testBillingWithWriter(t *testing.T, cfg conf.UsageLogConfig) (*BillingSvc, *UsageLogWriter, *ent.Client) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed usage writer tests")
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

	d := &data.Data{Ent: client}
	w := NewUsageLogWriter(d, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	w.Start(ctx)
	t.Cleanup(w.Stop)

	// settings/plans stay nil: WriteUsageLog's batch path touches neither.
	svc := &BillingSvc{d: d, usageWriter: w}
	return svc, w, client
}

// usage_logs.user_id is a real foreign key, so rows need a real user.
func testUser(t *testing.T, client *ent.Client) uuid.UUID {
	t.Helper()
	u, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		Save(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}

func testModelGroup(t *testing.T, client *ent.Client) uuid.UUID {
	t.Helper()
	g, err := client.ModelGroup.Create().
		SetName("grp-" + uuid.NewString()).
		SetBillingSource(entmodelgroup.BillingSourceBalanceOnly).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create model group: %v", err)
	}
	return g.ID
}

func TestUsageLogEndpointMessagesValid(t *testing.T) {
	if err := entusagelog.EndpointValidator(entusagelog.EndpointMessages); err != nil {
		t.Fatal(err)
	}
}

func sampleUsageInput(userID uuid.UUID, requestID string, groupID *uuid.UUID) WriteUsageLogInput {
	return WriteUsageLogInput{
		UserID:     userID,
		RequestID:  requestID,
		Endpoint:   entusagelog.EndpointChatCompletions,
		Model:      "test-model",
		GroupID:    groupID,
		Status:     entusagelog.StatusOk,
		HTTPStatus: 200,
		TokensIn:   11,
		TokensOut:  22,
		CostFinal:  decimal.RequireFromString("0.00012345"),
		CostBase:   decimal.RequireFromString("0.00012345"),
		StartedAt:  time.Now(),
		Delivered:  true,
	}
}

func TestUsageLogWriterFlushesOnStop(t *testing.T) {
	svc, w, client := testBillingWithWriter(t, conf.UsageLogConfig{
		BufferSize: 128, FlushMaxRows: 500, FlushMaxAge: time.Hour, // age never fires; only Stop drains
	})
	ctx := context.Background()
	userID := testUser(t, client)

	const n = 25
	for i := 0; i < n; i++ {
		row, err := svc.WriteUsageLog(ctx, sampleUsageInput(userID, uuid.NewString(), nil))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if row != nil {
			t.Fatalf("batch path should not return a row; got %v", row)
		}
	}

	// Nothing is committed yet: neither the row cap nor the age timer fired.
	if got := client.UsageLog.Query().Where(entusagelog.UserIDEQ(userID)).CountX(ctx); got != 0 {
		t.Fatalf("rows visible before flush: got %d, want 0", got)
	}

	w.Stop() // must drain, not discard
	if got := client.UsageLog.Query().Where(entusagelog.UserIDEQ(userID)).CountX(ctx); got != n {
		t.Fatalf("rows after Stop: got %d, want %d (dropped=%d)", got, n, w.Dropped())
	}
}

func TestUsageLogWriterFlushesOnRowCap(t *testing.T) {
	svc, w, client := testBillingWithWriter(t, conf.UsageLogConfig{
		BufferSize: 128, FlushMaxRows: 10, FlushMaxAge: time.Hour,
	})
	ctx := context.Background()
	userID := testUser(t, client)

	for i := 0; i < 10; i++ {
		if _, err := svc.WriteUsageLog(ctx, sampleUsageInput(userID, uuid.NewString(), nil)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client.UsageLog.Query().Where(entusagelog.UserIDEQ(userID)).CountX(ctx) == 10 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("row cap did not trigger a flush: got %d, want 10 (dropped=%d)",
		client.UsageLog.Query().Where(entusagelog.UserIDEQ(userID)).CountX(ctx), w.Dropped())
}

// Column coverage: the batched insert must persist exactly what the
// synchronous insert did. usageLogCreateBuilder is shared by both paths
// specifically so this cannot drift, and this asserts it end to end.
func TestUsageLogWriterPersistsAllFields(t *testing.T) {
	svc, w, client := testBillingWithWriter(t, conf.UsageLogConfig{
		BufferSize: 16, FlushMaxRows: 100, FlushMaxAge: time.Hour,
	})
	ctx := context.Background()
	userID := testUser(t, client)
	reqID := uuid.NewString()

	in := sampleUsageInput(userID, reqID, nil)
	in.TtftMs = ptrInt(123)
	in.ProviderChain = []string{"attempt:one", "attempt:two"}
	in.LatencyDetail = map[string]int{"route_ms": 7}
	in.ErrorClass = "provider_failure"
	in.UpstreamRequestID = "req_upstream_1"
	in.IP = "203.0.113.9"
	in.Stream = true
	if _, err := svc.WriteUsageLog(ctx, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Stop()

	row := client.UsageLog.Query().Where(entusagelog.RequestIDEQ(reqID)).OnlyX(ctx)
	if row.TokensIn != 11 || row.TokensOut != 22 {
		t.Errorf("tokens: got in=%d out=%d, want 11/22", row.TokensIn, row.TokensOut)
	}
	// Money must survive the round trip exactly — numeric(20,8), no float.
	if !row.CostFinal.Equal(decimal.RequireFromString("0.00012345")) {
		t.Errorf("cost_final: got %s, want 0.00012345", row.CostFinal)
	}
	if row.TtftMs == nil || *row.TtftMs != 123 {
		t.Errorf("ttft_ms: got %v, want 123", row.TtftMs)
	}
	if len(row.ProviderChain) != 2 || row.ProviderChain[0] != "attempt:one" {
		t.Errorf("provider_chain: got %v", row.ProviderChain)
	}
	if row.LatencyDetail["route_ms"] != 7 {
		t.Errorf("latency_detail: got %v", row.LatencyDetail)
	}
	if row.ErrorClass != "provider_failure" || row.UpstreamRequestID != "req_upstream_1" {
		t.Errorf("diagnostics: class=%q upstream_id=%q", row.ErrorClass, row.UpstreamRequestID)
	}
	if !row.Stream || row.IP != "203.0.113.9" {
		t.Errorf("stream/ip: %v %q", row.Stream, row.IP)
	}
	// Defaults applied by normalizeUsageLogInput, not by the caller.
	if row.RateMultiplierApplied != 1.0 {
		t.Errorf("rate_multiplier_applied: got %v, want 1.0", row.RateMultiplierApplied)
	}
	if row.BillingSource != entusagelog.BillingSourceNone {
		t.Errorf("billing_source: got %q, want %q", row.BillingSource, entusagelog.BillingSourceNone)
	}
}

// This is the regression guard for the riskiest part of going async: the probe
// marker lives on the request context, and the flush runs on a detached
// background context. If normalization ever slid from submit time to flush
// time, synthetic probe traffic would be recorded as business traffic and
// would quietly move the public availability number.
func TestUsageLogWriterPreservesProbeTrafficClass(t *testing.T) {
	svc, w, client := testBillingWithWriter(t, conf.UsageLogConfig{
		BufferSize: 16, FlushMaxRows: 100, FlushMaxAge: time.Hour,
	})
	groupID := testModelGroup(t, client)
	probeReq, businessReq := uuid.NewString(), uuid.NewString()

	probeCtx := WithProbeTraffic(context.Background())
	if _, err := svc.WriteUsageLog(probeCtx, sampleUsageInput(testUser(t, client), probeReq, &groupID)); err != nil {
		t.Fatalf("probe write: %v", err)
	}
	if _, err := svc.WriteUsageLog(context.Background(), sampleUsageInput(testUser(t, client), businessReq, &groupID)); err != nil {
		t.Fatalf("business write: %v", err)
	}
	w.Stop()

	ctx := context.Background()
	probeRow := client.AvailabilityObservation.Query().
		Where(entavailability.RequestIDEQ(probeReq)).OnlyX(ctx)
	if probeRow.TrafficClass != entavailability.TrafficClassProbe {
		t.Errorf("probe traffic recorded as %q; synthetic traffic would pollute the public SLA number", probeRow.TrafficClass)
	}
	businessRow := client.AvailabilityObservation.Query().
		Where(entavailability.RequestIDEQ(businessReq)).OnlyX(ctx)
	if businessRow.TrafficClass != entavailability.TrafficClassBusiness {
		t.Errorf("business traffic recorded as %q", businessRow.TrafficClass)
	}
}

// availability_observations has a partial unique index on (request_id,
// group_id) so retries cannot inflate availability numbers. A video task
// settled twice (client poll + leader reconciler) can put both rows in one
// batch, which is why the writer dedupes in Go before the bulk insert.
func TestUsageLogWriterDedupesObservationsWithinBatch(t *testing.T) {
	svc, w, client := testBillingWithWriter(t, conf.UsageLogConfig{
		BufferSize: 16, FlushMaxRows: 100, FlushMaxAge: time.Hour,
	})
	ctx := context.Background()
	groupID := testModelGroup(t, client)
	reqID := uuid.NewString()
	userID := testUser(t, client)

	for i := 0; i < 3; i++ {
		if _, err := svc.WriteUsageLog(ctx, sampleUsageInput(userID, reqID, &groupID)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	w.Stop()

	if got := w.Dropped(); got != 0 {
		t.Fatalf("batch was dropped (%d rows) — the duplicate observations broke the insert", got)
	}
	// All three usage_logs rows land; only one observation does.
	if got := client.UsageLog.Query().Where(entusagelog.RequestIDEQ(reqID)).CountX(ctx); got != 3 {
		t.Errorf("usage_logs rows: got %d, want 3", got)
	}
	if got := client.AvailabilityObservation.Query().Where(entavailability.RequestIDEQ(reqID)).CountX(ctx); got != 1 {
		t.Errorf("availability rows: got %d, want 1 (dedupe failed)", got)
	}
}

// A full buffer must drop rather than block the request that produced it, and
// the loss has to be both reported to the caller and counted.
func TestUsageLogWriterDropsWhenBufferFull(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set")
	}
	// No Start(): nothing drains the channel, so it fills deterministically.
	w := NewUsageLogWriter(&data.Data{}, conf.UsageLogConfig{BufferSize: 2},
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1})))
	svc := &BillingSvc{d: &data.Data{}, usageWriter: w}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := svc.WriteUsageLog(ctx, sampleUsageInput(uuid.New(), uuid.NewString(), nil)); err != nil {
			t.Fatalf("write %d should fit in the buffer: %v", i, err)
		}
	}
	if _, err := svc.WriteUsageLog(ctx, sampleUsageInput(uuid.New(), uuid.NewString(), nil)); err != ErrUsageLogDropped {
		t.Fatalf("overflow write: got %v, want ErrUsageLogDropped", err)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("dropped counter: got %d, want 1", got)
	}
}

func ptrInt(v int) *int { return &v }
