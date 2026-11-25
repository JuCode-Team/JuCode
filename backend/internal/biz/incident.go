// Package biz — user-incident recorder.
//
// IncidentSvc persists anomalous user-traffic events (policy hits,
// quota breaches, auth failures, ...) into the user_incidents table.
// Writes are best-effort: a failed insert never blocks the LLM hot
// path. Callers can use either Record (synchronous, ignores the
// returned row on the failure path) or RecordAsync (fire-and-forget
// goroutine — strictly preferable inside the gateway).
package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entincident "github.com/jucode/jucode/internal/data/ent/userincident"
)

// ErrInvalidReviewLabel is returned by Annotate for an unrecognized label.
var ErrInvalidReviewLabel = errors.New("invalid review label")

type IncidentSvc struct{ d *data.Data }

func NewIncidentSvc(d *data.Data) *IncidentSvc { return &IncidentSvc{d: d} }

type IncidentInput struct {
	UserID    *uuid.UUID
	ApiKeyID  *uuid.UUID
	Kind      string // matches entincident.Kind values
	Severity  string // matches entincident.Severity values
	Action    string // "blocked" | "logged" | "cooldown"
	Model     string
	Endpoint  string
	RequestID string
	IP        string
	Message   string
	Payload   map[string]any
}

func (s *IncidentSvc) Record(ctx context.Context, in IncidentInput) (*ent.UserIncident, error) {
	q := s.d.Ent.UserIncident.Create().
		SetKind(entincident.Kind(in.Kind)).
		SetAction(in.Action).
		SetModel(in.Model).
		SetEndpoint(in.Endpoint).
		SetRequestID(in.RequestID).
		SetIP(in.IP).
		SetMessage(in.Message)
	if in.Severity != "" {
		q = q.SetSeverity(entincident.Severity(in.Severity))
	}
	if in.UserID != nil {
		q = q.SetUserID(*in.UserID)
	}
	if in.ApiKeyID != nil {
		q = q.SetAPIKeyID(*in.ApiKeyID)
	}
	if in.Payload != nil {
		q = q.SetPayload(in.Payload)
	}
	row, err := q.Save(ctx)
	if err != nil {
		return nil, err
	}
	// Auto-decrement user priority ONLY on our own policy_keyword blocks
	// (-10/hit, clamped to 0). cyber_security is deliberately NOT penalized:
	// it reflects the UPSTREAM over-rejecting, not user abuse, so it must
	// not drag the user's priority down (nor block their recovery).
	if in.UserID != nil && in.Action == "blocked" && in.Kind == "policy_keyword" {
		s.decrementUserPriority(ctx, *in.UserID, 10)
	}
	return row, nil
}

// decrementUserPriority drops user.priority by delta (clamped to 0) using
// two conditional ranged updates so it can't lose updates against the
// concurrent recovery loop (the old Get+SetPriority was a racy RMW).
// Best-effort: errors are logged at WARN, never propagated to the hot path.
func (s *IncidentSvc) decrementUserPriority(ctx context.Context, uid uuid.UUID, delta int) {
	if delta <= 0 {
		return
	}
	// priority >= delta → subtract atomically.
	if _, err := s.d.Ent.User.Update().
		Where(entuser.IDEQ(uid), entuser.PriorityGTE(delta)).
		AddPriority(-delta).Save(ctx); err != nil {
		slog.Warn("incident.priority_update", "user_id", uid, "err", err.Error())
		return
	}
	// 0 < priority < delta → floor to 0.
	if _, err := s.d.Ent.User.Update().
		Where(entuser.IDEQ(uid), entuser.PriorityGT(0), entuser.PriorityLT(delta)).
		SetPriority(0).Save(ctx); err != nil {
		slog.Warn("incident.priority_floor", "user_id", uid, "err", err.Error())
	}
}

// RecordAsync detaches from the request lifecycle so a slow DB write
// can't stall the gateway. The new context inherits no deadlines from
// the request — callers MUST NOT depend on the row existing before the
// next line of their code executes.
func (s *IncidentSvc) RecordAsync(in IncidentInput) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := s.Record(ctx, in); err != nil {
			slog.Warn("incident.record_failed", "kind", in.Kind, "err", err.Error())
		}
	}()
}

// Annotate sets the human review verdict on an incident, feeding the
// false-positive measurement loop. label must be one of "true_positive",
// "false_positive", or "unreviewed".
func (s *IncidentSvc) Annotate(ctx context.Context, id uuid.UUID, label string) error {
	switch entincident.ReviewLabel(label) {
	case entincident.ReviewLabelTruePositive,
		entincident.ReviewLabelFalsePositive,
		entincident.ReviewLabelUnreviewed:
	default:
		return ErrInvalidReviewLabel
	}
	return s.d.Ent.UserIncident.UpdateOneID(id).
		SetReviewLabel(entincident.ReviewLabel(label)).
		Exec(ctx)
}

// ListInput drives the admin /v1/admin/incidents query.
type IncidentListInput struct {
	UserID   *uuid.UUID
	Kind     string
	Severity string
	Since    time.Time
	Until    time.Time
	Limit    int
	Offset   int
}

func (s *IncidentSvc) List(ctx context.Context, in IncidentListInput) ([]*ent.UserIncident, int, error) {
	q := s.d.Ent.UserIncident.Query()
	if in.UserID != nil {
		q = q.Where(entincident.UserIDEQ(*in.UserID))
	}
	if in.Kind != "" {
		q = q.Where(entincident.KindEQ(entincident.Kind(in.Kind)))
	}
	if in.Severity != "" {
		q = q.Where(entincident.SeverityEQ(entincident.Severity(in.Severity)))
	}
	if !in.Since.IsZero() {
		q = q.Where(entincident.CreatedAtGTE(in.Since))
	}
	if !in.Until.IsZero() {
		q = q.Where(entincident.CreatedAtLT(in.Until))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := q.
		Order(ent.Desc(entincident.FieldCreatedAt)).
		Limit(limit).Offset(in.Offset).
		All(ctx)
	return rows, total, err
}

// --- statistics for the admin dashboard ---

// IncidentStatsInput scopes the aggregation. Kind/Severity are intentionally
// omitted: the dashboard charts break DOWN by kind and severity, so filtering
// on them would flatten the very dimension being visualized. UserID + time
// window are the only meaningful scopes.
type IncidentStatsInput struct {
	UserID *uuid.UUID
	Since  time.Time
	Until  time.Time
}

// IncidentBucket is one (category, count) pair in a breakdown.
type IncidentBucket struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// IncidentDaily is one day of the trend line: total events plus how many
// were blocked, so the UI can draw a stacked/overlaid area.
type IncidentDaily struct {
	Date    string `json:"date"` // YYYY-MM-DD (UTC)
	Total   int    `json:"total"`
	Blocked int    `json:"blocked"`
}

// IncidentStats is the full payload behind GET /v1/admin/incidents/stats.
type IncidentStats struct {
	Total         int              `json:"total"`
	Blocked       int              `json:"blocked"`        // action == "blocked"
	FalsePositive int              `json:"false_positive"` // review_label == false_positive
	Reviewed      int              `json:"reviewed"`       // review_label != unreviewed
	ByKind        []IncidentBucket `json:"by_kind"`
	BySeverity    []IncidentBucket `json:"by_severity"`
	ByAction      []IncidentBucket `json:"by_action"`
	ByReviewLabel []IncidentBucket `json:"by_review_label"`
	Daily         []IncidentDaily  `json:"daily"`
}

func (s *IncidentSvc) Stats(ctx context.Context, in IncidentStatsInput) (*IncidentStats, error) {
	// Default window: trailing 30 days. Keeps the trend chart bounded and
	// the group-bys cheap even on a large backlog.
	if in.Until.IsZero() {
		in.Until = time.Now()
	}
	if in.Since.IsZero() {
		in.Since = in.Until.AddDate(0, 0, -30)
	}

	base := s.d.Ent.UserIncident.Query().
		Where(
			entincident.CreatedAtGTE(in.Since),
			entincident.CreatedAtLT(in.Until),
		)
	if in.UserID != nil {
		base = base.Where(entincident.UserIDEQ(*in.UserID))
	}

	out := &IncidentStats{
		ByKind:        []IncidentBucket{},
		BySeverity:    []IncidentBucket{},
		ByAction:      []IncidentBucket{},
		ByReviewLabel: []IncidentBucket{},
		Daily:         []IncidentDaily{},
	}

	var err error
	if out.Total, err = base.Clone().Count(ctx); err != nil {
		return nil, err
	}
	if out.Blocked, err = base.Clone().Where(entincident.ActionEQ("blocked")).Count(ctx); err != nil {
		return nil, err
	}
	if out.FalsePositive, err = base.Clone().
		Where(entincident.ReviewLabelEQ(entincident.ReviewLabelFalsePositive)).Count(ctx); err != nil {
		return nil, err
	}
	if out.Reviewed, err = base.Clone().
		Where(entincident.ReviewLabelNEQ(entincident.ReviewLabelUnreviewed)).Count(ctx); err != nil {
		return nil, err
	}

	if out.ByKind, err = s.groupCount(ctx, entincident.FieldKind, in); err != nil {
		return nil, err
	}
	if out.BySeverity, err = s.groupCount(ctx, entincident.FieldSeverity, in); err != nil {
		return nil, err
	}
	if out.ByAction, err = s.groupCount(ctx, entincident.FieldAction, in); err != nil {
		return nil, err
	}
	if out.ByReviewLabel, err = s.groupCount(ctx, entincident.FieldReviewLabel, in); err != nil {
		return nil, err
	}

	if out.Daily, err = s.dailyTrend(ctx, in); err != nil {
		return nil, err
	}
	return out, nil
}

// groupCount runs SELECT <field>, COUNT(*) GROUP BY <field> over the scoped
// window and returns descending-by-count buckets. Empty-string keys (e.g. no
// action recorded) are relabeled "none" so the UI has a stable slice name.
// `field` is always a controlled entincident.Field* constant — never user
// input — so interpolating it into the column position is safe.
func (s *IncidentSvc) groupCount(ctx context.Context, field string, in IncidentStatsInput) ([]IncidentBucket, error) {
	where := []string{"created_at >= $1", "created_at < $2"}
	args := []any{in.Since, in.Until}
	if in.UserID != nil {
		args = append(args, *in.UserID)
		where = append(where, "user_id = $3")
	}
	query := fmt.Sprintf(
		`SELECT %s AS k, COUNT(*) AS c FROM user_incidents WHERE %s GROUP BY k ORDER BY c DESC`,
		field, joinAnd(where),
	)
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]IncidentBucket, 0, 8)
	for rows.Next() {
		var b IncidentBucket
		if err := rows.Scan(&b.Key, &b.Count); err != nil {
			return nil, err
		}
		if b.Key == "" {
			b.Key = "none"
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// dailyTrend buckets by business-timezone calendar day, splitting blocked out of
// the total. Raw SQL mirrors the StatsSvc.AggregateByModel idiom — ent has no
// date_trunc helper. Days with zero events are simply absent (sparse series);
// the frontend fills gaps when rendering.
func (s *IncidentSvc) dailyTrend(ctx context.Context, in IncidentStatsInput) ([]IncidentDaily, error) {
	where := []string{"created_at >= $1", "created_at < $2"}
	args := []any{in.Since, in.Until}
	if in.UserID != nil {
		args = append(args, *in.UserID)
		where = append(where, "user_id = $3")
	}
	args = append(args, TimezoneName())
	tzArg := fmt.Sprintf("$%d", len(args))
	query := `
		SELECT to_char(created_at AT TIME ZONE ` + tzArg + `, 'YYYY-MM-DD') AS d,
		       COUNT(*)                                             AS total,
		       COUNT(*) FILTER (WHERE action = 'blocked')           AS blocked
		FROM user_incidents
		WHERE ` + joinAnd(where) + `
		GROUP BY d
		ORDER BY d ASC`
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]IncidentDaily, 0, 32)
	for rows.Next() {
		var r IncidentDaily
		if err := rows.Scan(&r.Date, &r.Total, &r.Blocked); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func joinAnd(parts []string) string { return strings.Join(parts, " AND ") }
