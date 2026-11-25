// reasoning.go: admin-configurable extraction of a request's "thinking
// strength" (reasoning effort) from the inbound LLM request body.
//
// Historically the gateway hardcoded three probes: OpenAI's
// `reasoning.effort` / `reasoning_effort` strings and Anthropic's
// `thinking.budget_tokens` (surfaced as a "thinking" sentinel). Those
// probes are now data: SiteSettings.ReasoningParseRules holds an
// ordered rule list the admin edits in the panel. Numeric probes like
// budget_tokens record the RAW token count ("thinking:16384") — the
// actual thinking budget is shown to users; we never bucket a token
// count into a strength label. The extracted value lands in
// usage_log.reasoning_effort.
package biz

import (
	"errors"
	"strconv"
	"strings"
)

// ReasoningParseRule is one probe into the request body. Rules run in
// order; the first rule that produces a non-empty label wins.
//
// Kind selects how the value at Path is interpreted:
//
//	"string" — value must be a string. ValueMap maps raw values
//	           to normalized labels; a raw value missing from the
//	           map passes through unchanged (empty map = always
//	           pass through). This lets an admin normalize e.g.
//	           "xhigh" → "high" without enumerating every level.
//	"number" — value must be a number (e.g. thinking.budget_tokens).
//	           The raw count is recorded verbatim so users see the
//	           actual thinking-token budget — we deliberately do NOT
//	           bucket token counts into strength labels. Label, when
//	           set, is prepended as "label:12345" for readability in
//	           mixed logs.
//	"exists" — the path merely being present (non-null) emits Label.
type ReasoningParseRule struct {
	Path     string            `json:"path"`                // dot-separated path into the JSON body, e.g. "reasoning.effort"
	Kind     string            `json:"kind"`                // string | number | exists
	ValueMap map[string]string `json:"value_map,omitempty"` // kind=string: raw value → label
	Label    string            `json:"label,omitempty"`     // kind=exists: emitted label; kind=number: optional prefix
}

// DefaultReasoningParseRules covers OpenAI Responses `reasoning.effort`,
// the top-level `reasoning_effort`, then Anthropic's
// `thinking.budget_tokens` — recorded as the raw token count so the
// actual thinking budget is visible to users.
func DefaultReasoningParseRules() []ReasoningParseRule {
	return []ReasoningParseRule{
		{Path: "reasoning.effort", Kind: "string"},
		{Path: "reasoning_effort", Kind: "string"},
		{Path: "thinking.budget_tokens", Kind: "number", Label: "thinking"},
	}
}

// ParseReasoningEffort runs the rule list over a decoded request body.
// Returns "" when no rule matches.
func ParseReasoningEffort(rules []ReasoningParseRule, body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	for _, r := range rules {
		v, ok := resolveBodyPath(body, r.Path)
		if !ok || v == nil {
			continue
		}
		switch r.Kind {
		case "string":
			s, ok := v.(string)
			if !ok || s == "" {
				continue
			}
			if mapped, ok := r.ValueMap[s]; ok && mapped != "" {
				return mapped
			}
			return s
		case "number":
			n, ok := toFloatLoose(v)
			if !ok {
				continue
			}
			count := strconv.FormatInt(int64(n), 10)
			if r.Label != "" {
				return r.Label + ":" + count
			}
			return count
		case "exists":
			if r.Label != "" {
				return r.Label
			}
		}
	}
	return ""
}

// resolveBodyPath walks a dot-separated path through nested JSON
// objects (map[string]any). Only object traversal is supported — array
// indexing is deliberately out of scope for these rules.
func resolveBodyPath(body map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	var cur any = body
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func toFloatLoose(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// NormalizeReasoningParseRules validates and canonicalizes an
// admin-submitted rule list before persisting: unknown kinds, empty
// paths and exists-without-label are rejected. nil input is returned
// as nil ("use built-in defaults"); an empty non-nil list is preserved
// ("parsing disabled").
func NormalizeReasoningParseRules(rules []ReasoningParseRule) ([]ReasoningParseRule, error) {
	if rules == nil {
		return nil, nil
	}
	out := make([]ReasoningParseRule, 0, len(rules))
	for _, r := range rules {
		r.Path = strings.TrimSpace(r.Path)
		r.Kind = strings.TrimSpace(strings.ToLower(r.Kind))
		r.Label = strings.TrimSpace(r.Label)
		if r.Path == "" {
			return nil, errors.New("reasoning parse rule: path is required")
		}
		switch r.Kind {
		case "string":
			cleaned := make(map[string]string, len(r.ValueMap))
			for k, v := range r.ValueMap {
				k, v = strings.TrimSpace(k), strings.TrimSpace(v)
				if k == "" || v == "" {
					continue
				}
				cleaned[k] = v
			}
			if len(cleaned) == 0 {
				cleaned = nil
			}
			r.ValueMap = cleaned
		case "number":
			// Label is an optional display prefix; nothing else to check.
			r.ValueMap = nil
		case "exists":
			if r.Label == "" {
				return nil, errors.New("reasoning parse rule: exists kind requires a label")
			}
			r.ValueMap = nil
		default:
			return nil, errors.New("reasoning parse rule: kind must be string, number or exists")
		}
		out = append(out, r)
	}
	return out, nil
}
