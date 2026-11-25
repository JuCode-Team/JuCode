package llm

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestActionableUpstreamError(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantOK   bool
		wantCode string
	}{
		{
			name:     "image generation disabled (403 permission_error)",
			status:   http.StatusForbidden,
			body:     `{"error":{"message":"Image generation is not enabled for this group","type":"permission_error","param":"","code":null}}`,
			wantOK:   true,
			wantCode: "feature_not_enabled",
		},
		{
			name:     "anthropic input plus max_tokens over the limit",
			status:   http.StatusBadRequest,
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"input length and ` + "`max_tokens`" + ` exceed context limit: 188240 + 32000 > 200000, decrease input length or ` + "`max_tokens`" + ` and try again"}}`,
			wantOK:   true,
			wantCode: "context_length_exceeded",
		},
		{
			name:     "context length exceeded",
			status:   http.StatusBadRequest,
			body:     `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`,
			wantOK:   true,
			wantCode: "context_length_exceeded",
		},
		{
			name:     "upstream stream_read_error",
			status:   http.StatusBadGateway,
			body:     `{"error":{"message":"stream_read_error","type":"upstream_error","param":"","code":"stream_read_error"}}`,
			wantOK:   true,
			wantCode: "stream_read_error",
		},
		{
			name:     "missing required parameter → endpoint/request-format error",
			status:   http.StatusBadRequest,
			body:     `{"error":{"message":"Missing required parameter: 'input[5].name'.","type":"invalid_request_error","param":"","code":null}}`,
			wantOK:   true,
			wantCode: "endpoint_or_request_format",
		},
		{
			name:     "generic invalid_request_error is surfaced",
			status:   http.StatusBadRequest,
			body:     `{"error":{"message":"invalid model","type":"invalid_request_error"}}`,
			wantOK:   true,
			wantCode: "invalid_request",
		},
		{
			name:   "auth/forbidden body stays masked",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"Forbidden","type":"permission_error"}}`,
			wantOK: false,
		},
		{
			name:   "empty body",
			status: http.StatusInternalServerError,
			body:   ``,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, code, ok := actionableUpstreamError([]byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (msg=%q code=%q)", ok, tc.wantOK, msg, code)
			}
			if !tc.wantOK {
				return
			}
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if msg == "" {
				t.Fatalf("message is empty for %q", tc.name)
			}
		})
	}
}

// TestEndpointOrRequestFormatErrorNamesParamOnly pins the request-shape
// rejection contract: fixed plain-language copy plus the offending parameter
// path, so the user knows where to look. The upstream's raw message is still
// NOT relayed — upstream error text is normalized to fixed Chinese copy (see
// 659ec28); only a structural parameter path passes through.
func TestEndpointOrRequestFormatErrorNamesParamOnly(t *testing.T) {
	const upstream = "Missing required parameter: 'input[5].name'."
	body := `{"error":{"message":"` + upstream + `","type":"invalid_request_error","param":"","code":null}}`
	msg, code, ok := actionableUpstreamError([]byte(body))
	if !ok || code != "endpoint_or_request_format" {
		t.Fatalf("ok=%v code=%q, want true / endpoint_or_request_format", ok, code)
	}
	if msg != "端点配置错误或请求格式错误，请检查请求参数（相关参数：input[5].name）" {
		t.Fatalf("message %q is not the fixed copy plus the parameter path", msg)
	}
	if strings.Contains(msg, upstream) {
		t.Fatalf("message %q leaks the upstream detail", msg)
	}
}

// TestContentFilterBlockIsNotRelayedVerbatim pins the handling of the
// upstream content-filter rejection, which arrives typed as a plain
// invalid_request_error:
//
//	{"error":{"type":"invalid_request_error",
//	          "message":"Request was blocked by upstream content filtering"}}
//
// It is a safety block, so it must take the cyber path — our own wording, the
// cyber_safety_block code — instead of being echoed through the
// invalid_request arm of actionableUpstreamError.
func TestContentFilterBlockIsNotRelayedVerbatim(t *testing.T) {
	const upstream = "Request was blocked by upstream content filtering"
	body := `{"error":{"type":"invalid_request_error","message":"` + upstream + `"}}`

	if msg, code, ok := actionableUpstreamError([]byte(body)); ok {
		t.Fatalf("actionableUpstreamError relayed a safety block: code=%q msg=%q", code, msg)
	}
	code, msg := failureReason(nil, http.StatusBadRequest, body)
	if code != "cyber_safety_block" {
		t.Fatalf("code = %q, want cyber_safety_block (msg=%q)", code, msg)
	}
	if msg != cyberSafetyMessage() {
		t.Fatalf("message = %q, want the safety message %q", msg, cyberSafetyMessage())
	}
	if strings.Contains(msg, upstream) {
		t.Fatalf("message %q echoes the upstream phrasing", msg)
	}
}

// TestOversizedRequestBodyIsExplained pins the handling of the upstream
// oversized-payload rejection:
//
//	{"error":{"type":"invalid_request_error",
//	          "message":"The upstream request body is too large. Reduce image
//	                     count, attachment size, or conversation history."}}
//
// It used to fall through to the generic invalid_request arm, and on the
// native Anthropic branch all the way to "request rejected by service". It
// must produce its own code and a Chinese message naming the fix.
func TestOversizedRequestBodyIsExplained(t *testing.T) {
	const upstream = "The upstream request body is too large. Reduce image count, attachment size, or conversation history."
	body := `{"error":{"type":"invalid_request_error","message":"` + upstream + `"}}`

	msg, code, ok := actionableUpstreamError([]byte(body))
	if !ok || code != "request_body_too_large" {
		t.Fatalf("ok=%v code=%q, want true / request_body_too_large", ok, code)
	}
	if !strings.Contains(msg, "请求体过大") {
		t.Fatalf("message %q does not name the oversized body in Chinese", msg)
	}
	if strings.Contains(msg, upstream) {
		t.Fatalf("message %q relays the upstream English instead of explaining it", msg)
	}
	if c, m := failureReason(nil, http.StatusBadRequest, body); c != "request_body_too_large" || m != msg {
		t.Fatalf("failureReason = (%q,%q), want (request_body_too_large,%q)", c, m, msg)
	}

	// The same condition reaches us under wordings that share no substring
	// with the one above: a relay's pre-flight reject that reports its own
	// byte/token limits and never forwarded, and a 413 carrying our own
	// wording (a chained JuCode/edge in front of us). All must land in the
	// same bucket rather than degrading to the generic reject.
	for name, variant := range map[string]string{
		"relay pre-flight reject": `{"error":{"type":"invalid_request_error","message":"Request is too large for claude-sonnet-5 after safe local compression (1353063 body bytes, 1319704 input-text bytes, 0 image bytes, approximately 359228 input tokens; body limit 4718592, text limit 950000, token limit 993573). Reduce old conversation history/tool output or the number/size of attached images or PDFs. The request was not sent upstream."}}`,
		"our own 413 wording":     `{"error":{"type":"invalid_request_error","code":"request_body_too_large","message":"request body too large"}}`,
	} {
		if m, c, ok := actionableUpstreamError([]byte(variant)); !ok || c != "request_body_too_large" || m != msg {
			t.Fatalf("%s: ok=%v code=%q msg=%q, want true / request_body_too_large / %q", name, ok, c, m, msg)
		}
	}
}

// TestImageTooSmallIsExplained pins Grok/xAI's image-input floor, which
// arrives as type=api_error (not invalid_request_error) and used to fall
// through to the generic "request rejected by service":
//
//	{"error":{"message":"Image has 336 total pixels (14x24), which is
//	          below the minimum of 512 pixels.","type":"api_error"},
//	 "type":"error"}
//
// The user can fix this (send a larger image), so we must explain it in
// Chinese and keep the measured size. Sibling wordings that do not carry
// WxH still land in the same bucket.
func TestImageTooSmallIsExplained(t *testing.T) {
	const grok = `{"error":{"message":"Image has 336 total pixels (14x24), which is below the minimum of 512 pixels.","type":"api_error"},"type":"error"}`
	msg, code, ok := actionableUpstreamError([]byte(grok))
	if !ok || code != "image_too_small" {
		t.Fatalf("ok=%v code=%q, want true / image_too_small (msg=%q)", ok, code, msg)
	}
	if !strings.Contains(msg, "图片") || !strings.Contains(msg, "过小") {
		t.Fatalf("message %q does not name the too-small image in Chinese", msg)
	}
	if !strings.Contains(msg, "14") || !strings.Contains(msg, "24") || !strings.Contains(msg, "512") {
		t.Fatalf("message %q dropped the measured size", msg)
	}
	if strings.Contains(msg, "total pixels") || strings.Contains(msg, "below the minimum") {
		t.Fatalf("message %q relays the upstream English instead of explaining it", msg)
	}
	if c, m := failureReason(nil, http.StatusBadRequest, grok); c != "image_too_small" || m != msg {
		t.Fatalf("failureReason = (%q,%q), want (image_too_small,%q)", c, m, msg)
	}

	for name, variant := range map[string]string{
		"no dimensions": `{"error":{"message":"The image is too small. Minimum is 512 pixels.","type":"api_error"}}`,
		"openai shape":  `{"error":{"message":"Image dimensions 14x24 are below the minimum of 512 total pixels.","type":"invalid_request_error"}}`,
	} {
		m, c, ok := actionableUpstreamError([]byte(variant))
		if !ok || c != "image_too_small" {
			t.Fatalf("%s: ok=%v code=%q msg=%q, want true / image_too_small", name, ok, c, m)
		}
		if !strings.Contains(m, "过小") {
			t.Fatalf("%s: message %q does not name the too-small image", name, m)
		}
	}
}

func TestProductionUserActionableErrorsAreExplained(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
		wantSub  string
		noSub    string
	}{
		{
			name:     "grok maximum prompt length",
			body:     `{"error":{"message":"This model's maximum prompt length is 500000 but the request contains 503999 tokens.","type":"api_error"},"type":"error"}`,
			wantCode: "context_length_exceeded",
			wantSub:  "上下文过长",
			noSub:    "maximum prompt length",
		},
		{
			name:     "prompt is too long type nil",
			body:     `{"error":{"type":"<nil>","message":"prompt is too long: 1451079 tokens > 1000000 maximum"},"type":"error"}`,
			wantCode: "context_length_exceeded",
			wantSub:  "上下文过长",
		},
		{
			name:     "invalid image data url",
			body:     `{"error":{"code":"invalid_value","message":"Invalid 'input[0].content[1].image_url'. Expected a base64-encoded data URL with an image MIME type (e.g. 'data:image/png;base64,aW1n')"}}`,
			wantCode: "invalid_image",
			wantSub:  "图片",
			noSub:    "aW1n",
		},
		{
			name:     "unsupported anthropic server tool",
			body:     `{"error":{"code":"server_error","message":"当前不支持 Anthropic server tool type=\"x_search\""}}`,
			wantCode: "unsupported_request",
			wantSub:  "不支持",
		},
		{
			name:     "unsupported effort none",
			body:     `{"error":{"code":"server_error","message":"不支持 output_config.effort=\"none\""}}`,
			wantCode: "unsupported_request",
			wantSub:  "不支持",
		},
		{
			name:     "unsupported model InvalidParameter",
			body:     `{"request_id":"x","error":{"type":"InvalidParameter","message":"Unsupported model: 'deepseek-v4-flash'."}}`,
			wantCode: "unsupported_request",
			wantSub:  "不支持",
		},
		{
			name:     "field messages required",
			body:     `{"error":{"message":"field messages is required","type":"aigateway_error","code":"invalid_text_request"}}`,
			wantCode: "endpoint_or_request_format",
			wantSub:  "请求参数",
		},
		{
			name:     "tool name too long",
			body:     `{"error":{"type":"<nil>","message":"工具名称过长，请检查: mcp__plugin_chrome-devtools"},"type":"error"}`,
			wantCode: "unsupported_request",
			wantSub:  "工具名称过长",
		},
		{
			name:     "unsupported input_text",
			body:     `{"error":{"code":"server_error","message":"当前不支持 content.type=input_text"}}`,
			wantCode: "unsupported_request",
			wantSub:  "不支持",
		},
		{
			name:     "codex chatgpt image model",
			body:     `{"error":{"message":"gpt-image-2-pro is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"}}`,
			wantCode: "unsupported_request",
			wantSub:  "不支持",
		},
		{
			name:     "duplicate tool_use id",
			body:     `{"error":{"code":"server_error","message":"检测到重复的 tool_use id，请新开会话"}}`,
			wantCode: "session_corrupt",
			wantSub:  "新开",
		},
		{
			name:     "invalid tool use format",
			body:     `{"error":{"type":"invalid_request_error","message":"Invalid tool use format. Please start a new session."}}`,
			wantCode: "session_corrupt",
			wantSub:  "新开",
		},
		{
			name:     "illegal developer role",
			body:     `{"error":{"type":"aigateway_error","message":"developer is not one of ['system', 'assistant', 'user']"}}`,
			wantCode: "endpoint_or_request_format",
			wantSub:  "请求参数",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, code, ok := actionableUpstreamError([]byte(tc.body))
			if !ok || code != tc.wantCode {
				t.Fatalf("ok=%v code=%q msg=%q, want true / %s", ok, code, msg, tc.wantCode)
			}
			if !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("message %q does not contain %q", msg, tc.wantSub)
			}
			if tc.noSub != "" && strings.Contains(msg, tc.noSub) {
				t.Fatalf("message %q still contains %q", msg, tc.noSub)
			}
			if c, m := failureReason(nil, http.StatusBadRequest, tc.body); c != tc.wantCode || m != msg {
				t.Fatalf("failureReason = (%q,%q)", c, m)
			}
		})
	}
}

func TestChannelLeakBodiesStayMasked(t *testing.T) {
	bodies := []string{
		`{"error":{"type":"invalid_request_error","message":"The requested model is not available for the configured upstream account."}}`,
		`{"error":{"message":"Upstream access forbidden, please contact administrator","type":"upstream_error"}}`,
		`{"error":{"message":"预扣费额度失败, 用户剩余额度: ¥0.000216, 需要预扣费额度: ¥0.010920","type":"upstream_error"}}`,
		`{"error":{"message":"No available accounts: no available accounts","type":"api_error"}}`,
		`{"error":{"message":"Access to model denied. Please make sure you are eligible for using the model.","type":"AccessDenied.Unpurchased","code":"AccessDenied.Unpurchased"}}`,
		`{"error":{"message":"All credentials for model gpt-5.6-luna are cooling down","type":"rate_limit_error"}}`,
		`{"error":{"type":"overloaded_error","message":"all accounts are busy — please retry shortly"}}`,
	}
	for _, body := range bodies {
		if msg, code, ok := actionableUpstreamError([]byte(body)); ok {
			t.Fatalf("leaked channel body: code=%q msg=%q body=%s", code, msg, body)
		}
	}
}

func TestFailureReason(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		status   int
		body     string
		wantCode string
		// wantMsgLeak, when set, must NOT appear in the client message.
		wantMsgLeak string
	}{
		{
			name:     "idle timeout",
			err:      errUpstreamStreamIdleTimeout,
			wantCode: "stream_idle_timeout",
		},
		{
			name:     "first text timeout",
			err:      errUpstreamFirstTextTimeout,
			wantCode: "first_text_timeout",
		},
		{
			name:     "stream closed before completion",
			err:      errUpstreamStreamIncomplete,
			wantCode: "stream_incomplete",
		},
		{
			// Every candidate skipped for capacity (account concurrency cap or
			// no eligible account) is backpressure, not a broken upstream.
			name:     "no upstream capacity",
			err:      errNoUpstreamCapacity,
			wantCode: "no_capacity",
		},
		{
			name:     "deadline exceeded",
			err:      context.DeadlineExceeded,
			wantCode: "upstream_timeout",
		},
		{
			name:        "transport/DNS failure does not leak hostname",
			err:         &net_dnsError{},
			status:      0,
			wantCode:    "upstream_unreachable",
			wantMsgLeak: "upstream.internal.example",
		},
		{
			name:     "429 rate limited",
			status:   http.StatusTooManyRequests,
			wantCode: "rate_limited",
		},
		{
			name:     "5xx unavailable",
			status:   http.StatusServiceUnavailable,
			wantCode: "upstream_unavailable",
		},
		{
			name:     "actionable 403 image generation surfaced over status bucket",
			status:   http.StatusForbidden,
			body:     `{"error":{"message":"Image generation is not enabled for this group","type":"permission_error"}}`,
			wantCode: "feature_not_enabled",
		},
		{
			name:     "masked channel-level 4xx",
			status:   http.StatusUnauthorized,
			body:     `{"error":{"message":"Unauthorized","type":"permission_error"}}`,
			wantCode: "upstream_rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := failureReason(tc.err, tc.status, tc.body)
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q (msg=%q)", code, tc.wantCode, msg)
			}
			if msg == "" {
				t.Fatalf("message is empty")
			}
			if tc.wantMsgLeak != "" && strings.Contains(msg, tc.wantMsgLeak) {
				t.Fatalf("message %q leaks %q", msg, tc.wantMsgLeak)
			}
		})
	}
}

// net_dnsError stands in for a Go transport error like
// `dial tcp: lookup upstream.internal.example: i/o timeout` — its Error() text
// embeds the internal hostname that must never reach the client.
type net_dnsError struct{}

func (e *net_dnsError) Error() string {
	return `Post "https://upstream.internal.example/v1/messages": dial tcp: lookup upstream.internal.example: i/o timeout`
}
