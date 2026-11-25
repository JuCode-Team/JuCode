package llm

import "strings"

const (
	anthropicWebSearchType = "web_search_20250305"
	anthropicWebSearchName = "web_search"
	openaiWebSearchPreview = "web_search_preview"
	openaiWebSearch        = "web_search"
	openaiWebSearchCall    = "web_search_call"

	webSearchMaxUsesLow    = 1
	webSearchMaxUsesMedium = 5
	webSearchMaxUsesHigh   = 10
)

func isWebSearchToolType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case openaiWebSearch, openaiWebSearchPreview, anthropicWebSearchType, openaiWebSearchCall:
		return true
	default:
		return false
	}
}

func isWebSearchTool(t map[string]any) bool {
	if t == nil {
		return false
	}
	if isWebSearchToolType(strVal(t["type"])) {
		return true
	}
	if strings.EqualFold(strVal(t["name"]), anthropicWebSearchName) && isWebSearchToolType(strVal(t["type"])) {
		return true
	}
	if fn := asMap(t["function"]); fn != nil {
		if strings.EqualFold(strVal(fn["name"]), anthropicWebSearchName) {
			return true
		}
	}
	return strings.EqualFold(strVal(t["name"]), anthropicWebSearchName) && t["input_schema"] == nil && t["parameters"] == nil
}

func webSearchMaxUses(size string) int {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "low":
		return webSearchMaxUsesLow
	case "high":
		return webSearchMaxUsesHigh
	case "medium":
		return webSearchMaxUsesMedium
	default:
		return 0
	}
}

func searchContextSizeFromMaxUses(n int) string {
	switch {
	case n <= 0:
		return ""
	case n <= webSearchMaxUsesLow:
		return "low"
	case n >= webSearchMaxUsesHigh:
		return "high"
	default:
		return "medium"
	}
}

func anthropicUserLocationFromOpenAI(loc any) map[string]any {
	m := asMap(loc)
	if m == nil {
		return nil
	}
	approx := asMap(m["approximate"])
	if approx == nil {
		approx = m
	}
	out := map[string]any{"type": "approximate"}
	copied := false
	for _, k := range []string{"timezone", "country", "region", "city"} {
		if s := strVal(approx[k]); s != "" {
			out[k] = s
			copied = true
		}
	}
	if !copied {
		return nil
	}
	return out
}

func openaiUserLocationFromAnthropic(loc any) map[string]any {
	m := asMap(loc)
	if m == nil {
		return nil
	}
	approx := map[string]any{}
	copied := false
	for _, k := range []string{"timezone", "country", "region", "city"} {
		if s := strVal(m[k]); s != "" {
			approx[k] = s
			copied = true
		}
	}
	if !copied {
		return nil
	}
	return map[string]any{"type": "approximate", "approximate": approx}
}

func anthropicWebSearchToolFromOptions(opts map[string]any) map[string]any {
	tool := map[string]any{
		"type": anthropicWebSearchType,
		"name": anthropicWebSearchName,
	}
	if opts == nil {
		return tool
	}
	if n := webSearchMaxUses(strVal(opts["search_context_size"])); n > 0 {
		tool["max_uses"] = n
	}
	if loc := anthropicUserLocationFromOpenAI(opts["user_location"]); loc != nil {
		tool["user_location"] = loc
	}
	return tool
}

func anthropicWebSearchToolFromAny(t map[string]any) map[string]any {
	tool := map[string]any{
		"type": anthropicWebSearchType,
		"name": anthropicWebSearchName,
	}
	if t == nil {
		return tool
	}
	if n := intVal(t["max_uses"]); n > 0 {
		tool["max_uses"] = n
	} else if n := webSearchMaxUses(strVal(t["search_context_size"])); n > 0 {
		tool["max_uses"] = n
	}
	if loc := t["user_location"]; loc != nil {
		if mapped := anthropicUserLocationFromOpenAI(loc); mapped != nil {
			tool["user_location"] = mapped
		} else if am := asMap(loc); am != nil {
			tool["user_location"] = am
		}
	}
	return tool
}

func webSearchOptionsFromAnthropicTool(t map[string]any) map[string]any {
	opts := map[string]any{}
	if t == nil {
		return opts
	}
	if size := searchContextSizeFromMaxUses(intVal(t["max_uses"])); size != "" {
		opts["search_context_size"] = size
	}
	if loc := openaiUserLocationFromAnthropic(t["user_location"]); loc != nil {
		opts["user_location"] = loc
	}
	return opts
}

func responsesWebSearchToolFromAny(t map[string]any) map[string]any {
	tool := map[string]any{"type": openaiWebSearch}
	if t == nil {
		return tool
	}
	if typ := strVal(t["type"]); typ == openaiWebSearchPreview {
		tool["type"] = openaiWebSearchPreview
	}
	if n := intVal(t["max_uses"]); n > 0 {
		if size := searchContextSizeFromMaxUses(n); size != "" {
			tool["search_context_size"] = size
		}
	}
	if s := strVal(t["search_context_size"]); s != "" {
		tool["search_context_size"] = s
	}
	if loc := t["user_location"]; loc != nil {
		if mapped := openaiUserLocationFromAnthropic(loc); mapped != nil {
			tool["user_location"] = mapped
		} else {
			tool["user_location"] = loc
		}
	}
	return tool
}

func extractWebSearchRequests(obj map[string]any) int {
	if obj == nil {
		return 0
	}
	if n := webSearchFromUsage(asMap(obj["usage"])); n > 0 {
		return n
	}
	if msg := asMap(obj["message"]); msg != nil {
		if n := webSearchFromUsage(asMap(msg["usage"])); n > 0 {
			return n
		}
	}
	if resp := asMap(obj["response"]); resp != nil {
		if n := webSearchFromUsage(asMap(resp["usage"])); n > 0 {
			return n
		}
		if n := countWebSearchCalls(asSlice(resp["output"])); n > 0 {
			return n
		}
	}
	if n := countWebSearchCalls(asSlice(obj["output"])); n > 0 {
		return n
	}
	if n := countWebSearchContent(asSlice(obj["content"])); n > 0 {
		return n
	}
	return 0
}

func webSearchFromUsage(usage map[string]any) int {
	if usage == nil {
		return 0
	}
	if n := intVal(usage["web_search_requests"]); n > 0 {
		return n
	}
	if stu := asMap(usage["server_tool_use"]); stu != nil {
		if n := intVal(stu["web_search_requests"]); n > 0 {
			return n
		}
	}
	return 0
}

func countWebSearchCalls(items []any) int {
	n := 0
	for _, item := range items {
		m := asMap(item)
		if m == nil {
			continue
		}
		typ := strVal(m["type"])
		if typ == openaiWebSearchCall || typ == "server_tool_use" || typ == "web_search_tool_result" {
			n++
		}
	}
	return n
}

func countWebSearchContent(blocks []any) int {
	n := 0
	for _, b := range blocks {
		m := asMap(b)
		if m == nil {
			continue
		}
		typ := strVal(m["type"])
		if typ == "server_tool_use" || typ == "web_search_tool_result" || typ == openaiWebSearchCall {
			n++
		}
	}
	return n
}

func copyUsageWithWebSearch(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := asMap(cloneAny(src))
	if out == nil {
		out = map[string]any{}
	}
	if n := extractWebSearchRequests(map[string]any{"usage": src}); n > 0 {
		out["web_search_requests"] = n
		if asMap(out["server_tool_use"]) == nil {
			out["server_tool_use"] = map[string]any{"web_search_requests": n}
		}
	}
	return out
}

// usageFromFrame pulls a usage object from the places Anthropic / OpenAI
// / Responses put it (flat .usage, message.usage, response.usage).
func usageFromFrame(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	if u := asMap(obj["usage"]); u != nil {
		return u
	}
	if msg := asMap(obj["message"]); msg != nil {
		if u := asMap(msg["usage"]); u != nil {
			return u
		}
	}
	if resp := asMap(obj["response"]); resp != nil {
		if u := asMap(resp["usage"]); u != nil {
			return u
		}
	}
	return nil
}

// mergeUsageMaps folds src into dst without letting a later output-only
// frame (Anthropic message_delta) zero input/cache counts already seen
// on message_start. Each numeric field keeps the max.
func mergeUsageMaps(dst, src map[string]any) map[string]any {
	if src == nil {
		return dst
	}
	src = copyUsageWithWebSearch(src)
	if dst == nil {
		return src
	}
	for _, k := range []string{
		"input_tokens", "output_tokens",
		"prompt_tokens", "completion_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
		"cached_tokens", "cache_write_tokens",
		"audio_input_tokens", "audio_output_tokens",
		"web_search_requests",
	} {
		if n := intVal(src[k]); n > intVal(dst[k]) {
			dst[k] = n
		}
	}
	// Nested details do not survive a later output-only frame unless we
	// lift their subsets onto the flat keys above. Cache is already
	// spelled that way on Anthropic; audio only ever lives in details.
	if ain, aout := mergedAudioSplit(src); ain > 0 || aout > 0 {
		if ain > intVal(dst["audio_input_tokens"]) {
			dst["audio_input_tokens"] = ain
		}
		if aout > intVal(dst["audio_output_tokens"]) {
			dst["audio_output_tokens"] = aout
		}
	}
	if stu := asMap(src["server_tool_use"]); stu != nil {
		existing := asMap(dst["server_tool_use"])
		if existing == nil {
			dst["server_tool_use"] = cloneAny(stu)
		} else if n := intVal(stu["web_search_requests"]); n > intVal(existing["web_search_requests"]) {
			existing["web_search_requests"] = n
		}
	}
	if n := intVal(dst["web_search_requests"]); n > 0 && asMap(dst["server_tool_use"]) == nil {
		dst["server_tool_use"] = map[string]any{"web_search_requests": n}
	}
	return dst
}

func attachWebSearchUsage(dst, src map[string]any) {
	n := extractWebSearchRequests(src)
	if n <= 0 {
		if usage := asMap(src["usage"]); usage != nil {
			dst["usage"] = copyUsageWithWebSearch(usage)
		}
		return
	}
	usage := asMap(dst["usage"])
	if usage == nil {
		usage = map[string]any{}
		dst["usage"] = usage
	}
	usage["web_search_requests"] = n
	usage["server_tool_use"] = map[string]any{"web_search_requests": n}
}
