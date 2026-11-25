// Package auth implements the /v1/auth/* HTTP routes.
package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

type Handler struct {
	acc *biz.Account
	// invites backs the pre-registration code check. Optional (setter, not
	// constructor arg): nil answers every code as unusable, which is also
	// what registration does when the service is absent.
	invites *biz.InviteSvc
}

func New(acc *biz.Account) *Handler { return &Handler{acc: acc} }

// SetInvites wires the invite service in after construction.
func (h *Handler) SetInvites(inv *biz.InviteSvc) { h.invites = inv }

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/send-register-verify-code", h.handleSendRegisterVerifyCode)
	rg.POST("/register", h.handleRegister)
	// Public so the register form can flag a bad code while the visitor is
	// still typing it, instead of failing the whole submit. The route group
	// already carries an IP rate limit.
	rg.GET("/invite-code/check", h.handleCheckInviteCode)
	rg.POST("/login", h.handleLogin)
	// Second login step for TOTP-enabled accounts: exchanges the
	// mfa_token from /login plus a 6-digit code (or a recovery code)
	// for the real token pair.
	rg.POST("/totp", h.handleTOTPLogin)
	rg.POST("/refresh", h.handleRefresh)
	rg.POST("/logout", h.handleLogout)
	// Public verification + reset endpoints — no auth required, the
	// token in the URL IS the proof of identity.
	rg.POST("/verify-email", h.handleVerifyEmail)
	rg.POST("/forgot-password", h.handleForgotPassword)
	rg.POST("/reset-password", h.handleResetPassword)
}

type registerReq struct {
	Email      string `json:"email" binding:"required,email"`
	Password   string `json:"password" binding:"required,min=8"`
	Nickname   string `json:"nickname"`
	VerifyCode string `json:"verify_code"`
	// InviteCode is optional, but a malformed or unknown one is rejected
	// rather than ignored — see handleRegister.
	InviteCode string `json:"invite_code"`
}

type sendRegisterVerifyCodeReq struct {
	Email string `json:"email" binding:"required,email"`
}

// loginReq accepts an email or a bare username under the same field
// name so the wire format stays backward-compatible with existing
// clients. Account.Login decides which by looking for '@'.
type loginReq struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type tokenResp struct {
	Access           string `json:"access_token"`
	AccessExpiresAt  int64  `json:"access_expires_at"`
	Refresh          string `json:"refresh_token"`
	RefreshExpiresAt int64  `json:"refresh_expires_at"`
	TokenType        string `json:"token_type"`
}

type userResp struct {
	ID       string  `json:"id"`
	Email    string  `json:"email"`
	Role     string  `json:"role"`
	Nickname *string `json:"nickname,omitempty"`
}

func (h *Handler) handleRegister(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.acc.Register(c.Request.Context(), biz.RegisterInput{
		Email:      req.Email,
		Password:   req.Password,
		Nickname:   req.Nickname,
		VerifyCode: req.VerifyCode,
		InviteCode: req.InviteCode,
		IP:         c.ClientIP(),
		UserAgent:  c.Request.UserAgent(),
	})
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrEmailExists):
			c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		case errors.Is(err, biz.ErrRegistrationDisabled):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "registration is currently disabled",
				"code":  "registration_disabled",
			})
		case errors.Is(err, biz.ErrEmailDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email verification is not available"})
		case errors.Is(err, biz.ErrInvalidVerifyCode):
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired verification code"})
		case errors.Is(err, biz.ErrInviteCodeMalformed):
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invite code is malformed",
				"code":  "invite_code_malformed",
			})
		case errors.Is(err, biz.ErrInviteCodeUnknown):
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invite code does not exist",
				"code":  "invite_code_unknown",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	if res.PendingApproval {
		// No tokens minted when the account is gated.
		msg := "registration submitted; awaiting administrator approval"
		c.JSON(http.StatusAccepted, gin.H{
			"user":             toUserResp(res.User),
			"pending_approval": res.PendingApproval,
			"message":          msg,
		})
		return
	}
	lr, err := h.acc.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil || lr.Tokens == nil {
		// A freshly registered account can't have TOTP yet, so a nil
		// token pair here is a genuine failure.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "registered but auto-login failed"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"user":   toUserResp(res.User),
		"tokens": toTokenResp(lr.Tokens),
	})
}

// handleCheckInviteCode answers whether a code would be accepted at
// registration. Always 200 — an unusable code is a legitimate answer to the
// question, not a failed request — so the register form can call it on every
// blur without special-casing. Empty codes report malformed.
func (h *Handler) handleCheckInviteCode(c *gin.Context) {
	if h.invites == nil {
		c.JSON(http.StatusOK, gin.H{"valid": false, "reason": "unavailable"})
		return
	}
	_, err := h.invites.ResolveInviter(c.Request.Context(), c.Query("code"))
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"valid": true})
	case errors.Is(err, biz.ErrInviteCodeMalformed):
		c.JSON(http.StatusOK, gin.H{"valid": false, "reason": "malformed"})
	case errors.Is(err, biz.ErrInviteCodeUnknown):
		c.JSON(http.StatusOK, gin.H{"valid": false, "reason": "unknown"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (h *Handler) handleSendRegisterVerifyCode(c *gin.Context) {
	var req sendRegisterVerifyCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.acc.SendRegisterVerifyCode(c.Request.Context(), req.Email, c.ClientIP()); err != nil {
		switch {
		case errors.Is(err, biz.ErrEmailExists):
			c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		case errors.Is(err, biz.ErrRegistrationDisabled):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "registration is currently disabled",
				"code":  "registration_disabled",
			})
		case errors.Is(err, biz.ErrRegisterVerificationDisabled):
			c.JSON(http.StatusBadRequest, gin.H{"error": "register email verification is not enabled"})
		case errors.Is(err, biz.ErrVerifyCodeTooFrequent):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "please wait before requesting a new verification code"})
		case errors.Is(err, biz.ErrEmailDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email verification is not available"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "cooldown_seconds": 60})
}

func (h *Handler) handleLogin(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	lr, err := h.acc.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrInvalidCredential):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
		case errors.Is(err, biz.ErrUserBanned):
			c.JSON(http.StatusForbidden, gin.H{"error": "account is banned", "code": "banned"})
		case errors.Is(err, biz.ErrUserPendingApproval):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "account is awaiting administrator approval",
				"code":  "pending_approval",
			})
		case errors.Is(err, biz.ErrUserRejected):
			c.JSON(http.StatusForbidden, gin.H{
				"error": "registration was rejected",
				"code":  "rejected",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	if lr.MFAToken != "" {
		// Password accepted; tokens withheld until the code arrives.
		// Deliberately no user object here — nothing about the account
		// leaks before the second factor.
		c.JSON(http.StatusOK, gin.H{
			"mfa_required": true,
			"mfa_token":    lr.MFAToken,
		})
		return
	}
	h.acc.RecordLogin(c.Request.Context(), lr.User, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"user":                toUserResp(lr.User),
		"tokens":              toTokenResp(lr.Tokens),
		"totp_setup_required": lr.TOTPSetupRequired,
	})
}

type totpLoginReq struct {
	MFAToken     string `json:"mfa_token" binding:"required"`
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

func (h *Handler) handleTOTPLogin(c *gin.Context) {
	var req totpLoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Code == "" && req.RecoveryCode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code or recovery_code required"})
		return
	}
	u, pair, err := h.acc.CompleteTOTPLogin(c.Request.Context(), req.MFAToken, req.Code, req.RecoveryCode)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrMFATicketInvalid):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "login expired, start over", "code": "mfa_ticket_invalid"})
		case errors.Is(err, biz.ErrTOTPInvalidCode):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code", "code": "totp_invalid_code"})
		case errors.Is(err, biz.ErrUserBanned):
			c.JSON(http.StatusForbidden, gin.H{"error": "account is banned", "code": "banned"})
		case errors.Is(err, biz.ErrUserPendingApproval):
			c.JSON(http.StatusForbidden, gin.H{"error": "account is awaiting administrator approval", "code": "pending_approval"})
		case errors.Is(err, biz.ErrUserRejected):
			c.JSON(http.StatusForbidden, gin.H{"error": "registration was rejected", "code": "rejected"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	h.acc.RecordLogin(c.Request.Context(), u, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"user":   toUserResp(u),
		"tokens": toTokenResp(pair),
	})
}

func (h *Handler) handleRefresh(c *gin.Context) {
	var req refreshReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pair, err := h.acc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": toTokenResp(pair)})
}

func (h *Handler) handleLogout(c *gin.Context) {
	var req refreshReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.acc.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func toTokenResp(p *biz.TokenPair) tokenResp {
	return tokenResp{
		Access:           p.Access,
		AccessExpiresAt:  p.AccessExpiresAt.Unix(),
		Refresh:          p.Refresh,
		RefreshExpiresAt: p.RefreshExpiresAt.Unix(),
		TokenType:        "Bearer",
	}
}

func toUserResp(u *ent.User) userResp {
	return userResp{
		ID:       u.ID.String(),
		Email:    u.Email,
		Role:     string(u.Role),
		Nickname: u.Nickname,
	}
}

// --- email verification + password reset ---

type verifyEmailReq struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handler) handleVerifyEmail(c *gin.Context) {
	var req verifyEmailReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	user, kind, err := h.acc.VerifyEmail(c.Request.Context(), req.Code)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrVerificationNotFound), errors.Is(err, biz.ErrVerificationExpired), errors.Is(err, biz.ErrVerificationUsed):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, biz.ErrEmailExists):
			c.JSON(http.StatusConflict, gin.H{"error": "email already in use"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "user": toUserResp(user), "kind": kind})
}

type forgotPasswordReq struct {
	Email string `json:"email" binding:"required,email"`
}

func (h *Handler) handleForgotPassword(c *gin.Context) {
	var req forgotPasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Always 204 — never disclose whether the email is registered.
	_ = h.acc.ForgotPassword(c.Request.Context(), req.Email, c.ClientIP())
	c.Status(http.StatusNoContent)
}

type resetPasswordReq struct {
	Code        string `json:"code" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8"`
}

func (h *Handler) handleResetPassword(c *gin.Context) {
	var req resetPasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.acc.ResetPasswordWithToken(c.Request.Context(), req.Code, req.NewPassword); err != nil {
		switch {
		case errors.Is(err, biz.ErrVerificationNotFound), errors.Is(err, biz.ErrVerificationExpired), errors.Is(err, biz.ErrVerificationUsed):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, biz.ErrEmailDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "password reset not available"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.Status(http.StatusNoContent)
}
