// anthropic_count_tokens.go: POST /v1/messages/count_tokens and the
// /anthropic/v1/messages/count_tokens alias.
//
// Read-only sibling of HandleMessages: authentication, model routing and
// group-access checks are identical, but the request is never billed,
// never reserved against a plan and never counted by the rate/concurrency
// gates — count produces no assistant output.
//
// Counting strategy: forward to the first provider that speaks the
// Anthropic wire protocol natively (their /v1/messages/count_tokens gives
// the exact tokenizer answer); when no such provider answers, fall back
// to a local conservative estimate so the endpoint still works for
// models routed exclusively through Chat-format providers.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// countTokensUpstreamTimeout bounds one upstream count attempt. Counting is
// a small synchronous JSON call clients fire before the real request
// (Claude Code checks context-window headroom with it), so a slow upstream
// must fail over to the local estimate quickly instead of hanging.
const countTokensUpstreamTimeout = 15 * time.Second

// countTokensMaxResponseBytes caps how much of an upstream count response
// is buffered. The real payload is a tiny JSON object; anything bigger is
// a misbehaving upstream.
const countTokensMaxResponseBytes = 1 << 20

// HandleMessagesCountTokens implements POST /v1/messages/count_tokens (and
// the /anthropic/v1 alias). Response shape matches Anthropic's: at minimum
// {"input_tokens": N}.
func (g *Gateway) HandleMessagesCountTokens(c *gin.Context) {
	requestID := gatewayRequestID(c)
	user, ok := middleware.UserFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, anthropicError("authentication_error", "no user", requestID))
		return
	}
	apiKey, _ := middleware.ApiKeyFrom(c)

	// Same LimitReader guard as HandleMessages.
	var raw []byte
	var err error
	if g.maxBodyBytes > 0 {
		raw, err = io.ReadAll(io.LimitReader(c.Request.Body, g.maxBodyBytes+1))
		if err == nil && int64(len(raw)) > g.maxBodyBytes {
			c.JSON(http.StatusRequestEntityTooLarge, anthropicError("invalid_request_error", "request body too large", requestID))
			return
		}
	} else {
		raw, err = io.ReadAll(c.Request.Body)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", "failed to read request body", requestID))
		return
	}
	var anthReq anthropicRequest
	if err := json.Unmarshal(raw, &anthReq); err != nil {
		c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", "body must be valid JSON", requestID))
		return
	}
	if anthReq.Model == "" {
		c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", "missing model", requestID))
		return
	}

	// Routing + group access identical to HandleMessages, so count never
	// succeeds for a model/group the same key could not generate with.
	resolveProtocol := biz.RequestProtocolAnthropic
	if g.formatConversionEnabled(c) {
		resolveProtocol = biz.RequestProtocolAny
	}
	route, err := g.models.Resolve(c.Request.Context(), anthReq.Model, user, apiKey, resolveProtocol)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrModelNotFound):
			c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", "model not found: "+anthReq.Model, requestID))
		case errors.Is(err, biz.ErrModelNotInGroup):
			c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", routeErrorMessage(anthReq.Model, err), requestID))
		case errors.Is(err, biz.ErrGroupDeniedForUser):
			c.JSON(http.StatusForbidden, anthropicError("permission_error", err.Error(), requestID))
		case errors.Is(err, biz.ErrNoProvider):
			c.JSON(http.StatusServiceUnavailable, anthropicError("api_error", routeErrorMessage(anthReq.Model, err), requestID))
		default:
			c.JSON(http.StatusInternalServerError, anthropicError("api_error", "internal server error", requestID))
		}
		return
	}
	if err := g.models.CheckAccess(c.Request.Context(), user, apiKey, route.Group); err != nil {
		c.JSON(http.StatusForbidden, anthropicError("permission_error", err.Error(), requestID))
		return
	}

	if body, ok := g.upstreamCountTokens(c, route, raw); ok {
		c.Data(http.StatusOK, "application/json", body)
		return
	}
	c.JSON(http.StatusOK, gin.H{"input_tokens": estimateAnthropicInputTokens(&anthReq)})
}

// upstreamCountTokens tries the providers that accept the Anthropic wire
// protocol natively, in route order, bounded by the same attempt budget as
// generation. Returns the upstream JSON verbatim once one answers with a
// valid count. Failures are silent by design — the caller falls back to
// the local estimate, so a flaky upstream must not fail the request.
func (g *Gateway) upstreamCountTokens(c *gin.Context, route *biz.Route, raw []byte) ([]byte, bool) {
	attempts := 0
	for _, rp := range route.Providers {
		if attempts >= g.maxAttempts {
			break
		}
		if rp.Account == nil || !biz.AccountHasNativeProtocol(rp.Account, biz.NativeProtocolMessages) {
			continue
		}
		body, err := anthropicNativeBody(raw, rp.UpstreamModel, rp.Account.DefinitionID)
		if err != nil {
			continue
		}
		// No usage row on this path (the probe is not billed), so no
		// attribution to set here.
		prov, err := g.buildProviderForAttempt(c.Request.Context(), rp, biz.RequestProtocolAnthropic)
		if err != nil {
			continue
		}
		attempts++
		ctx, cancel := context.WithTimeout(c.Request.Context(), countTokensUpstreamTimeout)
		resp, err := prov.Forward(ctx, "/v1/messages/count_tokens", body, false, c.Request.Header)
		if err != nil {
			cancel()
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, countTokensMaxResponseBytes))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
		}
		// Only relay a body that actually carries a count; a 2xx with an
		// unexpected shape (proxy splash page, HTML error) must not reach
		// the client as a count response.
		var parsed struct {
			InputTokens *int64 `json:"input_tokens"`
		}
		if json.Unmarshal(b, &parsed) != nil || parsed.InputTokens == nil {
			continue
		}
		return b, true
	}
	return nil, false
}

// Local-estimate cost constants. Deliberately conservative (round up, flat
// worst-case image cost) so a caller sizing a request against the context
// window errs on the safe side.
const (
	// countTokensBaseOverhead approximates the fixed per-request prompt
	// framing Anthropic's tokenizer adds around the conversation.
	countTokensBaseOverhead = 3
	// countTokensMessageOverhead approximates the per-message role/framing
	// tokens.
	countTokensMessageOverhead = 5
	// countTokensImageFlatCost is Anthropic's documented worst-case cost of
	// one image block (~1.15 megapixels ≈ 1590 tokens), rounded up.
	countTokensImageFlatCost = 1600
)

// estimateAnthropicInputTokens is the local fallback when no Anthropic-
// native upstream can count. It walks the same fields the tokenizer sees —
// system, message content blocks, and the tools declaration — and converts
// text with a conservative heuristic (see estimateTokensFromText).
func estimateAnthropicInputTokens(req *anthropicRequest) int {
	total := countTokensBaseOverhead
	if len(req.System) > 0 {
		total += estimateTokensFromText(flattenAnthropicSystem(req.System))
	}
	for _, m := range req.Messages {
		total += countTokensMessageOverhead
		total += estimateAnthropicContentTokens(m.Content)
	}
	// Tool declarations are serialized into the prompt; their JSON length
	// is a fair proxy for what the model actually sees.
	if len(req.Tools) > 0 {
		if rawTools, err := json.Marshal(req.Tools); err == nil {
			total += estimateTokensFromText(string(rawTools))
		}
	}
	return total
}

// estimateAnthropicContentTokens handles one message's content, which is
// either a plain string or an array of typed blocks. Non-text blocks
// (tool_use, tool_result, thinking, documents…) are costed by their JSON
// length — that is what gets serialized into the prompt — and image blocks
// get a flat worst-case cost so base64 payload length doesn't explode the
// estimate.
func estimateAnthropicContentTokens(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return estimateTokensFromText(asStr)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return estimateTokensFromText(string(raw))
	}
	total := 0
	for _, b := range blocks {
		switch t, _ := b["type"].(string); t {
		case "text":
			s, _ := b["text"].(string)
			total += estimateTokensFromText(s)
		case "image":
			total += countTokensImageFlatCost
		default:
			if enc, err := json.Marshal(b); err == nil {
				total += estimateTokensFromText(string(enc))
			}
		}
	}
	return total
}

// estimateTokensFromText converts text length to a conservative token
// count: ~4 ASCII characters per token (the usual English rule of thumb)
// and one full token per non-ASCII rune, since CJK and most other scripts
// tokenize at roughly one token per character or denser.
func estimateTokensFromText(s string) int {
	ascii := 0
	tokens := 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			tokens++
		}
	}
	return tokens + (ascii+3)/4
}
