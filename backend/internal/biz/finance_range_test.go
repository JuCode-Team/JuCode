package biz

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestNormalizeFinanceRangeDateOnlyInclusive(t *testing.T) {
	loc := Location()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 8, 26, 0, 0, 0, 0, loc)
	gotFrom, gotTo := NormalizeFinanceRange(from, to, true, true)
	if !gotFrom.Equal(from) {
		t.Fatalf("from: got %v want %v", gotFrom, from)
	}
	wantTo := time.Date(2026, 8, 27, 0, 0, 0, 0, loc)
	if !gotTo.Equal(wantTo) {
		t.Fatalf("to (exclusive next day): got %v want %v", gotTo, wantTo)
	}
}

func TestNormalizeFinanceRangeDefaultLast30Days(t *testing.T) {
	gotFrom, gotTo := NormalizeFinanceRange(time.Time{}, time.Time{}, false, false)
	span := gotTo.Sub(gotFrom)
	if span < 29*24*time.Hour || span > 31*24*time.Hour {
		t.Fatalf("default span %v, want ~30d", span)
	}
}

func TestNormalizeFinanceRangeClampsSpan(t *testing.T) {
	loc := Location()
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
	gotFrom, gotTo := NormalizeFinanceRange(from, to, false, false)
	if gotTo.Sub(gotFrom) > maxFinanceSpan {
		t.Fatalf("span %v exceeds cap %v", gotTo.Sub(gotFrom), maxFinanceSpan)
	}
}

func TestFillFinanceDaysZeroFills(t *testing.T) {
	loc := Location()
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 8, 4, 0, 0, 0, 0, loc)
	income := map[string]decimal.Decimal{"2026-08-02": decimal.RequireFromString("10")}
	days := fillFinanceDays(from, to, income, nil)
	if len(days) != 3 {
		t.Fatalf("days: got %d want 3", len(days))
	}
	if days[0].Date != "2026-08-01" || !days[0].Income.IsZero() {
		t.Fatalf("day 0 = %+v", days[0])
	}
	if days[1].Income.String() != "10" {
		t.Fatalf("day 1 income = %s", days[1].Income)
	}
}
