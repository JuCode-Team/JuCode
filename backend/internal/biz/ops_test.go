package biz

import (
	"bytes"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestParseOpsGrain(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if g := ParseOpsGrain("", from, from.Add(10*24*time.Hour)); g != "day" {
		t.Fatalf("short span: %s", g)
	}
	if g := ParseOpsGrain("", from, from.Add(120*24*time.Hour)); g != "month" {
		t.Fatalf("long span: %s", g)
	}
	if g := ParseOpsGrain("DAY", from, from.Add(120*24*time.Hour)); g != "day" {
		t.Fatalf("explicit: %s", g)
	}
}

func TestRatioAndPerUser(t *testing.T) {
	if RatioString(1, 4) != "0.2500" {
		t.Fatalf("ratio %s", RatioString(1, 4))
	}
	if RatioString(1, 0) != "0" {
		t.Fatal("zero den")
	}
	amt, err := decimal.NewFromString("100")
	if err != nil {
		t.Fatal(err)
	}
	if PerUserAmount(amt, 4) != "25.0000" {
		t.Fatalf("arpu %s", PerUserAmount(amt, 4))
	}
}

func TestDeltaAndRefundRate(t *testing.T) {
	if DeltaRatioInt(120, 100) != "0.2000" {
		t.Fatalf("int %s", DeltaRatioInt(120, 100))
	}
	if DeltaRatioInt(10, 0) != "" {
		t.Fatal("zero prev")
	}
	cur, _ := decimal.NewFromString("80")
	prev, _ := decimal.NewFromString("100")
	if DeltaRatioDec(cur, prev) != "-0.2000" {
		t.Fatalf("dec %s", DeltaRatioDec(cur, prev))
	}
	negCur, _ := decimal.NewFromString("-50")
	negPrev, _ := decimal.NewFromString("-100")
	if DeltaRatioDec(negCur, negPrev) != "0.5000" {
		t.Fatalf("neg base %s", DeltaRatioDec(negCur, negPrev))
	}
	if refundRate(cur, prev) != "0.8000" {
		t.Fatalf("refund %s", refundRate(cur, prev))
	}
	if refundRate(cur, decimal.Zero) != "0" {
		t.Fatal("zero gross")
	}
}

func TestFillOpsPeriods(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	from := time.Date(2026, 1, 15, 0, 0, 0, 0, loc)
	to := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	got := fillOpsPeriods(from, to, "month", loc)
	if len(got) != 2 || got[0] != "2026-01" || got[1] != "2026-02" {
		t.Fatalf("months %#v", got)
	}
	days := fillOpsPeriods(from, from.AddDate(0, 0, 3), "day", loc)
	if len(days) != 3 {
		t.Fatalf("days %d", len(days))
	}
}

func TestWriteOpsXLSX(t *testing.T) {
	var buf bytes.Buffer
	err := WriteOpsXLSX(&buf, &OpsReport{
		From:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Grain: "month",
		KPI:   OpsKPI{GrossRevenue: "1", NetRevenue: "1"},
		Series: []OpsPoint{{
			Period: "2026-01", NewUsers: 2, Revenue: "1", Refunds: "0", Net: "1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 200 || !bytes.HasPrefix(buf.Bytes(), []byte("PK")) {
		t.Fatalf("not xlsx, len=%d", buf.Len())
	}
}
