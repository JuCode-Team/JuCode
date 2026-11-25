package llm

import "testing"

func TestCurrentClaudeModelsGetAdaptiveThinking(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1", "claude-opus-4-6"} {
		out := chatToAnthropicRequest(map[string]any{
			"model":            model,
			"max_tokens":       32000,
			"reasoning_effort": "medium",
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		})
		th, _ := out["thinking"].(map[string]any)
		if th["type"] != "adaptive" {
			t.Fatalf("%s: thinking = %v", model, out["thinking"])
		}
		if oc, _ := out["output_config"].(map[string]any); oc["effort"] != "medium" {
			t.Fatalf("%s: output_config = %v", model, out["output_config"])
		}
	}
	legacy := chatToAnthropicRequest(map[string]any{
		"model":            "claude-sonnet-4-5",
		"max_tokens":       32000,
		"reasoning_effort": "medium",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if th, _ := legacy["thinking"].(map[string]any); th["type"] != "enabled" {
		t.Fatalf("legacy thinking = %v", legacy["thinking"])
	}
}
