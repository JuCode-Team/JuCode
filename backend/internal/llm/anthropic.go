package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/metrics"
	"github.com/jucode/jucode/internal/middleware"
)

// HandleMessages implements POST /anthropic/v1/messages and the Grok Build
// alias POST /v1/messages.
//
// Inbound request is in Anthropic's Messages-API shape. For Anthropic-format
// providers we forward /v1/messages natively; for Chat-format providers we
// translate through OpenAI Chat Completions and translate the response back.
// Non-stream is also supported by buffering the chat SSE and emitting a single
// Messages object. Usage and availability rows use endpoint=messages.

func (g *Gateway) HandleMessages(c *gin.Context) {
	startedAt := time.Now()
	requestID := gatewayRequestID(c)
	user, ok := middleware.UserFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, anthropicError("authentication_error", "no user", requestID))
		return
	}
	apiKey, _ := middleware.ApiKeyFrom(c)
	lc := logContext{
		IP:              c.ClientIP(),
		UserAgent:       c.Request.UserAgent(),
		ApiKeyName:      keyName(apiKey),
		AuthorizationID: middleware.AuthzIDFrom(c),
		TurnID:          agentTurnIDFrom(c),
		Latency:         newLatencyTracker(startedAt),
	}
	c.Writer = newTTFBResponseWriter(c.Writer, func() {
		if lc.TtfbMs != nil {
			return
		}
		d := int(time.Since(startedAt).Milliseconds())
		lc.TtfbMs = &d
	})

	// Same LimitReader guard as the OpenAI path (gateway.go). This entrypoint
	// used to read the body unbounded, so /anthropic/v1/messages was the one
	// generation route with no cap on how much it would buffer in memory.
	var raw []byte
	var err error
	stageStart := time.Now()
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
	lc.Latency.Duration("body_read_ms", time.Since(stageStart))
	lc.Latency.Value("body_bytes", len(raw))
	if c.Request.ContentLength >= 0 {
		lc.Latency.Value("content_length_bytes", int(c.Request.ContentLength))
	}
	var anthReq anthropicRequest
	stageStart = time.Now()
	if err := json.Unmarshal(raw, &anthReq); err != nil {
		c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", "body must be valid JSON", requestID))
		return
	}
	lc.Latency.Duration("json_decode_ms", time.Since(stageStart))
	if anthReq.Model == "" {
		c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", "missing model", requestID))
		return
	}
	lc.ReasoningEffort = g.reasoningEffortFromRaw(c.Request.Context(), raw)
	stageStart = time.Now()
	scan := g.policy.Scan(biz.RedactImagesForScan(raw))
	lc.Latency.Duration("policy_scan_ms", time.Since(stageStart))
	block, topSev := scan.BlockDecision()
	uid := user.ID
	if len(scan.Hits) > 0 {
		cats := make([]string, 0, len(scan.Hits))
		for _, h := range scan.Hits {
			cats = append(cats, h.Slug)
		}
		action := "logged"
		msg := "policy categories (logged): " + strings.Join(cats, ",")
		payload := biz.PolicyIncidentPayload(scan.Hits, scan.Truncated)
		if block {
			action = "blocked"
			msg = "blocked by policy categories: " + strings.Join(cats, ",")
		}
		g.incidents.RecordAsync(biz.IncidentInput{
			UserID:    &uid,
			ApiKeyID:  apiKeyID(apiKey),
			Kind:      "policy_keyword",
			Severity:  topSev,
			Action:    action,
			Model:     anthReq.Model,
			Endpoint:  "messages",
			RequestID: requestID,
			IP:        c.ClientIP(),
			Message:   msg,
			Payload:   payload,
		})
		if block {
			c.JSON(http.StatusUnprocessableEntity, anthropicError("content_policy_violation", "blocked by input moderation policy", requestID))
			return
		}
	}
	g.moderation.MaybeReview(biz.ModerationRequest{
		UserID:    uid,
		ApiKeyID:  apiKeyID(apiKey),
		Model:     anthReq.Model,
		Endpoint:  "messages",
		RequestID: requestID,
		IP:        c.ClientIP(),
		Text:      biz.RedactImagesForScan(raw),
		Scan:      scan,
	})
	// Tools are now fully translated end-to-end (see anthropic_translate.go
	// for request-side schema + tool_choice mapping; anthropic_emitter.go
	// for response-side tool_use SSE emission).

	lc.Stream = anthReq.Stream

	// Route. Conversion-on lets an Anthropic client reach openai-only
	// (and vice versa) providers; conversion-off keeps protocol filter.
	resolveProtocol := biz.RequestProtocolAnthropic
	if g.formatConversionEnabled(c) {
		resolveProtocol = biz.RequestProtocolAny
	}
	stageStart = time.Now()
	convID := ClientSessionAnchor(c.Request.Header, nil)
	resolveCtx := g.withStickyGroup(c.Request.Context(), user.ID, anthReq.Model, convID)
	route, err := g.models.Resolve(resolveCtx, anthReq.Model, user, apiKey, resolveProtocol)
	lc.Latency.Duration("route_ms", time.Since(stageStart))
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrModelNotFound):
			c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", "model not found: "+anthReq.Model, requestID))
		case errors.Is(err, biz.ErrModelNotInGroup):
			c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", routeErrorMessage(anthReq.Model, err), requestID))
		case errors.Is(err, biz.ErrGroupDeniedForUser):
			c.JSON(http.StatusForbidden, anthropicError("permission_error", err.Error(), requestID))
		case errors.Is(err, biz.ErrNoProvider):
			g.recordNoProviderAvailability(c.Request.Context(), err, requestID, anthReq.Model, entusagelog.EndpointMessages, startedAt)
			c.JSON(http.StatusServiceUnavailable, anthropicError("api_error", routeErrorMessage(anthReq.Model, err), requestID))
		default:
			c.JSON(http.StatusInternalServerError, anthropicError("api_error", "internal server error", requestID))
		}
		return
	}

	// Routing shape for the usage log: how many groups were on the table and
	// which one auto-route took. Only written when there was a real choice,
	// so single-group traffic adds no noise.
	if route.CandidateGroups > 1 {
		lc.Latency.Value("route_candidate_groups", route.CandidateGroups)
		lc.Latency.Value("route_group_index", route.GroupIndex)
	}

	stageStart = time.Now()
	if err := g.models.CheckAccess(c.Request.Context(), user, apiKey, route.Group); err != nil {
		lc.Latency.Duration("access_check_ms", time.Since(stageStart))
		g.recordRouteAvailability(c.Request.Context(), route.Group.ID, requestID, anthReq.Model, entusagelog.EndpointMessages, entavailability.OutcomeExcluded, biz.AvailabilityFailureAccess, http.StatusForbidden, startedAt)
		c.JSON(http.StatusForbidden, anthropicError("permission_error", err.Error(), requestID))
		return
	}
	lc.Latency.Duration("access_check_ms", time.Since(stageStart))

	// Billing precheck runs BEFORE the concurrency gate, matching the OpenAI
	// path (gateway.go). Two reasons the order matters: a request that is
	// going to be rejected for quota should never burn an in-flight slot, and
	// the resolved planState lets AcquireWithPlanState skip re-querying the
	// active plan. These two blocks used to be reversed here.
	stageStart = time.Now()
	planState, err := g.billing.ResolvePlanState(c.Request.Context(), user, apiKey, route.Group)
	lc.Latency.Duration("billing_precheck_ms", time.Since(stageStart))
	if err != nil {
		status := http.StatusPaymentRequired
		errKind := "billing_error"
		switch {
		case errors.Is(err, biz.ErrUnsupportedBillingSource):
			status = http.StatusServiceUnavailable
			errKind = "api_error"
		case errors.Is(err, biz.ErrPlanNotAllowed):
			status = http.StatusForbidden
			errKind = "permission_error"
		case errors.Is(err, biz.ErrPlanQuotaExhausted):
			status = http.StatusTooManyRequests
			errKind = "rate_limit_error"
		case errors.Is(err, biz.ErrMonthlySpendLimitExceeded), errors.Is(err, biz.ErrApiKeyCostLimitExceeded):
			status = http.StatusTooManyRequests
			errKind = "rate_limit_error"
		case errors.Is(err, biz.ErrOrganizationLimit), errors.Is(err, biz.ErrMemberLimit):
			status = http.StatusTooManyRequests
			errKind = "rate_limit_error"
		}
		c.JSON(status, anthropicError(errKind, err.Error(), requestID))
		var groupID *uuid.UUID
		if route != nil {
			groupID = &route.Group.ID
		}
		// Classify off the status just computed, not a second list of error
		// values. The two lists drifted: the switch above routes every
		// user-side rejection to a 4xx, but this whitelist named only three
		// errors, so ErrInsufficientBalance — by far the most common one —
		// fell through and logged a user with no money as an internal_error.
		// Measured at 465 rows in 6h, 1.6% of all traffic, which is also why
		// availability.go carries an explicit 4xx workaround for this status.
		// A 4xx here is the caller; only a 5xx is ours.
		st := entusagelog.StatusQuotaExceeded
		if status >= 500 {
			st = entusagelog.StatusInternalError
		}
		_, _ = g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
			UserID: user.ID, ApiKeyID: apiKeyID(apiKey), RequestID: requestID,
			OrganizationID: organizationID(apiKey),
			Endpoint:       entusagelog.EndpointMessages, Model: anthReq.Model, ModelID: &route.Model.ID, GroupID: groupID,
			Status: st, HTTPStatus: status, LatencyMs: int(time.Since(startedAt).Milliseconds()),
			ErrorMessage: err.Error(), StartedAt: startedAt,
			IP: lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
			Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
			LatencyDetail: lc.Latency.Snapshot(),
		})
		return
	}

	// Peek phase of the rate-limit protocol: read-only, consumes nothing.
	stageStart = time.Now()
	ticket, rlErr := g.rateLimit.Peek(c.Request.Context(), user, route.Group, planState)
	lc.Latency.Duration("rate_limit_ms", time.Since(stageStart))
	lc.RateTicket = ticket
	if rlErr != nil {
		code := "rpm_limit"
		if errors.Is(rlErr, biz.ErrTPMLimit) {
			code = "tpm_limit"
		}
		setRetryAfter(c, nil, rateLimitRetryAfter)
		g.recordRouteAvailability(c.Request.Context(), route.Group.ID, requestID, anthReq.Model, entusagelog.EndpointMessages, entavailability.OutcomeExcluded, code, http.StatusTooManyRequests, startedAt)
		c.JSON(http.StatusTooManyRequests, anthropicError("rate_limit_error", rlErr.Error(), requestID))
		return
	}

	stageStart = time.Now()
	release, err := g.concur.AcquireWithPlanState(c.Request.Context(), user, route.Group, planState)
	lc.Latency.Duration("concurrency_ms", time.Since(stageStart))
	if err != nil {
		g.recordRouteAvailability(c.Request.Context(), route.Group.ID, requestID, anthReq.Model, entusagelog.EndpointMessages, entavailability.OutcomeExcluded, "concurrency_limit", http.StatusTooManyRequests, startedAt)
		c.JSON(http.StatusTooManyRequests, anthropicError("rate_limit_error", err.Error(), requestID))
		return
	}
	defer release()

	// Convert anthropic request → chat completions body. When format
	// conversion is on this uses ConvertRequest (web_search, tools, stop);
	// when off we keep the existing anthropicToChatBody fallback.
	//
	// forceChatStreamWithUsage then applies to whichever body came back,
	// deliberately outside the branch: both paths owe the same contract, and
	// the one time they were allowed to satisfy it separately one of them
	// silently stopped asking for usage and those requests billed zero.
	clientStream := anthReq.Stream
	stageStart = time.Now()
	var chatBody []byte
	if g.formatConversionEnabled(c) {
		chatBody, err = ConvertRequest(DialectAnthropic, DialectCompatible, raw)
	} else {
		chatBody, err = anthropicToChatBody(&anthReq, true /*upstreamStream*/)
	}
	if err == nil {
		chatBody, err = forceChatStreamWithUsage(chatBody)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, anthropicError("invalid_request_error", err.Error(), requestID))
		return
	}
	lc.Latency.Duration("request_translate_ms", time.Since(stageStart))

	// Reserve before any provider is contacted — same protocol as the
	// OpenAI gateway (see gateway.go): last-request / 0.25 water-line,
	// exact settle may go negative, CapAtHold does not. chargeAndLog
	// settles with the real usage, logFinal settles last-hop observed
	// usage (499 / truncated stream / timeout / response.failed) and
	// releases otherwise, and the direct-return paths below release
	// explicitly.
	stageStart = time.Now()
	estInput := biz.CostEstimateInput{
		BodyBytes:       len(raw),
		MaxOutputTokens: anthReq.MaxTokens,
	}
	estCost := g.billing.EstimateMaxCost(c.Request.Context(), route.Model, route.Group, estInput)
	reservation, resErr := g.billing.Reserve(c.Request.Context(), biz.ReserveInput{
		User: user, ApiKey: apiKey, Group: route.Group, PlanState: planState,
		Key: newBillingHoldKey("llm"), Amount: estCost, TTL: biz.ReserveTTLRequest,
		Note: "anthropic messages reserve " + requestID,
	})
	lc.Latency.Duration("billing_reserve_ms", time.Since(stageStart))
	if resErr != nil {
		// Fail closed: without a hold the last-request / overdraft rules
		// do not apply and the request must not contact upstream.
		status := http.StatusPaymentRequired
		errKind := "billing_error"
		switch {
		case errors.Is(resErr, biz.ErrInsufficientBalance):
			// keep 402
		case errors.Is(resErr, biz.ErrOrganizationLimit), errors.Is(resErr, biz.ErrMemberLimit):
			status, errKind = http.StatusTooManyRequests, "rate_limit_error"
		default:
			status, errKind = http.StatusServiceUnavailable, "api_error"
		}
		c.JSON(status, anthropicError(errKind, resErr.Error(), requestID))
		st := entusagelog.StatusQuotaExceeded
		if status >= 500 {
			st = entusagelog.StatusInternalError
		}
		_, _ = g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
			UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID,
			OrganizationID: organizationID(apiKey),
			Endpoint:       entusagelog.EndpointMessages, Model: anthReq.Model, ModelID: &route.Model.ID, GroupID: &route.Group.ID,
			Status: st, HTTPStatus: status, LatencyMs: int(time.Since(startedAt).Milliseconds()),
			ErrorMessage: "billing reserve: " + resErr.Error(), StartedAt: startedAt,
			IP: lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
			Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
			LatencyDetail: lc.Latency.Snapshot(),
		})
		return
	}
	lc.Reservation = reservation

	acc := &Accumulator{}
	providerChain := make([]string, 0, len(route.Providers))
	var (
		usedRP       biz.RouteAccount
		usedUpstream string
		retryCount   int
		lastErr      error
		lastStatus   int
		ttftMs       *int
	)
	attempts := &usageAttemptRecorder{endpoint: "messages", group: route.Group.ID.String()}
	syncAttempts := func() {
		providerChain = attempts.Chain()
		retryCount = attempts.RetryCount()
	}
	markFirstTextTimeout := func() {
		d := int(time.Since(startedAt).Milliseconds())
		lc.Latency.Value("first_token_timeout_ms", d)
	}
	// Sticky / bound account affinity, same rule as the OpenAI-family path:
	// promote the account this (user, group) or conversation was pinned to.
	route.Providers = g.orderRouteAccounts(c.Request.Context(), user, apiKey, route.Group.ID, convID, anthReq.Model, route.Providers)
	// capacityDrops counts candidates skipped because the account was at its
	// concurrency cap; it turns a request that never reached an upstream into
	// backpressure instead of "unreachable".
	var capacityDrops int
	// recordAccountFailure feeds a failure class into the account health state
	// machine. It never cools anything above the account: a sibling account is
	// still healthy, and one bad credential must not take the pool down.
	recordAccountFailure := func(rp biz.RouteAccount, reason string) biz.AccountFailureOutcome {
		if rp.Account == nil || g.accounts == nil {
			return biz.AccountFailureOutcome{}
		}
		outcome := g.accounts.ReportAccountFailure(c.Request.Context(), rp.Account, biz.AccountFailure{
			Class:  accountFailureClass(reason),
			Reason: reason,
			Model:  anthReq.Model,
		})
		if outcome.Action != biz.ActionNone {
			g.accounts.RecordOutcome(c.Request.Context(), rp.Account.ID, false)
		}
		return outcome
	}
	recordStickyFailure := func(rp biz.RouteAccount, reason string) {
		if rp.Account == nil || g.accounts == nil {
			return
		}
		detached, err := g.accounts.RecordStickyFailure(c.Request.Context(), user.ID, route.Group.ID, rp.Account.ID, convID, 2)
		if err != nil {
			slog.Debug("sticky.account_failure_record_failed", "err", err.Error())
			return
		}
		if detached {
			slog.Info("sticky.account_detached_after_failures",
				"account_id", rp.Account.ID.String(),
				"reason", reason,
			)
		}
	}
	// The wall-clock cap applies to every request, streamed or not. Without
	// it a stream:false request had no deadline of any kind: an upstream that
	// accepted the connection and then went silent would hang until the caller
	// gave up. The cap terminates such a request — it deliberately does not
	// fail over, since the budget is the whole request's, not this attempt's.
	streamCtx, totalTimeout := withStreamTotalTimeout(c.Request.Context(), g.streamTotalTimeout(c.Request.Context()))
	defer totalTimeout.Stop()
	// First-token is a stream-shaped guard and stays on the streaming path.
	// Native pass-through never reports a first token, so arming it there
	// would cut off a slow but healthy non-streamed answer; the buffered
	// translation path is covered by the idle timer instead.
	var firstTextTimeout *streamFirstTextTimeout
	if clientStream {
		streamCtx, firstTextTimeout = withStreamFirstTextTimeout(streamCtx, g.streamFirstTextTimeout(c.Request.Context()))
		defer firstTextTimeout.Stop()
	}
	// recordUpstreamFailure carries the full upstream signal into the account
	// state machine, so a 429 parks the account until the window it advertises
	// resets and a rejected credential disables it instead of a short cooldown.
	// It returns the state machine's verdict, so the loop can honour a lenient
	// "retry the same account" request.
	recordUpstreamFailure := func(rp biz.RouteAccount, cls errorClass, status int, h http.Header, body string) biz.AccountFailureOutcome {
		if rp.Account == nil || g.accounts == nil {
			return biz.AccountFailureOutcome{}
		}
		outcome := g.accounts.ReportAccountFailure(c.Request.Context(), rp.Account, biz.AccountFailure{
			Class:   string(cls),
			Status:  status,
			Reason:  fmt.Sprintf("status_%d", status),
			Headers: h,
			Body:    body,
			Model:   anthReq.Model,
		})
		if outcome.Action != biz.ActionNone {
			g.accounts.RecordOutcome(c.Request.Context(), rp.Account.ID, false)
		}
		return outcome
	}

	// Cross-group retry (product rule D8=B), same policy as the OpenAI
	// gateway: only after the whole preferred group failed with nothing
	// written, same billing_source only, within the key's rate cap, and
	// never for image-generation models.
	failover := newGroupFailover(g, user, apiKey, resolveProtocol, requestID,
		estInput, isImageGenerationModelName(anthReq.Model))

	// upstreamAttempts counts only attempts that actually reached an
	// upstream — local pre-flight failures don't consume the retry budget.
	// The budget is PER GROUP and resets on every cross-group switch;
	// maxGroupSwitches caps the total (see cross_group.go).
	upstreamAttempts := 0
	// The RPM counter moves once per request, not once per accepted attempt:
	// an upstream can answer 2xx and then die before its terminal frame, and
	// the request fails over from there. Committing again would spend the
	// caller's window twice on one call.
	rateCommitted := false
	commitRate := func() {
		if rateCommitted {
			return
		}
		rateCommitted = true
		lc.RateTicket.Commit(c.Request.Context())
	}
	// In lenient pool mode the health state machine asks for the same account
	// to be retried instead of moving on. sameRetries bounds that per candidate.
	sameRetries := make([]int, len(route.Providers))
	shouldRetrySame := func(i int, outcome biz.AccountFailureOutcome) bool {
		if !outcome.RetrySame || i < 0 || i >= len(sameRetries) || sameRetries[i] >= maxSameAccountRetries {
			return false
		}
		sameRetries[i]++
		return true
	}
	// ctxFloor is set after a context-too-long rejection: only accounts with a
	// larger window are still worth trying (see contextRetryWindow).
	ctxFloor := 0
	// relayRetries counts waits for a restarting local relay (relay_retry.go).
	relayRetries := 0
attemptLoop:
	for {
		for i := 0; i < len(route.Providers); i++ {
			rp := route.Providers[i]
			if upstreamAttempts >= g.maxAttempts {
				// Per-group budget spent. Leave the inner loop rather than the
				// whole request: cross-group retry below still gets its turn,
				// and the budget resets for the group it moves to. Breaking
				// all the way out here is why a group with max_attempts-or-more
				// channels could never fail over.
				break
			}
			if ctxFloor > 0 && rp.ContextWindow <= ctxFloor {
				continue
			}
			usedRP = rp
			usedUpstream = rp.UpstreamModel
			attemptIdx := attempts.Start(rp)
			syncAttempts()

			// An account that answered 2xx and then died has already fed this
			// accumulator its tokens, text, tool calls and images. Carrying
			// that into the next account bills the caller for output nobody
			// delivered and lets the output-policy scan match across the seam
			// between two answers.
			// Discarding it is safe because no retry edge is reachable once
			// bytes have reached the client.
			*acc = Accumulator{}

			// Native pass-through whenever the account accepts the Anthropic
			// wire protocol; translation to Chat format is the fallback for
			// accounts that only speak Chat Completions.
			if rp.Account != nil && biz.AccountHasNativeProtocol(rp.Account, biz.NativeProtocolMessages) {
				attemptStage := time.Now()
				bodyWithModel, err := anthropicNativeBody(raw, rp.UpstreamModel, rp.Account.DefinitionID)
				if err != nil {
					attempts.SetLatency(attemptIdx, "request_prepare_ms", time.Since(attemptStage))
					lastErr = err
					attempts.SetResult(attemptIdx, 0, err.Error(), "request_prepare_error")
					syncAttempts()
					continue
				}
				attempts.SetLatency(attemptIdx, "request_prepare_ms", time.Since(attemptStage))
				attemptStage = time.Now()
				if !g.acquireAccountSlot(c.Request.Context(), rp) {
					// The account is at its concurrency cap: try the next
					// candidate (a sibling account or the next channel).
					attempts.SetLatency(attemptIdx, "provider_build_ms", time.Since(attemptStage))
					attempts.SetResult(attemptIdx, 0, "account at concurrency cap", "account_busy")
					syncAttempts()
					capacityDrops++
					continue
				}
				// Held until the handler returns, which covers the streaming copy.
				defer g.releaseAccountSlot(c.Request.Context(), rp)
				lc.AccountID = accountIDRef(rp.Account)
				prov, err := g.buildProviderForAttempt(c.Request.Context(), rp, biz.RequestProtocolAnthropic)
				attempts.SetLatency(attemptIdx, "provider_build_ms", time.Since(attemptStage))
				if err != nil {
					lastErr = err
					attempts.SetResult(attemptIdx, 0, err.Error(), "build_provider_error")
					syncAttempts()
					recordStickyFailure(rp, "build_provider_error")
					continue
				}

				// streamCtx, not the bare request context: the wall-clock cap
				// has to reach the upstream call. A non-streamed pass-through
				// reads the body in one go and has no frames to go idle
				// between, so it gets no stall detector — the cap is its only
				// deadline.
				attemptCtx := streamCtx
				var idleTimeout *streamIdleTimeout
				if clientStream {
					attemptCtx, idleTimeout = withStreamIdleTimeout(streamCtx, g.streamIdleTimeout(c.Request.Context()))
					defer idleTimeout.Stop()
				}
				attemptStage = time.Now()
				resp, err := prov.Forward(attemptCtx, "/v1/messages", bodyWithModel, clientStream, c.Request.Header)
				attempts.SetLatency(attemptIdx, "upstream_header_ms", time.Since(attemptStage))
				metrics.LLMAttemptDuration.WithLabelValues(metricOwner(rp), "messages").Observe(time.Since(attemptStage).Seconds())
				upstreamAttempts++
				if err != nil {
					lastErr = err
					lastStatus = 0
					lc.UpstreamRequestID = ""
					if streamFirstTextTimedOut(attemptCtx) {
						markFirstTextTimeout()
						lastErr = errUpstreamFirstTextTimeout
						attempts.SetResult(attemptIdx, 0, lastErr.Error(), "first_text_timeout")
						attempts.MarkFinal(attemptIdx)
						lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
						syncAttempts()
						metrics.LLMStreamFirstTextTimeout.WithLabelValues(metricOwner(rp)).Inc()
						recordAccountFailure(rp, "first_text_timeout")
						recordStickyFailure(rp, "first_text_timeout")
						break attemptLoop
					}
					if streamIdleTimedOut(attemptCtx) {
						lastErr = errUpstreamStreamIdleTimeout
						attempts.SetResult(attemptIdx, 0, lastErr.Error(), "stream_idle_timeout")
						lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
						syncAttempts()
						metrics.LLMStreamIdleTimeout.WithLabelValues(metricOwner(rp)).Inc()
						outcome := recordAccountFailure(rp, "stream_idle_timeout")
						recordStickyFailure(rp, "stream_idle_timeout")
						if shouldRetrySame(i, outcome) {
							i--
						}
						continue
					}
					if d, ok := relayRestartDelay(rp, err, relayRetries); ok && c.Request.Context().Err() == nil {
						// Wait out a local relay restart (relay_retry.go).
						relayRetries++
						upstreamAttempts--
						attempts.SetResult(attemptIdx, 0, err.Error(), "relay_restart_retry")
						lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(err.Error(), 8192)
						syncAttempts()
						sleepCtx(c.Request.Context(), d)
						i--
						continue
					}
					attempts.SetResult(attemptIdx, 0, err.Error(), "forward_error")
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(err.Error(), 8192)
					syncAttempts()
					outcome := recordAccountFailure(rp, "forward_error")
					recordStickyFailure(rp, "forward_error")
					if shouldRetrySame(i, outcome) {
						i--
					}
					continue
				}
				lastStatus = resp.StatusCode
				lc.UpstreamRequestID = captureUpstreamRequestID(resp.Header)

				if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
					b, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					cls := classifyUpstreamResponse(resp.StatusCode, b)
					lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
					lc.UpstreamRawError = truncate(string(b), 8192)
					if cls == classPoolExhausted {
						attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "pool_exhausted")
						attempts.MarkFinal(attemptIdx)
						lc.ErrorClass = string(attempts.FinalClass())
						syncAttempts()
						setRetryAfter(c, resp.Header, 30*time.Second)
						c.JSON(http.StatusServiceUnavailable, anthropicError("api_error", upstreamUnavailableMessage(), requestID))
						g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model, route, rp, usedUpstream,
							acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, http.StatusServiceUnavailable, lastErr.Error(), lc)
						return
					}
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), switchStrategyForStatus(resp.StatusCode))
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					recordUpstreamFailure(rp, cls, resp.StatusCode, resp.Header, string(b))
					recordStickyFailure(rp, fmt.Sprintf("status_%d", resp.StatusCode))
					continue
				}
				// Remaining-quota sampling: the vendor's own rate-limit
				// headers are the cheapest source of window utilization, and they
				// arrive on success as well as on 429. Detached from the request
				// (the caller may already be gone) and throttled inside the
				// service, so ordinary traffic costs nothing.
				g.sampleAccountQuota(c.Request.Context(), rp, resp.Header)
				if resp.StatusCode >= 400 {
					b, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					cls := classifyUpstreamResponse(resp.StatusCode, b)
					cyberBlocked := cls.IsSafetyBlock()
					lc.UpstreamRawError = truncate(string(b), 8192)
					// Over this account's window but within a later one's: fail over
					// instead of returning the rejection. Not a health sample.
					if w := contextRetryWindow(route.Providers, i, rp, b, upstreamAttempts, g.maxAttempts); w > 0 {
						ctxFloor = w
						lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
						attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "context_window_retry")
						lc.ErrorClass = string(attempts.FinalClass())
						syncAttempts()
						continue
					}
					// Channel-level 4xx (auth/quota/not-found/timeout) likely fail
					// on this provider only — fall over to the next one.
					if cls.Retryable() {
						lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
						attempts.SetResult(attemptIdx, resp.StatusCode, string(b), switchStrategyForStatus(resp.StatusCode))
						lc.ErrorClass = string(attempts.FinalClass())
						syncAttempts()
						recordUpstreamFailure(rp, cls, resp.StatusCode, resp.Header, string(b))
						recordStickyFailure(rp, fmt.Sprintf("status_%d", resp.StatusCode))
						continue
					}
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					// Terminal upstream rejection — no output delivered, return the
					// hold (this path writes its usage row directly, not via logFinal).
					g.releaseReservation(c.Request.Context(), lc.Reservation)
					if cyberBlocked {
						uid := user.ID
						g.incidents.RecordAsync(biz.IncidentInput{
							UserID:    &uid,
							ApiKeyID:  apiKeyID(apiKey),
							Kind:      "cyber_security",
							Severity:  "high",
							Action:    "blocked",
							Model:     anthReq.Model,
							Endpoint:  "messages",
							RequestID: requestID,
							IP:        c.ClientIP(),
							Message:   "upstream cyber-security/moderation block",
							Payload: map[string]any{
								"upstream_status": resp.StatusCode,
							},
						})
						c.JSON(http.StatusForbidden, anthropicError("content_policy_violation", cyberSafetyMessage(), requestID))
					} else if msg, _, ok := actionableUpstreamError(b); ok {
						// Actionable upstream rejection (request body too large,
						// input over the context window, a disabled feature, ...)
						// — relay it so the user knows what to fix. The native
						// passthrough branch used to mask every non-cyber 4xx
						// behind "request rejected by service", unlike the
						// translated branch and the OpenAI gateway, so an
						// oversized-body 400 reached the client with no hint.
						c.JSON(resp.StatusCode, anthropicError("invalid_request_error", msg, requestID))
					} else {
						st, _, msg := upstreamRejectedReply(resp.StatusCode, b)
						c.JSON(st, anthropicError("api_error", msg, requestID))
					}
					_, _ = g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
						UserID: user.ID, ApiKeyID: apiKeyID(apiKey), RequestID: requestID,
						OrganizationID: organizationID(apiKey),
						Endpoint:       entusagelog.EndpointMessages, Model: anthReq.Model, ModelID: &route.Model.ID,
						GroupID: &route.Group.ID, ProviderID: rp.LegacyProviderID(), ProviderAccountID: accountIDRef(rp.Account), UpstreamModel: rp.UpstreamModel,
						Status: entusagelog.StatusUpstreamError, HTTPStatus: resp.StatusCode,
						LatencyMs:  int(time.Since(startedAt).Milliseconds()),
						RetryCount: retryCount, ProviderChain: providerChain,
						ErrorMessage: fmt.Sprintf("upstream %d", resp.StatusCode), StartedAt: startedAt,
						ErrorClass: lc.ErrorClass, UpstreamRequestID: lc.UpstreamRequestID, UpstreamRawError: truncate(string(b), 8192),
						IP: lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
						Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
						LatencyDetail: lc.Latency.Snapshot(),
					})
					return
				}

				// 2xx success — commit phase of the rate-limit protocol. Counting
				// here rather than at peek keeps provider 5xx / connect failures
				// and the retries they trigger free for the caller.
				commitRate()

				if clientStream {
					commitDownstreamSSE(c.Writer)
					observeStreamTTFB(metricOwner(rp), "messages", startedAt)
					var policyHit *biz.PolicyMatch
					policyHook := g.policyStreamHook(func(h *biz.PolicyMatch) {
						policyHit = h
						g.recordPolicyOutputIncident(c, h, &user.ID, apiKeyID(apiKey), requestID, anthReq.Model, "messages")
					})
					pipeErr := PipeAndExtractOpts(c.Writer, resp.Body, acc, PipeOptions{
						TransformPayload: EnsureAnthropicThinkingSignature,
						OnFirstFrame: func() {
							lc.Latency.SinceStart("first_sse_frame_ms")
							// See gateway.go: the first content frame proves the
							// upstream is alive; stall detection moves to the
							// idle timer so long reasoning preludes don't trip
							// the first-token guard.
							firstTextTimeout.Stop()
						},
						OnFirstText: func() {
							firstTextTimeout.Stop()
							d := int(time.Since(startedAt).Milliseconds())
							ttftMs = &d
							lc.Latency.Duration("first_token_delta_ms", time.Duration(d)*time.Millisecond)
						},
						// See gateway.go: the decode window starts when the
						// model starts producing, which for a thinking block is
						// before anything readable arrives.
						OnFirstOutput: func() {
							lc.Latency.SinceStart("first_output_ms")
						},
						OnFrame: func(payload []byte, acc *Accumulator) error {
							if policyHook != nil {
								if err := policyHook(payload, acc); err != nil {
									return err
								}
							}
							return nil
						},
						// Reset the chunk-idle (stall) timer only on real content
						// frames — upstream keepalive comments must NOT keep a
						// stalled stream alive.
						AfterContentFrame: idleTimeout.Reset,
					})
					_ = resp.Body.Close()
					if errors.Is(pipeErr, ErrPolicyBlocked) {
						slug := ""
						if policyHit != nil {
							slug = policyHit.Slug
						}
						attempts.SetResult(attemptIdx, resp.StatusCode, "output_policy_block:"+slug, "output_policy_block")
						attempts.MarkFinal(attemptIdx)
						lc.ErrorClass = string(attempts.FinalClass())
						syncAttempts()
						g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
							route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
							entusagelog.StatusInternalError, resp.StatusCode, "output_policy_block:"+slug, lc)
						return
					}
					// Caller hung up mid-stream: our own socket write failed. Not a
					// provider fault, so it is logged at 499 and kept out of both the
					// cooldown signal and public availability. Mirrors the same guard on
					// the /v1/responses path.
					if errors.Is(pipeErr, errClientWrite) {
						lastErr = pipeErr
						attempts.SetResult(attemptIdx, resp.StatusCode, "client_canceled: "+pipeErr.Error(), "client_canceled")
						attempts.MarkFinal(attemptIdx)
						lc.ErrorClass = string(attempts.FinalClass())
						syncAttempts()
						g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
							route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
							entusagelog.StatusUpstreamError, 499, "client_canceled", lc)
						return
					}
					// Same rule as the gateway: a hang-up is the caller's doing.
					if streamEndedOnClientHangUp(pipeErr, c.Request.Context()) {
						attempts.SetResult(attemptIdx, resp.StatusCode, "client_canceled", "client_canceled")
						attempts.MarkFinal(attemptIdx)
						lc.ErrorClass = string(attempts.FinalClass())
						syncAttempts()
						g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
							route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
							entusagelog.StatusUpstreamError, 499, "client_canceled", lc)
						return
					}
					if pipeErr != nil && (!errors.Is(pipeErr, context.Canceled) || streamIdleTimedOut(attemptCtx) || streamFirstTextTimedOut(attemptCtx) || streamTotalTimedOut(attemptCtx)) {
						lastErr = pipeErr
						reason := "stream_error"
						if errors.Is(pipeErr, errUpstreamStreamIncomplete) {
							reason = "stream_incomplete"
						}
						// Check total BEFORE idle: both surface as a cancelled ctx, so
						// order decides the recorded reason.
						if streamFirstTextTimedOut(attemptCtx) {
							markFirstTextTimeout()
							lastErr = errUpstreamFirstTextTimeout
							reason = "first_text_timeout"
						} else if streamTotalTimedOut(attemptCtx) {
							lastErr = errUpstreamStreamTotalTimeout
							reason = "stream_total_timeout"
						} else if streamIdleTimedOut(attemptCtx) {
							lastErr = errUpstreamStreamIdleTimeout
							reason = "stream_idle_timeout"
						}
						// See gateway.go: neither a request-level deadline nor a
						// single truncation may take the channel out of rotation.
						if reason != "stream_total_timeout" && reason != "stream_incomplete" {
							recordAccountFailure(rp, reason)
						}
						recordStickyFailure(rp, reason)
						attempts.SetResult(attemptIdx, resp.StatusCode, lastErr.Error(), reason)
						lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
						syncAttempts()
						// Neither first-token nor total timeout should fail over (the
						// former re-enters a cancelled ctx; the latter is terminal).
						if !c.Writer.Written() && reason != "first_text_timeout" && reason != "stream_total_timeout" && upstreamAttempts < g.maxAttempts {
							continue
						}
						writeTimeoutJSON := (reason == "first_text_timeout" || reason == "stream_total_timeout") && !c.Writer.Written()
						logHTTPStatus := resp.StatusCode
						if c.Writer.Written() {
							writeStreamAbortTermination(c.Writer, "messages", requestID, acc.AssistantText.String())
						} else if writeTimeoutJSON {
							c.JSON(http.StatusGatewayTimeout, anthropicError("api_error", upstreamUnavailableMessage(), requestID))
							logHTTPStatus = http.StatusGatewayTimeout
						} else {
							// Never leave a non-started stream hanging on an empty 200.
							c.JSON(http.StatusBadGateway, anthropicError("api_error", upstreamUnavailableMessage(), requestID))
							logHTTPStatus = http.StatusBadGateway
						}
						attempts.MarkFinal(attemptIdx)
						syncAttempts()
						g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
							route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
							entusagelog.StatusUpstreamError, logHTTPStatus, lastErr.Error(), lc)
						return
					}
				} else {
					rawResp, readErr := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if readErr != nil {
						// A body that stopped mid-transfer is not a response.
						// Forwarding the prefix hands the client unparseable
						// JSON and still charges for it, so treat it like any
						// other attempt failure: fail over while nothing has
						// been written, otherwise surface a real error.
						if errors.Is(c.Request.Context().Err(), context.Canceled) {
							attempts.SetResult(attemptIdx, resp.StatusCode, "client_canceled", "client_canceled")
							attempts.MarkFinal(attemptIdx)
							lc.ErrorClass = string(attempts.FinalClass())
							syncAttempts()
							g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
								route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
								entusagelog.StatusUpstreamError, 499, "client_canceled", lc)
							return
						}
						// Our own wall-clock cap expiring is a request-level
						// deadline, not a channel fault. Retrying is pointless
						// (streamCtx is dead, so every remaining provider fails
						// at dial) and blaming the channel would cool down the
						// whole chain over one slow request. Same carve-out the
						// streaming path makes for stream_total_timeout.
						totalTimedOut := streamTotalTimedOut(attemptCtx)
						reason := "response_read_error"
						if totalTimedOut {
							reason = "stream_total_timeout"
							lastErr = errUpstreamStreamTotalTimeout
						} else {
							lastErr = fmt.Errorf("read upstream response: %w", readErr)
						}
						msg := lastErr.Error()
						if !totalTimedOut {
							recordAccountFailure(rp, reason)
						}
						recordStickyFailure(rp, reason)
						attempts.SetResult(attemptIdx, resp.StatusCode, msg, reason)
						lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(msg, 8192)
						syncAttempts()
						if !totalTimedOut && upstreamAttempts < g.maxAttempts {
							continue
						}
						attempts.MarkFinal(attemptIdx)
						syncAttempts()
						readErrStatus := http.StatusBadGateway
						if totalTimedOut {
							readErrStatus = http.StatusGatewayTimeout
						}
						c.JSON(readErrStatus, anthropicError("api_error", upstreamUnavailableMessage(), requestID))
						g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
							route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
							entusagelog.StatusUpstreamError, readErrStatus, msg, lc)
						return
					}
					acc.Inspect(rawResp)
					stageStart = time.Now()
					imageOutputAllowed := g.imageOutputAllowed(c)
					lc.Latency.Duration("image_output_policy_ms", time.Since(stageStart))
					if !imageOutputAllowed {
						var obj map[string]any
						if err := json.Unmarshal(rawResp, &obj); err == nil {
							if biz.RewriteImageOutputsWithNotice(obj) > 0 {
								if rewritten, err := json.Marshal(obj); err == nil {
									rawResp = rewritten
								}
								acc.OutputImages = nil
								g.recordImageBlockedIncident(c, &user.ID, apiKeyID(apiKey), requestID, anthReq.Model, "messages")
							}
						}
					}
					if g.policy != nil {
						hasText := g.policy.HasAnyScope(biz.ScopeInputOutput, biz.ScopeInputOutputTools)
						hasTools := g.policy.HasAnyScope(biz.ScopeInputOutputTools)
						if hasText || hasTools {
							sc := g.policy.NewStreamScanner()
							filter := biz.ScopeInputOutput
							if hasTools && !hasText {
								filter = biz.ScopeInputOutputTools
							}
							if hit := sc.Feed(rawResp, filter); hit != nil && hit.Action == "block" {
								g.recordPolicyOutputIncident(c, hit, &user.ID, apiKeyID(apiKey), requestID, anthReq.Model, "messages")
								c.JSON(http.StatusUnprocessableEntity, anthropicError("content_policy_violation", outputPolicyMessage(hit.Slug), requestID))
								attempts.SetResult(attemptIdx, http.StatusUnprocessableEntity, "output_policy_block:"+hit.Slug, "output_policy_block")
								attempts.MarkFinal(attemptIdx)
								lc.ErrorClass = string(attempts.FinalClass())
								syncAttempts()
								g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
									route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
									entusagelog.StatusInternalError, http.StatusUnprocessableEntity,
									"output_policy_block:"+hit.Slug, lc)
								return
							}
						}
					}
					ct := resp.Header.Get("Content-Type")
					if patched := EnsureAnthropicThinkingSignature(rawResp); patched != nil {
						rawResp = patched
					}
					if ct == "" {
						ct = "application/json"
					}
					c.Data(resp.StatusCode, ct, rawResp)
				}

				attempts.SetResult(attemptIdx, resp.StatusCode, "", "")
				attempts.MarkFinal(attemptIdx)
				lc.ErrorClass = string(attempts.FinalClass())
				syncAttempts()
				g.rememberAccountOutcome(c.Request.Context(), user.ID, route.Group.ID, convID, rp)
				g.rememberRouteGroup(c.Request.Context(), user.ID, route.Group.ID, anthReq.Model, convID, failover.switches > 0)
				g.chargeAndLog(c.Request.Context(), user, apiKey, requestID,
					entusagelog.EndpointMessages, anthReq.Model, route, rp, usedUpstream,
					acc, ttftMs, startedAt, providerChain, retryCount, resp.StatusCode, lc, planState)
				return
			}

			// Only the translation bridge renders through an emitter, and it
			// gets a fresh one per attempt for the same reason acc is cleared:
			// a buffered reply is assembled in here and would otherwise splice
			// the dead provider's half-written text and tool arguments onto
			// the next one's answer.
			emitter := newAnthropicEmitter(c.Writer, anthReq.Model)

			// Per-provider upstream model rewrite.
			attemptStage := time.Now()
			bodyWithModel, err := rewriteModel(chatBody, rp.UpstreamModel)
			if err != nil {
				attempts.SetLatency(attemptIdx, "request_prepare_ms", time.Since(attemptStage))
				lastErr = err
				attempts.SetResult(attemptIdx, 0, err.Error(), "request_prepare_error")
				syncAttempts()
				continue
			}
			attempts.SetLatency(attemptIdx, "request_prepare_ms", time.Since(attemptStage))

			attemptStage = time.Now()
			if !g.acquireAccountSlot(c.Request.Context(), rp) {
				attempts.SetLatency(attemptIdx, "provider_build_ms", time.Since(attemptStage))
				attempts.SetResult(attemptIdx, 0, "account at concurrency cap", "account_busy")
				syncAttempts()
				capacityDrops++
				continue
			}
			defer g.releaseAccountSlot(c.Request.Context(), rp)
			lc.AccountID = accountIDRef(rp.Account)
			prov, err := g.buildProviderForAttempt(c.Request.Context(), rp, biz.RequestProtocolOpenAI)
			attempts.SetLatency(attemptIdx, "provider_build_ms", time.Since(attemptStage))
			if err != nil {
				lastErr = err
				attempts.SetResult(attemptIdx, 0, err.Error(), "build_provider_error")
				syncAttempts()
				recordStickyFailure(rp, "build_provider_error")
				continue
			}

			// streamCtx so the wall-clock cap reaches the upstream call even
			// when the client is not streaming.
			//
			// The stall detector stays on the streaming path only. It arms
			// before Forward and is reset by content frames, so on a buffered
			// reply it becomes a first-frame deadline — and at its 60s default
			// that is tighter than the 90s first-token budget this repo picked
			// against a measured p95, which would fail relays that assemble
			// the whole answer before replaying it. The cap is the deadline
			// for buffered replies.
			attemptCtx := streamCtx
			var idleTimeout *streamIdleTimeout
			if clientStream {
				attemptCtx, idleTimeout = withStreamIdleTimeout(streamCtx, g.streamIdleTimeout(c.Request.Context()))
				defer idleTimeout.Stop()
			}
			attemptStage = time.Now()
			resp, err := prov.Forward(attemptCtx, "/v1/chat/completions", bodyWithModel, true, c.Request.Header)
			attempts.SetLatency(attemptIdx, "upstream_header_ms", time.Since(attemptStage))
			metrics.LLMAttemptDuration.WithLabelValues(metricOwner(rp), "chat_completions").Observe(time.Since(attemptStage).Seconds())
			upstreamAttempts++
			if err != nil {
				lastErr = err
				lastStatus = 0
				lc.UpstreamRequestID = ""
				if streamFirstTextTimedOut(attemptCtx) {
					markFirstTextTimeout()
					lastErr = errUpstreamFirstTextTimeout
					attempts.SetResult(attemptIdx, 0, lastErr.Error(), "first_text_timeout")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
					syncAttempts()
					metrics.LLMStreamFirstTextTimeout.WithLabelValues(metricOwner(rp)).Inc()
					recordAccountFailure(rp, "first_text_timeout")
					recordStickyFailure(rp, "first_text_timeout")
					break attemptLoop
				}
				if streamIdleTimedOut(attemptCtx) {
					lastErr = errUpstreamStreamIdleTimeout
					attempts.SetResult(attemptIdx, 0, lastErr.Error(), "stream_idle_timeout")
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
					syncAttempts()
					metrics.LLMStreamIdleTimeout.WithLabelValues(metricOwner(rp)).Inc()
					recordAccountFailure(rp, "stream_idle_timeout")
					recordStickyFailure(rp, "stream_idle_timeout")
					continue
				}
				if d, ok := relayRestartDelay(rp, err, relayRetries); ok && c.Request.Context().Err() == nil {
					// Wait out a local relay restart (relay_retry.go).
					relayRetries++
					upstreamAttempts--
					attempts.SetResult(attemptIdx, 0, err.Error(), "relay_restart_retry")
					lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(err.Error(), 8192)
					syncAttempts()
					sleepCtx(c.Request.Context(), d)
					i--
					continue
				}
				attempts.SetResult(attemptIdx, 0, err.Error(), "forward_error")
				lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(err.Error(), 8192)
				syncAttempts()
				recordAccountFailure(rp, "forward_error")
				recordStickyFailure(rp, "forward_error")
				continue
			}
			lastStatus = resp.StatusCode
			lc.UpstreamRequestID = captureUpstreamRequestID(resp.Header)

			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				cls := classifyUpstreamResponse(resp.StatusCode, b)
				lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
				lc.UpstreamRawError = truncate(string(b), 8192)
				if cls == classPoolExhausted {
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "pool_exhausted")
					attempts.MarkFinal(attemptIdx)
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					setRetryAfter(c, resp.Header, 30*time.Second)
					c.JSON(http.StatusServiceUnavailable, anthropicError("api_error", upstreamUnavailableMessage(), requestID))
					g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model, route, rp, usedUpstream,
						acc, ttftMs, startedAt, providerChain, retryCount, entusagelog.StatusUpstreamError, http.StatusServiceUnavailable, lastErr.Error(), lc)
					return
				}
				attempts.SetResult(attemptIdx, resp.StatusCode, string(b), switchStrategyForStatus(resp.StatusCode))
				lc.ErrorClass = string(attempts.FinalClass())
				syncAttempts()
				recordUpstreamFailure(rp, cls, resp.StatusCode, resp.Header, string(b))
				recordStickyFailure(rp, fmt.Sprintf("status_%d", resp.StatusCode))
				continue
			}
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				cls := classifyUpstreamResponse(resp.StatusCode, b)
				cyberBlocked := cls.IsSafetyBlock()
				lc.UpstreamRawError = truncate(string(b), 8192)
				// Over this account's window but within a later one's: fail over
				// instead of returning the rejection. Not a health sample.
				if w := contextRetryWindow(route.Providers, i, rp, b, upstreamAttempts, g.maxAttempts); w > 0 {
					ctxFloor = w
					lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "context_window_retry")
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					continue
				}
				// Channel-level 4xx (auth/quota/not-found/timeout) likely fail
				// on this provider only — fall over to the next one.
				if cls.Retryable() {
					lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(b), 256))
					attempts.SetResult(attemptIdx, resp.StatusCode, string(b), switchStrategyForStatus(resp.StatusCode))
					lc.ErrorClass = string(attempts.FinalClass())
					syncAttempts()
					recordUpstreamFailure(rp, cls, resp.StatusCode, resp.Header, string(b))
					recordStickyFailure(rp, fmt.Sprintf("status_%d", resp.StatusCode))
					continue
				}
				attempts.SetResult(attemptIdx, resp.StatusCode, string(b), "")
				attempts.MarkFinal(attemptIdx)
				lc.ErrorClass = string(attempts.FinalClass())
				syncAttempts()
				// Terminal upstream rejection on the translated branch — return
				// the hold (usage row is written directly below, not via logFinal).
				g.releaseReservation(c.Request.Context(), lc.Reservation)
				if cyberBlocked {
					uid := user.ID
					g.incidents.RecordAsync(biz.IncidentInput{
						UserID:    &uid,
						ApiKeyID:  apiKeyID(apiKey),
						Kind:      "cyber_security",
						Severity:  "high",
						Action:    "blocked",
						Model:     anthReq.Model,
						Endpoint:  "messages",
						RequestID: requestID,
						IP:        c.ClientIP(),
						Message:   "upstream cyber-security/moderation block",
						Payload: map[string]any{
							"upstream_status": resp.StatusCode,
						},
					})
				}
				if cyberBlocked {
					c.JSON(http.StatusForbidden, anthropicError("content_policy_violation", cyberSafetyMessage(), requestID))
				} else if msg, _, ok := actionableUpstreamError(b); ok {
					// Actionable upstream rejection (wrong client version, input
					// over the context window, a disabled feature, ...) — echo the
					// upstream's own message so the user knows what to fix, instead
					// of masking it behind the generic rejection text.
					c.JSON(resp.StatusCode, anthropicError("invalid_request_error", msg, requestID))
				} else {
					st, _, msg := upstreamRejectedReply(resp.StatusCode, b)
					c.JSON(st, anthropicError("api_error", msg, requestID))
				}
				_, _ = g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
					UserID: user.ID, ApiKeyID: apiKeyID(apiKey), RequestID: requestID,
					OrganizationID: organizationID(apiKey),
					Endpoint:       entusagelog.EndpointMessages, Model: anthReq.Model, ModelID: &route.Model.ID,
					GroupID: &route.Group.ID, ProviderID: rp.LegacyProviderID(), ProviderAccountID: accountIDRef(rp.Account), UpstreamModel: rp.UpstreamModel,
					Status: entusagelog.StatusUpstreamError, HTTPStatus: resp.StatusCode,
					LatencyMs:  int(time.Since(startedAt).Milliseconds()),
					RetryCount: retryCount, ProviderChain: providerChain,
					ErrorMessage: fmt.Sprintf("upstream %d", resp.StatusCode), StartedAt: startedAt,
					ErrorClass: lc.ErrorClass, UpstreamRequestID: lc.UpstreamRequestID, UpstreamRawError: truncate(string(b), 8192),
					IP: lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
					Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
					LatencyDetail: lc.Latency.Snapshot(),
				})
				return
			}

			// 2xx: translate chat-completions SSE → anthropic SSE (or buffer for non-stream).
			if clientStream {
				emitter.writeSSEHeaders()
				observeStreamTTFB(metricOwner(rp), "messages", startedAt)
			}
			var policyHit *biz.PolicyMatch
			policyHook := g.policyStreamHook(func(h *biz.PolicyMatch) {
				policyHit = h
				if clientStream {
					// Anthropic SDK retries on `event: error`. Close the
					// stream the same way a normal completion does so the
					// client moves on cleanly. end_turn is the closest
					// non-error stop_reason to "we decided to stop".
					emitter.finish("end_turn")
				}
				g.recordPolicyOutputIncident(c, h, &user.ID, apiKeyID(apiKey), requestID, anthReq.Model, "messages")
			})
			// Image-output gate: on the Anthropic bridge we have to
			// rewrite image_url blocks INSIDE the upstream chat-chunks
			// before pipeChatAsAnthropic decodes them — chatChunk only
			// knows about content:string, so an array-form delta with
			// image_url would otherwise silently drop. After the rewrite
			// we also flatten content back to a single string so the
			// existing chatChunk decoder picks up the notice as a normal
			// text_delta.
			stageStart = time.Now()
			imageOutputAllowed := g.imageOutputAllowed(c)
			lc.Latency.Duration("image_output_policy_ms", time.Since(stageStart))
			imageBlockedThisRequest := false
			anthropicTransform := func(payload []byte) []byte {
				if imageOutputAllowed {
					return nil
				}
				var obj map[string]any
				if err := json.Unmarshal(payload, &obj); err != nil {
					return nil
				}
				if biz.RewriteImageOutputsWithNotice(obj) == 0 {
					return nil
				}
				biz.FlattenChatContentArrayToString(obj)
				imageBlockedThisRequest = true
				out, err := json.Marshal(obj)
				if err != nil {
					return nil
				}
				return out
			}
			observeChatFrame := func(payload []byte, acc *Accumulator) error {
				if policyHook != nil {
					if err := policyHook(payload, acc); err != nil {
						return err
					}
				}
				return nil
			}
			pipeErr := pipeChatAsAnthropic(resp.Body, emitter, acc, clientStream, func() {
				lc.Latency.SinceStart("first_sse_frame_ms")
				if !clientStream {
					observeStreamTTFB(metricOwner(rp), "messages", startedAt)
				}
			}, func() {
				// This branch always streams upstream, so the first token has a
				// real arrival time even when the client asked for one JSON
				// body — record it either way rather than discarding a
				// measurement we already took.
				firstTextTimeout.Stop()
				d := int(time.Since(startedAt).Milliseconds())
				ttftMs = &d
				lc.Latency.Duration("first_token_delta_ms", time.Duration(d)*time.Millisecond)
			}, observeChatFrame, anthropicTransform, idleTimeout.Reset)
			if !imageOutputAllowed && imageBlockedThisRequest {
				acc.OutputImages = nil
				g.recordImageBlockedIncident(c, &user.ID, apiKeyID(apiKey), requestID, anthReq.Model, "messages")
			}
			_ = resp.Body.Close()
			if errors.Is(pipeErr, ErrPolicyBlocked) {
				slug := ""
				if policyHit != nil {
					slug = policyHit.Slug
				}
				if !clientStream {
					// Non-stream: switch the response to a 422 JSON.
					c.JSON(http.StatusUnprocessableEntity, anthropicError("content_policy_violation", outputPolicyMessage(slug), requestID))
				}
				attempts.SetResult(attemptIdx, resp.StatusCode, "output_policy_block:"+slug, "output_policy_block")
				attempts.MarkFinal(attemptIdx)
				lc.ErrorClass = string(attempts.FinalClass())
				syncAttempts()
				g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
					route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
					entusagelog.StatusInternalError, resp.StatusCode, "output_policy_block:"+slug, lc)
				return
			}
			// Same rule as the gateway: a hang-up is the caller's doing.
			if streamEndedOnClientHangUp(pipeErr, c.Request.Context()) {
				attempts.SetResult(attemptIdx, resp.StatusCode, "client_canceled", "client_canceled")
				attempts.MarkFinal(attemptIdx)
				lc.ErrorClass = string(attempts.FinalClass())
				syncAttempts()
				g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
					route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
					entusagelog.StatusUpstreamError, 499, "client_canceled", lc)
				return
			}
			if pipeErr != nil && (!errors.Is(pipeErr, context.Canceled) || streamIdleTimedOut(attemptCtx) || streamFirstTextTimedOut(attemptCtx) || streamTotalTimedOut(attemptCtx)) {
				lastErr = pipeErr
				reason := "stream_error"
				if errors.Is(pipeErr, errUpstreamStreamIncomplete) {
					reason = "stream_incomplete"
				}
				// Check total BEFORE idle: both surface as a cancelled ctx, so
				// order decides the recorded reason.
				if streamFirstTextTimedOut(attemptCtx) {
					markFirstTextTimeout()
					lastErr = errUpstreamFirstTextTimeout
					reason = "first_text_timeout"
				} else if streamTotalTimedOut(attemptCtx) {
					lastErr = errUpstreamStreamTotalTimeout
					reason = "stream_total_timeout"
				} else if streamIdleTimedOut(attemptCtx) {
					lastErr = errUpstreamStreamIdleTimeout
					reason = "stream_idle_timeout"
				}
				// See gateway.go: neither a request-level deadline nor a single
				// truncation may take the channel out of rotation.
				if reason != "stream_total_timeout" && reason != "stream_incomplete" {
					recordAccountFailure(rp, reason)
				}
				recordStickyFailure(rp, reason)
				attempts.SetResult(attemptIdx, resp.StatusCode, lastErr.Error(), reason)
				lc.ErrorClass, lc.UpstreamRawError = string(attempts.FinalClass()), truncate(lastErr.Error(), 8192)
				syncAttempts()
				// Neither first-token nor total timeout should fail over (the
				// former re-enters a cancelled ctx; the latter is terminal).
				if !c.Writer.Written() && reason != "first_text_timeout" && reason != "stream_total_timeout" && upstreamAttempts < g.maxAttempts {
					continue
				}
				writeTimeoutJSON := (reason == "first_text_timeout" || reason == "stream_total_timeout") && !c.Writer.Written()
				logHTTPStatus := resp.StatusCode
				if writeTimeoutJSON {
					c.JSON(http.StatusGatewayTimeout, anthropicError("api_error", upstreamUnavailableMessage(), requestID))
					logHTTPStatus = http.StatusGatewayTimeout
				} else if c.Writer.Written() {
					writeStreamAbortTermination(c.Writer, "messages", requestID, acc.AssistantText.String())
				} else {
					// Never leave a non-started stream hanging on an empty 200.
					c.JSON(http.StatusBadGateway, anthropicError("api_error", upstreamUnavailableMessage(), requestID))
					logHTTPStatus = http.StatusBadGateway
				}
				attempts.MarkFinal(attemptIdx)
				syncAttempts()
				g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
					route, rp, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
					entusagelog.StatusUpstreamError, logHTTPStatus, lastErr.Error(), lc)
				return
			}

			// Non-stream: emit the buffered message as a single JSON.
			if !clientStream {
				emitter.flushNonStream(acc)
			}

			attempts.SetResult(attemptIdx, resp.StatusCode, "", "")
			attempts.MarkFinal(attemptIdx)
			// Recompute from the final attempt, the way the native branch
			// does. A failed earlier attempt leaves provider_failure here, and
			// leaving it would file this succeeded request under the channel
			// fault counters that drive provider_fault_rate.
			lc.ErrorClass = string(attempts.FinalClass())
			lc.UpstreamRawError = ""
			syncAttempts()
			g.rememberAccountOutcome(c.Request.Context(), user.ID, route.Group.ID, convID, rp)
			g.rememberRouteGroup(c.Request.Context(), user.ID, route.Group.ID, anthReq.Model, convID, failover.switches > 0)
			g.chargeAndLog(c.Request.Context(), user, apiKey, requestID,
				entusagelog.EndpointMessages, anthReq.Model, route, rp, usedUpstream,
				acc, ttftMs, startedAt, providerChain, retryCount, resp.StatusCode, lc, planState)
			return
		}

		// Every provider of the current group failed with nothing written to
		// the client (terminal failures returned or broke out of attemptLoop
		// above). Cross-group retry: same billing_source only, rate within
		// the key's auto_route_max_rate, hold re-priced inside next().
		nextRoute, nextState := failover.next(c.Request.Context(), route, c.Writer.Written(), &lc)
		if nextRoute == nil {
			break
		}
		route = nextRoute
		planState = nextState
		// The failover route resolves its own accounts through the same router,
		// so there is nothing to expand here.
		lc.Latency.Value("cross_group_switches", failover.switches)
		// Fresh group, fresh attempt budget — see the per-group note above.
		upstreamAttempts = 0
	}

	if c.Writer.Written() {
		// Defensive: streaming failure paths normally return via logFinal
		// before reaching here — make sure a hold never leaks if one slips
		// through with bytes already on the wire.
		g.releaseReservation(c.Request.Context(), lc.Reservation)
		return
	}
	msg := "upstream unavailable"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	// Nothing ever reached an upstream and the reason was capacity: answer as
	// backpressure so clients back off instead of retrying a "broken" upstream.
	capacityFailure := lastErr == nil && capacityDrops > 0
	if capacityFailure {
		lastErr = errNoUpstreamCapacity
		msg = lastErr.Error()
	}
	// Derive a client-facing reason (idle/total timeout, incomplete stream,
	// unreachable upstream, or an actionable upstream body) instead of a
	// blanket "service temporarily unavailable".
	_, clientMsg := failureReason(lastErr, lastStatus, lc.UpstreamRawError)
	// 502 is reserved for transport failures (lastStatus==0). An upstream
	// 5xx becomes 503 + Retry-After; a terminal 429 keeps 429 semantics so
	// the Anthropic SDK applies its own backoff.
	status := http.StatusBadGateway
	errKind := "api_error"
	if isTimeoutErr(lastErr) {
		status = http.StatusGatewayTimeout
	} else if capacityFailure {
		// Backpressure (every account at its cap, or no eligible account), not
		// an unreachable upstream: 503 + Retry-After like the pool-exhausted
		// path, so SDKs back off instead of retrying immediately.
		status = http.StatusServiceUnavailable
		setRetryAfter(c, nil, 5*time.Second)
	} else if lastStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
		errKind = "rate_limit_error"
		setRetryAfter(c, nil, cooldownRateMin)
	} else if lastStatus >= 500 {
		status = http.StatusServiceUnavailable
		setRetryAfter(c, nil, 5*time.Second)
	} else if lastStatus >= 400 && lastStatus < 500 {
		status = lastStatus
	}
	c.JSON(status, anthropicError(errKind, clientMsg, requestID))
	logStatus := lastStatus
	if status == http.StatusGatewayTimeout {
		logStatus = status
	}
	// acc describes the last attempt that started and nothing before it, the
	// same rule the per-attempt failure paths above follow — each attempt
	// clears it on entry. An earlier provider's token count is therefore not
	// on this row; provider_chain is where the per-attempt story lives.
	g.logFinal(c, user, apiKey, requestID, entusagelog.EndpointMessages, anthReq.Model,
		route, usedRP, usedUpstream, acc, ttftMs, startedAt, providerChain, retryCount,
		entusagelog.StatusUpstreamError, logStatus, msg, lc)
}

// Types (anthropicRequest / anthropicMessage), request-side
// translation (anthropicToChatBody / flatten* / rewriteModel) live in
// anthropic_translate.go. Response-side translation (emitter,
// pipeChatAsAnthropic, mapFinishReason, error envelope, message-id
// generator) lives in anthropic_emitter.go.
