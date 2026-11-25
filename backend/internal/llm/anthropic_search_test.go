package llm

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPipeChatAsAnthropicPreservesWebSearchAndCitations(t *testing.T) {
	citeURL := "https://example.test/cite"
	phrase := "cited-answer"
	src := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"` + phrase + `","citations":[{"url":"` + citeURL + `","title":"Doc"}],"web_search_calls":[{"type":"web_search_call","id":"ws_1","content":[{"url":"` + citeURL + `"}]}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	t.Run("stream", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e := newAnthropicEmitter(rec, "claude-test")
		e.writeSSEHeaders()
		acc := &Accumulator{}
		if err := pipeChatAsAnthropic(strings.NewReader(src), e, acc, true, nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "web_search_tool_result") {
			t.Fatalf("stream missing web_search_tool_result:\n%s", body)
		}
		if !strings.Contains(body, citeURL) {
			t.Fatalf("stream missing citation url %q:\n%s", citeURL, body)
		}
		if !strings.Contains(body, phrase) {
			t.Fatalf("stream missing text %q:\n%s", phrase, body)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e := newAnthropicEmitter(rec, "claude-test")
		acc := &Accumulator{}
		if err := pipeChatAsAnthropic(strings.NewReader(src), e, acc, false, nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		e.flushNonStream(acc)
		body := rec.Body.String()
		if !strings.Contains(body, "web_search_tool_result") {
			t.Fatalf("non-stream missing web_search_tool_result:\n%s", body)
		}
		if !strings.Contains(body, citeURL) {
			t.Fatalf("non-stream missing citation url %q:\n%s", citeURL, body)
		}
		if !strings.Contains(body, phrase) {
			t.Fatalf("non-stream missing text %q:\n%s", phrase, body)
		}
	})
}
