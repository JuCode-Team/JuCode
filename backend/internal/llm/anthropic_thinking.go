package llm

import "encoding/json"

// anthropicThinkingBlock is the Anthropic thinking content block Grok Build
// deserializes with a required `signature` field. An empty string is a valid
// placeholder; later signature_delta events (or a non-stream signature) can
// still replace it. Omitting the key makes grok-shell 1.0.5 abort the turn.
func anthropicThinkingBlock(thinking string) map[string]any {
	return map[string]any{
		"type":      "thinking",
		"thinking":  thinking,
		"signature": "",
	}
}

// EnsureAnthropicThinkingSignature rewrites an Anthropic Messages JSON
// payload so every thinking block has a `signature` key. Returns nil when
// the payload is not JSON, has no thinking block, or already has the field.
// Safe to use as PipeOptions.TransformPayload (nil means "leave original").
func EnsureAnthropicThinkingSignature(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil
	}
	if !ensureThinkingSignatureMap(obj) {
		return nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

func ensureThinkingSignatureMap(m map[string]any) bool {
	if m == nil {
		return false
	}
	changed := false
	if strVal(m["type"]) == "thinking" {
		if _, ok := m["signature"].(string); !ok {
			m["signature"] = ""
			changed = true
		}
	}
	if cb := asMap(m["content_block"]); cb != nil {
		if ensureThinkingSignatureMap(cb) {
			changed = true
		}
	}
	if msg := asMap(m["message"]); msg != nil {
		if ensureThinkingSignatureMap(msg) {
			changed = true
		}
	}
	for _, raw := range asSlice(m["content"]) {
		if b := asMap(raw); b != nil && ensureThinkingSignatureMap(b) {
			changed = true
		}
	}
	return changed
}
