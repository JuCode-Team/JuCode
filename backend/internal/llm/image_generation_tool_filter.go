package llm

import (
	"encoding/json"
	"strings"
)

func prepareForwardBody(body map[string]any, upstreamModel string, blockImageGenerationTool bool) ([]byte, error) {
	if blockImageGenerationTool {
		forwardBody, err := cloneRequestBody(body)
		if err != nil {
			return nil, err
		}
		forwardBody["model"] = upstreamModel
		stripImageGenerationTool(forwardBody)
		return marshalJSON(forwardBody)
	}
	oldModel, hadModel := body["model"]
	body["model"] = upstreamModel
	defer func() {
		if hadModel {
			body["model"] = oldModel
		} else {
			delete(body, "model")
		}
	}()
	return marshalJSON(body)
}

func cloneRequestBody(body map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// stripImageGenerationTool removes OpenAI Responses image-generation tool
// definitions from a request body in place. Returns true when at least one
// tool was removed.
func stripImageGenerationTool(body map[string]any) bool {
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		return false
	}

	filtered := make([]any, 0, len(tools))
	removed := false
	for _, item := range tools {
		m, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if typ, _ := m["type"].(string); typ == "image_generation" {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return false
	}

	if len(filtered) == 0 {
		delete(body, "tools")
		delete(body, "tool_choice")
		return true
	}
	body["tools"] = filtered

	if tc, ok := body["tool_choice"].(map[string]any); ok {
		if typ, _ := tc["type"].(string); typ == "image_generation" {
			delete(body, "tool_choice")
		}
	}
	return true
}

func isLikelyImageGenerationRequest(modelName, upstreamModel string, body map[string]any) bool {
	return isImageGenerationModelName(modelName) ||
		isImageGenerationModelName(upstreamModel) ||
		hasImageGenerationTool(body)
}

func isImageGenerationModelName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "_", "-")
	return strings.HasPrefix(n, "gpt-image-") ||
		strings.HasPrefix(n, "dall-e-") ||
		n == "image2" ||
		n == "image-2"
}

func hasImageGenerationTool(body map[string]any) bool {
	if body == nil {
		return false
	}
	tools, ok := body["tools"].([]any)
	if ok {
		for _, item := range tools {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := m["type"].(string); typ == "image_generation" {
				return true
			}
		}
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		if typ, _ := tc["type"].(string); typ == "image_generation" {
			return true
		}
	}
	return false
}
