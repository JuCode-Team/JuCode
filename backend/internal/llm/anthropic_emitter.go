// anthropic_emitter.go: response-side translation. Consumes
// upstream OpenAI Chat-Completions SSE, emits Anthropic Messages-API
// SSE events (or buffers + emits a single JSON for non-stream
// clients), and feeds the shared Accumulator so the billing path
// receives accurate token counts.
package llm

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jucode/jucode/internal/biz"
)

// --- emitter: chat-completions SSE → anthropic SSE (or buffered for non-stream) ---
//
// State machine: Anthropic's wire format requires content_block_start /
// content_block_delta / content_block_stop to bracket each block, with an
// `index` counter that only increases. We may emit any mix of three block
// types per turn:
//
//   - text       (assistant chat text)
//   - thinking   (reasoning_content from upstream)
//   - tool_use   (one per function call; arguments stream as input_json_delta)
//
// blockKind tracks what's currently open so that switching kinds correctly
// closes the prior block before opening the new one.

type blockKind int

const (
	blockNone blockKind = iota
	blockText
	blockThinking
	blockToolUse
)

type toolBlockState struct {
	id    string          // anthropic block id (mirrors upstream tool call id)
	name  string          // function name
	args  strings.Builder // accumulated arguments JSON for non-stream replay
	index int             // anthropic content-block index
}

type anthropicEmitter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	msgID   string

	// streaming state. sseArmed means the client asked for a stream.
	// headersSent means writeSSEHeaders has already flushed the status line.
	sseArmed         bool
	headersSent      bool
	messageStartSent bool

	curKind      blockKind
	curIndex     int             // anthropic content-block index of the currently-open block
	curToolState *toolBlockState // pointer identity for the open tool_use block (nil when curKind != toolUse)

	bufText     bytes.Buffer // text-block buffer (also used by non-stream replay)
	bufThinking bytes.Buffer // thinking-block buffer for non-stream replay
	toolsByIdx  map[int]*toolBlockState
	toolOrder   []*toolBlockState // preserves chat tool_calls[index] order for non-stream

	stopReason   string
	inputTokens  int
	outputTokens int
	cachedTokens int

	// Built-in web-search results and text citations forwarded from an
	// OpenAI-compatible upstream. Search blocks are emitted as Anthropic
	// web_search_tool_result content blocks; citations attach to the text
	// block (or as citations_delta if text is already open).
	searchBlocks []any
	citations    any
	textOpened   bool
}

func newAnthropicEmitter(w http.ResponseWriter, model string) *anthropicEmitter {
	f, _ := w.(http.Flusher)
	return &anthropicEmitter{
		w:          w,
		flusher:    f,
		model:      model,
		msgID:      "msg_" + randomMessageID(),
		toolsByIdx: make(map[int]*toolBlockState),
	}
}

// writeSSEHeaders commits the SSE status line. The first packet the client
// sees is this header flush, which happens when the upstream has already
// accepted the request, not when the model emits its first event.
func (e *anthropicEmitter) writeSSEHeaders() {
	if e.headersSent {
		return
	}
	e.sseArmed = true
	commitDownstreamSSE(e.w)
	e.headersSent = true
}

// startSSE sends the headers the first time there is actually an event.
func (e *anthropicEmitter) startSSE() {
	if e.headersSent || !e.sseArmed {
		return
	}
	e.headersSent = true
	h := e.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	e.w.WriteHeader(http.StatusOK)
}

func (e *anthropicEmitter) writeEvent(eventType string, payload map[string]any) {
	e.startSSE()
	payload["type"] = eventType
	b, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", eventType, b)
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

// messageStart emits the message_start envelope. Subsequent content_block_*
// events are emitted lazily by openBlock/closeBlock as the upstream stream
// surfaces different block kinds. NOTE: unlike the prior implementation
// this no longer auto-opens a text block at index 0 — the first delta
// (text, thinking, or tool_use) opens its own block.
func (e *anthropicEmitter) messageStart() {
	if e.messageStartSent {
		return
	}
	e.messageStartSent = true
	e.writeEvent("message_start", map[string]any{
		"message": map[string]any{
			"id":            e.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         e.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// switchBlock ensures the currently-open block is closed (if any) and a
// fresh block of `want` kind is opened. Idempotent when want matches the
// current kind. ts is the tool state — required for tool_use, ignored
// otherwise — and identifies *which* tool block is open via pointer
// identity (multiple tool blocks may exist in one turn, each with its
// own state).
func (e *anthropicEmitter) switchBlock(want blockKind, ts *toolBlockState) {
	if e.curKind == want {
		switch want {
		case blockText, blockThinking:
			// text and thinking blocks are append-only; reuse.
			return
		case blockToolUse:
			if ts != nil && ts == e.curToolState {
				// same tool block — reuse for arguments deltas.
				return
			}
			// different tool block — fall through to close + reopen.
		}
	}
	if e.curKind != blockNone {
		e.writeEvent("content_block_stop", map[string]any{"index": e.curIndex})
		e.curIndex++
	}
	switch want {
	case blockText:
		e.curToolState = nil
		textBlock := map[string]any{"type": "text", "text": ""}
		if e.citations != nil {
			textBlock["citations"] = e.citations
		}
		e.writeEvent("content_block_start", map[string]any{
			"index":         e.curIndex,
			"content_block": textBlock,
		})
		e.textOpened = true
	case blockThinking:
		e.curToolState = nil
		e.writeEvent("content_block_start", map[string]any{
			"index":         e.curIndex,
			"content_block": anthropicThinkingBlock(""),
		})
	case blockToolUse:
		if ts != nil {
			ts.index = e.curIndex
		}
		e.curToolState = ts
		e.writeEvent("content_block_start", map[string]any{
			"index": e.curIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    nonEmptyID(ts),
				"name":  nonEmptyName(ts),
				"input": map[string]any{},
			},
		})
	}
	e.curKind = want
}

func nonEmptyID(t *toolBlockState) string {
	if t == nil {
		return ""
	}
	return t.id
}

func nonEmptyName(t *toolBlockState) string {
	if t == nil {
		return ""
	}
	return t.name
}

func (e *anthropicEmitter) textDelta(text string) {
	if text == "" {
		return
	}
	e.bufText.WriteString(text)
	e.messageStart()
	e.switchBlock(blockText, nil)
	e.writeEvent("content_block_delta", map[string]any{
		"index": e.curIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (e *anthropicEmitter) thinkingDelta(text string) {
	if text == "" {
		return
	}
	e.bufThinking.WriteString(text)
	e.messageStart()
	e.switchBlock(blockThinking, nil)
	e.writeEvent("content_block_delta", map[string]any{
		"index": e.curIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

// toolUseStart opens (or reuses) a tool_use block for the given upstream
// tool_calls[].index. Called by the chat-chunk reader when it sees a
// tool_calls chunk that carries id/name.
func (e *anthropicEmitter) toolUseStart(chatIdx int, id, name string) *toolBlockState {
	ts, ok := e.toolsByIdx[chatIdx]
	if !ok {
		ts = &toolBlockState{id: id, name: name}
		e.toolsByIdx[chatIdx] = ts
		e.toolOrder = append(e.toolOrder, ts)
	} else {
		// Late-arriving id/name (some upstreams ship the id after the
		// first arguments delta); back-fill.
		if ts.id == "" {
			ts.id = id
		}
		if ts.name == "" {
			ts.name = name
		}
	}
	e.messageStart()
	e.switchBlock(blockToolUse, ts)
	return ts
}

// toolUseDelta appends an arguments fragment. Anthropic's wire delta type
// is input_json_delta (carries `partial_json`); we forward verbatim.
func (e *anthropicEmitter) toolUseDelta(chatIdx int, argsFragment string) {
	if argsFragment == "" {
		return
	}
	ts, ok := e.toolsByIdx[chatIdx]
	if !ok {
		ts = &toolBlockState{}
		e.toolsByIdx[chatIdx] = ts
		e.toolOrder = append(e.toolOrder, ts)
	}
	ts.args.WriteString(argsFragment)
	e.messageStart()
	e.switchBlock(blockToolUse, ts)
	e.writeEvent("content_block_delta", map[string]any{
		"index": e.curIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": argsFragment},
	})
}

// webSearchBlock emits a one-shot Anthropic web_search_tool_result content
// block (start+stop) so built-in search results from a Chat Completions
// upstream are not dropped on the Messages path.
func (e *anthropicEmitter) webSearchBlock(item map[string]any) {
	if item == nil {
		return
	}
	block := webSearchCallToAnthropic(item)
	e.searchBlocks = append(e.searchBlocks, block)
	e.messageStart()
	if e.curKind != blockNone {
		e.writeEvent("content_block_stop", map[string]any{"index": e.curIndex})
		e.curIndex++
		e.curKind = blockNone
		e.curToolState = nil
	}
	e.writeEvent("content_block_start", map[string]any{
		"index":         e.curIndex,
		"content_block": block,
	})
	e.writeEvent("content_block_stop", map[string]any{"index": e.curIndex})
	e.curIndex++
}

// addCitations attaches citation/annotation objects to the in-flight text
// block (citations_delta) or stashes them for the next text start / non-stream
// replay.
func (e *anthropicEmitter) addCitations(c any) {
	if c == nil {
		return
	}
	if s, ok := c.([]any); ok && len(s) == 0 {
		return
	}
	e.citations = c
	if e.headersSent && e.curKind == blockText {
		for _, item := range asSlice(c) {
			e.writeEvent("content_block_delta", map[string]any{
				"index": e.curIndex,
				"delta": map[string]any{"type": "citations_delta", "citation": item},
			})
		}
	}
}

func absorbChatSearchAndCitations(e *anthropicEmitter, payload []byte, clientStream bool) {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return
	}
	choices := asSlice(obj["choices"])
	if len(choices) == 0 {
		return
	}
	ch := asMap(choices[0])
	for _, key := range []string{"delta", "message"} {
		m := asMap(ch[key])
		if m == nil {
			continue
		}
		if c := firstNonNil(m["citations"], m["annotations"]); c != nil {
			e.addCitations(c)
		}
		for _, raw := range asSlice(m["web_search_calls"]) {
			item := asMap(raw)
			if item == nil {
				continue
			}
			if clientStream {
				e.webSearchBlock(item)
			} else {
				e.searchBlocks = append(e.searchBlocks, webSearchCallToAnthropic(item))
			}
		}
	}
}

func (e *anthropicEmitter) finish(stopReason string) {
	// Headers may already be on the wire. An empty answer still has to emit
	// message_start / message_delta / message_stop or the SDK cannot parse it.
	if !e.sseArmed {
		return // non-stream path handles its own emit
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}
	e.stopReason = stopReason
	e.messageStart()
	if e.curKind != blockNone {
		e.writeEvent("content_block_stop", map[string]any{"index": e.curIndex})
		e.curKind = blockNone
		e.curToolState = nil
	}
	e.writeEvent("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": e.outputTokens},
	})
	e.writeEvent("message_stop", map[string]any{})
}

func (e *anthropicEmitter) flushNonStream(acc *Accumulator) {
	// Translate accumulator usage into Anthropic shape (separate input vs
	// cached). The accumulator keeps audio apart from text so each is priced
	// at its own rate, but Anthropic's shape has no audio field and counts
	// every modality in input_tokens — folding them back in is what keeps
	// this number reconcilable with the charge.
	usage := map[string]any{
		"input_tokens":                acc.TokensIn + acc.AudioTokensIn,
		"output_tokens":               acc.TokensOut + acc.AudioTokensOut,
		"cache_read_input_tokens":     acc.CachedTokensIn,
		"cache_creation_input_tokens": acc.CacheCreationTokensIn,
	}
	if acc.CacheCreation5mTokensIn > 0 || acc.CacheCreation1hTokensIn > 0 {
		usage["cache_creation"] = map[string]any{
			"ephemeral_5m_input_tokens": acc.CacheCreation5mTokensIn,
			"ephemeral_1h_input_tokens": acc.CacheCreation1hTokensIn,
		}
	}
	stop := e.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	// Build the content array reflecting whatever blocks were observed.
	// Order: thinking (if any) → text (if any) → tool_use blocks in
	// upstream tool_calls[] index order. Matches Anthropic's
	// recommended serialization for a single turn.
	content := make([]any, 0, 2+len(e.toolOrder)+len(e.searchBlocks))
	content = append(content, e.searchBlocks...)
	if e.bufThinking.Len() > 0 {
		content = append(content, anthropicThinkingBlock(e.bufThinking.String()))
	}
	if e.bufText.Len() > 0 || e.citations != nil {
		textBlock := map[string]any{
			"type": "text",
			"text": e.bufText.String(),
		}
		if e.citations != nil {
			textBlock["citations"] = e.citations
		}
		content = append(content, textBlock)
	}
	for _, t := range e.toolOrder {
		var inputObj any = map[string]any{}
		if s := t.args.String(); s != "" {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				inputObj = parsed
			} else {
				// Arguments didn't deserialize as JSON — surface the raw
				// string so the model client at least gets the call,
				// even if it has to handle the parse mismatch itself.
				inputObj = map[string]any{"_raw_arguments": s}
			}
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    t.id,
			"name":  t.name,
			"input": inputObj,
		})
	}
	// Anthropic spec requires content to be a non-empty array — fall back
	// to an empty text block when nothing was emitted (rare, e.g. tool-only
	// with zero arguments and no text).
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	resp := map[string]any{
		"id":            e.msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         e.model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         usage,
	}
	b, _ := json.Marshal(resp)
	e.w.Header().Set("Content-Type", "application/json")
	e.w.WriteHeader(http.StatusOK)
	_, _ = e.w.Write(b)
}

// pipeChatAsAnthropic reads OpenAI Chat Completions SSE from upstream and
// either:
//   - writes Anthropic SSE events to the emitter (streaming mode), or
//   - buffers content into the emitter for a single JSON response
//
// In both cases it parses usage (the final chunk carries it because we
// inject stream_options.include_usage=true) and feeds the Accumulator so
// the gateway's charging path receives accurate token counts.
// onPolicyFrame is an optional hook that gets each parsed chat chunk
// payload AFTER acc.Inspect runs and BEFORE we emit Anthropic SSE
// frames. Returning ErrPolicyBlocked stops further streaming; the
// caller is responsible for having written a terminal error to the
// client (see anthropic block-event helper).
// afterWrite, when non-nil, runs after each upstream SSE line is
// processed — the bridge's equivalent of PipeOptions.AfterWrite, used
// to reset the stream idle-timeout on every frame that keeps flowing.
func pipeChatAsAnthropic(src io.Reader, e *anthropicEmitter, acc *Accumulator, clientStream bool, onFirstFrame func(), onFirstText func(), onPolicyFrame func(payload []byte, acc *Accumulator) error, transformPayload func(payload []byte) []byte, afterWrite func()) error {
	br := bufio.NewReaderSize(src, 64<<10)
	firstFrameSeen := onFirstFrame == nil
	firstTokenSeen := onFirstText == nil
	// Same completeness contract as PipeAndExtractOpts, sharing its type so
	// the two relays cannot drift apart on what "finished" means.
	var complete streamCompleteness
	markFirstToken := func() {
		if firstTokenSeen {
			return
		}
		onFirstText()
		firstTokenSeen = true
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			isContentFrame := false
			if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(line[len("data:"):])
				complete.observe(payload)
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) && payload[0] == '{' {
					// Real content progress — drives the chunk-idle timer so
					// upstream keepalive comments can't mask a stalled stream.
					isContentFrame = true
					if !firstFrameSeen {
						onFirstFrame()
						firstFrameSeen = true
					}
					acc.Inspect(payload)
					if onPolicyFrame != nil {
						if perr := onPolicyFrame(payload, acc); perr != nil {
							return perr
						}
					}
					// Image-output gate: when the gateway has
					// AllowImageOutput=false the transform rewrites
					// image_url blocks into text "notice" content
					// BEFORE the chat-chunk → anthropic conversion
					// runs. The emitter then emits the notice as a
					// normal text_delta. Acc has already seen the
					// original frame above.
					if transformPayload != nil {
						if rewritten := transformPayload(payload); rewritten != nil {
							payload = rewritten
						}
					}
					var chunk chatChunk
					if json.Unmarshal(payload, &chunk) == nil {
						// Built-in web_search_calls / citations live on the
						// chat chunk as extra fields chatChunk historically
						// ignored. Absorb them before text so a later text
						// block can carry the citations.
						absorbChatSearchAndCitations(e, payload, clientStream)
						if len(chunk.Choices) > 0 {
							ch := chunk.Choices[0]

							// Reasoning content (DeepSeek-style extension
							// — the codex bridge surfaces upstream
							// reasoning_summary_text.delta / reasoning_text.delta
							// in this field). Emit as Anthropic thinking
							// block in stream mode; buffer for non-stream.
							if ch.Delta.ReasoningContent != "" {
								markFirstToken()
								if clientStream {
									e.thinkingDelta(ch.Delta.ReasoningContent)
								} else {
									e.bufThinking.WriteString(ch.Delta.ReasoningContent)
								}
							}

							if ch.Delta.Content != "" {
								markFirstToken()
								if clientStream {
									e.textDelta(ch.Delta.Content)
								} else {
									e.bufText.WriteString(ch.Delta.Content)
								}
							}

							// Tool calls: openai chat stream multiplexes
							// multiple parallel tool calls via tool_calls[]
							// array, where each item has an index, an id,
							// and a function {name, arguments} where
							// arguments is a streaming JSON string. We
							// route each item to a stable tool block.
							for _, tc := range ch.Delta.ToolCalls {
								if tc.ID != "" || tc.Function.Name != "" {
									markFirstToken()
									if clientStream {
										e.toolUseStart(tc.Index, tc.ID, tc.Function.Name)
									} else {
										// Non-stream: just stash the id/name
										// in toolOrder so flushNonStream
										// can replay them.
										if _, ok := e.toolsByIdx[tc.Index]; !ok {
											ts := &toolBlockState{id: tc.ID, name: tc.Function.Name}
											e.toolsByIdx[tc.Index] = ts
											e.toolOrder = append(e.toolOrder, ts)
										}
									}
								}
								if tc.Function.Arguments != "" {
									markFirstToken()
									if clientStream {
										e.toolUseDelta(tc.Index, tc.Function.Arguments)
									} else {
										ts, ok := e.toolsByIdx[tc.Index]
										if !ok {
											ts = &toolBlockState{}
											e.toolsByIdx[tc.Index] = ts
											e.toolOrder = append(e.toolOrder, ts)
										}
										ts.args.WriteString(tc.Function.Arguments)
									}
								}
							}

							if ch.FinishReason != "" && e.stopReason == "" {
								e.stopReason = mapFinishReason(ch.FinishReason)
							}
						}
						if chunk.Usage != nil {
							e.inputTokens = chunk.Usage.PromptTokens
							e.outputTokens = chunk.Usage.CompletionTokens
							if chunk.Usage.PromptTokensDetails != nil {
								e.cachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
							}
						}
					}
				}
			}
			if isContentFrame && afterWrite != nil {
				afterWrite()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := complete.err(); err != nil {
		return err
	}
	if clientStream {
		e.finish(e.stopReason)
	}
	return nil
}

// chatChunk mirrors the OpenAI Chat-Completions streaming chunk shape with
// the two extensions we depend on:
//
//   - delta.reasoning_content (DeepSeek-style chain-of-thought stream that
//     the codex bridge populates from upstream reasoning_*.delta events)
//   - delta.tool_calls[]      (standard function-calling stream, multi-call
//     multiplexed by the .index field)
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Role             string         `json:"role"`
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content,omitempty"`
			ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}

type chatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// mapFinishReason translates OpenAI finish_reason values to Anthropic's
// stop_reason vocabulary. Unknown values default to "end_turn" — the
// most benign client-visible signal.
func mapFinishReason(r string) string {
	switch r {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	}
	return "end_turn"
}

// anthropicError builds the Anthropic-shape error envelope used by the
// HandleMessages handler. Kind is the Anthropic error type token
// ("authentication_error", "invalid_request_error", ...).
func anthropicError(kind, msg string, requestID ...string) map[string]any {
	err := map[string]any{
		"type":    kind,
		"message": msg,
	}
	out := map[string]any{
		"type":  "error",
		"error": err,
	}
	if len(requestID) > 0 && requestID[0] != "" {
		err["request_id"] = requestID[0]
		out["request_id"] = requestID[0]
	}
	return out
}

// randomMessageID returns the 24-char hex tail used after "msg_" in the
// emitted Anthropic message id. Falls back to time-derived bytes only on
// rand.Read failure — for correctness it's enough that the id be unique
// within a process, not cryptographically strong.
func randomMessageID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// Pseudo-random fallback rather than failing the request.
		ns := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(ns >> (i * 8))
		}
	}
	return hex.EncodeToString(b)
}

// Compile-time guards so future edits don't break the imports.
var (
	_ = biz.IsStickyDisabled
)
