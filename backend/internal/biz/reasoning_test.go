package biz

import (
	"encoding/json"
	"testing"
)

func bodyOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseReasoningEffortDefaults(t *testing.T) {
	rules := DefaultReasoningParseRules()
	cases := []struct {
		name, body, want string
	}{
		{"responses nested effort", `{"reasoning":{"effort":"high"}}`, "high"},
		{"chat top-level effort", `{"reasoning_effort":"low"}`, "low"},
		{"anthropic budget records raw count", `{"thinking":{"type":"enabled","budget_tokens":4096}}`, "thinking:4096"},
		{"nested wins over budget", `{"reasoning":{"effort":"medium"},"thinking":{"budget_tokens":1}}`, "medium"},
		{"nothing", `{"model":"x"}`, ""},
		{"thinking without budget", `{"thinking":{"type":"disabled"}}`, ""},
		{"non-numeric budget ignored", `{"thinking":{"budget_tokens":"a lot"}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseReasoningEffort(rules, bodyOf(t, tc.body)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseReasoningEffortCustomRules(t *testing.T) {
	rules := []ReasoningParseRule{
		{Path: "reasoning.effort", Kind: "string", ValueMap: map[string]string{"xhigh": "high"}},
		{Path: "reasoning.mode", Kind: "string"},
		{Path: "thinking.budget_tokens", Kind: "number"}, // no label prefix
		{Path: "thinking", Kind: "exists", Label: "thinking"},
	}

	cases := []struct {
		name, body, want string
	}{
		{"value map hit", `{"reasoning":{"effort":"xhigh"}}`, "high"},
		{"value map miss passes raw", `{"reasoning":{"effort":"minimal"}}`, "minimal"},
		{"second rule reads mode", `{"reasoning":{"mode":"pro"}}`, "pro"},
		{"bare number without prefix", `{"thinking":{"budget_tokens":16384}}`, "16384"},
		{"exists fallback when budget missing", `{"thinking":{"type":"enabled"}}`, "thinking"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseReasoningEffort(rules, bodyOf(t, tc.body)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	// Empty rule list = extraction disabled.
	if got := ParseReasoningEffort([]ReasoningParseRule{}, bodyOf(t, `{"reasoning_effort":"high"}`)); got != "" {
		t.Fatalf("disabled rules produced %q", got)
	}
}

func TestNormalizeReasoningParseRules(t *testing.T) {
	rules, err := NormalizeReasoningParseRules([]ReasoningParseRule{
		{Path: " thinking.budget_tokens ", Kind: "Number", Label: " thinking "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rules[0].Path != "thinking.budget_tokens" || rules[0].Kind != "number" || rules[0].Label != "thinking" {
		t.Fatalf("not normalized: %+v", rules[0])
	}

	for name, bad := range map[string][]ReasoningParseRule{
		"empty path":           {{Path: "", Kind: "string"}},
		"unknown kind":         {{Path: "a", Kind: "regex"}},
		"legacy number_range":  {{Path: "a", Kind: "number_range"}},
		"exists without label": {{Path: "a", Kind: "exists"}},
	} {
		if _, err := NormalizeReasoningParseRules(bad); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}

	if out, err := NormalizeReasoningParseRules(nil); err != nil || out != nil {
		t.Fatalf("nil input should stay nil, got %v, %v", out, err)
	}
	out, err := NormalizeReasoningParseRules([]ReasoningParseRule{})
	if err != nil || out == nil || len(out) != 0 {
		t.Fatalf("empty input should stay empty non-nil, got %v, %v", out, err)
	}
}
