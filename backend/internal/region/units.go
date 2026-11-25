package region

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// UnitsPerCNY is the fixed-point scale used in Redis. Redis floating point
// counters are deliberately avoided because billing must not depend on IEEE
// 754 rounding.
const UnitsPerCNY int64 = 1_000_000

func ParseUnits(value string) (int64, error) {
	d, err := decimal.NewFromString(value)
	if err != nil {
		return 0, fmt.Errorf("invalid monetary value %q: %w", value, err)
	}
	u := d.Mul(decimal.NewFromInt(UnitsPerCNY))
	if !u.Equal(u.Truncate(0)) {
		return 0, fmt.Errorf("monetary value %q has more than 6 decimal places", value)
	}
	return u.IntPart(), nil
}

// ParseUnitsRounded converts a computed monetary amount to the fixed-point
// precision used by the Redis ledger. Configuration values use ParseUnits so
// typos with excess precision are rejected; model usage calculations can
// naturally produce more than six decimal places and should be rounded to the
// nearest micro-CNY instead of falling back to a reservation.
func ParseUnitsRounded(value string) (int64, error) {
	d, err := decimal.NewFromString(value)
	if err != nil {
		return 0, fmt.Errorf("invalid monetary value %q: %w", value, err)
	}
	return d.Mul(decimal.NewFromInt(UnitsPerCNY)).Round(0).IntPart(), nil
}

func FormatUnits(units int64) string {
	return decimal.New(units, 0).Div(decimal.NewFromInt(UnitsPerCNY)).StringFixed(6)
}
