package llm

import (
	"bytes"
	"encoding/json"
	"strings"
)

// adaptAzureFoundryClaudeBody drops or rewrites the Messages API fields that
// Claude on Azure AI Foundry rejects with a 400, so the request runs as the
// first-party API would have run it minus the unsupported feature. Only the
// touched top-level keys are re-encoded; everything else keeps its bytes.
//
//   - tools of type advisor_*: Foundry has no advisor tool (the beta header is
//     already dropped by the definition's header rule, the definition is not).
//   - top-level fallbacks: server-side fallback is not offered on Foundry.
//   - cache_control.scope: rejected as an extra input.
//   - thinking {type:"disabled"} on Sonnet 5.5: that model only accepts
//     {type:"between_tools"} as its thinking-off form.
func adaptAzureFoundryClaudeBody(body []byte, upstreamModel string) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"advisor_`)) &&
		!bytes.Contains(body, []byte(`"fallbacks"`)) &&
		!bytes.Contains(body, []byte(`"scope"`)) &&
		!bytes.Contains(body, []byte(`"disabled"`)) {
		return body, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	changed := false

	if _, ok := raw["fallbacks"]; ok {
		delete(raw, "fallbacks")
		changed = true
	}

	if tools, ok := raw["tools"]; ok && bytes.Contains(tools, []byte(`"advisor_`)) {
		var list []json.RawMessage
		if err := json.Unmarshal(tools, &list); err == nil {
			kept := list[:0]
			for _, item := range list {
				var head struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(item, &head) == nil && strings.HasPrefix(head.Type, "advisor_") {
					continue
				}
				kept = append(kept, item)
			}
			if len(kept) == 0 {
				delete(raw, "tools")
				delete(raw, "tool_choice")
			} else if raw["tools"], err = marshalJSON(kept); err != nil {
				return nil, err
			}
			changed = true
		}
	}

	if strings.Contains(strings.ToLower(upstreamModel), "sonnet-5-5") {
		var th struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw["thinking"], &th) == nil && th.Type == "disabled" {
			raw["thinking"] = json.RawMessage(`{"type":"between_tools"}`)
			changed = true
		}
	}

	for _, key := range []string{"cache_control", "tools", "system", "messages"} {
		v, ok := raw[key]
		if !ok || !bytes.Contains(v, []byte(`"scope"`)) {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.UseNumber()
		var tree any
		if err := dec.Decode(&tree); err != nil {
			return nil, err
		}
		if key == "cache_control" {
			// Top-level automatic caching: the value is the cache_control
			// object itself.
			tree = map[string]any{"cache_control": tree}
		}
		if !stripCacheControlScope(tree) {
			continue
		}
		if key == "cache_control" {
			tree = tree.(map[string]any)["cache_control"]
		}
		out, err := marshalJSON(tree)
		if err != nil {
			return nil, err
		}
		raw[key] = out
		changed = true
	}

	if !changed {
		return body, nil
	}
	return marshalJSON(raw)
}

// thinkingDisplayUpdatesBeta enables thinking.display "updates". Without it
// Foundry rejects that display value, and some relays in front of Claude Code
// drop the client's beta header while keeping the body.
const thinkingDisplayUpdatesBeta = "thinking-display-updates-2026-08-18"

func wantsThinkingDisplayUpdates(body []byte) bool {
	if !bytes.Contains(body, []byte(`"updates"`)) {
		return false
	}
	var req struct {
		Thinking struct {
			Display string `json:"display"`
		} `json:"thinking"`
	}
	return json.Unmarshal(body, &req) == nil && req.Thinking.Display == "updates"
}

// stripCacheControlScope removes `scope` from every cache_control object in
// the tree.
func stripCacheControlScope(node any) bool {
	removed := false
	switch n := node.(type) {
	case map[string]any:
		if cc, ok := n["cache_control"].(map[string]any); ok {
			if _, has := cc["scope"]; has {
				delete(cc, "scope")
				removed = true
			}
		}
		for k, v := range n {
			if k == "cache_control" {
				continue
			}
			if stripCacheControlScope(v) {
				removed = true
			}
		}
	case []any:
		for _, v := range n {
			if stripCacheControlScope(v) {
				removed = true
			}
		}
	}
	return removed
}
