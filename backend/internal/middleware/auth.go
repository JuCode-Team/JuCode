// Package middleware holds Gin middlewares for auth and request limits.
package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/pkg/jwt"
)

// Gin context keys used by all middlewares + handlers.
const (
	CtxUserID = "jucode.user_id"
	CtxRole   = "jucode.role"
	CtxUser   = "jucode.user"
	CtxApiKey = "jucode.api_key"
	// CtxAuthz holds the *ent.OAuthAuthorization (device) when the caller
	// authenticated via the OAuth device flow. Used to attribute usage to
	// a device (usage_logs.authorization_id).
	CtxAuthz = "jucode.authz"
	// CtxAccessTokenID holds the personal access token's id when the caller
	// authenticated with one instead of a session JWT.
	CtxAccessTokenID = "jucode.access_token_id"
	// CtxAuditDetail holds a map[string]any a handler adds to its request's
	// admin audit row (the body itself is never recorded).
	CtxAuditDetail = "jucode.audit_detail"
)

// StatusChecker reports whether userID's account is currently active.
// JWT() calls it after signature verification so a banned user can't
// keep using their valid-but-recently-revoked access token.
//
// Implementations should be cheap (cache hit, ~milliseconds). When the
// underlying lookup fails (e.g. transient DB error), returning
// (false, err) blocks the request; callers can choose to ignore err
// inside the checker if they want a fail-open posture.
type StatusChecker func(ctx context.Context, userID uuid.UUID) (active bool, err error)

// JWT enforces an Authorization: Bearer header carrying either a session
// access token or, when tokens is non-nil, a personal access token (jcp-…).
// On success it injects the user_id and role into the gin.Context; a
// personal access token also sets CtxAccessTokenID and takes the owner's
// current role rather than one frozen at issue time. When statusCheck is
// non-nil it also rejects requests from non-active users so an in-flight
// access token doesn't outlive a ban.
func JWT(signer *jwt.Signer, tokens *biz.AccessTokenSvc, statusCheck StatusChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := bearer(c.GetHeader("Authorization"))
		if raw == "" {
			respondUnauthorized(c, "missing bearer token")
			return
		}
		var userID uuid.UUID
		var role string
		if tokens != nil && biz.IsAccessToken(raw) {
			t, u, err := tokens.Resolve(c.Request.Context(), raw)
			if errors.Is(err, biz.ErrAccessTokenInvalid) {
				respondUnauthorized(c, "invalid access token")
				return
			}
			if err != nil {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "auth check failed"})
				return
			}
			// The owner was just loaded, so check their status here rather
			// than through the cached statusCheck.
			if u.Status != entuser.StatusActive {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "account is not active", "code": "account_disabled"})
				return
			}
			userID, role = u.ID, string(u.Role)
			c.Set(CtxAccessTokenID, t.ID)
		} else {
			claims, err := signer.Parse(raw)
			if err != nil || claims.Kind != jwt.KindAccess {
				respondUnauthorized(c, "invalid access token")
				return
			}
			userID, role = claims.UserID, claims.Role
		}
		if _, viaToken := c.Get(CtxAccessTokenID); !viaToken && statusCheck != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 500*time.Millisecond)
			active, cerr := statusCheck(ctx, userID)
			cancel()
			if cerr != nil {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "auth check failed"})
				return
			}
			if !active {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "account is not active", "code": "account_disabled"})
				return
			}
		}
		c.Set(CtxUserID, userID)
		c.Set(CtxRole, role)
		c.Next()
	}
}

// SessionOnly must run AFTER JWT(). It refuses personal access tokens on
// routes that manage credentials or account security, so a leaked token
// cannot mint more tokens or lock its owner out.
func SessionOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Get(CtxAccessTokenID); ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "this action requires a signed-in session, not an access token",
				"code":  "session_required",
			})
			return
		}
		c.Next()
	}
}

// AdminOnly must run AFTER JWT(). Rejects non-admin roles.
func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get(CtxRole)
		if role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin only"})
			return
		}
		c.Next()
	}
}

// RoleChecker is the slimmed interface RequireRoleAny needs from RoleSvc.
// Defined here (rather than imported) so this middleware file does not
// depend on the biz package, keeping the import graph one-way.
type RoleChecker interface {
	HasAny(ctx context.Context, userID uuid.UUID, roles ...string) (bool, error)
}

// RequireRoleAny must run AFTER JWT(). Admits requests whose user holds
// at least one of the listed functional roles (e.g. "cs", "agent"). An
// admin (primary role) always passes regardless of the list — admin is
// the universal override.
//
// Pass "admin" explicitly when you want a route that ONLY admins reach
// (use AdminOnly() for the legacy clean path); pass functional names
// like "cs" to gate the customer-service endpoints; pass several to
// permit any-of (e.g. RequireRoleAny(checker, "cs", "agent")).
func RequireRoleAny(checker RoleChecker, roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Fast path: primary role is admin → always allowed.
		if pr, _ := c.Get(CtxRole); pr == "admin" {
			c.Next()
			return
		}
		uid, ok := UserIDFrom(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 500*time.Millisecond)
		ok2, err := checker.HasAny(ctx, uid, roles...)
		cancel()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "role check failed"})
			return
		}
		if !ok2 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient role"})
			return
		}
		c.Next()
	}
}

// resolveApiKey resolves a sk-juc-* bearer, applies the account checks, sets
// CtxApiKey + CtxUser, fires the throttled last-used touch, and calls Next
// (or aborts). Shared by ApiKey() and ClientAuth().
func resolveApiKey(c *gin.Context, svc *biz.ApiKeySvc, raw string) {
	rec, u, err := svc.Resolve(c.Request.Context(), raw)
	if err != nil {
		if errors.Is(err, biz.ErrApiKeyExpired) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, openAIError(c, "expired", "api key expired"))
			return
		}
		respondAPIKeyUnauthorized(c, "invalid api key")
		return
	}
	if rec.System {
		c.AbortWithStatusJSON(http.StatusForbidden, openAIError(c, "lab_key", "实验室密钥不能用于外部调用"))
		return
	}
	if u.Status != "active" {
		c.AbortWithStatusJSON(http.StatusForbidden, openAIError(c, "account_disabled", "user is not active"))
		return
	}
	// Per-key IP whitelist. c.ClientIP() trusts forwarding headers only from
	// configured proxies, so a client-set header can't bypass it.
	if !biz.APIKeyIPAllowed(rec, c.ClientIP()) {
		c.AbortWithStatusJSON(http.StatusForbidden, openAIError(c, "ip_not_allowed", "client ip is not in this api key's ip whitelist"))
		return
	}
	// Reversible, time-boxed freeze applied by the automated sanction
	// engine. Lifts on its own at expiry — no unfreeze job needed.
	if u.FrozenUntil != nil && u.FrozenUntil.After(time.Now()) {
		c.AbortWithStatusJSON(http.StatusForbidden, openAIError(c, "account_frozen", "account temporarily frozen due to policy violations"))
		return
	}
	c.Set(CtxApiKey, rec)
	c.Set(CtxUser, u)
	c.Set(CtxUserID, u.ID)
	// Platform-owned keys (the status-page prober) tag their observations as
	// synthetic. Deliberately keyed off the stored flag rather than a header,
	// so a caller cannot opt its own traffic out of the availability metric.
	if rec.Internal {
		c.Request = c.Request.WithContext(biz.WithProbeTraffic(c.Request.Context()))
	}
	if rec.LastUsedAt != nil && time.Since(*rec.LastUsedAt) < 30*time.Second {
		c.Next()
		return
	}
	// fire-and-forget touch — must use a context that isn't tied
	// to the request, since the goroutine outlives the handler
	// (c.Request.Context() is cancelled the moment the response is
	// flushed, which would silently fail the DB write).
	go func(id uuid.UUID) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		svc.TouchLastUsed(ctx, id)
	}(rec.ID)
	c.Next()
}

// ClientAuth guards the LLM gateway routes. It accepts EITHER a classic
// sk-juc-* API key OR an OAuth device access token (kind=oauth_access).
// For the OAuth path it resolves the device → user, verifies the device
// isn't revoked and the account is active, then injects an in-memory
// principal (auto_route + personal billing — field-for-field identical to
// the key the CLI used to receive) as CtxApiKey so the routing/billing
// code path is unchanged. The device is recorded in CtxAuthz so usage is
// attributed to it (usage_logs.authorization_id) while api_key_id stays NULL.
func ClientAuth(apiKeySvc *biz.ApiKeySvc, oauthSvc *biz.OAuthSvc, signer *jwt.Signer) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := bearer(c.GetHeader("Authorization"))
		if raw == "" {
			raw = strings.TrimSpace(c.GetHeader("x-api-key"))
		}
		if raw == "" {
			raw = websocketCredential(c)
		}
		if raw == "" {
			respondAPIKeyUnauthorized(c, "missing credentials")
			return
		}
		if strings.HasPrefix(raw, "sk-juc-") {
			resolveApiKey(c, apiKeySvc, raw)
			return
		}
		// OAuth device access token.
		claims, err := signer.Parse(raw)
		if err != nil || claims.Kind != jwt.KindOAuthAccess {
			respondAPIKeyUnauthorized(c, "invalid token")
			return
		}
		authz, u, err := oauthSvc.ResolveAccess(c.Request.Context(), claims.AuthorizationID)
		if err != nil {
			respondAPIKeyUnauthorized(c, "device revoked or not found")
			return
		}
		if u.Status != "active" {
			c.AbortWithStatusJSON(http.StatusForbidden, openAIError(c, "account_disabled", "user is not active"))
			return
		}
		if u.FrozenUntil != nil && u.FrozenUntil.After(time.Now()) {
			c.AbortWithStatusJSON(http.StatusForbidden, openAIError(c, "account_frozen", "account temporarily frozen due to policy violations"))
			return
		}
		c.Set(CtxUser, u)
		c.Set(CtxUserID, u.ID)
		c.Set(CtxAuthz, authz)
		// Synthetic, non-persisted principal (ID stays uuid.Nil so usage_log
		// api_key_id is NULL). Mirrors the CLI's historical api key.
		c.Set(CtxApiKey, &ent.ApiKey{
			AutoRoute:       true,
			AllowedGroupIds: []uuid.UUID{},
			BillingScope:    entapikey.BillingScopePersonal,
			Name:            authz.Name,
		})
		if authz.LastUsedAt == nil || time.Since(*authz.LastUsedAt) > 30*time.Second {
			ip, ua := c.ClientIP(), c.Request.UserAgent()
			go func(id uuid.UUID) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				oauthSvc.TouchLastUsed(ctx, id, ip, ua)
			}(authz.ID)
		}
		c.Next()
	}
}

// OAuthAccess guards read endpoints reachable with an OAuth device access
// token (account / usage queries, the relay). It sets CtxUserID (so the
// existing /me read handlers work verbatim) and CtxAuthz, rejecting revoked
// devices and non-active accounts.
func OAuthAccess(oauthSvc *biz.OAuthSvc, signer *jwt.Signer) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := OAuthToken(c)
		if raw == "" {
			respondUnauthorized(c, "missing bearer token")
			return
		}
		claims, err := signer.Parse(raw)
		if err != nil || claims.Kind != jwt.KindOAuthAccess {
			respondUnauthorized(c, "invalid oauth access token")
			return
		}
		authz, u, err := oauthSvc.ResolveAccess(c.Request.Context(), claims.AuthorizationID)
		if errors.Is(err, biz.ErrOAuthAuthorizationNotFound) {
			respondUnauthorized(c, "device revoked or not found")
			return
		}
		if err != nil {
			// Not a verdict on the token (a slow database): clients retry
			// instead of treating the device as signed out.
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "cannot check the device right now"})
			return
		}
		if u.Status != "active" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "account is not active", "code": "account_disabled"})
			return
		}
		c.Set(CtxUserID, u.ID)
		c.Set(CtxUser, u)
		c.Set(CtxRole, string(u.Role))
		c.Set(CtxAuthz, authz)
		c.Next()
	}
}

// --- helpers ---

// OAuthToken is the bearer token OAuthAccess checks: the Authorization
// header, or on the relay's WebSocket routes the subprotocol
// "jucode-bearer.<token>", which browsers offer because they cannot set
// headers on a WebSocket.
func OAuthToken(c *gin.Context) string {
	if raw := bearer(c.GetHeader("Authorization")); raw != "" {
		return raw
	}
	if c.Request.Method != http.MethodGet || !strings.Contains(c.FullPath(), "/relay/") || !isWebSocketUpgrade(c.Request.Header) {
		return ""
	}
	for _, p := range strings.Split(c.GetHeader("Sec-WebSocket-Protocol"), ",") {
		if token, ok := strings.CutPrefix(strings.TrimSpace(p), "jucode-bearer."); ok {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

// websocketCredential pulls an API key from the places Realtime / Responses
// WebSocket clients actually send it: Sec-WebSocket-Protocol
// (openai-insecure-api-key.<key>) or ?api_key= on the handshake. Browsers
// cannot set headers on a WebSocket, hence the two side channels.
//
// The gate is the matched route, not the caller's headers: only a GET on a
// WebSocket endpoint may authenticate this way. Header-driven gating would let
// any request opt into query-string auth by claiming "Connection: upgrade",
// which leaks the key into access logs and proxy traces on routes that never
// upgrade.
func websocketCredential(c *gin.Context) string {
	if c.Request.Method != http.MethodGet || !websocketRoute(c.FullPath()) {
		return ""
	}
	if !isWebSocketUpgrade(c.Request.Header) {
		return ""
	}
	if q := strings.TrimSpace(c.Query("api_key")); q != "" {
		return q
	}
	for _, p := range strings.Split(c.GetHeader("Sec-WebSocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		// Case-folded to stay in step with llm.sanitizeUpstreamSubprotocols,
		// which strips this entry before the list is replayed upstream. An
		// entry accepted here but not stripped there would send the caller's
		// own key to the provider.
		const prefix = "openai-insecure-api-key."
		if len(p) > len(prefix) && strings.EqualFold(p[:len(prefix)], prefix) {
			return strings.TrimSpace(p[len(prefix):])
		}
	}
	return ""
}

// websocketRoute reports whether a matched route pattern is one of the
// WebSocket upgrade endpoints (see api/llm.MountGeneration). Compared on the
// suffix so the mount prefix is free to move.
func websocketRoute(pattern string) bool {
	return strings.HasSuffix(pattern, "/realtime") || strings.HasSuffix(pattern, "/responses")
}

// isWebSocketUpgrade mirrors websocket.IsWebSocketUpgrade: both handshake
// headers must be present, not either one.
func isWebSocketUpgrade(h http.Header) bool {
	return tokenListContains(h.Get("Connection"), "upgrade") &&
		tokenListContains(h.Get("Upgrade"), "websocket")
}

func tokenListContains(list, want string) bool {
	for _, tok := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(tok), want) {
			return true
		}
	}
	return false
}

func bearer(h string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func respondUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msg})
}

// OpenAI-style error envelope, used on the /v1/* gateway routes.
func respondAPIKeyUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, openAIError(c, "invalid_api_key", msg))
}

func openAIError(c *gin.Context, code, msg string) gin.H {
	requestID := RequestIDFrom(c)
	if requestID == "" {
		requestID = c.GetHeader("CF-Ray")
		if requestID == "" {
			requestID = c.GetHeader("X-Request-Id")
		}
		if requestID == "" {
			requestID = uuid.NewString()
		}
		c.Set(CtxRequestID, requestID)
	}
	c.Header("X-Request-Id", requestID)
	err := gin.H{"type": "authentication_error", "code": code, "message": msg}
	err["request_id"] = requestID
	return gin.H{"error": err, "request_id": requestID}
}

// Helpers for handlers to retrieve injected values without sprinkling
// type assertions everywhere.

func UserIDFrom(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(CtxUserID)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

func UserFrom(c *gin.Context) (*ent.User, bool) {
	v, ok := c.Get(CtxUser)
	if !ok {
		return nil, false
	}
	u, ok := v.(*ent.User)
	return u, ok
}

func ApiKeyFrom(c *gin.Context) (*ent.ApiKey, bool) {
	v, ok := c.Get(CtxApiKey)
	if !ok {
		return nil, false
	}
	k, ok := v.(*ent.ApiKey)
	return k, ok
}

func AuthzFrom(c *gin.Context) (*ent.OAuthAuthorization, bool) {
	v, ok := c.Get(CtxAuthz)
	if !ok {
		return nil, false
	}
	a, ok := v.(*ent.OAuthAuthorization)
	return a, ok
}

// AuthzIDFrom returns the authorizing device id, or nil when the request
// wasn't authenticated via the OAuth device flow. Used by the gateway to
// stamp usage_logs.authorization_id.
func AuthzIDFrom(c *gin.Context) *uuid.UUID {
	a, ok := AuthzFrom(c)
	if !ok {
		return nil
	}
	id := a.ID
	return &id
}
