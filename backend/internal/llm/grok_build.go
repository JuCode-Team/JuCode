package llm

import (
	"net/http"
	"strings"
)

// Grok Build (grok-shell) speaks the same three wire protocols JuCode already
// has, but it looks them up under the OpenAI-style /v1 prefix:
//
//	POST /v1/chat/completions   api_backend=chat_completions
//	POST /v1/responses          api_backend=responses  (official xAI default)
//	POST /v1/messages           api_backend=messages   (NOT /anthropic/v1/messages)
//	GET  /v1/models             envelope includes api_backend
//
// JuCode's existing Anthropic surface stays at /anthropic/v1/* for Claude
// Code / official SDKs. Grok Build compatibility is the /v1 alias plus the
// model-list envelope below.

const grokBuildAPIBackendResponses = "responses"

// IsGrokBuildClient reports whether the caller is Grok Build / Grok Shell.
// Detection matches a User-Agent containing "grok-shell" and also the
// official CLI's "xai-grok-build" product string and client identifier.
func IsGrokBuildClient(r *http.Request) bool {
	if r == nil {
		return false
	}
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if strings.Contains(ua, "grok-shell") || strings.Contains(ua, "xai-grok-build") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Grok-Client-Identifier")), "grok-shell")
}

// GrokBuildReasoningEffort is the object form Grok Shell expects on /v1/models.
type GrokBuildReasoningEffort struct {
	Value string `json:"value"`
}

// GrokBuildModelEntry is one model in the Grok Shell /v1/models envelope.
type GrokBuildModelEntry struct {
	ID               string                     `json:"id"`
	Model            string                     `json:"model"`
	Name             string                     `json:"name"`
	ContextWindow    int                        `json:"context_window,omitempty"`
	APIBackend       string                     `json:"api_backend"`
	SupportedInAPI   bool                       `json:"supported_in_api"`
	ReasoningEfforts []GrokBuildReasoningEffort `json:"reasoning_efforts,omitempty"`
}

// GrokBuildModelsResponse is the list envelope Grok Build reads from GET /v1/models.
type GrokBuildModelsResponse struct {
	Object string                `json:"object"`
	Data   []GrokBuildModelEntry `json:"data"`
}

// GrokBuildModelInfo is the gateway-side input used to build a Grok Shell entry.
type GrokBuildModelInfo struct {
	ID               string
	Name             string
	ContextWindow    int
	APIBackend       string
	ReasoningEfforts []string
}

// GrokBuildAPIBackendForModel is the api_backend Grok Shell should use for a
// catalog id. Official grok-shell defaults to "responses" (/v1/responses);
// JuCode advertises the same so the CLI does not have to override it.
func GrokBuildAPIBackendForModel(id string) string {
	_ = id
	return grokBuildAPIBackendResponses
}

// BuildGrokBuildModelsResponse formats models the way Grok Build / Grok Shell
// expects. api_backend follows GrokBuildAPIBackendForModel unless the caller
// set an explicit override.
func BuildGrokBuildModelsResponse(models []GrokBuildModelInfo) GrokBuildModelsResponse {
	out := make([]GrokBuildModelEntry, 0, len(models))
	for _, m := range models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = m.ID
		}
		backend := strings.TrimSpace(m.APIBackend)
		if backend == "" {
			backend = GrokBuildAPIBackendForModel(m.ID)
		}
		entry := GrokBuildModelEntry{
			ID:             m.ID,
			Model:          m.ID,
			Name:           name,
			APIBackend:     backend,
			SupportedInAPI: true,
		}
		if m.ContextWindow > 0 {
			entry.ContextWindow = m.ContextWindow
		}
		for _, level := range m.ReasoningEfforts {
			if trimmed := strings.TrimSpace(level); trimmed != "" {
				entry.ReasoningEfforts = append(entry.ReasoningEfforts, GrokBuildReasoningEffort{Value: trimmed})
			}
		}
		out = append(out, entry)
	}
	return GrokBuildModelsResponse{Object: "list", Data: out}
}

// IsAnthropicRequestPath reports whether a client-facing path carries the
// Anthropic Messages wire protocol. JuCode's native surface is
// /anthropic/v1/messages; Grok Build uses /v1/messages.
func IsAnthropicRequestPath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	if strings.HasPrefix(path, "/anthropic") {
		return true
	}
	return path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
}
