package biz

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// memberLimitErr wraps ErrMemberLimit so the two member caps can be told apart
// in a message. Six call sites — three HTTP mappers, the availability
// classifier, and two gateway paths — branch on errors.Is(err, ErrMemberLimit)
// to answer 429 and to exclude the request from availability metrics. If the
// wrapping ever stops matching, org requests silently become 500s and start
// counting against uptime.
func TestMemberLimitErrStaysMatchable(t *testing.T) {
	for _, which := range []string{"monthly spend limit reached", "total spend limit reached"} {
		err := memberLimitErr(which)
		if !errors.Is(err, ErrMemberLimit) {
			t.Fatalf("memberLimitErr(%q) no longer matches ErrMemberLimit; every 429 mapping breaks", which)
		}
		if !strings.Contains(err.Error(), which) {
			t.Errorf("message %q lost the distinguishing detail %q", err.Error(), which)
		}
	}
	// The two must remain distinguishable from each other by message, since
	// that is the only thing telling an operator which cap was hit.
	if memberLimitErr("monthly spend limit reached").Error() == memberLimitErr("total spend limit reached").Error() {
		t.Error("the two member limits produce identical messages")
	}
}

// A cap of zero means "unlimited", not "may spend nothing". Both limits are
// non-nillable decimals defaulting to zero, so every existing member row has
// zeros — reading those as a hard stop would freeze every organization on
// deploy.
func TestZeroLimitMeansUnlimited(t *testing.T) {
	// This mirrors the guard used at every enforcement site: Sign() > 0.
	for _, tc := range []struct {
		name    string
		limit   string
		enforce bool
	}{
		{"unset default", "0", false},
		{"explicit zero", "0.00000000", false},
		{"positive", "0.00000001", true},
		{"large", "1000", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := decimal.NewFromString(tc.limit)
			if err != nil {
				t.Fatalf("bad fixture: %v", err)
			}
			if got := d.Sign() > 0; got != tc.enforce {
				t.Errorf("limit %q: enforced=%v, want %v", tc.limit, got, tc.enforce)
			}
		})
	}
}

// The cumulative cap compares against lifetime spend, so the comparison must
// be the same one the charge path uses: used+cost > limit blocks, used+cost ==
// limit is allowed to land exactly on the cap.
func TestTotalLimitBoundary(t *testing.T) {
	limit := decimal.RequireFromString("100")
	tests := []struct {
		name        string
		used, cost  string
		shouldBlock bool
	}{
		{"well under", "10", "1", false},
		{"lands exactly on the cap", "99", "1", false},
		{"one unit over", "99", "1.00000001", true},
		{"already over", "100", "0.00000001", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			used := decimal.RequireFromString(tc.used)
			cost := decimal.RequireFromString(tc.cost)
			if got := used.Add(cost).GreaterThan(limit); got != tc.shouldBlock {
				t.Errorf("used=%s cost=%s limit=%s: block=%v, want %v",
					tc.used, tc.cost, limit, got, tc.shouldBlock)
			}
		})
	}
}

// UpsertMember is the only writer of these caps and the only place a negative
// value could get in. A negative cap would make Sign() > 0 false and silently
// read as "unlimited" — the opposite of what an operator typing "-1" expects.
func TestUpsertMemberRejectsNegativeLimits(t *testing.T) {
	svc := &OrganizationSvc{}
	neg := decimal.RequireFromString("-1")
	zero := decimal.Zero

	for _, tc := range []struct {
		name    string
		monthly decimal.Decimal
		total   decimal.Decimal
	}{
		{"negative monthly", neg, zero},
		{"negative total", zero, neg},
		{"both negative", neg, neg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.UpsertMember(t.Context(), uuid.New(), OrganizationMemberInput{
				UserID:            uuid.New(),
				Role:              "member",
				MonthlySpendLimit: tc.monthly,
				TotalSpendLimit:   tc.total,
			})
			if !errors.Is(err, ErrInvalidAmount) {
				t.Errorf("got %v, want ErrInvalidAmount (validation must run before any DB access)", err)
			}
		})
	}
}
