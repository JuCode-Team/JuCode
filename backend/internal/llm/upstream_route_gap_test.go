package llm

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jucode/jucode/internal/biz"
)

// Bodies below are verbatim upstream error bodies.

func TestRouteGapIsUpstreamConfigNotUserRequest(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"relay 405", http.StatusMethodNotAllowed, `{"error":{"message":"Method Not Allowed","type":"api_error"}}`},
		{"relay 405 relayed", http.StatusMethodNotAllowed, `{"error":{"message":"Upstream error: 405","type":"api_error"}}`},
		{"no upstream provider", http.StatusBadRequest, `{"error":{"message":"no upstream provider could serve this request","type":"invalid_request_error"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := classifyUpstreamResponse(tc.status, []byte(tc.body))
			if cls != classUpstreamConfig {
				t.Fatalf("class = %q, want %q", cls, classUpstreamConfig)
			}
			if got := deriveErrorClass("", tc.status, tc.body); got != classUpstreamConfig {
				t.Fatalf("deriveErrorClass = %q, want %q", got, classUpstreamConfig)
			}
			// No failover/cooldown and no provider-health sample: one gap must
			// not take the whole channel out.
			if cls.Retryable() || cls.CountsForCooldown() {
				t.Fatalf("upstream_config must not be retryable or cool the channel down")
			}
			if count, _ := cls.HealthOutcome(); count {
				t.Fatalf("upstream_config must not be a provider-health sample")
			}
			if _, _, ok := actionableUpstreamError([]byte(tc.body)); ok {
				t.Fatalf("a route gap must not be answered as an invalid request")
			}
			st, code, msg := upstreamRejectedReply(tc.status, []byte(tc.body))
			if st != http.StatusBadGateway || code != "upstream_route_unsupported" || msg != biz.UpstreamRouteUnsupportedText {
				t.Fatalf("reply = (%d, %q, %q)", st, code, msg)
			}
			if c, _ := failureReason(nil, tc.status, tc.body); c != "upstream_route_unsupported" {
				t.Fatalf("failureReason code = %q", c)
			}
		})
	}
	// A genuine request-level 400 keeps its class and its status.
	if cls := classifyUpstreamResponse(http.StatusBadRequest, []byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`)); cls != classUserRequest {
		t.Fatalf("plain 400 class = %q, want user_request", cls)
	}
	if st, code, _ := upstreamRejectedReply(http.StatusUnauthorized, []byte(`{"error":{"message":"Unauthorized"}}`)); st != http.StatusUnauthorized || code != "upstream_rejected" {
		t.Fatalf("masked 401 reply = (%d, %q)", st, code)
	}
}

func TestActionableUpstreamErrorGivesSpecificHints(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
		wantIn   string
		leak     string
	}{
		{
			name:     "function tools with reasoning_effort",
			body:     `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6-luna in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error","param":null,"code":null}}`,
			wantCode: "tools_with_reasoning_unsupported",
			wantIn:   "/v1/responses",
		},
		{
			name:     "function_call_output without call_id",
			body:     `{"error":{"message":"function_call_output requires call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2","type":"invalid_request_error"}}`,
			wantCode: "tool_output_missing_call_id",
			wantIn:   "call_id",
		},
		{
			name:     "relay session lock relayed verbatim",
			body:     `{"error":{"message":"检测到缓存用量异常：本会话连续多次大请求的 input / cache_read / cache_creation 比例几乎不变，且缓存写入随上下文线性增长，已锁定本会话以避免异常计费。请新建会话后重试。若你确认风险并要继续当前会话，请在下一条消息中明确写「继续会话」。","type":"cache_anomaly"},"type":"error"}`,
			wantCode: "session_locked",
			wantIn:   "继续会话",
		},
		{
			name:     "model must think",
			body:     `{"error":{"details":[{"code":"model_must_think","param":"thinking","model":"claude-opus-5-5","official":"thinking.type: This model requires thinking to be enabled","reason":"模型 claude-opus-5-5 官方拒绝 thinking=disabled。"}],"message":"thinking.type: This model requires thinking to be enabled","param":"thinking","type":"invalid_request_error"},"type":"error"}`,
			wantCode: "thinking_required",
			wantIn:   "thinking",
		},
		{
			name:     "anthropic-beta value",
			body:     "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Unexpected value(s) `advisor-tool-2026-03-01` for the `anthropic-beta` header. Please consult our documentation at docs.anthropic.com or try again without the header.\"}}",
			wantCode: "unsupported_beta_header",
			wantIn:   "advisor-tool-2026-03-01",
		},
		{
			name:     "413 without detail",
			body:     `{"error":{"message":"Upstream error: 413","type":"api_error"}}`,
			wantCode: "request_body_too_large",
		},
		{
			name:     "422 invalid request body",
			body:     `{"error":{"message":"422: Invalid request body.","type":"api_error"}}`,
			wantCode: "endpoint_or_request_format",
		},
		{
			name:     "channel detail stays masked",
			body:     `{"error":{"message":"The 'gpt-image-2' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"}}`,
			wantCode: "unsupported_request",
			leak:     "ChatGPT",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, code, ok := actionableUpstreamError([]byte(tc.body))
			if !ok || code != tc.wantCode {
				t.Fatalf("ok=%v code=%q msg=%q, want %q", ok, code, msg, tc.wantCode)
			}
			if tc.wantIn != "" && !strings.Contains(msg, tc.wantIn) {
				t.Fatalf("message %q does not mention %q", msg, tc.wantIn)
			}
			if tc.leak != "" && strings.Contains(msg, tc.leak) {
				t.Fatalf("message %q leaks %q", msg, tc.leak)
			}
		})
	}
}
