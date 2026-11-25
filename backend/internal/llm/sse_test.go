package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsResponsesTerminalPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{
			name:    "completed",
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`),
			want:    true,
		},
		{
			name:    "incomplete",
			payload: []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"}}`),
			want:    true,
		},
		{
			name:    "failed",
			payload: []byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed"}}`),
			want:    true,
		},
		{
			name:    "completed event wrapper ignored",
			payload: []byte(`event: response.completed`),
			want:    false,
		},
		{
			name:    "created",
			payload: []byte(`{"type":"response.created","response":{"id":"resp_1"}}`),
			want:    false,
		},
		{
			name:    "done sentinel",
			payload: []byte(`[DONE]`),
			want:    false,
		},
		{
			name:    "invalid",
			payload: []byte(`not json`),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isResponsesTerminalPayload(tt.payload); got != tt.want {
				t.Fatalf("isResponsesTerminalPayload() = %v, want %v", got, tt.want)
			}
		})
	}
}

// runGatewayLikePipe mirrors the streaming /v1/responses path. Deciding
// whether the stream finished is the pipe's job now, so the caller reacts to
// errUpstreamStreamIncomplete instead of tracking terminal frames itself; the
// hook is left only to spot an upstream response.failed, which is a finished
// stream that failed rather than an unfinished one.
func runGatewayLikePipe(src string) (out bytes.Buffer, incomplete, upstreamFailed bool, err error) {
	acc := &Accumulator{}
	err = PipeAndExtractOpts(&out, strings.NewReader(src), acc, PipeOptions{
		OnFrame: func(payload []byte, a *Accumulator) error {
			var ev struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(payload, &ev) == nil && ev.Type == "response.failed" {
				upstreamFailed = true
			}
			return nil
		},
	})
	if errors.Is(err, errUpstreamStreamIncomplete) {
		incomplete, err = true, nil
		if out.Len() > 0 {
			writeStreamAbortTermination(&out, "responses", "req_123", acc.AssistantText.String())
		}
	}
	return
}

func countGatewayAbortFrames(s string) int {
	// The gateway-generated abort frame carries an explicit SSE event line.
	return strings.Count(s, "event: response.failed")
}

func TestNoDuplicateAbortOnTerminalResponseEvents(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "failed",
			src:  "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"too many\"}}}\n\n",
		},
		{
			name: "incomplete",
			src:  "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"status\":\"incomplete\"}}\n\n",
		},
		{
			name: "completed",
			src:  "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, incomplete, failed, err := runGatewayLikePipe(tt.src)
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			if incomplete {
				t.Fatal("a terminal event must not read as an unfinished stream")
			}
			if tt.name == "failed" && !failed {
				t.Fatal("expected response.failed to set upstreamFailed")
			}
			if tt.name != "failed" && failed {
				t.Fatalf("did not expect upstreamFailed for %s", tt.name)
			}
			if n := countGatewayAbortFrames(out.String()); n != 0 {
				t.Fatalf("expected 0 gateway abort frames for terminal event, got %d in %s", n, out.String())
			}
		})
	}
}

func TestAbortAppendedOnTruncatedResponsesStream(t *testing.T) {
	src := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	out, incomplete, failed, err := runGatewayLikePipe(src)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if !incomplete {
		t.Fatal("a stream that stopped before its terminal event must be reported as unfinished")
	}
	if failed {
		t.Fatal("truncated stream should not set upstreamFailed")
	}
	if n := countGatewayAbortFrames(out.String()); n != 1 {
		t.Fatalf("expected 1 gateway abort frame, got %d in %s", n, out.String())
	}
}

// Each case is a complete stream, closed the way its own dialect closes one —
// the pipe rejects a stream that stops without saying so, and these cases are
// about which deltas count as the first token.
func TestPipeAndExtractFirstTokenIncludesReasoningAndTools(t *testing.T) {
	const (
		doneChat      = "data: [DONE]\n\n"
		doneResponses = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"
		doneAnthropic = "data: {\"type\":\"message_stop\"}\n\n"
	)
	tests := []struct {
		name string
		src  string
	}{
		{
			name: "responses reasoning delta",
			src: "data: {\"type\":\"response.created\"}\n\n" +
				"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"thinking\"}\n\n" + doneResponses,
		},
		{
			name: "responses function call start",
			src:  "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n" + doneResponses,
		},
		{
			name: "chat reasoning content",
			src:  "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n" + doneChat,
		},
		{
			name: "chat tool call",
			src:  "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\n" + doneChat,
		},
		{
			name: "anthropic thinking delta",
			src:  "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thinking\"}}\n\n" + doneAnthropic,
		},
		{
			name: "anthropic tool args",
			src:  "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n" + doneAnthropic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := PipeAndExtractOpts(&out, bytes.NewBufferString(tt.src), &Accumulator{}, PipeOptions{
				OnFirstText: func() { calls++ },
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("OnFirstText calls = %d, want 1", calls)
			}
		})
	}
}

// OnFirstOutput anchors the decode window, so it must fire earlier than
// OnFirstText whenever the model starts producing something unreadable — an
// encrypted reasoning item is the case that matters: its tokens are counted
// in tokens_out but no text ever streams for them.
func TestPipeAndExtractFirstOutputFiresBeforeFirstText(t *testing.T) {
	var out bytes.Buffer
	var order []string
	src := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.in_progress\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"gAAAA\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	err := PipeAndExtractOpts(&out, bytes.NewBufferString(src), &Accumulator{}, PipeOptions{
		OnFirstOutput: func() { order = append(order, "output") },
		OnFirstText:   func() { order = append(order, "text") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "output" || order[1] != "text" {
		t.Fatalf("hook order = %v, want [output text]", order)
	}
}

// Lifecycle frames say nothing about the model, so neither hook may fire on
// them; the reasoning-summary part opening is the first thing that does.
func TestPipeAndExtractFirstOutputIgnoresLifecycleFrames(t *testing.T) {
	var out bytes.Buffer
	outputs, texts := 0, 0
	src := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.in_progress\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_part.added\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	err := PipeAndExtractOpts(&out, bytes.NewBufferString(src), &Accumulator{}, PipeOptions{
		OnFirstOutput: func() { outputs++ },
		OnFirstText:   func() { texts++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if outputs != 1 || texts != 0 {
		t.Fatalf("hooks = (output %d, text %d), want (1, 0)", outputs, texts)
	}
}

// A stream that only ever carries lifecycle frames — the shape a relay
// produces while it assembles an answer elsewhere — must not move either
// hook, so the decode window stays anchored to the delivery itself.
func TestPipeAndExtractFirstOutputIgnoresPureLifecycleStream(t *testing.T) {
	var out bytes.Buffer
	outputs, texts := 0, 0
	src := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.in_progress\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	err := PipeAndExtractOpts(&out, bytes.NewBufferString(src), &Accumulator{}, PipeOptions{
		OnFirstOutput: func() { outputs++ },
		OnFirstText:   func() { texts++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if outputs != 0 || texts != 0 {
		t.Fatalf("hooks = (output %d, text %d), want (0, 0)", outputs, texts)
	}
}

func TestPipeAndExtractFirstTokenIgnoresMetadata(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	src := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.in_progress\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"
	err := PipeAndExtractOpts(&out, bytes.NewBufferString(src), &Accumulator{}, PipeOptions{
		OnFirstText: func() { calls++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("OnFirstText calls = %d, want 0", calls)
	}
}

func TestPipeChatAsAnthropicFirstTokenIncludesReasoningAndTools(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		// Each fixture ends with [DONE]: pipeChatAsAnthropic now rejects a
		// stream that stops without any terminal signal, and these cases are
		// about which deltas count as the first token, not about truncation.
		{
			name: "reasoning content",
			src:  "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\ndata: [DONE]\n\n",
		},
		{
			name: "tool call start",
			src:  "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"\"}}]}}]}\n\ndata: [DONE]\n\n",
		},
		{
			name: "tool call args",
			src:  "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\ndata: [DONE]\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			emitter := newAnthropicEmitter(rec, "model")
			calls := 0
			err := pipeChatAsAnthropic(strings.NewReader(tt.src), emitter, &Accumulator{}, true, nil, func() {
				calls++
			}, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("onFirstText calls = %d, want 1", calls)
			}
		})
	}
}

// The pipe is the single place that decides whether a relayed stream finished.
// A 2xx stream that stops mid-answer reaches this layer looking exactly like a
// completed one — HTTP/2 END_STREAM and close-delimited bodies both surface as
// a clean EOF — and forwarding it as success shipped truncated answers with a
// 200 and billed them. Every dialect this gateway relays is covered here
// because they all share one predicate.
func TestPipeReportsStreamsThatStopWithoutFinishing(t *testing.T) {
	t.Parallel()
	const (
		chatDelta = `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"
		respDelta = `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"
		anthDelta = `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n"
	)

	complete := map[string]string{
		"chat closes with [DONE]":        chatDelta + "data: [DONE]\n\n",
		"chat closes with finish_reason": chatDelta + `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"chat closes with a usage frame": chatDelta + `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n",
		"responses closes completed":     respDelta + `data: {"type":"response.completed","response":{"id":"r"}}` + "\n\n",
		"responses closes incomplete":    respDelta + `data: {"type":"response.incomplete","response":{"id":"r"}}` + "\n\n",
		"responses closes failed":        respDelta + `data: {"type":"response.failed","response":{"id":"r"}}` + "\n\n",
		"anthropic closes message_stop":  anthDelta + `data: {"type":"message_stop"}` + "\n\n",
		// Never a stream to begin with: relays that ignore stream=true answer
		// with one plain JSON object. That is a separate, pre-existing shape
		// with its own handling, not a channel that truncates.
		"not an SSE body at all": `{"id":"x","object":"chat.completion"}`,
	}
	for name, src := range complete {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := PipeAndExtractOpts(&out, strings.NewReader(src), &Accumulator{}, PipeOptions{}); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}

	truncated := map[string]string{
		"chat stops mid-answer":      chatDelta,
		"responses stops mid-answer": respDelta,
		"anthropic stops mid-answer": anthDelta,
		// Usage repeated on a content-carrying chunk is not an ending:
		// vLLM's continuous_usage_stats does this on every frame, and taking
		// it as terminal would let truncation through for those upstreams.
		"chat repeats usage on every chunk": `data: {"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n",
	}
	for name, src := range truncated {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := PipeAndExtractOpts(&out, strings.NewReader(src), &Accumulator{}, PipeOptions{})
			if !errors.Is(err, errUpstreamStreamIncomplete) {
				t.Errorf("err = %v, want errUpstreamStreamIncomplete — otherwise a half answer ships as 200 and bills", err)
			}
			// The bytes still reach the client; the caller decides what to do
			// with a stream it now knows was cut short.
			if out.Len() == 0 {
				t.Error("frames seen before the cut should still have been forwarded")
			}
		})
	}
}

func TestPipeAndExtractAfterWrite(t *testing.T) {
	var out bytes.Buffer
	calls := 0

	err := PipeAndExtractOpts(&out, bytes.NewBufferString("data: {}\n\ndata: [DONE]\n\n"), &Accumulator{}, PipeOptions{
		AfterWrite: func() {
			calls++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two data: lines and the blank line after each — AfterWrite fires per line.
	if calls != 4 {
		t.Fatalf("AfterWrite calls = %d, want 4", calls)
	}
}

// AfterContentFrame must fire only on real content data: frames, not on SSE
// keepalive comments, blank separators, or the terminal [DONE] sentinel — this
// is what lets the chunk-idle (stall) timer ignore upstream keepalives.
func TestPipeAndExtractAfterContentFrame(t *testing.T) {
	var out bytes.Buffer
	content, anyLine := 0, 0

	// content frame, blank, keepalive comment, blank, terminal sentinel, blank.
	src := "data: {\"a\":1}\n\n: keepalive\n\ndata: [DONE]\n\n"
	err := PipeAndExtractOpts(&out, bytes.NewBufferString(src), &Accumulator{}, PipeOptions{
		AfterWrite:        func() { anyLine++ },
		AfterContentFrame: func() { content++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != 1 {
		t.Fatalf("AfterContentFrame calls = %d, want 1 (only the content frame)", content)
	}
	if anyLine <= content {
		t.Fatalf("AfterWrite (%d) should fire on more lines than AfterContentFrame (%d)", anyLine, content)
	}
}

func TestStreamIdleTimeoutCancelsWithCause(t *testing.T) {
	ctx, idle := withStreamIdleTimeout(context.Background(), time.Millisecond)
	defer idle.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not canceled")
	}
	if !streamIdleTimedOut(ctx) {
		t.Fatalf("streamIdleTimedOut = false, cause = %v", context.Cause(ctx))
	}
}

func TestWriteStreamAbortTerminationForResponses(t *testing.T) {
	var out bytes.Buffer

	writeStreamAbortTermination(&out, "responses", "req_123", "partial answer")

	got := out.String()
	for _, want := range []string{
		"event: response.failed",
		`"status":"failed"`,
		`"code":"upstream_error"`,
		`"request_id":"req_123"`,
		`"text":"partial answer"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("abort termination missing %q in %s", want, got)
		}
	}
}

func TestWriteStreamAbortTerminationForChat(t *testing.T) {
	var out bytes.Buffer

	writeStreamAbortTermination(&out, "chat_completions", "req_123", "")

	got := out.String()
	for _, want := range []string{
		`"error"`,
		`"code":"upstream_error"`,
		`"request_id":"req_123"`,
		"data: [DONE]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("abort termination missing %q in %s", want, got)
		}
	}
}

func TestWriteStreamAbortTerminationForAnthropic(t *testing.T) {
	var out bytes.Buffer

	writeStreamAbortTermination(&out, "messages", "req_123", "")

	got := out.String()
	for _, want := range []string{
		"event: error",
		`"type":"api_error"`,
		`"request_id":"req_123"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("abort termination missing %q in %s", want, got)
		}
	}
}

func TestResponsesAggregatorCapturesTerminalFrame(t *testing.T) {
	agg := newResponsesAggregator()
	stream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n"
	if _, err := agg.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := agg.final()
	want := `{"id":"resp_1","status":"completed","output":[]}`
	if string(got) != want {
		t.Fatalf("final() = %s, want %s", got, want)
	}
}

func TestResponsesAggregatorSplitAcrossWrites(t *testing.T) {
	agg := newResponsesAggregator()
	frame := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_2\",\"status\":\"failed\"}}\n\n"
	// Byte-by-byte writes exercise the partial-line buffering.
	for i := 0; i < len(frame); i++ {
		if _, err := agg.Write([]byte(frame[i : i+1])); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := agg.final()
	want := `{"id":"resp_2","status":"failed"}`
	if string(got) != want {
		t.Fatalf("final() = %s, want %s", got, want)
	}
}

func TestResponsesAggregatorPlainJSONFallback(t *testing.T) {
	agg := newResponsesAggregator()
	// Upstream ignored stream=true and answered with a plain JSON body.
	if _, err := agg.Write([]byte(`{"id":"resp_3","object":"response","status":"completed","output":[]}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := agg.final()
	want := `{"id":"resp_3","object":"response","status":"completed","output":[]}`
	if string(got) != want {
		t.Fatalf("final() = %s, want %s", got, want)
	}
}

func TestResponsesAggregatorEmpty(t *testing.T) {
	agg := newResponsesAggregator()
	_, _ = agg.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
	if got := agg.final(); got != nil {
		t.Fatalf("final() = %s, want nil", got)
	}
}
