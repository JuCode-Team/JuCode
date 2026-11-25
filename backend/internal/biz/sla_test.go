package biz

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
)

// The admin SLA is business-only everywhere; the public kinds keep their
// exact split. Getting this table wrong either leaks probe padding into the
// admin percentages or (worse) silently removes it from the public bar.
func TestTrafficClassPredicateKinds(t *testing.T) {
	t.Parallel()
	const businessOnly = "traffic_class = 'business'"
	const withProbes = "traffic_class IN ('business','probe')"
	cases := map[string]string{
		"window":    businessOnly,
		"sla":       businessOnly,
		"bucket":    withProbes,
		"aggregate": withProbes,
	}
	for kind, want := range cases {
		if got := trafficClassPredicate(kind); got != want {
			t.Errorf("trafficClassPredicate(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestSLAAvgLatencyNullWithoutSamples(t *testing.T) {
	t.Parallel()

	var a slaAccumulator
	if got := a.avgLatencyMs(); got != nil {
		t.Fatalf("empty average latency = %v, want null", *got)
	}
	a.LatencySum = 250
	a.LatencyCount = 2
	if got := a.avgLatencyMs(); got == nil || *got != 125 {
		t.Fatalf("average latency = %v, want 125", got)
	}
}

// The public /v1/public/status endpoint is fed by exactly these three query
// shapes. The strings are frozen so that adding the admin "sla" kind (or any
// later kind) can never change what the public page executes — if a byte
// moves here, it is a deliberate public-status change, not a side effect.
func TestAvailabilityGroupQueryPublicKindsFrozen(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	const columns = `  count(*) FILTER (WHERE outcome = 'good')::int,
  count(*) FILTER (WHERE outcome = 'bad')::int,
  count(*) FILTER (WHERE outcome = 'excluded')::int,
  coalesce(sum(latency_ms) FILTER (WHERE outcome IN ('good','bad')), 0)::bigint,
  count(latency_ms) FILTER (WHERE outcome IN ('good','bad') AND latency_ms > 0)::bigint,
  max(started_at),
  max(started_at) FILTER (WHERE outcome IN ('good','bad'))
FROM availability_observations WHERE `

	wantBucket := `SELECT group_id,
  floor(extract(epoch FROM (started_at - $1)) / $3)::int AS bucket_index,
` + columns + `traffic_class IN ('business','probe') AND started_at >= $1 AND started_at < $2 AND group_id IN ($4)
GROUP BY group_id, bucket_index ORDER BY group_id, bucket_index`
	gotBucket, argsBucket := availabilityGroupQuery("bucket", start, end, 30*time.Minute, []uuid.UUID{id})
	if gotBucket != wantBucket {
		t.Errorf("bucket query changed:\n got: %s\nwant: %s", gotBucket, wantBucket)
	}
	if len(argsBucket) != 4 {
		t.Errorf("bucket args = %d, want 4 (start, end, seconds, id)", len(argsBucket))
	}

	wantAggregate := `SELECT group_id,
` + columns + `traffic_class IN ('business','probe') AND started_at >= $1 AND started_at < $2 AND group_id IN ($3) GROUP BY group_id`
	gotAggregate, argsAggregate := availabilityGroupQuery("aggregate", start, end, 0, []uuid.UUID{id})
	if gotAggregate != wantAggregate {
		t.Errorf("aggregate query changed:\n got: %s\nwant: %s", gotAggregate, wantAggregate)
	}
	if len(argsAggregate) != 3 {
		t.Errorf("aggregate args = %d, want 3 (start, end, id)", len(argsAggregate))
	}

	wantWindow := `SELECT group_id,
` + columns + `traffic_class = 'business' AND started_at >= $1 AND started_at < $2 AND group_id IN ($3) GROUP BY group_id`
	gotWindow, _ := availabilityGroupQuery("window", start, end, 0, []uuid.UUID{id})
	if gotWindow != wantWindow {
		t.Errorf("window query changed:\n got: %s\nwant: %s", gotWindow, wantWindow)
	}
}

// The "sla" kind must combine the bucketed shape with the business-only
// predicate — the one combination the public page never uses.
func TestAvailabilityQuerySLAKindShape(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	bucketQ, bucketArgs := availabilityGroupQuery("bucket", start, end, 30*time.Minute, []uuid.UUID{id})
	slaQ, slaArgs := availabilityGroupQuery("sla", start, end, 30*time.Minute, []uuid.UUID{id})

	// Identical to the public bucket query except for the traffic class —
	// same grid math, same placeholders, same grouping.
	if n := strings.Count(bucketQ, "traffic_class IN ('business','probe')"); n != 1 {
		t.Fatalf("bucket query mentions the probe predicate %d times, want 1:\n%s", n, bucketQ)
	}
	rewritten := strings.Replace(bucketQ, "traffic_class IN ('business','probe')", "traffic_class = 'business'", 1)
	if slaQ != rewritten {
		t.Errorf("sla query is not the business-only bucket query:\n got: %s\nwant: %s", slaQ, rewritten)
	}
	if len(slaArgs) != len(bucketArgs) {
		t.Errorf("sla args = %d, want %d", len(slaArgs), len(bucketArgs))
	}

	modelQ, _ := availabilityModelQuery("sla", start, end, 30*time.Minute, []uuid.UUID{id}, []string{"m"})
	if want := "traffic_class = 'business'"; !strings.Contains(modelQ, want) {
		t.Errorf("sla model query missing %q:\n%s", want, modelQ)
	}
	if strings.Contains(modelQ, "probe") {
		t.Errorf("sla model query still mentions probe traffic:\n%s", modelQ)
	}
}

// Postgres-backed end-to-end check that probe traffic stays out of every
// admin SLA number — counts, percentage, state and latency. Same opt-in as
// the invite/usage-writer tests:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/biz -run AdminSLASnapshot
func TestAdminSLASnapshotBusinessOnly(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed SLA test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	d := data.NewWith(client, nil, db)
	settings := NewSettingsSvc(d)

	group, err := client.ModelGroup.Create().
		SetName("sla-test-" + uuid.NewString()).
		SetBillingSource("plan_only").
		Save(ctx)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	model, err := client.Model.Create().
		SetName("sla-test-model-" + uuid.NewString()).
		SetBillingMode("per_token").
		Save(ctx)
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := client.ModelGroupModel.Create().SetGroupID(group.ID).SetModelID(model.ID).Save(ctx); err != nil {
		t.Fatalf("link model: %v", err)
	}
	// Group membership alone is not enough: the report lists only models a
	// provider in the group actually serves (filterRoutableModels), so an
	// unwired model would leave the group with an empty model list.
	account, err := client.UpstreamAccount.Create().
		SetName("sla-test-account-" + uuid.NewString()).
		SetBaseURL("http://127.0.0.1:1").
		SetNativeProtocols([]string{NativeProtocolChat}).
		Save(ctx)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := client.UpstreamAccountModel.Create().SetAccountID(account.ID).SetModelName(model.Name).Save(ctx); err != nil {
		t.Fatalf("serve model: %v", err)
	}
	if _, err := client.GroupAccount.Create().SetGroupID(group.ID).SetAccountID(account.ID).Save(ctx); err != nil {
		t.Fatalf("link account: %v", err)
	}
	st, err := settings.GetSite(ctx)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	st.StatusPageGroupIDs = []uuid.UUID{group.ID}
	settings.storeSite(st)

	observe := func(outcome entavailability.Outcome, class entavailability.TrafficClass, latencyMs int) {
		t.Helper()
		if err := client.AvailabilityObservation.Create().
			SetRequestID(uuid.NewString()).
			SetGroupID(group.ID).
			SetModel(model.Name).
			SetOutcome(outcome).
			SetLatencyMs(latencyMs).
			SetStartedAt(time.Now().UTC().Add(-time.Minute)).
			SetTrafficClass(class).
			Exec(ctx); err != nil {
			t.Fatalf("create observation: %v", err)
		}
	}
	// Business: 3 good + 1 bad + 1 excluded → 75%, degraded (single failure
	// damping). Probe: 5 bad at a wild latency — if any of them leaked into
	// the report, the percentage drops to 33%, the state falls to "down" and
	// the latency average moves off 100.
	for i := 0; i < 3; i++ {
		observe(entavailability.OutcomeGood, entavailability.TrafficClassBusiness, 100)
	}
	observe(entavailability.OutcomeBad, entavailability.TrafficClassBusiness, 100)
	observe(entavailability.OutcomeExcluded, entavailability.TrafficClassBusiness, 100)
	for i := 0; i < 5; i++ {
		observe(entavailability.OutcomeBad, entavailability.TrafficClassProbe, 9999)
	}

	out, err := AdminSLASnapshot(ctx, client, db, settings, 24, nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if out.WindowHours != 24 {
		t.Errorf("window_hours = %d, want 24", out.WindowHours)
	}
	if out.Thresholds != (SLAThresholds{Healthy: 98, Degraded: 90, Serious: 80}) {
		t.Errorf("thresholds = %+v", out.Thresholds)
	}
	if len(out.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(out.Groups))
	}
	g := out.Groups[0]
	if g.GroupID != group.ID.String() || g.Name != group.Name {
		t.Errorf("group identity = (%s, %s)", g.GroupID, g.Name)
	}
	if g.Good != 3 || g.Bad != 1 || g.Excluded != 1 {
		t.Errorf("counts = %d/%d/%d, want 3/1/1 — probe rows leaked in", g.Good, g.Bad, g.Excluded)
	}
	if g.AvailabilityPct == nil || *g.AvailabilityPct != 75 {
		t.Errorf("availability = %v, want 75", g.AvailabilityPct)
	}
	if g.State != StatusHealthy {
		t.Errorf("state = %q, want healthy (single business failure, damped)", g.State)
	}
	if g.AvgLatencyMs == nil || *g.AvgLatencyMs != 100 {
		t.Errorf("avg latency = %v, want 100 — probe latency leaked in", g.AvgLatencyMs)
	}
	if g.OpenIncidentID != nil {
		t.Errorf("open_incident_id = %v, want null in this PR", g.OpenIncidentID)
	}
	if len(g.Buckets) != statusBucketCount {
		t.Fatalf("buckets = %d, want %d", len(g.Buckets), statusBucketCount)
	}
	bucketGood := 0
	for _, b := range g.Buckets {
		bucketGood += b.Good
	}
	if bucketGood != 3 {
		t.Errorf("bucket good sum = %d, want 3", bucketGood)
	}
	if len(g.Models) != 1 || g.Models[0].Model != model.Name {
		t.Fatalf("models = %+v, want the one linked model", g.Models)
	}
	m := g.Models[0]
	if m.Good != 3 || m.Bad != 1 || m.Excluded != 1 {
		t.Errorf("model counts = %d/%d/%d, want 3/1/1", m.Good, m.Bad, m.Excluded)
	}

	// Narrowing to an unknown-but-valid group answers empty, not an error —
	// consistent with the series endpoint's zero-fill contract.
	other := uuid.New()
	filtered, err := AdminSLASnapshot(ctx, client, db, settings, 24, &other)
	if err != nil {
		t.Fatalf("filtered snapshot: %v", err)
	}
	if len(filtered.Groups) != 0 {
		t.Errorf("unknown group filter returned %d groups, want 0", len(filtered.Groups))
	}
}
