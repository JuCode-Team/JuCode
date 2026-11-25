package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func convertSSE(from, to Dialect, src io.Reader, dst io.Writer) error {
	st := &streamConvState{from: from, to: to, id: "resp_" + randomMessageID(), created: time.Now().Unix()}
	br := bufio.NewReaderSize(src, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			payload, ok := sseDataPayload(line)
			if !ok {
				if _, werr := dst.Write(line); werr != nil {
					return werr
				}
			} else if bytes.Equal(payload, []byte("[DONE]")) {
				if err := st.flushDone(dst); err != nil {
					return err
				}
			} else if len(payload) > 0 && payload[0] == '{' {
				// Latch "the source said it was done" here, with the same
				// predicate the pipe uses, rather than in each direction's
				// handler. Six hand-written state machines each deciding this
				// for themselves is how anthropic→chat ended up never setting
				// it: harmless while nothing read the flag at EOF, wrong the
				// moment something did.
				if isStreamTerminalPayload(payload) {
					st.respFinished = true
				}
				if err := st.feedJSON(payload, dst); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			if ferr := st.flushEOF(dst); ferr != nil {
				return ferr
			}
			// Report a source that stopped without finishing, even though the
			// terminal frames for the client have already been written. For a
			// Chat target those frames include [DONE], which the pipe reads as
			// "this stream finished" — the only way it can learn otherwise is
			// out of band, and wrapConvertedBody turns this error into a read
			// error on the converted body. Without it, a truncated upstream
			// behind format conversion is delivered as a 200 and billed.
			if !st.respFinished {
				return errUpstreamStreamIncomplete
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func sseDataPayload(line []byte) ([]byte, bool) {
	trim := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trim, []byte("data:")) {
		return nil, false
	}
	return bytes.TrimSpace(trim[len("data:"):]), true
}

func writeSSE(dst io.Writer, event string, payload []byte) error {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(payload)
	b.WriteString("\n\n")
	_, err := io.WriteString(dst, b.String())
	return err
}

func writeSSEObj(dst io.Writer, event string, obj map[string]any) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return writeSSE(dst, event, raw)
}

type streamConvState struct {
	from, to Dialect
	id       string
	created  int64
	started  bool
	usage    map[string]any
	search   []any

	// --- Anthropic target: content block lifecycle ---
	// Anthropic's SSE contract is one content_block_start / N deltas /
	// content_block_stop per block, indices monotonic, and a block keeps its
	// type for life. Tracking open/closed and the current type is what makes a
	// text → tool_use → text sequence come out as three properly delimited
	// blocks instead of everything piled onto index 0.
	blockIndex int
	blockOpen  bool
	blockType  string
	// srcToBlock maps a source-side index (chat tool_calls[].index, Responses
	// output_index) onto the Anthropic block index opened for it.
	srcToBlock map[int]int
	// toolArgsSeen records which source tool indices already streamed argument
	// fragments, so a terminal "complete arguments" frame does not repeat them.
	toolArgsSeen map[int]bool
	hasToolUse   bool
	stopSent     bool
	doneSent     bool

	// --- Chat target: tool_calls[].index has to be dense from 0 ---
	chatToolIdx  map[int]int
	chatToolNext int

	// --- Responses target: output items ---
	// Items are addressed by the output_index they were announced at, and the
	// terminal frame's output array is those same items in that order. Holding
	// the index, the item and its fragment buffer in one struct is what keeps
	// the three from drifting: a source index that streams argument fragments
	// without ever announcing an item — an Anthropic server_tool_use does
	// exactly that — would otherwise consume a slot anyway and shift every
	// later call's arguments onto the wrong call.
	respItems []*respItem
	respBySrc map[int]*respItem
	// The message and reasoning items have no source index; they are opened on
	// the first fragment of their kind.
	respText   *respItem
	respReason *respItem

	// --- Responses target: the terminal frame is deferred ---
	// The finish signal and the usage block do not arrive together. With
	// stream_options.include_usage — which this converter asks for — OpenAI
	// reports usage on a trailing chunk with an empty choices array, *after*
	// the one carrying finish_reason. Emitting response.completed on the
	// finish signal would therefore always ship it without usage, and the
	// gateway bills from this converted stream, so the request would bill
	// zero. Latch the finish instead and write the frame at [DONE]/EOF.
	respFinished bool
	respStatus   string
	respModel    string
	respID       string
	respDoneSent bool

	// --- Anthropic target: the same trailing-usage problem ---
	//
	// message_delta is the only frame that reports usage to an Anthropic
	// client, so it is deferred to [DONE]/EOF for the reason above.
	anthStop    string
	anthRefused bool
	// Encrypted reasoning blob to emit as Anthropic signature_delta before
	// the thinking block closes. Responses carries it on output_item.done;
	// Grok Build requires the field on the thinking block (start can be
	// empty, but the delta must land before content_block_stop).
	anthThinkingSig string
}

// respItem is one announced Responses output item.
type respItem struct {
	slot int
	item map[string]any
	// buf accumulates whichever payload arrives in fragments for this kind of
	// item: argument JSON for a function call, visible text for a message,
	// summary text for reasoning.
	buf strings.Builder
}

// callID is the identifier the argument deltas have to point back at. Chat
// sends it only on a call's first fragment, so it is read off the item rather
// than off each frame.
func (ri *respItem) callID() string {
	if ri == nil || ri.item == nil {
		return ""
	}
	return firstNonEmpty(strVal(ri.item["call_id"]), strVal(ri.item["id"]))
}

// seal folds the accumulated fragments into the item and marks it complete.
func (ri *respItem) seal() {
	if ri.buf.Len() > 0 {
		switch strVal(ri.item["type"]) {
		case "message":
			ri.item["content"] = []any{map[string]any{
				"type":        "output_text",
				"text":        ri.buf.String(),
				"annotations": []any{},
			}}
		case "reasoning":
			ri.item["summary"] = []any{map[string]any{
				"type": "summary_text",
				"text": ri.buf.String(),
			}}
		default:
			ri.item["arguments"] = ri.buf.String()
		}
	}
	ri.item["status"] = "completed"
}

// closeAnthBlock ends the open Anthropic content block, if any, and advances
// the index. Single place that keeps index and open state in step.
func (s *streamConvState) closeAnthBlock(dst io.Writer) error {
	if !s.blockOpen {
		return nil
	}
	idx := s.blockIndex
	if s.blockType == "thinking" && s.anthThinkingSig != "" {
		if err := writeSSEObj(dst, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{"type": "signature_delta", "signature": s.anthThinkingSig},
		}); err != nil {
			return err
		}
		s.anthThinkingSig = ""
	}
	s.blockOpen = false
	s.blockType = ""
	s.blockIndex++
	return writeSSEObj(dst, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

// openAnthBlock starts a block of blockType, closing a block of any other type
// first. Reports the index it is streaming at.
func (s *streamConvState) openAnthBlock(dst io.Writer, blockType string, block map[string]any) (int, error) {
	if s.blockOpen && s.blockType == blockType {
		return s.blockIndex, nil
	}
	if err := s.closeAnthBlock(dst); err != nil {
		return 0, err
	}
	idx := s.blockIndex
	s.blockOpen = true
	s.blockType = blockType
	return idx, writeSSEObj(dst, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": block,
	})
}

// openAnthToolBlock opens a tool_use block for a source-side tool index.
func (s *streamConvState) openAnthToolBlock(dst io.Writer, src int, callID, name string) error {
	if err := s.closeAnthBlock(dst); err != nil {
		return err
	}
	idx := s.blockIndex
	s.blockOpen = true
	s.blockType = "tool_use"
	s.hasToolUse = true
	s.mapBlock(src, idx)
	return writeSSEObj(dst, "content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    callID,
			"name":  name,
			"input": map[string]any{},
		},
	})
}

// emitAnthWholeBlock writes a delta-free block (start immediately followed by
// stop), for the server-side tool blocks that arrive fully formed.
func (s *streamConvState) emitAnthWholeBlock(dst io.Writer, block map[string]any) error {
	if err := s.closeAnthBlock(dst); err != nil {
		return err
	}
	idx := s.blockIndex
	if err := writeSSEObj(dst, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": block,
	}); err != nil {
		return err
	}
	s.blockIndex++
	return writeSSEObj(dst, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (s *streamConvState) blockFor(src int) (int, bool) {
	idx, ok := s.srcToBlock[src]
	return idx, ok
}

// anthToolBlockFor resolves the block to address a tool fragment at, reopening
// one when the block previously opened for this source index has already been
// closed. Text arriving between two fragments of the same call closes it, and
// addressing the closed index would put a content_block_delta after its own
// content_block_stop — which no Anthropic client will accept.
func (s *streamConvState) anthToolBlockFor(dst io.Writer, src int, callID, name string) (int, error) {
	if idx, ok := s.blockFor(src); ok && s.blockOpen && idx == s.blockIndex {
		return idx, nil
	}
	if err := s.openAnthToolBlock(dst, src, callID, name); err != nil {
		return 0, err
	}
	idx, _ := s.blockFor(src)
	return idx, nil
}

func (s *streamConvState) mapBlock(src, block int) {
	if s.srcToBlock == nil {
		s.srcToBlock = map[int]int{}
	}
	s.srcToBlock[src] = block
}

// closeAnthBlockFor closes the open block only when it is the one opened for
// this source index, so a late done frame cannot truncate a later block.
func (s *streamConvState) closeAnthBlockFor(dst io.Writer, src int) error {
	if idx, ok := s.blockFor(src); ok && s.blockOpen && idx == s.blockIndex {
		return s.closeAnthBlock(dst)
	}
	return nil
}

// emitAnthCitations attaches source-side citations to the open text block.
// Anthropic carries one citation object per citations_delta, so a list has to
// be unrolled rather than forwarded whole — a client reading delta.citation as
// an object finds an array and drops the lot. Citations annotate the block they
// arrived with, so they can only be written while one is open.
func (s *streamConvState) emitAnthCitations(dst io.Writer, raw any) error {
	if raw == nil || !s.blockOpen {
		return nil
	}
	list := asSlice(raw)
	if list == nil {
		list = []any{raw}
	}
	for _, cite := range list {
		if cite == nil {
			continue
		}
		if err := writeSSEObj(dst, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "citations_delta", "citation": cloneAny(cite)},
		}); err != nil {
			return err
		}
	}
	return nil
}

// finishAnth closes any open block and writes the message_delta / message_stop
// pair exactly once, whatever mixture of upstream terminal frame, [DONE] and
// EOF arrives.
func (s *streamConvState) finishAnth(dst io.Writer, stopReason string) error {
	if !s.started || s.stopSent {
		return nil
	}
	if err := s.closeAnthBlock(dst); err != nil {
		return err
	}
	s.stopSent = true
	// A refusal outranks whatever generic finish reason the source paired it
	// with: "end_turn" would tell the caller the model answered normally.
	if s.anthRefused {
		stopReason = "refusal"
	}
	if stopReason == "" {
		stopReason = s.anthStop
	}
	if stopReason == "" {
		stopReason = "end_turn"
		if s.hasToolUse {
			stopReason = "tool_use"
		}
	}
	usage := anthropicUsageFromMerged(s.usage)
	if usage == nil {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	if err := writeSSEObj(dst, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	}); err != nil {
		return err
	}
	return writeSSEObj(dst, "message_stop", map[string]any{"type": "message_stop"})
}

// chatToolSlot assigns a dense chat tool_calls index to a source-side tool
// identifier. Chat clients accumulate fragments by that index, so it must
// start at 0 and rise by one per call; Anthropic block indices and Responses
// output_index values count other things too and cannot be reused directly.
func (s *streamConvState) chatToolSlot(src int) int {
	if s.chatToolIdx == nil {
		s.chatToolIdx = map[int]int{}
	}
	if idx, ok := s.chatToolIdx[src]; ok {
		return idx
	}
	idx := s.chatToolNext
	s.chatToolNext++
	s.chatToolIdx[src] = idx
	return idx
}

// addRespItem announces an output item for a source index. src < 0 is for the
// items that have no source index of their own (message, reasoning). Slots are
// handed out in announcement order, which is also the order the terminal
// output array reports them in.
func (s *streamConvState) addRespItem(dst io.Writer, src int, item map[string]any) (*respItem, error) {
	ri := &respItem{slot: len(s.respItems), item: item}
	s.respItems = append(s.respItems, ri)
	if src >= 0 {
		if s.respBySrc == nil {
			s.respBySrc = map[int]*respItem{}
		}
		s.respBySrc[src] = ri
	}
	return ri, writeSSEObj(dst, "response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": ri.slot,
		"item":         item,
	})
}

// respItemFor finds the item announced for a source index, nil when that index
// never announced one.
func (s *streamConvState) respItemFor(src int) *respItem {
	return s.respBySrc[src]
}

// respTextItem opens the assistant message item on its first text fragment.
//
// The terminal response.completed carries the output array, and gateway.go
// answers a non-streaming /v1/responses request out of exactly that object, so
// a converted stream that never builds this item replies HTTP 200 with an
// empty output and the model's answer is simply gone.
func (s *streamConvState) respTextItem(dst io.Writer) (*respItem, error) {
	if s.respText != nil {
		return s.respText, nil
	}
	ri, err := s.addRespItem(dst, -1, map[string]any{
		"type":    "message",
		"id":      "msg_" + randomMessageID(),
		"role":    "assistant",
		"status":  "in_progress",
		"content": []any{},
	})
	if err != nil {
		return nil, err
	}
	s.respText = ri
	return ri, nil
}

func (s *streamConvState) respReasonItem(dst io.Writer) (*respItem, error) {
	if s.respReason != nil {
		return s.respReason, nil
	}
	ri, err := s.addRespItem(dst, -1, map[string]any{
		"type":    "reasoning",
		"id":      "rs_" + randomMessageID(),
		"status":  "in_progress",
		"summary": []any{},
	})
	if err != nil {
		return nil, err
	}
	s.respReason = ri
	return ri, nil
}

func (s *streamConvState) feedJSON(payload []byte, dst io.Writer) error {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return writeSSE(dst, "", payload)
	}
	if n := extractWebSearchRequests(obj); n > 0 {
		s.usage = mergeUsageMaps(s.usage, map[string]any{"web_search_requests": n})
	}
	if u := usageFromFrame(obj); u != nil {
		s.usage = mergeUsageMaps(s.usage, u)
	}
	s.captureSearch(obj)

	switch {
	case s.from == DialectCompatible && s.to == DialectAnthropic:
		return s.chatToAnthropic(obj, dst)
	case s.from == DialectCompatible && s.to == DialectOpenAI:
		return s.chatToResponses(obj, dst)
	case s.from == DialectOpenAI && s.to == DialectCompatible:
		return s.responsesToChat(obj, dst)
	case s.from == DialectAnthropic && s.to == DialectCompatible:
		return s.anthropicToChat(obj, dst)
	case s.from == DialectOpenAI && s.to == DialectAnthropic:
		return s.responsesToAnthropic(obj, dst)
	case s.from == DialectAnthropic && s.to == DialectOpenAI:
		return s.anthropicToResponses(obj, dst)
	default:
		return writeSSE(dst, strVal(obj["type"]), payload)
	}
}

// addSearch records a built-in search item for the terminal frame, ignoring
// one it already holds. The same item legitimately surfaces more than once —
// captureSearch reads it off a terminal frame's output array while the
// per-dialect handlers read it off the item/block frame that announced it —
// and reporting it twice in output is a protocol error, not just noise.
func (s *streamConvState) addSearch(m map[string]any) {
	if m == nil {
		return
	}
	if id := strVal(m["id"]); id != "" {
		for _, seen := range s.search {
			if sm := asMap(seen); sm != nil && strVal(sm["id"]) == id {
				return
			}
		}
	}
	s.search = append(s.search, cloneAny(m))
}

func (s *streamConvState) captureSearch(obj map[string]any) {
	for _, raw := range asSlice(obj["output"]) {
		m := asMap(raw)
		if m == nil {
			continue
		}
		if isWebSearchToolType(strVal(m["type"])) || strVal(m["type"]) == "server_tool_use" || strVal(m["type"]) == "web_search_tool_result" {
			s.addSearch(m)
		}
	}
	if t := strVal(obj["type"]); t == "content_block_start" {
		if b := asMap(obj["content_block"]); b != nil {
			if isWebSearchToolType(strVal(b["type"])) || strVal(b["type"]) == "server_tool_use" || strVal(b["type"]) == "web_search_tool_result" {
				s.addSearch(b)
			}
		}
	}
}

func (s *streamConvState) chatToAnthropic(obj map[string]any, dst io.Writer) error {
	if !s.started {
		s.started = true
		if err := writeSSEObj(dst, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      firstNonEmpty(strVal(obj["id"]), "msg_"+s.id),
				"type":    "message",
				"role":    "assistant",
				"model":   strVal(obj["model"]),
				"content": []any{},
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}); err != nil {
			return err
		}
	}
	choices := asSlice(obj["choices"])
	if len(choices) == 0 {
		// The trailing usage-only chunk lands here. feedJSON has already
		// merged its usage, which is why the terminal frame waits for
		// [DONE]/EOF instead of firing on finish_reason.
		return nil
	}
	ch := asMap(choices[0])
	delta := asMap(ch["delta"])
	if delta != nil {
		if t := strVal(delta["reasoning_content"]); t != "" {
			if err := s.emitAnthDelta(dst, "thinking_delta", "thinking", t); err != nil {
				return err
			}
		}
		if t := flattenChatContent(delta["content"]); t != "" {
			if err := s.emitAnthDelta(dst, "text_delta", "text", t); err != nil {
				return err
			}
		}
		if t := strVal(delta["refusal"]); t != "" {
			// Anthropic has no refusal block, but it does have a refusal stop
			// reason. Carrying the wording as text and the fact as the stop
			// reason is the closest honest mapping; dropping it would leave
			// the caller an empty message that looks like a normal answer.
			if err := s.emitAnthDelta(dst, "text_delta", "text", t); err != nil {
				return err
			}
			s.anthRefused = true
		}
		for _, raw := range asSlice(delta["web_search_calls"]) {
			item := asMap(raw)
			if item == nil {
				continue
			}
			s.addSearch(item)
			if err := s.emitAnthWholeBlock(dst, webSearchCallToAnthropic(item)); err != nil {
				return err
			}
		}
		if err := s.emitAnthCitations(dst, firstNonNil(delta["citations"], delta["annotations"])); err != nil {
			return err
		}
		if err := s.chatToolCallsToAnthropic(dst, delta); err != nil {
			return err
		}
	}
	if fr := strVal(ch["finish_reason"]); fr != "" {
		// Latch the reason rather than terminating here: OpenAI reports usage
		// on a later chunk (stream_options.include_usage), so a message_delta
		// written now carries zeroes and the request bills nothing.
		s.anthStop = mapFinishReason(fr)
		s.respFinished = true
	}
	return nil
}

// chatToolCallsToAnthropic maps chat tool_calls fragments onto tool_use blocks.
// OpenAI keys fragments with tool_calls[].index and streams one call's
// arguments contiguously, so each unseen index opens a block and the arguments
// that follow are addressed at the index that call was opened at — the
// alternative (one shared index) merges parallel calls into one broken block.
func (s *streamConvState) chatToolCallsToAnthropic(dst io.Writer, delta map[string]any) error {
	for _, raw := range asSlice(delta["tool_calls"]) {
		tc := asMap(raw)
		if tc == nil {
			continue
		}
		fn := asMap(tc["function"])
		src := intVal(tc["index"])
		name := ""
		if fn != nil {
			name = strVal(fn["name"])
		}
		blockIdx, err := s.anthToolBlockFor(dst, src, firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"])), name)
		if err != nil {
			return err
		}
		if fn == nil {
			continue
		}
		if args := strVal(fn["arguments"]); args != "" {
			if err := writeSSEObj(dst, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamConvState) emitAnthDelta(dst io.Writer, deltaType, field, text string) error {
	if !s.started {
		return nil
	}
	blockType := "text"
	if deltaType == "thinking_delta" {
		blockType = "thinking"
	}
	block := map[string]any{"type": blockType, field: ""}
	if blockType == "thinking" {
		block = anthropicThinkingBlock("")
	}
	idx, err := s.openAnthBlock(dst, blockType, block)
	if err != nil {
		return err
	}
	return writeSSEObj(dst, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": deltaType, field: text},
	})
}

func (s *streamConvState) responsesToAnthropic(obj map[string]any, dst io.Writer) error {
	if !s.started {
		s.started = true
		if err := writeSSEObj(dst, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      "msg_" + s.id,
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}); err != nil {
			return err
		}
	}
	typ := strVal(obj["type"])
	outIdx := intVal(obj["output_index"])
	switch typ {
	case "response.output_text.delta":
		return s.emitAnthDelta(dst, "text_delta", "text", strVal(obj["delta"]))
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return s.emitAnthDelta(dst, "thinking_delta", "thinking", strVal(obj["delta"]))
	case "response.refusal.delta":
		if err := s.emitAnthDelta(dst, "text_delta", "text", strVal(obj["delta"])); err != nil {
			return err
		}
		s.anthRefused = true
		return nil
	case "response.output_text.annotation.added":
		return s.emitAnthCitations(dst, firstNonNil(obj["annotation"], obj["annotations"]))
	case "response.output_item.added":
		item := asMap(obj["item"])
		if item == nil {
			return nil
		}
		itemType := strVal(item["type"])
		switch {
		case isWebSearchToolType(itemType) || itemType == openaiWebSearchCall:
			s.addSearch(item)
			return s.emitAnthWholeBlock(dst, webSearchCallToAnthropic(item))
		case itemType == "function_call", itemType == "custom_tool_call":
			return s.openAnthToolBlock(dst, outIdx,
				firstNonEmpty(strVal(item["call_id"]), strVal(item["id"])), strVal(item["name"]))
		}
		return nil
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		// Arguments arriving before the item was announced, or after an
		// intervening block closed this one, open a block rather than drop the
		// fragments or address a stopped index.
		blockIdx, err := s.anthToolBlockFor(dst, outIdx, strVal(obj["item_id"]), "")
		if err != nil {
			return err
		}
		if s.toolArgsSeen == nil {
			s.toolArgsSeen = map[int]bool{}
		}
		s.toolArgsSeen[outIdx] = true
		return writeSSEObj(dst, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": strVal(obj["delta"])},
		})
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		// Upstreams that only send the finished arguments still have to
		// produce one input_json_delta or the tool_use block stays empty.
		if !s.toolArgsSeen[outIdx] {
			if args := strVal(obj["arguments"]); args != "" {
				if blockIdx, ok := s.blockFor(outIdx); ok {
					if err := writeSSEObj(dst, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": blockIdx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					}); err != nil {
						return err
					}
				}
			}
		}
		return s.closeAnthBlockFor(dst, outIdx)
	case "response.output_text.done":
		return s.closeAnthBlock(dst)
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		// Keep the thinking block open so output_item.done can still attach
		// encrypted_content as signature_delta before content_block_stop.
		return nil
	case "response.output_item.done":
		if item := asMap(obj["item"]); strVal(item["type"]) == "reasoning" {
			if enc := strVal(item["encrypted_content"]); enc != "" {
				s.anthThinkingSig = enc
			}
			return s.closeAnthBlock(dst)
		}
		return s.closeAnthBlockFor(dst, outIdx)
	case "response.completed", "response.incomplete", "response.failed":
		stop := "end_turn"
		if resp := asMap(obj["response"]); resp != nil {
			if u := asMap(resp["usage"]); u != nil {
				s.usage = mergeUsageMaps(s.usage, u)
			}
			if d := asMap(resp["incomplete_details"]); d != nil && strVal(d["reason"]) == "max_output_tokens" {
				stop = "max_tokens"
			}
		}
		if stop == "end_turn" && s.hasToolUse {
			stop = "tool_use"
		}
		return s.finishAnth(dst, stop)
	}
	return nil
}

func (s *streamConvState) chatToResponses(obj map[string]any, dst io.Writer) error {
	if !s.started {
		s.started = true
		if err := writeSSEObj(dst, "response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":     firstNonEmpty(strVal(obj["id"]), s.id),
				"object": "response",
				"status": "in_progress",
				"model":  strVal(obj["model"]),
				"output": []any{},
			},
		}); err != nil {
			return err
		}
	}
	if v := strVal(obj["model"]); v != "" {
		s.respModel = v
	}
	if v := strVal(obj["id"]); v != "" {
		s.respID = v
	}
	choices := asSlice(obj["choices"])
	if len(choices) == 0 {
		// The trailing usage-only chunk lands here. It carries no content, but
		// feedJSON has already merged its usage, which is the whole reason the
		// terminal frame waits for [DONE]/EOF.
		return nil
	}
	ch := asMap(choices[0])
	delta := asMap(ch["delta"])
	if delta != nil {
		if t := strVal(delta["reasoning_content"]); t != "" {
			if err := s.emitRespReasoning(dst, t); err != nil {
				return err
			}
		}
		if t := flattenChatContent(delta["content"]); t != "" {
			if err := s.emitRespText(dst, t); err != nil {
				return err
			}
		}
		if t := strVal(delta["refusal"]); t != "" {
			if err := writeSSEObj(dst, "response.refusal.delta", map[string]any{
				"type":  "response.refusal.delta",
				"delta": t,
			}); err != nil {
				return err
			}
		}
		if err := s.emitRespAnnotations(dst, firstNonNil(delta["annotations"], delta["citations"])); err != nil {
			return err
		}
		if err := s.chatToolCallsToResponses(dst, delta); err != nil {
			return err
		}
	}
	if fr := strVal(ch["finish_reason"]); fr != "" {
		s.respFinished = true
		s.respStatus = "completed"
		if fr == "length" {
			s.respStatus = "incomplete"
		}
	}
	return nil
}

// emitRespAnnotations forwards source-side citations as Responses annotation
// events. They are what ties a web-search answer to the pages it came from, so
// dropping them turns a sourced answer into an unattributed one.
func (s *streamConvState) emitRespAnnotations(dst io.Writer, raw any) error {
	if raw == nil {
		return nil
	}
	list := asSlice(raw)
	if list == nil {
		list = []any{raw}
	}
	for _, ann := range list {
		if ann == nil {
			continue
		}
		ev := map[string]any{
			"type":       "response.output_text.annotation.added",
			"annotation": cloneAny(ann),
		}
		if s.respText != nil {
			ev["output_index"] = s.respText.slot
			ev["item_id"] = strVal(s.respText.item["id"])
		}
		if err := writeSSEObj(dst, "response.output_text.annotation.added", ev); err != nil {
			return err
		}
	}
	return nil
}

// emitRespText streams one visible text fragment and keeps a copy for the
// message item the terminal frame has to carry.
func (s *streamConvState) emitRespText(dst io.Writer, text string) error {
	ri, err := s.respTextItem(dst)
	if err != nil {
		return err
	}
	ri.buf.WriteString(text)
	return writeSSEObj(dst, "response.output_text.delta", map[string]any{
		"type":         "response.output_text.delta",
		"output_index": ri.slot,
		"item_id":      strVal(ri.item["id"]),
		"delta":        text,
	})
}

func (s *streamConvState) emitRespReasoning(dst io.Writer, text string) error {
	ri, err := s.respReasonItem(dst)
	if err != nil {
		return err
	}
	ri.buf.WriteString(text)
	return writeSSEObj(dst, "response.reasoning_summary_text.delta", map[string]any{
		"type":         "response.reasoning_summary_text.delta",
		"output_index": ri.slot,
		"item_id":      strVal(ri.item["id"]),
		"delta":        text,
	})
}

// finishResponses writes the deferred terminal frame, once. Called from the
// [DONE] and EOF paths so any trailing usage chunk has already merged.
func (s *streamConvState) finishResponses(dst io.Writer) error {
	if !s.started || s.respDoneSent {
		return nil
	}
	s.respDoneSent = true
	status := s.respStatus
	if status == "" {
		status = "completed"
	}
	resp := map[string]any{
		"id":     firstNonEmpty(s.respID, s.id),
		"object": "response",
		"status": status,
		"output": s.finalizeRespItems(dst),
	}
	if s.respModel != "" {
		resp["model"] = s.respModel
	}
	if s.usage != nil {
		resp["usage"] = responsesUsageFromMerged(s.usage)
	}
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	return writeSSEObj(dst, event, map[string]any{
		"type":     event,
		"response": resp,
	})
}

// chatToolCallsToResponses announces each chat tool call as a Responses output
// item before streaming its arguments. Without the added frame the client never
// learns the call_id or the function name, which makes the converted tool call
// unusable however many argument fragments arrive.
func (s *streamConvState) chatToolCallsToResponses(dst io.Writer, delta map[string]any) error {
	for _, raw := range asSlice(delta["tool_calls"]) {
		tc := asMap(raw)
		if tc == nil {
			continue
		}
		fn := asMap(tc["function"])
		src := intVal(tc["index"])
		ri := s.respItemFor(src)
		if ri == nil {
			callID := firstNonEmpty(strVal(tc["id"]), strVal(tc["call_id"]))
			name := ""
			if fn != nil {
				name = strVal(fn["name"])
			}
			var err error
			ri, err = s.addRespItem(dst, src, map[string]any{
				"type":      "function_call",
				"id":        firstNonEmpty(callID, "fc_"+randomMessageID()),
				"call_id":   callID,
				"name":      name,
				"arguments": "",
				"status":    "in_progress",
			})
			if err != nil {
				return err
			}
			s.hasToolUse = true
		}
		if fn == nil {
			continue
		}
		if args := strVal(fn["arguments"]); args != "" {
			ri.buf.WriteString(args)
			if err := writeSSEObj(dst, "response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"output_index": ri.slot,
				"item_id":      ri.callID(),
				"call_id":      ri.callID(),
				"delta":        args,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// finalizeRespItems completes the announced output items: arguments collected
// from the argument deltas are folded back in so the terminal
// response.completed carries tool calls a client can actually execute. Emits
// the matching output_item.done frames as a side effect.
func (s *streamConvState) finalizeRespItems(dst io.Writer) []any {
	out := make([]any, 0, len(s.respItems)+len(s.search))
	seen := map[string]bool{}
	for _, ri := range s.respItems {
		if ri.item == nil {
			continue
		}
		ri.seal()
		out = append(out, ri.item)
		if id := strVal(ri.item["id"]); id != "" {
			seen[id] = true
		}
		_ = writeSSEObj(dst, "response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": ri.slot,
			"item":         ri.item,
		})
	}
	// captureSearch also records server-tool blocks so directions that
	// never announce them as output items still carry the search. Skip
	// any id we already sealed — otherwise the same search appears twice.
	for _, raw := range s.search {
		if id := strVal(asMap(raw)["id"]); id != "" && seen[id] {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func (s *streamConvState) responsesToChat(obj map[string]any, dst io.Writer) error {
	typ := strVal(obj["type"])
	id := firstNonEmpty(strVal(obj["id"]), s.id)
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil},
		},
	}
	switch typ {
	case "response.output_text.delta":
		delta := obj["delta"]
		if delta == nil {
			delta = obj["text"]
		}
		chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"content": strVal(delta)}, "finish_reason": nil}}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": strVal(obj["delta"])}, "finish_reason": nil}}
	case "response.refusal.delta":
		// A refusal is the model's answer. Dropping it leaves an empty message
		// with finish_reason "stop", so the caller sees the model produce
		// nothing and never learns it declined.
		chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"refusal": strVal(obj["delta"])}, "finish_reason": nil}}
	case "response.output_text.annotation.added":
		ann := firstNonNil(obj["annotation"], obj["annotations"])
		if ann == nil {
			return nil
		}
		chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"annotations": []any{cloneAny(ann)}}, "finish_reason": nil}}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		// The tool slot is keyed on output_index: a hardcoded 0 merges every
		// parallel call's arguments into one unparseable blob.
		src := intVal(obj["output_index"])
		s.toolArgsSeen = markSeen(s.toolArgsSeen, src)
		chunk["choices"] = []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{
					map[string]any{"index": s.chatToolSlot(src), "function": map[string]any{"arguments": strVal(obj["delta"])}},
				},
			},
			"finish_reason": nil,
		}}
	case "response.output_item.added", "response.output_item.done":
		item := asMap(obj["item"])
		if item == nil {
			return nil
		}
		itemType := strVal(item["type"])
		src := intVal(obj["output_index"])
		switch {
		case isWebSearchToolType(itemType) || itemType == openaiWebSearchCall:
			s.addSearch(item)
			chunk["choices"] = []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"web_search_calls": []any{cloneAny(item)}},
				"finish_reason": nil,
			}}
		case itemType == "function_call" || itemType == "custom_tool_call":
			// The name and call id only ever appear on the item frames, so
			// this is the client's one chance to learn what it is calling.
			s.hasToolUse = true
			if typ == "response.output_item.added" {
				chunk["choices"] = []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": s.chatToolSlot(src),
								"id":    firstNonEmpty(strVal(item["call_id"]), strVal(item["id"])),
								"type":  "function",
								"function": map[string]any{
									"name":      strVal(item["name"]),
									"arguments": "",
								},
							},
						},
					},
					"finish_reason": nil,
				}}
				break
			}
			// A done frame whose arguments never streamed as deltas is the
			// only place they exist.
			if s.toolArgsSeen[src] || strVal(item["arguments"]) == "" {
				return nil
			}
			chunk["choices"] = []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{"index": s.chatToolSlot(src), "function": map[string]any{"arguments": strVal(item["arguments"])}},
					},
				},
				"finish_reason": nil,
			}}
		default:
			return nil
		}
	case "response.completed", "response.incomplete", "response.failed":
		s.respFinished = true
		resp := asMap(obj["response"])
		fr := "stop"
		if typ == "response.incomplete" {
			fr = "length"
		}
		if resp != nil {
			if u := asMap(resp["usage"]); u != nil {
				s.usage = mergeUsageMaps(s.usage, u)
			}
			if d := asMap(resp["incomplete_details"]); d != nil && strVal(d["reason"]) == "max_output_tokens" {
				fr = "length"
			}
			// captureSearch has already read resp["output"] for this frame.
		}
		// An agent loop branches on tool_calls; reporting stop makes it treat
		// the empty message as the final answer and never run the tool it was
		// just handed.
		if fr == "stop" && s.hasToolUse {
			fr = "tool_calls"
		}
		ch := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": fr}
		if len(s.search) > 0 {
			ch["delta"] = map[string]any{"web_search_calls": s.search}
		}
		chunk["choices"] = []any{ch}
		if s.usage != nil {
			chunk["usage"] = chatUsageFromMerged(s.usage)
		}
	case "response.created", "response.in_progress":
		chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}
	default:
		// Passthrough unknown responses events as chat chunks so citations
		// / web_search frames are not dropped.
		if strings.Contains(typ, "web_search") {
			s.addSearch(obj)
			chunk["choices"] = []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"web_search_calls": []any{cloneAny(obj)}},
				"finish_reason": nil,
			}}
		} else {
			return nil
		}
	}
	return writeSSEObj(dst, "", chunk)
}

func (s *streamConvState) anthropicToChat(obj map[string]any, dst io.Writer) error {
	typ := strVal(obj["type"])
	if msg := asMap(obj["message"]); msg != nil {
		// Only message_start carries the id, so adopting it for the rest of
		// the stream is what keeps every chunk under one identifier — clients
		// that group chunks by id otherwise see the first frame as a separate
		// message from the answer that follows it.
		if v := strVal(msg["id"]); v != "" {
			s.id = v
		}
		if u := asMap(msg["usage"]); u != nil {
			s.usage = mergeUsageMaps(s.usage, u)
		}
	}
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil},
		},
	}
	switch typ {
	case "message_start":
		chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}
	case "content_block_start":
		block := asMap(obj["content_block"])
		if block != nil {
			switch strVal(block["type"]) {
			case "tool_use":
				chunk["choices"] = []any{map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								// Anthropic's content_block index counts text and
								// thinking blocks too; chat tool indices must be
								// dense from 0 or the client cannot reassemble.
								"index": s.chatToolSlot(intVal(obj["index"])),
								"id":    strVal(block["id"]),
								"type":  "function",
								"function": map[string]any{
									"name":      strVal(block["name"]),
									"arguments": "",
								},
							},
						},
					},
					"finish_reason": nil,
				}}
			case "server_tool_use", "web_search_tool_result":
				s.addSearch(block)
				chunk["choices"] = []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{"web_search_calls": []any{cloneAny(block)}},
					"finish_reason": nil,
				}}
			default:
				return nil
			}
		}
	case "content_block_delta":
		delta := asMap(obj["delta"])
		if delta == nil {
			return nil
		}
		switch strVal(delta["type"]) {
		case "text_delta":
			chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"content": strVal(delta["text"])}, "finish_reason": nil}}
		case "thinking_delta":
			chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": strVal(delta["thinking"])}, "finish_reason": nil}}
		case "input_json_delta":
			chunk["choices"] = []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{
						map[string]any{"index": s.chatToolSlot(intVal(obj["index"])), "function": map[string]any{"arguments": strVal(delta["partial_json"])}},
					},
				},
				"finish_reason": nil,
			}}
		case "citations_delta":
			// The chat counterpart is delta.annotations, which is also what
			// chatToAnthropic reads back — so a citation survives a round trip
			// instead of being dropped on the way out.
			cite := firstNonNil(delta["citation"], delta["citations"])
			if cite == nil {
				return nil
			}
			chunk["choices"] = []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"annotations": []any{cloneAny(cite)}},
				"finish_reason": nil,
			}}
		default:
			return nil
		}
	case "message_delta":
		delta := asMap(obj["delta"])
		fr := "stop"
		if delta != nil {
			fr = mapAnthropicStop(strVal(delta["stop_reason"]))
		}
		if u := asMap(obj["usage"]); u != nil {
			s.usage = mergeUsageMaps(s.usage, u)
		}
		ch := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": fr}
		if len(s.search) > 0 {
			ch["delta"] = map[string]any{"web_search_calls": s.search}
		}
		chunk["choices"] = []any{ch}
		if s.usage != nil {
			chunk["usage"] = chatUsageFromMerged(s.usage)
		}
	case "message_stop":
		return s.flushChatDone(dst)
	default:
		return nil
	}
	return writeSSEObj(dst, "", chunk)
}

func (s *streamConvState) anthropicToResponses(obj map[string]any, dst io.Writer) error {
	typ := strVal(obj["type"])
	if !s.started && typ == "message_start" {
		s.started = true
		msg := asMap(obj["message"])
		model := ""
		if msg != nil {
			model = strVal(msg["model"])
			s.respModel = model
			if u := asMap(msg["usage"]); u != nil {
				s.usage = mergeUsageMaps(s.usage, u)
			}
		}
		return writeSSEObj(dst, "response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":     s.id,
				"object": "response",
				"status": "in_progress",
				"model":  model,
				"output": []any{},
			},
		})
	}
	switch typ {
	case "content_block_delta":
		delta := asMap(obj["delta"])
		if delta == nil {
			return nil
		}
		switch strVal(delta["type"]) {
		case "text_delta":
			return s.emitRespText(dst, strVal(delta["text"]))
		case "thinking_delta":
			return s.emitRespReasoning(dst, strVal(delta["thinking"]))
		case "citations_delta":
			return s.emitRespAnnotations(dst, firstNonNil(delta["citation"], delta["citations"]))
		case "input_json_delta":
			// Only a block that announced a function_call item has arguments a
			// Responses client can act on. A server_tool_use block streams its
			// query the same way, and forwarding that as function-call
			// arguments would attribute it to a call it does not belong to.
			ri := s.respItemFor(intVal(obj["index"]))
			if ri == nil {
				return nil
			}
			ri.buf.WriteString(strVal(delta["partial_json"]))
			return writeSSEObj(dst, "response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"output_index": ri.slot,
				"item_id":      ri.callID(),
				"call_id":      ri.callID(),
				"delta":        strVal(delta["partial_json"]),
			})
		}
	case "content_block_start":
		block := asMap(obj["content_block"])
		if block == nil {
			return nil
		}
		switch strVal(block["type"]) {
		case "server_tool_use", "web_search_tool_result":
			// src=-1 so a later input_json_delta on this block cannot be
			// mistaken for a function-call argument stream. The item still
			// gets an output_index and a matching .done from addRespItem /
			// finalizeRespItems, which the previous ad-hoc frame lacked.
			_, err := s.addRespItem(dst, -1, webSearchCallToResponses(block))
			return err
		case "tool_use":
			// The Responses wire only carries a call's identity on its item
			// frames; without this the client receives argument fragments for
			// a tool it was never told about.
			s.hasToolUse = true
			_, err := s.addRespItem(dst, intVal(obj["index"]), map[string]any{
				"type":      "function_call",
				"id":        firstNonEmpty(strVal(block["id"]), "fc_"+randomMessageID()),
				"call_id":   strVal(block["id"]),
				"name":      strVal(block["name"]),
				"arguments": "",
				"status":    "in_progress",
			})
			return err
		}
	case "message_delta":
		if u := asMap(obj["usage"]); u != nil {
			s.usage = mergeUsageMaps(s.usage, u)
		}
		if d := asMap(obj["delta"]); d != nil && strVal(d["stop_reason"]) == "max_tokens" {
			s.respStatus = "incomplete"
		}
	case "message_stop":
		s.respFinished = true
	}
	return nil
}

func markSeen(m map[int]bool, key int) map[int]bool {
	if m == nil {
		m = map[int]bool{}
	}
	m[key] = true
	return m
}

// flushDone handles the source stream's [DONE] sentinel: whatever terminal
// shape the target dialect needs, written once. All three helpers are
// idempotent, so a stream that also EOFs adds nothing on the second pass.
func (s *streamConvState) flushDone(dst io.Writer) error {
	// The sentinel is itself a terminal signal in the source protocol, so a
	// stream that ends with [DONE] but no per-choice finish_reason still
	// finished — unlike one that just stops (flushEOF).
	s.respFinished = true
	return s.flushTerminal(dst)
}

// flushEOF is the same for a stream that just ended, with or without [DONE].
// A Responses source has no sentinel of its own — it terminates on
// response.completed and closes — so for a chat client this is the only place
// [DONE] can come from, and a client blocking on the sentinel hangs without it.
func (s *streamConvState) flushEOF(dst io.Writer) error {
	return s.flushTerminal(dst)
}

func (s *streamConvState) flushTerminal(dst io.Writer) error {
	switch s.to {
	case DialectOpenAI:
		// A source that stopped without saying it was done did not produce a
		// complete answer, and synthesizing response.completed here is what
		// makes a truncated stream look finished: the gateway accepts that
		// frame as the terminal one, so it never fails over, never cools the
		// channel, and the caller is billed for a cut-off answer it has no way
		// to detect. Staying quiet lets the incomplete-stream path run and
		// emit response.failed with the partial text instead.
		if !s.respFinished {
			return nil
		}
		return s.finishResponses(dst)
	case DialectAnthropic:
		// Same rule as Responses: a source that just stopped is not a
		// completed message. Inventing end_turn / message_stop is what made
		// a truncated stream look finished to the SDK.
		if !s.respFinished {
			return s.closeAnthBlock(dst)
		}
		return s.finishAnth(dst, "")
	case DialectCompatible:
		if !s.respFinished {
			return s.flushChatIncomplete(dst)
		}
		return s.flushChatDone(dst)
	}
	return nil
}

func (s *streamConvState) flushChatDone(dst io.Writer) error {
	if s.doneSent {
		return nil
	}
	s.doneSent = true
	return writeSSE(dst, "", []byte("[DONE]"))
}

// flushChatIncomplete unblocks Chat clients on a truncated source without
// pretending the model finished (no finish_reason: stop). The error frame
// is what SDKs raise on; [DONE] is only there so a client blocked on the
// sentinel does not hang until its read timeout.
func (s *streamConvState) flushChatIncomplete(dst io.Writer) error {
	if s.doneSent {
		return nil
	}
	s.doneSent = true
	if err := writeSSEObj(dst, "", map[string]any{
		"id":      firstNonEmpty(s.respID, s.id),
		"object":  "chat.completion.chunk",
		"created": s.created,
		"choices": []any{},
		"error": map[string]any{
			"message": "stream interrupted",
			"type":    "upstream_error",
			"code":    "stream_incomplete",
		},
	}); err != nil {
		return err
	}
	return writeSSE(dst, "", []byte("[DONE]"))
}

// mergedCacheSplit reads a merged usage map — which may be keyed either
// dialect's way — and returns the input accounting in one fixed form: the
// GROSS total including the cached and cache-write portions, plus those two
// portions.
//
// The field spelling is what says which convention the numbers were counted
// in, the same discriminator Accumulator.Inspect uses. Anthropic reports
// input_tokens NET of cache_read_input_tokens / cache_creation_input_tokens,
// so those add on top; OpenAI's prompt_tokens is already the gross total and
// prompt_tokens_details.cached_tokens is a subset of it. Emitting one
// convention's numbers under the other's field names charges the cached
// prompt twice — once inside the input total, again at the cache-read rate —
// and shows the client an input count larger than the one it was quoted.
func mergedCacheSplit(u map[string]any) (gross, cached, cacheWrite int) {
	in := firstPositiveInt(u["input_tokens"], u["prompt_tokens"])
	cached = firstPositiveInt(u["cache_read_input_tokens"])
	cacheWrite = firstPositiveInt(u["cache_creation_input_tokens"])
	if cached > 0 || cacheWrite > 0 {
		return in + cached + cacheWrite, cached, cacheWrite
	}
	details := asMap(u["prompt_tokens_details"])
	if details == nil {
		details = asMap(u["input_tokens_details"])
	}
	cached = intVal(details["cached_tokens"])
	cacheWrite = firstPositiveInt(u["cache_write_tokens"], details["cache_write_tokens"])
	return in, cached, cacheWrite
}

// mergedAudioSplit reads the audio subset out of either family's details
// object (or the flat keys mergeUsageMaps stores so a later frame cannot
// drop them). The parent totals already contain these numbers; they are
// split out so the target dialect can put them back under its own details
// spelling. Without that, Inspect on the converted stream bills audio at
// the text rate.
func mergedAudioSplit(u map[string]any) (in, out int) {
	in = firstPositiveInt(u["audio_input_tokens"], u["audio_tokens"])
	out = firstPositiveInt(u["audio_output_tokens"])
	if in == 0 {
		for _, k := range []string{"prompt_tokens_details", "input_tokens_details", "input_token_details"} {
			if d := asMap(u[k]); d != nil {
				if n := intVal(d["audio_tokens"]); n > in {
					in = n
				}
			}
		}
	}
	if out == 0 {
		for _, k := range []string{"completion_tokens_details", "output_tokens_details", "output_token_details"} {
			if d := asMap(u[k]); d != nil {
				if n := intVal(d["audio_tokens"]); n > out {
					out = n
				}
			}
		}
	}
	return in, out
}

// anthropicUsageFromMerged normalizes a merged usage map (which may be keyed
// the OpenAI way) into the block Anthropic clients read.
func anthropicUsageFromMerged(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	out := firstPositiveInt(u["output_tokens"], u["completion_tokens"])
	gross, cached, cacheWrite := mergedCacheSplit(u)
	audioIn, audioOut := mergedAudioSplit(u)
	usage := map[string]any{
		// Anthropic's documented usage has no audio field. The totals still
		// contain those tokens (Inspect subtracts the details below); without
		// the details the converted stream bills voice at the text rate.
		"input_tokens":  max(gross-cached-cacheWrite, 0),
		"output_tokens": out,
	}
	if cached > 0 {
		usage["cache_read_input_tokens"] = cached
	}
	if cacheWrite > 0 {
		usage["cache_creation_input_tokens"] = cacheWrite
	}
	if audioIn > 0 {
		usage["input_tokens_details"] = map[string]any{"audio_tokens": audioIn}
	}
	if audioOut > 0 {
		usage["output_tokens_details"] = map[string]any{"audio_tokens": audioOut}
	}
	if stu := asMap(u["server_tool_use"]); stu != nil {
		usage["server_tool_use"] = cloneAny(stu)
	} else if n := intVal(u["web_search_requests"]); n > 0 {
		usage["server_tool_use"] = map[string]any{"web_search_requests": n}
	}
	return usage
}

func chatUsageFromMerged(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	out := firstPositiveInt(u["output_tokens"], u["completion_tokens"])
	gross, cached, cacheWrite := mergedCacheSplit(u)
	audioIn, audioOut := mergedAudioSplit(u)
	// Chat Completions counts the cache inside prompt_tokens and breaks it
	// out under prompt_tokens_details, so the whole block is emitted in that
	// one convention — mixing in Anthropic's field names would leave the
	// cache counted twice by anything reading both.
	usage := map[string]any{
		"prompt_tokens":     gross,
		"completion_tokens": out,
		"input_tokens":      gross,
		"output_tokens":     out,
		"total_tokens":      gross + out,
	}
	if cached > 0 || cacheWrite > 0 || audioIn > 0 {
		usage["prompt_tokens_details"] = map[string]any{
			"cached_tokens":      cached,
			"cache_write_tokens": cacheWrite,
			"audio_tokens":       audioIn,
		}
	}
	if audioOut > 0 {
		usage["completion_tokens_details"] = map[string]any{"audio_tokens": audioOut}
	}
	if n := intVal(u["web_search_requests"]); n > 0 {
		usage["web_search_requests"] = n
		usage["server_tool_use"] = map[string]any{"web_search_requests": n}
	}
	return usage
}

func responsesUsageFromMerged(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	out := firstPositiveInt(u["output_tokens"], u["completion_tokens"])
	gross, cached, cacheWrite := mergedCacheSplit(u)
	audioIn, audioOut := mergedAudioSplit(u)
	usage := map[string]any{
		"input_tokens":  gross,
		"output_tokens": out,
	}
	if cached > 0 || cacheWrite > 0 || audioIn > 0 {
		usage["input_tokens_details"] = map[string]any{
			"cached_tokens":      cached,
			"cache_write_tokens": cacheWrite,
			"audio_tokens":       audioIn,
		}
	}
	if audioOut > 0 {
		usage["output_tokens_details"] = map[string]any{"audio_tokens": audioOut}
	}
	if n := intVal(u["web_search_requests"]); n > 0 {
		usage["web_search_requests"] = n
		usage["server_tool_use"] = map[string]any{"web_search_requests": n}
	}
	return usage
}

func wrapConvertedBody(from, to Dialect, src io.ReadCloser, stream bool) io.ReadCloser {
	if from == to || src == nil {
		return src
	}
	pr, pw := io.Pipe()
	go func() {
		defer src.Close()
		var err error
		// The converters walk untrusted upstream JSON. A panic here would
		// take the process down with it (this is a bare goroutine, out of
		// reach of gin's recovery), so degrade it into a stream error and
		// let the caller's failover/error path handle it.
		defer func() {
			if r := recover(); r != nil {
				_ = pw.CloseWithError(fmt.Errorf("format conversion panic: %v", r))
			}
		}()
		if stream {
			err = ConvertStream(from, to, src, pw)
		} else {
			raw, rerr := io.ReadAll(src)
			if rerr != nil {
				err = rerr
			} else {
				var out []byte
				out, err = ConvertResponse(from, to, raw)
				if err == nil {
					_, err = pw.Write(out)
				}
			}
		}
		_ = pw.CloseWithError(err)
	}()
	return pr
}
