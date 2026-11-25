package llm

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// isRetryableClientStatus reports whether an upstream 4xx is "channel-level"
// (auth, quota, not-found, timeout) — failing on this provider but possibly
// succeeding on another — versus "request-level" (400/413/422) that fails
// identically everywhere. Content-safety 403s are filtered by the caller
// before this is consulted, so a 403 reaching here is an auth/forbidden one.
func isRetryableClientStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, // 401
		http.StatusPaymentRequired, // 402
		http.StatusForbidden,       // 403 (non-cyber; caller pre-filters)
		http.StatusNotFound,        // 404
		http.StatusRequestTimeout:  // 408
		return true
	default:
		return false
	}
}

// generationShouldFailover reports whether a generation upstream status is
// the account rather than the request, so the gateway tries the next provider.
func generationShouldFailover(status int) bool {
	return status >= 500 || status == http.StatusTooManyRequests || isRetryableClientStatus(status)
}

func routeErrorMessage(model string, err error) string {
	switch {
	case errors.Is(err, biz.ErrModelNotInGroup):
		return "model " + model + " is not available for this API key"
	case errors.Is(err, biz.ErrNoProvider):
		return "model " + model + " is temporarily unavailable"
	default:
		return "request could not be routed"
	}
}

func gatewayRequestID(c *gin.Context) string {
	id := middleware.RequestIDFrom(c)
	if id == "" {
		id = c.GetHeader("CF-Ray")
		if id == "" {
			id = c.GetHeader("X-Request-Id")
		}
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(middleware.CtxRequestID, id)
	}
	c.Header("X-Request-Id", id)
	return id
}

func openAIError(kind, code, message string, requestID ...string) map[string]any {
	err := map[string]any{
		"type":    kind,
		"message": message,
	}
	if code != "" {
		err["code"] = code
	}
	out := map[string]any{"error": err}
	if len(requestID) > 0 && requestID[0] != "" {
		err["request_id"] = requestID[0]
		out["request_id"] = requestID[0]
	}
	return out
}

// setRetryAfter sets a client-facing Retry-After header, preferring the
// upstream's hint (h) and clamping to [1s, 60s] — Anthropic/OpenAI SDKs cap
// their own backoff around 60s, so a larger value is misleading.
// rateLimitRetryAfter is the hint sent with a gateway-imposed RPM/TPM 429.
// Half a window: long enough to be useful, short enough that a client backing
// off does not idle through a whole window it could have used.
const rateLimitRetryAfter = 30 * time.Second

func setRetryAfter(c *gin.Context, h http.Header, def time.Duration) {
	d := def
	if ra, ok := parseRetryAfter(h); ok {
		d = ra
	}
	if d < time.Second {
		d = time.Second
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	c.Header("Retry-After", strconv.Itoa(int(d.Seconds())))
}

func cyberSafetyMessage() string {
	return "request blocked by safety policy"
}

func upstreamRejectedMessage() string {
	return "request rejected by service"
}

// upstreamRejectedReply is the reply for a terminal upstream 4xx that no
// actionable matcher explained. A route gap in our own upstream (405 / "no
// upstream provider could serve") is answered as 502 with copy saying the
// request itself is fine; everything else keeps the masked generic rejection
// and the upstream status.
func upstreamRejectedReply(status int, body []byte) (httpStatus int, code, message string) {
	if biz.IsUpstreamRouteGap(status, string(body)) {
		return http.StatusBadGateway, "upstream_route_unsupported", biz.UpstreamRouteUnsupportedText
	}
	return status, "upstream_rejected", upstreamRejectedMessage()
}

func upstreamUnavailableMessage() string {
	return "service temporarily unavailable"
}

func upstreamTimeoutMessage() string {
	return "the model took too long to respond; please try again"
}

// upstreamStalledMessage is for an idle timeout: the stream started but then
// stopped producing tokens, distinct from never responding at all.
func upstreamStalledMessage() string {
	return "the model stopped responding mid-stream; please try again"
}

func streamInterruptedMessage() string {
	return "the response stream ended before completing; please try again"
}

func upstreamBusyMessage() string {
	return "the model is rate-limited right now; please retry shortly"
}

// upstreamUnreachableMessage covers transport/DNS/connection failures where
// there is no upstream HTTP status. The raw error embeds the internal
// upstream hostname, so it is never echoed to the client.
func upstreamUnreachableMessage() string {
	return "could not reach the model service; please try again"
}

// unsupportedProtocolMessage answers a request whose API format no candidate
// account speaks (and conversion is off for them). It names the endpoints the
// accounts do speak natively so the client can switch instead of retrying.
func unsupportedProtocolMessage(model, path string, accounts []biz.RouteAccount) string {
	var endpoints []string
	for _, rp := range accounts {
		for _, proto := range biz.AccountProtocols(rp.Account) {
			var ep string
			switch proto {
			case biz.NativeProtocolMessages:
				ep = "/anthropic/v1/messages"
			case biz.NativeProtocolChat:
				ep = "/v1/chat/completions"
			case biz.NativeProtocolResponses:
				ep = "/v1/responses"
			}
			if ep != "" && ep != path && !slices.Contains(endpoints, ep) {
				endpoints = append(endpoints, ep)
			}
		}
	}
	msg := "model " + model + " does not support the " + path + " API format in this group"
	if len(endpoints) > 0 {
		msg += "; use " + strings.Join(endpoints, " or ") + " instead"
	}
	return msg
}

// failureReason turns the internal outcome of a request that exhausted every
// provider — the last error, the last upstream HTTP status, and the last
// upstream error body — into a client-facing (code, message) that explains
// *why* the request failed. It surfaces timeouts, incomplete streams,
// unreachable upstreams, and recognizable/actionable upstream error bodies
// (input too long, a disabled feature, ...) instead of the old blanket
// "service temporarily unavailable", while never leaking raw transport
// errors (internal hostnames) or masked channel-level auth/quota bodies.
func failureReason(lastErr error, lastStatus int, upstreamBody string) (code, message string) {
	switch {
	case errors.Is(lastErr, errUpstreamFirstTextTimeout):
		return "first_text_timeout", upstreamTimeoutMessage()
	case errors.Is(lastErr, errUpstreamStreamTotalTimeout):
		return "stream_total_timeout", upstreamTimeoutMessage()
	case errors.Is(lastErr, errUpstreamStreamIdleTimeout):
		return "stream_idle_timeout", upstreamStalledMessage()
	case errors.Is(lastErr, context.DeadlineExceeded):
		return "upstream_timeout", upstreamTimeoutMessage()
	case errors.Is(lastErr, errUpstreamStreamIncomplete):
		return "stream_incomplete", streamInterruptedMessage()
	case errors.Is(lastErr, errNoUpstreamCapacity):
		// Every candidate was skipped for capacity/credential reasons: this is
		// backpressure, not an unreachable upstream.
		return "no_capacity", upstreamUnavailableMessage()
	}
	// A safety block reads the same everywhere: the per-attempt paths answer
	// 403 cyber_safety_block, so the exhausted-providers path must too rather
	// than bucketing it as a generic 4xx reject by status.
	if looksLikeCyberSecurityBlock([]byte(upstreamBody)) {
		return "cyber_safety_block", cyberSafetyMessage()
	}
	// A recognized/actionable upstream body wins over a status-only bucket:
	// a 403 "image generation is not enabled" or a 400 "context length
	// exceeded" should reach the user verbatim, not as a generic reject.
	if msg, c, ok := actionableUpstreamError([]byte(upstreamBody)); ok {
		return c, msg
	}
	switch {
	case lastStatus == http.StatusTooManyRequests:
		return "rate_limited", upstreamBusyMessage()
	case lastStatus >= 500:
		return "upstream_unavailable", upstreamUnavailableMessage()
	case lastStatus >= 400:
		_, code, message = upstreamRejectedReply(lastStatus, []byte(upstreamBody))
		return code, message
	default:
		// No upstream HTTP status → transport/DNS/connection failure.
		return "upstream_unreachable", upstreamUnreachableMessage()
	}
}

// isTimeoutErr reports whether lastErr is any of the gateway's stream/request
// timeout sentinels — the set that maps to a 504 Gateway Timeout.
func isTimeoutErr(lastErr error) bool {
	return errors.Is(lastErr, context.DeadlineExceeded) ||
		errors.Is(lastErr, errUpstreamFirstTextTimeout) ||
		errors.Is(lastErr, errUpstreamStreamIdleTimeout) ||
		errors.Is(lastErr, errUpstreamStreamTotalTimeout)
}

func outputPolicyMessage(_ string) string {
	return "blocked by output moderation policy"
}
