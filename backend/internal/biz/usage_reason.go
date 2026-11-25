// usage_reason.go: turn the admin-only error classification of a usage_log
// row into a sanitized, user-facing reason. The gateway already stores a
// rich `error_class` (see internal/llm/error_class.go) plus the raw upstream
// body; users only ever saw the coarse `status` enum ("upstream_error").
// ResolveUsageReason maps that internal signal to a curated Chinese message
// so a failed request shows WHY it failed — without leaking upstream
// provider identity, account ids, keys or internal URLs (we never echo the
// raw body to users; only the texts below).
package biz

import "strings"

// UsageReason is the user-facing explanation for a non-ok usage_log row.
// Code is a stable machine token (for frontend i18n / filtering later);
// Text is the ready-to-show Chinese message. Both empty for successful rows.
type UsageReason struct {
	Code string `json:"reason_code,omitempty"`
	Text string `json:"reason,omitempty"`
}

// ResolveUsageReason derives the reason from a row's (status, errorClass,
// httpStatus, rawError/errMsg). errorClass is the primary signal because it
// is the gateway's structured outcome and is more precise than status — e.g.
// an output-policy block is stored with status=internal_error but
// errorClass=user_policy. status is only consulted as a fallback for rows
// that carry no class (precheck rejections, charge failures).
//
// The class strings mirror internal/llm/error_class.go; they are matched as
// plain strings here to avoid an import cycle (llm imports biz, not vice
// versa). Each class (and the common last-hop subtypes) gets its own code
// and copy — do not collapse them onto "上游服务暂时不可用".
func ResolveUsageReason(status, errorClass string, httpStatus int, rawError, errMsg string) UsageReason {
	if status == "" || status == "ok" {
		return UsageReason{}
	}
	body := strings.ToLower(rawError + " " + errMsg)

	switch errorClass {
	case "client_canceled":
		return UsageReason{"client_canceled", "客户端已取消请求"}
	case "cyber_block":
		return UsageReason{"content_safety", "内容被上游安全策略拦截，请调整输入后重试"}
	case "user_policy":
		return UsageReason{"output_policy", "输出内容触发审核策略，已被拦截"}
	case "rate_limit":
		return UsageReason{"rate_limited", "请求过于频繁，已被上游限流，请稍后重试"}
	case "timeout":
		return timeoutReason(body)
	case "pool_exhausted":
		return UsageReason{"capacity_full", "当前模型容量已满，请稍后重试"}
	case "auth":
		// Channel quota / key failures are operator facts. Users only see
		// a generic busy/unavailable line — "渠道额度" would reveal the
		// request was proxied onto someone else's account.
		if httpStatus == 402 || containsAny(body, "insufficient_quota", "insufficient quota", "exceeded your current quota") {
			return UsageReason{"service_busy", "服务暂时繁忙，请稍后重试"}
		}
		return UsageReason{"upstream_unavailable", "上游服务暂时不可用，请稍后重试"}
	case "provider_failure":
		return providerFailureReason(httpStatus, body)
	case "upstream_config":
		return UsageReason{"upstream_route_unsupported", UpstreamRouteUnsupportedText}
	case "user_request":
		raw := rawError
		if strings.TrimSpace(raw) == "" {
			raw = errMsg // older rows: only the truncated message was stored
		}
		return userRequestReason(body, raw)
	case "unknown":
		return UsageReason{"unknown", "请求失败，原因未知"}
	}

	// No (or unrecognized) class — fall back to HTTP status / coarse enum.
	if httpStatus == 499 {
		return UsageReason{"client_canceled", "客户端已取消请求"}
	}
	switch status {
	case "quota_exceeded":
		return UsageReason{"account_quota_exceeded", "账户额度不足，请充值或检查套餐额度"}
	case "internal_error":
		return UsageReason{"internal_error", "系统内部错误，请稍后重试或联系客服"}
	case "banned":
		return UsageReason{"banned", "请求被风控拦截"}
	case "unauthorized":
		return UsageReason{"unauthorized", "API Key 无效或已被禁用"}
	case "upstream_error":
		if httpStatus == 504 {
			return timeoutReason(body)
		}
		if httpStatus == 502 {
			return UsageReason{"upstream_bad_gateway", "上游网关错误，请稍后重试"}
		}
		return UsageReason{"upstream_unavailable", "上游服务暂时不可用，请稍后重试"}
	default:
		return UsageReason{"unknown", "请求失败，原因未知"}
	}
}

func timeoutReason(body string) UsageReason {
	switch {
	case containsAny(body, "first_text_timeout"):
		return UsageReason{"first_token_timeout", "上游迟迟未返回首个 token，已超时"}
	case containsAny(body, "stream_idle_timeout"):
		return UsageReason{"stream_stalled", "上游输出中途停顿超时"}
	case containsAny(body, "stream_total_timeout"):
		return UsageReason{"request_timeout", "请求总时长超时"}
	default:
		return UsageReason{"timeout", "上游响应超时，请稍后重试"}
	}
}

func providerFailureReason(httpStatus int, body string) UsageReason {
	if httpStatus == 529 || containsAny(body, "overloaded", "is currently overloaded", "server_overloaded") {
		return UsageReason{"upstream_overloaded", "上游模型服务过载，请稍后重试"}
	}
	if containsAny(body, "response.failed", "response_failed") {
		return UsageReason{"generation_failed", "上游在生成过程中失败，请重试"}
	}
	if containsAny(body, "stream closed", "stream_incomplete", "stream_interrupted") {
		return UsageReason{"stream_interrupted", "上游响应中断，输出未完成"}
	}
	switch httpStatus {
	case 502:
		return UsageReason{"upstream_bad_gateway", "上游网关错误，请稍后重试"}
	case 503:
		return UsageReason{"upstream_unavailable", "上游服务暂时不可用，请稍后重试"}
	default:
		return UsageReason{"upstream_error", "上游返回错误，请稍后重试"}
	}
}

// userRequestReason explains a request-level rejection. body is the
// lowercased haystack for keyword matching; raw keeps the original case for
// the shared hints, which parse the upstream JSON.
func userRequestReason(body, raw string) UsageReason {
	if code, text, ok := UpstreamErrorHint(strings.TrimSpace(raw)); ok {
		return UsageReason{code, text}
	}
	switch {
	case containsAny(body, "request_body_too_large", "body is too large", "body too large", "request entity too large",
		"payload too large", "request is too large", "after safe local compression"):
		return UsageReason{"request_body_too_large", "请求体过大，请减少对话历史、工具输出或图片/PDF 附件后重试"}
	case containsAny(body, "context length", "context_length", "maximum context", "too long", "reduce the length", "max_tokens"):
		return UsageReason{"context_length_exceeded", "输入内容超出模型上下文长度上限，请缩短上下文后重试"}
	case containsAny(body, "model_not_found", "does not exist", "no such model", "model not found"):
		return UsageReason{"model_not_found", "请求的模型不存在或当前不可用"}
	default:
		return UsageReason{"invalid_request", WithUpstreamParam("请求参数无效，请检查请求内容后重试", strings.TrimSpace(raw))}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
