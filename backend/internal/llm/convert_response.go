package llm

import (
	"encoding/json"
	"strings"
	"time"
)

func chatToAnthropicResponse(in map[string]any) map[string]any {
	id := firstNonEmpty(strVal(in["id"]), "msg_"+randomMessageID())
	model := strVal(in["model"])
	stop := "end_turn"
	var text string
	var thinking string
	var thinkingSig string
	var toolBlocks []any
	var searchBlocks []any
	var citations any

	if choices := asSlice(in["choices"]); len(choices) > 0 {
		ch := asMap(choices[0])
		if fr := strVal(ch["finish_reason"]); fr != "" {
			stop = mapFinishReason(fr)
		}
		msg := asMap(ch["message"])
		if msg == nil {
			msg = asMap(ch["delta"])
		}
		if msg != nil {
			text = flattenChatContent(msg["content"])
			thinking = firstNonEmpty(strVal(msg["reasoning_content"]), strVal(msg["reasoning"]))
			thinkingSig = firstNonEmpty(strVal(msg["reasoning_encrypted_content"]), strVal(msg["signature"]))
			citations = firstNonNil(msg["citations"], msg["annotations"])
			for _, raw := range asSlice(msg["tool_calls"]) {
				tc := asMap(raw)
				if tc == nil {
					continue
				}
				if isWebSearchTool(tc) || strVal(tc["type"]) == openaiWebSearchCall {
					searchBlocks = append(searchBlocks, webSearchCallToAnthropic(tc))
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
				if s, ok := args.(string); ok && s != "" {
					var parsed any
					if json.Unmarshal([]byte(s), &parsed) == nil {
						input = parsed
					} else {
						input = map[string]any{"_raw_arguments": s}
					}
				} else if args != nil {
					input = args
				}
				toolBlocks = append(toolBlocks, map[string]any{
					"type":  "tool_use",
					"id":    firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"])),
					"name":  name,
					"input": input,
				})
			}
			for _, raw := range asSlice(msg["web_search_calls"]) {
				if m := asMap(raw); m != nil {
					searchBlocks = append(searchBlocks, webSearchCallToAnthropic(m))
				}
			}
		}
	}

	var content []any
	content = append(content, searchBlocks...)
	if thinking != "" || thinkingSig != "" {
		block := anthropicThinkingBlock(thinking)
		if thinkingSig != "" {
			block["signature"] = thinkingSig
		}
		content = append(content, block)
	}
	if text != "" || len(content) == 0 {
		block := map[string]any{"type": "text", "text": text}
		if citations != nil {
			block["citations"] = citations
		}
		content = append(content, block)
	}
	content = append(content, toolBlocks...)

	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
	}
	attachWebSearchUsage(out, in)
	if inU := asMap(in["usage"]); inU != nil {
		out["usage"] = anthropicUsageFromMerged(inU)
		attachWebSearchUsage(out, in)
	}
	return out
}

func anthropicToChatResponse(in map[string]any) map[string]any {
	if msg := asMap(in["message"]); msg != nil && in["content"] == nil {
		in = msg
	}
	id := firstNonEmpty(strVal(in["id"]), "chatcmpl_"+randomMessageID())
	model := strVal(in["model"])
	finish := "stop"
	if sr := strVal(in["stop_reason"]); sr != "" {
		finish = mapAnthropicStop(sr)
	}

	var textParts []string
	var thinking string
	var toolCalls []any
	var searchCalls []any
	var citations any
	for _, raw := range asSlice(in["content"]) {
		b := asMap(raw)
		if b == nil {
			continue
		}
		switch strVal(b["type"]) {
		case "text":
			textParts = append(textParts, strVal(b["text"]))
			if c := firstNonNil(b["citations"], b["annotations"]); c != nil {
				citations = c
			}
		case "thinking":
			thinking += strVal(b["thinking"])
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   strVal(b["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      strVal(b["name"]),
					"arguments": stringifyArgs(b["input"]),
				},
			})
		case "server_tool_use", "web_search_tool_result", openaiWebSearchCall:
			searchCalls = append(searchCalls, cloneAny(b))
		}
	}

	msg := map[string]any{
		"role":    "assistant",
		"content": strings.Join(textParts, ""),
	}
	if thinking != "" {
		msg["reasoning_content"] = thinking
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(searchCalls) > 0 {
		msg["web_search_calls"] = searchCalls
	}
	if citations != nil {
		msg["citations"] = citations
		msg["annotations"] = citations
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	if inU := asMap(in["usage"]); inU != nil {
		out["usage"] = chatUsageFromMerged(inU)
	}
	attachWebSearchUsage(out, in)
	return out
}

func chatToResponsesResponse(in map[string]any) map[string]any {
	id := firstNonEmpty(strVal(in["id"]), "resp_"+randomMessageID())
	model := strVal(in["model"])
	status := "completed"
	var output []any
	var text string
	var thinking string
	var citations any

	if choices := asSlice(in["choices"]); len(choices) > 0 {
		ch := asMap(choices[0])
		if fr := strVal(ch["finish_reason"]); fr == "length" {
			status = "incomplete"
		}
		msg := asMap(ch["message"])
		if msg == nil {
			msg = asMap(ch["delta"])
		}
		if msg != nil {
			text = flattenChatContent(msg["content"])
			thinking = firstNonEmpty(strVal(msg["reasoning_content"]), strVal(msg["reasoning"]))
			citations = firstNonNil(msg["citations"], msg["annotations"])
			for _, raw := range asSlice(msg["web_search_calls"]) {
				output = append(output, webSearchCallToResponses(asMap(raw)))
			}
			for _, raw := range asSlice(msg["tool_calls"]) {
				tc := asMap(raw)
				if tc == nil {
					continue
				}
				if isWebSearchTool(tc) || strVal(tc["type"]) == openaiWebSearchCall {
					output = append(output, webSearchCallToResponses(tc))
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
				output = append(output, map[string]any{
					"type":      "function_call",
					"id":        firstNonEmpty(strVal(tc["id"]), "fc_"+randomMessageID()),
					"call_id":   firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"])),
					"name":      name,
					"arguments": stringifyArgs(args),
					"status":    "completed",
				})
			}
		}
	}
	if thinking != "" {
		output = append(output, map[string]any{
			"type":   "reasoning",
			"id":     id + "_reasoning",
			"status": status,
			"content": []any{
				map[string]any{"type": "summary_text", "text": thinking},
			},
		})
	}
	if text != "" || len(output) == 0 {
		block := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		if citations != nil {
			block["annotations"] = citations
			block["citations"] = citations
		}
		output = append(output, map[string]any{
			"type":    "message",
			"id":      id + "_msg",
			"status":  status,
			"role":    "assistant",
			"content": []any{block},
		})
	}

	out := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
	}
	if inU := asMap(in["usage"]); inU != nil {
		out["usage"] = responsesUsageFromMerged(inU)
	}
	attachWebSearchUsage(out, in)
	return out
}

func responsesToChatResponse(in map[string]any) map[string]any {
	id := firstNonEmpty(strVal(in["id"]), "chatcmpl_"+randomMessageID())
	model := strVal(in["model"])
	finish := "stop"
	if strVal(in["status"]) == "incomplete" {
		finish = "length"
	}

	var textParts []string
	var thinking string
	var thinkingSig string
	var toolCalls []any
	var searchCalls []any
	var citations any
	for _, raw := range asSlice(in["output"]) {
		item := asMap(raw)
		if item == nil {
			continue
		}
		switch strVal(item["type"]) {
		case "message":
			for _, c := range asSlice(item["content"]) {
				cm := asMap(c)
				if cm == nil {
					continue
				}
				typ := strVal(cm["type"])
				if typ == "output_text" || typ == "text" {
					textParts = append(textParts, strVal(cm["text"]))
					if cit := firstNonNil(cm["citations"], cm["annotations"]); cit != nil {
						citations = cit
					}
				}
			}
		case "reasoning":
			for _, c := range asSlice(item["content"]) {
				cm := asMap(c)
				if cm == nil {
					continue
				}
				thinking += firstNonEmpty(strVal(cm["text"]), strVal(cm["reasoning"]))
			}
			if enc := strVal(item["encrypted_content"]); enc != "" {
				thinkingSig = enc
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":   firstNonEmpty(strVal(item["call_id"]), strVal(item["id"])),
				"type": "function",
				"function": map[string]any{
					"name":      strVal(item["name"]),
					"arguments": stringifyArgs(item["arguments"]),
				},
			})
			finish = "tool_calls"
		case openaiWebSearchCall, "web_search_tool_result", "server_tool_use":
			searchCalls = append(searchCalls, cloneAny(item))
		}
	}

	msg := map[string]any{
		"role":    "assistant",
		"content": strings.Join(textParts, ""),
	}
	if thinking != "" {
		msg["reasoning_content"] = thinking
	}
	if thinkingSig != "" {
		msg["reasoning_encrypted_content"] = thinkingSig
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(searchCalls) > 0 {
		msg["web_search_calls"] = searchCalls
	}
	if citations != nil {
		msg["citations"] = citations
		msg["annotations"] = citations
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	if inU := asMap(in["usage"]); inU != nil {
		out["usage"] = chatUsageFromMerged(inU)
	}
	attachWebSearchUsage(out, in)
	return out
}

func webSearchCallToAnthropic(tc map[string]any) map[string]any {
	if tc == nil {
		return map[string]any{"type": "web_search_tool_result"}
	}
	typ := strVal(tc["type"])
	if typ == "server_tool_use" || typ == "web_search_tool_result" {
		if m := asMap(cloneAny(tc)); m != nil {
			return m
		}
	}
	out := map[string]any{
		"type": "web_search_tool_result",
		"id":   firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"])),
	}
	if tc["content"] != nil {
		out["content"] = tc["content"]
	}
	if tc["action"] != nil {
		out["action"] = tc["action"]
	}
	if tc["citations"] != nil {
		out["citations"] = tc["citations"]
	}
	if tc["results"] != nil {
		out["content"] = tc["results"]
	}
	return out
}

func webSearchCallToResponses(tc map[string]any) map[string]any {
	if tc == nil {
		return map[string]any{"type": openaiWebSearchCall, "status": "completed"}
	}
	if strVal(tc["type"]) == openaiWebSearchCall {
		if m := asMap(cloneAny(tc)); m != nil {
			return m
		}
	}
	out := map[string]any{
		"type":   openaiWebSearchCall,
		"id":     firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"])),
		"status": "completed",
	}
	if tc["action"] != nil {
		out["action"] = tc["action"]
	}
	if tc["content"] != nil {
		out["content"] = tc["content"]
	}
	if tc["citations"] != nil {
		out["citations"] = tc["citations"]
	}
	if tc["results"] != nil {
		out["content"] = tc["results"]
	}
	return out
}

func mapAnthropicStop(sr string) string {
	switch sr {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v == nil {
			continue
		}
		if s, ok := v.([]any); ok && len(s) == 0 {
			continue
		}
		return v
	}
	return nil
}
