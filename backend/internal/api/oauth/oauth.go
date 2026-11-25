// Package oauth implements the first-party OAuth flow for the configured
// clients (by default the JuCode CLI, Desktop and web app). It issues a
// short-lived access token plus a rotating refresh token bound to an
// oauth_authorizations (device) row, so the device can be listed and
// revoked, and usage attributed to it.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
	"github.com/jucode/jucode/internal/pkg/jwt"
)

const (
	codeTTL       = 5 * time.Minute
	codeKeyPrefix = "oauth:cli:code:"
)

// Clients are the first-party OAuth clients (config oauth.*). There is no
// dynamic client registration — this is not a general-purpose
// authorization server.
type Clients struct {
	// IDs are the accepted client ids. Every one may redirect to a loopback
	// address (the CLI/Desktop device flow).
	IDs []string
	// WebClientID is the one client that may also redirect to an exact entry
	// of WebRedirectURIs (the deployment's web app callbacks).
	WebClientID     string
	WebRedirectURIs []string
}

func (cl Clients) valid(clientID string) bool {
	return clientID != "" && slices.Contains(cl.IDs, clientID)
}

// allowedRedirect decides whether a client's redirect URI is usable:
// loopback always passes, and the web client also accepts its configured
// callbacks, matched exactly.
func (cl Clients) allowedRedirect(clientID, raw string) bool {
	if isLoopbackRedirect(raw) {
		return true
	}
	return clientID == cl.WebClientID && slices.Contains(cl.WebRedirectURIs, raw)
}

type Handler struct {
	redis   *redis.Client
	oauth   *biz.OAuthSvc
	signer  *jwt.Signer
	clients Clients
}

type codeRecord struct {
	UserID        string `json:"user_id"`
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	CodeChallenge string `json:"code_challenge"`
	CreatedAt     int64  `json:"created_at"`
}

type tokenReq struct {
	GrantType string `json:"grant_type" binding:"required"`
	ClientID  string `json:"client_id"`
	// authorization_code grant
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier"`
	DeviceName   string `json:"device_name"`
	// refresh_token grant
	RefreshToken string `json:"refresh_token"`
}

func New(redis *redis.Client, oauth *biz.OAuthSvc, signer *jwt.Signer, clients Clients) *Handler {
	return &Handler{redis: redis, oauth: oauth, signer: signer, clients: clients}
}

func (h *Handler) MountAuthorized(rg *gin.RouterGroup) {
	rg.POST("/authorize", h.authorize)
}

func (h *Handler) MountPublic(rg *gin.RouterGroup) {
	rg.POST("/token", h.token)
	// Public: limited per IP so it cannot be used to probe tokens in bulk.
	rg.POST("/revoke", middleware.IPScopedRateLimit(h.redis, "oauth_revoke", 20, 60), middleware.BodyLimit(4<<10), h.revoke)
}

// revoke implements RFC 7009 for refresh tokens: the device row the token
// belongs to is revoked. Unknown tokens still get 200 so the endpoint
// cannot be used to probe token validity.
func (h *Handler) revoke(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.RefreshToken) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refresh_token is required"})
		return
	}
	if err := h.oauth.RevokeByRefreshToken(c.Request.Context(), req.RefreshToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}

func (h *Handler) authorize(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}

	responseType := strings.TrimSpace(c.Query("response_type"))
	clientID := strings.TrimSpace(c.Query("client_id"))
	redirectURI := strings.TrimSpace(c.Query("redirect_uri"))
	codeChallenge := strings.TrimSpace(c.Query("code_challenge"))
	challengeMethod := strings.TrimSpace(c.Query("code_challenge_method"))
	state := strings.TrimSpace(c.Query("state"))
	if responseType != "code" || !h.clients.valid(clientID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported oauth request"})
		return
	}
	if challengeMethod != "S256" || codeChallenge == "" || state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pkce challenge and state are required"})
		return
	}
	if !h.clients.allowedRedirect(clientID, redirectURI) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "redirect_uri not allowed for client"})
		return
	}

	code, err := randomURLToken(32)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	record, err := json.Marshal(codeRecord{
		UserID:        uid.String(),
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		CodeChallenge: codeChallenge,
		CreatedAt:     time.Now().Unix(),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.redis.Set(c.Request.Context(), codeKeyPrefix+code, record, codeTTL).Err(); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to store oauth code"})
		return
	}

	u, _ := url.Parse(redirectURI)
	q := u.Query()
	q.Set("code", code)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	c.JSON(http.StatusOK, gin.H{"redirect_to": u.String(), "expires_in": int(codeTTL.Seconds())})
}

func (h *Handler) token(c *gin.Context) {
	var req tokenReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	switch req.GrantType {
	case "authorization_code":
		h.tokenAuthorizationCode(c, req)
	case "refresh_token":
		h.tokenRefresh(c, req)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported grant"})
	}
}

func (h *Handler) tokenAuthorizationCode(c *gin.Context, req tokenReq) {
	if !h.clients.valid(req.ClientID) || req.Code == "" || req.RedirectURI == "" || req.CodeVerifier == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid authorization_code request"})
		return
	}
	raw, err := h.redis.GetDel(c.Request.Context(), codeKeyPrefix+req.Code).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired code"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to load oauth code"})
		return
	}
	var record codeRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code"})
		return
	}
	if record.ClientID != req.ClientID {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "client_id mismatch"})
		return
	}
	if record.RedirectURI != req.RedirectURI || !h.clients.allowedRedirect(record.ClientID, req.RedirectURI) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "redirect_uri mismatch"})
		return
	}
	if pkceChallenge(req.CodeVerifier) != record.CodeChallenge {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code_verifier"})
		return
	}
	uid, err := uuid.Parse(record.UserID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code subject"})
		return
	}

	name := strings.TrimSpace(req.DeviceName)
	if name == "" {
		name = record.ClientID
	}
	_, bundle, err := h.oauth.CreateDevice(c.Request.Context(), uid, record.ClientID, name, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tokenResponse(bundle))
}

func (h *Handler) tokenRefresh(c *gin.Context, req tokenReq) {
	if strings.TrimSpace(req.RefreshToken) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refresh_token is required"})
		return
	}
	bundle, err := h.oauth.Refresh(c.Request.Context(), req.RefreshToken, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		if errors.Is(err, biz.ErrOAuthRefreshInvalid) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired refresh token"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tokenResponse(bundle))
}

func tokenResponse(b *biz.TokenBundle) gin.H {
	now := time.Now()
	return gin.H{
		"token_type":         "Bearer",
		"access_token":       b.AccessToken,
		"refresh_token":      b.RefreshToken,
		"expires_in":         int(time.Until(b.AccessExpiresAt).Seconds()),
		"refresh_expires_in": int(b.RefreshExpiresAt.Sub(now).Seconds()),
	}
}

func randomURLToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func isLoopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
