// anthropic_translate.go: convert Anthropic Messages-API request
// bodies into OpenAI Chat-Completions bodies before forwarding, and
// the inverse "rewrite model name per provider" helper. The shape
// here is request-side only; response-side translation lives in
// anthropic_emitter.go.
package llm

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/jucode/jucode/internal/biz"
)

// --- anthropic types (only what we read) ---

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      json.RawMessage    `json:"system"`
	Messages    []anthropicMessage `json:"messages"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Stop        []string           `json:"stop_sequences,omitempty"`
	Tools       []map[string]any   `json:"tools,omitempty"`
	ToolChoice  json.RawMessage    `json:"tool_choice,omitempty"`
	Metadata    map[string]any     `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// forceChatStreamWithUsage makes a Chat Completions body stream upstream and
// report token counts on its terminal chunk. The Anthropic bridge always
// pipes the upstream response — a stream:false client is served by buffering
// — so both flags are set regardless of what the client asked for. Without
// include_usage the terminal chunk carries no usage block and the request
// bills as zero.
//
// Callers must not make this conditional. ConvertRequest's own
// enableChatStreamUsage looks like it covers the same ground but runs while
// the body still holds the CLIENT's stream flag, so it skips exactly the
// requests this has to catch.
func forceChatStreamWithUsage(chatBody []byte) ([]byte, error) {
	var body map[string]any
	// A nil map survives Unmarshal (the literal `null`) and panics on write.
	if json.Unmarshal(chatBody, &body) != nil || body == nil {
		return chatBody, nil
	}
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	return json.Marshal(body)
}

// anthropicToChatBody converts an Anthropic Messages-API request body to
// an OpenAI Chat Completions body, preserving system, messages, temperature
// and stream flags. Content blocks are flattened to text (vision/tool blocks
// fall back to their text where present).
func anthropicToChatBody(in *anthropicRequest, stream bool) ([]byte, error) {
	out := map[string]any{
		"model":  in.Model,
		"stream": stream,
	}
	if in.MaxTokens > 0 {
		out["max_tokens"] = in.MaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.Stop) > 0 {
		out["stop"] = in.Stop
	}
	// stream_options.include_usage so the upstream final chunk carries
	// token counts we can map back into Anthropic's usage block.
	if stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	msgs := make([]map[string]any, 0, len(in.Messages)+1)

	// system: anthropic accepts either a string or an array of content blocks.
	if len(in.System) > 0 {
		if sys := flattenAnthropicSystem(in.System); sys != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": sys})
		}
	}

	for _, m := range in.Messages {
		converted := convertAnthropicMessage(m)
		msgs = append(msgs, converted...)
	}
	out["messages"] = msgs

	// Tools schema conversion.
	//
	//   Anthropic: [{name, description, input_schema}]
	//   OpenAI:    [{type:"function", function:{name, description, parameters}}]
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		var searchOpts map[string]any
		for _, t := range in.Tools {
			if isWebSearchTool(t) {
				searchOpts = webSearchOptionsFromAnthropicTool(t)
				tools = append(tools, map[string]any{"type": openaiWebSearch})
				continue
			}
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if desc, ok := t["description"].(string); ok && desc != "" {
				fn["description"] = desc
			}
			if schema, ok := t["input_schema"]; ok {
				fn["parameters"] = schema
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
		if len(searchOpts) > 0 {
			out["web_search_options"] = searchOpts
		}
	}

	// tool_choice mapping.
	//
	//   {type:"auto"}              → "auto"
	//   {type:"any"}               → "required"
	//   {type:"none"}              → "none"
	//   {type:"tool", name:"foo"}  → {type:"function", function:{name:"foo"}}
	if tc := convertToolChoice(in.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}

	return json.Marshal(out)
}

// convertAnthropicMessage handles one inbound Anthropic message and returns
// the one-or-more OpenAI chat messages it expands into. A user message
// carrying tool_result blocks fans out into multiple role=tool messages
// (one per result); an assistant message with mixed text + tool_use blocks
// becomes a single assistant message with both content and tool_calls.
func convertAnthropicMessage(m anthropicMessage) []map[string]any {
	// Try array content first — that's where tool_use / tool_result /
	// images live. Fall back to string content for the legacy text path.
	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil || len(blocks) == 0 {
		var asStr string
		if json.Unmarshal(m.Content, &asStr) == nil && asStr != "" {
			return []map[string]any{{"role": m.Role, "content": asStr}}
		}
		return nil
	}

	// First pass: scan for tool_result / tool_use / image presence.
	var hasToolResult, hasToolUse, hasImage bool
	for _, b := range blocks {
		switch t, _ := b["type"].(string); t {
		case "tool_result":
			hasToolResult = true
		case "tool_use":
			hasToolUse = true
		case "image":
			hasImage = true
		}
	}

	// user message with tool_result blocks: fan out into separate tool
	// messages, one per result. Any remaining non-tool_result blocks
	// (text/image) get emitted as a trailing user message so the
	// conversation order matches Anthropic's intent.
	if m.Role == "user" && hasToolResult {
		out := make([]map[string]any, 0, len(blocks))
		var leftover []map[string]any
		for _, b := range blocks {
			t, _ := b["type"].(string)
			if t != "tool_result" {
				leftover = append(leftover, b)
				continue
			}
			toolID, _ := b["tool_use_id"].(string)
			content := stringifyToolResultContent(b["content"])
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": toolID,
				"content":      content,
			})
		}
		if len(leftover) > 0 {
			// Render remaining blocks as a separate user message; reuse
			// the same conversion path by repackaging into a synthetic
			// anthropicMessage.
			raw, _ := json.Marshal(leftover)
			tail := convertAnthropicMessage(anthropicMessage{Role: "user", Content: raw})
			out = append(out, tail...)
		}
		return out
	}

	// assistant message with tool_use blocks: emit one assistant message
	// carrying both text content (if any) and tool_calls array.
	if m.Role == "assistant" && hasToolUse {
		var textBuf string
		var toolCalls []map[string]any
		for _, b := range blocks {
			switch t, _ := b["type"].(string); t {
			case "text":
				s, _ := b["text"].(string)
				textBuf += s
			case "tool_use":
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				args := ""
				if input, ok := b["input"]; ok {
					raw, _ := json.Marshal(input)
					args = string(raw)
				}
				if args == "" {
					args = "{}"
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				})
			}
		}
		msg := map[string]any{"role": "assistant"}
		if textBuf != "" {
			msg["content"] = textBuf
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return []map[string]any{msg}
	}

	// Multimodal user message (image present): the existing parts
	// converter handles it.
	if hasImage {
		raw, _ := json.Marshal(blocks)
		if parts := anthropicContentToOpenAIParts(raw); parts != nil {
			return []map[string]any{{"role": m.Role, "content": parts}}
		}
	}

	// Plain text path: concat all text blocks.
	raw, _ := json.Marshal(blocks)
	text := flattenAnthropicContent(raw)
	if text == "" {
		return nil
	}
	return []map[string]any{{"role": m.Role, "content": text}}
}

// stringifyToolResultContent normalizes Anthropic's tool_result.content
// (which can be a string or an array of {type:"text"|"image"} blocks) into
// a single string for the OpenAI tool message slot. Image blocks inside a
// tool_result are not representable in the OpenAI tool role; drop them
// with a sentinel so the model knows something was elided.
func stringifyToolResultContent(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if arr, ok := v.([]any); ok {
		var sb strings.Builder
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := m["type"].(string); t {
			case "text":
				if s, ok := m["text"].(string); ok {
					sb.WriteString(s)
				}
			case "image":
				sb.WriteString("[image omitted]")
			}
		}
		return sb.String()
	}
	// Catch-all: serialize as JSON so the model still sees structured
	// content rather than an empty string.
	raw, _ := json.Marshal(v)
	return string(raw)
}

// convertToolChoice maps Anthropic's tool_choice envelope to OpenAI's.
// Returns nil when the input is empty / unparseable so the caller can
// omit the field entirely (OpenAI default = "auto" with tools present).
func convertToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	// Anthropic may send a bare string in some shims; honor it.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	t, _ := obj["type"].(string)
	switch t {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := obj["name"].(string)
		if name == "" {
			return "auto"
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		}
	}
	return nil
}

func flattenAnthropicSystem(raw json.RawMessage) string {
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return stripClaudeCodeBillingHeader(asStr)
	}
	var asArr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &asArr); err == nil {
		parts := make([]string, 0, len(asArr))
		for _, p := range asArr {
			if p.Type != "text" || p.Text == "" {
				continue
			}
			if isClaudeCodeBillingBlock(p.Text) {
				continue
			}
			parts = append(parts, p.Text)
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// isClaudeCodeBillingBlock reports whether a system block was injected by
// Claude Code ≥ 2.1.36 as the per-request billing/tracking header. Its
// `cch` field is SHA-256(message_text)[:5] and changes on every turn,
// which scrambles prompt-cache keys on upstreams that bucket on prompt
// content (e.g. when translating to another vendor). Stripping the block
// restores cache hit rate; the operator-side billing impact is none
// because we do our own accounting in usage_logs anyway.
//
// Matches the format documented by the reverse-engineering write-up at
// https://a10k.co/b/reverse-engineering-claude-code-cch.html — block
// content begins with "x-anthropic-billing-header:" followed by
// semicolon-separated key=value pairs. We match the prefix (cheap) and
// don't bother validating the cch hex shape; any future field tweak by
// Anthropic still gets the same correct treatment ("strip").
func isClaudeCodeBillingBlock(text string) bool {
	t := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(strings.ToLower(t), "x-anthropic-billing-header:")
}

// stripClaudeCodeBillingHeader is the string-form sibling of
// isClaudeCodeBillingBlock — used when a client sent the system field as
// a single string instead of an array of blocks. We remove the header
// line and any whitespace immediately following it; if the entire system
// string is the header we return "".
func stripClaudeCodeBillingHeader(s string) string {
	const marker = "x-anthropic-billing-header:"
	lower := strings.ToLower(s)
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return s
	}
	// Find the end of the header line: bounded by newline or by the
	// closing ";" + newline. We accept the line ending at the next
	// LF; Claude Code's serializer emits a single line then either
	// concatenates the next block or ends the string.
	end := strings.IndexByte(s[idx:], '\n')
	if end < 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s[:idx] + s[idx+end+1:])
}

func flattenAnthropicContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return asStr
	}
	var asArr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &asArr); err == nil {
		parts := make([]string, 0, len(asArr))
		for _, p := range asArr {
			if p.Type == "text" && p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// rewriteModel swaps only the top-level "model" field. Nested values stay
// raw bytes: rebuilding them HTML-escapes text and changes the prefix the
// upstream uses for prompt cache, which turns a warm TTFT into a full prefill.
// An already-correct model is returned untouched.
func rewriteModel(body []byte, upstreamModel string) ([]byte, error) {
	want, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(raw["model"]), want) {
		return body, nil
	}
	raw["model"] = want
	return marshalJSON(raw)
}

// RewriteTopLevelModel is rewriteModel for callers outside this package.
func RewriteTopLevelModel(body []byte, upstreamModel string) ([]byte, error) {
	return rewriteModel(body, upstreamModel)
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func anthropicNativeBody(body []byte, upstreamModel string, definitionID string) ([]byte, error) {
	body, err := rewriteModel(body, upstreamModel)
	if err != nil {
		return nil, err
	}
	if definitionID == biz.AzureFoundryClaudeDefinitionID {
		return adaptAzureFoundryClaudeBody(body, upstreamModel)
	}
	return body, nil
}

// anthropicContentToOpenAIParts converts an Anthropic content payload
// (string OR array of typed blocks) into OpenAI Chat's multipart
// content array — used only when the message contains at least one
// image block. Returns nil when there are no images (so the caller
// can fall through to the simpler flat-text path that preserves
// byte-identical behavior for plain chats).
//
// Per-block mapping:
//
//	{type:"text", text:"…"}
//	   → {type:"text", text:"…"}
//
//	{type:"image", source:{type:"base64", media_type:"image/png", data:"…"}}
//	   → {type:"image_url", image_url:{url:"data:image/png;base64,…"}}
//
//	{type:"image", source:{type:"url", url:"https://…"}}
//	   → {type:"image_url", image_url:{url:"https://…"}}
//
// Unknown / tool / thinking blocks are dropped — they have no
// universally-meaningful OpenAI counterpart and the rest of the
// gateway doesn't depend on them for routing/billing.
func anthropicContentToOpenAIParts(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	hasImage := false
	for _, b := range arr {
		if t, _ := b["type"].(string); t == "image" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, b := range arr {
		t, _ := b["type"].(string)
		switch t {
		case "text":
			text, _ := b["text"].(string)
			if text == "" {
				continue
			}
			out = append(out, map[string]any{"type": "text", "text": text})
		case "image":
			src, _ := b["source"].(map[string]any)
			if src == nil {
				continue
			}
			switch s, _ := src["type"].(string); s {
			case "base64":
				media, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				if data == "" {
					continue
				}
				if media == "" {
					media = "image/jpeg"
				}
				url := "data:" + media + ";base64," + data
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			case "url":
				url, _ := src["url"].(string)
				if url == "" {
					continue
				}
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		}
		// silently drop other block types (tool_use, thinking, etc)
	}
	return out
}
