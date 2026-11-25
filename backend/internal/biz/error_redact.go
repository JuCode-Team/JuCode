package biz

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Stored error text. Rows keep when and why a request failed, never what was
// in it: an upstream error body can quote the caller's prompt back ("Invalid
// 'input[3].content': '...'"), so no upstream-written text reaches Postgres
// verbatim. The gateway still reads the full body in memory to classify the
// failure and answer the client; only the persisted copy is reduced here.

// upstreamErrorMarkers is the fixed vocabulary the reason resolvers
// (ResolveUsageReason, UpstreamErrorHint, IsUpstreamRouteGap) match on.
// SanitizeUpstreamError keeps which of these occurred and nothing else of
// the message, so a stored row resolves to the same reason the raw body did.
var upstreamErrorMarkers = []string{
	sessionLockMarker,
	"first_text_timeout", "stream_idle_timeout", "stream_total_timeout",
	"insufficient_quota", "insufficient quota", "exceeded your current quota",
	"overloaded", "server_overloaded",
	"response.failed", "response_failed",
	"stream closed", "stream_incomplete", "stream_interrupted", "stream_read_error",
	"request_body_too_large", "body is too large", "body too large", "request entity too large",
	"payload too large", "request is too large", "after safe local compression",
	"context length", "context_length", "maximum context", "too long", "reduce the length", "max_tokens",
	"model_not_found", "does not exist", "no such model", "model not found",
	"function tools with reasoning_effort are not supported",
	"function_call_output requires call_id",
	"model_must_think", "requires thinking to be enabled",
	"items are not persisted when `store` is set to false",
	"invalid schema for response_format",
	"for the `anthropic-beta` header",
	"maximum number of images",
	"no upstream provider could serve", "upstream error: 405",
}

// maxStoredErrorBytes caps every stored error string.
const maxStoredErrorBytes = 512

var upstreamStatusPrefixRe = regexp.MustCompile(`upstream (\d{3}): `)

// SanitizeUpstreamError reduces an upstream error body (or an error string
// that embeds one) to what the usage reason resolvers read: the error type and
// code when they are plain tokens, the offending parameter when it is a
// structural path such as `input[3].content`, and which known failure phrases
// occurred. The result is a small JSON envelope in the shape upstreams use, so
// ResolveUsageReason reads it like a raw body. Returns "" when nothing in raw
// is known to be safe.
func SanitizeUpstreamError(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var env upstreamErrorEnvelope
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		_ = json.Unmarshal([]byte(raw[i:]), &env)
	}
	out := map[string]any{}
	errType := env.Error.Type
	if errType == "" {
		errType = env.Type
	}
	if safeParamRe.MatchString(errType) {
		out["type"] = errType
	}
	if code := fmt.Sprint(env.Error.Code); env.Error.Code != nil && safeParamRe.MatchString(code) {
		out["code"] = code
	}
	if p := UpstreamErrorParam(raw); p != "" {
		out["param"] = p
	}
	lower := strings.ToLower(raw)
	var markers []string
	for _, m := range upstreamErrorMarkers {
		if strings.Contains(lower, m) {
			markers = append(markers, m)
		}
	}
	if len(markers) > 0 {
		out["message"] = strings.Join(markers, "; ")
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(map[string]any{"error": out})
	if err != nil {
		return ""
	}
	return truncateUTF8(string(b), maxStoredErrorBytes)
}

// SanitizeErrorMessage prepares a short error message for storage. Messages
// the gateway writes itself ("billing reserve: ...", "stream_idle_timeout",
// "dial tcp ...") are kept; an upstream body, or the part of a message after
// an "upstream NNN: " prefix, is reduced by SanitizeUpstreamError.
func SanitizeErrorMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	switch msg[0] {
	case '{', '[', '<':
		return SanitizeUpstreamError(msg)
	}
	if loc := upstreamStatusPrefixRe.FindStringSubmatchIndex(msg); loc != nil {
		out := msg[:loc[1]-2] // keep "...upstream NNN", drop ": <body>"
		if s := SanitizeUpstreamError(msg[loc[1]:]); s != "" {
			out += " " + s
		}
		return truncateUTF8(out, maxStoredErrorBytes)
	}
	return truncateUTF8(msg, 256)
}

// internalErrorTokenRe matches the fixed tokens the gateway records as an
// attempt's error ("client_canceled", "output_policy_block:<slug>").
var internalErrorTokenRe = regexp.MustCompile(`^[a-z_]+(:[A-Za-z0-9_.\-]{1,64})?$`)

// SanitizeUpstreamReply prepares the error of an attempt that got an HTTP
// reply. A body the upstream wrote is reduced by SanitizeUpstreamError; the
// gateway's own tokens and messages pass through SanitizeErrorMessage.
func SanitizeUpstreamReply(httpStatus int, msg string) string {
	msg = strings.TrimSpace(msg)
	if httpStatus < 400 || internalErrorTokenRe.MatchString(msg) || upstreamStatusPrefixRe.MatchString(msg) {
		return SanitizeErrorMessage(msg)
	}
	return SanitizeUpstreamError(msg)
}

// SanitizeTaskError keeps an async task's upstream failure reason only when
// it is a plain error code; free text is replaced by a fixed label.
func SanitizeTaskError(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" || safeParamRe.MatchString(msg) {
		return msg
	}
	return "upstream task failed"
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
