package llm

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jucode/jucode/internal/biz"
)

func TestAdaptAzureFoundryClaudeBodyUntouchedBytes(t *testing.T) {
	raw := []byte("{\"model\":\"claude-opus-5-5\",\n\"thinking\":{\"type\":\"adaptive\"},\"messages\":[{\"role\":\"user\",\"content\":\"<b>hi</b>\"}]}")
	got, err := adaptAzureFoundryClaudeBody(raw, "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("body was modified:\n%s", got)
	}
}

func TestAdaptAzureFoundryClaudeBodyStripsUnsupported(t *testing.T) {
	raw := []byte(`{
		"model":"claude-opus-5-5",
		"fallbacks":"default",
		"cache_control":{"type":"ephemeral","scope":"global"},
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],
		"tools":[{"name":"bash","input_schema":{"type":"object"}},{"type":"advisor_20260301","name":"advisor","model":"claude-opus-5-5"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"say \"scope\"","cache_control":{"type":"ephemeral","scope":"global"}}]}],
		"thinking":{"type":"disabled"}
	}`)
	got, err := adaptAzureFoundryClaudeBody(raw, "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["fallbacks"]; ok {
		t.Fatalf("fallbacks kept: %s", got)
	}
	if bytes.Contains(got, []byte(`"scope"`)) || bytes.Contains(got, []byte(`advisor`)) {
		t.Fatalf("scope or advisor kept: %s", got)
	}
	if tools := body["tools"].([]any); len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	if !bytes.Contains(got, []byte(`"ttl":"1h"`)) || !bytes.Contains(got, []byte(`say \"scope\"`)) {
		t.Fatalf("unrelated content changed: %s", got)
	}
	// Opus 5.5 cannot turn thinking off; its request is left to fail upstream.
	if th := body["thinking"].(map[string]any); th["type"] != "disabled" {
		t.Fatalf("opus thinking rewritten: %v", th)
	}
}

func TestAdaptAzureFoundryClaudeBodyOnlyAdvisorTool(t *testing.T) {
	raw := []byte(`{"model":"m","tools":[{"type":"advisor_20260301","name":"advisor"}],"tool_choice":{"type":"auto"},"messages":[]}`)
	got, err := adaptAzureFoundryClaudeBody(raw, "m")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`"tools"`)) || bytes.Contains(got, []byte(`"tool_choice"`)) {
		t.Fatalf("empty tool list kept: %s", got)
	}
}

func TestAdaptAzureFoundryClaudeBodySonnetThinkingOff(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-5-5","thinking":{"type":"disabled"},"messages":[]}`)
	got, err := adaptAzureFoundryClaudeBody(raw, "claude-sonnet-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"thinking":{"type":"between_tools"}`)) {
		t.Fatalf("thinking not rewritten: %s", got)
	}
}

func TestAnthropicNativeBodyAdaptsOnlyAzureFoundry(t *testing.T) {
	raw := []byte(`{"model":"client","fallbacks":"default","messages":[]}`)
	got, err := anthropicNativeBody(raw, "claude-opus-5-5", biz.AzureFoundryClaudeDefinitionID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`fallbacks`)) || !bytes.Contains(got, []byte(`"claude-opus-5-5"`)) {
		t.Fatalf("azure body = %s", got)
	}
	got, err = anthropicNativeBody(raw, "claude-opus-5-5", "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`fallbacks`)) {
		t.Fatalf("non-azure body lost fallbacks: %s", got)
	}
}
