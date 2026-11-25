package llm

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jucode/jucode/internal/biz"
)

// errorClass is the structured classification of a single upstream
// attempt's outcome. It is the single source of truth for the three
// decisions that used to be scattered across substring matchers:
//   - whether to fail over to the next provider  (Retryable)
//   - whether the attempt counts toward provider health (HealthOutcome)
//   - whether it was a content-safety block       (IsSafetyBlock)
//
// It also drives whether a failure marks the provider for runtime
// cooldown (CountsForCooldown).
type errorClass string

const (
	classOK              errorClass = "ok"
	classProviderFailure errorClass = "provider_failure" // 5xx, transport, mid-stream
	classRateLimit       errorClass = "rate_limit"       // 429
	classTimeout         errorClass = "timeout"          // first-text / idle / deadline
	classAuth            errorClass = "auth"             // channel-level 4xx: 401/402/403/404/408
	classUserRequest     errorClass = "user_request"     // request-level 4xx: 400/413/422...
	classUserPolicy      errorClass = "user_policy"      // our own output-policy block
	classCyberBlock      errorClass = "cyber_block"      // upstream cyber/moderation block
	classPoolExhausted   errorClass = "pool_exhausted"   // upstream account pool dry (global capacity, not a bad channel)
	classUpstreamConfig  errorClass = "upstream_config"  // our upstream cannot route the request (405 / "no upstream provider could serve")
	classClientCanceled  errorClass = "client_canceled"  // caller disconnected/canceled
	classUnknown         errorClass = "unknown"          // unclassifiable result
)

// Cooldown durations by failure class. Rate-limit cooldown is clamped to
// [min,max] so honoring a huge upstream Retry-After can't over-isolate a
// channel, and a floor of 15s stops fast re-probing a still-saturated pool
// from amplifying 429s. Auth-style dead keys cool much longer (no probe
// brings them back; an operator must rotate the key).
const (
	cooldownDefault  = 2 * time.Minute
	cooldownRateMin  = 15 * time.Second
	cooldownRateMax  = 60 * time.Second
	cooldownAuthLong = 10 * time.Minute
)

// looksLikePoolExhausted detects the upstream "account pool is dry" signal
// (upstream dispatch exhausted / "No available accounts"). This is a
// GLOBAL capacity condition, not a per-channel fault: retrying a sibling
// channel that shares the same pool just fans out onto the same empty pool
// (the 1014/287≈3.5≈max_attempts amplification we observed). It is kept
// distinct from new-api "可用渠道不存在" (a config/no-channel error handled
// elsewhere).
//
// TODO(robustness): ask the upstream to emit a structured header
// (e.g. X-Pool-Status: exhausted) and key off that instead of matching
// free-text bodies, which are vendor-version fragile.
func looksLikePoolExhausted(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, p := range []string{
		"no available account",
		"no shared",
		"dispatch.exhausted",
		"dispatch exhausted",
	} {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// classifyUpstreamResponse classifies a completed HTTP response by status
// and (for non-2xx) body. Cyber/moderation blocks are detected on the
// body regardless of status code so a 400/403-wrapped block is still
// caught — but a 2xx is always success (we never re-flag a delivered
// response).
func classifyUpstreamResponse(status int, body []byte) errorClass {
	if status >= 200 && status < 300 {
		return classOK
	}
	if looksLikeCyberSecurityBlock(body) {
		return classCyberBlock
	}
	if looksLikePoolExhausted(body) {
		return classPoolExhausted
	}
	// A route gap in an upstream relay (it answers 405 or "no upstream
	// provider could serve this request") is a service-side failure, not the
	// caller's: it must count against availability and stay visible in the
	// error monitor. It is deliberately NOT retryable/cooldown-worthy — a single
	// 405 used to be one of ~60k successes on that channel, and cooling the
	// whole channel down for it would turn a rare gap into an outage.
	if status >= 400 && status < 500 && biz.IsUpstreamRouteGap(status, string(body)) {
		return classUpstreamConfig
	}
	switch {
	case status == http.StatusTooManyRequests:
		return classRateLimit
	case status >= 500:
		return classProviderFailure
	case status >= 400:
		if isRetryableClientStatus(status) {
			return classAuth
		}
		return classUserRequest
	default:
		return classUnknown
	}
}

// deriveErrorClass maps an attempt's recorded (switch-strategy, status,
// body/error message) tuple to an errorClass. Strategy is the primary
// signal because it disambiguates transport/timeout failures that have
// no HTTP status; an empty strategy means "judge by the response itself"
// (covers 2xx success and request-level 4xx finals). Keeping this in one
// place means every attempt — and therefore every health sample — is
// classified identically.
func deriveErrorClass(strategy string, status int, body string) errorClass {
	// Body-driven classes take precedence over the status/strategy: a 503
	// whose body says "no available accounts" is pool exhaustion, not a
	// generic provider failure, and must not be retried/cooled like one.
	if len(body) > 0 {
		if looksLikeCyberSecurityBlock([]byte(body)) {
			return classCyberBlock
		}
		if looksLikePoolExhausted([]byte(body)) {
			return classPoolExhausted
		}
	}
	switch strategy {
	case "":
		return classifyUpstreamResponse(status, []byte(body))
	case "pool_exhausted":
		return classPoolExhausted
	case "status_429":
		return classRateLimit
	case "first_text_timeout", "stream_idle_timeout", "stream_total_timeout":
		return classTimeout
	case "stream_incomplete", "stream_error", "forward_error", "relay_restart_retry", "build_provider_error", "response_read_error":
		return classProviderFailure
	case "output_policy_block":
		return classUserPolicy
	case "client_canceled":
		return classClientCanceled
	case "request_prepare_error":
		return classUnknown
	}
	// A terminal `response.failed` frame arrives after a 200 header, so the HTTP
	// status carries no information; the upstream error code appended by the
	// caller is the only signal. Without this the whole family fell through to
	// classUnknown, which counted user-caused aborts (content blocks, over-long
	// prompts) against public availability.
	if code, ok := strings.CutPrefix(strategy, "response_failed"); ok {
		return responseFailedClass(strings.TrimPrefix(code, ":"))
	}
	if strings.HasPrefix(strategy, "status_") {
		return classProviderFailure // 5xx
	}
	if strings.HasPrefix(strategy, "client_status_") {
		code, _ := strconv.Atoi(strings.TrimPrefix(strategy, "client_status_"))
		return classifyUpstreamResponse(code, []byte(body))
	}
	return classUnknown
}

// responseFailedClass maps the error code carried by an upstream
// `response.failed` frame onto a class. The codes are the ones observed in
// production; an unrecognized or absent code defaults to provider_failure
// because the upstream aborted a response it had already committed to, which
// is the same shape of fault as a 5xx.
func responseFailedClass(code string) errorClass {
	switch code {
	case "rate_limit_exceeded":
		return classRateLimit
	case "cyber_policy":
		return classCyberBlock
	case "context_length_exceeded", "invalid_request_error", "invalid_prompt", "string_above_max_length":
		return classUserRequest
	default:
		return classProviderFailure
	}
}

// Retryable reports whether a failure of this class should fail over to
// the next provider. Channel-level problems (provider down, rate-limited,
// timed out, auth/quota on this channel) may succeed elsewhere; request-
// level and safety failures fail identically everywhere.
func (c errorClass) Retryable() bool {
	switch c {
	case classProviderFailure, classRateLimit, classTimeout, classAuth:
		return true
	default:
		return false
	}
}

// CountsForCooldown reports whether a failure of this class should put
// the provider into a short runtime cooldown. Same set as Retryable:
// only genuine channel-level failures, never user_request/cyber/policy.
func (c errorClass) CountsForCooldown() bool { return c.Retryable() }

// IsSafetyBlock reports whether this was an upstream content-safety block.
func (c errorClass) IsSafetyBlock() bool { return c == classCyberBlock }

// HealthOutcome reports whether the attempt counts as a provider-health
// sample and, if so, whether it counts as a failure. Only provider-side
// failures (and successes) are samples; user_request / user_policy /
// cyber_block / unknown are excluded so user-side errors never drag a
// provider's health down.
func (c errorClass) HealthOutcome() (count, failed bool) {
	switch c {
	case classOK:
		return true, false
	case classProviderFailure, classRateLimit, classTimeout, classAuth:
		return true, true
	default:
		return false, false
	}
}

// parseRetryAfter extracts a backoff hint from upstream rate-limit headers:
// the standard Retry-After (delta-seconds or HTTP-date) plus common
// provider reset headers. Returns ok=false on absent/garbage input rather
// than erroring.
func parseRetryAfter(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d, true
			}
		}
	}
	for _, k := range []string{
		"X-Ratelimit-Reset-Requests",
		"X-Ratelimit-Reset",
		"Anthropic-Ratelimit-Requests-Reset",
		"Anthropic-Ratelimit-Tokens-Reset",
	} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second, true
			}
		}
	}
	return 0, false
}

// accountFailureClass maps a gateway strategy string onto the failure class the
// account health state machine understands. Strategy strings are the ones the
// attempt loop records ("status_429", "first_text_timeout", …), so this keeps
// the health verdict identical whichever path reported the failure.
func accountFailureClass(reason string) string {
	switch reason {
	case "first_text_timeout", "stream_idle_timeout", "stream_total_timeout":
		return string(classTimeout)
	case "pool_exhausted":
		return string(classPoolExhausted)
	case "output_policy_block":
		return string(classUserPolicy)
	case "client_canceled":
		return string(classClientCanceled)
	}
	if strings.HasPrefix(reason, "status_") || strings.HasPrefix(reason, "client_status_") {
		code, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(reason, "client_"), "status_"))
		if code > 0 {
			return string(classifyUpstreamResponse(code, nil))
		}
	}
	if strings.HasPrefix(reason, "response_failed") {
		code, _ := strings.CutPrefix(reason, "response_failed")
		return string(responseFailedClass(strings.TrimPrefix(code, ":")))
	}
	return string(classProviderFailure)
}

// cooldownFor returns how long to runtime-cool a provider after a failure
// of the given class. Returns 0 (no cooldown) for pool exhaustion, since
// that is a global capacity condition rather than a bad channel.
func cooldownFor(class errorClass, status int, h http.Header) time.Duration {
	switch class {
	case classPoolExhausted:
		return 0
	case classRateLimit:
		d := cooldownRateMin
		if ra, ok := parseRetryAfter(h); ok {
			d = ra
		}
		if d < cooldownRateMin {
			d = cooldownRateMin
		}
		if d > cooldownRateMax {
			d = cooldownRateMax
		}
		return d
	case classAuth:
		// 401/403/404 are dead-key style (non self-healing) → cool long.
		// 402 (payment) / 408 (timeout) are transient → default.
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return cooldownAuthLong
		default:
			return cooldownDefault
		}
	default:
		return cooldownDefault
	}
}

// captureUpstreamRequestID harvests the provider's own request id from
// response headers for admin-side reconciliation. Multiple known headers
// are joined so we keep whatever the vendor sent (OpenAI, Codex,
// Cloudflare, AWS). Result is bounded to the column width by the caller.
func captureUpstreamRequestID(h http.Header) string {
	if h == nil {
		return ""
	}
	var parts []string
	for _, k := range []string{
		"X-Request-Id",
		"Openai-Request-Id",
		"X-Codex-Request-Id",
		"Cf-Ray",
		"X-Amzn-Requestid",
	} {
		if v := h.Get(k); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return truncate(strings.Join(parts, "; "), 256)
}
