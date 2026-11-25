package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

func mustUnmarshal(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	return m
}

func TestConvertRequestCompatibleToAnthropic(t *testing.T) {
	sys := "be brief"
	user := "search the docs"
	in := map[string]any{
		"model":      "gpt-test",
		"max_tokens": 32,
		"stop":       []string{"END"},
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": user},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup",
					"description": "look things up",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
				},
			},
		},
		"tool_choice":        "auto",
		"web_search_options": map[string]any{"search_context_size": "medium"},
	}
	raw, _ := json.Marshal(in)
	out, err := ConvertRequest(DialectCompatible, DialectAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := mustUnmarshal(t, out)
	if got["system"] != sys {
		t.Fatalf("system = %#v, want %q", got["system"], sys)
	}
	msgs := asSlice(got["messages"])
	if len(msgs) == 0 {
		t.Fatal("missing messages")
	}
	first := asMap(msgs[0])
	if flattenChatContent(first["content"]) != user {
		t.Fatalf("user content = %#v, want %q", first["content"], user)
	}
	if intVal(got["max_tokens"]) != 32 {
		t.Fatalf("max_tokens = %v", got["max_tokens"])
	}
	stops := asSlice(got["stop_sequences"])
	if len(stops) != 1 || strVal(stops[0]) != "END" {
		t.Fatalf("stop_sequences = %#v", got["stop_sequences"])
	}
	var sawLookup, sawSearch bool
	for _, rawT := range asSlice(got["tools"]) {
		tl := asMap(rawT)
		if strVal(tl["name"]) == "lookup" && tl["input_schema"] != nil {
			sawLookup = true
		}
		if strVal(tl["type"]) == anthropicWebSearchType && strVal(tl["name"]) == "web_search" {
			sawSearch = true
			if intVal(tl["max_uses"]) != webSearchMaxUsesMedium {
				t.Fatalf("web_search max_uses = %v", tl["max_uses"])
			}
		}
	}
	if !sawLookup {
		t.Fatalf("lookup tool missing: %s", out)
	}
	if !sawSearch {
		t.Fatalf("anthropic web_search tool missing: %s", out)
	}
}

func TestConvertRequestCompatibleToResponses(t *testing.T) {
	sys := "system-rail"
	user := "hello-input"
	in := map[string]any{
		"model":      "gpt-test",
		"max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": user},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}},
			map[string]any{"type": "web_search_preview"},
		},
		"web_search_options": map[string]any{"search_context_size": "low"},
	}
	raw, _ := json.Marshal(in)
	out, err := ConvertRequest(DialectCompatible, DialectOpenAI, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := mustUnmarshal(t, out)
	if got["instructions"] != sys {
		t.Fatalf("instructions = %#v, want %q", got["instructions"], sys)
	}
	if got["input"] != user && flattenChatContent(got["input"]) != user {
		// input may be a string or an array of items
		if s, ok := got["input"].(string); !ok || s != user {
			items := asSlice(got["input"])
			found := false
			for _, it := range items {
				m := asMap(it)
				if m != nil && flattenChatContent(m["content"]) == user {
					found = true
				}
			}
			if !found {
				t.Fatalf("input missing %q: %#v", user, got["input"])
			}
		}
	}
	if intVal(got["max_output_tokens"]) != 64 {
		t.Fatalf("max_output_tokens = %v", got["max_output_tokens"])
	}
	var sawFn, sawSearch bool
	for _, rawT := range asSlice(got["tools"]) {
		tl := asMap(rawT)
		if strVal(tl["type"]) == "function" && strVal(tl["name"]) == "lookup" {
			sawFn = true
		}
		if isWebSearchTool(tl) {
			sawSearch = true
		}
	}
	if !sawFn || !sawSearch {
		t.Fatalf("tools incomplete fn=%v search=%v body=%s", sawFn, sawSearch, out)
	}
}

func TestApplyRequestConversionDisabledPassthrough(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"keep-me"}],"web_search_options":{"search_context_size":"high"}}`)
	out, err := ApplyRequestConversion(false, DialectCompatible, DialectAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("disabled conversion rewrote body:\n%s", out)
	}
	got := mustUnmarshal(t, out)
	if _, ok := got["messages"]; !ok {
		t.Fatal("disabled conversion dropped messages")
	}
	if _, ok := got["system"]; ok {
		t.Fatal("disabled conversion force-extracted system")
	}
}

func TestApplyRequestConversionEnabledRewrites(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"keep-me"}]}`)
	out, err := ApplyRequestConversion(true, DialectCompatible, DialectAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(out, raw) {
		t.Fatal("enabled conversion left body unchanged")
	}
	got := mustUnmarshal(t, out)
	if _, ok := got["messages"]; !ok {
		t.Fatal("converted body missing messages")
	}
	if intVal(got["max_tokens"]) <= 0 {
		t.Fatal("anthropic conversion must set max_tokens")
	}
}

func TestConvertResponsePreservesTextToolsAndWebSearch(t *testing.T) {
	userText := "cited answer"
	anth := map[string]any{
		"id":    "msg_src",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-test",
		"content": []any{
			map[string]any{
				"type": "web_search_tool_result",
				"id":   "ws_1",
				"content": []any{
					map[string]any{"type": "web_search_result", "url": "https://example.test/a", "title": "A"},
				},
			},
			map[string]any{"type": "text", "text": userText, "citations": []any{map[string]any{"url": "https://example.test/a"}}},
			map[string]any{"type": "tool_use", "id": "tu_1", "name": "lookup", "input": map[string]any{"q": "docs"}},
		},
		"stop_reason": "tool_use",
		"usage": map[string]any{
			"input_tokens": 11, "output_tokens": 7,
			"server_tool_use": map[string]any{"web_search_requests": 2},
		},
	}
	raw, _ := json.Marshal(anth)
	chatRaw, err := ConvertResponse(DialectAnthropic, DialectCompatible, raw)
	if err != nil {
		t.Fatal(err)
	}
	chat := mustUnmarshal(t, chatRaw)
	ch0 := asMap(asSlice(chat["choices"])[0])
	msg := asMap(ch0["message"])
	if flattenChatContent(msg["content"]) != userText {
		t.Fatalf("chat text = %#v", msg["content"])
	}
	if len(asSlice(msg["tool_calls"])) == 0 {
		t.Fatalf("tool_calls dropped: %s", chatRaw)
	}
	if len(asSlice(msg["web_search_calls"])) == 0 && msg["citations"] == nil {
		t.Fatalf("web search / citations dropped: %s", chatRaw)
	}
	if extractWebSearchRequests(chat) != 2 {
		t.Fatalf("chat web_search_requests = %d", extractWebSearchRequests(chat))
	}

	respRaw, err := ConvertResponse(DialectCompatible, DialectOpenAI, chatRaw)
	if err != nil {
		t.Fatal(err)
	}
	resp := mustUnmarshal(t, respRaw)
	if resp["output"] == nil {
		t.Fatalf("responses output missing: %s", respRaw)
	}
	joined, _ := json.Marshal(resp["output"])
	if !bytes.Contains(joined, []byte(userText)) {
		t.Fatalf("responses dropped text: %s", respRaw)
	}
	if !bytes.Contains(joined, []byte("lookup")) && !bytes.Contains(respRaw, []byte("lookup")) {
		t.Fatalf("responses dropped tool call: %s", respRaw)
	}
	if extractWebSearchRequests(resp) != 2 {
		t.Fatalf("responses web_search_requests = %d body=%s", extractWebSearchRequests(resp), respRaw)
	}
}

func TestConvertStreamChatToAnthropicAndResponses(t *testing.T) {
	phrase := "stream-hello"
	src := strings.Join([]string{
		`data: {"id":"cmpl_1","choices":[{"delta":{"content":"` + phrase + `"}}]}`,
		``,
		`data: {"id":"cmpl_1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"web_search_requests":1}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var anth bytes.Buffer
	if err := ConvertStream(DialectCompatible, DialectAnthropic, strings.NewReader(src), &anth); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(anth.String(), phrase) {
		t.Fatalf("anthropic stream dropped text: %s", anth.String())
	}
	if !strings.Contains(anth.String(), "text_delta") && !strings.Contains(anth.String(), "content_block_delta") {
		t.Fatalf("anthropic stream missing deltas: %s", anth.String())
	}

	var resp bytes.Buffer
	if err := ConvertStream(DialectCompatible, DialectOpenAI, strings.NewReader(src), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.String(), phrase) {
		t.Fatalf("responses stream dropped text: %s", resp.String())
	}
	if !strings.Contains(resp.String(), "response.output_text.delta") && !strings.Contains(resp.String(), "response.completed") {
		t.Fatalf("responses stream missing events: %s", resp.String())
	}
}

func TestConvertStreamPreservesWebSearchBlocks(t *testing.T) {
	src := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"web_search_tool_result","id":"ws_1","content":[{"url":"https://example.test"}]}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"found-it"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3,"server_tool_use":{"web_search_requests":1}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	var dst bytes.Buffer
	if err := ConvertStream(DialectAnthropic, DialectCompatible, strings.NewReader(src), &dst); err != nil {
		t.Fatal(err)
	}
	got := dst.String()
	if !strings.Contains(got, "found-it") {
		t.Fatalf("dropped text: %s", got)
	}
	if !strings.Contains(got, "web_search") && !strings.Contains(got, "example.test") {
		t.Fatalf("dropped web search block: %s", got)
	}
	if !strings.Contains(got, "web_search_requests") {
		t.Fatalf("dropped web_search usage: %s", got)
	}
}

func TestRewriteForAccountToggle(t *testing.T) {
	body := []byte(`{"model":"client-m","messages":[{"role":"user","content":"ping-me"}],"web_search_options":{"search_context_size":"high"}}`)

	// An account native to the client protocol is forwarded verbatim: the
	// conversion toggle has nothing to do.
	native := &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolChat}}
	off, path, target, err := RewriteForAccount(false, DialectCompatible, biz.RequestProtocolCompatible, native, "", body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(off, body) {
		t.Fatalf("native rewrite changed body: %s", off)
	}
	if path != "/v1/chat/completions" || target != DialectCompatible {
		t.Fatalf("native path=%s target=%s", path, target)
	}

	// An Anthropic-only account that allows conversion cannot serve a Chat
	// client while the toggle is off: the candidate is skipped.
	anthOnly := &ent.UpstreamAccount{NativeProtocols: []string{biz.NativeProtocolMessages}, AllowConversion: true}
	if _, _, _, err := RewriteForAccount(false, DialectCompatible, biz.RequestProtocolCompatible, anthOnly, "", body); !errors.Is(err, errAccountProtocolUnsupported) {
		t.Fatalf("conversion off: err = %v, want errAccountProtocolUnsupported", err)
	}

	// With the toggle on the same account serves the request converted to
	// Messages.
	on, path, target, err := RewriteForAccount(true, DialectCompatible, biz.RequestProtocolCompatible, anthOnly, "", body)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/messages" {
		t.Fatalf("path = %s", path)
	}
	if target != DialectAnthropic {
		t.Fatalf("target = %s", target)
	}
	got := mustUnmarshal(t, on)
	if intVal(got["max_tokens"]) <= 0 {
		t.Fatal("enabled rewrite missing max_tokens")
	}
	sawSearch := false
	for _, rawT := range asSlice(got["tools"]) {
		if strVal(asMap(rawT)["type"]) == anthropicWebSearchType {
			sawSearch = true
		}
	}
	if !sawSearch {
		t.Fatalf("enabled rewrite dropped web_search: %s", on)
	}
}

func TestConvertAndForwardHTTP(t *testing.T) {
	userText := "forward-hi"
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","role":"assistant","model":"claude-up","content":[{"type":"web_search_tool_result","id":"ws_1","content":[{"url":"https://ex.test"}]},{"type":"text","text":"forward-ok","citations":[{"url":"https://ex.test"}]}],"usage":{"input_tokens":5,"output_tokens":3,"server_tool_use":{"web_search_requests":2}}}`))
	}))
	defer srv.Close()

	p := &ent.UpstreamAccount{BaseURL: srv.URL, NativeProtocols: []string{biz.NativeProtocolMessages}, AllowConversion: true}
	req := []byte(`{"model":"client-m","messages":[{"role":"system","content":"brief"},{"role":"user","content":"` + userText + `"}],"max_tokens":16,"web_search_options":{"search_context_size":"medium"}}`)
	encoded, path, target, err := RewriteForAccount(true, DialectCompatible, biz.RequestProtocolCompatible, p, "", req)
	if err != nil {
		t.Fatal(err)
	}
	prov := NewAnthropicProvider(p, "test-key")
	resp, err := prov.Forward(context.Background(), path, encoded, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	back, err := ApplyResponseConversion(true, target, DialectCompatible, raw)
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %s", gotPath)
	}
	up := mustUnmarshal(t, gotBody)
	if up["system"] != "brief" {
		t.Fatalf("upstream system = %#v", up["system"])
	}
	if !bytes.Contains(gotBody, []byte(userText)) {
		t.Fatalf("upstream missing user text: %s", gotBody)
	}
	sawSearch := false
	for _, rawT := range asSlice(up["tools"]) {
		if strVal(asMap(rawT)["type"]) == anthropicWebSearchType {
			sawSearch = true
		}
	}
	if !sawSearch {
		t.Fatalf("upstream missing web_search: %s", gotBody)
	}

	chat := mustUnmarshal(t, back)
	msg := asMap(asMap(asSlice(chat["choices"])[0])["message"])
	if flattenChatContent(msg["content"]) != "forward-ok" {
		t.Fatalf("rewritten text = %#v", msg["content"])
	}
	if extractWebSearchRequests(chat) != 2 {
		t.Fatalf("rewritten web_search_requests = %d (%s)", extractWebSearchRequests(chat), back)
	}
	if msg["citations"] == nil && msg["web_search_calls"] == nil {
		t.Fatalf("rewritten dropped citations/search: %s", back)
	}

	var acc Accumulator
	acc.Inspect(back)
	if acc.WebSearchRequests != 2 {
		t.Fatalf("accumulator web_search_requests = %d", acc.WebSearchRequests)
	}
	if acc.TokensIn != 5 || acc.TokensOut != 3 {
		t.Fatalf("acc tokens in=%d out=%d", acc.TokensIn, acc.TokensOut)
	}
}

func TestAccumulatorInspectWebSearchUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"anthropic server_tool_use", `{"usage":{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search_requests":3}}}`, 3},
		{"openai flat", `{"usage":{"prompt_tokens":1,"completion_tokens":1,"web_search_requests":4}}`, 4},
		{"responses nested", `{"response":{"usage":{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search_requests":5}}}}`, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Accumulator
			a.Inspect([]byte(tc.body))
			if a.WebSearchRequests != tc.want {
				t.Fatalf("got %d want %d", a.WebSearchRequests, tc.want)
			}
		})
	}
}

// TestRewriteForAccountEndpointPathOverride covers custom providers whose
// generation endpoint does not live under /v1/*: the account's declaration
// replaces the dialect default, and accounts that declare nothing keep it.
func TestRewriteForAccountEndpointPathOverride(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	cases := []struct {
		name     string
		account  *ent.UpstreamAccount
		client   Dialect
		protocol biz.RequestProtocol
		wantPath string
	}{
		{
			name: "no override keeps the dialect default",
			account: &ent.UpstreamAccount{
				NativeProtocols: []string{biz.NativeProtocolChat},
			},
			client:   DialectCompatible,
			protocol: biz.RequestProtocolCompatible,
			wantPath: "/v1/chat/completions",
		},
		{
			name: "empty map keeps the dialect default",
			account: &ent.UpstreamAccount{
				NativeProtocols: []string{biz.NativeProtocolChat},
				EndpointPaths:   map[string]any{},
			},
			client:   DialectCompatible,
			protocol: biz.RequestProtocolCompatible,
			wantPath: "/v1/chat/completions",
		},
		{
			name: "chat override wins",
			account: &ent.UpstreamAccount{
				NativeProtocols: []string{biz.NativeProtocolChat},
				EndpointPaths:   map[string]any{"chat_completions": "/v2/chat/completions"},
			},
			client:   DialectCompatible,
			protocol: biz.RequestProtocolCompatible,
			wantPath: "/v2/chat/completions",
		},
		{
			name: "an override for another protocol does not leak",
			account: &ent.UpstreamAccount{
				NativeProtocols: []string{biz.NativeProtocolChat},
				EndpointPaths:   map[string]any{"messages": "/v9/messages"},
			},
			client:   DialectCompatible,
			protocol: biz.RequestProtocolCompatible,
			wantPath: "/v1/chat/completions",
		},
		{
			name: "converted request uses the target protocol's override",
			account: &ent.UpstreamAccount{
				NativeProtocols: []string{biz.NativeProtocolMessages},
				AllowConversion: true,
				EndpointPaths:   map[string]any{"messages": "/v3/messages"},
			},
			client:   DialectCompatible,
			protocol: biz.RequestProtocolCompatible,
			wantPath: "/v3/messages",
		},
		{
			name: "unknown override value type is ignored",
			account: &ent.UpstreamAccount{
				NativeProtocols: []string{biz.NativeProtocolChat},
				EndpointPaths:   map[string]any{"chat_completions": 42},
			},
			client:   DialectCompatible,
			protocol: biz.RequestProtocolCompatible,
			wantPath: "/v1/chat/completions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, path, _, err := RewriteForAccount(true, tc.client, tc.protocol, tc.account, "", body)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if path != tc.wantPath {
				t.Fatalf("path = %q want %q", path, tc.wantPath)
			}
		})
	}
}
