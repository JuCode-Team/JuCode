package llm

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestPrepareForwardBodyDoesNotHTMLEscape(t *testing.T) {
	body := map[string]any{
		"model":    "client-model",
		"messages": []any{map[string]any{"role": "user", "content": "a<b & c"}},
	}
	got, err := prepareForwardBody(body, "upstream-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`\u003c`)) || bytes.Contains(got, []byte(`\u0026`)) {
		t.Fatalf("request text was HTML-escaped: %s", got)
	}
	if !bytes.Contains(got, []byte("a<b & c")) || !bytes.Contains(got, []byte(`"upstream-model"`)) {
		t.Fatalf("body = %s", got)
	}
}

func TestStripImageGenerationToolRemovesImageGenerationDefinition(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "png"},
			map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "web_search"},
			},
		},
		"tool_choice": "required",
	}

	if !stripImageGenerationTool(body) {
		t.Fatalf("expected image_generation tool to be removed")
	}

	tools, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("expected filtered tools to remain")
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool after filtering, got %d", len(tools))
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["type"] != "function" {
		t.Fatalf("expected non-image tool to remain, got %#v", tools[0])
	}
	if got, _ := body["tool_choice"].(string); got != "required" {
		t.Fatalf("expected generic tool_choice to remain, got %#v", body["tool_choice"])
	}
}

func TestStripImageGenerationToolDropsToolChoiceWhenNothingRemains(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
		"tool_choice": map[string]any{"type": "image_generation"},
	}

	if !stripImageGenerationTool(body) {
		t.Fatalf("expected image_generation tool to be removed")
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("expected tools to be removed when no definitions remain")
	}
	if _, ok := body["tool_choice"]; ok {
		t.Fatalf("expected tool_choice to be removed when image_generation was blocked")
	}
}

func TestStripImageGenerationToolNoopWithoutMatchingTool(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "shell"},
			},
		},
	}

	if stripImageGenerationTool(body) {
		t.Fatalf("expected no change when image_generation is absent")
	}
}

func TestIsLikelyImageGenerationRequest(t *testing.T) {
	tests := []struct {
		name          string
		modelName     string
		upstreamModel string
		body          map[string]any
		want          bool
	}{
		{
			name:      "gpt image model",
			modelName: "gpt-image-2",
			want:      true,
		},
		{
			name:          "routed upstream image model",
			modelName:     "image2",
			upstreamModel: "gpt-image-2",
			want:          true,
		},
		{
			name:      "responses image tool",
			modelName: "gpt-5.5",
			body: map[string]any{
				"tools": []any{map[string]any{"type": "image_generation"}},
			},
			want: true,
		},
		{
			name:      "forced image tool choice",
			modelName: "gpt-5.6",
			body: map[string]any{
				"tool_choice": map[string]any{"type": "image_generation"},
			},
			want: true,
		},
		{
			name:      "plain text model",
			modelName: "gpt-5.5",
			body: map[string]any{
				"tools": []any{map[string]any{"type": "function"}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLikelyImageGenerationRequest(tt.modelName, tt.upstreamModel, tt.body); got != tt.want {
				t.Fatalf("isLikelyImageGenerationRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamIdleTimeoutForImageGenerationRequestUsesMinimum(t *testing.T) {
	g := &Gateway{requestTimeout: 60 * time.Second}
	body := map[string]any{
		"tools": []any{map[string]any{"type": "image_generation"}},
	}

	got := g.streamIdleTimeoutForRequest(context.Background(), "gpt-5.5", "", body)
	if got != minImageGenerationStreamIdleTimeout {
		t.Fatalf("streamIdleTimeoutForRequest() = %v, want %v", got, minImageGenerationStreamIdleTimeout)
	}
}

func TestStreamIdleTimeoutForPlainRequestKeepsConfiguredTimeout(t *testing.T) {
	g := &Gateway{requestTimeout: 60 * time.Second}

	got := g.streamIdleTimeoutForRequest(context.Background(), "gpt-5.5", "", nil)
	if got != 60*time.Second {
		t.Fatalf("streamIdleTimeoutForRequest() = %v, want %v", got, 60*time.Second)
	}
}
