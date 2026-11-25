package biz

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The bug this guards: a console recomputing `now - 24h` per poll produced a
// different window every time, which both defeated the response cache (a new
// key per request) and disqualified the Redis hour ring (Start not on an hour
// boundary).
//
// The guarantee is per-step, not absolute: the key changes at most once per
// step, so a run of polls inside one step shares a key and a longer run costs
// one miss per step rather than one per request.
func TestSnapWindowCollapsesPollsWithinAStep(t *testing.T) {
	const step = 5 * time.Second
	// All inside the 14:37:10–14:37:15 step.
	base := time.Date(2026, 8, 3, 14, 37, 10, 200_000_000, time.UTC)

	var first string
	for _, offsetMs := range []int{0, 900, 2_400, 4_100} {
		now := base.Add(time.Duration(offsetMs) * time.Millisecond)
		f := StatsFilter{Start: now.Add(-24 * time.Hour), End: now, Bucket: "hour"}
		f.SnapWindow(step)
		got := f.CacheSuffix()
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("polls within one step produced different keys:\n  %s\n  %s", first, got)
		}
	}
}

// Over a longer run the win is the ratio: one key per step instead of one per
// request. At 2s polling against a 30s TTL that is a 15x reduction in misses.
func TestSnapWindowBoundsDistinctKeysByStep(t *testing.T) {
	const (
		step     = 30 * time.Second
		pollGap  = 2 * time.Second
		duration = 2 * time.Minute
	)
	base := time.Date(2026, 8, 3, 14, 37, 12, 345_000_000, time.UTC)

	seen := map[string]bool{}
	polls := 0
	for elapsed := time.Duration(0); elapsed < duration; elapsed += pollGap {
		now := base.Add(elapsed)
		f := StatsFilter{Start: now.Add(-24 * time.Hour), End: now, Bucket: "hour"}
		f.SnapWindow(step)
		seen[f.CacheSuffix()] = true
		polls++
	}

	maxKeys := int(duration/step) + 1
	if len(seen) > maxKeys {
		t.Errorf("got %d distinct keys over %d polls, want at most %d (one per %s step)",
			len(seen), polls, maxKeys, step)
	}
	if len(seen) >= polls {
		t.Errorf("got %d distinct keys for %d polls — no collapsing happened at all", len(seen), polls)
	}
}

// Start must land exactly on a UTC hour or bucketSeriesEligible rejects the
// window and every query falls through to SQL.
func TestSnapWindowAlignsStartForHourRing(t *testing.T) {
	now := time.Date(2026, 8, 3, 14, 37, 12, 345_000_000, time.UTC)
	f := StatsFilter{Start: now.Add(-24 * time.Hour), End: now, Bucket: "hour"}
	f.SnapWindow(5 * time.Second)

	if got := f.Start.UTC(); !got.Truncate(time.Hour).Equal(got) {
		t.Errorf("Start = %s, not on a UTC hour boundary", got)
	}
	if got := f.Start.UTC(); got.Minute() != 0 || got.Second() != 0 || got.Nanosecond() != 0 {
		t.Errorf("Start = %s, want zero minutes/seconds/nanos", got)
	}
	// End must stay close to now, or gate 3 (End within ~2min of now) rejects it.
	if d := now.Sub(f.End); d < 0 || d > 5*time.Second {
		t.Errorf("End drifted %s from now; the ring only answers windows ending near now", d)
	}
}

// The payoff: the console's rolling 24h window is rejected by the hour ring
// before snapping and accepted after. Without this the whole ring is dead code
// for the personal charts, which is exactly the state this fixed.
func TestSnapWindowMakesTheConsole24hWindowRingEligible(t *testing.T) {
	now := time.Date(2026, 8, 3, 14, 37, 12, 345_000_000, time.UTC)

	// Exactly what Me.tsx / Usage.tsx compute per poll.
	raw := StatsFilter{Start: now.Add(-24 * time.Hour), End: now, Bucket: "hour"}
	if bucketSeriesEligible(raw, now) {
		t.Fatal("unaligned rolling window was already eligible; this test no longer proves anything")
	}

	snapped := raw
	snapped.SnapWindow(5 * time.Second)
	if !bucketSeriesEligible(snapped, now) {
		t.Errorf("snapped window still rejected by the ring: start=%s end=%s span=%d",
			snapped.Start.UTC(), snapped.End.UTC(), usageBucketSpan(snapped))
	}
}

// The 24h window lands on exactly 25 buckets, which is the documented limit —
// one more and it falls back to SQL. Pin it so a change to usageBucketCount or
// to the snapping cannot silently push it over.
func TestSnappedDayWindowStaysWithinRingCapacity(t *testing.T) {
	for _, minute := range []int{0, 1, 30, 59} {
		now := time.Date(2026, 8, 3, 14, minute, 59, 999_000_000, time.UTC)
		f := StatsFilter{Start: now.Add(-24 * time.Hour), End: now, Bucket: "hour"}
		f.SnapWindow(5 * time.Second)
		if span := usageBucketSpan(f); span > usageBucketCount-1 {
			t.Errorf("minute=%d: span %d exceeds ring capacity %d", minute, span, usageBucketCount-1)
		}
	}
}

func TestSnapWindowAlignsStartToDayForDayBuckets(t *testing.T) {
	now := time.Date(2026, 8, 3, 14, 37, 12, 0, time.UTC)
	f := StatsFilter{Start: now.Add(-7 * 24 * time.Hour), End: now, Bucket: "day"}
	f.SnapWindow(30 * time.Second)

	got := f.Start.In(Location())
	if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 || got.Nanosecond() != 0 {
		t.Errorf("Start = %s, want a Shanghai midnight boundary", got)
	}
}

// 16:30 UTC is 00:30 the next calendar day in Shanghai. UTC Truncate(24h)
// would snap that back to the previous UTC midnight and pull 16 extra hours
// into a "today" / day-bucket chart.
func TestSnapWindowDayUsesShanghaiNotUTC(t *testing.T) {
	start := time.Date(2026, 8, 3, 16, 30, 0, 0, time.UTC) // 2026-08-04 00:30 CST
	f := StatsFilter{Start: start, End: start.Add(time.Hour), Bucket: "day"}
	f.SnapWindow(30 * time.Second)

	want := time.Date(2026, 8, 4, 0, 0, 0, 0, Location())
	if !f.Start.Equal(want) {
		t.Errorf("Start = %s, want Shanghai midnight %s", f.Start, want)
	}
}

func TestSnapWindowPreservesExplicitShanghaiMidnight(t *testing.T) {
	start := time.Date(2026, 9, 17, 0, 0, 0, 0, Location())
	f := StatsFilter{Start: start, End: start.Add(24 * time.Hour), Bucket: "day"}
	f.SnapWindow(30 * time.Second)
	if !f.Start.Equal(start) {
		t.Errorf("Start = %s, want unchanged %s", f.Start, start)
	}
}

func TestStatsTimeTruncSQL(t *testing.T) {
	if got := statsTimeTruncSQL("hour"); got != "date_trunc('hour', created_at)" {
		t.Errorf("hour trunc = %s", got)
	}
	day := statsTimeTruncSQL("day")
	if !strings.Contains(day, "'"+TimezoneName()+"'") {
		t.Errorf("day trunc must pin the business timezone, got %s", day)
	}
}

// An unrecognized bucket must not be silently reshaped — Aggregate rejects it
// downstream, and snapping would hide the caller's mistake.
func TestSnapWindowLeavesUnknownBucketAlone(t *testing.T) {
	now := time.Date(2026, 8, 3, 14, 37, 12, 0, time.UTC)
	start := now.Add(-time.Hour)
	f := StatsFilter{Start: start, End: now, Bucket: "week"}
	f.SnapWindow(5 * time.Second)

	if !f.Start.Equal(start) || !f.End.Equal(now) {
		t.Errorf("window changed for unknown bucket: %s..%s", f.Start, f.End)
	}
}

// Snapping must never produce an inverted or empty range: a caller asking for
// a window narrower than one step would otherwise get End <= Start.
func TestSnapWindowNeverInvertsRange(t *testing.T) {
	now := time.Date(2026, 8, 3, 14, 0, 3, 0, time.UTC)
	f := StatsFilter{Start: now.Add(-2 * time.Second), End: now, Bucket: "hour"}
	f.SnapWindow(30 * time.Second)

	if !f.End.After(f.Start) {
		t.Errorf("End %s is not after Start %s", f.End, f.Start)
	}
}

// The scope (who may see these rows) is deliberately NOT in CacheSuffix —
// callers prefix it. This test exists so that if someone later "helpfully"
// makes CacheSuffix self-sufficient, they do it knowingly rather than leaving
// two tenants sharing a key.
func TestCacheSuffixExcludesScopeButSeparatesDimensions(t *testing.T) {
	u1, u2 := uuid.New(), uuid.New()
	base := StatsFilter{
		Start:  time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		End:    time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
		Bucket: "hour",
	}

	a, b := base, base
	a.UserID, b.UserID = &u1, &u2
	a.PersonalOnly, b.PersonalOnly = true, false
	if a.CacheSuffix() != b.CacheSuffix() {
		t.Error("CacheSuffix varies by scope; callers prefix scope, so this would silently double the key space")
	}

	// Dimension filters DO change the answer and must change the key.
	m1, m2 := base, base
	m1.Model, m2.Model = "gpt-5.4", "kimi-k2.6"
	if m1.CacheSuffix() == m2.CacheSuffix() {
		t.Error("different Model produced the same key; one model's chart would serve another's")
	}

	s1, s2 := base, base
	s1.Status, s2.Status = "ok", "upstream_error"
	if s1.CacheSuffix() == s2.CacheSuffix() {
		t.Error("different Status produced the same key")
	}

	g := uuid.New()
	g1, g2 := base, base
	g2.GroupID = &g
	if g1.CacheSuffix() == g2.CacheSuffix() {
		t.Error("GroupID filter did not change the key")
	}
}
