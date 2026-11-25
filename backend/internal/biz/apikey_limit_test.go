package biz

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/shopspring/decimal"
)

func dp(s string) *decimal.Decimal { d := decimal.RequireFromString(s); return &d }
func TestCostLimitExceeded(t *testing.T) {
	k := &ent.ApiKey{DailyCostLimit: dp("10"), MonthlyCostLimit: dp("20"), TotalCostLimit: dp("30")}
	if CostLimitExceeded(k, CostUsage{Daily: *dp("9.99"), Monthly: *dp("19"), Total: *dp("29")}) {
		t.Fatal("below limits")
	}
	if !CostLimitExceeded(k, CostUsage{Daily: *dp("10")}) {
		t.Fatal("equal must reject")
	}
	k.DailyCostLimit = dp("0")
	if CostLimitExceeded(k, CostUsage{Daily: *dp("999")}) {
		t.Fatal("zero unlimited")
	}
}

func TestNegativeCostLimit(t *testing.T) {
	if validateCostLimits(dp("-0.1")) != ErrInvalidCostLimit {
		t.Fatal("negative accepted")
	}
	if validateCostLimits(nil, dp("0")) != nil {
		t.Fatal("zero/null rejected")
	}
}

func TestCalendarPeriods(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	d, m := CalendarPeriods(now)
	if d.Day() != 1 || m.Day() != 1 || d.Hour() != 0 {
		t.Fatalf("bad boundary %v %v", d, m)
	}
	prev := now.Add(-time.Hour)
	d, _ = CalendarPeriods(prev)
	if d.Day() != 30 || d.Month() != 6 {
		t.Fatalf("bad previous day %v", d)
	}
}

func TestKeepLiveGroupIDs(t *testing.T) {
	liveID := uuid.New()
	retired := uuid.New()
	live := map[uuid.UUID]struct{}{liveID: {}}

	next, changed := keepLiveGroupIDs([]uuid.UUID{liveID, retired}, live)
	if !changed || len(next) != 1 || next[0] != liveID {
		t.Fatalf("retired id kept: %v changed=%v", next, changed)
	}

	// Emptying the list would open the key to every group, so it is kept.
	onlyRetired, changed := keepLiveGroupIDs([]uuid.UUID{retired}, live)
	if changed || len(onlyRetired) != 1 || onlyRetired[0] != retired {
		t.Fatalf("retired-only list = %v changed=%v, want kept", onlyRetired, changed)
	}

	same, changed := keepLiveGroupIDs([]uuid.UUID{liveID}, live)
	if changed || len(same) != 1 || same[0] != liveID {
		t.Fatalf("live list rewritten: %v changed=%v", same, changed)
	}

	empty, changed := keepLiveGroupIDs(nil, live)
	if changed || len(empty) != 0 {
		t.Fatalf("empty list changed: %v", empty)
	}
}
