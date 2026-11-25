// usage_log_browse.go: query plumbing shared by the two usage_logs browsers
// (/v1/admin/usage-logs and /v1/me/usage-logs). usage_logs is the largest
// table and sits on the gateway write path, so the browsers must not ask
// it for anything a bounded index scan cannot answer: no exact COUNT(*)
// over the whole table, no unbounded time range, no OFFSET that walks
// hundreds of thousands of rows to throw them away.
package biz

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	"github.com/jucode/jucode/internal/data/ent/predicate"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

const (
	// UsageLogCountCap bounds the "共 N 条" figure. Beyond it the count
	// stops scanning and reports the cap with TotalCapped=true; nobody
	// pages through more than 10k rows, and an exact count above that is
	// a full index scan per request.
	UsageLogCountCap = 10000
	// UsageLogDefaultWindow is the lower bound applied when the caller
	// gives no start: every unindexed filter (ILIKE on model / ip / key
	// name, error_class, provider_id) then scans a week instead of the
	// whole table. An explicit start widens it without limit.
	UsageLogDefaultWindow = 7 * 24 * time.Hour
	// UsageLogMinQueryLen is the shortest `q` the browsers accept. The
	// fuzzy search is an OR of four ILIKE '%q%' and can only ever scan;
	// one or two characters match nearly every row and buy nothing.
	UsageLogMinQueryLen = 3
)

// DefaultUsageLogStart returns the effective lower bound of a browse:
// the requested start when there is one, otherwise UsageLogDefaultWindow
// before the requested end (or now).
func DefaultUsageLogStart(now time.Time, requestedStart, requestedEnd *time.Time) time.Time {
	if requestedStart != nil {
		return *requestedStart
	}
	end := now
	if requestedEnd != nil {
		end = *requestedEnd
	}
	return end.Add(-UsageLogDefaultWindow)
}

// CountUsageLogsCapped counts the rows matching preds, giving up once
// more than capN exist:
//
//	SELECT COUNT(*) FROM (SELECT id FROM usage_logs WHERE ... LIMIT capN+1) t
//
// The inner LIMIT lets Postgres stop the scan early, so a filter that
// matches most of the table costs a few index pages instead of a full
// pass. Returns (capN, true) when the cap was hit.
func (s *StatsSvc) CountUsageLogsCapped(ctx context.Context, preds []predicate.UsageLog, capN int) (int, bool, error) {
	query, args := cappedCountQuery(preds, capN)
	var n int
	if err := s.d.DB().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, false, err
	}
	if n > capN {
		return capN, true, nil
	}
	return n, false, nil
}

func cappedCountQuery(preds []predicate.UsageLog, capN int) (string, []any) {
	inner := sql.Dialect(dialect.Postgres).
		Select(entusagelog.FieldID).
		From(sql.Table(entusagelog.Table)).
		Limit(capN + 1)
	for _, p := range preds {
		p(inner)
	}
	// Wrapped by hand: Selector.As would rename the inner table for the
	// predicates too, and the alias must belong to the derived table only.
	query, args := inner.Query()
	return "SELECT COUNT(*) FROM (" + query + ") AS t", args
}

// UsageLogModelFilter picks the predicate for a `model` filter value. A
// value that names a catalog model matches with equality, which the
// (model, created_at) index serves; anything else stays a substring match
// so partial typing in the filter box keeps working (bounded by the time
// window, not the index).
func (s *StatsSvc) UsageLogModelFilter(ctx context.Context, v string) (predicate.UsageLog, error) {
	exact, err := s.d.Ent.Model.Query().Where(entmodel.NameEQ(v)).Exist(ctx)
	if err != nil {
		return nil, err
	}
	if exact {
		return entusagelog.ModelEQ(v), nil
	}
	return entusagelog.ModelContainsFold(v), nil
}

// Keyset paging. A cursor is the (created_at, id) of the last row of a
// page; the next page is everything strictly before it in the browse
// order `created_at DESC, id DESC`. Unlike OFFSET the cost of page N does
// not grow with N: the row comparison is a single index seek.

// EncodeUsageLogCursor serializes a page boundary for the client.
func EncodeUsageLogCursor(createdAt time.Time, id uuid.UUID) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + "." + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeUsageLogCursor parses a cursor produced by EncodeUsageLogCursor.
func DecodeUsageLogCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("cursor 无法解析")
	}
	ts, idStr, ok := strings.Cut(string(raw), ".")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("cursor 无法解析")
	}
	nanos, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("cursor 无法解析")
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("cursor 无法解析")
	}
	return time.Unix(0, nanos).UTC(), id, nil
}

// UsageLogBefore is the keyset predicate `(created_at, id) < ($1, $2)`.
// The row-value form (rather than an OR of two comparisons) is what lets
// the planner turn it into one seek on the created_at index.
func UsageLogBefore(createdAt time.Time, id uuid.UUID) predicate.UsageLog {
	return func(s *sql.Selector) {
		s.Where(sql.P(func(b *sql.Builder) {
			b.WriteString("(").Ident(s.C(entusagelog.FieldCreatedAt)).Comma().Ident(s.C(entusagelog.FieldID)).
				WriteString(") < (").Arg(createdAt).Comma().Arg(id).WriteString(")")
		}))
	}
}
