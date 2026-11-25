package catalog

import "testing"

func TestCatalogParsesPinnedData(t *testing.T) {
	c := MustLoad()
	if c.Version() == "" {
		t.Fatal("missing ompVersion")
	}
	if c.AuthProvider("anthropic") == nil {
		t.Fatal("anthropic auth rule missing")
	}
	if c.AuthProvider("openrouter") == nil {
		t.Fatal("openrouter auth rule missing")
	}
}

func TestProtocolMapsOnlySupportedDialects(t *testing.T) {
	cases := map[string]Protocol{
		"anthropic-messages":     ProtocolAnthropicMessages,
		"openai-completions":     ProtocolOpenAIChatCompletions,
		"openai-responses":       ProtocolOpenAIResponses,
		"azure-openai-responses": ProtocolAzureOpenAIResponses,
	}
	for api, want := range cases {
		got, ok := ProtocolForAPI(api)
		if !ok || got != want {
			t.Fatalf("ProtocolForAPI(%q) = %q,%v want %q", api, got, ok, want)
		}
	}
	if _, ok := ProtocolForAPI("google-generative-ai"); ok {
		t.Fatal("google dialects must be unmapped")
	}
	if _, ok := ProtocolForAPI("bedrock-converse-stream"); ok {
		t.Fatal("bedrock dialects must be unmapped")
	}
	if _, ok := ProtocolForAPI("openai-codex-responses"); ok {
		t.Fatal("the ChatGPT subscription backend must be unmapped")
	}
}

func TestResolveProtocolPrefersExplicit(t *testing.T) {
	if got := ResolveProtocol(ProtocolAnthropicMessages, "gpt-5.5"); got != ProtocolAnthropicMessages {
		t.Fatalf("explicit anthropic lost to heuristic: %s", got)
	}
	if got := ResolveProtocol("", "claude-opus-4-8"); got != ProtocolAnthropicMessages {
		t.Fatalf("claude-* should resolve anthropic, got %s", got)
	}
	if got := ResolveProtocol("", "gpt-5.5"); got != ProtocolOpenAIResponses {
		t.Fatalf("non-claude should resolve responses, got %s", got)
	}
	for _, p := range []Protocol{ProtocolOpenAIResponses, ProtocolAzureOpenAIResponses, ProtocolAnthropicMessages, ProtocolOpenAIChatCompletions} {
		if got := ResolveProtocol(p, "any-model"); got != p {
			t.Fatalf("round trip failed for %s: %s", p, got)
		}
	}
}

func TestResolvesModelAPIFromCatalog(t *testing.T) {
	c := MustLoad()
	if got := c.APIFor("anthropic", "claude-sonnet-4-5"); got != "anthropic-messages" {
		t.Fatalf("anthropic model api = %q", got)
	}
	if got, ok := c.ProtocolFor("deepseek", "deepseek-v4-pro"); !ok || got != ProtocolOpenAIChatCompletions {
		t.Fatalf("deepseek protocol = %q,%v", got, ok)
	}
}

func TestGlobMatcherHandlesStarAndQuestion(t *testing.T) {
	cases := []struct {
		pattern, text string
		want          bool
	}{
		{"claude-*", "claude-opus-4-8", true},
		{"gpt-?", "gpt-6", true},
		{"claude-*", "gpt-5", false},
		{"*", "anything", true},
		{"a*c", "abc", true},
		{"a*c", "abd", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.text); got != tc.want {
			t.Fatalf("globMatch(%q,%q) = %v", tc.pattern, tc.text, got)
		}
	}
}

// The vendored artifact is filtered at sync time: only API-key (and
// hook-driven key) providers remain, never a subscription login.
func TestCatalogCarriesNoSubscriptionLogins(t *testing.T) {
	c := MustLoad()
	for _, p := range c.AuthProviders() {
		switch p.LoginKind() {
		case "", "api-key", "custom":
		default:
			t.Fatalf("%s declares a %q login", p.ID, p.LoginKind())
		}
	}
	for _, id := range []string{"openai-codex", "kimi-code", "github-copilot", "xai-oauth", "muse-code"} {
		if c.AuthProvider(id) != nil || len(c.Models(id)) > 0 {
			t.Fatalf("%s must not be in the catalog", id)
		}
	}
}
