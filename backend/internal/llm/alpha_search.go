package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/middleware"
)

// HandleAlphaSearch implements POST /v1/alpha/search (Codex standalone
// web search). The body is forwarded to the same upstream path. When the
// upstream reports no usage, one web_search request is recorded so the
// call is visible on the usage record.
func (g *Gateway) HandleAlphaSearch(c *gin.Context) {
	startedAt := time.Now()
	requestID := gatewayRequestID(c)
	writeErr := func(status int, kind, code, message string) {
		c.JSON(status, openAIError(kind, code, message, requestID))
	}
	user, ok := middleware.UserFrom(c)
	if !ok {
		writeErr(http.StatusUnauthorized, "authentication_error", "missing_user", "no user")
		return
	}
	apiKey, _ := middleware.ApiKeyFrom(c)

	raw, err := readLimitedBody(c, g.maxBodyBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeErr(http.StatusRequestEntityTooLarge, "invalid_request_error", "request_body_too_large", "request body too large")
			return
		}
		writeErr(http.StatusBadRequest, "invalid_request_error", "read_body_failed", "failed to read request body")
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(http.StatusBadRequest, "invalid_request_error", "invalid_json", "body must be valid JSON")
		return
	}
	modelName, _ := body["model"].(string)
	if modelName == "" {
		writeErr(http.StatusBadRequest, "invalid_request_error", "missing_model", "missing model")
		return
	}

	reqCtx := c.Request.Context()
	if g.requestTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, g.requestTimeout)
		defer cancel()
	}

	lc := logContext{
		IP:              c.ClientIP(),
		UserAgent:       c.Request.UserAgent(),
		ApiKeyName:      keyName(apiKey),
		AuthorizationID: middleware.AuthzIDFrom(c),
	}

	if g.policy != nil {
		scan := g.policy.Scan(biz.RedactImagesForScan(raw))
		if block, _ := scan.BlockDecision(); block {
			writeErr(http.StatusUnprocessableEntity, "content_policy_violation", "input_policy_block", "blocked by input moderation policy")
			return
		}
	}

	ep := entusagelog.EndpointChatCompletions
	route, err := g.models.Resolve(reqCtx, modelName, user, apiKey, biz.RequestProtocolOpenAI)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrModelNotFound):
			writeErr(http.StatusBadRequest, "invalid_request_error", "model_not_found", "model not found: "+modelName)
		case errors.Is(err, biz.ErrModelNotInGroup):
			writeErr(http.StatusBadRequest, "invalid_request_error", "model_group_unavailable", routeErrorMessage(modelName, err))
		case errors.Is(err, biz.ErrGroupDeniedForUser):
			writeErr(http.StatusForbidden, "permission_error", "access_denied", err.Error())
		case errors.Is(err, biz.ErrNoProvider):
			g.recordNoProviderAvailability(reqCtx, err, requestID, modelName, ep, startedAt)
			writeErr(http.StatusServiceUnavailable, "service_unavailable", "no_available_provider", routeErrorMessage(modelName, err))
		default:
			writeErr(http.StatusInternalServerError, "api_error", "internal_error", "internal server error")
		}
		return
	}
	if err := g.models.CheckAccess(reqCtx, user, apiKey, route.Group); err != nil {
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, biz.AvailabilityFailureAccess, http.StatusForbidden, startedAt)
		writeErr(http.StatusForbidden, "permission_error", "access_denied", err.Error())
		return
	}
	planState, err := g.billing.ResolvePlanState(reqCtx, user, apiKey, route.Group)
	if err != nil {
		writeErr(http.StatusPaymentRequired, "billing_error", "billing_rejected", err.Error())
		return
	}
	if g.concur != nil {
		release, err := g.concur.AcquireWithPlanState(reqCtx, user, route.Group, planState)
		if err != nil {
			g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, "concurrency_limit", http.StatusTooManyRequests, startedAt)
			writeErr(http.StatusTooManyRequests, "rate_limit_error", "concurrency_limit", err.Error())
			return
		}
		defer release()
	}

	// Peek phase of the rate-limit protocol, same as the chat path: search
	// requests count against RPM/TPM too, otherwise this endpoint is a way
	// around the caps.
	ticket, rlErr := g.rateLimit.Peek(reqCtx, user, route.Group, planState)
	lc.RateTicket = ticket
	if rlErr != nil {
		code := "rpm_limit"
		if errors.Is(rlErr, biz.ErrTPMLimit) {
			code = "tpm_limit"
		}
		// Same as the chat path: the cap is ours, so hint the time until the
		// current window rolls over. Without it a client retries immediately
		// and burns its way through the window.
		setRetryAfter(c, nil, rateLimitRetryAfter)
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, code, http.StatusTooManyRequests, startedAt)
		writeErr(http.StatusTooManyRequests, "rate_limit_error", code, rlErr.Error())
		return
	}

	var lastErr error
	var lastStatus int
	for i, rp := range route.Providers {
		if i >= g.maxAttempts {
			break
		}
		lc.AccountID = accountIDRef(rp.Account)
		prov, err := g.buildProviderForAttempt(reqCtx, rp, biz.RequestProtocolOpenAI)
		if err != nil {
			lastErr = err
			continue
		}
		encoded, err := prepareForwardBody(body, rp.UpstreamModel, false)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := prov.Forward(reqCtx, "/v1/alpha/search", encoded, false, c.Request.Header)
		if err != nil {
			lastErr = err
			lastStatus = 0
			continue
		}
		rawResp, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastStatus = resp.StatusCode
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(rawResp), 256))
			continue
		}
		if resp.StatusCode >= 400 {
			ct := resp.Header.Get("Content-Type")
			if ct == "" {
				ct = "application/json"
			}
			c.Data(resp.StatusCode, ct, rawResp)
			g.logFinal(c, user, apiKey, requestID, ep, modelName, route, rp, rp.UpstreamModel,
				&Accumulator{}, nil, startedAt, nil, 0, entusagelog.StatusUpstreamError, resp.StatusCode, truncate(string(rawResp), 256), lc)
			return
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		// Commit phase: an upstream accepted the call, so it counts. Connect
		// failures and 5xx retries above stay free.
		lc.RateTicket.Commit(reqCtx)
		c.Data(resp.StatusCode, ct, rawResp)
		acc := &Accumulator{}
		acc.Inspect(rawResp)
		if acc.WebSearchRequests == 0 {
			acc.WebSearchRequests = 1
			acc.HasUsage = true
		}
		g.chargeAndLog(reqCtx, user, apiKey, requestID, ep, modelName, route, rp, rp.UpstreamModel,
			acc, nil, startedAt, nil, 0, resp.StatusCode, lc, planState)
		return
	}
	msg := "upstream unavailable"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	status := http.StatusBadGateway
	if lastStatus >= 500 {
		status = http.StatusServiceUnavailable
	}
	writeErr(status, "upstream_error", "upstream_unavailable", msg)
}
