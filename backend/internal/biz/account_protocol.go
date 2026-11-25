package biz

import (
	"fmt"
	"strings"

	"github.com/jucode/jucode/internal/data/ent"
)

// Native wire protocols an upstream endpoint speaks itself. A client request
// whose protocol is in an account's native set is forwarded unchanged; a
// request on any other protocol needs conversion and therefore both
// UpstreamAccount.allow_conversion and the site's format_conversion_enabled.
const (
	NativeProtocolChat      = "chat_completions"
	NativeProtocolResponses = "responses"
	NativeProtocolMessages  = "messages"
	// NativeProtocolWeb marks a web-tool vendor account (search / page fetch,
	// see llm/web_tools.go). It speaks no chat dialect, so chat requests never
	// route to it.
	NativeProtocolWeb = "web"
)

// ParseNativeProtocol canonicalizes one declaration value. Admin-supplied
// aliases are accepted; anything else is rejected.
func ParseNativeProtocol(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case NativeProtocolChat, "chat", "compatible", "chat-completions":
		return NativeProtocolChat, true
	case NativeProtocolResponses, "response":
		return NativeProtocolResponses, true
	case NativeProtocolMessages, "anthropic", "messages-api":
		return NativeProtocolMessages, true
	case NativeProtocolWeb:
		return NativeProtocolWeb, true
	default:
		return "", false
	}
}

// NormalizeNativeProtocols validates and de-duplicates an admin-supplied
// declaration, preserving the caller's order.
func NormalizeNativeProtocols(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, entry := range raw {
		value := strings.TrimSpace(entry)
		if value == "" {
			continue
		}
		proto, ok := ParseNativeProtocol(value)
		if !ok {
			return nil, fmt.Errorf("unknown native protocol %q: want chat_completions, responses, messages or web", entry)
		}
		if _, dup := seen[proto]; dup {
			continue
		}
		seen[proto] = struct{}{}
		out = append(out, proto)
	}
	return out, nil
}

// NativeProtocolForRequest maps a client-facing request protocol onto the
// native protocol tag it requires. The family protocol ("" / "openai") has no
// single native tag — callers on that path must pass the concrete dialect.
func NativeProtocolForRequest(p RequestProtocol) (string, bool) {
	switch p {
	case RequestProtocolCompatible:
		return NativeProtocolChat, true
	case RequestProtocolResponses:
		return NativeProtocolResponses, true
	case RequestProtocolAnthropic:
		return NativeProtocolMessages, true
	default:
		return "", false
	}
}

// AccountProtocols returns an account's declared native protocols. Rows that
// predate the declaration (or leave it empty) default to chat_completions,
// matching the old openai_compatible default.
func AccountProtocols(a *ent.UpstreamAccount) []string {
	if a == nil || len(a.NativeProtocols) == 0 {
		return []string{NativeProtocolChat}
	}
	out := make([]string, 0, len(a.NativeProtocols))
	for _, raw := range a.NativeProtocols {
		if proto, ok := ParseNativeProtocol(raw); ok {
			out = append(out, proto)
		}
	}
	if len(out) == 0 {
		return []string{NativeProtocolChat}
	}
	return out
}

// AccountHasNativeProtocol reports whether the account speaks proto itself.
func AccountHasNativeProtocol(a *ent.UpstreamAccount, proto string) bool {
	for _, p := range AccountProtocols(a) {
		if p == proto {
			return true
		}
	}
	return false
}

// AccountServesRequest reports whether this account may serve a client request
// on the given protocol: natively, or (when both the account and the site
// allow conversion) through protocol conversion.
//
// RequestProtocolOpenAI is the OpenAI *family* ("follow the client"), so it is
// satisfied by an account that speaks either OpenAI dialect — and NOT by a
// Messages-only account, which would otherwise be handed a Chat Completions
// body. Which of the two dialects the attempt uses is decided per attempt from
// the client's concrete dialect (see the gateway's dialect resolution).
func AccountServesRequest(a *ent.UpstreamAccount, client RequestProtocol, conversionEnabled bool) bool {
	if a == nil {
		return false
	}
	needed, ok := NativeProtocolForRequest(client)
	if !ok {
		if client != RequestProtocolAny && client != RequestProtocolOpenAI {
			// A protocol this gateway does not implement at all.
			return false
		}
		if AccountHasOpenAIFamily(a) {
			return true
		}
		return conversionEnabled && a.AllowConversion
	}
	if AccountHasNativeProtocol(a, needed) {
		return true
	}
	return conversionEnabled && a.AllowConversion
}

// AccountHasOpenAIFamily reports whether the account speaks Chat Completions or
// the Responses API.
func AccountHasOpenAIFamily(a *ent.UpstreamAccount) bool {
	return AccountHasNativeProtocol(a, NativeProtocolChat) ||
		AccountHasNativeProtocol(a, NativeProtocolResponses)
}

// AccountUpstreamProtocol picks the protocol the account is spoken to in for
// this client request: the native protocol when it matches, otherwise the
// account's conversion target (default_protocol, else the first native one).
// ok=false means the pair cannot be served at all.
//
// The OpenAI family is settled against the client's own dialect: an account
// that speaks Chat Completions still speaks Chat Completions when the client
// sent a Chat Completions request, and one that speaks Responses speaks
// Responses. Callers that already know the concrete client dialect should pass
// it (RequestProtocolCompatible / RequestProtocolResponses) rather than the
// family value.
func AccountUpstreamProtocol(a *ent.UpstreamAccount, client RequestProtocol, conversionEnabled bool) (string, bool) {
	if a == nil {
		return "", false
	}
	native := AccountProtocols(a)
	if client == RequestProtocolOpenAI {
		// Family request: prefer the OpenAI dialect the client actually used,
		// which the caller narrows. With only one family dialect declared there
		// is nothing to choose.
		for _, proto := range []string{NativeProtocolChat, NativeProtocolResponses} {
			if AccountHasNativeProtocol(a, proto) {
				return proto, true
			}
		}
		if !conversionEnabled || !a.AllowConversion {
			return "", false
		}
		return conversionTarget(a, native), true
	}
	if needed, ok := NativeProtocolForRequest(client); ok {
		if AccountHasNativeProtocol(a, needed) {
			return needed, true
		}
		if !conversionEnabled || !a.AllowConversion {
			return "", false
		}
		return conversionTarget(a, native), true
	}
	if client != RequestProtocolAny {
		return "", false
	}
	// "Any" speaks whatever the account speaks best.
	return native[0], true
}

// AccountUpstreamProtocolForModel is AccountUpstreamProtocol with an optional
// per-model pin (upstream_account_models.upstream_protocol). A non-empty pin
// fixes the protocol the upstream is spoken to in for this model: it must be
// one of the account's native protocols, and a client request on any other
// protocol is served through conversion (gated the same way as before).
// ok=false means the pair cannot serve this request — including a pin that
// names a protocol the endpoint does not speak, which is misconfiguration.
func AccountUpstreamProtocolForModel(a *ent.UpstreamAccount, pin string, client RequestProtocol, conversionEnabled bool) (string, bool) {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return AccountUpstreamProtocol(a, client, conversionEnabled)
	}
	pinned, ok := ParseNativeProtocol(pin)
	if !ok || !AccountHasNativeProtocol(a, pinned) {
		return "", false
	}
	switch client {
	case RequestProtocolAny:
		return pinned, true
	case RequestProtocolOpenAI:
		// Family request: an OpenAI-dialect pin serves natively (the concrete
		// client dialect is settled per attempt); a Messages pin needs
		// conversion.
		if pinned == NativeProtocolChat || pinned == NativeProtocolResponses {
			return pinned, true
		}
		if conversionEnabled && a.AllowConversion {
			return pinned, true
		}
		return "", false
	default:
		if needed, ok := NativeProtocolForRequest(client); ok {
			if needed == pinned {
				return pinned, true
			}
			if conversionEnabled && a.AllowConversion {
				return pinned, true
			}
		}
		return "", false
	}
}

// AccountServesModelRequest is the eligibility half of
// AccountUpstreamProtocolForModel: whether the account may serve this client
// protocol for a model carrying the given pin.
func AccountServesModelRequest(a *ent.UpstreamAccount, pin string, client RequestProtocol, conversionEnabled bool) bool {
	_, ok := AccountUpstreamProtocolForModel(a, pin, client, conversionEnabled)
	return ok
}

// conversionTarget is the dialect an account is converted to when the client's
// protocol is not native: the declared default when it is one of its own, else
// its first native protocol.
func conversionTarget(a *ent.UpstreamAccount, native []string) string {
	if preferred := strings.TrimSpace(a.DefaultProtocol); preferred != "" {
		if proto, ok := ParseNativeProtocol(preferred); ok && AccountHasNativeProtocol(a, proto) {
			return proto
		}
	}
	return native[0]
}
