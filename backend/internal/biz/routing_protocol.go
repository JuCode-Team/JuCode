package biz

import (
	"fmt"
	"strings"

	"github.com/jucode/jucode/internal/data/ent"
)

// RequestProtocol is the wire protocol a *client* request came in on. The
// account an attempt uses is chosen to match it (or to convert from it).
type RequestProtocol string

const (
	RequestProtocolAny        RequestProtocol = ""
	RequestProtocolOpenAI     RequestProtocol = "openai"     // OpenAI family, match the client
	RequestProtocolCompatible RequestProtocol = "compatible" // Chat Completions only
	RequestProtocolResponses  RequestProtocol = "responses"  // Responses API only
	RequestProtocolAnthropic  RequestProtocol = "anthropic"  // Anthropic Messages
	RequestProtocolGemini     RequestProtocol = "gemini"
)

// ParseRequestProtocol accepts the client-facing protocol vocabulary.
func ParseRequestProtocol(raw string) (RequestProtocol, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return RequestProtocolAny, true
	case string(RequestProtocolOpenAI):
		return RequestProtocolOpenAI, true
	case string(RequestProtocolCompatible):
		return RequestProtocolCompatible, true
	case string(RequestProtocolResponses):
		return RequestProtocolResponses, true
	case string(RequestProtocolAnthropic):
		return RequestProtocolAnthropic, true
	case string(RequestProtocolGemini):
		return RequestProtocolGemini, true
	}
	return "", false
}

// NormalizeRequestProtocol folds a set of candidate protocols into the one that
// describes the request. Anthropic wins over the OpenAI family when both are
// plausible: a client that speaks Messages gets native handling wherever it
// can, and conversion only as a fallback.
func NormalizeRequestProtocol(protocols ...RequestProtocol) RequestProtocol {
	for _, p := range protocols {
		if p == RequestProtocolAnthropic {
			return RequestProtocolAnthropic
		}
	}
	for _, p := range protocols {
		switch p {
		case RequestProtocolResponses:
			return RequestProtocolResponses
		case RequestProtocolCompatible:
			return RequestProtocolCompatible
		case RequestProtocolOpenAI:
			return RequestProtocolOpenAI
		case RequestProtocolGemini:
			return RequestProtocolGemini
		}
	}
	return RequestProtocolAny
}

// AccountSupportsRequestProtocol reports whether an account can serve this
// client protocol, either natively or by conversion. conversionEnabled is the
// site switch: with conversion off, only native accounts qualify.
func AccountSupportsRequestProtocol(acct *ent.UpstreamAccount, protocol RequestProtocol, conversionEnabled bool) bool {
	if protocol == RequestProtocolAny {
		return true
	}
	return AccountServesRequest(acct, protocol, conversionEnabled)
}

// ErrUnknownProtocol is returned when an admin supplies a protocol the gateway
// does not implement.
var ErrUnknownProtocol = fmt.Errorf("want %s, %s, %s or %s",
	NativeProtocolChat, NativeProtocolResponses, NativeProtocolMessages, RequestProtocolAnthropic)
