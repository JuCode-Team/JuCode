// Package llm contains the OpenAI-compatible gateway: stream piping,
// usage extraction, provider abstraction, and the request handler.
package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/jucode/jucode/internal/biz"
)

// ToolCallSummary is the redacted summary the Accumulator builds for
// tool_use events appearing on the SSE response stream. We never
// store the raw `arguments` / `input` payload — only its size and
// sha256 — so the audit log can prove a tool call happened without
// retaining the body.
//
// One entry per tool_use. arguments accumulate across SSE delta
// frames into argsBuf, then finalize() flips the entry to its frozen
// summary form when the block closes (or at stream end).
type ToolCallSummary struct {
	Name        string `json:"name,omitempty"`
	ID          string `json:"id,omitempty"`
	InputSize   int    `json:"input_size"`
	InputSHA256 string `json:"input_sha256"`

	// argsBuf accumulates the streaming arguments bytes; finalize()
	// closes the block and produces InputSize + InputSHA256.
	argsBuf []byte
}

func (t *ToolCallSummary) finalize() {
	t.InputSize = len(t.argsBuf)
	h := sha256.Sum256(t.argsBuf)
	t.InputSHA256 = hex.EncodeToString(h[:])
	t.argsBuf = nil
}

// Accumulator parses OpenAI Chat Completions, Responses, and Anthropic
// Messages-format payloads for token usage. The same instance is fed
// stream chunks one-by-one via Inspect and non-stream bodies the same
// way. Usage is MERGED across frames (max per field), not overwritten:
// Anthropic streaming splits usage across message_start (input +
// cache_read/creation) and message_delta (output only), so a later
// output-only frame must not be allowed to zero out the input counts a
// prior frame already reported.
//
// Cached vs non-cached input tokens:
//   - OpenAI: prompt_tokens (Chat) / input_tokens (Responses) is the TOTAL.
//     prompt_tokens_details.cached_tokens (Chat) /
//     input_tokens_details.cached_tokens (Responses) is the cache-hit subset.
//     GPT-series relays that bill cache writes report them as
//     cache_write_tokens — either inside the details object or as a
//     sibling of prompt_tokens — also a subset of the total.
//     Non-cached = total - cached - cache_write.
//   - Anthropic: input_tokens is the NON-cached part already.
//     cache_read_input_tokens and cache_creation_input_tokens are separate.
type Accumulator struct {
	TokensIn              int // non-cached TEXT input tokens (priced at input_unit_price)
	TokensOut             int // TEXT output tokens
	CachedTokensIn        int // cache-hit input tokens (priced at cache_read_input_price)
	CacheCreationTokensIn int // cache-write input tokens (Anthropic cache_creation_input_tokens / GPT cache_write_tokens)
	// CacheCreation5m/1hTokensIn are Anthropic's TTL split from
	// usage.cache_creation.ephemeral_{5m,1h}_input_tokens. Zero when the
	// upstream only reported the aggregate — billing then treats the
	// whole CacheCreationTokensIn as 5m (see biz.TokenUsage.CacheWriteSplit).
	CacheCreation5mTokensIn int
	CacheCreation1hTokensIn int
	ImageTokensIn           int // built-in image-generation tool input tokens
	ImageTokensOut          int // built-in image-generation tool output tokens
	// AudioTokens*: the audio subset of the reported totals, split out
	// because upstreams price audio several times the text rate. Excluded
	// from TokensIn/TokensOut so the two can be billed independently
	// without double-counting.
	//
	// The input side counts UNCACHED audio only. cached_tokens spans
	// modalities, so audio served from cache is already priced under
	// CachedTokensIn and must not be charged the audio rate on top of that.
	AudioTokensIn  int
	AudioTokensOut int
	HasUsage       bool

	// rawTokensIn/Out are the non-cached totals as parsed, audio still
	// included. Keeping them lets TokensIn/TokensOut be re-derived whenever
	// either the total or the audio subset grows: a stream may report the
	// total on one frame and the audio breakdown on a later one, and a plain
	// subtract-on-arrival would then either double-count or over-deduct.
	rawTokensIn  int
	rawTokensOut int

	// rawAudioIn/Out are the audio subsets as reported, cache hits included;
	// cachedAudioIn is the cache-hit part of rawAudioIn. Same reason as the
	// raw totals: the pieces arrive on separate frames, so the split has to be
	// recomputed from all of them rather than applied as each one lands.
	rawAudioIn    int
	rawAudioOut   int
	cachedAudioIn int

	// AssistantText accumulates the visible text portion of the
	// assistant's response across both streaming and non-streaming
	// modes for OpenAI Chat, OpenAI Responses, and Anthropic Messages.
	// Thinking blocks are deliberately dropped (not visible).
	//
	// The builder is a strings.Builder so concatenation across N SSE
	// frames is amortized O(N) instead of O(N²).
	AssistantText strings.Builder

	// ToolCalls is the per-call summary of every tool_use observed
	// on the response stream. Entries land here in stream order;
	// argument deltas accumulate into ToolCallSummary.argsBuf and
	// are finalized into InputSize + InputSHA256 when the content
	// block closes (Anthropic content_block_stop).
	ToolCalls []*ToolCallSummary

	// openOAIToolCall tracks the per-index streaming buffer for
	// OpenAI Chat tool_calls. OpenAI sends tool_calls as
	// delta.tool_calls[].function.{name,arguments} with an `index`
	// field, multiple calls per request keyed on that index.
	openOAIToolCall map[int]*ToolCallSummary

	// openAnthropicToolBlock holds the currently-building Anthropic
	// content_block tool_use entry. Anthropic sends
	// content_block_start with name+id followed by content_block_delta
	// frames carrying partial_json fragments; closed on content_block_stop.
	openAnthropicToolBlock map[int]*ToolCallSummary

	// OutputImages is every image the model emitted on the response
	// stream, in document order. Populated by Inspect via
	// biz.ExtractOutputImages and dedup'd by SHA-256 so a single
	// image surfacing in multiple SSE frames (e.g. a streaming
	// partial-image followed by the final image) only lands once.
	//
	// When SiteSettings.AllowImageOutput is false the gateway
	// substitutes the image with a notice before the frame reaches
	// here, so this slice stays empty by construction.
	OutputImages []biz.OutputImage
	// seenImageSHA dedup set keyed by SHA-256 of decoded bytes (or of
	// the URL string when source_kind="url"). Lazy-init.
	seenImageSHA map[string]struct{}

	// imageToolUsageSeen prevents a terminal Responses object from charging
	// the same built-in image-generation tool usage twice if it is surfaced in
	// more than one inspected frame.
	imageToolUsageSeen bool

	// WebSearchRequests is the number of built-in web searches the upstream
	// reported (Anthropic usage.server_tool_use.web_search_requests, OpenAI
	// usage.web_search_requests, or counted web_search_call items). Visible
	// on the request usage record and copied onto the persisted usage_log
	// row via latency_detail["web_search_requests"].
	WebSearchRequests     int
	FileSearchRequests    int
	CodeExecutionRequests int
	ComputerUseRequests   int
}

// ToolArgumentBytes is the total size in bytes of the tool-call arguments
// observed on the response stream so far — model output the gateway already
// relayed downstream, whether or not the call has been finalized.
// finalize() moves len(argsBuf) into InputSize and drops the buffer, so
// summing both never double-counts either state.
func (a *Accumulator) ToolArgumentBytes() int {
	total := 0
	for _, t := range a.ToolCalls {
		if t == nil {
			continue
		}
		total += t.InputSize + len(t.argsBuf)
	}
	return total
}

// tokenDetails is the breakdown object that hangs off a token total. The
// key spelling varies by surface — Chat uses prompt_tokens_details /
// completion_tokens_details, Responses uses input_tokens_details /
// output_tokens_details, and Realtime drops the plural
// (input_token_details) — but the members we bill on are identical, so one
// type covers all six. Every member is a SUBSET of its parent total.
type tokenDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	AudioTokens      int `json:"audio_tokens"`
	// cached_tokens_details breaks the cache hit down by modality, and reading
	// it is what keeps audio from being billed twice: cached_tokens is NOT
	// disjoint from audio_tokens. Both are cuts of the same parent total
	// (text + audio + image == input_tokens, with the cache slicing across
	// all three), which is the whole reason this sub-object exists upstream.
	CachedTokensDetails *tokenDetails `json:"cached_tokens_details,omitempty"`
}

// The three accessors are nil-tolerant so callers can probe every spelling
// without guarding each one.
func (d *tokenDetails) audio() int {
	if d == nil {
		return 0
	}
	return d.AudioTokens
}

func (d *tokenDetails) cached() int {
	if d == nil {
		return 0
	}
	return d.CachedTokens
}

func (d *tokenDetails) cacheWrite() int {
	if d == nil {
		return 0
	}
	return d.CacheWriteTokens
}

// cachedAudio is the part of this total's audio that was served from cache.
func (d *tokenDetails) cachedAudio() int {
	if d == nil {
		return 0
	}
	return d.CachedTokensDetails.audio()
}

// usageTokenDetails is the input/output detail pair shared by every usage
// block shape, spelled all three ways so one struct parses Chat, Responses
// and Realtime.
type usageTokenDetails struct {
	PromptTokensDetails     *tokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *tokenDetails `json:"completion_tokens_details,omitempty"`
	InputTokensDetails      *tokenDetails `json:"input_tokens_details,omitempty"`
	OutputTokensDetails     *tokenDetails `json:"output_tokens_details,omitempty"`
	// Realtime spells these singular.
	InputTokenDetails  *tokenDetails `json:"input_token_details,omitempty"`
	OutputTokenDetails *tokenDetails `json:"output_token_details,omitempty"`
}

// audioIn / audioOut pick whichever spelling the upstream used.
func (d usageTokenDetails) audioIn() int {
	return firstNonZero(d.PromptTokensDetails.audio(), d.InputTokensDetails.audio(), d.InputTokenDetails.audio())
}

func (d usageTokenDetails) audioOut() int {
	return firstNonZero(d.CompletionTokensDetails.audio(), d.OutputTokensDetails.audio(), d.OutputTokenDetails.audio())
}

// cachedAudioIn is the cache-hit part of audioIn. Output has no cache, so
// there is no counterpart on that side.
func (d usageTokenDetails) cachedAudioIn() int {
	return firstNonZero(
		d.PromptTokensDetails.cachedAudio(), d.InputTokensDetails.cachedAudio(), d.InputTokenDetails.cachedAudio())
}

// cached / cacheWrite read the input-side details, preferring an explicit
// count over a zero from a details object that omitted the field.
func (d usageTokenDetails) cached() int {
	return firstNonZero(d.PromptTokensDetails.cached(), d.InputTokensDetails.cached(), d.InputTokenDetails.cached())
}

func (d usageTokenDetails) cacheWrite() int {
	return firstNonZero(
		d.PromptTokensDetails.cacheWrite(), d.InputTokensDetails.cacheWrite(), d.InputTokenDetails.cacheWrite())
}

// cacheCreationUsage is Anthropic's TTL split nested under
// usage.cache_creation. 5m ≈ 1.25× input, 1h ≈ 2× input.
type cacheCreationUsage struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

type usageEnvelope struct {
	// OpenAI Chat shape
	Usage *struct {
		// Chat
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`

		// Responses / Realtime / Anthropic
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`

		usageTokenDetails

		// GPT-series cache write count as a sibling of prompt_tokens
		// (some relays put it here instead of inside the details object).
		CacheWriteTokens int `json:"cache_write_tokens"`

		// Anthropic Messages
		CacheReadInputTokens     int                 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int                 `json:"cache_creation_input_tokens"`
		CacheCreation            *cacheCreationUsage `json:"cache_creation,omitempty"`
	} `json:"usage,omitempty"`

	// Responses / Realtime terminal frame nests usage under .response.usage
	Response *struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			usageTokenDetails
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"usage,omitempty"`
		ToolUsage *imageToolUsageEnvelope `json:"tool_usage,omitempty"`
	} `json:"response,omitempty"`

	// Responses non-stream bodies may report built-in tool billing in a
	// sibling tool_usage object. Image generation tool tokens are not included
	// in the main response.usage block, so merge them explicitly.
	ToolUsage *imageToolUsageEnvelope `json:"tool_usage,omitempty"`

	// Anthropic Messages streaming: the message_start event nests the
	// authoritative INPUT token counts under .message.usage. The later
	// message_delta carries only output_tokens on the flat .usage, so
	// message_start is the only frame that reports input; it must be read.
	Message *struct {
		Usage *struct {
			InputTokens              int                 `json:"input_tokens"`
			OutputTokens             int                 `json:"output_tokens"`
			CacheReadInputTokens     int                 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int                 `json:"cache_creation_input_tokens"`
			CacheCreation            *cacheCreationUsage `json:"cache_creation,omitempty"`
		} `json:"usage,omitempty"`
	} `json:"message,omitempty"`
}

func cacheCreationSplit(cc *cacheCreationUsage) (fiveM, oneH int) {
	if cc == nil {
		return 0, 0
	}
	return cc.Ephemeral5mInputTokens, cc.Ephemeral1hInputTokens
}

func liftCacheCreationTotal(aggregate, fiveM, oneH int) int {
	if sum := fiveM + oneH; sum > aggregate {
		return sum
	}
	return aggregate
}

type imageToolUsageEnvelope struct {
	ImageGen *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
	} `json:"image_gen,omitempty"`
}

// Inspect parses a single JSON payload (one SSE frame's data: object).
// Forgiving: any parse error short-circuits silently.
func (a *Accumulator) Inspect(payload []byte) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if payload[0] != '{' {
		return
	}
	// Best-effort assistant text extraction. Runs before the typed
	// usage parse so a malformed frame can't skip it; failures inside
	// each format helper are silently ignored.
	a.extractText(payload)
	a.extractImages(payload)

	var e usageEnvelope
	if err := json.Unmarshal(payload, &e); err != nil {
		return
	}

	// 1) Anthropic message_start: usage nested under .message.usage.
	// This is the authoritative source for input + cache_read/creation
	// tokens on a streamed Anthropic response; message_delta only reports
	// output. input_tokens here is already the non-cached part.
	if e.Message != nil && e.Message.Usage != nil {
		u := e.Message.Usage
		fiveM, oneH := cacheCreationSplit(u.CacheCreation)
		total := liftCacheCreationTotal(u.CacheCreationInputTokens, fiveM, oneH)
		if u.InputTokens > 0 || u.CacheReadInputTokens > 0 || total > 0 || fiveM > 0 || oneH > 0 || u.OutputTokens > 0 {
			a.mergeUsage(u.InputTokens, u.CacheReadInputTokens, total, fiveM, oneH, u.OutputTokens)
		}
	}

	// 2) flat .usage  — covers OpenAI Chat + Anthropic message_delta +
	// non-stream Anthropic bodies + mapped Chat.
	if e.Usage != nil {
		// Output: completion_tokens (chat) OR output_tokens (responses/anthropic)
		out := e.Usage.CompletionTokens
		if out == 0 {
			out = e.Usage.OutputTokens
		}

		// Anthropic style: input_tokens is already the non-cached part
		// and cache_*_input_tokens are separate. A nested cache_creation
		// object (even when the aggregate is 0) is also Anthropic — taking
		// the OpenAI subtract path would drop the 5m/1h split.
		fiveM, oneH := cacheCreationSplit(e.Usage.CacheCreation)
		cacheWrite := liftCacheCreationTotal(e.Usage.CacheCreationInputTokens, fiveM, oneH)
		if e.Usage.CacheReadInputTokens > 0 || cacheWrite > 0 || e.Usage.CacheCreation != nil {
			a.mergeUsage(e.Usage.InputTokens, e.Usage.CacheReadInputTokens, cacheWrite, fiveM, oneH, out)
			// Anthropic itself reports no audio, but a relay that normalizes
			// several upstreams into this shape can carry both; parse it
			// before returning so such a frame is not billed at the text rate.
			a.mergeAudioUsage(e.Usage.audioIn(), e.Usage.audioOut(), e.Usage.cachedAudioIn())
			return
		}

		// OpenAI style: prompt_tokens/input_tokens is the TOTAL; cached and
		// cache_write (GPT-series relays) are subsets of it.
		total := firstNonZero(e.Usage.PromptTokens, e.Usage.InputTokens)
		cached := e.Usage.cached()
		cacheWrite = firstNonZero(e.Usage.CacheWriteTokens, e.Usage.cacheWrite())
		if total > 0 || out > 0 {
			in := total - cached - cacheWrite
			if in < 0 {
				in = 0
			}
			a.mergeUsage(in, cached, cacheWrite, 0, 0, out)
			a.mergeAudioUsage(e.Usage.audioIn(), e.Usage.audioOut(), e.Usage.cachedAudioIn())
		}
	}

	// 3) Responses / Realtime terminal frame: .response.usage
	if e.Response != nil && e.Response.Usage != nil {
		u := e.Response.Usage
		cached := u.cached()
		cacheWrite := firstNonZero(u.CacheWriteTokens, u.cacheWrite())
		in := u.InputTokens - cached - cacheWrite
		if in < 0 {
			in = 0
		}
		a.mergeUsage(in, cached, cacheWrite, 0, 0, u.OutputTokens)
		a.mergeAudioUsage(u.audioIn(), u.audioOut(), u.cachedAudioIn())
	}

	// 4) Responses image-generation tool usage. This is separate from the
	// text model's usage in non-stream responses, and may also appear nested
	// under response on streaming terminal events.
	a.mergeImageToolUsage(e.ToolUsage)
	if e.Response != nil {
		a.mergeImageToolUsage(e.Response.ToolUsage)
	}

	// 5) Built-in web-search usage. Merge max like token counts so a
	// later output-only frame cannot zero a count already reported.
	var envelope map[string]any
	if json.Unmarshal(payload, &envelope) == nil {
		if n := extractWebSearchRequests(envelope); n > a.WebSearchRequests {
			a.WebSearchRequests = n
			a.HasUsage = true
		}
		counts := extractBuiltInToolCounts(envelope)
		if n := counts[builtinFileSearch]; n > a.FileSearchRequests {
			a.FileSearchRequests = n
			a.HasUsage = true
		}
		if n := counts[builtinCodeExecution]; n > a.CodeExecutionRequests {
			a.CodeExecutionRequests = n
			a.HasUsage = true
		}
		if n := counts[builtinComputerUse]; n > a.ComputerUseRequests {
			a.ComputerUseRequests = n
			a.HasUsage = true
		}
	}
}

func (a *Accumulator) mergeImageToolUsage(toolUsage *imageToolUsageEnvelope) {
	if toolUsage == nil || toolUsage.ImageGen == nil || a.imageToolUsageSeen {
		return
	}
	u := toolUsage.ImageGen
	in := firstNonZero(u.InputTokens, u.PromptTokens)
	out := firstNonZero(u.OutputTokens, u.CompletionTokens)
	if in == 0 && out == 0 {
		return
	}
	a.ImageTokensIn += in
	a.ImageTokensOut += out
	a.HasUsage = true
	a.imageToolUsageSeen = true
}

// mergeUsage folds one frame's token counts into the accumulator without
// letting a frame that omits a field regress an already-captured value.
// Each field takes the max seen so far; output grows as message_delta
// frames report the running total. This is what makes a trailing
// output-only Anthropic frame unable to zero out the input counts that
// message_start reported.
func (a *Accumulator) mergeUsage(in, cachedIn, cacheCreationIn, cache5m, cache1h, out int) {
	if in > a.rawTokensIn {
		a.rawTokensIn = in
	}
	if cachedIn > a.CachedTokensIn {
		a.CachedTokensIn = cachedIn
	}
	if cacheCreationIn > a.CacheCreationTokensIn {
		a.CacheCreationTokensIn = cacheCreationIn
	}
	if cache5m > a.CacheCreation5mTokensIn {
		a.CacheCreation5mTokensIn = cache5m
	}
	if cache1h > a.CacheCreation1hTokensIn {
		a.CacheCreation1hTokensIn = cache1h
	}
	if sum := a.CacheCreation5mTokensIn + a.CacheCreation1hTokensIn; sum > a.CacheCreationTokensIn {
		a.CacheCreationTokensIn = sum
	}
	if out > a.rawTokensOut {
		a.rawTokensOut = out
	}
	a.HasUsage = true
	a.splitAudio()
}

// mergeAudioUsage records the audio breakdown of the input/output totals: the
// audio subset itself, plus how much of the input subset was a cache hit. All
// three merge as max, like every other usage field, so a later frame that
// omits the breakdown cannot erase it.
func (a *Accumulator) mergeAudioUsage(audioIn, audioOut, cachedAudioIn int) {
	if audioIn <= a.rawAudioIn && audioOut <= a.rawAudioOut && cachedAudioIn <= a.cachedAudioIn {
		return
	}
	if audioIn > a.rawAudioIn {
		a.rawAudioIn = audioIn
	}
	if audioOut > a.rawAudioOut {
		a.rawAudioOut = audioOut
	}
	if cachedAudioIn > a.cachedAudioIn {
		a.cachedAudioIn = cachedAudioIn
	}
	a.HasUsage = true
	a.splitAudio()
}

// splitAudio re-derives the billable split from the reported pieces, holding
// three invariants in this order:
//
//   - Nothing is billed twice. Audio that was a cache hit stays accounted for
//     under CachedTokensIn and is kept out of AudioTokensIn.
//   - Nothing is billed beyond what the upstream reported. The audio share is
//     capped at the total it belongs to, so a self-contradictory breakdown
//     (audio larger than its own total) cannot inflate the charge.
//   - TokensIn/TokensOut are the text remainder, and cannot go negative.
//
// Together those make TokensIn + CachedTokensIn + CacheCreationTokensIn +
// AudioTokensIn come to exactly the input total the upstream reported.
func (a *Accumulator) splitAudio() {
	a.AudioTokensIn = min(max(a.rawAudioIn-a.cachedAudioIn, 0), a.rawTokensIn)
	a.AudioTokensOut = min(a.rawAudioOut, a.rawTokensOut)
	a.TokensIn = a.rawTokensIn - a.AudioTokensIn
	a.TokensOut = a.rawTokensOut - a.AudioTokensOut
}

// extractText pulls the visible assistant text out of one SSE / JSON
// frame. Three formats are recognized:
//
//  1. OpenAI Chat Completions
//     - stream: choices[].delta.content (incremental string)
//     - non-stream: choices[].message.content (full string)
//
//  2. OpenAI Responses
//     - stream typed events: type="response.output_text.delta" → .delta
//     (preferred — only fires for text deltas, not tool/audio).
//     Fallback: response.output[].content[].text on the terminal frame.
//
//  3. Anthropic Messages
//     - stream: type="content_block_delta" with delta.type="text_delta"
//     → delta.text. Or "input_json_delta" / "thinking_delta" which
//     we skip (not visible text).
//     - non-stream: content[].text where content[].type=="text"
//
// Any other shape is silently ignored. Tool calls, function calls,
// thinking blocks, images, audio — all dropped.
func (a *Accumulator) extractText(payload []byte) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return
	}

	// --- Anthropic streaming: typed events ---
	if t, _ := obj["type"].(string); t != "" {
		switch t {
		case "content_block_start":
			// Opens a new content block. tool_use blocks land an
			// initial { id, name, input } object inside content_block;
			// subsequent partial_json deltas extend the args.
			idx := toIntLoose(obj["index"])
			block, _ := obj["content_block"].(map[string]any)
			if block != nil && strOfAny(block["type"]) == "tool_use" {
				if a.openAnthropicToolBlock == nil {
					a.openAnthropicToolBlock = make(map[int]*ToolCallSummary)
				}
				sum := &ToolCallSummary{
					Name: strOfAny(block["name"]),
					ID:   strOfAny(block["id"]),
				}
				// Seed argsBuf with the initial input value when present
				// (some implementations emit input as a fixed object here
				// rather than incremental partial_json).
				if v, ok := block["input"]; ok && v != nil {
					if b, err := json.Marshal(v); err == nil && string(b) != "{}" {
						sum.argsBuf = append(sum.argsBuf, b...)
					}
				}
				a.openAnthropicToolBlock[idx] = sum
				a.ToolCalls = append(a.ToolCalls, sum)
			}
			return
		case "content_block_delta":
			delta, _ := obj["delta"].(map[string]any)
			if delta == nil {
				return
			}
			dt, _ := delta["type"].(string)
			if dt == "text_delta" {
				if s, ok := delta["text"].(string); ok && s != "" {
					a.AssistantText.WriteString(s)
				}
				return
			}
			if dt == "input_json_delta" {
				idx := toIntLoose(obj["index"])
				if sum, ok := a.openAnthropicToolBlock[idx]; ok && sum != nil {
					if s, ok := delta["partial_json"].(string); ok {
						sum.argsBuf = append(sum.argsBuf, []byte(s)...)
					}
				}
				return
			}
			return
		case "content_block_stop":
			idx := toIntLoose(obj["index"])
			if sum, ok := a.openAnthropicToolBlock[idx]; ok && sum != nil {
				sum.finalize()
				delete(a.openAnthropicToolBlock, idx)
			}
			return
		case "response.output_text.delta":
			if s, ok := obj["delta"].(string); ok && s != "" {
				a.AssistantText.WriteString(s)
			}
			return
		case "message_start", "message_delta", "message_stop",
			"ping", "error",
			"response.created", "response.in_progress",
			"response.output_text.done", "response.output_item.added",
			"response.output_item.done", "response.completed":
			// Known events that don't carry incremental text.
			return
		}
	}

	// --- OpenAI Chat Completions ---
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		first, _ := choices[0].(map[string]any)
		if first != nil {
			// Stream: delta.{content, tool_calls}
			if delta, ok := first["delta"].(map[string]any); ok {
				if s, ok := delta["content"].(string); ok && s != "" {
					a.AssistantText.WriteString(s)
				}
				if calls, ok := delta["tool_calls"].([]any); ok {
					a.absorbOpenAIToolCallDelta(calls)
				}
				// Some servers emit finish_reason="tool_calls" on the
				// closing chunk — finalize any open tool_call accumulators.
				if fr, _ := first["finish_reason"].(string); fr == "tool_calls" {
					for _, t := range a.openOAIToolCall {
						t.finalize()
					}
				}
			}
			// Non-stream: message.{content, tool_calls}
			if msg, ok := first["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok && s != "" {
					a.AssistantText.WriteString(s)
				}
				if calls, ok := msg["tool_calls"].([]any); ok {
					a.absorbOpenAINonStreamToolCalls(calls)
				}
			}
		}
	}

	// --- Anthropic non-stream Messages: content[].text ---
	if content, ok := obj["content"].([]any); ok {
		for _, c := range content {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			if t, _ := cm["type"].(string); t == "text" {
				if s, ok := cm["text"].(string); ok && s != "" {
					a.AssistantText.WriteString(s)
				}
			}
		}
	}

	// --- OpenAI Responses non-stream / terminal frame: response.output[].content[].text ---
	if resp, ok := obj["response"].(map[string]any); ok {
		if out, ok := resp["output"].([]any); ok {
			for _, item := range out {
				m, _ := item.(map[string]any)
				if m == nil {
					continue
				}
				cs, _ := m["content"].([]any)
				for _, c := range cs {
					cm, _ := c.(map[string]any)
					if cm == nil {
						continue
					}
					if t, _ := cm["type"].(string); t == "output_text" || t == "text" {
						if s, ok := cm["text"].(string); ok && s != "" {
							a.AssistantText.WriteString(s)
						}
					}
				}
			}
		}
	}
}

// extractImages parses the same payload extractText sees but pulls
// any model-OUTPUT image blocks out via biz.ExtractOutputImages.
// Idempotent — dedups by SHA-256 so the same image surfacing in
// multiple SSE frames (e.g. partial → final) only lands in
// OutputImages once.
func (a *Accumulator) extractImages(payload []byte) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return
	}
	imgs := biz.ExtractOutputImages(obj)
	if len(imgs) == 0 {
		return
	}
	if a.seenImageSHA == nil {
		a.seenImageSHA = make(map[string]struct{})
	}
	for _, im := range imgs {
		if im.SHA256 == "" {
			continue
		}
		if _, dup := a.seenImageSHA[im.SHA256]; dup {
			continue
		}
		a.seenImageSHA[im.SHA256] = struct{}{}
		im.Index = len(a.OutputImages)
		a.OutputImages = append(a.OutputImages, im)
	}
}

// absorbOpenAIToolCallDelta processes one streaming choices[0].delta
// .tool_calls slice. Each entry has an `index` field; OpenAI keys
// concurrent tool_calls by that index, with name appearing in the
// first delta and arguments arriving across many subsequent deltas.
func (a *Accumulator) absorbOpenAIToolCallDelta(calls []any) {
	if a.openOAIToolCall == nil {
		a.openOAIToolCall = make(map[int]*ToolCallSummary)
	}
	for _, c := range calls {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		idx := toIntLoose(cm["index"])
		sum, ok := a.openOAIToolCall[idx]
		if !ok {
			sum = &ToolCallSummary{}
			a.openOAIToolCall[idx] = sum
			a.ToolCalls = append(a.ToolCalls, sum)
		}
		if id, ok := cm["id"].(string); ok && id != "" && sum.ID == "" {
			sum.ID = id
		}
		fn, _ := cm["function"].(map[string]any)
		if fn != nil {
			if n, ok := fn["name"].(string); ok && n != "" && sum.Name == "" {
				sum.Name = n
			}
			if a, ok := fn["arguments"].(string); ok && a != "" {
				sum.argsBuf = append(sum.argsBuf, []byte(a)...)
			}
		}
	}
}

// absorbOpenAINonStreamToolCalls handles the non-stream
// message.tool_calls payload (full object, not deltas). We treat
// each call as a single finalize.
func (a *Accumulator) absorbOpenAINonStreamToolCalls(calls []any) {
	for _, c := range calls {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		fn, _ := cm["function"].(map[string]any)
		var name, args string
		if fn != nil {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		sum := &ToolCallSummary{
			ID:      strOfAny(cm["id"]),
			Name:    name,
			argsBuf: []byte(args),
		}
		sum.finalize()
		a.ToolCalls = append(a.ToolCalls, sum)
	}
}

func toIntLoose(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

func strOfAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
