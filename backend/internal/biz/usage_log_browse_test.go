package biz

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent/predicate"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

func TestDefaultUsageLogStart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	explicit := now.Add(-40 * 24 * time.Hour)
	end := now.Add(-10 * 24 * time.Hour)

	if got := DefaultUsageLogStart(now, nil, nil); !got.Equal(now.Add(-UsageLogDefaultWindow)) {
		t.Fatalf("no bounds: got %v", got)
	}
	if got := DefaultUsageLogStart(now, &explicit, nil); !got.Equal(explicit) {
		t.Fatalf("explicit start must win: got %v", got)
	}
	if got := DefaultUsageLogStart(now, nil, &end); !got.Equal(end.Add(-UsageLogDefaultWindow)) {
		t.Fatalf("end-only anchors the window to end: got %v", got)
	}
}

func TestCappedCountQuery(t *testing.T) {
	t.Parallel()
	preds := []predicate.UsageLog{
		entusagelog.StatusEQ(entusagelog.StatusOk),
		entusagelog.CreatedAtGTE(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)),
	}
	query, args := cappedCountQuery(preds, 10000)
	want := `SELECT COUNT(*) FROM (SELECT "id" FROM "usage_logs" WHERE "usage_logs"."status" = $1 AND "usage_logs"."created_at" >= $2 LIMIT 10001) AS t`
	if query != want {
		t.Fatalf("query =\n%s\nwant\n%s", query, want)
	}
	if len(args) != 2 {
		t.Fatalf("args = %v", args)
	}
}

func TestUsageLogCursorRoundTrip(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 9, 27, 8, 30, 15, 123456000, time.UTC)
	id := uuid.New()
	gotTS, gotID, err := DecodeUsageLogCursor(EncodeUsageLogCursor(ts, id))
	if err != nil {
		t.Fatal(err)
	}
	if !gotTS.Equal(ts) || gotID != id {
		t.Fatalf("round trip = (%v, %s), want (%v, %s)", gotTS, gotID, ts, id)
	}
	for _, bad := range []string{"", "not-base64!", "bm9kb3Q", "MTIzLm5vdC1hLXV1aWQ"} {
		if _, _, err := DecodeUsageLogCursor(bad); err == nil {
			t.Fatalf("cursor %q must be rejected", bad)
		}
	}
}

func TestUsageLogBeforeSQL(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 9, 27, 8, 30, 15, 0, time.UTC)
	id := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	query, args := cappedCountQuery([]predicate.UsageLog{UsageLogBefore(ts, id)}, 1)
	want := `SELECT COUNT(*) FROM (SELECT "id" FROM "usage_logs" WHERE ("usage_logs"."created_at", "usage_logs"."id") < ($1, $2) LIMIT 2) AS t`
	if query != want {
		t.Fatalf("query =\n%s\nwant\n%s", query, want)
	}
	if len(args) != 2 || args[1] != id {
		t.Fatalf("args = %v", args)
	}
}
