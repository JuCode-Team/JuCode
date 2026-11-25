// billing_log.go: post-response usage_log + billing writes shared by
// every gateway endpoint (OpenAI chat/responses, Anthropic messages).
// Splitting these helpers out of gateway.go keeps the main retry
// loop focused on policy + provider selection.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/metrics"
)

// logContext bundles the per-request fields that every WriteUsageLog
// call needs to populate. Computed once at the top of handle() and
// passed down to chargeAndLog / logFinal / inline reject paths.
type logContext struct {
	// RateTicket carries the RPM/TPM scope resolved at peek time through to
	// the commit and settle phases. nil whenever the gate is off or the user
	// has no caps configured; every method on it is nil-safe.
	RateTicket *biz.RateLimitTicket

	// Reservation is the pre-charge hold taken before the provider loop
	// (see biz.BillingSvc.Reserve). Every terminal path must finish it:
	// chargeAndLog settles it with the actual cost; logFinal settles it
	// against observed usage on any last-hop failure (499 hang-up,
	// stream_incomplete, timeout, response.failed) and releases it when
	// nothing billable was observed. nil on paths that reject before
	// the reserve point.
	Reservation *biz.Reservation

	IP              string
	UserAgent       string
	ReasoningEffort string
	Stream          bool
	ApiKeyName      string
	TtfbMs          *int // first-byte (first downstream response byte/flush)
	Latency         *latencyTracker
	// Admin-only upstream diagnostics for the final usage_log row.
	// ErrorClass is the structured outcome of the last attempt;
	// UpstreamRequestID is the provider's own request id;
	// UpstreamRawError is the upstream error body (≤8KB) used to classify
	// the failure; only a redacted summary of it is stored.
	ErrorClass        string
	UpstreamRequestID string
	UpstreamRawError  string
	// AccountID is the provider account that served the final attempt, when
	// the account layer handled the request. Stamped onto
	// usage_logs.provider_account_id — internal attribution, never returned to
	// clients.
	AccountID *uuid.UUID
	// AuthorizationID is set when the caller authenticated via the OAuth
	// device flow — stamped onto usage_logs.authorization_id (api_key_id
	// stays NULL for those requests).
	AuthorizationID *uuid.UUID
	// TurnID is the JuCode Desktop agent turn (X-JuCode-Turn) this request
	// belongs to; only set for OAuth device callers. See agent_turn.go.
	TurnID string
}

// newBillingHoldKey returns a server-owned idempotency identity. Trace request
// IDs are intentionally not accepted here: X-Request-Id is client-controlled,
// so using it as a hold key lets parallel requests share one reservation.
func newBillingHoldKey(scope string) string {
	return scope + ":" + uuid.NewString()
}

func modelIDFromRoute(route *biz.Route) *uuid.UUID {
	if route == nil || route.Model == nil {
		return nil
	}
	id := route.Model.ID
	return &id
}

func modelIDFromEntity(model *ent.Model) *uuid.UUID {
	if model == nil {
		return nil
	}
	id := model.ID
	return &id
}

// chargeAndLog is the success path: calculate cost, debit the user
// (balance / plan), then persist a usage_log row. Errors from charging
// are tolerated (the row goes in with status=internal_error and the
// charging error message); we never reject a response the user already
// got just because logging or charging fell over.
func (g *Gateway) chargeAndLog(ctx context.Context, user *ent.User, apiKey *ent.ApiKey, requestID string,
	ep entusagelog.Endpoint, modelName string, route *biz.Route, ra biz.RouteAccount, upstream string,
	acc *Accumulator, ttftMs *int, startedAt time.Time, chain []string, retry int, httpStatus int, lc logContext, state *biz.PlanChargeState,
) {
	acct := ra.Account
	legacyProvID, acctID := usageAttribution(ra)

	// The client holds the whole response by now; everything below is our own
	// bookkeeping, so the clock stops here rather than in the WriteUsageLog
	// literals. See docs/observability.md.
	latencyMs := int(time.Since(startedAt).Milliseconds())

	// Survive client disconnect: charging + usage-log writes must complete even
	// when the caller's request context was cancelled mid-stream, otherwise
	// delivered usage goes unbilled and the row is lost. (WriteUsageLog detaches
	// again internally; that is harmless.)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	tokUsage := usageFromAccumulator(acc)
	// Estimate a missing cache-write count BEFORE pricing, so billing and
	// the usage_log row are computed from the same (possibly reclassified)
	// numbers. Keyed off the PROVIDER that actually served this request —
	// reporting behavior is an upstream trait. The accumulator itself is
	// left untouched.
	tokUsage, cacheWriteEstimated := biz.ApplyCacheWriteEstimate(acct, tokUsage)
	// Keyed by the provider that actually served the request, so "which
	// channel burned the input tokens" is answerable without a SQL scan.
	metrics.BillingTokensTotal.WithLabelValues(metricOwner(ra), modelName, "in").Add(float64(tokUsage.TokensIn))
	metrics.BillingTokensTotal.WithLabelValues(metricOwner(ra), modelName, "out").Add(float64(tokUsage.TokensOut))
	metrics.BillingTokensTotal.WithLabelValues(metricOwner(ra), modelName, "cached").Add(float64(tokUsage.CachedTokensIn))

	// Settle the output-only TPM counter on the same detached context as
	// charging. Prompt input, cache reads, and cache writes are intentionally
	// excluded. Failure paths route through logFinal, which settles TPM
	// only when the hold is settled against observed usage.
	lc.RateTicket.Settle(ctx, rateLimitOutputTokens(tokUsage))

	base, final, err := g.billing.CalcCost(route.Model, route.Group, tokUsage)
	if err != nil {
		// Nothing chargeable can be computed — return the hold rather than
		// leaving it to the reaper.
		g.releaseReservation(ctx, lc.Reservation)
		_, _ = g.billing.WriteUsageLog(ctx, biz.WriteUsageLogInput{
			UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID, Endpoint: ep,
			OrganizationID: organizationID(apiKey),
			Model:          modelName, ModelID: &route.Model.ID, GroupID: &route.Group.ID, ProviderID: legacyProvID, ProviderAccountID: acctID, UpstreamModel: upstream,
			TokensIn: tokUsage.TokensIn, TokensOut: tokUsage.TokensOut,
			CachedTokensIn: tokUsage.CachedTokensIn, CacheCreationTokensIn: tokUsage.CacheCreationTokensIn,
			CacheCreation5mTokensIn: tokUsage.CacheCreation5mTokensIn, CacheCreation1hTokensIn: tokUsage.CacheCreation1hTokensIn,
			ImageTokensIn: tokUsage.ImageTokensIn, ImageTokensOut: tokUsage.ImageTokensOut,
			AudioTokensIn: tokUsage.AudioTokensIn, AudioTokensOut: tokUsage.AudioTokensOut,
			CacheWriteEstimated: cacheWriteEstimated,
			OutputImages:        tokUsage.OutputImages,
			Status:              entusagelog.StatusInternalError, HTTPStatus: httpStatus,
			LatencyMs:         latencyMs,
			TtftMs:            ttftMs,
			TtfbMs:            lc.TtfbMs,
			LatencyDetail:     withToolUsageDetail(lc.Latency.Snapshot(), acc.WebSearchRequests, acc.FileSearchRequests, acc.CodeExecutionRequests, acc.ComputerUseRequests),
			WebSearchRequests: acc.WebSearchRequests,
			RetryCount:        retry, ProviderChain: chain,
			ErrorMessage: "cost calc: " + err.Error(),
			ErrorClass:   lc.ErrorClass, UpstreamRequestID: lc.UpstreamRequestID, UpstreamRawError: lc.UpstreamRawError,
			IP: lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
			Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
			StartedAt: startedAt, Delivered: true,
		})
		return
	}

	effectiveMult := route.Group.RateMultiplier

	// Settle the pre-charge hold with the exact upstream-reported cost
	// (refund the difference / collect any overage; exact settle may
	// overdraft). Paths that never reserved — e.g. the pass-through
	// realtime meter before its per-turn gate resolves — fall back to
	// Charge, which also deducts final in full and may overdraft.
	// CapAtHold still clamps.
	var res *biz.ChargeResult
	var chargeErr error
	if lc.Reservation != nil {
		res, chargeErr = g.billing.SettleReservation(ctx, lc.Reservation, base, final,
			biz.SettleOptions{Note: "llm gateway settle " + requestID})
	} else {
		res, chargeErr = g.billing.Charge(ctx, user, apiKey, route.Group, base, final, state)
	}
	status := entusagelog.StatusOk
	errMsg := ""
	if chargeErr != nil {
		status = entusagelog.StatusInternalError
		errMsg = chargeErr.Error()
	}
	bs := entusagelog.BillingSourceNone
	var userPlanID *uuid.UUID
	var orgID *uuid.UUID
	chargedFinal := final
	if res != nil {
		bs = res.BillingSource
		userPlanID = res.UserPlanID
		orgID = res.OrganizationID
		// Exact settle records full CostFinal (balance may go negative).
		// CapAtHold still may charge less than asked (reconstructed usage
		// does not create debt). usage_logs records money actually collected.
		chargedFinal = res.CostFinal
	}
	if chargeErr == nil {
		g.recordAgentTurn(user.ID, lc, route, chargedFinal)
	}
	_, _ = g.billing.WriteUsageLog(ctx, biz.WriteUsageLogInput{
		UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID, Endpoint: ep,
		OrganizationID: orgID,
		Model:          modelName, ModelID: &route.Model.ID, GroupID: &route.Group.ID, ProviderID: legacyProvID, ProviderAccountID: acctID, UpstreamModel: upstream,
		TokensIn: tokUsage.TokensIn, TokensOut: tokUsage.TokensOut,
		CachedTokensIn: tokUsage.CachedTokensIn, CacheCreationTokensIn: tokUsage.CacheCreationTokensIn,
		CacheCreation5mTokensIn: tokUsage.CacheCreation5mTokensIn, CacheCreation1hTokensIn: tokUsage.CacheCreation1hTokensIn,
		ImageTokensIn: tokUsage.ImageTokensIn, ImageTokensOut: tokUsage.ImageTokensOut,
		AudioTokensIn: tokUsage.AudioTokensIn, AudioTokensOut: tokUsage.AudioTokensOut,
		CacheWriteEstimated: cacheWriteEstimated,
		OutputImages:        tokUsage.OutputImages,
		// Currency stored in usage_logs is the ledger unit (CNY) — see
		// internal/biz/currency.go § normalization. user.Currency is now
		// a display-preference only; the frontend converts CNY → user
		// preference at render time using settings.ExchangeCNYPerUSD.
		CostBase: base, CostFinal: chargedFinal, RateMultiplierApplied: effectiveMult, Currency: biz.CurrencyCNY, BillingSource: bs,
		UserPlanID: userPlanID,
		Status:     status, HTTPStatus: httpStatus,
		ErrorClass: lc.ErrorClass, UpstreamRequestID: lc.UpstreamRequestID, UpstreamRawError: lc.UpstreamRawError,
		LatencyMs:         latencyMs,
		TtftMs:            ttftMs,
		TtfbMs:            lc.TtfbMs,
		LatencyDetail:     withToolUsageDetail(lc.Latency.Snapshot(), acc.WebSearchRequests, acc.FileSearchRequests, acc.CodeExecutionRequests, acc.ComputerUseRequests),
		WebSearchRequests: acc.WebSearchRequests,
		RetryCount:        retry, ProviderChain: chain,
		ErrorMessage: errMsg,
		IP:           lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
		Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
		StartedAt: startedAt, Delivered: true,
	})
}

// rateLimitOutputTokens is the TPM settlement amount: generated tokens only.
// Audio output counts — it is output the model produced, just in another
// modality, and it lives outside TokensOut since the audio pricing split.
func rateLimitOutputTokens(usage biz.TokenUsage) int64 {
	return int64(usage.TokensOut) + int64(usage.AudioTokensOut)
}

func withToolUsageDetail(detail map[string]int, webSearch, fileSearch, codeExec, computer int) map[string]int {
	if webSearch <= 0 && fileSearch <= 0 && codeExec <= 0 && computer <= 0 {
		return detail
	}
	if detail == nil {
		detail = map[string]int{}
	}
	if webSearch > 0 {
		detail["web_search_requests"] = webSearch
	}
	if fileSearch > 0 {
		detail["file_search_requests"] = fileSearch
	}
	if codeExec > 0 {
		detail["code_execution_requests"] = codeExec
	}
	if computer > 0 {
		detail["computer_use_requests"] = computer
	}
	return detail
}

// logFinal is the failure path: finish the reservation, then write the
// usage_log row.
//
// logFinal is only reached after failover cannot continue (or must not).
// Observed usage on that last hop is work the upstream already billed —
// a 499 hang-up, a truncated stream after message_start, an idle/total
// timeout, or response.failed after tokens. Those SETTLE the hold
// against observed usage only: exact when a usage block arrived
// (including cache_read / cache_create), otherwise reconstructed from
// streamed visible text / images and capped at the hold. The reserve-time
// input overestimate is never billed. No observed work → Release.
//
// Cache-write estimation runs ONLY on the exact-usage settle (see
// cancelBillableUsage), mirroring chargeAndLog so a last-hop failure
// cannot dodge the cache-write rate the completed request would have
// paid. Every non-billing row keeps the upstream's raw token shape.
func (g *Gateway) logFinal(c *gin.Context, user *ent.User, apiKey *ent.ApiKey, requestID string,
	ep entusagelog.Endpoint, modelName string, route *biz.Route, ra biz.RouteAccount, upstream string,
	acc *Accumulator, ttftMs *int, startedAt time.Time, chain []string, retry int, status entusagelog.Status, httpStatus int, errMsg string, lc logContext,
) {
	acct := ra.Account

	// Same as chargeAndLog: the reservation settle/release below is bookkeeping.
	latencyMs := int(time.Since(startedAt).Milliseconds())
	var groupID *uuid.UUID
	if route != nil {
		groupID = &route.Group.ID
	}
	var provID *uuid.UUID
	if legacy := ra.LegacyProviderID(); legacy != nil {
		provID = legacy
	}
	costBase, costFinal := decimal.Zero, decimal.Zero
	billingSource := entusagelog.BillingSourceNone
	var userPlanID *uuid.UUID
	orgID := organizationID(apiKey)
	effectiveMult := 0.0
	logUsage := usageFromAccumulator(acc)
	logCacheWriteEstimated := false
	logWebSearch, logFileSearch, logCodeExec, logComputer := 0, 0, 0, 0
	if acc != nil {
		logWebSearch = acc.WebSearchRequests
		logFileSearch = acc.FileSearchRequests
		logCodeExec = acc.CodeExecutionRequests
		logComputer = acc.ComputerUseRequests
	}
	if lc.Reservation != nil {
		// Detached context: the settle/release must complete even though the
		// request context is usually already cancelled on this path.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 20*time.Second)
		defer cancel()
		if usage, estimated, cacheWriteEstimated, settle := failureShouldSettle(route, acct, acc); settle && route.Model != nil && route.Group != nil {
			if base, final, err := g.billing.CalcCost(route.Model, route.Group, usage); err == nil {
				effectiveMult = route.Group.RateMultiplier
				res, settleErr := g.billing.SettleReservation(ctx, lc.Reservation, base, final, biz.SettleOptions{
					Note:      "llm gateway observed-usage settle " + requestID,
					CapAtHold: estimated,
				})
				if settleErr != nil {
					errMsg = errMsg + "; observed-usage settle: " + settleErr.Error()
				}
				if res != nil {
					costBase, costFinal = base, res.CostFinal
					billingSource = res.BillingSource
					userPlanID = res.UserPlanID
					if res.OrganizationID != nil {
						orgID = res.OrganizationID
					}
				}
				// Persist the token breakdown the money was computed from —
				// exact usage (possibly cache-write-reclassified) or the
				// output-only reconstruction. A row must never say cost>0
				// next to an all-zero (or differently-shaped) token split.
				logUsage = usage
				logCacheWriteEstimated = cacheWriteEstimated
				if res != nil {
					g.recordAgentTurn(user.ID, lc, route, res.CostFinal)
				}
				lc.RateTicket.Settle(ctx, rateLimitOutputTokens(usage))
			} else {
				g.releaseReservation(ctx, lc.Reservation)
			}
		} else {
			g.releaseReservation(ctx, lc.Reservation)
		}
	}
	_, _ = g.billing.WriteUsageLog(c.Request.Context(), biz.WriteUsageLogInput{
		UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID, Endpoint: ep,
		OrganizationID: orgID,
		Model:          modelName, ModelID: modelIDFromRoute(route), GroupID: groupID, ProviderID: provID, ProviderAccountID: lc.AccountID, UpstreamModel: upstream,
		TokensIn: logUsage.TokensIn, TokensOut: logUsage.TokensOut,
		CachedTokensIn: logUsage.CachedTokensIn, CacheCreationTokensIn: logUsage.CacheCreationTokensIn,
		CacheCreation5mTokensIn: logUsage.CacheCreation5mTokensIn, CacheCreation1hTokensIn: logUsage.CacheCreation1hTokensIn,
		ImageTokensIn: logUsage.ImageTokensIn, ImageTokensOut: logUsage.ImageTokensOut,
		AudioTokensIn: logUsage.AudioTokensIn, AudioTokensOut: logUsage.AudioTokensOut,
		CacheWriteEstimated: logCacheWriteEstimated,
		OutputImages:        logUsage.OutputImages,
		CostBase:            costBase, CostFinal: costFinal, RateMultiplierApplied: effectiveMult, Currency: biz.CurrencyCNY, BillingSource: billingSource,
		UserPlanID: userPlanID,
		Status:     status, HTTPStatus: httpStatus,
		LatencyMs:         latencyMs,
		TtftMs:            ttftMs,
		TtfbMs:            lc.TtfbMs,
		LatencyDetail:     withToolUsageDetail(lc.Latency.Snapshot(), logWebSearch, logFileSearch, logCodeExec, logComputer),
		WebSearchRequests: logWebSearch,
		IP:                lc.IP, UserAgent: lc.UserAgent, ReasoningEffort: lc.ReasoningEffort,
		Stream: lc.Stream, ApiKeyName: lc.ApiKeyName,
		RetryCount: retry, ProviderChain: chain,
		ErrorMessage: errMsg, StartedAt: startedAt,
		ErrorClass: lc.ErrorClass, UpstreamRequestID: lc.UpstreamRequestID, UpstreamRawError: lc.UpstreamRawError,
	})
}

// usageFromAccumulator copies the accumulator's reported token shape for
// usage-log persistence. Safe on nil.
func usageFromAccumulator(acc *Accumulator) biz.TokenUsage {
	if acc == nil {
		return biz.TokenUsage{}
	}
	return biz.TokenUsage{
		TokensIn:                acc.TokensIn,
		TokensOut:               acc.TokensOut,
		CachedTokensIn:          acc.CachedTokensIn,
		CacheCreationTokensIn:   acc.CacheCreationTokensIn,
		CacheCreation5mTokensIn: acc.CacheCreation5mTokensIn,
		CacheCreation1hTokensIn: acc.CacheCreation1hTokensIn,
		ImageTokensIn:           acc.ImageTokensIn,
		ImageTokensOut:          acc.ImageTokensOut,
		AudioTokensIn:           acc.AudioTokensIn,
		AudioTokensOut:          acc.AudioTokensOut,
		OutputImages:            len(acc.OutputImages),
	}
}

// cancelSettlementUsage decides what a last-hop failure is charged for.
// Exact usage wins when the upstream reported it before the disconnect or
// stream abort — including its cache_read / cache_creation split, so cache
// hits keep their discounted rate instead of collapsing into full-price
// input. Otherwise the charge is reconstructed from observed output only:
// visible text plus streamed tool-call arguments actually relayed
// downstream (~3 bytes per token), plus any images. Thinking deltas are
// never accumulated, so an aborted reasoning stream under-counts — by
// design. The reserve-time input overestimate is intentionally not used:
// it is a freeze ceiling, not a meter. `estimated` is true on the
// reconstructed path so the settle caps at the hold.
func cancelSettlementUsage(acc *Accumulator) (usage biz.TokenUsage, estimated bool) {
	if acc != nil && acc.HasUsage {
		return usageFromAccumulator(acc), false
	}
	out := 0
	images := 0
	if acc != nil {
		out = (acc.AssistantText.Len() + acc.ToolArgumentBytes()) / 3
		images = len(acc.OutputImages)
	}
	return biz.TokenUsage{TokensOut: out, OutputImages: images}, true
}

// cancelBillableUsage is the last-hop-failure counterpart of
// chargeAndLog's pricing prep: pick the settlement usage, then — only when
// the upstream reported exact usage — run the same provider-keyed
// cache-write reclassification the success path applies
// (biz.ApplyCacheWriteEstimate). Without it, a hang-up or truncated stream
// against a provider flagged estimate_cache_write would bill the uncached
// input remainder at the plain input rate while a completed request pays
// the cache-write rate. The reconstructed (no-usage) path never runs the
// estimate: its input side is zero by construction, and inventing an input
// to reclassify is exactly the phantom-charge bug this path exists to
// prevent.
func cancelBillableUsage(acct *ent.UpstreamAccount, acc *Accumulator) (usage biz.TokenUsage, estimated, cacheWriteEstimated bool) {
	usage, estimated = cancelSettlementUsage(acc)
	if !estimated {
		usage, cacheWriteEstimated = biz.ApplyCacheWriteEstimate(acct, usage)
	}
	return usage, estimated, cacheWriteEstimated
}

// failureShouldSettle reports whether logFinal should settle the hold.
// logFinal is never called on a hop that can still fail over, so observed
// usage here is the last hop — the same "upstream billed, we must bill"
// case as a 499 hang-up. A nil route or empty observed usage releases.
func failureShouldSettle(route *biz.Route, acct *ent.UpstreamAccount, acc *Accumulator) (usage biz.TokenUsage, estimated, cacheWriteEstimated, settle bool) {
	if route == nil {
		return biz.TokenUsage{}, false, false, false
	}
	usage, estimated, cacheWriteEstimated = cancelBillableUsage(acct, acc)
	return usage, estimated, cacheWriteEstimated, usageHasBillableWork(usage)
}

// usageHasBillableWork reports whether reconstructed/exact usage contains
// anything that CalcCost would turn into a charge. Empty cancel paths must
// Release rather than Settle-at-the-reserve-estimate.
func usageHasBillableWork(u biz.TokenUsage) bool {
	return u.TokensIn > 0 || u.TokensOut > 0 ||
		u.CachedTokensIn > 0 || u.CacheCreationTokensIn > 0 ||
		u.ImageTokensIn > 0 || u.ImageTokensOut > 0 ||
		u.AudioTokensIn > 0 || u.AudioTokensOut > 0 ||
		u.OutputImages > 0 || u.OutputSeconds > 0
}

// requestedMaxOutputTokens pulls the client's generation cap out of a decoded
// request body, across the dialects the gateway serves: chat completions
// (max_tokens / max_completion_tokens), responses (max_output_tokens) and
// Anthropic messages (max_tokens). 0 = unbounded, which makes the reserve
// estimator fall back to its own conservative default.
func requestedMaxOutputTokens(body map[string]any) int {
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if v, ok := body[key].(float64); ok && v > 0 {
			return int(v)
		}
	}
	return 0
}

// releaseReservation returns a hold with a detached, bounded context; safe
// on nil reservations. Failures are logged — the expiry reaper is the
// backstop for a release that could not run.
func (g *Gateway) releaseReservation(ctx context.Context, r *biz.Reservation) {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := g.billing.ReleaseReservation(ctx, r); err != nil {
		slog.Warn("billing hold release failed", "key", r.Key, "err", err)
	}
}

// looksLikeCyberSecurityBlock checks an upstream error body for the
// signatures OpenAI / Codex use when their cyber-security classifier
// intercepts a prompt. We match on a small set of substrings rather
// than parse the JSON because the exact response shape has shifted
// across vendor versions and we'd rather over-flag in ambiguous cases —
// every match also gets a normal usage-log row so admins can review.
func looksLikeCyberSecurityBlock(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	// "cyber security" appears in Codex blocks; "harmful content" in
	// some Responses API rejections; "moderation_blocked" / "moderation"
	// in OpenAI moderation hits. "content filtering" / "content_filter"
	// is the relay-side wording, e.g. the invalid_request_error body
	// {"message":"Request was blocked by upstream content filtering"} —
	// it arrives typed as a request error but is a safety block, so it
	// must classify as one rather than as a user_request 400. We
	// intentionally don't match on the generic "policy" word — that
	// catches too many benign 400s.
	for _, pat := range []string{
		"cyber security",
		"cybersecurity",
		"moderation_blocked",
		"harmful content",
		"safety_violation",
		"unsafe content",
		"content filtering",
		"content_filtering",
		"content filter",
		"content_filter",
		"content policy",
		"content_policy",
	} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// actionableUpstreamError inspects an upstream error body and, when it
// describes a failure the caller should relay verbatim instead of masking
// behind the generic "request rejected by service", returns that message, a
// short machine code, and true. The gateway masks upstream bodies by default
// to avoid leaking channel/account details; this is the curated allowlist of
// exceptions the user can actually act on:
//
//   - input over the context window → shorten the conversation (context_length_exceeded)
//   - a disabled feature, e.g. "image generation is not enabled
//     for this group"                                          (feature_not_enabled)
//   - a transient upstream stream-read failure                 (stream_read_error)
//   - an input image below the model pixel floor (Grok/xAI)    (image_too_small)
//   - a bad image data-URL / MIME                              (invalid_image)
//   - unsupported tool / parameter / model (user-fixable)      (unsupported_request)
//   - a broken tool/session transcript                         (session_corrupt)
//   - any other invalid_request_error whose message plainly
//     describes a request problem                              (invalid_request)
//
// Auth/quota bodies that merely say "forbidden", "unauthorized", or name an
// account are NOT matched and stay masked. Matching is keyword/type based
// because upstream wording is vendor-version fragile.
func actionableUpstreamError(body []byte) (message, code string, ok bool) {
	if len(body) == 0 {
		return "", "", false
	}
	// A content-filter / moderation rejection is never relayed verbatim, even
	// when the upstream types it as invalid_request_error: it belongs to the
	// safety path, which answers with our own wording (cyberSafetyMessage).
	// Without this guard the `typ == "invalid_request_error"` arm below would
	// echo the upstream's phrasing and the block would read as a request bug.
	if looksLikeCyberSecurityBlock(body) {
		return "", "", false
	}
	// A route gap in our own upstream is not the caller's fault; it gets its
	// own reply (upstreamRejectedReply), never an "invalid request" label.
	if biz.IsUpstreamRouteGap(0, string(body)) {
		return "", "", false
	}
	// Known request-level failures get specific fixed copy (cause + fix),
	// shared with the console's per-request reason.
	if code, text, ok := biz.UpstreamErrorHint(string(body)); ok {
		return text, code, true
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) != nil {
		return "", "", false
	}
	typ := firstNonEmpty(env.Error.Type, env.Type)
	rawCode := firstNonEmpty(env.Error.Code, env.Code)
	msg := firstNonEmpty(env.Error.Message, env.Message)
	if msg == "" {
		return "", "", false
	}
	hay := strings.ToLower(msg + " " + rawCode + " " + typ)
	if leaksChannel(hay) {
		return "", "", false
	}
	switch {
	case rawCode == "context_length_exceeded" || looksLikeContextTooLong(hay):
		return contextTooLongMessage(msg), "context_length_exceeded", true
	case looksLikeImageTooSmall(hay):
		// Grok/xAI types this as api_error, so it never reaches the
		// invalid_request_error arm. The user can fix it (send a larger
		// image); explain in Chinese and keep the measured size.
		return imageTooSmallMessage(msg), "image_too_small", true
	case rawCode == "request_body_too_large" ||
		strings.Contains(hay, "request body is too large") ||
		strings.Contains(hay, "request body too large") ||
		strings.Contains(hay, "request entity too large") ||
		strings.Contains(hay, "payload too large") ||
		strings.Contains(hay, "request is too large") ||
		strings.Contains(hay, "upstream error: 413") ||
		strings.Contains(hay, "after safe local compression"):
		// The whole payload is over a byte cap — distinct from
		// context_length_exceeded, which is about the model's context window.
		// Two upstream wordings land here: a plain "request body is too large",
		// and the relay-side pre-flight reject that reports its own body/text/
		// token limits and ends with "The request was not sent upstream". Both
		// have the same fix — send less — so we state that in Chinese instead
		// of relaying the upstream's English.
		return "请求体过大：请减少对话历史、工具输出或图片/PDF 附件后重试", "request_body_too_large", true
	case rawCode == "stream_read_error" || strings.Contains(hay, "stream_read_error"):
		return "the upstream response stream failed to read; please try again", "stream_read_error", true
	case strings.Contains(hay, "not enabled") || strings.Contains(hay, "is not available for"):
		return "该功能未启用，请联系管理员确认配置", "feature_not_enabled", true
	case looksLikeInvalidImage(hay):
		return "图片附件无效：请使用带图像 MIME 的 base64 data URL（例如 data:image/png;base64,...）", "invalid_image", true
	case looksLikeCorruptSession(hay, msg):
		return "当前会话状态已损坏，请新开一个会话后重试", "session_corrupt", true
	case looksLikeUnsupportedRequest(hay, msg):
		return biz.WithUpstreamParam(unsupportedRequestMessage(msg), string(body)), "unsupported_request", true
	case strings.Contains(hay, "missing required parameter") ||
		strings.Contains(hay, "unknown parameter") ||
		strings.Contains(hay, "unexpected parameter") ||
		strings.Contains(hay, "unrecognized request argument") ||
		strings.Contains(hay, "field messages is required") ||
		strings.Contains(hay, "messages is required") ||
		strings.Contains(hay, "is not one of") ||
		strings.Contains(hay, "invalid request body") ||
		strings.Contains(hay, "invalid schema"):
		// The upstream rejected the request body's shape — a required field
		// omitted (e.g. "Missing required parameter: 'input[5].name'.") or an
		// unexpected one sent. This almost always means the endpoint is
		// misconfigured or the client built a body the upstream schema does
		// not accept, not an account/channel fault. Label it plainly and
		// relay the upstream's own detail verbatim so it can be fixed.
		return biz.WithUpstreamParam("端点配置错误或请求格式错误，请检查请求参数", string(body)), "endpoint_or_request_format", true
	case typ == "invalid_request_error":
		return biz.WithUpstreamParam("请求参数有误，请检查后重试", string(body)), "invalid_request", true
	}
	return "", "", false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	imageTooSmallPixelsRe = regexp.MustCompile(`(?i)(\d+)\s+total pixels\s+\((\d+)\s*[x×]\s*(\d+)\).*minimum of\s+(\d+)\s+pixels`)
	imageTooSmallDimRe    = regexp.MustCompile(`(?i)(\d+)\s*[x×]\s*(\d+).*minimum of\s+(\d+)`)
	imageTooSmallMinRe    = regexp.MustCompile(`(?i)minimum(?: of| is)\s+(\d+)\s+pixels`)
)

func looksLikeImageTooSmall(hay string) bool {
	if !strings.Contains(hay, "image") {
		return false
	}
	switch {
	case strings.Contains(hay, "too small"):
		return true
	case strings.Contains(hay, "below the minimum") && strings.Contains(hay, "pixel"):
		return true
	case strings.Contains(hay, "total pixels") && strings.Contains(hay, "minimum"):
		return true
	default:
		return false
	}
}

func imageTooSmallMessage(msg string) string {
	if m := imageTooSmallPixelsRe.FindStringSubmatch(msg); len(m) == 5 {
		return fmt.Sprintf("图片尺寸过小：当前 %s×%s（%s 像素），模型要求总像素不少于 %s。请换一张更大的图片后重试", m[2], m[3], m[1], m[4])
	}
	if m := imageTooSmallDimRe.FindStringSubmatch(msg); len(m) == 4 {
		return fmt.Sprintf("图片尺寸过小：当前 %s×%s，模型要求总像素不少于 %s。请换一张更大的图片后重试", m[1], m[2], m[3])
	}
	if m := imageTooSmallMinRe.FindStringSubmatch(msg); len(m) == 2 {
		return fmt.Sprintf("图片尺寸过小：未达到模型要求的最小 %s 像素。请换一张更大的图片后重试", m[1])
	}
	return "图片尺寸过小，未达到模型要求的最小像素。请换一张更大的图片后重试"
}

func leaksChannel(hay string) bool {
	switch {
	case strings.Contains(hay, "configured upstream"):
		return true
	case strings.Contains(hay, "upstream account"):
		return true
	case strings.Contains(hay, "no available account"):
		return true
	case strings.Contains(hay, "no shared"):
		return true
	case strings.Contains(hay, "accounts exhausted"):
		return true
	case strings.Contains(hay, "please contact administrator"):
		return true
	case strings.Contains(hay, "预扣费"):
		return true
	case strings.Contains(hay, "剩余额度"):
		return true
	case strings.Contains(hay, "unpurchased"):
		return true
	case strings.Contains(hay, "cooling down"):
		return true
	case strings.Contains(hay, "insufficient_quota"):
		return true
	case strings.Contains(hay, "allocated quota"):
		return true
	default:
		return false
	}
}

func looksLikeContextTooLong(hay string) bool {
	switch {
	case strings.Contains(hay, "context window"):
		return true
	case strings.Contains(hay, "maximum context length"):
		return true
	case strings.Contains(hay, "context length"):
		return true
	case strings.Contains(hay, "maximum prompt length"):
		return true
	// Anthropic, when input + max_tokens overflows: "input length and
	// `max_tokens` exceed context limit: 188240 + 32000 > 200000".
	case strings.Contains(hay, "exceed context limit"):
		return true
	case strings.Contains(hay, "prompt is too long"):
		return true
	case strings.Contains(hay, "prompt too long"):
		return true
	default:
		return false
	}
}

var (
	contextMaxPromptRe = regexp.MustCompile(`(?i)maximum prompt length is\s+(\d+).*contains\s+(\d+)\s+tokens`)
	contextPromptGTRe  = regexp.MustCompile(`(?i)too long:\s*(\d+)\s+tokens\s*>\s*(\d+)`)
)

func contextTooLongMessage(msg string) string {
	if m := contextMaxPromptRe.FindStringSubmatch(msg); len(m) == 3 {
		return fmt.Sprintf("上下文过长：当前约 %s tokens，模型上限 %s。请缩短对话历史、系统提示、工具输出或附件后重试", m[2], m[1])
	}
	if m := contextPromptGTRe.FindStringSubmatch(msg); len(m) == 3 {
		return fmt.Sprintf("上下文过长：当前约 %s tokens，模型上限 %s。请缩短对话历史、系统提示、工具输出或附件后重试", m[1], m[2])
	}
	return "上下文过长：请缩短对话历史、系统提示、工具输出或图片/附件后重试"
}

func looksLikeInvalidImage(hay string) bool {
	if !strings.Contains(hay, "image") {
		return false
	}
	return strings.Contains(hay, "image_url") ||
		strings.Contains(hay, "data url") ||
		(strings.Contains(hay, "base64") && strings.Contains(hay, "mime"))
}

func looksLikeCorruptSession(hay, msg string) bool {
	switch {
	case strings.Contains(hay, "invalid tool use"):
		return true
	case strings.Contains(hay, "no tool output found"):
		return true
	case strings.Contains(hay, "encrypted content") && strings.Contains(hay, "verified"):
		return true
	case strings.Contains(hay, "tool_use") && (strings.Contains(msg, "重复") || strings.Contains(hay, "duplicate")):
		return true
	case strings.Contains(msg, "无效") && strings.Contains(hay, "assistant"):
		return true
	default:
		return false
	}
}

func looksLikeUnsupportedRequest(hay, msg string) bool {
	switch {
	case strings.Contains(msg, "工具名称过长"):
		return true
	case strings.Contains(hay, "unsupported parameter"):
		return true
	case strings.Contains(hay, "unsupported model"):
		return true
	case strings.Contains(hay, "unsupported value"):
		return true
	case strings.Contains(hay, "is not supported when using"):
		return true
	case strings.Contains(hay, "当前不支持"):
		return true
	case strings.Contains(hay, "模型不支持"):
		return true
	case strings.Contains(hay, "input_text") && (strings.Contains(hay, "不支持") || strings.Contains(hay, "unsupported")):
		return true
	case strings.Contains(hay, "不支持 ") && (strings.Contains(hay, "effort") || strings.Contains(hay, "output_config") || strings.Contains(hay, "tool")):
		return true
	default:
		return false
	}
}

func unsupportedRequestMessage(msg string) string {
	if strings.Contains(msg, "工具名称过长") {
		return "工具名称过长，请缩短 MCP/工具名后重试"
	}
	return "当前请求包含不支持的参数或功能"
}

// extractReasoningEffort sniffs the request body for the thinking-
// strength value using the admin-configured rule list
// (SiteSettings.ReasoningParseRules); the built-in defaults cover
// OpenAI's `reasoning.effort` / `reasoning_effort` and Anthropic's
// `thinking.budget_tokens` (recorded verbatim as "thinking:<count>").
// Settings read failures degrade to those defaults rather than
// dropping the value.
func (g *Gateway) extractReasoningEffort(ctx context.Context, body map[string]any) string {
	rules := biz.DefaultReasoningParseRules()
	if g.settings != nil {
		if st, err := g.settings.GetSite(ctx); err == nil && st != nil && st.ReasoningParseRules != nil {
			rules = st.ReasoningParseRules
		}
	}
	return biz.ParseReasoningEffort(rules, body)
}

// reasoningEffortFromRaw is extractReasoningEffort for endpoints that
// keep the request as raw bytes (the Anthropic messages handler). Only
// top-level keys other than the potentially huge `messages` / `system`
// are decoded, so rule paths stay cheap to resolve on multi-MB bodies.
func (g *Gateway) reasoningEffortFromRaw(ctx context.Context, raw []byte) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	body := make(map[string]any, len(top))
	for k, v := range top {
		if k == "messages" || k == "system" {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err == nil {
			body[k] = val
		}
	}
	return g.extractReasoningEffort(ctx, body)
}

func keyName(k *ent.ApiKey) string {
	if k == nil {
		return ""
	}
	return k.Name
}

func apiKeyID(k *ent.ApiKey) *uuid.UUID {
	// k.ID == uuid.Nil is the synthetic principal minted for OAuth device
	// requests — it has no api_keys row, so api_key_id must stay NULL
	// (a non-nil Nil would violate the FK). Device attribution happens via
	// usage_logs.authorization_id instead.
	if k == nil || k.ID == uuid.Nil {
		return nil
	}
	id := k.ID
	return &id
}

func organizationID(k *ent.ApiKey) *uuid.UUID {
	if k == nil || k.OrganizationID == nil {
		return nil
	}
	id := *k.OrganizationID
	return &id
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
