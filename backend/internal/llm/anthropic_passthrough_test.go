package llm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnthropicNativeBodyDefaultRewritesModel(t *testing.T) {
	raw := []byte(`{"model":"client-model","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`)

	got, err := anthropicNativeBody(raw, "upstream-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`"client-model"`)) {
		t.Fatalf("client model leaked into rewritten body: %s", got)
	}
	if !bytes.Contains(got, []byte(`"upstream-model"`)) {
		t.Fatalf("upstream model missing from rewritten body: %s", got)
	}
}

// Remarshalling the whole document HTML-escapes text and rebuilds nested
// objects. Prompt caches and the prefill that follows key off that text, so
// a model swap has to leave every other byte alone.
func TestRewriteModelKeepsNestedBytes(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"a<b","model":"keep-me"}],"model":"client-model","max_tokens":1}`)

	got, err := rewriteModel(raw, "upstream-model")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`\u003c`)) {
		t.Fatalf("nested text was HTML-escaped: %s", got)
	}
	if !bytes.Contains(got, []byte(`a<b`)) || !bytes.Contains(got, []byte(`"model":"keep-me"`)) {
		t.Fatalf("nested bytes were rebuilt: %s", got)
	}
	if bytes.Contains(got, []byte(`"client-model"`)) || !bytes.Contains(got, []byte(`"upstream-model"`)) {
		t.Fatalf("top-level model = %s", got)
	}

	same := []byte("{\n  \"model\": \"upstream-model\",\n  \"messages\": [{\"content\": \"a<b\"}]\n}")
	untouched, err := rewriteModel(same, "upstream-model")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(untouched, same) {
		t.Fatalf("unchanged model was re-encoded:\n%s", untouched)
	}
}

func TestDefaultAnthropicForwardKeepsClientBeta(t *testing.T) {
	var gotReq *http.Request
	prov := &anthropicProvider{
		baseURL: "https://upstream.example",
		apiKey:  "provider-key",
		hc: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotReq = r.Clone(r.Context())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    r,
			}, nil
		})},
	}
	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Anthropic-Beta", "prompt-caching-2024-07-31,fine-grained-tool-streaming-2025-05-14")
	headers.Set("Authorization", "Bearer sk-juc-client")
	headers.Set("X-API-Key", "sk-juc-client")

	resp, err := prov.Forward(context.Background(), "/v1/messages", []byte(`{}`), true, headers)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotReq.Header.Get("X-API-Key") != "provider-key" {
		t.Fatalf("X-API-Key = %q", gotReq.Header.Get("X-API-Key"))
	}
	if gotReq.Header.Get("Authorization") != "" {
		t.Fatalf("caller Authorization leaked: %q", gotReq.Header.Get("Authorization"))
	}
	if gotReq.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q", gotReq.Header.Get("Anthropic-Version"))
	}
	beta := gotReq.Header.Get("Anthropic-Beta")
	for _, want := range []string{"prompt-caching-2024-07-31", "fine-grained-tool-streaming-2025-05-14"} {
		if !strings.Contains(beta, want) {
			t.Fatalf("Anthropic-Beta = %q, missing %s", beta, want)
		}
	}
}

func TestUpstreamTransportsAttemptHTTP2(t *testing.T) {
	if !newProviderTransport().ForceAttemptHTTP2 {
		t.Fatal("provider transport is not attempting HTTP/2")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
