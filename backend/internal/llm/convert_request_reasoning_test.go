package llm

import "testing"

// A caller that asks to think harder must not be answered by a model that did
// not think at all. The budget is anchored to the caller's own max_tokens
// because thinking tokens bill as output — a translated request must not cost
// more than the untranslated one would have.
func TestChatReasoningEffortReachesAnthropicThinking(t *testing.T) {
	for _, tc := range []struct {
		effort    string
		maxTokens int
		want      int // 0 = no thinking block
	}{
		{"low", 8000, 2000},
		{"medium", 8000, 4000},
		{"high", 8000, 6400},
		{"minimal", 8000, 0},
		{"", 8000, 0},
		// Too small to fit the 1024 floor and still leave room to answer.
		{"high", 1500, 0},
		// The clamp keeps at least 1024 for the visible answer.
		{"high", 2048, 1024},
		// A quarter of 2048 is below Anthropic's 1024 floor, and rounding it
		// up would spend half the caller's ceiling on thinking they asked to
		// keep light.
		{"low", 2048, 0},
	} {
		in := map[string]any{
			"model":       "claude",
			"max_tokens":  tc.maxTokens,
			"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
			"temperature": 0.2,
		}
		if tc.effort != "" {
			in["reasoning_effort"] = tc.effort
		}
		out := chatToAnthropicRequest(in)
		th := asMap(out["thinking"])
		if tc.want == 0 {
			if th != nil {
				t.Errorf("effort=%q max=%d: unexpected thinking %v", tc.effort, tc.maxTokens, th)
			}
			if out["temperature"] == nil {
				t.Errorf("effort=%q max=%d: temperature dropped without thinking", tc.effort, tc.maxTokens)
			}
			continue
		}
		if got := intVal(th["budget_tokens"]); got != tc.want {
			t.Errorf("effort=%q max=%d: budget_tokens=%d want %d", tc.effort, tc.maxTokens, got, tc.want)
		}
		if got := intVal(th["budget_tokens"]); got >= tc.maxTokens {
			t.Errorf("effort=%q: budget %d must stay below max_tokens %d", tc.effort, got, tc.maxTokens)
		}
		// Anthropic rejects thinking alongside a sampling override, so the
		// request would 400 with these still attached.
		if out["temperature"] != nil || out["top_p"] != nil || out["top_k"] != nil {
			t.Errorf("effort=%q: samplers survived alongside thinking: %v", tc.effort, out)
		}
	}
}

// The reverse direction, so a request that crosses two conversions keeps
// roughly the strength it was sent with.
func TestAnthropicThinkingReachesChatReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		thinking map[string]any
		want     string
	}{
		{map[string]any{"type": "enabled", "budget_tokens": 2000}, "low"},
		{map[string]any{"type": "enabled", "budget_tokens": 4000}, "medium"},
		{map[string]any{"type": "enabled", "budget_tokens": 6400}, "high"},
		{map[string]any{"type": "disabled"}, ""},
		{nil, ""},
	} {
		in := map[string]any{
			"model":      "claude",
			"max_tokens": 8000,
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		}
		if tc.thinking != nil {
			in["thinking"] = tc.thinking
		}
		out := anthropicToChatRequest(in)
		if got := strVal(out["reasoning_effort"]); got != tc.want {
			t.Errorf("thinking=%v: reasoning_effort=%q want %q", tc.thinking, got, tc.want)
		}
	}
}

// An explicit thinking block from a client that already speaks Anthropic wins
// over the effort ladder — it is the more precise instruction of the two.
func TestExplicitThinkingBeatsEffortLadder(t *testing.T) {
	out := chatToAnthropicRequest(map[string]any{
		"model":            "claude",
		"max_tokens":       8000,
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "high",
		"thinking":         map[string]any{"type": "enabled", "budget_tokens": 1500},
	})
	if got := intVal(asMap(out["thinking"])["budget_tokens"]); got != 1500 {
		t.Errorf("budget_tokens=%d want the client's own 1500", got)
	}
}
