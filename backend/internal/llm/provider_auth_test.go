package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestAnthropicProviderKeepsAPIKeySchemeForKeys(t *testing.T) {
	var gotAuth, gotAPIKey, gotBeta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-API-Key")
		gotBeta = r.Header.Get("Anthropic-Beta")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	prov := NewAnthropicProviderWithClient(&ent.UpstreamAccount{BaseURL: server.URL}, "sk-ant-key", server.Client())
	if _, err := prov.Forward(context.Background(), "/v1/messages", []byte(`{}`), false, nil); err != nil {
		t.Fatal(err)
	}
	if gotAPIKey != "sk-ant-key" {
		t.Fatalf("X-API-Key = %q, want sk-ant-key", gotAPIKey)
	}
	if gotAuth != "" || gotBeta != "" {
		t.Fatalf("api-key accounts must not send Authorization=%q / Anthropic-Beta=%q", gotAuth, gotBeta)
	}
}
