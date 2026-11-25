// anomaly.go: the single source of truth for what counts as a "request
// anomaly" in usage_logs. Three consumers read these definitions — the
// admin usage-log browser (per-row flags + the ?anomaly= filter), the
// observability series/leaderboard aggregations (per-bucket anomaly
// counts), and the docs — and they must agree row for row, so the
// thresholds and exclusion lists live here exactly once and everything
// else (Go classifier, ent predicates, raw-SQL fragments) is derived
// from them. anomaly_test.go enforces the Go/SQL parity on fixtures.
package biz

import (
	"fmt"
	"strings"

	"github.com/jucode/jucode/internal/data/ent/predicate"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// Time-anomaly thresholds. These are deliberately blunt: they flag the
// tail an operator wants to eyeball, not an SLO. TTFT ≥ 30s means the
// caller stared at a spinner for half a minute; latency ≥ 120s is past
// every sane client timeout; retry_count ≥ 2 means at least two channels
// failed before one answered (a single failover is routine).
const (
	AnomalyTTFTThresholdMs    = 30000
	AnomalyLatencyThresholdMs = 120000
	AnomalyRetryThreshold     = 2
)

// anomalyExcludedStatuses are terminal statuses caused by the caller's
// own account state (quota, ban, bad credentials) — the request never
// reached a provider on our behalf, so it is not an error anomaly.
var anomalyExcludedStatuses = []string{"quota_exceeded", "banned", "unauthorized"}

// anomalyExcludedErrorClasses are the caller-side outcomes of an
// otherwise-failed row. "ok" appears because a row can fail *after*
// delivery (billing/audit persistence) while the service outcome was
// fine; the user_* / cyber_block / client_canceled classes mirror the
// availability exclusions in availability.go — user-caused failures must
// not page anyone. Note the asymmetry with provider faults: "timeout"
// is NOT excluded here (a 30s+ hang is exactly what this flag is for)
// even though the SLA pipeline excludes it.
var anomalyExcludedErrorClasses = []string{"ok", "user_request", "user_policy", "cyber_block", "client_canceled"}

// AnomalyFlags is the per-row verdict attached to admin usage-log rows.
// Reasons is always non-nil so the JSON shape is stable ("reasons":[]).
type AnomalyFlags struct {
	Time    bool     `json:"time"`
	Error   bool     `json:"error"`
	Reasons []string `json:"reasons"`
}

// ClassifyAnomaly grades one usage_logs row. The arguments mirror the
// columns the SQL predicates read, in the same order, so a drift between
// this function and the fragments below is a one-line diff to spot.
//
// A nil ttftMs never trips the TTFT threshold — matching SQL, where
// `ttft_ms >= N` is NULL (false) on a NULL column.
func ClassifyAnomaly(ttftMs *int, latencyMs, retryCount int, status, errorClass string) AnomalyFlags {
	out := AnomalyFlags{Reasons: []string{}}

	if ttftMs != nil && *ttftMs >= AnomalyTTFTThresholdMs {
		out.Time = true
		out.Reasons = append(out.Reasons, "ttft_ge_30s")
	}
	if latencyMs >= AnomalyLatencyThresholdMs {
		out.Time = true
		out.Reasons = append(out.Reasons, "latency_ge_120s")
	}
	if retryCount >= AnomalyRetryThreshold {
		out.Time = true
		out.Reasons = append(out.Reasons, "retry_ge_2")
	}

	if isErrorAnomaly(status, errorClass) {
		out.Error = true
		if errorClass != "" {
			out.Reasons = append(out.Reasons, errorClass)
		} else {
			out.Reasons = append(out.Reasons, "status:"+status)
		}
	}
	return out
}

func isErrorAnomaly(status, errorClass string) bool {
	if status == "ok" {
		return false
	}
	for _, s := range anomalyExcludedStatuses {
		if status == s {
			return false
		}
	}
	if errorClass == "" {
		return true
	}
	for _, c := range anomalyExcludedErrorClasses {
		if errorClass == c {
			return false
		}
	}
	return true
}

// AnomalyTimeSQL returns the time-anomaly predicate as a raw SQL fragment
// over unqualified usage_logs columns, parenthesized so callers can embed
// it in FILTER (WHERE ...) or AND chains without precedence surprises.
func AnomalyTimeSQL() string {
	return fmt.Sprintf("((ttft_ms IS NOT NULL AND ttft_ms >= %d) OR latency_ms >= %d OR retry_count >= %d)",
		AnomalyTTFTThresholdMs, AnomalyLatencyThresholdMs, AnomalyRetryThreshold)
}

// AnomalyErrorSQL is AnomalyTimeSQL's error-anomaly sibling.
func AnomalyErrorSQL() string {
	return fmt.Sprintf("(status <> 'ok' AND status NOT IN (%s) AND (error_class = '' OR error_class NOT IN (%s)))",
		quotedSQLList(anomalyExcludedStatuses), quotedSQLList(anomalyExcludedErrorClasses))
}

// AnomalyAnySQL is the union, for "how many anomalies in this bucket".
func AnomalyAnySQL() string {
	return "(" + AnomalyTimeSQL() + " OR " + AnomalyErrorSQL() + ")"
}

// quotedSQLList renders a fixed identifier list as 'a','b','c'. Inputs
// come only from the package-level constants above (never user input),
// and none of them contain quotes.
func quotedSQLList(vs []string) string {
	quoted := make([]string, len(vs))
	for i, v := range vs {
		quoted[i] = "'" + v + "'"
	}
	return strings.Join(quoted, ",")
}

// AnomalyTimePredicate is the ent form of AnomalyTimeSQL, for composing
// into the typed usage-log query in the admin log browser.
func AnomalyTimePredicate() predicate.UsageLog {
	return entusagelog.Or(
		entusagelog.And(
			entusagelog.TtftMsNotNil(),
			entusagelog.TtftMsGTE(AnomalyTTFTThresholdMs),
		),
		entusagelog.LatencyMsGTE(AnomalyLatencyThresholdMs),
		entusagelog.RetryCountGTE(AnomalyRetryThreshold),
	)
}

// AnomalyErrorPredicate is the ent form of AnomalyErrorSQL.
func AnomalyErrorPredicate() predicate.UsageLog {
	excluded := make([]entusagelog.Status, len(anomalyExcludedStatuses))
	for i, s := range anomalyExcludedStatuses {
		excluded[i] = entusagelog.Status(s)
	}
	return entusagelog.And(
		entusagelog.StatusNEQ(entusagelog.StatusOk),
		entusagelog.StatusNotIn(excluded...),
		entusagelog.Or(
			entusagelog.ErrorClassEQ(""),
			entusagelog.ErrorClassNotIn(anomalyExcludedErrorClasses...),
		),
	)
}
