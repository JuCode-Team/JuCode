package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// sseFrames parses a converted stream back into its data payloads. Frames that
// are not JSON (the [DONE] sentinel) come back as nil so the count of real
// events stays meaningful.
func sseFrames(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("frame is not JSON: %s", payload)
		}
		out = append(out, obj)
	}
	return out
}

func convertStreamString(t *testing.T, from, to Dialect, src string) string {
	t.Helper()
	var dst bytes.Buffer
	if err := ConvertStream(from, to, strings.NewReader(src), &dst); err != nil {
		t.Fatalf("convert %s → %s: %v", from, to, err)
	}
	return dst.String()
}

// convertTruncatedStreamString converts a source that stops mid-answer. The
// converter still writes whatever terminal frames the target's clients need in
// order not to hang — for a Chat target that includes [DONE], which is
// indistinguishable from a real ending once it is on the wire. So it also has
// to say so out of band, and that is what the caller bills on.
func convertTruncatedStreamString(t *testing.T, from, to Dialect, src string) string {
	t.Helper()
	var dst bytes.Buffer
	err := ConvertStream(from, to, strings.NewReader(src), &dst)
	if !errors.Is(err, errUpstreamStreamIncomplete) {
		t.Fatalf("convert %s → %s: err = %v, want errUpstreamStreamIncomplete — a truncated source reported as clean is billed as a delivered answer", from, to, err)
	}
	return dst.String()
}

func sse(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: ")
		b.WriteString(l)
		b.WriteString("\n\n")
	}
	return b.String()
}

// A text block followed by two parallel tool calls has to come out as three
// separate Anthropic blocks, each opened and closed exactly once. The previous
// converter streamed everything at index 0 and never closed a tool block.
func TestChatToAnthropicBlockLifecycle(t *testing.T) {
	got := convertStreamString(t, DialectCompatible, DialectAnthropic, sse(
		`{"id":"cmpl_1","model":"m","choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"alpha","arguments":"{\"x\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"beta","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		`[DONE]`,
	))

	starts := map[int]string{}
	stops := map[int]int{}
	argsByIndex := map[int]string{}
	terminals := map[string]int{}
	for _, f := range sseFrames(t, got) {
		typ := strVal(f["type"])
		idx := intVal(f["index"])
		switch typ {
		case "content_block_start":
			if _, dup := starts[idx]; dup {
				t.Fatalf("index %d started twice:\n%s", idx, got)
			}
			starts[idx] = strVal(asMap(f["content_block"])["type"])
		case "content_block_stop":
			stops[idx]++
		case "content_block_delta":
			d := asMap(f["delta"])
			if strVal(d["type"]) == "input_json_delta" {
				argsByIndex[idx] += strVal(d["partial_json"])
			}
		case "message_delta", "message_stop":
			terminals[typ]++
		}
	}

	if len(starts) != 3 {
		t.Fatalf("want 3 blocks, got %v:\n%s", starts, got)
	}
	if starts[0] != "text" || starts[1] != "tool_use" || starts[2] != "tool_use" {
		t.Fatalf("block types = %v:\n%s", starts, got)
	}
	for idx := range starts {
		if stops[idx] != 1 {
			t.Fatalf("block %d stopped %d times:\n%s", idx, stops[idx], got)
		}
	}
	if argsByIndex[1] != `{"x":1}` {
		t.Fatalf("first tool arguments = %q:\n%s", argsByIndex[1], got)
	}
	if argsByIndex[2] != `{}` {
		t.Fatalf("second tool arguments = %q:\n%s", argsByIndex[2], got)
	}
	// [DONE] after a finish_reason must not produce a second terminal pair.
	if terminals["message_delta"] != 1 || terminals["message_stop"] != 1 {
		t.Fatalf("terminal frames = %v:\n%s", terminals, got)
	}
}

// Interleaved thinking and text must not share one block: a block keeps its
// type for life, so the converter has to close and reopen.
func TestChatToAnthropicSplitsThinkingFromText(t *testing.T) {
	got := convertStreamString(t, DialectCompatible, DialectAnthropic, sse(
		`{"id":"c","model":"m","choices":[{"delta":{"reasoning_content":"pondering"}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	))
	blockTypes := map[int]string{}
	for _, f := range sseFrames(t, got) {
		if strVal(f["type"]) == "content_block_start" {
			blockTypes[intVal(f["index"])] = strVal(asMap(f["content_block"])["type"])
		}
		if strVal(f["type"]) == "content_block_delta" {
			d := asMap(f["delta"])
			idx := intVal(f["index"])
			if strVal(d["type"]) == "thinking_delta" && blockTypes[idx] != "thinking" {
				t.Fatalf("thinking delta on %s block %d:\n%s", blockTypes[idx], idx, got)
			}
			if strVal(d["type"]) == "text_delta" && blockTypes[idx] != "text" {
				t.Fatalf("text delta on %s block %d:\n%s", blockTypes[idx], idx, got)
			}
		}
	}
	if len(blockTypes) != 2 {
		t.Fatalf("want thinking + text blocks, got %v:\n%s", blockTypes, got)
	}
	var thinkingStart map[string]any
	for _, f := range sseFrames(t, got) {
		if strVal(f["type"]) == "content_block_start" && strVal(asMap(f["content_block"])["type"]) == "thinking" {
			thinkingStart = asMap(f["content_block"])
			break
		}
	}
	if _, ok := thinkingStart["signature"]; !ok {
		t.Fatalf("thinking content_block_start missing signature:\n%s", got)
	}
}

func TestResponsesToAnthropicEmitsSignatureDelta(t *testing.T) {
	got := convertStreamString(t, DialectOpenAI, DialectAnthropic, sse(
		`{"type":"response.created","response":{"id":"r","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"plan"}`,
		`{"type":"response.reasoning_text.done","output_index":0}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-sig"}}`,
		`{"type":"response.output_text.delta","delta":"ok"}`,
		`{"type":"response.completed","response":{"status":"completed"}}`,
	))
	var sawStart, sawSig, sawStop bool
	startIdx, sigIdx, stopIdx := -1, -1, -1
	for i, f := range sseFrames(t, got) {
		switch strVal(f["type"]) {
		case "content_block_start":
			if strVal(asMap(f["content_block"])["type"]) == "thinking" {
				sawStart = true
				startIdx = i
				if _, ok := asMap(f["content_block"])["signature"]; !ok {
					t.Fatalf("thinking start missing signature:\n%s", got)
				}
			}
		case "content_block_delta":
			if strVal(asMap(f["delta"])["type"]) == "signature_delta" {
				sawSig = true
				sigIdx = i
				if strVal(asMap(f["delta"])["signature"]) != "enc-sig" {
					t.Fatalf("signature_delta = %#v\n%s", f["delta"], got)
				}
			}
		case "content_block_stop":
			if sawStart && !sawStop && startIdx >= 0 && i > startIdx {
				sawStop = true
				stopIdx = i
			}
		}
	}
	if !sawStart || !sawSig || !sawStop {
		t.Fatalf("start=%v sig=%v stop=%v\n%s", sawStart, sawSig, sawStop, got)
	}
	if !(startIdx < sigIdx && sigIdx < stopIdx) {
		t.Fatalf("order start=%d sig=%d stop=%d\n%s", startIdx, sigIdx, stopIdx, got)
	}
}

// Responses tool calls used to be dropped wholesale on the way to Anthropic.
func TestResponsesToAnthropicCarriesToolCall(t *testing.T) {
	got := convertStreamString(t, DialectOpenAI, DialectAnthropic, sse(
		`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_z","name":"lookup"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":1}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"q\":1}"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":2}}}`,
	))

	var toolBlock map[string]any
	args := ""
	stopReason := ""
	for _, f := range sseFrames(t, got) {
		switch strVal(f["type"]) {
		case "content_block_start":
			if b := asMap(f["content_block"]); strVal(b["type"]) == "tool_use" {
				toolBlock = b
			}
		case "content_block_delta":
			if d := asMap(f["delta"]); strVal(d["type"]) == "input_json_delta" {
				args += strVal(d["partial_json"])
			}
		case "message_delta":
			stopReason = strVal(asMap(f["delta"])["stop_reason"])
		}
	}
	if toolBlock == nil {
		t.Fatalf("tool_use block missing:\n%s", got)
	}
	if strVal(toolBlock["id"]) != "call_z" || strVal(toolBlock["name"]) != "lookup" {
		t.Fatalf("tool block = %v:\n%s", toolBlock, got)
	}
	// The done frame repeats the full arguments; emitting them again would
	// double the JSON.
	if args != `{"q":1}` {
		t.Fatalf("arguments = %q:\n%s", args, got)
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q:\n%s", stopReason, got)
	}
}

// Anthropic numbers content blocks across text and tools alike; chat clients
// accumulate tool calls by a dense index of their own.
func TestAnthropicToChatDenseToolIndex(t *testing.T) {
	got := convertStreamString(t, DialectAnthropic, DialectCompatible, sse(
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"sure"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"run"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	))
	seen := false
	for _, f := range sseFrames(t, got) {
		for _, rawCh := range asSlice(f["choices"]) {
			delta := asMap(asMap(rawCh)["delta"])
			for _, rawTC := range asSlice(delta["tool_calls"]) {
				seen = true
				if idx := intVal(asMap(rawTC)["index"]); idx != 0 {
					t.Fatalf("chat tool index = %d, want 0:\n%s", idx, got)
				}
			}
		}
	}
	if !seen {
		t.Fatalf("tool call dropped:\n%s", got)
	}
}

// A converted chat tool call is unusable unless the client is told the call id
// and function name, which only travel on the output item frame.
func TestChatToResponsesAnnouncesToolItem(t *testing.T) {
	got := convertStreamString(t, DialectCompatible, DialectOpenAI, sse(
		`{"id":"cmpl_1","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_q","function":{"name":"search","arguments":"{\"q\":2}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
	))
	var added, completedOutput map[string]any
	for _, f := range sseFrames(t, got) {
		switch strVal(f["type"]) {
		case "response.output_item.added":
			added = asMap(f["item"])
		case "response.completed":
			for _, raw := range asSlice(asMap(f["response"])["output"]) {
				if item := asMap(raw); strVal(item["type"]) == "function_call" {
					completedOutput = item
				}
			}
		}
	}
	if added == nil || strVal(added["name"]) != "search" || strVal(added["call_id"]) != "call_q" {
		t.Fatalf("output_item.added = %v:\n%s", added, got)
	}
	if completedOutput == nil || strVal(completedOutput["arguments"]) != `{"q":2}` {
		t.Fatalf("completed output = %v:\n%s", completedOutput, got)
	}
}

// Parallel Responses tool calls are keyed on output_index; collapsing them onto
// index 0 concatenates two argument streams into invalid JSON.
func TestResponsesToChatParallelToolIndices(t *testing.T) {
	got := convertStreamString(t, DialectOpenAI, DialectCompatible, sse(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"a","name":"one"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"b","name":"two"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"i\":0}"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"i\":1}"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	))
	args := map[int]string{}
	names := map[int]string{}
	for _, f := range sseFrames(t, got) {
		for _, rawCh := range asSlice(f["choices"]) {
			delta := asMap(asMap(rawCh)["delta"])
			for _, rawTC := range asSlice(delta["tool_calls"]) {
				tc := asMap(rawTC)
				idx := intVal(tc["index"])
				fn := asMap(tc["function"])
				if n := strVal(fn["name"]); n != "" {
					names[idx] = n
				}
				args[idx] += strVal(fn["arguments"])
			}
		}
	}
	if names[0] != "one" || names[1] != "two" {
		t.Fatalf("tool names = %v:\n%s", names, got)
	}
	if args[0] != `{"i":0}` || args[1] != `{"i":1}` {
		t.Fatalf("tool arguments = %v:\n%s", args, got)
	}
}

// A truncated Chat upstream must not come out as a clean Anthropic
// completion. message_stop / end_turn is what made a cut-off answer look
// finished to the SDK (and hid the incomplete-stream path).
func TestChatToAnthropicDoesNotFakeTerminalOnTruncatedSource(t *testing.T) {
	got := convertTruncatedStreamString(t, DialectCompatible, DialectAnthropic, sse(
		`{"id":"c","model":"m","choices":[{"delta":{"content":"cut off"}}]}`,
	))
	if !strings.Contains(got, "cut off") {
		t.Fatalf("partial text was dropped:\n%s", got)
	}
	counts := map[string]int{}
	for _, f := range sseFrames(t, got) {
		counts[strVal(f["type"])]++
	}
	if counts["message_delta"] != 0 || counts["message_stop"] != 0 {
		t.Fatalf("truncated stream synthesized a finished message: %v\n%s", counts, got)
	}

	// finish_reason (even without [DONE]) is a real terminal; usage rides a
	// later chunk, but the source did say it was done.
	finished := convertStreamString(t, DialectCompatible, DialectAnthropic, sse(
		`{"id":"c","model":"m","choices":[{"delta":{"content":"whole"}}]}`,
		`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
	))
	fin := map[string]int{}
	for _, f := range sseFrames(t, finished) {
		fin[strVal(f["type"])]++
	}
	if fin["message_delta"] != 1 || fin["message_stop"] != 1 {
		t.Fatalf("finished stream missing Anthropic terminal: %v\n%s", fin, finished)
	}
}

// A bare "data:" line is legal SSE (a keep-alive comment style some upstreams
// emit) and used to index into an empty slice.
func TestConvertStreamEmptyDataLine(t *testing.T) {
	got := convertStreamString(t, DialectCompatible, DialectAnthropic,
		"data:\n\ndata: \n\n"+sse(
			`{"id":"c","model":"m","choices":[{"delta":{"content":"ok"}}]}`,
			`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		))
	if !strings.Contains(got, "ok") {
		t.Fatalf("dropped text after empty data line:\n%s", got)
	}
}

// A Responses client converted onto a Chat upstream has to ask for usage or
// the stream ends with no token counts and the request bills as zero.
func TestResponsesToChatRequestAsksForStreamUsage(t *testing.T) {
	out, err := ConvertRequest(DialectOpenAI, DialectCompatible,
		[]byte(`{"model":"m","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if opts := asMap(body["stream_options"]); opts == nil || opts["include_usage"] != true {
		t.Fatalf("stream_options = %v", body["stream_options"])
	}

	// Non-streaming requests get no stream_options at all.
	out, err = ConvertRequest(DialectOpenAI, DialectCompatible, []byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body = nil
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatalf("stream_options set on a non-stream request: %s", out)
	}
}

// Sampling and formatting knobs that exist in both dialects must survive the
// round trip; a whitelist that forgets them silently changes the request.
func TestRequestConversionKeepsCommonFields(t *testing.T) {
	src := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":false,
		"metadata":{"tenant":"acme"},"store":true,"parallel_tool_calls":false,"user":"u-1",
		"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":true,"schema":{"type":"object"}}},
		"reasoning_effort":"high"}`)
	out, err := ConvertRequest(DialectCompatible, DialectOpenAI, src)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if strVal(asMap(resp["metadata"])["tenant"]) != "acme" {
		t.Fatalf("metadata dropped: %s", out)
	}
	if resp["store"] != true || resp["parallel_tool_calls"] != false || strVal(resp["user"]) != "u-1" {
		t.Fatalf("common fields dropped: %s", out)
	}
	if got := strVal(asMap(asMap(resp["text"])["format"])["name"]); got != "out" {
		t.Fatalf("response_format did not become text.format: %s", out)
	}
	if got := strVal(asMap(resp["reasoning"])["effort"]); got != "high" {
		t.Fatalf("reasoning_effort dropped: %s", out)
	}

	// And back the other way.
	back, err := ConvertRequest(DialectOpenAI, DialectCompatible, out)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(back, &chat); err != nil {
		t.Fatal(err)
	}
	rf := asMap(chat["response_format"])
	if strVal(rf["type"]) != "json_schema" || strVal(asMap(rf["json_schema"])["name"]) != "out" {
		t.Fatalf("text.format did not become response_format: %s", back)
	}
	if strVal(chat["reasoning_effort"]) != "high" {
		t.Fatalf("reasoning effort lost on the way back: %s", back)
	}
}
