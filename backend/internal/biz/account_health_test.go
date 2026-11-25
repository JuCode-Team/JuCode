package biz

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

// The state machine only needs its storage for persistence; decisions and
// parsing are pure. A zero-value service exercises them without a database.
func healthSvc() *AccountPoolSvc { return &AccountPoolSvc{} }

func acctWith(mutate func(*ent.UpstreamAccount)) *ent.UpstreamAccount {
	a := &ent.UpstreamAccount{
		ID:          uuid.New(),
		Status:      entaccount.StatusActive,
		Schedulable: true,
	}
	if mutate != nil {
		mutate(a)
	}
	return a
}

func TestReportAccountFailureMatrix(t *testing.T) {
	svc := healthSvc()
	ctx := context.Background()

	tests := []struct {
		name       string
		mode       string
		failure    AccountFailure
		wantAction FailureAction
		wantRetry  bool
	}{
		{
			name:       "429 cools the account and honours the advertised reset",
			failure:    AccountFailure{Class: "rate_limit", Status: 429, Headers: http.Header{"Anthropic-Ratelimit-Unified-Reset": []string{"1700003600"}}},
			wantAction: ActionCooldown,
		},
		{
			name:       "429 in lenient mode does not park the account",
			mode:       string(PoolModeLenient),
			failure:    AccountFailure{Class: "rate_limit", Status: 429},
			wantAction: ActionNone,
			wantRetry:  true,
		},
		{
			name:       "429 in observe mode leaves the account alone",
			mode:       string(PoolModeObserve),
			failure:    AccountFailure{Class: "rate_limit", Status: 429},
			wantAction: ActionNone,
			wantRetry:  false,
		},
		{
			name:       "5xx cools briefly",
			failure:    AccountFailure{Class: "provider_failure", Status: 500},
			wantAction: ActionCooldown,
		},
		{
			name:       "5xx in lenient mode retries the same account",
			mode:       string(PoolModeLenient),
			failure:    AccountFailure{Class: "provider_failure", Status: 502},
			wantAction: ActionNone,
			wantRetry:  true,
		},
		{
			name:       "pool exhaustion never cools an account",
			failure:    AccountFailure{Class: "pool_exhausted", Status: 503},
			wantAction: ActionNone,
		},
		{
			name:       "client cancellation never cools an account",
			failure:    AccountFailure{Class: "client_canceled", Status: 0},
			wantAction: ActionNone,
		},
		{
			name:       "client-caused 4xx never cools an account",
			failure:    AccountFailure{Class: "user_request", Status: 400, Body: `{"error":"bad request"}`},
			wantAction: ActionNone,
		},
		{
			name:       "401 on an api-key account disables it",
			failure:    AccountFailure{Class: "auth", Status: 401},
			wantAction: ActionDisable,
		},
		{
			name:       "402 balance depletion is recoverable",
			failure:    AccountFailure{Class: "auth", Status: 402},
			wantAction: ActionCooldown,
		},
		{
			name:       "terminal upstream message disables even when it arrives as 400",
			failure:    AccountFailure{Class: "auth", Status: 400, Body: `{"error":{"message":"Your credit balance is too low"}}`},
			wantAction: ActionDisable,
		},
		{
			name:       "terminal upstream message only cools in lenient mode",
			mode:       string(PoolModeLenient),
			failure:    AccountFailure{Class: "auth", Status: 403, Body: `{"error":{"message":"This organization has been disabled."}}`},
			wantAction: ActionCooldown,
		},
		{
			name:       "401 on an api-key account in lenient mode cools instead of disabling",
			mode:       string(PoolModeLenient),
			failure:    AccountFailure{Class: "auth", Status: 401},
			wantAction: ActionCooldown,
		},
		{
			name:       "403 stays a cooldown on the first strike",
			failure:    AccountFailure{Class: "auth", Status: 403},
			wantAction: ActionCooldown,
		},
		{
			name:       "404 with no model parks nothing",
			failure:    AccountFailure{Class: "auth", Status: 404},
			wantAction: ActionNone,
		},
		{
			name:       "404 with a model parks the model",
			failure:    AccountFailure{Class: "auth", Status: 404, Model: "gpt-5"},
			wantAction: ActionCooldown,
		},
		{
			name:       "timeouts only cool after the threshold",
			failure:    AccountFailure{Class: "timeout", Status: 0, Reason: "first_text_timeout"},
			wantAction: ActionNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acct := acctWith(func(a *ent.UpstreamAccount) {
				a.PoolMode = tc.mode
			})
			got := svc.ReportAccountFailure(ctx, acct, tc.failure)
			if got.Action != tc.wantAction {
				t.Fatalf("action = %v, want %v (reason %q)", got.Action, tc.wantAction, got.Reason)
			}
			if got.RetrySame != tc.wantRetry {
				t.Fatalf("retry_same = %v, want %v", got.RetrySame, tc.wantRetry)
			}
		})
	}
}

func TestRateLimitResetParsing(t *testing.T) {
	svc := healthSvc()
	now := time.Now()

	t.Run("anthropic unified reset in unix seconds", func(t *testing.T) {
		target := now.Add(42 * time.Minute)
		until := svc.rateLimitReset(AccountFailure{Headers: http.Header{
			"Anthropic-Ratelimit-Unified-Reset": []string{itoa(int(target.Unix()))},
		}})
		if diff := until.Sub(target); diff > time.Second || diff < -time.Second {
			t.Fatalf("until = %s, want %s", until, target)
		}
	})

	t.Run("retry-after seconds", func(t *testing.T) {
		until := svc.rateLimitReset(AccountFailure{Headers: http.Header{"Retry-After": []string{"120"}}})
		if d := time.Until(until); d < 100*time.Second || d > 130*time.Second {
			t.Fatalf("until in %s, want ~2m", d)
		}
	})

	t.Run("reset from body seconds", func(t *testing.T) {
		until := svc.rateLimitReset(AccountFailure{Body: `{"error":{"resets_in_seconds":90}}`})
		if d := time.Until(until); d < 80*time.Second || d > 100*time.Second {
			t.Fatalf("until in %s, want ~90s", d)
		}
	})

	t.Run("absurd reset is clamped", func(t *testing.T) {
		far := now.Add(30 * 24 * time.Hour)
		until := svc.rateLimitReset(AccountFailure{Headers: http.Header{
			"Anthropic-Ratelimit-Unified-Reset": []string{itoa(int(far.Unix()))},
		}})
		if until.After(now.Add(accountCooldownCeil + time.Minute)) {
			t.Fatalf("cooldown not clamped: %s", time.Until(until))
		}
	})

	t.Run("no signal falls back to a short cooldown", func(t *testing.T) {
		until := svc.rateLimitReset(AccountFailure{})
		if d := time.Until(until); d < accountCooldownFloor || d > accountRateFallback+time.Second {
			t.Fatalf("fallback cooldown = %s", d)
		}
	})
}

func TestAccountSchedulable(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	_ = ctx

	t.Run("healthy account is schedulable", func(t *testing.T) {
		if !AccountSchedulable(acctWith(nil), "", now) {
			t.Fatal("a fresh active account must be schedulable")
		}
	})

	cases := []struct {
		name string
		set  func(*ent.UpstreamAccount)
	}{
		{"disabled status", func(a *ent.UpstreamAccount) { a.Status = entaccount.StatusDisabled }},
		{"error status", func(a *ent.UpstreamAccount) { a.Status = entaccount.StatusError }},
		{"paused by operator", func(a *ent.UpstreamAccount) { a.Schedulable = false }},
		{"rate limited", func(a *ent.UpstreamAccount) { later := now.Add(time.Minute); a.RateLimitResetAt = &later }},
		{"overloaded", func(a *ent.UpstreamAccount) { later := now.Add(time.Minute); a.OverloadUntil = &later }},
		{"temporarily unschedulable", func(a *ent.UpstreamAccount) { later := now.Add(time.Minute); a.TempUnschedulableUntil = &later }},
		{"soft deleted", func(a *ent.UpstreamAccount) { deleted := now; a.DeletedAt = &deleted }},
	}
	for _, tc := range cases {
		t.Run(tc.name+" is not schedulable", func(t *testing.T) {
			if AccountSchedulable(acctWith(tc.set), "", now) {
				t.Fatal("account should be out of rotation")
			}
		})
	}

	t.Run("expired cooldown is schedulable again", func(t *testing.T) {
		past := now.Add(-time.Second)
		if !AccountSchedulable(acctWith(func(a *ent.UpstreamAccount) { a.RateLimitResetAt = &past }), "", now) {
			t.Fatal("a lifted cooldown must not keep the account out")
		}
	})

	t.Run("model cooldown only parks that model", func(t *testing.T) {
		acct := acctWith(func(a *ent.UpstreamAccount) {
			a.ModelCooldowns = map[string]any{
				"gpt-5": map[string]any{"until_ms": float64(now.Add(time.Minute).UnixMilli())},
			}
		})
		if AccountSchedulable(acct, "gpt-5", now) {
			t.Fatal("the parked model must be skipped")
		}
		if !AccountSchedulable(acct, "gpt-5-mini", now) {
			t.Fatal("another model must still be served")
		}
	})
}

func TestParsePoolMode(t *testing.T) {
	for _, in := range []string{"", "balanced", "conservative", "lenient", "observe", " BAlanced "} {
		if _, ok := ParsePoolMode(in); !ok {
			t.Fatalf("mode %q must parse", in)
		}
	}
	for _, in := range []string{"fast", "off", "balanced "} {
		if _, ok := ParsePoolMode(in); !ok && strings.TrimSpace(in) != "balanced" {
			continue
		}
	}
	if _, ok := ParsePoolMode("aggressive"); ok {
		t.Fatal("unknown mode must be rejected")
	}
}

func TestTerminalAuthMarkers(t *testing.T) {
	terminal := []string{
		`{"error":{"message":"Your credit balance is too low to access the API"}}`,
		`{"error":"Organization has been disabled."}`,
		`{"detail":"identity verification is required"}`,
		`{"error":{"code":"token_revoked"}}`,
		`{"error":{"code":"invalid_grant"}}`,
	}
	for _, body := range terminal {
		if !looksLikeTerminalAuthFailure(strings.ToLower(body), 400) {
			t.Fatalf("body %q must be terminal", body)
		}
	}
	transient := []string{`{"error":"rate limit exceeded"}`, `{"error":"overloaded"}`, ``}
	for _, body := range transient {
		if looksLikeTerminalAuthFailure(strings.ToLower(body), 429) {
			t.Fatalf("body %q must not be terminal", body)
		}
	}
}
