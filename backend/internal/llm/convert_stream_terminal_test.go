package llm

import (
	"strings"
	"testing"
)

// OpenAI reports usage on a trailing chunk with an empty choices array, after
// the chunk that carried finish_reason. The gateway bills from the converted
// stream, so a terminal frame written before that chunk arrives bills the
// request at zero tokens — and the assistant's text has to reach the output
// array too, because a non-streaming /v1/responses call is answered out of
// exactly that object.
func TestChatToResponsesTerminalFrameCarriesUsageAndText(t *testing.T) {
	got := convertStreamString(t, DialectCompatible, DialectOpenAI, sse(
		`{"id":"c1","model":"m","choices":[{"delta":{"role":"assistant","content":"The answer is 42."}}]}`,
		`{"id":"c1","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c1","model":"m","choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":567}}`,
		`[DONE]`,
	))

	var terminal map[string]any
	completedSeen := 0
	for _, f := range sseFrames(t, got) {
		if strVal(f["type"]) == "response.completed" {
			completedSeen++
			terminal = asMap(f["response"])
		}
	}
	if completedSeen != 1 {
		t.Fatalf("want exactly one response.completed, got %d\n%s", completedSeen, got)
	}
	u := asMap(terminal["usage"])
	if intVal(u["input_tokens"]) != 1234 || intVal(u["output_tokens"]) != 567 {
		t.Errorf("terminal usage = %v, want input 1234 / output 567", u)
	}
	out := asSlice(terminal["output"])
	if len(out) != 1 {
		t.Fatalf("terminal output = %v, want one message item", out)
	}
	item := asMap(out[0])
	if strVal(item["type"]) != "message" || strVal(item["role"]) != "assistant" {
		t.Errorf("output item = %v, want an assistant message", item)
	}
	content := asSlice(item["content"])
	if len(content) != 1 || strVal(asMap(content[0])["text"]) != "The answer is 42." {
		t.Errorf("message content = %v, want the streamed text", content)
	}

	// The same stream has to bill, which is what reads it in production.
	acc := &Accumulator{}
	for _, line := range strings.Split(got, "\n") {
		if p, ok := sseDataPayload([]byte(line + "\n")); ok {
			acc.Inspect(p)
		}
	}
	if !acc.HasUsage || acc.TokensIn != 1234 || acc.TokensOut != 567 {
		t.Errorf("billing read %d/%d (hasUsage=%v), want 1234/567", acc.TokensIn, acc.TokensOut, acc.HasUsage)
	}
}

// A Responses source terminates on response.completed and closes; it has no
// sentinel of its own. A chat client blocking on [DONE] hangs until its read
// timeout unless the converter supplies one.
func TestResponsesToChatEmitsDoneExactlyOnce(t *testing.T) {
	got := convertStreamString(t, DialectOpenAI, DialectCompatible, sse(
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":5,"output_tokens":2}}}`,
	))
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] appeared %d times, want 1\n%s", n, got)
	}
}

// A source that sends both [DONE] and then EOFs must not terminate twice.
func TestChatToResponsesTerminalFrameIsIdempotent(t *testing.T) {
	got := convertStreamString(t, DialectCompatible, DialectOpenAI, sse(
		`{"id":"c","model":"m","choices":[{"delta":{"content":"x"}}]}`,
		`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))
	completed, itemDone := 0, 0
	for _, f := range sseFrames(t, got) {
		switch strVal(f["type"]) {
		case "response.completed":
			completed++
		case "response.output_item.done":
			itemDone++
		}
	}
	if completed != 1 || itemDone != 1 {
		t.Fatalf("completed=%d output_item.done=%d, want 1 each\n%s", completed, itemDone, got)
	}
}

// An Anthropic server_tool_use block streams its query as input_json_delta
// exactly like a real tool call does. Letting it consume an output slot shifts
// every later call's arguments onto the wrong call.
func TestAnthropicToResponsesServerToolDoesNotStealToolSlot(t *testing.T) {
	got := convertStreamString(t, DialectAnthropic, DialectOpenAI, sse(
		`{"type":"message_start","message":{"id":"msg_1","model":"claude"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"golang\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"calc"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":9,"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	))

	var call map[string]any
	searchItems := 0
	for _, f := range sseFrames(t, got) {
		if strVal(f["type"]) != "response.completed" {
			continue
		}
		for _, raw := range asSlice(asMap(f["response"])["output"]) {
			item := asMap(raw)
			switch strVal(item["type"]) {
			case "function_call":
				call = item
			case "server_tool_use", "web_search_call":
				searchItems++
			}
		}
	}
	if call == nil {
		t.Fatalf("no function_call in terminal output\n%s", got)
	}
	if strVal(call["name"]) != "calc" || strVal(call["arguments"]) != `{"a":1}` {
		t.Errorf("function_call = %v, want calc holding its own arguments", call)
	}
	if searchItems != 1 {
		t.Errorf("terminal output carries %d search items, want 1\n%s", searchItems, got)
	}
}

// A chat client's agent loop branches on tool_calls; reporting stop makes it
// treat the empty message as the final answer and never run the tool.
func TestResponsesToChatReportsToolCallsFinishReason(t *testing.T) {
	got := convertStreamString(t, DialectOpenAI, DialectCompatible, sse(
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"calc"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":1}"}`,
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":5,"output_tokens":2}}}`,
	))
	var fr string
	for _, f := range sseFrames(t, got) {
		for _, raw := range asSlice(f["choices"]) {
			if v := strVal(asMap(raw)["finish_reason"]); v != "" {
				fr = v
			}
		}
	}
	if fr != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls\n%s", fr, got)
	}
}
