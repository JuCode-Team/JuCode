package llm

import (
	"net/http"
	"testing"
)

func TestIsGrokBuildClient(t *testing.T) {
	req := func(ua, ident string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "/v1/models", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if ident != "" {
			r.Header.Set("X-Grok-Client-Identifier", ident)
		}
		return r
	}
	for _, tc := range []struct {
		name string
		r    *http.Request
		want bool
	}{
		{name: "nil", want: false},
		{name: "generic curl", r: req("curl/8.0", ""), want: false},
		{name: "claude-cli", r: req("claude-cli/1.0", ""), want: false},
		{name: "grok-shell", r: req("grok-shell/1.0.5 (windows; x86_64)", ""), want: true},
		{name: "xai-grok-build", r: req("xai-grok-build/0.2.99", ""), want: true},
		{name: "identifier", r: req("Go-http-client/2.0", "grok-shell"), want: true},
	} {
		if got := IsGrokBuildClient(tc.r); got != tc.want {
			t.Fatalf("%s: IsGrokBuildClient = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildGrokBuildModelsResponse(t *testing.T) {
	got := BuildGrokBuildModelsResponse([]GrokBuildModelInfo{
		{ID: "grok-4.6", Name: "Grok 4.6", ContextWindow: 256000, ReasoningEfforts: []string{"low", "high"}},
		{ID: "gpt-5.5", ReasoningEfforts: []string{"", "medium"}},
	})
	if got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("envelope = %+v", got)
	}
	first := got.Data[0]
	if first.ID != "grok-4.6" || first.Model != "grok-4.6" || first.Name != "Grok 4.6" {
		t.Fatalf("first identity = %+v", first)
	}
	if first.APIBackend != "responses" || !first.SupportedInAPI || first.ContextWindow != 256000 {
		t.Fatalf("first flags = %+v", first)
	}
	if len(first.ReasoningEfforts) != 2 || first.ReasoningEfforts[0].Value != "low" {
		t.Fatalf("first efforts = %+v", first.ReasoningEfforts)
	}
	second := got.Data[1]
	if second.APIBackend != "responses" {
		t.Fatalf("non-grok model api_backend = %q; want responses", second.APIBackend)
	}
	if second.Name != "gpt-5.5" || second.ContextWindow != 0 {
		t.Fatalf("second = %+v", second)
	}
	if len(second.ReasoningEfforts) != 1 || second.ReasoningEfforts[0].Value != "medium" {
		t.Fatalf("second efforts = %+v", second.ReasoningEfforts)
	}
}

func TestGrokBuildAPIBackendForModel(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"grok-4.6", "responses"},
		{"Grok-4.5", "responses"},
		{"grok-composer-2.5-fast", "responses"},
		{"gpt-5.5", "responses"},
		{"claude-opus-4-6", "responses"},
	} {
		if got := GrokBuildAPIBackendForModel(tc.id); got != tc.want {
			t.Fatalf("GrokBuildAPIBackendForModel(%q) = %q; want %q", tc.id, got, tc.want)
		}
	}
}

func TestIsAnthropicRequestPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/anthropic/v1/messages", true},
		{"/anthropic/v1/models", true},
		{"/v1/messages", true},
		{"/v1/messages/count_tokens", true},
		{"/v1/chat/completions", false},
		{"/v1/responses", false},
		{"/v1/models", false},
	} {
		if got := IsAnthropicRequestPath(tc.path); got != tc.want {
			t.Fatalf("IsAnthropicRequestPath(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}
