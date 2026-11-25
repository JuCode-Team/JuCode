package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAIProviderForwardPassesSessionHeaders(t *testing.T) {
	var gotReq *http.Request
	prov := &openaiProvider{
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
	headers.Set("User-Agent", "codex_cli_rs/0.87.0 (Windows 10.0.26200; x86_64) WindowsTerminal")
	headers.Set("session_id", "sess-abc")
	headers.Set("session-id", "thread-xyz")
	headers.Set("conversation_id", "conv-1")
	headers.Set("x-codex-parent-thread-id", "parent-1")
	headers.Set("x-codex-turn-metadata", `{"turn":1}`)
	headers.Set("Authorization", "Bearer sk-juc-client")
	headers.Set("Cookie", "session=secret")

	resp, err := prov.Forward(context.Background(), "/v1/responses", []byte(`{"ok":true}`), true, headers)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("Authorization") != "Bearer provider-key" {
		t.Fatalf("Authorization = %q", gotReq.Header.Get("Authorization"))
	}
	if gotReq.Header.Get("Cookie") != "" {
		t.Fatalf("Cookie leaked upstream: %q", gotReq.Header.Get("Cookie"))
	}
	if gotReq.Header.Get("User-Agent") != "codex_cli_rs/0.87.0 (Windows 10.0.26200; x86_64) WindowsTerminal" {
		t.Fatalf("User-Agent = %q", gotReq.Header.Get("User-Agent"))
	}
	if gotReq.Header.Get("session_id") != "sess-abc" {
		t.Fatalf("session_id = %q", gotReq.Header.Get("session_id"))
	}
	if gotReq.Header.Get("session-id") != "thread-xyz" {
		t.Fatalf("session-id = %q", gotReq.Header.Get("session-id"))
	}
	if gotReq.Header.Get("conversation_id") != "conv-1" {
		t.Fatalf("conversation_id = %q", gotReq.Header.Get("conversation_id"))
	}
	if gotReq.Header.Get("x-codex-parent-thread-id") != "parent-1" {
		t.Fatalf("x-codex-parent-thread-id = %q", gotReq.Header.Get("x-codex-parent-thread-id"))
	}
	if gotReq.Header.Get("x-codex-turn-metadata") != `{"turn":1}` {
		t.Fatalf("x-codex-turn-metadata = %q", gotReq.Header.Get("x-codex-turn-metadata"))
	}
}

func TestClientSessionAnchorPrefersSessionID(t *testing.T) {
	h := http.Header{}
	h.Set("conversation_id", "conv-1")
	h.Set("session_id", "sess-abc")
	if got := ClientSessionAnchor(h, map[string]any{"prompt_cache_key": "pck-1"}); got != "sess-abc" {
		t.Fatalf("anchor = %q; want sess-abc", got)
	}
}

func TestClientSessionAnchorFallsBackToSessionHyphenID(t *testing.T) {
	h := http.Header{}
	h.Set("session-id", "thread-xyz")
	h.Set("conversation_id", "conv-1")
	if got := ClientSessionAnchor(h, nil); got != "thread-xyz" {
		t.Fatalf("anchor = %q; want thread-xyz", got)
	}
}

func TestClientSessionAnchorReadsClaudeCodeSession(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "6d37b1f4-67eb-4cb8-8334-08c85ce90d5a")
	if got := ClientSessionAnchor(h, nil); got != "6d37b1f4-67eb-4cb8-8334-08c85ce90d5a" {
		t.Fatalf("anchor = %q", got)
	}
}

func TestClientSessionAnchorFallsBackToPromptCacheKey(t *testing.T) {
	if got := ClientSessionAnchor(http.Header{}, map[string]any{"prompt_cache_key": "pck-1"}); got != "pck-1" {
		t.Fatalf("anchor = %q; want pck-1", got)
	}
}

func TestClientSessionAnchorEmpty(t *testing.T) {
	if got := ClientSessionAnchor(http.Header{}, map[string]any{"model": "gpt-5"}); got != "" {
		t.Fatalf("anchor = %q; want empty", got)
	}
}

func TestApplyOpenAIClientSessionHeadersDoesNotCopySecrets(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer sk-juc-client")
	src.Set("Cookie", "session=secret")
	src.Set("X-API-Key", "sk-juc-client")
	src.Set("session_id", "sess-abc")
	src.Set("User-Agent", "codex_cli_rs/0.87.0")

	dst := http.Header{}
	dst.Set("Authorization", "Bearer provider-key")
	ApplyOpenAIClientSessionHeaders(dst, src)

	if dst.Get("Authorization") != "Bearer provider-key" {
		t.Fatalf("Authorization overwritten: %q", dst.Get("Authorization"))
	}
	if dst.Get("Cookie") != "" {
		t.Fatalf("Cookie leaked: %q", dst.Get("Cookie"))
	}
	if dst.Get("X-API-Key") != "" {
		t.Fatalf("X-API-Key leaked: %q", dst.Get("X-API-Key"))
	}
	if dst.Get("session_id") != "sess-abc" {
		t.Fatalf("session_id = %q", dst.Get("session_id"))
	}
	if dst.Get("User-Agent") != "codex_cli_rs/0.87.0" {
		t.Fatalf("User-Agent = %q", dst.Get("User-Agent"))
	}
}
