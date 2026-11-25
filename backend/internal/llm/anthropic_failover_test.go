package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// A buffered (stream:false) reply is assembled in the emitter and the
// accumulator and only written at the very end, so a provider that answers
// 2xx and then dies mid-answer leaves its text, its half-built tool arguments
// and its reported token counts behind. HandleMessages fails over to the next
// provider in that situation, and unless both are empty again the caller gets
// two answers spliced together and a bill for output nobody delivered.
//
// Scope, so nobody reads more into a green run than it earns: this exercises
// the emitter and accumulator directly and performs the per-attempt reset
// itself, because HandleMessages cannot be driven without a live gateway.
// It proves the reset produces a clean delivery and receipt —
// it does not prove HandleMessages still performs one. Deleting the reset in
// anthropic.go leaves this test green.
func TestFailoverDeliversOnlyTheWinningAttempt(t *testing.T) {
	t.Parallel()

	// Provider A: text plus a half-built tool call, cut off before [DONE] and
	// before any finish_reason — the shape of a connection dropped mid-answer.
	// It still reported usage, which a stream carrying include_usage does.
	dead := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"HALF-ANSWER-FROM-A"}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"tool_a","arguments":"{\"partial\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":900,"completion_tokens":800}}`,
		``,
	}, "\n")

	// Provider B: a complete, smaller answer.
	alive := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"FULL-ANSWER-FROM-B"}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_b","function":{"name":"tool_b","arguments":"{\"ok\":true}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	acc := &Accumulator{}

	// Attempt 1 dies after filling both the emitter and the accumulator.
	first := newAnthropicEmitter(httptest.NewRecorder(), "claude-test")
	_ = pipeChatAsAnthropic(strings.NewReader(dead), first, acc, false, nil, nil, nil, nil, nil)
	if acc.AssistantText.Len() == 0 {
		t.Fatal("fixture is not exercising anything: attempt 1 accumulated no text")
	}

	// What HandleMessages does before the next attempt.
	*acc = Accumulator{}
	rec := httptest.NewRecorder()
	second := newAnthropicEmitter(rec, "claude-test")
	if err := pipeChatAsAnthropic(strings.NewReader(alive), second, acc, false, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	second.flushNonStream(acc)

	body := rec.Body.String()
	if strings.Contains(body, "HALF-ANSWER-FROM-A") {
		t.Errorf("attempt 1's text was delivered to the client:\n%s", body)
	}
	if strings.Contains(body, "call_a") || strings.Contains(body, "tool_a") || strings.Contains(body, "partial") {
		t.Errorf("attempt 1's tool call was delivered to the client:\n%s", body)
	}
	if !strings.Contains(body, "FULL-ANSWER-FROM-B") || !strings.Contains(body, "call_b") {
		t.Errorf("attempt 2's answer is missing:\n%s", body)
	}

	// The usage block the caller sees and the numbers billing reads are the
	// same accumulator, so a leaked count is both a wrong receipt and a wrong
	// charge. Attempt 1 reported 900/800; only attempt 2's 10/7 may survive.
	var parsed struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if parsed.Usage.InputTokens != 10 || parsed.Usage.OutputTokens != 7 {
		t.Errorf("usage = %d in / %d out, want 10/7 — attempt 1's 900/800 leaked into the receipt and the charge",
			parsed.Usage.InputTokens, parsed.Usage.OutputTokens)
	}
	if acc.TokensOut != 7 {
		t.Errorf("acc.TokensOut = %d, want 7 (this is what chargeAndLog bills)", acc.TokensOut)
	}
	if got := acc.AssistantText.String(); got != "FULL-ANSWER-FROM-B" {
		t.Errorf("acc.AssistantText = %q — this is what the output policy scan reads", got)
	}
}

// A 2xx stream that just stops is indistinguishable from a finished one at the
// transport layer: HTTP/2 END_STREAM and a close-delimited body both arrive as
// a clean EOF. Treating that as success delivered a truncated answer stamped
// 200, with a fabricated stop_reason, and charged for it. Any one of the three
// ways the protocol signals completion counts; a stream giving none of them is
// reported incomplete so the caller can fail over.
func TestPipeRejectsAStreamThatStopsWithoutSayingSo(t *testing.T) {
	t.Parallel()
	const text = `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"

	for name, src := range map[string]string{
		"[DONE]":        text + "data: [DONE]\n\n",
		"finish_reason": text + `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		// The real shape of OpenAI's trailing usage chunk: choices is empty.
		"trailing usage frame": text + `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n",
	} {
		t.Run("complete via "+name, func(t *testing.T) {
			e := newAnthropicEmitter(httptest.NewRecorder(), "claude-test")
			if err := pipeChatAsAnthropic(strings.NewReader(src), e, &Accumulator{}, false, nil, nil, nil, nil, nil); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}

	t.Run("truncated", func(t *testing.T) {
		e := newAnthropicEmitter(httptest.NewRecorder(), "claude-test")
		err := pipeChatAsAnthropic(strings.NewReader(text), e, &Accumulator{}, false, nil, nil, nil, nil, nil)
		if !errors.Is(err, errUpstreamStreamIncomplete) {
			t.Errorf("err = %v, want errUpstreamStreamIncomplete — otherwise a half answer ships as 200", err)
		}
	})

	// Repeating usage on every chunk is a real upstream shape (vLLM's
	// continuous_usage_stats, several relays), and mergeUsage is built for it.
	// Only an empty-choices frame ends a stream, or the check would pass on
	// frame one and catch nothing for exactly those upstreams.
	t.Run("per-frame usage is not an ending", func(t *testing.T) {
		src := `data: {"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n"
		e := newAnthropicEmitter(httptest.NewRecorder(), "claude-test")
		err := pipeChatAsAnthropic(strings.NewReader(src), e, &Accumulator{}, false, nil, nil, nil, nil, nil)
		if !errors.Is(err, errUpstreamStreamIncomplete) {
			t.Errorf("err = %v, want errUpstreamStreamIncomplete — usage alongside content is not a terminal frame", err)
		}
	})

	// A relay that ignores stream:true and answers with one plain JSON object
	// has no data: lines at all. That predates this check and stays as it was,
	// rather than turning every such request into a channel failure.
	t.Run("not a stream at all", func(t *testing.T) {
		e := newAnthropicEmitter(httptest.NewRecorder(), "claude-test")
		err := pipeChatAsAnthropic(strings.NewReader(`{"id":"x","choices":[]}`), e, &Accumulator{}, false, nil, nil, nil, nil, nil)
		if err != nil {
			t.Errorf("err = %v, want nil — a non-SSE body is a separate, pre-existing condition", err)
		}
	})
}

// Arming SSE commits the status line immediately. Holding it until the first
// model event made the client's first packet wait out prefill. After this
// point the response is written, so a later failure is an in-stream abort
// rather than a failover onto another provider.
func TestArmingSSECommitsHeadersImmediately(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	e := newAnthropicEmitter(c.Writer, "claude-test")
	e.writeSSEHeaders()

	if !c.Writer.Written() {
		t.Fatal("SSE status line was not committed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}

	e.writeEvent("ping", map[string]any{})
	if !strings.Contains(rec.Body.String(), "event: ping") {
		t.Fatalf("first event missing from body: %s", rec.Body.String())
	}
}

// An answer can legitimately be empty: finish_reason with no content, or an
// output-policy block that hits the very first frame. Deferring the headers
// means nothing has been written when the stream ends, and terminating on
// "have the headers gone out" instead of "did the client ask for a stream"
// would send a bodyless 200 that no SDK can parse — while the request still
// records as ok and bills.
func TestEmptyStreamStillTerminatesProperly(t *testing.T) {
	t.Parallel()

	t.Run("upstream finishes with no content", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e := newAnthropicEmitter(rec, "claude-test")
		e.writeSSEHeaders()
		src := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
		if err := pipeChatAsAnthropic(strings.NewReader(src), e, &Accumulator{}, true, nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		assertWellFormedStream(t, rec.Body.String(), rec.Header().Get("Content-Type"))
	})

	t.Run("terminated before any content", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e := newAnthropicEmitter(rec, "claude-test")
		e.writeSSEHeaders()
		// What the output-policy hook does when it blocks on the first frame.
		e.finish("end_turn")
		assertWellFormedStream(t, rec.Body.String(), rec.Header().Get("Content-Type"))
	})
}

func assertWellFormedStream(t *testing.T, body, contentType string) {
	t.Helper()
	if contentType != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", contentType)
	}
	if body == "" {
		t.Fatal("empty body: the client gets a bodyless 200 it cannot parse")
	}
	for _, want := range []string{"message_start", "message_delta", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %s:\n%s", want, body)
		}
	}
}
