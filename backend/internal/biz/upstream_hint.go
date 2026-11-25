package biz

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Upstream error hints: the user-facing diagnosis for request-level upstream
// rejections. Both the gateway's error reply (llm.actionableUpstreamError) and
// the console's per-request reason (ResolveUsageReason) read from here, so a
// failed call and its row in the user's log explain the same thing.
//
// The upstream's message is never relayed as-is (659ec28: raw upstream text can
// name the channel, pool or account). Instead, a recognized failure gets fixed
// Chinese copy that says what to change, and the only upstream-derived text
// ever shown is a parameter path that passes safeParamRe — a structural token
// like `input[91].arguments`, which describes the caller's own request.

// sessionLockMarker identifies an upstream relay's cache-anomaly session lock.
// That message is user-facing copy (it tells the user how to unlock the
// session), so it is the one upstream text relayed verbatim.
const sessionLockMarker = "检测到缓存用量异常"

// UpstreamRouteUnsupportedText is shown when an upstream relay could not
// route the request at all — HTTP 405 or "no upstream provider could serve".
// That is a gap on our side, not a problem with the request.
const UpstreamRouteUnsupportedText = "上游通道暂时无法处理该请求，并非请求本身有误。请稍后重试；如持续出现，请把 request_id 发给客服"

var (
	safeParamRe = regexp.MustCompile(`^[A-Za-z0-9_.\[\]\-]{1,64}$`)
	// quotedParamRe pulls the parameter path out of OpenAI-style shape errors:
	// "Missing required parameter: 'input[91].arguments'",
	// "Invalid 'input[261].id': ...", "Unknown parameter: 'foo'".
	quotedParamRe = regexp.MustCompile(`(?i)(?:missing required parameter|unknown parameter|unexpected parameter|invalid)\s*:?\s*'([^']{1,64})'`)
	// betaValueRe pulls the rejected anthropic-beta token out of
	// "Unexpected value(s) `advisor-tool-2026-03-01` for the `anthropic-beta` header".
	betaValueRe = regexp.MustCompile("unexpected value\\(s\\) `([^`]{1,64})` for the `anthropic-beta` header")
	// imageCapRe pulls N out of "Exceeded maximum number of images (50) allowed".
	imageCapRe = regexp.MustCompile(`maximum number of images \((\d{1,4})\)`)
)

type upstreamErrorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Param   any    `json:"param"`
		Message string `json:"message"`
	} `json:"error"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func parseUpstreamError(raw string) (msg, param string) {
	var env upstreamErrorEnvelope
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &env) != nil {
		return "", ""
	}
	msg = env.Error.Message
	if msg == "" {
		msg = env.Message
	}
	if p, ok := env.Error.Param.(string); ok {
		param = p
	}
	return msg, param
}

// UpstreamErrorParam returns the request parameter an upstream shape error
// points at, or "" when there is none or it is not a plain structural path.
func UpstreamErrorParam(raw string) string {
	msg, param := parseUpstreamError(raw)
	if param != "" && safeParamRe.MatchString(param) {
		return param
	}
	if m := quotedParamRe.FindStringSubmatch(msg); len(m) == 2 && safeParamRe.MatchString(m[1]) {
		return m[1]
	}
	return ""
}

// WithUpstreamParam appends the offending parameter to a fixed label so the
// user knows where to look. The label is returned unchanged when no safe
// parameter path can be extracted.
func WithUpstreamParam(label, raw string) string {
	if p := UpstreamErrorParam(raw); p != "" {
		return label + "（相关参数：" + p + "）"
	}
	return label
}

// UpstreamErrorHint maps the request-level upstream rejections seen in
// production onto a stable code and fixed Chinese copy stating the cause and
// the fix. ok=false means "no specific hint"; callers fall back to their
// generic label (optionally with WithUpstreamParam).
func UpstreamErrorHint(raw string) (code, text string, ok bool) {
	if raw == "" {
		return "", "", false
	}
	msg, _ := parseUpstreamError(raw)
	if strings.Contains(msg, sessionLockMarker) {
		return "session_locked", msg, true
	}
	hay := strings.ToLower(msg)
	if hay == "" {
		hay = strings.ToLower(raw)
	}
	switch {
	case strings.Contains(hay, "function tools with reasoning_effort are not supported"):
		return "tools_with_reasoning_unsupported",
			"该模型的 /v1/chat/completions 接口不支持在设置 reasoning_effort 时使用函数工具。请改用 /v1/responses 接口，或把 reasoning_effort 设为 none 后重试", true
	case strings.Contains(hay, "function_call_output requires call_id"):
		return "tool_output_missing_call_id",
			"工具结果（function_call_output）缺少 call_id。通过 HTTP 调用时请为每条工具结果带上对应的 call_id；previous_response_id 续接仅在 Responses WebSocket 模式下可用", true
	case strings.Contains(hay, "model_must_think") || strings.Contains(hay, "requires thinking to be enabled"):
		return "thinking_required", "该模型必须开启 thinking，请在请求中启用 thinking 后重试", true
	case strings.Contains(hay, "items are not persisted when `store` is set to false"):
		return "response_item_not_found",
			"引用的历史响应条目不存在：store 为 false 时上游不保存条目。请在请求中发送完整上下文，或新开会话后重试", true
	case strings.Contains(hay, "invalid schema for response_format"):
		return "invalid_response_schema",
			"response_format 中的 JSON Schema 不符合上游要求，请检查 required、additionalProperties 等字段后重试", true
	case strings.Contains(hay, "for the `anthropic-beta` header"):
		if m := betaValueRe.FindStringSubmatch(hay); len(m) == 2 && safeParamRe.MatchString(m[1]) {
			return "unsupported_beta_header", "请求头 anthropic-beta 含有当前不支持的值 " + m[1] + "，请去掉后重试", true
		}
		return "unsupported_beta_header", "请求头 anthropic-beta 含有当前不支持的值，请去掉后重试", true
	case strings.Contains(hay, "maximum number of images"):
		if m := imageCapRe.FindStringSubmatch(hay); len(m) == 2 {
			return "too_many_images", "请求中的图片超过上游单次上限（" + m[1] + " 张），请压缩上下文（如 /compact）或新开会话后重试", true
		}
		return "too_many_images", "请求中的图片超过上游单次上限，请压缩上下文（如 /compact）或新开会话后重试", true
	}
	return "", "", false
}

// IsUpstreamRouteGap reports whether an upstream rejection means the upstream
// relay could not route the request (it answers 405 or "no upstream provider
// could serve this request"), as opposed to the request itself being wrong.
func IsUpstreamRouteGap(status int, raw string) bool {
	if status == 405 {
		return true
	}
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "no upstream provider could serve") ||
		strings.Contains(lower, "upstream error: 405")
}
