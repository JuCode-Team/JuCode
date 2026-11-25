package biz

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestParseQuotaHeadersAnthropic(t *testing.T) {
	now := time.Now()
	reset := now.Add(90 * time.Minute).Unix()
	h := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.82"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       []string{itoa(int(reset))},
		"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"41"},
	}
	windows := parseQuotaHeaders(h, now)
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}
	byName := map[string]QuotaWindow{}
	for _, w := range windows {
		byName[w.Name] = w
	}
	// 0.82 is a fraction and 41 is already a percentage: both must land on 0-100.
	if got := byName["5h"].UsedPercent; got < 81 || got > 83 {
		t.Fatalf("5h used = %v, want ~82", got)
	}
	if got := byName["7d"].UsedPercent; got != 41 {
		t.Fatalf("7d used = %v, want 41", got)
	}
	if byName["5h"].ResetAt != reset {
		t.Fatalf("5h reset = %d, want %d", byName["5h"].ResetAt, reset)
	}
}

func TestParseQuotaHeadersCodex(t *testing.T) {
	now := time.Now()
	h := http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"97"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"600"},
		"X-Codex-Secondary-Used-Percent":      []string{"10"},
	}
	windows := parseQuotaHeaders(h, now)
	byName := map[string]QuotaWindow{}
	for _, w := range windows {
		byName[w.Name] = w
	}
	if byName["5h"].UsedPercent != 97 {
		t.Fatalf("primary used = %v, want 97", byName["5h"].UsedPercent)
	}
	if d := time.Until(time.Unix(byName["5h"].ResetAt, 0)); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("primary reset in %s, want ~10m", d)
	}
	if byName["7d"].UsedPercent != 10 {
		t.Fatalf("secondary used = %v, want 10", byName["7d"].UsedPercent)
	}
}

func TestMergeQuotaSnapshotKeepsKnownWindows(t *testing.T) {
	existing := snapshotMap(QuotaSnapshot{
		Source: "probe",
		Plan:   "max",
		At:     100,
		Windows: []QuotaWindow{
			{Name: "5h", UsedPercent: 10, ResetAt: 111},
			{Name: "7d", UsedPercent: 20, ResetAt: 222},
		},
	})
	merged := mergeQuotaSnapshot(existing, QuotaSnapshot{
		Source:  "passive",
		At:      200,
		Windows: []QuotaWindow{{Name: "5h", UsedPercent: 88, ResetAt: 333}},
	})
	if merged.Source != "passive" || merged.At != 200 {
		t.Fatalf("source/at not refreshed: %+v", merged)
	}
	if merged.Plan != "max" {
		t.Fatalf("plan lost: %q", merged.Plan)
	}
	if len(merged.Windows) != 2 {
		t.Fatalf("windows = %d, want 2 (one refreshed, one kept)", len(merged.Windows))
	}
	byName := map[string]QuotaWindow{}
	for _, w := range merged.Windows {
		byName[w.Name] = w
	}
	if byName["5h"].UsedPercent != 88 {
		t.Fatalf("5h not refreshed: %v", byName["5h"].UsedPercent)
	}
	if byName["7d"].UsedPercent != 20 {
		t.Fatalf("7d dropped: %v", byName["7d"].UsedPercent)
	}
}

func TestWindowSelected(t *testing.T) {
	// The short window gates traffic by default; the weekly one is reported but
	// does not park a healthy account.
	if !windowSelected(QuotaPolicy{}, "5h") {
		t.Fatal("5h must participate by default")
	}
	if windowSelected(QuotaPolicy{}, "7d") {
		t.Fatal("7d must be informational unless opted in")
	}
	if !windowSelected(QuotaPolicy{Windows: []string{"7d"}}, "7d") {
		t.Fatal("an explicit window list must win")
	}
	if windowSelected(QuotaPolicy{Windows: []string{"7d"}}, "5h") {
		t.Fatal("windows outside the list must not participate")
	}
}

func TestQuotaPolicyFor(t *testing.T) {
	svc := healthSvc()

	t.Run("accounts do not auto-pause by default", func(t *testing.T) {
		policy := svc.QuotaPolicyFor(context.Background(), &ent.UpstreamAccount{})
		if policy.PauseAtPercent != 0 {
			t.Fatalf("pause threshold = %v, want 0 for pay-per-token keys", policy.PauseAtPercent)
		}
	})

	t.Run("account override wins", func(t *testing.T) {
		acct := &ent.UpstreamAccount{
			QuotaPolicy: map[string]any{"pause_at_percent": float64(80), "windows": []any{"7d"}},
		}
		policy := svc.QuotaPolicyFor(context.Background(), acct)
		if policy.PauseAtPercent != 80 {
			t.Fatalf("pause threshold = %v, want 80", policy.PauseAtPercent)
		}
		if len(policy.Windows) != 1 || policy.Windows[0] != "7d" {
			t.Fatalf("windows = %v, want [7d]", policy.Windows)
		}
	})
}

func TestParseFirecrawlCreditUsage(t *testing.T) {
	snapshot, err := parseFirecrawlCreditUsage([]byte(`{"success":true,"data":{"remainingCredits":1500,"planCredits":3000,"billingPeriodEnd":"2026-11-01T00:00:00Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != 1500 || snapshot.Balance.Currency != "credits" {
		t.Fatalf("balance = %+v", snapshot.Balance)
	}
	if _, err := parseFirecrawlCreditUsage([]byte(`{"data":{}}`)); err == nil {
		t.Fatal("a body without remainingCredits must fail")
	}
}

func TestParseQuotaHeadersRateLimitPolicy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := http.Header{
		"X-Ratelimit-Policy":    []string{"1;w=1, 15000;w=2592000"},
		"X-Ratelimit-Limit":     []string{"1, 15000"},
		"X-Ratelimit-Remaining": []string{"0, 14523"},
		"X-Ratelimit-Reset":     []string{"1, 86400"},
	}
	windows := parseQuotaHeaders(h, now)
	// The per-second window is a rate limit, not a quota.
	if len(windows) != 1 {
		t.Fatalf("windows = %+v, want only the monthly one", windows)
	}
	w := windows[0]
	if w.Name != "30d" || w.Limit != 15000 || w.Used != 477 || w.ResetAt != now.Unix()+86400 {
		t.Fatalf("window = %+v", w)
	}
	if w.UsedPercent < 3.17 || w.UsedPercent > 3.19 {
		t.Fatalf("used percent = %v", w.UsedPercent)
	}
}

func TestParseQuotaHeadersRateLimitPolicyUnlimited(t *testing.T) {
	h := http.Header{
		"X-Ratelimit-Policy":    []string{"20;w=1, 0;w=2592000"},
		"X-Ratelimit-Limit":     []string{"20, 0"},
		"X-Ratelimit-Remaining": []string{"19, 0"},
	}
	if windows := parseQuotaHeaders(h, time.Now()); len(windows) != 0 {
		t.Fatalf("an unlimited monthly window must not look exhausted: %+v", windows)
	}
}

func TestWebAccountServesNoChatProtocol(t *testing.T) {
	acct := &ent.UpstreamAccount{NativeProtocols: []string{NativeProtocolWeb}}
	for _, p := range []RequestProtocol{RequestProtocolOpenAI, RequestProtocolCompatible, RequestProtocolResponses, RequestProtocolAnthropic} {
		if AccountServesRequest(acct, p, true) {
			t.Fatalf("a web account must not serve %q", p)
		}
	}
	if !AccountSupportsRequestProtocol(acct, RequestProtocolAny, false) {
		t.Fatal("web tool routing (protocol any) must reach the web account")
	}
}
