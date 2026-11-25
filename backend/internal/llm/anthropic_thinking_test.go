package llm

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestEnsureAnthropicThinkingSignature(t *testing.T) {
	t.Run("content_block_start missing signature", func(t *testing.T) {
		in := []byte(`{"type":"content_block_start","index":0,"content_block":{"thinking":"","type":"thinking"}}`)
		out := EnsureAnthropicThinkingSignature(in)
		if out == nil {
			t.Fatal("expected rewritten payload")
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		block := asMap(obj["content_block"])
		if _, ok := block["signature"]; !ok {
			t.Fatalf("signature missing: %s", out)
		}
		if strVal(block["signature"]) != "" {
			t.Fatalf("empty placeholder should stay empty, got %q", strVal(block["signature"]))
		}
	})

	t.Run("keeps existing signature", func(t *testing.T) {
		in := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"abc"}}`)
		out := EnsureAnthropicThinkingSignature(in)
		if out != nil {
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatal(err)
			}
			if strVal(asMap(obj["content_block"])["signature"]) != "abc" {
				t.Fatalf("rewrote existing signature: %s", out)
			}
		}
	})

	t.Run("message thinking block", func(t *testing.T) {
		in := []byte(`{"type":"message","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"hi"}]}`)
		out := EnsureAnthropicThinkingSignature(in)
		if out == nil {
			t.Fatal("expected rewritten payload")
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		block := asMap(asSlice(obj["content"])[0])
		if _, ok := block["signature"]; !ok {
			t.Fatalf("signature missing: %s", out)
		}
	})

	t.Run("null signature becomes empty string", func(t *testing.T) {
		in := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":null}}`)
		out := EnsureAnthropicThinkingSignature(in)
		if out == nil {
			t.Fatal("expected rewritten payload")
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		sig, ok := asMap(obj["content_block"])["signature"]
		if !ok {
			t.Fatalf("signature missing: %s", out)
		}
		if _, isStr := sig.(string); !isStr {
			t.Fatalf("signature still not a string: %s", out)
		}
	})

	t.Run("untouched text start", func(t *testing.T) {
		in := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		if got := EnsureAnthropicThinkingSignature(in); got != nil {
			t.Fatalf("rewrote non-thinking frame: %s", got)
		}
	})
}

func TestChatToAnthropicResponseThinkingHasSignature(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"id":    "c",
		"model": "m",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           "hi",
				"reasoning_content": "hmm",
			},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ConvertResponse(DialectCompatible, DialectAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var thinking map[string]any
	for _, rawBlock := range asSlice(obj["content"]) {
		if b := asMap(rawBlock); strVal(b["type"]) == "thinking" {
			thinking = b
			break
		}
	}
	if thinking == nil {
		t.Fatalf("no thinking block: %s", out)
	}
	if _, ok := thinking["signature"]; !ok {
		t.Fatalf("thinking block missing signature: %s", out)
	}
}

func TestResponsesToAnthropicResponseCarriesThinkingSignature(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"id":     "resp_1",
		"model":  "grok-4.6",
		"status": "completed",
		"output": []any{
			map[string]any{
				"type":              "reasoning",
				"encrypted_content": "enc-sig",
				"content":           []any{map[string]any{"type": "reasoning_text", "text": "plan"}},
			},
			map[string]any{
				"type":    "message",
				"content": []any{map[string]any{"type": "output_text", "text": "ok"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ConvertResponse(DialectOpenAI, DialectAnthropic, raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var thinking map[string]any
	for _, rawBlock := range asSlice(obj["content"]) {
		if b := asMap(rawBlock); strVal(b["type"]) == "thinking" {
			thinking = b
			break
		}
	}
	if thinking == nil {
		t.Fatalf("no thinking block: %s", out)
	}
	if strVal(thinking["thinking"]) != "plan" {
		t.Fatalf("thinking text = %q: %s", strVal(thinking["thinking"]), out)
	}
	if strVal(thinking["signature"]) != "enc-sig" {
		t.Fatalf("thinking signature = %q; want enc-sig: %s", strVal(thinking["signature"]), out)
	}
}

func TestPipeAndExtractFillsThinkingSignature(t *testing.T) {
	src := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"thinking\":\"\",\"type\":\"thinking\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	var dst bytes.Buffer
	if err := PipeAndExtractOpts(&dst, bytes.NewBufferString(src), &Accumulator{}, PipeOptions{
		TransformPayload: EnsureAnthropicThinkingSignature,
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dst.Bytes(), []byte(`"signature"`)) {
		t.Fatalf("piped stream still missing signature:\n%s", dst.String())
	}
}
