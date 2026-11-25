// convert.go: configurable conversion among the three client/upstream
// dialects the gateway speaks:
//
//	compatible — OpenAI Chat Completions (/v1/chat/completions)
//	openai     — OpenAI Responses        (/v1/responses)
//	anthropic  — Anthropic Messages      (/v1/messages)
//
// Converters are pure request/response/stream functions so tests (and the
// WebSocket HTTP-bridge) can call them directly. The gateway only invokes
// them when SiteSettings.FormatConversionEnabled is true; when the toggle
// is off, native passthrough and today's anthropic→chat fallback stay as
// they are.
package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

// Dialect is one of the three named generation wire formats.
type Dialect string

const (
	DialectCompatible Dialect = "compatible"
	DialectOpenAI     Dialect = "openai"
	DialectAnthropic  Dialect = "anthropic"
)

// PathForDialect is the upstream POST path for a dialect.
func PathForDialect(d Dialect) string {
	switch d {
	case DialectAnthropic:
		return "/v1/messages"
	case DialectOpenAI:
		return "/v1/responses"
	default:
		return "/v1/chat/completions"
	}
}

// AccountDialect picks the upstream dialect for one account serving a request
// whose client protocol is `client`. The account's protocol vocabulary (see
// biz/account_protocol.go) decides:
//
//	messages         → DialectAnthropic   (native, or the conversion target)
//	chat_completions → DialectCompatible
//	responses        → DialectOpenAI
//
// The target is the account's native protocol when the client spoke it, else
// the conversion target biz.AccountUpstreamProtocol resolves. ok=false means
// the account cannot serve this request at all (no native match and conversion
// is not allowed): the candidate must not be attempted.
func AccountDialect(acct *ent.UpstreamAccount, pin string, client biz.RequestProtocol, conversionEnabled bool) (Dialect, bool) {
	proto, ok := biz.AccountUpstreamProtocolForModel(acct, pin, client, conversionEnabled)
	if !ok {
		return "", false
	}
	switch proto {
	case biz.NativeProtocolMessages:
		return DialectAnthropic, true
	case biz.NativeProtocolResponses:
		return DialectOpenAI, true
	case biz.NativeProtocolChat:
		return DialectCompatible, true
	default:
		return "", false
	}
}

// RewriteForAccount applies the conversion toggle and returns the body, path,
// and dialect that should be forwarded upstream. handle() uses this.
//
// errAccountProtocolUnsupported means the account cannot serve this request's
// protocol (no native match, conversion not allowed): the caller skips the
// candidate instead of contacting it.
func RewriteForAccount(enabled bool, client Dialect, clientProtocol biz.RequestProtocol, acct *ent.UpstreamAccount, upstreamProtocol string, body []byte) (fwd []byte, path string, target Dialect, err error) {
	target, ok := AccountDialect(acct, upstreamProtocol, clientProtocol, enabled)
	if !ok {
		return nil, "", "", errAccountProtocolUnsupported
	}
	path = PathForDialect(target)
	if proto, ok := dialectProtocol(target); ok {
		// A custom provider may live outside the /v1/* conventions; the
		// account's declaration then replaces the dialect default. Accounts
		// that declare nothing keep exactly the old path.
		if override := biz.EndpointPathFor(acct, proto); override != "" {
			path = override
		}
	}
	fwd, err = ApplyRequestConversion(enabled, client, target, body)
	return fwd, path, target, err
}

// dialectProtocol maps the gateway dialect onto the account protocol vocabulary
// (the names accounts declare their overrides with).
func dialectProtocol(d Dialect) (string, bool) {
	switch d {
	case DialectAnthropic:
		return biz.NativeProtocolMessages, true
	case DialectOpenAI:
		return biz.NativeProtocolResponses, true
	case DialectCompatible:
		return biz.NativeProtocolChat, true
	default:
		return "", false
	}
}

// errAccountProtocolUnsupported is an internal routing verdict: no account
// endpoint can speak this request's protocol. It is never surfaced verbatim to
// a client (failureReason masks it) and names no account.
var errAccountProtocolUnsupported = errors.New("no upstream endpoint can serve this request protocol")

// ApplyRequestConversion is the single request-side entry used by the
// gateway. When enabled is false or the dialects match, the original body
// is returned unchanged (no force rewrite).
func ApplyRequestConversion(enabled bool, from, to Dialect, body []byte) ([]byte, error) {
	if !enabled || from == "" || to == "" || from == to {
		return body, nil
	}
	return ConvertRequest(from, to, body)
}

// ApplyResponseConversion is the response-side sibling of ApplyRequestConversion.
func ApplyResponseConversion(enabled bool, from, to Dialect, body []byte) ([]byte, error) {
	if !enabled || from == "" || to == "" || from == to {
		return body, nil
	}
	return ConvertResponse(from, to, body)
}

// ConvertRequest rewrites a generation request body from one dialect to another.
func ConvertRequest(from, to Dialect, body []byte) ([]byte, error) {
	if from == to {
		return body, nil
	}
	src, err := decodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("convert request: %w", err)
	}
	var dst map[string]any
	switch {
	case from == DialectCompatible && to == DialectAnthropic:
		dst = chatToAnthropicRequest(src)
	case from == DialectCompatible && to == DialectOpenAI:
		dst = chatToResponsesRequest(src)
	case from == DialectOpenAI && to == DialectCompatible:
		dst = responsesToChatRequest(src)
	case from == DialectOpenAI && to == DialectAnthropic:
		dst = chatToAnthropicRequest(responsesToChatRequest(src))
	case from == DialectAnthropic && to == DialectCompatible:
		dst = anthropicToChatRequest(src)
	case from == DialectAnthropic && to == DialectOpenAI:
		dst = chatToResponsesRequest(anthropicToChatRequest(src))
	default:
		return nil, fmt.Errorf("unsupported request conversion %s → %s", from, to)
	}
	out, err := json.Marshal(dst)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ConvertResponse rewrites a non-stream generation response from one dialect to another.
func ConvertResponse(from, to Dialect, body []byte) ([]byte, error) {
	if from == to {
		return body, nil
	}
	src, err := decodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("convert response: %w", err)
	}
	// Responses terminal frames nest the object under "response".
	if from == DialectOpenAI {
		if inner, ok := src["response"].(map[string]any); ok && src["object"] != "response" {
			if _, hasOutput := inner["output"]; hasOutput {
				src = inner
			}
		}
	}
	var dst map[string]any
	switch {
	case from == DialectCompatible && to == DialectAnthropic:
		dst = chatToAnthropicResponse(src)
	case from == DialectCompatible && to == DialectOpenAI:
		dst = chatToResponsesResponse(src)
	case from == DialectOpenAI && to == DialectCompatible:
		dst = responsesToChatResponse(src)
	case from == DialectOpenAI && to == DialectAnthropic:
		dst = chatToAnthropicResponse(responsesToChatResponse(src))
	case from == DialectAnthropic && to == DialectCompatible:
		dst = anthropicToChatResponse(src)
	case from == DialectAnthropic && to == DialectOpenAI:
		dst = chatToResponsesResponse(anthropicToChatResponse(src))
	default:
		return nil, fmt.Errorf("unsupported response conversion %s → %s", from, to)
	}
	out, err := json.Marshal(dst)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ConvertStream rewrites an SSE stream from one dialect to another.
// Identity when from == to (bytes copied through).
func ConvertStream(from, to Dialect, src io.Reader, dst io.Writer) error {
	if from == to {
		_, err := io.Copy(dst, src)
		return err
	}
	return convertSSE(from, to, src, dst)
}

func decodeObject(body []byte) (map[string]any, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m, nil
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func strVal(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case fmt.Stringer:
		return t.String()
	default:
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
}

func intVal(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		var n int
		_, _ = fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}

func firstPositiveInt(vals ...any) int {
	for _, v := range vals {
		if n := intVal(v); n > 0 {
			return n
		}
	}
	return 0
}

func boolVal(v any) bool {
	b, _ := v.(bool)
	return b
}

func cloneAny(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}
