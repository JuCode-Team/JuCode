package biz

import "testing"

func TestResolveUsageReason(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		class      string
		httpStatus int
		raw        string
		wantCode   string
	}{
		{"ok row has no reason", "ok", "ok", 200, "", ""},
		{"empty status has no reason", "", "", 0, "", ""},
		{"cyber block", "upstream_error", "cyber_block", 403, "harmful content", "content_safety"},
		{"output policy under internal_error", "internal_error", "user_policy", 422, "output_policy_block:abc", "output_policy"},
		{"rate limit", "upstream_error", "rate_limit", 429, "", "rate_limited"},
		{"timeout", "upstream_error", "timeout", 504, "", "timeout"},
		{"pool exhausted", "upstream_error", "pool_exhausted", 503, "No available accounts", "capacity_full"},
		{"auth 402 quota", "upstream_error", "auth", 402, "insufficient_quota", "service_busy"},
		{"auth generic", "upstream_error", "auth", 401, "invalid api key", "upstream_unavailable"},
		{"provider overloaded", "upstream_error", "provider_failure", 529, "Overloaded", "upstream_overloaded"},
		{"provider failure 503", "upstream_error", "provider_failure", 503, "bad gateway", "upstream_unavailable"},
		{"provider failure 502", "upstream_error", "provider_failure", 502, "bad gateway", "upstream_bad_gateway"},
		{"response.failed mid-generation", "upstream_error", "provider_failure", 200, "upstream response.failed: upstream_error", "generation_failed"},
		{"stream closed incomplete", "upstream_error", "provider_failure", 200, "stream closed before response.completed", "stream_interrupted"},
		{"client canceled", "upstream_error", "client_canceled", 499, "client_canceled", "client_canceled"},
		{"client canceled 499 no class", "upstream_error", "", 499, "", "client_canceled"},
		{"unknown class", "upstream_error", "unknown", 200, "something odd", "unknown"},
		{"first token timeout", "upstream_error", "timeout", 504, "first_text_timeout", "first_token_timeout"},
		{"idle stream timeout", "upstream_error", "timeout", 200, "stream_idle_timeout", "stream_stalled"},
		{"total stream timeout", "upstream_error", "timeout", 200, "stream_total_timeout", "request_timeout"},
		{"context length", "upstream_error", "user_request", 400, "This model's maximum context length is 200000 tokens", "context_length_exceeded"},
		{"oversized request body", "upstream_error", "user_request", 400, "The upstream request body is too large. Reduce image count, attachment size, or conversation history.", "request_body_too_large"},
		{"oversized body, relay pre-flight wording", "upstream_error", "user_request", 400, "Request is too large for claude-sonnet-5 after safe local compression (1353063 body bytes; text limit 950000). The request was not sent upstream.", "request_body_too_large"},
		{"model not found", "upstream_error", "user_request", 404, "model_not_found", "model_not_found"},
		{"invalid request generic", "upstream_error", "user_request", 400, "invalid 'temperature'", "invalid_request"},
		{"quota fallback by status", "quota_exceeded", "", 429, "", "account_quota_exceeded"},
		{"internal fallback (charge failed, class=ok)", "internal_error", "ok", 200, "", "internal_error"},
		{"upstream fallback no class", "upstream_error", "", 502, "", "upstream_bad_gateway"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveUsageReason(tc.status, tc.class, tc.httpStatus, tc.raw, "")
			if got.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (text=%q)", got.Code, tc.wantCode, got.Text)
			}
			if tc.wantCode != "" && got.Text == "" {
				t.Fatalf("non-empty code %q but empty text", got.Code)
			}
		})
	}
}

func TestResolveUsageReasonTextsAreSpecific(t *testing.T) {
	generic := "上游服务暂时不可用，请稍后重试"
	cases := []struct {
		name       string
		status     string
		class      string
		httpStatus int
		raw        string
		errMsg     string
		wantCode   string
		wantText   string
	}{
		{
			name:   "client cancel is not upstream down",
			status: "upstream_error", class: "client_canceled", httpStatus: 499,
			errMsg:   "client_canceled",
			wantCode: "client_canceled", wantText: "客户端已取消请求",
		},
		{
			name:   "response.failed is not upstream down",
			status: "upstream_error", class: "provider_failure", httpStatus: 200,
			errMsg:   "upstream response.failed: upstream_error",
			wantCode: "generation_failed", wantText: "上游在生成过程中失败，请重试",
		},
		{
			name:   "truncated stream is not upstream down",
			status: "upstream_error", class: "provider_failure", httpStatus: 200,
			errMsg:   "stream closed before response.completed",
			wantCode: "stream_interrupted", wantText: "上游响应中断，输出未完成",
		},
		{
			name:   "upstream quota is not named as a channel",
			status: "upstream_error", class: "auth", httpStatus: 402,
			raw:      "insufficient_quota",
			wantCode: "service_busy", wantText: "服务暂时繁忙，请稍后重试",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveUsageReason(tc.status, tc.class, tc.httpStatus, tc.raw, tc.errMsg)
			if got.Code != tc.wantCode || got.Text != tc.wantText {
				t.Fatalf("got %+v, want code=%q text=%q", got, tc.wantCode, tc.wantText)
			}
			if got.Text == generic {
				t.Fatalf("collapsed onto generic unavailable text")
			}
		})
	}
}
