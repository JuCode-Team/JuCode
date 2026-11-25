package biz

import (
	"strings"

	"github.com/shopspring/decimal"
)

// Currency / unit-of-account conventions.
//
// The platform's unit of account is 积分 (points): 1 point = 1 CNY
// (recharge 1:1), stored with currency = "CNY". Internal storage values
// are unchanged from the historical CNY ledger — every historical CNY
// value IS the point value.
//
// User-facing paths (billing, orders, topups, refunds, plans, redeem
// codes) do NOT convert anything: model prices are entered directly in
// points and every ledger row is written in points.
//
// ToCNY/FromCNY exist ONLY for the provider (channel) settlement link:
// provider costs may be entered in USD and are converted at settlement
// time (see FinanceSvc.providerSettlementUsageDetails /
// providerSettlementManualDetails / sumCNYByCurrency). Calling them from
// any user billing path is forbidden.
//
// Persistence convention: balance_tx.currency, usage_log.currency, and
// any other ledger-side column are written as "CNY" unconditionally —
// the value is already in points by the time it reaches the writer.

const (
	CurrencyCNY = "CNY"
	CurrencyUSD = "USD"
)

// moneyScaleExp is the ledger's own resolution: every monetary column in the
// schema is numeric(20,8), so 1e8 is the smallest amount the system can hold.
//
// Redis counters that accumulate money scale by it and add as int64 via
// INCRBY. The two alternatives are both wrong: INCRBYFLOAT drifts, and
// refusing values that carry more precision silently under-counts — CalcCost
// divides by 1000 per component, so over-precise costs are the norm, not the
// exception.
//
// int64 headroom: 9.22e18 / 1e8 ≈ 9.2e10 points. No window of spend approaches
// that, but moneyToFixed guards rather than wrapping a counter.
const moneyScaleExp = 8

// moneyToFixed quantizes an amount to the ledger's precision and returns it as
// a scaled int64.
//
// Quantizing is not a loss of fidelity against the source of truth: Postgres
// rounds half-away-from-zero when storing numeric(20,8), so this produces the
// same value the SQL path would read back.
//
// Returns false only on overflow, where the caller should drop the increment
// rather than wrap the counter.
func moneyToFixed(d decimal.Decimal) (int64, bool) {
	shifted := d.Round(moneyScaleExp).Shift(moneyScaleExp)
	if shifted.GreaterThan(decimal.NewFromInt(1<<62)) || shifted.LessThan(decimal.NewFromInt(-(1 << 62))) {
		return 0, false
	}
	return shifted.IntPart(), true
}

// moneyFromFixed is the inverse of moneyToFixed.
func moneyFromFixed(n int64) decimal.Decimal {
	return decimal.New(n, -moneyScaleExp)
}

// ToCNY converts amount to CNY using cnyPerUSD as the spot rate.
// PROVIDER SETTLEMENT ONLY — see the file header; never call from
// user-facing billing paths.
//
// CNY input passes through. USD input is multiplied by cnyPerUSD. Unknown
// currency codes fall through as-is — defensive: silently doubling or
// zeroing a value is worse than a tiny visible mismatch the operator
// can correct.
//
// cnyPerUSD ≤ 0 is treated as "rate not configured yet" — same
// fall-through behavior. The settings service warms its cache at boot
// so the first request after startup hits a populated value.
func ToCNY(amount decimal.Decimal, currency string, cnyPerUSD decimal.Decimal) decimal.Decimal {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if c == "" || c == CurrencyCNY {
		return amount
	}
	if c != CurrencyUSD {
		return amount
	}
	if cnyPerUSD.Sign() <= 0 {
		return amount
	}
	return amount.Mul(cnyPerUSD)
}

// FromCNY converts amount from CNY back into the requested currency.
// PROVIDER SETTLEMENT ONLY — see the file header; never call from
// user-facing billing paths.
//
// CNY input passes through. USD input is divided by cnyPerUSD.
// Unknown currency codes fall through as-is.
func FromCNY(amount decimal.Decimal, currency string, cnyPerUSD decimal.Decimal) decimal.Decimal {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if c == "" || c == CurrencyCNY {
		return amount
	}
	if c != CurrencyUSD {
		return amount
	}
	if cnyPerUSD.Sign() <= 0 {
		return amount
	}
	return amount.Div(cnyPerUSD)
}
