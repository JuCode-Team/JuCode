package llm

import (
	"strings"
	"testing"
)

// A refusal is the model's answer to the request. When the converter drops it,
// the caller receives an empty assistant message with a normal stop reason —
// the model appears to have produced nothing, and the reason it declined is
// gone.
func TestRefusalSurvivesConversion(t *testing.T) {
	const wording = "I can't help with that."

	t.Run("responses_to_chat", func(t *testing.T) {
		got := convertStreamString(t, DialectOpenAI, DialectCompatible, sse(
			`{"type":"response.created","response":{"id":"r","model":"m"}}`,
			`{"type":"response.refusal.delta","delta":"`+wording+`"}`,
			`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		))
		var seen string
		for _, f := range sseFrames(t, got) {
			for _, raw := range asSlice(f["choices"]) {
				seen += strVal(asMap(asMap(raw)["delta"])["refusal"])
			}
		}
		if seen != wording {
			t.Errorf("chat delta.refusal = %q, want %q\n%s", seen, wording, got)
		}
	})

	t.Run("chat_to_responses", func(t *testing.T) {
		got := convertStreamString(t, DialectCompatible, DialectOpenAI, sse(
			`{"id":"c","model":"m","choices":[{"delta":{"role":"assistant","refusal":"`+wording+`"}}]}`,
			`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		))
		var seen string
		for _, f := range sseFrames(t, got) {
			if strVal(f["type"]) == "response.refusal.delta" {
				seen += strVal(f["delta"])
			}
		}
		if seen != wording {
			t.Errorf("response.refusal.delta = %q, want %q\n%s", seen, wording, got)
		}
	})

	// Anthropic has no refusal block, so the wording rides a text block and
	// the fact becomes the stop reason.
	t.Run("chat_to_anthropic", func(t *testing.T) {
		got := convertStreamString(t, DialectCompatible, DialectAnthropic, sse(
			`{"id":"c","model":"m","choices":[{"delta":{"role":"assistant","refusal":"`+wording+`"}}]}`,
			`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		))
		if !strings.Contains(got, wording) {
			t.Errorf("refusal wording missing:\n%s", got)
		}
		var stop string
		for _, f := range sseFrames(t, got) {
			if strVal(f["type"]) == "message_delta" {
				stop = strVal(asMap(f["delta"])["stop_reason"])
			}
		}
		if stop != "refusal" {
			t.Errorf("stop_reason = %q, want refusal\n%s", stop, got)
		}
	})
}

// A truncated upstream must not come out looking finished. The gateway treats
// response.completed as proof the stream ended cleanly, so synthesizing one at
// EOF hides the truncation: no failover, no channel cooldown, and the caller
// pays for a cut-off answer. Staying quiet lets the incomplete-stream path emit
// response.failed with whatever text did arrive.
func TestChatToResponsesDoesNotFakeTerminalOnTruncatedSource(t *testing.T) {
	terminalTypes := func(raw string) []string {
		var out []string
		for _, f := range sseFrames(t, raw) {
			switch strVal(f["type"]) {
			case "response.completed", "response.incomplete", "response.failed":
				out = append(out, strVal(f["type"]))
			}
		}
		return out
	}

	// Upstream dies after some text: no finish_reason, no [DONE].
	truncated := convertTruncatedStreamString(t, DialectCompatible, DialectOpenAI, sse(
		`{"id":"c","model":"m","choices":[{"delta":{"content":"half an ans"}}]}`,
	))
	if got := terminalTypes(truncated); len(got) != 0 {
		t.Errorf("truncated stream emitted terminal frames %v:\n%s", got, truncated)
	}
	if !strings.Contains(truncated, "half an ans") {
		t.Errorf("partial text was dropped:\n%s", truncated)
	}

	// A source whose only terminal signal is [DONE] — no per-choice
	// finish_reason — did finish, and still has to be reported as such.
	sentinelOnly := convertStreamString(t, DialectCompatible, DialectOpenAI, sse(
		`{"id":"c","model":"m","choices":[{"delta":{"content":"whole answer"}}]}`,
		`[DONE]`,
	))
	if got := terminalTypes(sentinelOnly); len(got) != 1 || got[0] != "response.completed" {
		t.Errorf("[DONE]-terminated stream produced %v, want one response.completed:\n%s", got, sentinelOnly)
	}
}

// Anthropic→Chat truncation used to emit a lone [DONE] with no finish_reason
// and no error, which many Chat clients treat as a clean stop.
func TestAnthropicToChatDoesNotFakeDoneOnTruncatedSource(t *testing.T) {
	got := convertTruncatedStreamString(t, DialectAnthropic, DialectCompatible, sse(
		`{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":5,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half"}}`,
	))
	if !strings.Contains(got, "half") {
		t.Fatalf("partial text was dropped:\n%s", got)
	}
	if strings.Contains(got, "data: [DONE]") && !strings.Contains(got, `"error"`) {
		t.Fatalf("truncated Anthropic source emitted a clean [DONE]:\n%s", got)
	}
	for _, f := range sseFrames(t, got) {
		for _, raw := range asSlice(f["choices"]) {
			if strVal(asMap(raw)["finish_reason"]) == "stop" {
				t.Fatalf("truncated stream synthesized finish_reason=stop:\n%s", got)
			}
		}
	}
}

// Citations are what tie a web-search answer to the pages it came from. The
// search results themselves were already forwarded; without the annotations the
// answer arrives unattributed, which is exactly what a grounded client renders.
func TestCitationsSurviveConversion(t *testing.T) {
	const url = "https://example.test/page"

	t.Run("anthropic_to_chat", func(t *testing.T) {
		got := convertStreamString(t, DialectAnthropic, DialectCompatible, sse(
			`{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":5,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"grounded"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"`+url+`"}}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		))
		found := false
		for _, f := range sseFrames(t, got) {
			for _, raw := range asSlice(f["choices"]) {
				for _, a := range asSlice(asMap(asMap(raw)["delta"])["annotations"]) {
					if strVal(asMap(a)["url"]) == url {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("citation did not reach chat delta.annotations:\n%s", got)
		}
	})

	t.Run("anthropic_to_responses", func(t *testing.T) {
		got := convertStreamString(t, DialectAnthropic, DialectOpenAI, sse(
			`{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":5,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"grounded"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"`+url+`"}}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		))
		found := false
		for _, f := range sseFrames(t, got) {
			if strVal(f["type"]) == "response.output_text.annotation.added" &&
				strVal(asMap(f["annotation"])["url"]) == url {
				found = true
			}
		}
		if !found {
			t.Errorf("citation did not reach an annotation event:\n%s", got)
		}
	})

	t.Run("responses_to_chat", func(t *testing.T) {
		got := convertStreamString(t, DialectOpenAI, DialectCompatible, sse(
			`{"type":"response.created","response":{"id":"r","model":"m"}}`,
			`{"type":"response.output_text.delta","delta":"grounded"}`,
			`{"type":"response.output_text.annotation.added","annotation":{"type":"url_citation","url":"`+url+`"}}`,
			`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		))
		found := false
		for _, f := range sseFrames(t, got) {
			for _, raw := range asSlice(f["choices"]) {
				for _, a := range asSlice(asMap(asMap(raw)["delta"])["annotations"]) {
					if strVal(asMap(a)["url"]) == url {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("annotation did not reach chat delta.annotations:\n%s", got)
		}
	})

	// The chat spelling round-trips back out to Anthropic, so a citation that
	// came in one way can leave the other.
	t.Run("chat_to_anthropic", func(t *testing.T) {
		got := convertStreamString(t, DialectCompatible, DialectAnthropic, sse(
			`{"id":"c","model":"m","choices":[{"delta":{"role":"assistant","content":"grounded","annotations":[{"type":"url_citation","url":"`+url+`"}]}}]}`,
			`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		))
		found := false
		for _, f := range sseFrames(t, got) {
			if strVal(f["type"]) != "content_block_delta" {
				continue
			}
			d := asMap(f["delta"])
			if strVal(d["type"]) != "citations_delta" {
				continue
			}
			if strVal(asMap(d["citation"])["url"]) == url {
				found = true
			}
		}
		if !found {
			t.Errorf("annotation did not reach a citations_delta:\n%s", got)
		}
	})
}
