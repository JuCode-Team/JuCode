package biz

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jucode/jucode/internal/data/ent"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// anomalyTestStatuses is every value of the usage_logs status enum, and
// anomalyTestClasses is every error class llm/error_class.go can emit plus
// the empty legacy value. The matrix test walks the full cross product so a
// new class or status can't silently land in the wrong bucket.
var anomalyTestStatuses = []string{"ok", "upstream_error", "quota_exceeded", "banned", "internal_error", "unauthorized"}

var anomalyTestClasses = []string{
	"", "ok", "provider_failure", "rate_limit", "timeout", "auth",
	"user_request", "user_policy", "cyber_block", "pool_exhausted",
	"client_canceled", "unknown",
}

// wantErrorAnomaly restates the contract independently of the
// implementation's exclusion slices: an edit to anomaly.go that widens or
// narrows either list must show up here as a diff, not silently pass.
func wantErrorAnomaly(status, class string) bool {
	switch status {
	case "ok", "quota_exceeded", "banned", "unauthorized":
		return false
	}
	switch class {
	case "ok", "user_request", "user_policy", "cyber_block", "client_canceled":
		return false
	}
	return true // includes "", provider_failure, rate_limit, timeout, auth, pool_exhausted, unknown
}

func TestClassifyAnomalyErrorMatrix(t *testing.T) {
	t.Parallel()
	for _, status := range anomalyTestStatuses {
		for _, class := range anomalyTestClasses {
			got := ClassifyAnomaly(nil, 0, 0, status, class)
			want := wantErrorAnomaly(status, class)
			if got.Error != want {
				t.Errorf("status=%q class=%q: error=%v, want %v", status, class, got.Error, want)
			}
			if got.Time {
				t.Errorf("status=%q class=%q: time anomaly flagged with no time signal", status, class)
			}
			// The reason token must let an operator see WHY without opening
			// the row: the class when there is one, the status otherwise.
			if want {
				wantReason := class
				if class == "" {
					wantReason = "status:" + status
				}
				if len(got.Reasons) != 1 || got.Reasons[0] != wantReason {
					t.Errorf("status=%q class=%q: reasons=%v, want [%s]", status, class, got.Reasons, wantReason)
				}
			} else if len(got.Reasons) != 0 {
				t.Errorf("status=%q class=%q: non-anomalous row carries reasons %v", status, class, got.Reasons)
			}
		}
	}
}

func intPtr(v int) *int { return &v }

// The thresholds are inclusive (>=), so every case sits exactly on a
// boundary or one below it.
func TestClassifyAnomalyTimeEdges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		ttft        *int
		latency     int
		retry       int
		wantTime    bool
		wantReasons []string
	}{
		{"all quiet", nil, 0, 0, false, []string{}},
		{"ttft one below", intPtr(29999), 0, 0, false, []string{}},
		{"ttft on threshold", intPtr(30000), 0, 0, true, []string{"ttft_ge_30s"}},
		{"nil ttft never trips", nil, 0, 0, false, []string{}},
		{"latency one below", nil, 119999, 0, false, []string{}},
		{"latency on threshold", nil, 120000, 0, true, []string{"latency_ge_120s"}},
		{"single retry is routine", nil, 0, 1, false, []string{}},
		{"retry on threshold", nil, 0, 2, true, []string{"retry_ge_2"}},
		{"all three at once", intPtr(45000), 180000, 3, true, []string{"ttft_ge_30s", "latency_ge_120s", "retry_ge_2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyAnomaly(tc.ttft, tc.latency, tc.retry, "ok", "ok")
			if got.Time != tc.wantTime || got.Error {
				t.Fatalf("time=%v error=%v, want time=%v error=false", got.Time, got.Error, tc.wantTime)
			}
			if !reflect.DeepEqual(got.Reasons, tc.wantReasons) {
				t.Fatalf("reasons=%v, want %v", got.Reasons, tc.wantReasons)
			}
		})
	}
}

// A slow AND failed row carries both flags and both reason families —
// time reasons first, the error token last.
func TestClassifyAnomalyCombinedReasons(t *testing.T) {
	t.Parallel()
	got := ClassifyAnomaly(intPtr(31000), 0, 0, "upstream_error", "provider_failure")
	if !got.Time || !got.Error {
		t.Fatalf("time=%v error=%v, want both true", got.Time, got.Error)
	}
	if want := []string{"ttft_ge_30s", "provider_failure"}; !reflect.DeepEqual(got.Reasons, want) {
		t.Fatalf("reasons=%v, want %v", got.Reasons, want)
	}
}

// The fragments are frozen because they are embedded verbatim in the
// observability aggregation SQL and quoted in docs/observability.md — a
// threshold or list edit must be a conscious three-place change.
func TestAnomalySQLFragmentsFrozen(t *testing.T) {
	t.Parallel()
	if got, want := AnomalyTimeSQL(), "((ttft_ms IS NOT NULL AND ttft_ms >= 30000) OR latency_ms >= 120000 OR retry_count >= 2)"; got != want {
		t.Errorf("time SQL:\n got %s\nwant %s", got, want)
	}
	wantErr := "(status <> 'ok' AND status NOT IN ('quota_exceeded','banned','unauthorized') AND (error_class = '' OR error_class NOT IN ('ok','user_request','user_policy','cyber_block','client_canceled')))"
	if got := AnomalyErrorSQL(); got != wantErr {
		t.Errorf("error SQL:\n got %s\nwant %s", got, wantErr)
	}
	if got, want := AnomalyAnySQL(), "("+AnomalyTimeSQL()+" OR "+AnomalyErrorSQL()+")"; got != want {
		t.Errorf("any SQL:\n got %s\nwant %s", got, want)
	}
}

// Postgres-backed parity check: the Go classifier, the raw-SQL fragments and
// the ent predicates must select exactly the same fixture rows. Same opt-in
// as the invite/usage-writer tests:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/biz -run AnomalySQLGoParity
func TestAnomalySQLGoParity(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed anomaly parity test")
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

	u, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// A unique model name scopes every query below to this run's fixtures,
	// so the shared test database needs no truncation.
	model := "anomaly-parity-" + uuid.NewString()
	type fixture struct {
		ttft    *int
		latency int
		retry   int
		status  string
		class   string
	}
	fixtures := []fixture{}
	for _, status := range anomalyTestStatuses {
		for _, class := range anomalyTestClasses {
			fixtures = append(fixtures, fixture{nil, 0, 0, status, class})
		}
	}
	fixtures = append(fixtures,
		fixture{intPtr(29999), 0, 0, "ok", "ok"},
		fixture{intPtr(30000), 0, 0, "ok", "ok"},
		fixture{nil, 119999, 0, "ok", "ok"},
		fixture{nil, 120000, 0, "ok", "ok"},
		fixture{nil, 0, 1, "ok", "ok"},
		fixture{nil, 0, 2, "ok", "ok"},
		fixture{intPtr(31000), 130000, 3, "upstream_error", "provider_failure"},
	)

	wantTime := map[string]bool{}
	wantError := map[string]bool{}
	wantNone := map[string]bool{}
	for _, f := range fixtures {
		row, err := client.UsageLog.Create().
			SetUserID(u.ID).
			SetEndpoint(entusagelog.EndpointChatCompletions).
			SetModel(model).
			SetStatus(entusagelog.Status(f.status)).
			SetErrorClass(f.class).
			SetLatencyMs(f.latency).
			SetRetryCount(f.retry).
			SetNillableTtftMs(f.ttft).
			Save(ctx)
		if err != nil {
			t.Fatalf("create usage log %+v: %v", f, err)
		}
		verdict := ClassifyAnomaly(f.ttft, f.latency, f.retry, f.status, f.class)
		wantTime[row.ID.String()] = verdict.Time
		wantError[row.ID.String()] = verdict.Error
		wantNone[row.ID.String()] = !verdict.Time && !verdict.Error
	}

	idsMatching := func(fragment string) map[string]bool {
		q := fmt.Sprintf("SELECT id::text FROM usage_logs WHERE model = $1 AND %s", fragment)
		rows, err := db.QueryContext(ctx, q, model)
		if err != nil {
			t.Fatalf("query %s: %v", fragment, err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[id] = true
		}
		return out
	}
	entIDsMatching := func(p func() []string) map[string]bool {
		out := map[string]bool{}
		for _, id := range p() {
			out[id] = true
		}
		return out
	}

	check := func(name string, want map[string]bool, got map[string]bool) {
		var diffs []string
		for id, w := range want {
			if got[id] != w {
				diffs = append(diffs, fmt.Sprintf("%s: go=%v sql=%v", id, w, got[id]))
			}
		}
		sort.Strings(diffs)
		if len(diffs) > 0 {
			t.Errorf("%s parity broken on %d rows:\n%v", name, len(diffs), diffs)
		}
	}

	sqlTime := idsMatching(AnomalyTimeSQL())
	sqlError := idsMatching(AnomalyErrorSQL())
	sqlNone := idsMatching("NOT " + AnomalyAnySQL())
	check("time raw-SQL", wantTime, sqlTime)
	check("error raw-SQL", wantError, sqlError)
	check("none raw-SQL", wantNone, sqlNone)

	entTime := entIDsMatching(func() []string {
		rows, err := client.UsageLog.Query().
			Where(entusagelog.ModelEQ(model), AnomalyTimePredicate()).
			All(ctx)
		if err != nil {
			t.Fatalf("ent time query: %v", err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID.String())
		}
		return out
	})
	entError := entIDsMatching(func() []string {
		rows, err := client.UsageLog.Query().
			Where(entusagelog.ModelEQ(model), AnomalyErrorPredicate()).
			All(ctx)
		if err != nil {
			t.Fatalf("ent error query: %v", err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID.String())
		}
		return out
	})
	entNone := entIDsMatching(func() []string {
		rows, err := client.UsageLog.Query().
			Where(
				entusagelog.ModelEQ(model),
				entusagelog.Not(AnomalyTimePredicate()),
				entusagelog.Not(AnomalyErrorPredicate()),
			).
			All(ctx)
		if err != nil {
			t.Fatalf("ent none query: %v", err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID.String())
		}
		return out
	})
	check("time ent-predicate", wantTime, entTime)
	check("error ent-predicate", wantError, entError)
	check("none ent-predicate", wantNone, entNone)
}
