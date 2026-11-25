package llm

import (
	"net/http"
	"strings"
)

// openaiClientSessionHeaderAllowlist is the subset of client headers that
// OpenAI / Codex schedulers use for session stickiness and product identity.
// Secrets (Authorization, Cookie, X-API-Key) are intentionally absent.
var openaiClientSessionHeaderAllowlist = []string{
	"Accept-Language",
	"OpenAI-Beta",
	"User-Agent",
	"X-Request-Id",
	"originator",
	"Conversation-Id",
	"conversation_id",
	"session_id",
	"session-id",
	"thread-id",
	"x-client-request-id",
	"x-codex-beta-features",
	"x-codex-installation-id",
	"x-codex-parent-thread-id",
	"x-codex-routing-hint",
	"x-codex-turn-metadata",
	"x-codex-turn-state",
	"x-codex-window-id",
	"x-openai-memgen-request",
	"x-openai-subagent",
}

// ApplyOpenAIClientSessionHeaders copies the Codex / OpenAI session-identity
// headers that upstream schedulers (including relays) use for account
// stickiness. It is an allowlist: Authorization, cookies, and other client
// credentials are never copied.
func ApplyOpenAIClientSessionHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range openaiClientSessionHeaderAllowlist {
		vals := src.Values(key)
		if len(vals) == 0 {
			continue
		}
		dst.Del(key)
		for _, v := range vals {
			if strings.TrimSpace(v) == "" {
				continue
			}
			dst.Add(key, v)
		}
	}
}

// ClientSessionAnchor returns the best available conversation/session pin
// from client headers or the request body. Empty means the caller should
// fall back to user+group sticky.
//
// Order: session_id / session-id,
// then conversation_id, then Codex thread headers, then Claude Code's
// session header (its Anthropic requests carry no other), then
// prompt_cache_key.
func ClientSessionAnchor(h http.Header, body map[string]any) string {
	if h != nil {
		for _, key := range []string{
			"session_id",
			"session-id",
			"conversation_id",
			"Conversation-Id",
			"x-codex-parent-thread-id",
			"thread-id",
			"X-Claude-Code-Session-Id",
		} {
			if v := strings.TrimSpace(h.Get(key)); v != "" {
				return v
			}
		}
	}
	if body != nil {
		if v, ok := body["prompt_cache_key"].(string); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}
