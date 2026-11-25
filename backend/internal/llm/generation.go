package llm

// generation.go holds the gateway HTTP handlers for the non-chat generation
// endpoints (embeddings / image / audio / rerank; video is deferred to the
// async task store). See endpoints.go for the taxonomy and
// provider_generation.go for the upstream forwarding.
//
// The JSON synchronous endpoints (embeddings, image_generation, audio_speech,
// rerank) run the full pipeline: input policy scan -> model routing ->
// access + billing precheck -> concurrency gate -> provider failover forward
// -> response passthrough -> metered charge + usage log. It mirrors the chat
// path (gateway.go handle()) but without streaming, sticky sessions or agent
// enhancement, none of which apply to a single request/response generation
// call. It reuses the same biz services, so no existing behavior changes.
//
// Multipart endpoints (image_edit / image_variation / audio_transcription /
// audio_translation) run the same pipeline with multipart body handling; the
// always-async video endpoint is served by video_handlers.go when the task
// store is wired and returns a stable 501 otherwise.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/metrics"
	"github.com/jucode/jucode/internal/middleware"
)

// upstreamStatusErr wraps a retryable upstream status + body snippet.
func upstreamStatusErr(status int, body []byte) error {
	return fmt.Errorf("upstream %d: %s", status, truncate(string(body), 256))
}

// errAccountsBusy is the answer when every capable account was at its
// concurrency cap.
var errAccountsBusy = errors.New("every account for this model is at its concurrency limit; retry shortly")

// reportGenerationFailure feeds a failed generation attempt into the account
// health state machine, as the chat path does, so it picks cooldown or
// disable for this account alone.
func (g *Gateway) reportGenerationFailure(ctx context.Context, rp biz.RouteAccount, model, class string, status int, header http.Header, body string) {
	if rp.Account == nil || g.accounts == nil {
		return
	}
	reason := "forward_error"
	if status > 0 {
		reason = fmt.Sprintf("status_%d", status)
	}
	outcome := g.accounts.ReportAccountFailure(ctx, rp.Account, biz.AccountFailure{
		Class:   class,
		Status:  status,
		Reason:  reason,
		Headers: header,
		Body:    body,
		Model:   model,
	})
	if outcome.Action != biz.ActionNone {
		g.accounts.RecordOutcome(ctx, rp.Account.ID, false)
	}
}

// recordGenerationSuccess keeps the account's health and last-used time
// current, which is what weighted rotation and recovery read.
func (g *Gateway) recordGenerationSuccess(ctx context.Context, rp biz.RouteAccount) {
	if rp.Account == nil || g.accounts == nil {
		return
	}
	g.accounts.TouchLastUsed(ctx, rp.Account.ID)
	g.accounts.RecordOutcome(ctx, rp.Account.ID, true)
	g.accounts.RecordAccountSuccess(ctx, rp.Account)
}

// HandleEmbeddings implements POST /v1/embeddings.
func (g *Gateway) HandleEmbeddings(c *gin.Context) {
	g.handleGeneration(c, EndpointEmbeddings)
}

// HandleImageGenerations implements POST /v1/images/generations.
func (g *Gateway) HandleImageGenerations(c *gin.Context) {
	g.handleGeneration(c, EndpointImageGeneration)
}

// HandleImageEdits implements POST /v1/images/edits (multipart).
func (g *Gateway) HandleImageEdits(c *gin.Context) {
	g.handleGeneration(c, EndpointImageEdit)
}

// HandleImageVariations implements POST /v1/images/variations (multipart).
func (g *Gateway) HandleImageVariations(c *gin.Context) {
	g.handleGeneration(c, EndpointImageVariation)
}

// HandleAudioSpeech implements POST /v1/audio/speech (TTS, binary out).
func (g *Gateway) HandleAudioSpeech(c *gin.Context) {
	g.handleGeneration(c, EndpointAudioSpeech)
}

// HandleAudioTranscriptions implements POST /v1/audio/transcriptions
// (STT/ASR, multipart in).
func (g *Gateway) HandleAudioTranscriptions(c *gin.Context) {
	g.handleGeneration(c, EndpointAudioTranscription)
}

// HandleAudioTranslations implements POST /v1/audio/translations
// (speech -> English text, multipart in).
func (g *Gateway) HandleAudioTranslations(c *gin.Context) {
	g.handleGeneration(c, EndpointAudioTranslation)
}

// HandleRerank implements POST /v1/rerank.
func (g *Gateway) HandleRerank(c *gin.Context) {
	g.handleGeneration(c, EndpointRerank)
}

// HandleVideoGenerations implements POST /v1/videos (async job create).
// When the task store is wired and the routed provider has a video adaptor,
// it submits the upstream job and returns a job object; otherwise it falls
// back to the stable 501 from handleGeneration.
func (g *Gateway) HandleVideoGenerations(c *gin.Context) {
	if g.tasks == nil {
		g.handleGeneration(c, EndpointVideoGeneration)
		return
	}
	g.handleVideoCreate(c)
}

// handleGeneration dispatches by endpoint shape. JSON synchronous endpoints
// run the full pipeline; multipart and always-async endpoints are 501 until
// their passes land.
func (g *Gateway) handleGeneration(c *gin.Context, kind EndpointKind) {
	requestID := gatewayRequestID(c)
	writeErr := func(status int, errKind, code, message string) {
		c.JSON(status, openAIError(errKind, code, message, requestID))
	}

	if _, ok := middleware.UserFrom(c); !ok {
		writeErr(http.StatusUnauthorized, "authentication_error", "missing_user", "no user")
		return
	}
	spec, ok := SpecFor(kind)
	if !ok {
		writeErr(http.StatusInternalServerError, "api_error", "unknown_endpoint", "unknown generation endpoint")
		return
	}

	switch {
	case spec.AlwaysAsync:
		// Video: served by the async task store when wired (handled by
		// HandleVideoGenerations before reaching here). If we get here the
		// task store is unset — return a stable 501.
		writeErr(http.StatusNotImplemented, "api_error", "async_not_implemented",
			"the "+string(kind)+" endpoint requires the async task store, which is not yet available")
		return
	case spec.Request == EncodingMultipart:
		g.handleGenerationMultipart(c, spec, requestID, writeErr)
		return
	}

	g.handleGenerationJSON(c, spec, requestID, writeErr)
}

// handleGenerationJSON runs the synchronous JSON generation pipeline.
func (g *Gateway) handleGenerationJSON(c *gin.Context, spec EndpointSpec, requestID string, writeErr func(int, string, string, string)) {
	startedAt := time.Now()
	kind := spec.Kind
	ep := entusagelog.Endpoint(string(kind))

	user, _ := middleware.UserFrom(c)
	apiKey, _ := middleware.ApiKeyFrom(c)

	// --- read + parse body ---
	var raw []byte
	var err error
	if g.maxBodyBytes > 0 {
		raw, err = io.ReadAll(io.LimitReader(c.Request.Body, g.maxBodyBytes+1))
		if err == nil && int64(len(raw)) > g.maxBodyBytes {
			writeErr(http.StatusRequestEntityTooLarge, "invalid_request_error", "request_body_too_large", "request body too large")
			return
		}
	} else {
		raw, err = io.ReadAll(c.Request.Body)
	}
	if err != nil {
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
	if t := g.generationTimeout(); t > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, t)
		defer cancel()
	}

	lc := logContext{
		IP:         c.ClientIP(),
		UserAgent:  c.Request.UserAgent(),
		ApiKeyName: keyName(apiKey),
	}

	// --- input policy scan (same dictionary as chat) ---
	if g.policy != nil {
		scan := g.policy.Scan(biz.RedactImagesForScan(raw))
		if block, topSev := scan.BlockDecision(); block {
			cats := make([]string, 0, len(scan.Hits))
			for _, h := range scan.Hits {
				cats = append(cats, h.Slug)
			}
			uid := user.ID
			g.incidents.RecordAsync(biz.IncidentInput{
				UserID: &uid, ApiKeyID: apiKeyID(apiKey), Kind: "policy_keyword",
				Severity: topSev, Action: "blocked", Model: modelName, Endpoint: string(ep),
				RequestID: requestID, IP: lc.IP,
				Message: "blocked by input moderation policy",
			})
			resp := openAIError("content_policy_violation", "input_policy_block", "blocked by input moderation policy", requestID)
			resp["error"].(map[string]any)["categories"] = cats
			c.JSON(http.StatusUnprocessableEntity, resp)
			return
		}
	}

	// --- route -> access -> billing precheck -> concurrency ---
	route, err := g.models.Resolve(reqCtx, modelName, user, apiKey, biz.RequestProtocolOpenAI)
	if err == nil && route != nil {
		// The router already resolved accounts for this group; generation shares
		// the same candidate list as the chat paths.
	}
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
		outcome, failureClass, status := billingRouteAvailability(err)
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, outcome, failureClass, status, startedAt)
		g.writeBillingError(c, writeErr, err)
		return
	}
	release, err := g.concur.AcquireWithPlanState(reqCtx, user, route.Group, planState)
	if err != nil {
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, "concurrency_limit", http.StatusTooManyRequests, startedAt)
		writeErr(http.StatusTooManyRequests, "rate_limit_error", "concurrency_limit", err.Error())
		return
	}
	defer release()

	// --- provider failover forward ---
	var (
		lastErr      error
		lastStatus   int
		capable      int // providers that actually advertise this endpoint
		attempted    int
		providerList = route.Providers
		usedAccount  biz.RouteAccount
		usedUpstream string
	)
	// Same per-attempt record as the chat path: provider_chain feeds the
	// admin drill-down and, through WriteUsageLog, the provider health
	// buckets. Without it a failing generation channel was invisible until
	// an operator read the raw log lines.
	attempts := &usageAttemptRecorder{endpoint: string(ep), group: route.Group.ID.String()}
	for _, rp := range providerList {
		if attempted >= g.maxAttempts {
			break
		}
		lc.AccountID = accountIDRef(rp.Account)
		prov, err := g.buildProviderForAttempt(reqCtx, rp, biz.RequestProtocolOpenAI)
		if err != nil {
			lastErr = err
			continue
		}
		gf, ok := asGenerationForwarder(prov)
		if !ok || !gf.SupportsEndpoint(kind) {
			// This provider is chat-only or doesn't serve this endpoint;
			// skip without consuming the retry budget.
			continue
		}
		capable++
		attemptIdx := attempts.Start(rp)
		usedAccount, usedUpstream = rp, rp.UpstreamModel
		encoded, err := prepareForwardBody(body, rp.UpstreamModel, false)
		if err != nil {
			lastErr = err
			attempts.SetResult(attemptIdx, 0, err.Error(), "request_prepare_error")
			continue
		}
		// The account is at its concurrency cap: try a sibling account.
		if !g.acquireAccountSlot(reqCtx, rp) {
			lastErr, lastStatus = errAccountsBusy, http.StatusTooManyRequests
			attempts.SetResult(attemptIdx, 0, "account at concurrency cap", "account_busy")
			continue
		}
		attempted++
		attemptStart := time.Now()
		genResp, err := gf.ForwardGeneration(reqCtx, &GenerationRequest{
			Kind: kind, Model: rp.UpstreamModel, JSONBody: encoded, ClientHeaders: c.Request.Header,
		})
		// The response is fully buffered, so the slot is free once the call returns.
		g.releaseAccountSlot(reqCtx, rp)
		metrics.LLMAttemptDuration.WithLabelValues(metricOwner(rp), string(ep)).Observe(time.Since(attemptStart).Seconds())
		if err != nil {
			// Not-implemented/unsupported are provider-capability gaps, not
			// upstream failures — try the next candidate.
			if errors.Is(err, ErrEndpointNotImplemented) || errors.Is(err, ErrEndpointUnsupported) {
				capable--
				attempts.SetResult(attemptIdx, 0, err.Error(), "endpoint_unsupported")
				continue
			}
			lastErr = err
			lastStatus = 0
			attempts.SetResult(attemptIdx, 0, err.Error(), "forward_error")
			g.reportGenerationFailure(reqCtx, rp, modelName, accountFailureClass("forward_error"), 0, nil, err.Error())
			continue
		}
		// 5xx, 429, and channel-level 4xx (401/402/403/404/408) fail over.
		// A 403 such as "image generation is not enabled for this group" is
		// the account, not the request — the next account in the group may
		// serve it. 400 stays put: the same body fails everywhere.
		if generationShouldFailover(genResp.StatusCode) {
			lastErr = upstreamStatusErr(genResp.StatusCode, genResp.Body)
			lastStatus = genResp.StatusCode
			lc.UpstreamRawError = truncate(string(genResp.Body), 8192)
			attempts.SetResult(attemptIdx, genResp.StatusCode, string(genResp.Body), switchStrategyForStatus(genResp.StatusCode))
			// Channel-level failure: the account state machine cools a 429
			// until its window resets and disables a rejected credential, so
			// the next request skips this account instead of re-trying it.
			g.reportGenerationFailure(reqCtx, rp, modelName,
				string(classifyUpstreamResponse(genResp.StatusCode, genResp.Body)),
				genResp.StatusCode, genResp.Header, string(genResp.Body))
			continue
		}
		// Non-retryable client error: forward it to the caller and log.
		if genResp.StatusCode >= 400 {
			ct := genResp.ContentType
			if ct == "" {
				ct = "application/json"
			}
			c.Data(genResp.StatusCode, ct, genResp.Body)
			attempts.SetResult(attemptIdx, genResp.StatusCode, string(genResp.Body), "")
			attempts.MarkFinal(attemptIdx)
			lc.ErrorClass = string(attempts.FinalClass())
			g.logGenerationError(reqCtx, user, apiKey, requestID, ep, modelName, route, rp, rp.UpstreamModel,
				genResp.StatusCode, truncate(string(genResp.Body), 256), startedAt, lc, attempts.Chain(), attempts.RetryCount())
			return
		}
		attempts.SetResult(attemptIdx, genResp.StatusCode, "", "")
		attempts.MarkFinal(attemptIdx)
		lc.ErrorClass = string(attempts.FinalClass())
		g.recordGenerationSuccess(reqCtx, rp)
		// --- 2xx success: passthrough response + charge/log ---
		ct := genResp.ContentType
		if ct == "" {
			ct = "application/json"
		}
		c.Data(genResp.StatusCode, ct, genResp.Body)
		usage := genResp.Usage
		// TTS is metered by input characters, which live in the request, not
		// the response — count them here so per-character pricing can apply.
		if kind == EndpointAudioSpeech {
			if usage == nil {
				usage = &GenerationUsage{}
			}
			if in, ok := body["input"].(string); ok {
				usage.Characters = len([]rune(in))
			}
		}
		g.chargeAndLogGeneration(reqCtx, user, apiKey, requestID, ep, modelName, route, rp, rp.UpstreamModel,
			usage, startedAt, genResp.StatusCode, lc, planState, attempts.Chain(), attempts.RetryCount())
		return
	}

	// --- all candidates exhausted ---
	if capable == 0 {
		writeErr(http.StatusServiceUnavailable, "service_unavailable", "endpoint_unsupported_by_providers",
			"no provider for model "+modelName+" serves the "+string(kind)+" endpoint")
		return
	}
	status := http.StatusBadGateway
	msg := "upstream unavailable"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	if lastStatus == http.StatusTooManyRequests || lastStatus >= 500 {
		status = http.StatusServiceUnavailable
	}
	writeErr(status, "upstream_error", "upstream_unavailable", msg)
	// The caller got nothing, but the attempts happened — record them so the
	// row exists in usage_logs (and the channel's health bucket) instead of
	// only in this response.
	lc.ErrorClass = string(attempts.FinalClass())
	g.logGenerationError(reqCtx, user, apiKey, requestID, ep, modelName, route, usedAccount, usedUpstream,
		status, msg, startedAt, lc, attempts.Chain(), attempts.RetryCount())
}

// handleGenerationMultipart runs the pipeline for multipart-upload endpoints
// (image edits/variations, audio transcription/translation). It parses the
// inbound multipart form once, then re-encodes it per provider (rewriting the
// model) via the multipart forwarder. Mirrors handleGenerationJSON's routing/
// billing; the divergence is body handling.
func (g *Gateway) handleGenerationMultipart(c *gin.Context, spec EndpointSpec, requestID string, writeErr func(int, string, string, string)) {
	startedAt := time.Now()
	kind := spec.Kind
	ep := entusagelog.Endpoint(string(kind))
	user, _ := middleware.UserFrom(c)
	apiKey, _ := middleware.ApiKeyFrom(c)

	maxMem := int64(32 << 20)
	if g.maxBodyBytes > 0 {
		maxMem = g.maxBodyBytes
	}
	if err := c.Request.ParseMultipartForm(maxMem); err != nil {
		writeErr(http.StatusBadRequest, "invalid_request_error", "invalid_multipart", "body must be multipart/form-data")
		return
	}
	form := c.Request.MultipartForm
	modelName := c.Request.FormValue("model")
	if modelName == "" {
		writeErr(http.StatusBadRequest, "invalid_request_error", "missing_model", "missing model")
		return
	}

	// Collect fields + files into the neutral MultipartInput once.
	mp := &MultipartInput{Fields: map[string][]string{}}
	if form != nil {
		for k, v := range form.Value {
			mp.Fields[k] = v
		}
		for field, headers := range form.File {
			for _, fh := range headers {
				f, err := fh.Open()
				if err != nil {
					writeErr(http.StatusBadRequest, "invalid_request_error", "file_read_failed", "failed to read uploaded file")
					return
				}
				content, err := io.ReadAll(f)
				_ = f.Close()
				if err != nil {
					writeErr(http.StatusBadRequest, "invalid_request_error", "file_read_failed", "failed to read uploaded file")
					return
				}
				mp.Files = append(mp.Files, MultipartFile{
					Field: field, Filename: fh.Filename,
					ContentType: fh.Header.Get("Content-Type"), Content: content,
				})
			}
		}
	}

	reqCtx := c.Request.Context()
	if t := g.generationTimeout(); t > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, t)
		defer cancel()
	}
	lc := logContext{IP: c.ClientIP(), UserAgent: c.Request.UserAgent(), ApiKeyName: keyName(apiKey)}

	// Input policy scan over the text fields only (file bytes are binary).
	if g.policy != nil {
		var sb strings.Builder
		for _, vals := range mp.Fields {
			for _, v := range vals {
				sb.WriteString(v)
				sb.WriteByte('\n')
			}
		}
		scan := g.policy.Scan(sb.String())
		if block, _ := scan.BlockDecision(); block {
			writeErr(http.StatusUnprocessableEntity, "content_policy_violation", "input_policy_block", "blocked by input moderation policy")
			return
		}
	}

	route, err := g.models.Resolve(reqCtx, modelName, user, apiKey, biz.RequestProtocolOpenAI)
	if err != nil {
		if errors.Is(err, biz.ErrNoProvider) {
			g.recordNoProviderAvailability(reqCtx, err, requestID, modelName, ep, startedAt)
		}
		g.writeRouteError(writeErr, modelName, err)
		return
	}
	if err := g.models.CheckAccess(reqCtx, user, apiKey, route.Group); err != nil {
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, biz.AvailabilityFailureAccess, http.StatusForbidden, startedAt)
		writeErr(http.StatusForbidden, "permission_error", "access_denied", err.Error())
		return
	}
	planState, err := g.billing.ResolvePlanState(reqCtx, user, apiKey, route.Group)
	if err != nil {
		outcome, failureClass, status := billingRouteAvailability(err)
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, outcome, failureClass, status, startedAt)
		g.writeBillingError(c, writeErr, err)
		return
	}
	release, err := g.concur.AcquireWithPlanState(reqCtx, user, route.Group, planState)
	if err != nil {
		g.recordRouteAvailability(reqCtx, route.Group.ID, requestID, modelName, ep, entavailability.OutcomeExcluded, "concurrency_limit", http.StatusTooManyRequests, startedAt)
		writeErr(http.StatusTooManyRequests, "rate_limit_error", "concurrency_limit", err.Error())
		return
	}
	defer release()

	var (
		lastErr      error
		lastStatus   int
		capable      int
		attempted    int
		usedAccount  biz.RouteAccount
		usedUpstream string
	)
	attempts := &usageAttemptRecorder{endpoint: string(ep), group: route.Group.ID.String()}
	for _, rp := range route.Providers {
		if attempted >= g.maxAttempts {
			break
		}
		lc.AccountID = accountIDRef(rp.Account)
		prov, err := g.buildProviderForAttempt(reqCtx, rp, biz.RequestProtocolOpenAI)
		if err != nil {
			lastErr = err
			continue
		}
		gf, ok := asGenerationForwarder(prov)
		if !ok || !gf.SupportsEndpoint(kind) {
			continue
		}
		capable++
		attemptIdx := attempts.Start(rp)
		usedAccount, usedUpstream = rp, rp.UpstreamModel
		attempted++
		attemptStart := time.Now()
		genResp, err := gf.ForwardGeneration(reqCtx, &GenerationRequest{
			Kind: kind, Model: rp.UpstreamModel, Multipart: mp, ClientHeaders: c.Request.Header,
		})
		metrics.LLMAttemptDuration.WithLabelValues(metricOwner(rp), string(ep)).Observe(time.Since(attemptStart).Seconds())
		if err != nil {
			if errors.Is(err, ErrEndpointNotImplemented) || errors.Is(err, ErrEndpointUnsupported) {
				capable--
				attempts.SetResult(attemptIdx, 0, err.Error(), "endpoint_unsupported")
				continue
			}
			lastErr = err
			attempts.SetResult(attemptIdx, 0, err.Error(), "forward_error")
			continue
		}
		if generationShouldFailover(genResp.StatusCode) {
			lastErr = upstreamStatusErr(genResp.StatusCode, genResp.Body)
			lastStatus = genResp.StatusCode
			lc.UpstreamRawError = truncate(string(genResp.Body), 8192)
			attempts.SetResult(attemptIdx, genResp.StatusCode, string(genResp.Body), switchStrategyForStatus(genResp.StatusCode))
			continue
		}
		ct := genResp.ContentType
		if ct == "" {
			ct = "application/json"
		}
		c.Data(genResp.StatusCode, ct, genResp.Body)
		attempts.SetResult(attemptIdx, genResp.StatusCode, string(genResp.Body), "")
		attempts.MarkFinal(attemptIdx)
		lc.ErrorClass = string(attempts.FinalClass())
		if genResp.StatusCode >= 400 {
			g.logGenerationError(reqCtx, user, apiKey, requestID, ep, modelName, route, rp, rp.UpstreamModel,
				genResp.StatusCode, truncate(string(genResp.Body), 256), startedAt, lc, attempts.Chain(), attempts.RetryCount())
		} else {
			g.chargeAndLogGeneration(reqCtx, user, apiKey, requestID, ep, modelName, route, rp, rp.UpstreamModel,
				genResp.Usage, startedAt, genResp.StatusCode, lc, planState, attempts.Chain(), attempts.RetryCount())
		}
		return
	}

	if capable == 0 {
		writeErr(http.StatusServiceUnavailable, "service_unavailable", "endpoint_unsupported_by_providers",
			"no provider for model "+modelName+" serves the "+string(kind)+" endpoint")
		return
	}
	status := http.StatusBadGateway
	msg := "upstream unavailable"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	if lastStatus == http.StatusTooManyRequests || lastStatus >= 500 {
		status = http.StatusServiceUnavailable
	}
	writeErr(status, "upstream_error", "upstream_unavailable", msg)
	lc.ErrorClass = string(attempts.FinalClass())
	g.logGenerationError(reqCtx, user, apiKey, requestID, ep, modelName, route, usedAccount, usedUpstream,
		status, msg, startedAt, lc, attempts.Chain(), attempts.RetryCount())
}

// writeBillingError maps a ResolvePlanState/billing error to an HTTP status,
// matching the chat path's precheck rejection codes.
func (g *Gateway) writeBillingError(_ *gin.Context, writeErr func(int, string, string, string), err error) {
	status := http.StatusPaymentRequired
	switch {
	case errors.Is(err, biz.ErrUnsupportedBillingSource):
		status = http.StatusServiceUnavailable
	case errors.Is(err, biz.ErrPlanNotAllowed):
		status = http.StatusForbidden
	case errors.Is(err, biz.ErrPlanQuotaExhausted),
		errors.Is(err, biz.ErrMonthlySpendLimitExceeded),
		errors.Is(err, biz.ErrApiKeyCostLimitExceeded),
		errors.Is(err, biz.ErrOrganizationLimit),
		errors.Is(err, biz.ErrMemberLimit):
		status = http.StatusTooManyRequests
	}
	writeErr(status, "billing_error", "billing_rejected", err.Error())
}

// minGenerationTimeout floors the cap on generation calls: image edits on
// gpt-image-2.5 routinely run past the 2-minute chat request timeout.
const minGenerationTimeout = 10 * time.Minute

func (g *Gateway) generationTimeout() time.Duration {
	if g.requestTimeout <= 0 {
		return 0
	}
	return max(g.requestTimeout, minGenerationTimeout)
}

// chargeAndLogGeneration meters a successful generation call and writes its
// usage_log row. Billing reuses the existing model billing modes: per_token
// prices the input tokens (embeddings), per_request is a flat charge
// (image/audio/rerank). Per-image / per-second / per-character dimensions are
// a later enhancement; until then a per_request model bills these correctly.
func (g *Gateway) chargeAndLogGeneration(ctx context.Context, user *ent.User, apiKey *ent.ApiKey, requestID string,
	ep entusagelog.Endpoint, modelName string, route *biz.Route, ra biz.RouteAccount, upstream string,
	usage *GenerationUsage, startedAt time.Time, httpStatus int, lc logContext, state *biz.PlanChargeState,
	chain []string, retry int,
) {
	// Same as chargeAndLog: the image is already written to the client, which
	// may disconnect during the multi-MB body; the charge must still land.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	legacyProvID, acctID := usageAttribution(ra)

	tok := biz.TokenUsage{}
	if usage != nil {
		switch ep {
		case entusagelog.EndpointImageGeneration, entusagelog.EndpointImageEdit, entusagelog.EndpointImageVariation:
			tok.ImageTokensIn = int(usage.InputToks)
			tok.ImageTokensOut = int(usage.OutputToks)
		default:
			tok.TokensIn = int(usage.InputToks)
			tok.TokensOut = int(usage.OutputToks)
		}
		tok.OutputImages = usage.Images
		tok.OutputSeconds = usage.AudioSecs
		tok.InputCharacters = usage.Characters
	}
	base, final, err := g.billing.CalcCost(route.Model, route.Group, tok)
	if err != nil {
		_, _ = g.billing.WriteUsageLog(ctx, biz.WriteUsageLogInput{
			UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID,
			OrganizationID: organizationID(apiKey), Endpoint: ep, Model: modelName, ModelID: &route.Model.ID, GroupID: &route.Group.ID,
			ProviderID: legacyProvID, ProviderAccountID: acctID, UpstreamModel: upstream,
			TokensIn: tok.TokensIn, TokensOut: tok.TokensOut,
			ImageTokensIn: tok.ImageTokensIn, ImageTokensOut: tok.ImageTokensOut,
			OutputImages: tok.OutputImages,
			Status:       entusagelog.StatusInternalError, HTTPStatus: httpStatus,
			LatencyMs: int(time.Since(startedAt).Milliseconds()), StartedAt: startedAt,
			ErrorMessage: "cost calc: " + err.Error(),
			ErrorClass:   lc.ErrorClass,
			RetryCount:   retry, ProviderChain: chain,
			IP: lc.IP, UserAgent: lc.UserAgent, ApiKeyName: lc.ApiKeyName,
			Delivered: true,
		})
		return
	}

	effectiveMult := route.Group.RateMultiplier

	res, chargeErr := g.billing.Charge(ctx, user, apiKey, route.Group, base, final, state)
	status := entusagelog.StatusOk
	errMsg := ""
	if chargeErr != nil {
		status = entusagelog.StatusInternalError
		errMsg = chargeErr.Error()
	}
	bs := entusagelog.BillingSourceNone
	orgID := organizationID(apiKey)
	var userPlanID *uuid.UUID
	if res != nil {
		bs = res.BillingSource
		if res.OrganizationID != nil {
			orgID = res.OrganizationID
		}
		userPlanID = res.UserPlanID
	}
	_, _ = g.billing.WriteUsageLog(ctx, biz.WriteUsageLogInput{
		UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID,
		OrganizationID: orgID, Endpoint: ep, Model: modelName, ModelID: &route.Model.ID, GroupID: &route.Group.ID,
		ProviderID: legacyProvID, ProviderAccountID: acctID, UpstreamModel: upstream,
		TokensIn: tok.TokensIn, TokensOut: tok.TokensOut,
		ImageTokensIn: tok.ImageTokensIn, ImageTokensOut: tok.ImageTokensOut,
		OutputImages: tok.OutputImages,
		CostBase:     base, CostFinal: final, RateMultiplierApplied: effectiveMult, Currency: biz.CurrencyCNY,
		BillingSource: bs, UserPlanID: userPlanID,
		Status: status, HTTPStatus: httpStatus,
		LatencyMs: int(time.Since(startedAt).Milliseconds()), StartedAt: startedAt,
		ErrorMessage: errMsg,
		ErrorClass:   lc.ErrorClass,
		RetryCount:   retry, ProviderChain: chain,
		IP: lc.IP, UserAgent: lc.UserAgent, ApiKeyName: lc.ApiKeyName,
		Delivered: true,
	})
}

// logGenerationError records a failed generation call: a terminal upstream
// client-error (4xx) the caller received, or the all-candidates-exhausted
// outcome where no provider answered. In the latter case lc.ErrorClass
// already carries the last attempt's structured class, so the row agrees
// with provider_chain instead of re-deriving from the synthesized status.
func (g *Gateway) logGenerationError(ctx context.Context, user *ent.User, apiKey *ent.ApiKey, requestID string,
	ep entusagelog.Endpoint, modelName string, route *biz.Route, ra biz.RouteAccount, upstream string,
	httpStatus int, errMsg string, startedAt time.Time, lc logContext,
	chain []string, retry int,
) {
	legacyProvID, acctID := usageAttribution(ra)

	var provID *uuid.UUID
	if legacyProvID != nil {
		provID = legacyProvID
	}
	_ = acctID
	class := lc.ErrorClass
	if class == "" {
		class = string(classifyUpstreamResponse(httpStatus, []byte(lc.UpstreamRawError)))
	}
	_, _ = g.billing.WriteUsageLog(ctx, biz.WriteUsageLogInput{
		UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID,
		OrganizationID: organizationID(apiKey), Endpoint: ep, Model: modelName, ModelID: &route.Model.ID, GroupID: &route.Group.ID,
		ProviderID: provID, UpstreamModel: upstream,
		Status: entusagelog.StatusUpstreamError, HTTPStatus: httpStatus,
		LatencyMs: int(time.Since(startedAt).Milliseconds()), StartedAt: startedAt,
		ErrorMessage: errMsg, UpstreamRawError: lc.UpstreamRawError,
		ErrorClass: class,
		RetryCount: retry, ProviderChain: chain,
		IP: lc.IP, UserAgent: lc.UserAgent, ApiKeyName: lc.ApiKeyName,
	})
}
