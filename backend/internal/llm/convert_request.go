package llm

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Converters build their output from a whitelist, so any field they do not
// name is silently dropped — and a request that comes back without its seed,
// penalties, or response_format is not the request the caller sent. These
// helpers carry over the knobs that exist in both dialects; fields with no
// counterpart in the target (Responses has no seed, Chat has no truncation)
// have nowhere to go and are still dropped.

// openAIFamilyCarryFields mean the same thing in Chat Completions and
// Responses, so they move across verbatim.
var openAIFamilyCarryFields = []string{
	"metadata", "store", "parallel_tool_calls", "service_tier",
	"user", "top_logprobs", "safety_identifier", "prompt_cache_key",
}

func carryFields(in, out map[string]any, names []string) {
	for _, name := range names {
		if v, ok := in[name]; ok && v != nil {
			out[name] = cloneAny(v)
		}
	}
}

// enableChatStreamUsage makes a streaming Chat request report token counts on
// its terminal frame. Without it a converted stream carries no usage block and
// the request bills as zero. The source dialect's own stream_options is not
// carried over — neither Anthropic nor Responses has a field of that shape.
func enableChatStreamUsage(out map[string]any) {
	if !boolVal(out["stream"]) {
		return
	}
	out["stream_options"] = map[string]any{"include_usage": true}
}

func chatToAnthropicRequest(in map[string]any) map[string]any {
	out := map[string]any{
		"model": in["model"],
	}
	if n := firstPositiveInt(in["max_tokens"], in["max_completion_tokens"]); n > 0 {
		out["max_tokens"] = n
	} else {
		out["max_tokens"] = 4096
	}
	if _, ok := in["stream"]; ok {
		out["stream"] = boolVal(in["stream"])
	}
	if in["temperature"] != nil {
		out["temperature"] = in["temperature"]
	}
	if in["top_p"] != nil {
		out["top_p"] = in["top_p"]
	}
	if stop := normalizeStop(in["stop"]); len(stop) > 0 {
		out["stop_sequences"] = stop
	}

	var systemParts []string
	var messages []any
	for _, raw := range asSlice(in["messages"]) {
		msg := asMap(raw)
		if msg == nil {
			continue
		}
		role := strVal(msg["role"])
		switch role {
		case "system", "developer":
			if s := flattenChatContent(msg["content"]); s != "" {
				systemParts = append(systemParts, s)
			}
		case "tool", "function":
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": firstNonEmpty(strVal(msg["tool_call_id"]), strVal(msg["id"])),
						"content":     flattenChatContent(msg["content"]),
					},
				},
			})
		case "assistant":
			messages = append(messages, chatAssistantToAnthropic(msg))
		default:
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": chatContentToAnthropic(msg["content"]),
			})
		}
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	out["messages"] = messages

	tools := make([]any, 0)
	for _, raw := range asSlice(in["tools"]) {
		t := asMap(raw)
		if t == nil {
			continue
		}
		if isWebSearchTool(t) {
			tools = append(tools, anthropicWebSearchToolFromAny(t))
			continue
		}
		if isBuiltInTool(t) {
			if b := passthroughBuiltInTool(t); b != nil {
				tools = append(tools, b)
			}
			continue
		}
		fn := asMap(t["function"])
		name := strVal(t["name"])
		if fn != nil {
			if name == "" {
				name = strVal(fn["name"])
			}
			item := map[string]any{"name": name}
			if d := strVal(fn["description"]); d != "" {
				item["description"] = d
			}
			if schema := fn["parameters"]; schema != nil {
				item["input_schema"] = schema
			} else if schema := t["parameters"]; schema != nil {
				item["input_schema"] = schema
			}
			if name != "" {
				tools = append(tools, item)
			}
			continue
		}
		if name != "" {
			item := map[string]any{"name": name}
			if d := strVal(t["description"]); d != "" {
				item["description"] = d
			}
			if schema := t["input_schema"]; schema != nil {
				item["input_schema"] = schema
			} else if schema := t["parameters"]; schema != nil {
				item["input_schema"] = schema
			}
			tools = append(tools, item)
		}
	}
	if opts := asMap(in["web_search_options"]); opts != nil {
		tools = append(tools, anthropicWebSearchToolFromOptions(opts))
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := chatToolChoiceToAnthropic(in["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	// top_k and thinking are Anthropic-native and reach us from compatible
	// clients that already speak them; user maps onto the one key Anthropic
	// allows in metadata.
	if v := in["top_k"]; v != nil {
		out["top_k"] = v
	}
	if th := asMap(in["thinking"]); th != nil {
		out["thinking"] = cloneAny(th)
	} else if effort := effortOf(in); usesAdaptiveThinking(strVal(out["model"])) {
		// Current Claude models reject thinking.budget_tokens (400 once tools
		// are present) and take the strength as output_config.effort instead.
		if adaptiveEffort(effort) {
			out["thinking"] = map[string]any{"type": "adaptive"}
			out["output_config"] = map[string]any{"effort": strings.ToLower(effort)}
		}
	} else if th := thinkingFromEffort(effort, intVal(out["max_tokens"])); th != nil {
		out["thinking"] = th
	}
	if out["thinking"] != nil {
		// Anthropic rejects extended thinking combined with any sampling
		// override, so the two requests cannot both be honored. The caller
		// asked for reasoning explicitly and it is the far larger effect on
		// the answer, so it wins and the samplers go.
		delete(out, "temperature")
		delete(out, "top_p")
		delete(out, "top_k")
	}
	if u := strVal(in["user"]); u != "" {
		out["metadata"] = map[string]any{"user_id": u}
	}
	return out
}

// effortOf reads the reasoning strength from either OpenAI-family spelling.
func effortOf(in map[string]any) string {
	if e := strVal(in["reasoning_effort"]); e != "" {
		return e
	}
	return strVal(asMap(in["reasoning"])["effort"])
}

// thinkingFromEffort translates an OpenAI reasoning effort into an Anthropic
// thinking budget. Without it a caller that asked to think harder is answered
// by a model that did not think at all.
//
// The budget is a fraction of the max_tokens the caller already set, never an
// absolute figure: thinking tokens bill as output, so anchoring to the caller's
// own ceiling keeps a translated request from costing more than the untranslated
// one would have. Anthropic also needs budget_tokens >= 1024 and strictly below
// max_tokens, so a ceiling too low to leave room for both is left alone rather
// than raised behind the caller's back.
func thinkingFromEffort(effort string, maxTokens int) map[string]any {
	const minBudget = 1024
	var fraction float64
	switch strings.ToLower(effort) {
	case "low":
		fraction = 0.25
	case "medium":
		fraction = 0.5
	case "high":
		fraction = 0.8
	default:
		// "minimal"/"none" ask for no thinking; anything else is unknown and
		// guessing a budget for it would spend tokens on a request that never
		// asked for any.
		return nil
	}
	budget := int(float64(maxTokens) * fraction)
	if room := maxTokens - minBudget; budget > room {
		budget = room
	}
	if budget < minBudget {
		return nil
	}
	return map[string]any{"type": "enabled", "budget_tokens": budget}
}

// usesAdaptiveThinking reports whether a Claude model takes adaptive thinking
// (output_config.effort) rather than a thinking budget: Fable and Mythos, and
// Opus/Sonnet from 4.6 on. Haiku and older Opus/Sonnet keep budget_tokens.
func usesAdaptiveThinking(model string) bool {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	rest, ok := strings.CutPrefix(strings.ToLower(model), "claude-")
	if !ok {
		return false
	}
	if strings.HasPrefix(rest, "fable") || strings.HasPrefix(rest, "mythos") {
		return true
	}
	family, version, ok := strings.Cut(rest, "-")
	if !ok || (family != "opus" && family != "sonnet") {
		return false
	}
	parts := strings.Split(version, "-")
	major, _ := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		if n, err := strconv.Atoi(parts[1]); err == nil && n < 100 {
			minor = n
		}
	}
	return major > 4 || (major == 4 && minor >= 6)
}

// adaptiveEffort reports whether effort is an output_config.effort level.
func adaptiveEffort(effort string) bool {
	switch strings.ToLower(effort) {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// effortFromThinking is the inverse, by the same fractions, so a request that
// crosses two conversions keeps roughly the strength it was sent with.
func effortFromThinking(th map[string]any, maxTokens int) string {
	if th == nil || strVal(th["type"]) == "disabled" {
		return ""
	}
	budget := intVal(th["budget_tokens"])
	if budget <= 0 || maxTokens <= 0 {
		return ""
	}
	switch fraction := float64(budget) / float64(maxTokens); {
	case fraction <= 0.3:
		return "low"
	case fraction <= 0.65:
		return "medium"
	default:
		return "high"
	}
}

func chatAssistantToAnthropic(msg map[string]any) map[string]any {
	var blocks []any
	if text := flattenChatContent(msg["content"]); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	for _, raw := range asSlice(msg["tool_calls"]) {
		tc := asMap(raw)
		if tc == nil {
			continue
		}
		fn := asMap(tc["function"])
		name := strVal(tc["name"])
		args := tc["arguments"]
		if fn != nil {
			if name == "" {
				name = strVal(fn["name"])
			}
			if args == nil {
				args = fn["arguments"]
			}
		}
		var input any = map[string]any{}
		switch a := args.(type) {
		case string:
			if a != "" {
				var parsed any
				if json.Unmarshal([]byte(a), &parsed) == nil {
					input = parsed
				} else {
					input = map[string]any{"_raw_arguments": a}
				}
			}
		case map[string]any:
			input = a
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"])),
			"name":  name,
			"input": input,
		})
	}
	if len(blocks) == 0 {
		blocks = []any{map[string]any{"type": "text", "text": ""}}
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

func chatContentToAnthropic(content any) any {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	var blocks []any
	for _, raw := range asSlice(content) {
		p := asMap(raw)
		if p == nil {
			continue
		}
		switch strVal(p["type"]) {
		case "text":
			if s := strVal(p["text"]); s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
		case "image_url":
			url := ""
			if img := asMap(p["image_url"]); img != nil {
				url = strVal(img["url"])
			} else {
				url = strVal(p["image_url"])
			}
			if url == "" {
				continue
			}
			if strings.HasPrefix(url, "data:") {
				media, data := splitDataURL(url)
				blocks = append(blocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": media,
						"data":       data,
					},
				})
			} else {
				blocks = append(blocks, map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "url", "url": url},
				})
			}
		}
	}
	if len(blocks) == 0 {
		return flattenChatContent(content)
	}
	return blocks
}

func flattenChatContent(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	for _, raw := range asSlice(content) {
		p := asMap(raw)
		if p == nil {
			continue
		}
		if s := strVal(p["text"]); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "")
	}
	b, _ := json.Marshal(content)
	return string(b)
}

func chatToolChoiceToAnthropic(raw any) any {
	if raw == nil {
		return nil
	}
	if s, ok := raw.(string); ok {
		switch s {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		default:
			return map[string]any{"type": "auto"}
		}
	}
	m := asMap(raw)
	if m == nil {
		return nil
	}
	switch strVal(m["type"]) {
	case "auto":
		return map[string]any{"type": "auto"}
	case "any", "required":
		return map[string]any{"type": "any"}
	case "none":
		return map[string]any{"type": "none"}
	case "function", "tool":
		name := strVal(m["name"])
		if fn := asMap(m["function"]); fn != nil && name == "" {
			name = strVal(fn["name"])
		}
		if name == "" {
			return map[string]any{"type": "auto"}
		}
		return map[string]any{"type": "tool", "name": name}
	}
	return nil
}

func anthropicToChatRequest(in map[string]any) map[string]any {
	out := map[string]any{
		"model":  in["model"],
		"stream": boolVal(in["stream"]),
	}
	if n := intVal(in["max_tokens"]); n > 0 {
		out["max_tokens"] = n
	}
	if in["temperature"] != nil {
		out["temperature"] = in["temperature"]
	}
	if in["top_p"] != nil {
		out["top_p"] = in["top_p"]
	}
	if stop := normalizeStop(in["stop_sequences"]); len(stop) > 0 {
		out["stop"] = stop
	} else if stop := normalizeStop(in["stop"]); len(stop) > 0 {
		out["stop"] = stop
	}

	var msgs []any
	if sys := flattenAnthropicSystemValue(in["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, raw := range asSlice(in["messages"]) {
		m := asMap(raw)
		if m == nil {
			continue
		}
		for _, converted := range convertAnthropicMessageMap(m) {
			msgs = append(msgs, converted)
		}
	}
	out["messages"] = msgs

	var tools []any
	var searchOpts map[string]any
	for _, raw := range asSlice(in["tools"]) {
		t := asMap(raw)
		if t == nil {
			continue
		}
		if isWebSearchTool(t) {
			searchOpts = webSearchOptionsFromAnthropicTool(t)
			tools = append(tools, map[string]any{"type": openaiWebSearch})
			continue
		}
		if isBuiltInTool(t) {
			if b := passthroughBuiltInTool(t); b != nil {
				tools = append(tools, b)
			}
			continue
		}
		name := strVal(t["name"])
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d := strVal(t["description"]); d != "" {
			fn["description"] = d
		}
		if schema := t["input_schema"]; schema != nil {
			fn["parameters"] = schema
		} else if schema := t["parameters"]; schema != nil {
			fn["parameters"] = schema
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if len(searchOpts) > 0 {
		out["web_search_options"] = searchOpts
	}
	if tc := anthropicToolChoiceToChat(in["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	if md := asMap(in["metadata"]); md != nil {
		if u := strVal(md["user_id"]); u != "" {
			out["user"] = u
		}
	}
	if e := effortFromThinking(asMap(in["thinking"]), intVal(in["max_tokens"])); e != "" {
		out["reasoning_effort"] = e
	}
	enableChatStreamUsage(out)
	return out
}

func flattenAnthropicSystemValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return stripClaudeCodeBillingHeader(s)
	}
	raw, _ := json.Marshal(v)
	return flattenAnthropicSystem(raw)
}

func convertAnthropicMessageMap(m map[string]any) []map[string]any {
	raw, _ := json.Marshal(m["content"])
	return convertAnthropicMessage(anthropicMessage{Role: strVal(m["role"]), Content: raw})
}

func anthropicToolChoiceToChat(raw any) any {
	if raw == nil {
		return nil
	}
	if s, ok := raw.(string); ok {
		return s
	}
	b, _ := json.Marshal(raw)
	return convertToolChoice(b)
}

func chatToResponsesRequest(in map[string]any) map[string]any {
	out := map[string]any{
		"model": in["model"],
	}
	if _, ok := in["stream"]; ok {
		out["stream"] = boolVal(in["stream"])
	}
	if n := firstPositiveInt(in["max_output_tokens"], in["max_tokens"], in["max_completion_tokens"]); n > 0 {
		out["max_output_tokens"] = n
	}
	if in["temperature"] != nil {
		out["temperature"] = in["temperature"]
	}
	if in["top_p"] != nil {
		out["top_p"] = in["top_p"]
	}

	var instructions []string
	var input []any
	for _, raw := range asSlice(in["messages"]) {
		msg := asMap(raw)
		if msg == nil {
			continue
		}
		role := strVal(msg["role"])
		switch role {
		case "system", "developer":
			if s := flattenChatContent(msg["content"]); s != "" {
				instructions = append(instructions, s)
			}
		case "tool", "function":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": firstNonEmpty(strVal(msg["tool_call_id"]), strVal(msg["id"])),
				"output":  flattenChatContent(msg["content"]),
			})
		case "assistant":
			item := map[string]any{"role": "assistant", "content": chatContentToResponses(msg["content"], true)}
			input = append(input, item)
			for _, rawTC := range asSlice(msg["tool_calls"]) {
				tc := asMap(rawTC)
				if tc == nil {
					continue
				}
				fn := asMap(tc["function"])
				name := strVal(tc["name"])
				args := tc["arguments"]
				if fn != nil {
					if name == "" {
						name = strVal(fn["name"])
					}
					if args == nil {
						args = fn["arguments"]
					}
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"])),
					"name":      name,
					"arguments": stringifyArgs(args),
				})
			}
		default:
			input = append(input, map[string]any{
				"role":    "user",
				"content": chatContentToResponses(msg["content"], false),
			})
		}
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	if len(input) == 1 {
		if only, ok := input[0].(map[string]any); ok && strVal(only["role"]) == "user" {
			if s, ok := only["content"].(string); ok {
				out["input"] = s
			} else {
				out["input"] = input
			}
		} else {
			out["input"] = input
		}
	} else {
		out["input"] = input
	}

	var tools []any
	for _, raw := range asSlice(in["tools"]) {
		t := asMap(raw)
		if t == nil {
			continue
		}
		if isWebSearchTool(t) {
			tools = append(tools, responsesWebSearchToolFromAny(t))
			continue
		}
		if isBuiltInTool(t) {
			if b := passthroughBuiltInTool(t); b != nil {
				tools = append(tools, b)
			}
			continue
		}
		fn := asMap(t["function"])
		if fn != nil {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        fn["name"],
				"description": fn["description"],
				"parameters":  fn["parameters"],
			})
			continue
		}
		tools = append(tools, cloneAny(t))
	}
	if opts := asMap(in["web_search_options"]); opts != nil {
		tools = append(tools, responsesWebSearchToolFromAny(opts))
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := chatToolChoiceToResponses(in["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	carryFields(in, out, openAIFamilyCarryFields)
	if text := responseFormatToResponsesText(asMap(in["response_format"])); text != nil {
		out["text"] = text
	}
	if effort := strVal(in["reasoning_effort"]); effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
	} else if r := asMap(in["reasoning"]); r != nil {
		out["reasoning"] = cloneAny(r)
	}
	return out
}

// responseFormatToResponsesText maps Chat's response_format onto the Responses
// text.format object. The two carry the same intent with the json_schema body
// nested one level differently.
func responseFormatToResponsesText(rf map[string]any) map[string]any {
	if rf == nil {
		return nil
	}
	switch strVal(rf["type"]) {
	case "json_schema":
		js := asMap(rf["json_schema"])
		if js == nil {
			return map[string]any{"format": cloneAny(rf)}
		}
		format := map[string]any{"type": "json_schema"}
		for _, k := range []string{"name", "description", "schema", "strict"} {
			if v, ok := js[k]; ok && v != nil {
				format[k] = cloneAny(v)
			}
		}
		return map[string]any{"format": format}
	case "json_object", "text":
		return map[string]any{"format": map[string]any{"type": strVal(rf["type"])}}
	}
	return nil
}

// responsesTextToResponseFormat is the inverse of responseFormatToResponsesText.
func responsesTextToResponseFormat(text map[string]any) map[string]any {
	format := asMap(text["format"])
	if format == nil {
		return nil
	}
	switch strVal(format["type"]) {
	case "json_schema":
		js := map[string]any{}
		for _, k := range []string{"name", "description", "schema", "strict"} {
			if v, ok := format[k]; ok && v != nil {
				js[k] = cloneAny(v)
			}
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	case "json_object", "text":
		return map[string]any{"type": strVal(format["type"])}
	}
	return nil
}

func chatContentToResponses(content any, assistant bool) any {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	textType := "input_text"
	if assistant {
		textType = "output_text"
	}
	var parts []any
	for _, raw := range asSlice(content) {
		p := asMap(raw)
		if p == nil {
			continue
		}
		switch strVal(p["type"]) {
		case "text":
			parts = append(parts, map[string]any{"type": textType, "text": strVal(p["text"])})
		case "image_url":
			url := ""
			if img := asMap(p["image_url"]); img != nil {
				url = strVal(img["url"])
			} else {
				url = strVal(p["image_url"])
			}
			parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
		default:
			parts = append(parts, cloneAny(p))
		}
	}
	if len(parts) == 0 {
		return flattenChatContent(content)
	}
	return parts
}

func chatToolChoiceToResponses(raw any) any {
	if raw == nil {
		return nil
	}
	if s, ok := raw.(string); ok {
		return s
	}
	m := asMap(raw)
	if m == nil {
		return raw
	}
	if strVal(m["type"]) == "function" {
		name := strVal(m["name"])
		if fn := asMap(m["function"]); fn != nil && name == "" {
			name = strVal(fn["name"])
		}
		if name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return cloneAny(m)
}

func responsesToChatRequest(in map[string]any) map[string]any {
	out := map[string]any{
		"model":  in["model"],
		"stream": boolVal(in["stream"]),
	}
	if n := firstPositiveInt(in["max_tokens"], in["max_output_tokens"]); n > 0 {
		out["max_tokens"] = n
	}
	if in["temperature"] != nil {
		out["temperature"] = in["temperature"]
	}
	if in["top_p"] != nil {
		out["top_p"] = in["top_p"]
	}

	var msgs []any
	if inst := flattenChatContent(in["instructions"]); inst != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}
	switch input := in["input"].(type) {
	case string:
		if input != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, raw := range input {
			m := asMap(raw)
			if m == nil {
				continue
			}
			typ := strVal(m["type"])
			switch typ {
			case "function_call":
				msgs = append(msgs, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{
						map[string]any{
							"id":   firstNonEmpty(strVal(m["call_id"]), strVal(m["id"])),
							"type": "function",
							"function": map[string]any{
								"name":      strVal(m["name"]),
								"arguments": stringifyArgs(m["arguments"]),
							},
						},
					},
				})
			case "function_call_output":
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": firstNonEmpty(strVal(m["call_id"]), strVal(m["id"])),
					"content":      flattenChatContent(m["output"]),
				})
			default:
				role := strVal(m["role"])
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{
					"role":    role,
					"content": responsesContentToChat(m["content"]),
				})
			}
		}
	}
	out["messages"] = msgs

	var tools []any
	var searchOpts map[string]any
	for _, raw := range asSlice(in["tools"]) {
		t := asMap(raw)
		if t == nil {
			continue
		}
		if isWebSearchTool(t) {
			searchOpts = webSearchOptionsFromAnthropicTool(t)
			if s := strVal(t["search_context_size"]); s != "" {
				if searchOpts == nil {
					searchOpts = map[string]any{}
				}
				searchOpts["search_context_size"] = s
			}
			tools = append(tools, map[string]any{"type": firstNonEmpty(strVal(t["type"]), openaiWebSearch)})
			continue
		}
		if isBuiltInTool(t) {
			if b := passthroughBuiltInTool(t); b != nil {
				tools = append(tools, b)
			}
			continue
		}
		if strVal(t["type"]) == "function" || t["name"] != nil {
			fn := map[string]any{"name": t["name"]}
			if t["description"] != nil {
				fn["description"] = t["description"]
			}
			if t["parameters"] != nil {
				fn["parameters"] = t["parameters"]
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
			continue
		}
		tools = append(tools, cloneAny(t))
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if len(searchOpts) > 0 {
		out["web_search_options"] = searchOpts
	}
	if tc := in["tool_choice"]; tc != nil {
		out["tool_choice"] = responsesToolChoiceToChat(tc)
	}
	carryFields(in, out, openAIFamilyCarryFields)
	// Responses takes top_logprobs on its own; Chat Completions rejects it
	// unless logprobs is true. Carrying one without the other turns a request
	// the caller was allowed to make into an upstream 400 — once per candidate
	// provider, since the rewrite happens inside the retry loop.
	if _, ok := out["top_logprobs"]; ok {
		out["logprobs"] = true
	}
	if rf := responsesTextToResponseFormat(asMap(in["text"])); rf != nil {
		out["response_format"] = rf
	}
	if r := asMap(in["reasoning"]); r != nil {
		if effort := strVal(r["effort"]); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	enableChatStreamUsage(out)
	return out
}

func responsesContentToChat(content any) any {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	var parts []any
	var texts []string
	onlyText := true
	for _, raw := range asSlice(content) {
		p := asMap(raw)
		if p == nil {
			continue
		}
		switch strVal(p["type"]) {
		case "input_text", "output_text", "text":
			s := strVal(p["text"])
			texts = append(texts, s)
			parts = append(parts, map[string]any{"type": "text", "text": s})
		case "input_image":
			onlyText = false
			url := p["image_url"]
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		default:
			onlyText = false
			parts = append(parts, cloneAny(p))
		}
	}
	if onlyText {
		return strings.Join(texts, "")
	}
	if len(parts) == 0 {
		return flattenChatContent(content)
	}
	return parts
}

func responsesToolChoiceToChat(raw any) any {
	if s, ok := raw.(string); ok {
		return s
	}
	m := asMap(raw)
	if m == nil {
		return raw
	}
	if strVal(m["type"]) == "function" {
		name := strVal(m["name"])
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return cloneAny(m)
}

func normalizeStop(v any) []string {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok && s != "" {
		return []string{s}
	}
	var out []string
	for _, item := range asSlice(v) {
		if s := strVal(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringifyArgs(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return "{}"
		}
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func splitDataURL(url string) (media, data string) {
	// data:image/png;base64,XXXX
	rest := strings.TrimPrefix(url, "data:")
	media = "image/jpeg"
	if i := strings.Index(rest, ";base64,"); i >= 0 {
		if i > 0 {
			media = rest[:i]
		}
		data = rest[i+len(";base64,"):]
		return media, data
	}
	if i := strings.Index(rest, ","); i >= 0 {
		if i > 0 {
			media = strings.TrimSuffix(rest[:i], ";base64")
		}
		data = rest[i+1:]
	}
	return media, data
}
