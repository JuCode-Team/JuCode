package biz

import (
	"testing"

	"github.com/shopspring/decimal"
)

// These tests cover the PROVIDER SETTLEMENT semantics of ToCNY/FromCNY —
// the only remaining legitimate users of the exchange rate (provider
// costs may be entered in USD and are converted at settlement time).
// User-facing billing paths do NOT call these functions: 1 point = 1 CNY.

func TestSettlementConversionUSDAndCNY(t *testing.T) {
	rate := decimal.NewFromFloat(7.2)
	usd := decimal.NewFromInt(10)
	cny := decimal.NewFromInt(72)

	if got := ToCNY(usd, CurrencyUSD, rate); !got.Equal(cny) {
		t.Fatalf("ToCNY USD = %s, want %s", got, cny)
	}
	if got := FromCNY(cny, CurrencyUSD, rate); !got.Equal(usd) {
		t.Fatalf("FromCNY USD = %s, want %s", got, usd)
	}
	if got := ToCNY(cny, CurrencyCNY, rate); !got.Equal(cny) {
		t.Fatalf("ToCNY CNY = %s, want %s", got, cny)
	}
	if got := FromCNY(cny, CurrencyCNY, rate); !got.Equal(cny) {
		t.Fatalf("FromCNY CNY = %s, want %s", got, cny)
	}
}

// Unknown currency codes pass through unchanged — defensive: silently
// doubling or zeroing a settlement figure is worse than a small visible
// mismatch the operator can correct.
func TestSettlementConversionUnknownCurrencyPassthrough(t *testing.T) {
	rate := decimal.NewFromFloat(7.2)
	amount := decimal.NewFromInt(42)

	for _, c := range []string{"EUR", "jpy", "", "  "} {
		if got := ToCNY(amount, c, rate); !got.Equal(amount) {
			t.Fatalf("ToCNY(%q) = %s, want passthrough %s", c, got, amount)
		}
		if got := FromCNY(amount, c, rate); !got.Equal(amount) {
			t.Fatalf("FromCNY(%q) = %s, want passthrough %s", c, got, amount)
		}
	}
}

// rate ≤ 0 means "rate not configured yet" — pass through unconverted
// rather than zeroing or erroring out the settlement math.
func TestSettlementConversionNonPositiveRatePassthrough(t *testing.T) {
	amount := decimal.NewFromInt(10)

	for _, rate := range []decimal.Decimal{decimal.Zero, decimal.NewFromInt(-1)} {
		if got := ToCNY(amount, CurrencyUSD, rate); !got.Equal(amount) {
			t.Fatalf("ToCNY rate %s = %s, want passthrough %s", rate, got, amount)
		}
		if got := FromCNY(amount, CurrencyUSD, rate); !got.Equal(amount) {
			t.Fatalf("FromCNY rate %s = %s, want passthrough %s", rate, got, amount)
		}
	}
}
