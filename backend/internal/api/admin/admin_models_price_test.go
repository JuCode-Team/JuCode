package admin

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
)

func ptr(s string) *string { return &s }

func priceOpEq(a, b biz.PriceOp) bool {
	if a.Clear != b.Clear {
		return false
	}
	if a.Value == nil && b.Value == nil {
		return true
	}
	if a.Value == nil || b.Value == nil {
		return false
	}
	return a.Value.Equal(*b.Value)
}

func TestParseOptionalPrice(t *testing.T) {
	t.Parallel()

	op, msg := parseOptionalPrice(nil, "cache_read_input_price")
	if msg != "" || !priceOpEq(op, biz.PriceOp{}) {
		t.Fatalf("omit: op=%+v msg=%q, want zero", op, msg)
	}

	op, msg = parseOptionalPrice(ptr(""), "cache_read_input_price")
	if msg != "" || !priceOpEq(op, biz.ClearPrice()) {
		t.Fatalf("empty: op=%+v msg=%q, want Clear", op, msg)
	}

	op, msg = parseOptionalPrice(ptr("  "), "cache_read_input_price")
	if msg != "" || !priceOpEq(op, biz.ClearPrice()) {
		t.Fatalf("whitespace: op=%+v msg=%q, want Clear", op, msg)
	}

	op, msg = parseOptionalPrice(ptr(" 1.25 "), "cache_read_input_price")
	want := biz.SetPrice(decimal.RequireFromString("1.25"))
	if msg != "" || !priceOpEq(op, want) {
		t.Fatalf("set: op=%+v msg=%q, want %v", op, msg, *want.Value)
	}

	op, msg = parseOptionalPrice(ptr("nope"), "cache_read_input_price")
	if msg != "invalid cache_read_input_price" || !priceOpEq(op, biz.PriceOp{}) {
		t.Fatalf("garbage: op=%+v msg=%q", op, msg)
	}
}

func TestParseRequiredDec(t *testing.T) {
	t.Parallel()

	d, msg := parseRequiredDec(nil, "input_unit_price")
	if msg != "" || d != nil {
		t.Fatalf("omit: d=%v msg=%q", d, msg)
	}

	d, msg = parseRequiredDec(ptr(""), "input_unit_price")
	if d != nil || msg == "" {
		t.Fatalf("empty should reject, got d=%v msg=%q", d, msg)
	}

	d, msg = parseRequiredDec(ptr("0.4"), "input_unit_price")
	if msg != "" || d == nil || !d.Equal(decimal.RequireFromString("0.4")) {
		t.Fatalf("set: d=%v msg=%q", d, msg)
	}
}

func TestParseCreateDecEmptyIsUnset(t *testing.T) {
	t.Parallel()

	d, msg := parseCreateDec(ptr(""), "audio_input_unit_price")
	if msg != "" || d != nil {
		t.Fatalf("empty create: d=%v msg=%q, want unset", d, msg)
	}
	d, msg = parseCreateDec(ptr("2"), "audio_input_unit_price")
	if msg != "" || d == nil || !d.Equal(decimal.RequireFromString("2")) {
		t.Fatalf("set create: d=%v msg=%q", d, msg)
	}
}
