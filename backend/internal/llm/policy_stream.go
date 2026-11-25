package llm

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
)

// policyStreamHook returns a closure suitable for PipeOptions.OnFrame
// that runs the streaming policy scanner per the active groups' scopes.
//
// Two scanners are used in parallel:
//   - textScanner: fed only with assistant-text deltas (scope filter
//     "input_output"). Catches groups with scope input_output OR
//     input_output_tools.
//   - rawScanner: fed with the full SSE data frame (scope filter
//     "input_output_tools"). Only matches groups whose scope is
//     "input_output_tools" — they get the bonus of seeing tool_use
//     arguments / tool_result content inside the JSON payload.
//
// onBlock is invoked with the offending PolicyMatch right before we
// short-circuit; the gateway uses it to write a terminal error event
// and to record a user incident. Returning ErrPolicyBlocked tells
// PipeAndExtractOpts to stop streaming further bytes.
//
// When no enabled groups have any output-scope at all, returns nil so
// callers can skip the hook (0 overhead default path).
type policyOnBlock func(*biz.PolicyMatch)

func (g *Gateway) policyStreamHook(onBlock policyOnBlock) func([]byte, *Accumulator) error {
	if g.policy == nil {
		return nil
	}
	hasText := g.policy.HasAnyScope(biz.ScopeInputOutput, biz.ScopeInputOutputTools)
	hasTools := g.policy.HasAnyScope(biz.ScopeInputOutputTools)
	if !hasText && !hasTools {
		return nil
	}
	textScanner := g.policy.NewStreamScanner()
	rawScanner := g.policy.NewStreamScanner()

	prevTextLen := 0
	return func(payload []byte, acc *Accumulator) error {
		if hasText {
			full := acc.AssistantText.String()
			if len(full) > prevTextLen {
				delta := full[prevTextLen:]
				prevTextLen = len(full)
				if hit := textScanner.Feed([]byte(delta), biz.ScopeInputOutput); hit != nil && hit.Action == "block" {
					if onBlock != nil {
						onBlock(hit)
					}
					return ErrPolicyBlocked
				}
			}
		}
		if hasTools {
			if hit := rawScanner.Feed(payload, biz.ScopeInputOutputTools); hit != nil && hit.Action == "block" {
				if onBlock != nil {
					onBlock(hit)
				}
				return ErrPolicyBlocked
			}
		}
		return nil
	}
}

// writePolicyBlockedSSE writes a graceful "stop the stream cleanly"
// signal that matches each endpoint's native protocol, so the client
// SDK does NOT retry. Previously we wrote `event: error` which Codex
// and other SDKs treat as a transient upstream failure and re-issue
// the request — causing repeated blocks instead of a single rejection.
//
// Per-endpoint termination shape:
//
//   - chat_completions:  one chunk with finish_reason=content_filter,
//     then a "data: [DONE]" sentinel. OpenAI SDKs treat content_filter
//     as a terminal finish, not an error → no retry.
//   - responses:         emit response.completed (with status="completed"
//     and incomplete_details.reason="content_filter") so the OpenAI
//     Responses SDK / Codex sees a normal stream close.
//   - messages (Anthropic): the caller invokes emitter.finish("end_turn")
//     instead — Anthropic has no SSE-level moderation kind, but
//     end_turn is a clean stop the SDK won't retry on.
//
// The helper writes nothing for endpoint="messages" because anthropic
// path handles its own emitter; caller checks endpoint and routes.
func writePolicyBlockedSSE(w io.Writer, hit *biz.PolicyMatch, endpoint string) {
	switch endpoint {
	case "responses":
		writeResponsesBlockedTermination(w, hit)
	case "messages":
		// caller (anthropic.go) uses emitter.finish("end_turn") instead
		return
	default:
		// chat_completions and anything else falls through to the
		// Chat Completions stream shape — that's what Codex speaks.
		writeChatBlockedTermination(w, hit)
	}
}

// writeChatBlockedTermination emits one Chat Completions delta with
// finish_reason=content_filter followed by [DONE]. The id/created
// fields are placeholders — clients tolerate them.
func writeChatBlockedTermination(w io.Writer, hit *biz.PolicyMatch) {
	reason := "content_filter"
	chunk := map[string]any{
		"id":     "policy-blocked",
		"object": "chat.completion.chunk",
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": reason,
			},
		},
	}
	if hit != nil {
		chunk["x_jucode_block"] = map[string]any{
			"category": hit.Slug,
			"severity": hit.Severity,
		}
	}
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flushIfPossible(w)
}

// writeResponsesBlockedTermination emits a response.completed event
// shaped to look like a normal close. OpenAI's Responses SDK exits
// the stream loop on this event; without it, errors keep the SDK in
// a retry-with-backoff state.
func writeResponsesBlockedTermination(w io.Writer, hit *biz.PolicyMatch) {
	payload := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "policy-blocked",
			"status": "incomplete",
			"incomplete_details": map[string]any{
				"reason": "content_filter",
			},
		},
	}
	if hit != nil {
		if r, ok := payload["response"].(map[string]any); ok {
			r["x_jucode_block"] = map[string]any{
				"category": hit.Slug,
				"severity": hit.Severity,
			}
		}
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", b)
	flushIfPossible(w)
}

func writeStreamAbortTermination(w io.Writer, endpoint, requestID, text string) {
	switch endpoint {
	case "responses":
		writeResponsesAbortTermination(w, requestID, text)
	case "messages":
		writeAnthropicAbortTermination(w, requestID)
	default:
		writeChatAbortTermination(w, requestID)
	}
}

// writeChatAbortTermination emits a Chat Completions in-stream error frame
// followed by [DONE]. OpenAI-compatible clients raise on a frame carrying
// "error", so the caller sees a real failure — and retry-aware clients
// re-issue immediately — instead of a truncated stream that looks like a
// normal finish (the old finish_reason:"length" disguise).
func writeChatAbortTermination(w io.Writer, requestID string) {
	errObj := map[string]any{
		"message": "Upstream request failed",
		"type":    "upstream_error",
		"code":    "upstream_error",
	}
	if requestID != "" {
		errObj["request_id"] = requestID
	}
	b, _ := json.Marshal(map[string]any{"error": errObj})
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flushIfPossible(w)
}

// writeResponsesAbortTermination emits a response.failed event. A fake
// response.completed previously let clients mistake a truncated stream for a
// finished one; response.failed makes SDKs raise, and retry-aware clients
// (agent-core's is_retryable_response_failed matches code "upstream_error")
// re-issue the request immediately.
func writeResponsesAbortTermination(w io.Writer, requestID, text string) {
	output := []any{}
	if text != "" {
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "output_text",
					"text": text,
				},
			},
		})
	}
	response := map[string]any{
		"id":     "stream-aborted",
		"status": "failed",
		"error": map[string]any{
			"code":    "upstream_error",
			"message": "Upstream request failed",
		},
		"output": output,
	}
	if requestID != "" {
		response["request_id"] = requestID
	}
	payload := map[string]any{
		"type":     "response.failed",
		"response": response,
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", b)
	flushIfPossible(w)
}

// writeAnthropicAbortTermination emits an Anthropic error event (api_error)
// instead of a fake end_turn/message_stop, so the SDK raises and the
// client's retry logic (api_error is retryable) engages at once.
func writeAnthropicAbortTermination(w io.Writer, requestID string) {
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": "Upstream request failed",
		},
	}
	if requestID != "" {
		payload["request_id"] = requestID
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
	flushIfPossible(w)
}

func flushIfPossible(w io.Writer) {
	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
}

// recordPolicyOutputIncident logs an incident row for an output-side
// policy hit. Mirrors the input-side recorder in gateway.handle.
func (g *Gateway) recordPolicyOutputIncident(c *gin.Context, hit *biz.PolicyMatch,
	user *uuid.UUID, apiKeyID *uuid.UUID, requestID, modelName, endpoint string,
) {
	if hit == nil || g.incidents == nil {
		return
	}
	g.incidents.RecordAsync(biz.IncidentInput{
		UserID:    user,
		ApiKeyID:  apiKeyID,
		Kind:      "policy_keyword",
		Severity:  hit.Severity,
		Action:    "blocked",
		Model:     modelName,
		Endpoint:  endpoint,
		RequestID: requestID,
		IP:        c.ClientIP(),
		Message:   "blocked by output moderation policy: " + hit.Slug,
		Payload: func() map[string]any {
			p := biz.PolicyIncidentPayload([]biz.PolicyMatch{*hit}, false)
			p["side"] = "output"
			p["scope"] = hit.Scope
			return p
		}(),
	})
}
