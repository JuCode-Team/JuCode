package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/schema"
)

// Azure AI Foundry answers 400 "Unexpected value(s) `advisor-tool-…` for the
// `anthropic-beta` header" to Claude Code's default beta list. The account's
// rules must strip exactly the rejected flag and keep the rest.
func TestAnthropicProviderAppliesAzureDefinitionHeaderRules(t *testing.T) {
	azure, ok := biz.BuiltinDefinitionByID("azure-foundry-claude")
	if !ok {
		t.Fatal("azure-foundry-claude builtin definition missing")
	}
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	acct := &ent.UpstreamAccount{BaseURL: server.URL, HeaderRules: azure.HeaderRules}
	client := http.Header{}
	client.Set("Anthropic-Beta", "claude-code-20250219,advisor-tool-2026-03-01,context-management-2025-06-27")
	client.Set("X-Api-Key", "jucode-caller-key")
	prov := NewAnthropicProviderWithClient(acct, "azure-key", server.Client())
	if _, err := prov.Forward(context.Background(), "/v1/messages", []byte(`{}`), true, client); err != nil {
		t.Fatal(err)
	}
	if beta := got.Get("Anthropic-Beta"); beta != "claude-code-20250219,context-management-2025-06-27" {
		t.Fatalf("Anthropic-Beta = %q, want advisor flag dropped and the rest kept", beta)
	}
	if key := got.Get("X-Api-Key"); key != "azure-key" {
		t.Fatalf("X-Api-Key = %q, want the account key (Foundry accepts x-api-key)", key)
	}
}

// Gateways that want the key under their own header name declare it with the
// {api_key} placeholder; the default credential header can be removed.
func TestOpenAIProviderHeaderRulesMoveCredential(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	acct := &ent.UpstreamAccount{BaseURL: server.URL, HeaderRules: schema.HeaderRules{
		Set:    map[string]string{"Ocp-Apim-Subscription-Key": "{api_key}"},
		Remove: []string{"Authorization"},
	}}
	prov := NewOpenAIProviderWithClient(acct, "gw-key", server.Client())
	if _, err := prov.Forward(context.Background(), "/v1/chat/completions", []byte(`{}`), false, nil); err != nil {
		t.Fatal(err)
	}
	if got.Get("Ocp-Apim-Subscription-Key") != "gw-key" || got.Get("Authorization") != "" {
		t.Fatalf("headers = %v, want key moved to Ocp-Apim-Subscription-Key", got)
	}
}

// Relays in front of Claude Code have been seen to drop the client's beta
// header while keeping thinking.display "updates", which Foundry then rejects.
func TestAnthropicProviderAddsDisplayUpdatesBetaForAzure(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	updates := []byte(`{"thinking":{"type":"adaptive","display":"updates"},"messages":[]}`)
	cases := []struct {
		definition string
		body       []byte
		want       string
	}{
		{biz.AzureFoundryClaudeDefinitionID, updates, "claude-code-20250219,thinking-display-updates-2026-08-18"},
		{biz.AzureFoundryClaudeDefinitionID, []byte(`{"messages":[{"role":"user","content":"updates"}]}`), "claude-code-20250219"},
		{"anthropic", updates, "claude-code-20250219"},
	}
	for _, tc := range cases {
		acct := &ent.UpstreamAccount{BaseURL: server.URL, DefinitionID: tc.definition}
		client := http.Header{}
		client.Set("Anthropic-Beta", "claude-code-20250219")
		prov := NewAnthropicProviderWithClient(acct, "key", server.Client())
		if _, err := prov.Forward(context.Background(), "/v1/messages", tc.body, false, client); err != nil {
			t.Fatal(err)
		}
		if beta := got.Get("Anthropic-Beta"); beta != tc.want {
			t.Fatalf("%s %s: Anthropic-Beta = %q, want %q", tc.definition, tc.body, beta, tc.want)
		}
	}
}
