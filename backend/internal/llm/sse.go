package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jucode/jucode/internal/metrics"
)

// errClientWrite wraps a failed write to the caller's own connection (EPIPE /
// ECONNRESET on our listening socket) so callers can tell it apart from a read
// failure on the upstream body. Both surface as one pipeErr, but they mean
// opposite things: the client gave up, versus the provider did. Without the
// distinction a client hanging up mid-stream was recorded as a provider
// failure, cooling a healthy channel and counting against public availability.
var errClientWrite = errors.New("client connection write failed")

// PipeOptions is the optional hookset for PipeAndExtractOpts. All fields
// may be nil; nil means "skip that hook entirely".
type PipeOptions struct {
	// OnFirstFrame fires once when the first non-empty `data:` frame
	// is observed. This is useful for transport diagnostics, but it is
	// not necessarily a visible token.
	OnFirstFrame func()
	// OnFirstText fires once when the stream first produces semantic model
	// output: visible text, reasoning/thinking deltas, or tool-call deltas.
	// The field name is kept for source compatibility; this is the TTFT hook.
	OnFirstText func()
	// OnFirstOutput fires once when the stream first shows the model
	// producing output of ANY kind — a superset of OnFirstText that also
	// covers a reasoning item whose content is encrypted and therefore never
	// readable. It exists to anchor the decode window: a Responses stream
	// opens with lifecycle frames (response.created, response.in_progress)
	// that say nothing about the model, so "the first frame arrived" is not
	// the same moment as "the model started producing".
	OnFirstOutput func()
	// OnFrame fires for every parsed `data:` JSON object after
	// Accumulator.Inspect runs. Returning a non-empty error string
	// signals a policy block: the function should also have already
	// written a terminal error event to the wire (caller is allowed
	// to write into dst); PipeAndExtractOpts will close out without
	// streaming further bytes and return ErrPolicyBlocked.
	OnFrame func(payload []byte, acc *Accumulator) error
	// TransformPayload, if set, rewrites the JSON payload bytes
	// inside each `data:` frame before they're sent to the client.
	// Return nil (or an empty slice) to forward the original bytes
	// untouched. Used by the image-output gate to substitute model-
	// emitted images with a text notice when the operator hasn't
	// enabled image output. Runs AFTER Accumulator.Inspect, so the
	// accumulator still sees the original (unsubstituted) frame for
	// usage-token counting purposes — but the BYTES that reach the
	// client are the transformed ones.
	TransformPayload func(payload []byte) []byte
	// AfterWrite runs after each line has been forwarded and flushed.
	AfterWrite func()
	// AfterContentFrame runs after a data: frame carrying a non-empty,
	// non-[DONE] payload has been forwarded and flushed. Unlike AfterWrite
	// (which fires on every line, including SSE keepalive comments and blank
	// separators), this fires only on real content progress — so it can drive
	// a chunk-idle/stall timer that upstream keepalives can't keep alive.
	AfterContentFrame func()
}

// ErrPolicyBlocked is returned when an OnFrame hook signals the
// upstream stream should be cut short. Gateway treats it as a normal
// terminal condition (not a retryable failure).
var ErrPolicyBlocked = errors.New("policy violation: stream blocked")

// isStreamTerminalPayload reports whether a `data:` frame is the upstream
// saying "that is the whole answer". Every relayed dialect is covered by this
// one predicate because their frame shapes are disjoint: Responses and
// Anthropic events are tagged with `type`, Chat Completions chunks are not.
// Keeping it in one place is deliberate — the alternative, a per-caller notion
// of "finished", is what let three code paths each decide differently and let
// truncated answers ship as 200.
//
// Callers do not consult this directly; PipeAndExtractOpts uses it to tell a
// finished stream from one that was cut short.
func isStreamTerminalPayload(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return false
	}
	// The sentinel both OpenAI dialects close with.
	if bytes.Equal(payload, []byte("[DONE]")) {
		return true
	}
	if payload[0] != '{' {
		return false
	}
	// Typed, and only the four fields that matter. This runs on every frame of
	// every stream, and decoding a whole chunk into map[string]any to answer
	// one boolean cost roughly 1.5x the time and 4x the allocations — on a
	// long answer that is megabytes of garbage for nothing.
	var frame struct {
		Type    string `json:"type"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return false
	}
	if frame.Type != "" {
		switch frame.Type {
		// Responses API.
		case "response.completed", "response.incomplete", "response.failed":
			return true
		// Anthropic Messages.
		case "message_stop":
			return true
		}
		return false
	}
	// Chat Completions: a chunk carrying finish_reason ends the answer, and
	// under stream_options.include_usage a trailing usage-only chunk (empty
	// choices) follows it. Usage alongside content does NOT end anything —
	// vLLM's continuous_usage_stats repeats it on every chunk.
	if len(frame.Choices) == 0 {
		return len(frame.Usage) > 0 && !bytes.Equal(frame.Usage, []byte("null"))
	}
	for _, ch := range frame.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			return true
		}
	}
	return false
}

func isResponsesTerminalPayload(payload []byte) bool {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return false
	}
	switch ev.Type {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

// isFirstTokenPayload reports whether a parsed SSE data payload carries the
// first semantic model output for TTFT purposes. It deliberately ignores pure
// lifecycle/metadata frames such as response.created, message_start, usage and
// completion sentinels.
func isFirstTokenPayload(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || payload[0] != '{' {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return false
	}

	if typ, _ := obj["type"].(string); typ != "" {
		switch typ {
		case "response.output_text.delta",
			"response.reasoning_summary_text.delta",
			"response.reasoning_text.delta",
			"response.function_call_arguments.delta":
			return strOfAny(obj["delta"]) != ""
		case "response.output_item.added":
			if item, _ := obj["item"].(map[string]any); item != nil {
				switch strOfAny(item["type"]) {
				case "function_call":
					return true
				}
			}
		case "content_block_start":
			if block, _ := obj["content_block"].(map[string]any); block != nil {
				return strOfAny(block["type"]) == "tool_use"
			}
		case "content_block_delta":
			if delta, _ := obj["delta"].(map[string]any); delta != nil {
				switch strOfAny(delta["type"]) {
				case "text_delta":
					return strOfAny(delta["text"]) != ""
				case "thinking_delta":
					return strOfAny(delta["thinking"]) != ""
				case "input_json_delta":
					return strOfAny(delta["partial_json"]) != ""
				}
			}
		}
	}

	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	first, _ := choices[0].(map[string]any)
	if first == nil {
		return false
	}
	delta, _ := first["delta"].(map[string]any)
	if delta == nil {
		return false
	}
	if strOfAny(delta["content"]) != "" || strOfAny(delta["reasoning_content"]) != "" {
		return true
	}
	if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	if call, ok := delta["function_call"].(map[string]any); ok && call != nil {
		return strOfAny(call["name"]) != "" || strOfAny(call["arguments"]) != ""
	}
	return false
}

// isFirstOutputPayload reports whether a parsed SSE payload is the first
// evidence that the model has started producing output — the superset of
// isFirstTokenPayload used to anchor the decode window.
//
// The gap it closes is a reasoning model whose thinking is delivered as an
// encrypted blob: no summary and no readable delta ever streams, so the
// first *token* lands when the answer starts, hundreds of seconds later,
// while the model had in fact been producing output the whole time. The
// events below mark that start without claiming any of it is readable:
// Responses opens a reasoning item, or the summary part that will carry it,
// and Anthropic opens a thinking content block.
func isFirstOutputPayload(payload []byte) bool {
	if isFirstTokenPayload(payload) {
		return true
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return false
	}
	switch strOfAny(obj["type"]) {
	case "response.output_item.added":
		item, _ := obj["item"].(map[string]any)
		return strOfAny(item["type"]) == "reasoning"
	case "response.reasoning_summary_part.added":
		return true
	case "content_block_start":
		block, _ := obj["content_block"].(map[string]any)
		return strOfAny(block["type"]) == "thinking"
	}
	return false
}

// responsesAggregator is an io.Writer sink for an upstream Responses-API
// SSE stream, used when the client asked /v1/responses for a non-streaming
// JSON response. It scans the forwarded `data:` frames for the terminal
// event (response.completed / response.incomplete / response.failed) and
// keeps that frame's `response` object. The raw bytes are also retained so
// an upstream that answered with a plain JSON body (some relays ignore
// stream=true) can still be served.
type responsesAggregator struct {
	pending  []byte // partial-line remainder between Write calls
	raw      bytes.Buffer
	response []byte
}

func newResponsesAggregator() *responsesAggregator {
	return &responsesAggregator{}
}

func (a *responsesAggregator) Write(p []byte) (int, error) {
	a.raw.Write(p)
	a.pending = append(a.pending, p...)
	for {
		i := bytes.IndexByte(a.pending, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(a.pending[:i])
		a.pending = a.pending[i+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
			continue
		}
		var ev struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.completed", "response.incomplete", "response.failed":
			if len(ev.Response) > 0 {
				a.response = append([]byte(nil), ev.Response...)
			}
		}
	}
	return len(p), nil
}

// final returns the aggregated response JSON: the terminal frame's
// `response` object when one streamed by, otherwise the whole buffer when
// it holds a single plain response object. nil when neither is available.
func (a *responsesAggregator) final() []byte {
	if len(a.response) > 0 {
		return a.response
	}
	trimmed := bytes.TrimSpace(a.raw.Bytes())
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var probe struct {
			Object string `json:"object"`
			Output []any  `json:"output"`
		}
		if json.Unmarshal(trimmed, &probe) == nil && (probe.Object == "response" || probe.Output != nil) {
			return trimmed
		}
	}
	return nil
}

// streamCompleteness answers one question for every SSE relay in this package:
// did the upstream finish, or did it just stop? It exists as a type so the
// answer cannot drift between the pipes — the bug it guards against is a
// truncated answer delivered as a 200 and billed, and that came from each
// relay path deciding this for itself.
type streamCompleteness struct {
	sawData     bool
	sawTerminal bool
}

// observe records one `data:` payload. Empty payloads are SSE keepalive
// padding and say nothing either way.
func (c *streamCompleteness) observe(payload []byte) {
	if len(payload) == 0 {
		return
	}
	c.sawData = true
	if isStreamTerminalPayload(payload) {
		c.sawTerminal = true
	}
}

// err reports a stream that carried frames and then stopped without any of its
// dialect's terminal signals. A body with no `data:` frame at all returns nil:
// relays that ignore stream=true and answer with a plain JSON object are a
// different condition, and the callers that can hit it handle it themselves.
func (c *streamCompleteness) err() error {
	if c.sawData && !c.sawTerminal {
		return errUpstreamStreamIncomplete
	}
	return nil
}

// IsStreamIncomplete reports whether a pipe returned because the upstream
// stopped mid-answer. Exported for relay surfaces outside this package: an
// unfinished stream must not be settled as a delivered response, and the
// judgement has to come from here rather than each surface inventing its own.
func IsStreamIncomplete(err error) bool {
	return errors.Is(err, errUpstreamStreamIncomplete)
}

// streamEndedOnClientHangUp reports whether a finished pipe should be read as
// "the caller went away" rather than "the provider failed". Both arrive as an
// unfinished stream: the sentinel when the upstream closed cleanly, a wrapped
// context.Canceled when the cancelled read failed first. The deciding evidence
// is reqCtx — the stream timeouts cancel derived contexts, so only an actual
// hang-up cancels this one.
//
// Shared by all three relay paths on purpose: when they each decided this for
// themselves, a hang-up cooled the channel on two of them and not the third.
func streamEndedOnClientHangUp(pipeErr error, reqCtx context.Context) bool {
	if !errors.Is(reqCtx.Err(), context.Canceled) {
		return false
	}
	return errors.Is(pipeErr, errUpstreamStreamIncomplete) || errors.Is(pipeErr, context.Canceled)
}

// commitDownstreamSSE writes the streaming status line as soon as an upstream
// 2xx is in hand. The client's first packet is then those headers, not the
// first model token. Flushing marks the response written, so a later failure
// cannot change the status code or fail over to another provider.
func commitDownstreamSSE(w http.ResponseWriter) {
	if w == nil {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// streamPrelude holds the frames a stream sends before any model output
// (response.created, response.in_progress, a role-only chat delta). Until the
// model produces something, a failed attempt can still fail over to another
// account without the client having seen the dead one. release writes the
// held frames and passes everything after straight through.
type streamPrelude struct {
	w        http.ResponseWriter
	buf      bytes.Buffer
	released bool
}

func (p *streamPrelude) Write(b []byte) (int, error) {
	if p.released {
		return p.w.Write(b)
	}
	return p.buf.Write(b)
}

func (p *streamPrelude) Flush() {
	if p.released {
		if f, ok := p.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// release is nil-safe so callers need not check whether the attempt used a
// prelude (aggregate mode does not). A write error resurfaces on the pipe's
// next write.
func (p *streamPrelude) release() {
	if p == nil || p.released {
		return
	}
	p.released = true
	if p.buf.Len() > 0 {
		_, _ = p.w.Write(p.buf.Bytes())
		p.buf.Reset()
	}
	p.Flush()
}

func observeStreamTTFB(account, endpoint string, started time.Time) {
	metrics.LLMTTFB.WithLabelValues(account, endpoint).Observe(time.Since(started).Seconds())
}

// PipeAndExtractOpts relays an SSE stream to dst while feeding every frame
// to acc and the hooks in opts.
func PipeAndExtractOpts(dst io.Writer, src io.Reader, acc *Accumulator, opts PipeOptions) error {
	flusher, _ := dst.(http.Flusher)
	br := bufio.NewReaderSize(src, 64<<10)
	firstFrameSeen := opts.OnFirstFrame == nil
	firstTextSeen := opts.OnFirstText == nil
	firstOutputSeen := opts.OnFirstOutput == nil
	var complete streamCompleteness

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var blockErr error
			outLine := line
			isContentFrame := false
			if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(line[len("data:"):])
				complete.observe(payload)
				// Real content progress (any non-empty data: frame that isn't
				// the terminal sentinel) — drives the chunk-idle timer so
				// upstream keepalive comments can't mask a stalled stream.
				isContentFrame = len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]"))
				if !firstFrameSeen && len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					opts.OnFirstFrame()
					firstFrameSeen = true
				}
				if !firstOutputSeen && isFirstOutputPayload(payload) {
					opts.OnFirstOutput()
					firstOutputSeen = true
				}
				prevTextLen := 0
				if acc != nil && !firstTextSeen {
					prevTextLen = acc.AssistantText.Len()
				}
				firstTokenPayload := !firstTextSeen && isFirstTokenPayload(payload)
				if acc != nil {
					acc.Inspect(payload)
				}
				if !firstTextSeen && (firstTokenPayload || (acc != nil && acc.AssistantText.Len() > prevTextLen)) {
					opts.OnFirstText()
					firstTextSeen = true
				}
				if opts.OnFrame != nil {
					blockErr = opts.OnFrame(payload, acc)
				}
				// Optional payload rewrite (image-output gate, etc).
				// Skip for terminal sentinel "[DONE]" so we don't
				// accidentally turn it into something the client
				// won't recognize.
				if blockErr == nil && opts.TransformPayload != nil && len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					if newPayload := opts.TransformPayload(payload); newPayload != nil {
						outLine = append([]byte("data: "), newPayload...)
						outLine = append(outLine, '\n')
					}
				}
			}
			if blockErr == nil {
				if _, werr := dst.Write(outLine); werr != nil {
					return fmt.Errorf("%w: %w", errClientWrite, werr)
				}
				if flusher != nil {
					flusher.Flush()
				}
				if opts.AfterWrite != nil {
					opts.AfterWrite()
				}
				if isContentFrame && opts.AfterContentFrame != nil {
					opts.AfterContentFrame()
				}
			} else {
				// Blocked: stop streaming. The OnFrame hook is
				// responsible for having written a terminal error
				// event to dst already, if it wants the client to
				// see one.
				if flusher != nil {
					flusher.Flush()
				}
				return blockErr
			}
		}
		if err == io.EOF {
			// A clean EOF is only a finished answer if the upstream said so:
			// HTTP/2 END_STREAM and a close-delimited body both arrive here
			// looking exactly like success.
			return complete.err()
		}
		if err != nil {
			return err
		}
	}
}
