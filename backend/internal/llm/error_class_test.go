package llm

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyUpstreamResponse(t *testing.T) {
	cyber := []byte(`{"error":{"message":"blocked for cyber security reasons"}}`)
	// The relay wording: typed invalid_request_error, but it is a safety
	// block and must not fall through to the 400 → user_request bucket.
	contentFilter := []byte(`{"error":{"type":"invalid_request_error","message":"Request was blocked by upstream content filtering"}}`)
	cases := []struct {
		name   string
		status int
		body   []byte
		want   errorClass
	}{
		{"2xx ok", 200, []byte(`{"ok":true}`), classOK},
		{"2xx ignores cyber body", 200, cyber, classOK}, // never re-flag a delivered response
		{"429 rate limit", 429, nil, classRateLimit},
		{"500 provider failure", 500, nil, classProviderFailure},
		{"503 provider failure", 503, nil, classProviderFailure},
		{"401 auth", 401, nil, classAuth},
		{"402 auth", 402, nil, classAuth},
		{"403 auth", 403, []byte(`{"error":"forbidden"}`), classAuth},
		{"404 auth", 404, nil, classAuth},
		{"408 auth", 408, nil, classAuth},
		{"400 user request", 400, []byte(`{"error":"bad json"}`), classUserRequest},
		{"413 user request", 413, nil, classUserRequest},
		{"422 user request", 422, nil, classUserRequest},
		{"cyber block on 400", 400, cyber, classCyberBlock},
		{"cyber block on 403", 403, cyber, classCyberBlock},
		{"content filtering block on 400", 400, contentFilter, classCyberBlock},
		{"content filtering block on 403", 403, contentFilter, classCyberBlock},
		{"2xx ignores content filtering body", 200, contentFilter, classOK},
		{"unknown 3xx", 302, nil, classUnknown},
	}
	for _, tc := range cases {
		if got := classifyUpstreamResponse(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: classifyUpstreamResponse(%d) = %q, want %q", tc.name, tc.status, got, tc.want)
		}
	}
}

func TestErrorClassDecisions(t *testing.T) {
	cases := []struct {
		c        errorClass
		retry    bool
		cooldown bool
		safety   bool
		count    bool
		failed   bool
	}{
		{classOK, false, false, false, true, false},
		{classProviderFailure, true, true, false, true, true},
		{classRateLimit, true, true, false, true, true},
		{classTimeout, true, true, false, true, true},
		{classAuth, true, true, false, true, true},
		{classUserRequest, false, false, false, false, false},
		{classUserPolicy, false, false, false, false, false},
		{classCyberBlock, false, false, true, false, false},
		{classUnknown, false, false, false, false, false},
	}
	for _, tc := range cases {
		if got := tc.c.Retryable(); got != tc.retry {
			t.Errorf("%s.Retryable() = %v, want %v", tc.c, got, tc.retry)
		}
		if got := tc.c.CountsForCooldown(); got != tc.cooldown {
			t.Errorf("%s.CountsForCooldown() = %v, want %v", tc.c, got, tc.cooldown)
		}
		if got := tc.c.IsSafetyBlock(); got != tc.safety {
			t.Errorf("%s.IsSafetyBlock() = %v, want %v", tc.c, got, tc.safety)
		}
		count, failed := tc.c.HealthOutcome()
		if count != tc.count || failed != tc.failed {
			t.Errorf("%s.HealthOutcome() = (%v,%v), want (%v,%v)", tc.c, count, failed, tc.count, tc.failed)
		}
	}
}

func TestDeriveErrorClass(t *testing.T) {
	cases := []struct {
		strategy string
		status   int
		body     string
		want     errorClass
	}{
		{"", 200, "", classOK},
		{"", 400, "bad", classUserRequest},
		{"", 403, "cyber security block", classCyberBlock},
		{"status_429", 429, "", classRateLimit},
		{"status_502", 502, "", classProviderFailure},
		{"client_status_401", 401, "", classAuth},
		{"client_status_400", 400, "", classUserRequest},
		{"first_text_timeout", 0, "", classTimeout},
		{"stream_idle_timeout", 0, "", classTimeout},
		{"stream_error", 0, "", classProviderFailure},
		{"forward_error", 0, "", classProviderFailure},
		{"build_provider_error", 0, "", classProviderFailure},
		{"output_policy_block", 422, "", classUserPolicy},
		{"client_canceled", 0, "", classClientCanceled},
		{"request_prepare_error", 0, "", classUnknown},
	}
	for _, tc := range cases {
		if got := deriveErrorClass(tc.strategy, tc.status, tc.body); got != tc.want {
			t.Errorf("deriveErrorClass(%q,%d,%q) = %q, want %q", tc.strategy, tc.status, tc.body, got, tc.want)
		}
	}
}

func TestPoolExhaustedClassification(t *testing.T) {
	bodies := []string{
		`{"error":{"message":"No available accounts: no available accounts","type":"api_error"}}`,
		`{"error":{"message":"No available accounts: this group only allows Claude"}}`,
		`dispatch.exhausted`,
	}
	for _, b := range bodies {
		if got := classifyUpstreamResponse(503, []byte(b)); got != classPoolExhausted {
			t.Errorf("classifyUpstreamResponse(503,%q)=%q want pool_exhausted", b, got)
		}
		// And via the per-attempt derivation (strategy=status_503).
		if got := deriveErrorClass("status_503", 503, b); got != classPoolExhausted {
			t.Errorf("deriveErrorClass(status_503,%q)=%q want pool_exhausted", b, got)
		}
	}
	// pool_exhausted must NOT retry / cooldown / count for health.
	if classPoolExhausted.Retryable() || classPoolExhausted.CountsForCooldown() {
		t.Fatal("pool_exhausted must be non-retryable and non-cooldown")
	}
	if count, _ := classPoolExhausted.HealthOutcome(); count {
		t.Fatal("pool_exhausted must not count toward health")
	}
	// A plain 503 without the pool signal stays provider_failure.
	if got := classifyUpstreamResponse(503, []byte(`{"error":"overloaded"}`)); got != classProviderFailure {
		t.Errorf("generic 503 = %q want provider_failure", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	if _, ok := parseRetryAfter(h); ok {
		t.Fatal("empty header should not parse")
	}
	h.Set("Retry-After", "30")
	if d, ok := parseRetryAfter(h); !ok || d != 30*time.Second {
		t.Fatalf("Retry-After=30 → %v,%v", d, ok)
	}
	h2 := http.Header{}
	h2.Set("X-Ratelimit-Reset", "12")
	if d, ok := parseRetryAfter(h2); !ok || d != 12*time.Second {
		t.Fatalf("reset header → %v,%v", d, ok)
	}
}

func TestCooldownFor(t *testing.T) {
	// pool exhausted → no cooldown.
	if d := cooldownFor(classPoolExhausted, 503, nil); d != 0 {
		t.Fatalf("pool cooldown = %v want 0", d)
	}
	// 429 with no header → floor 15s.
	if d := cooldownFor(classRateLimit, 429, nil); d != cooldownRateMin {
		t.Fatalf("429 cooldown = %v want %v", d, cooldownRateMin)
	}
	// 429 honoring a huge Retry-After is clamped to max 60s.
	h := http.Header{}
	h.Set("Retry-After", "3600")
	if d := cooldownFor(classRateLimit, 429, h); d != cooldownRateMax {
		t.Fatalf("429 clamp = %v want %v", d, cooldownRateMax)
	}
	// 401 dead-key → long cooldown; 402 transient → default.
	if d := cooldownFor(classAuth, 401, nil); d != cooldownAuthLong {
		t.Fatalf("401 cooldown = %v want %v", d, cooldownAuthLong)
	}
	if d := cooldownFor(classAuth, 402, nil); d != cooldownDefault {
		t.Fatalf("402 cooldown = %v want %v", d, cooldownDefault)
	}
	if d := cooldownFor(classProviderFailure, 502, nil); d != cooldownDefault {
		t.Fatalf("5xx cooldown = %v want %v", d, cooldownDefault)
	}
}

func TestCaptureUpstreamRequestID(t *testing.T) {
	h := http.Header{}
	if got := captureUpstreamRequestID(h); got != "" {
		t.Errorf("empty header should yield empty id, got %q", got)
	}
	h.Set("X-Request-Id", "req_123")
	h.Set("Cf-Ray", "ray_abc")
	got := captureUpstreamRequestID(h)
	if got != "X-Request-Id=req_123; Cf-Ray=ray_abc" {
		t.Errorf("unexpected captured id: %q", got)
	}
}
