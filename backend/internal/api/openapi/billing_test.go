package openapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
)

func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal %q: %v", s, err)
	}
	return d
}

// Every key checker in this ecosystem computes
//
//	remaining = soft_limit_usd - total_usage/100
//
// If that does not land on the real balance, users see a wrong number in a
// third-party tool and open a support ticket. Pin the round trip.
func TestBillingPairSubtractsToBalance(t *testing.T) {
	balance := mustDec(t, "128.45")
	spend := mustDec(t, "71.55")
	sc := billingScope{granted: balance.Add(spend), used: spend}

	sub := subscriptionBody(sc)
	usage := usageBody(sc)

	softLimit := sub["soft_limit_usd"].(float64)
	totalUsage := usage["total_usage"].(float64)

	remaining := softLimit - totalUsage/100
	if remaining != 128.45 {
		t.Errorf("remaining = %v, want 128.45 (soft_limit %v − total_usage %v/100)",
			remaining, softLimit, totalUsage)
	}
	// total_usage is 分, not 元.
	if totalUsage != 7155 {
		t.Errorf("total_usage = %v, want 7155 (71.55 元 → 分)", totalUsage)
	}
}

// The field names are upstream's contract, not ours. Renaming one compiles
// fine and breaks every client silently, so assert the exact set.
func TestCompatFieldNamesAreFrozen(t *testing.T) {
	sc := billingScope{granted: mustDec(t, "200"), used: mustDec(t, "71.55"), accessUntil: 0}

	t.Run("subscription", func(t *testing.T) {
		got := marshalKeys(t, subscriptionBody(sc))
		want := []string{"access_until", "hard_limit_usd", "has_payment_method", "object", "soft_limit_usd", "system_hard_limit_usd"}
		assertKeys(t, got, want)
		if v := subscriptionBody(sc)["object"]; v != "billing_subscription" {
			t.Errorf("object = %v, want billing_subscription", v)
		}
		// All three limits carry the same number upstream; clients read
		// whichever one they were written against.
		b := subscriptionBody(sc)
		if b["soft_limit_usd"] != b["hard_limit_usd"] || b["soft_limit_usd"] != b["system_hard_limit_usd"] {
			t.Error("the three *_limit_usd fields must be equal")
		}
	})

	t.Run("usage", func(t *testing.T) {
		got := marshalKeys(t, usageBody(sc))
		assertKeys(t, got, []string{"object", "total_usage"})
		if v := usageBody(sc)["object"]; v != "list" {
			t.Errorf("object = %v, want list", v)
		}
	})
}

func marshalKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	have := map[string]bool{}
	for _, k := range got {
		have[k] = true
	}
	for _, k := range want {
		if !have[k] {
			t.Errorf("missing field %q", k)
		}
		delete(have, k)
	}
	for k := range have {
		t.Errorf("unexpected extra field %q — this response shape is frozen by upstream", k)
	}
}

// access_until is a Unix second count, and "no expiry" must be 0 rather than
// a negative or a zero-time epoch — clients render anything nonzero as a date.
func TestExpiryUnix(t *testing.T) {
	if got := expiryUnix(nil); got != 0 {
		t.Errorf("expiryUnix(nil) = %d, want 0", got)
	}
	if got := expiryUnix(&ent.ApiKey{}); got != 0 {
		t.Errorf("expiryUnix(no expiry) = %d, want 0", got)
	}
	var zero time.Time
	if got := expiryUnix(&ent.ApiKey{ExpiresAt: &zero}); got != 0 {
		t.Errorf("expiryUnix(zero time) = %d, want 0, not the epoch offset", got)
	}
	at := time.Unix(1786000000, 0)
	if got := expiryUnix(&ent.ApiKey{ExpiresAt: &at}); got != 1786000000 {
		t.Errorf("expiryUnix = %d, want 1786000000", got)
	}
}

// Only a key with a positive total cost limit is a sub-budget. Treating an
// unset limit as a ceiling would report every uncapped key as having no
// funds; treating a capped key as uncapped would let a 10-元 key claim the
// whole account balance.
func TestIsCappedKey(t *testing.T) {
	positive := mustDec(t, "10")
	zero := decimal.Zero
	negative := mustDec(t, "-1")

	tests := []struct {
		name string
		key  *ent.ApiKey
		want bool
	}{
		{"nil key (OAuth device token)", nil, false},
		{"no limit set", &ent.ApiKey{}, false},
		{"zero limit means no cap", &ent.ApiKey{TotalCostLimit: &zero}, false},
		{"negative limit is not a cap", &ent.ApiKey{TotalCostLimit: &negative}, false},
		{"positive limit", &ent.ApiKey{TotalCostLimit: &positive}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCappedKey(tc.key); got != tc.want {
				t.Errorf("isCappedKey = %v, want %v", got, tc.want)
			}
		})
	}
}
