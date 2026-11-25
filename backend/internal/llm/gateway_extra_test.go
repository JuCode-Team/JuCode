package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

func TestAccountDialectDeclaration(t *testing.T) {
	cases := []struct {
		name   string
		acct   *ent.UpstreamAccount
		client biz.RequestProtocol
		conv   bool
		want   Dialect
		wantOK bool
	}{
		// A native protocol match wins and needs no conversion.
		{"responses-native keeps a responses client", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolResponses}}, biz.RequestProtocolResponses, true, DialectOpenAI, true},
		{"chat-native keeps a chat client", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolChat}}, biz.RequestProtocolCompatible, true, DialectCompatible, true},
		{"messages-native keeps an anthropic client", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolMessages}}, biz.RequestProtocolAnthropic, true, DialectAnthropic, true},
		// No native match: with conversion allowed, the account is spoken to
		// in its own (first) native protocol.
		{"responses-only converts a chat client", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolResponses}, AllowConversion: true}, biz.RequestProtocolCompatible, true, DialectOpenAI, true},
		{"chat-only converts a responses client", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolChat}, AllowConversion: true}, biz.RequestProtocolResponses, true, DialectCompatible, true},
		{"anthropic-only converts a chat client", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolMessages}, AllowConversion: true}, biz.RequestProtocolCompatible, true, DialectAnthropic, true},
		// An Anthropic client has no Anthropic upstream here, so it converts
		// to the account's OpenAI-family protocol.
		{"anthropic client on chat account", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolChat}, AllowConversion: true}, biz.RequestProtocolAnthropic, true, DialectCompatible, true},
		{"anthropic client on responses-only account", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolResponses}, AllowConversion: true}, biz.RequestProtocolAnthropic, true, DialectOpenAI, true},
		// Dual-surface account: the Messages tag must not hijack an
		// OpenAI-family client that the account serves natively.
		{"dual keeps a chat client native", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolChat, biz.NativeProtocolMessages}, AllowConversion: true}, biz.RequestProtocolCompatible, true, DialectCompatible, true},
		// Empty declaration no longer follows the old channel "kind"; it
		// defaults to chat_completions, so a chat client is native.
		{"empty declaration defaults to chat", &ent.UpstreamAccount{}, biz.RequestProtocolCompatible, true, DialectCompatible, true},
		// No native match and conversion off: the account must not be tried.
		{"no native match with conversion off", &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolMessages}, AllowConversion: true}, biz.RequestProtocolCompatible, false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AccountDialect(tc.acct, "", tc.client, tc.conv)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("got (%q, %v) want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestCompletionsToChatAndBack(t *testing.T) {
	prompt := "complete-me"
	raw := []byte(`{"model":"m","prompt":"` + prompt + `","max_tokens":8,"stream":false}`)
	chat, stream, err := completionsToChatBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		t.Fatal("expected non-stream")
	}
	got := mustUnmarshal(t, chat)
	msgs := asSlice(got["messages"])
	if len(msgs) == 0 || flattenChatContent(asMap(msgs[0])["content"]) != prompt {
		t.Fatalf("messages = %#v", got["messages"])
	}
	back := chatJSONToCompletions([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"done-` + prompt + `"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	out := mustUnmarshal(t, back)
	if out["object"] != "text_completion" {
		t.Fatalf("object = %v", out["object"])
	}
	text := strVal(asMap(asSlice(out["choices"])[0])["text"])
	if !strings.Contains(text, prompt) {
		t.Fatalf("text = %q", text)
	}
}

func TestHandleCompletionsMounted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/v1")
	v1.POST("/completions", (&Gateway{}).HandleCompletions)
	v1.POST("/alpha/search", (&Gateway{}).HandleAlphaSearch)
	var hasComp, hasSearch bool
	for _, rt := range r.Routes() {
		if rt.Method == http.MethodPost && strings.HasSuffix(rt.Path, "/completions") {
			hasComp = true
		}
		if rt.Method == http.MethodPost && strings.HasSuffix(rt.Path, "/alpha/search") {
			hasSearch = true
		}
	}
	if !hasComp || !hasSearch {
		t.Fatalf("routes completions=%v alpha/search=%v", hasComp, hasSearch)
	}
}

func TestAlphaSearchForwardsAndCountsWebSearch(t *testing.T) {
	query := "codex-alpha-q"
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"s1","object":"search","results":[{"url":"https://ex.test"}]}`))
	}))
	defer srv.Close()

	p := &ent.UpstreamAccount{BaseURL: srv.URL}
	prov := NewOpenAIProvider(p, "k")
	body := []byte(`{"model":"gpt-search","query":"` + query + `"}`)
	var obj map[string]any
	_ = json.Unmarshal(body, &obj)
	encoded, err := prepareForwardBody(obj, "gpt-search-up", false)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := prov.Forward(context.Background(), "/v1/alpha/search", encoded, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if gotPath != "/v1/alpha/search" {
		t.Fatalf("path = %s", gotPath)
	}
	if !strings.Contains(string(gotBody), query) {
		t.Fatalf("upstream missing query: %s", gotBody)
	}
	acc := &Accumulator{}
	acc.Inspect(raw)
	if acc.WebSearchRequests == 0 {
		acc.WebSearchRequests = 1
	}
	if acc.WebSearchRequests != 1 {
		t.Fatalf("web_search_requests = %d", acc.WebSearchRequests)
	}
}

func TestConvertRequestPreservesCodexBuiltins(t *testing.T) {
	in := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "run it"},
		},
		"tools": []any{
			map[string]any{"type": "file_search", "vector_store_ids": []any{"vs_1"}},
			map[string]any{"type": "code_execution"},
			map[string]any{"type": "web_search"},
		},
	}
	raw, _ := json.Marshal(in)
	out, err := ConvertRequest(DialectCompatible, DialectAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "file_search") {
		t.Fatalf("dropped file_search: %s", got)
	}
	if !strings.Contains(got, "code_execution") {
		t.Fatalf("dropped code_execution: %s", got)
	}
	if !strings.Contains(got, anthropicWebSearchType) {
		t.Fatalf("dropped web_search mapping: %s", got)
	}
}
