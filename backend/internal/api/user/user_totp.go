// user_totp.go: TOTP self-service under /v1/me/totp — status, setup
// (secret + otpauth URI for the QR), confirm (enables + returns recovery
// codes once), disable (password + code required).
package user

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) handleTOTPStatus(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":             u.TotpEnabled,
		"confirmed_at":        u.TotpConfirmedAt,
		"recovery_codes_left": len(u.TotpRecoveryCodes),
		"setup_required":      !u.TotpEnabled && h.account.TOTPRequiredForUser(c.Request.Context(), u),
	})
}

func (h *Handler) handleTOTPSetup(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	secret, uri, err := h.account.BeginTOTPSetup(c.Request.Context(), uid)
	if err != nil {
		if errors.Is(err, biz.ErrTOTPAlreadyEnabled) {
			c.JSON(http.StatusConflict, gin.H{"error": "totp already enabled", "code": "totp_already_enabled"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"secret": secret, "otpauth_url": uri})
}

type totpConfirmReq struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handler) handleTOTPConfirm(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req totpConfirmReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	codes, err := h.account.ConfirmTOTPSetup(c.Request.Context(), uid, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrTOTPAlreadyEnabled):
			c.JSON(http.StatusConflict, gin.H{"error": "totp already enabled", "code": "totp_already_enabled"})
		case errors.Is(err, biz.ErrTOTPNoPendingSetup):
			c.JSON(http.StatusBadRequest, gin.H{"error": "no pending setup, call setup first", "code": "totp_no_pending_setup"})
		case errors.Is(err, biz.ErrTOTPInvalidCode):
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid code", "code": "totp_invalid_code"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	// The only time the plaintext recovery codes leave the server.
	c.JSON(http.StatusOK, gin.H{"ok": true, "recovery_codes": codes})
}

type totpDisableReq struct {
	Password string `json:"password" binding:"required"`
	// Code accepts a current 6-digit TOTP code or an unused recovery code.
	Code string `json:"code" binding:"required"`
}

func (h *Handler) handleTOTPDisable(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req totpDisableReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.account.DisableTOTP(c.Request.Context(), uid, req.Password, req.Code); err != nil {
		switch {
		case errors.Is(err, biz.ErrTOTPNotEnabled):
			c.JSON(http.StatusBadRequest, gin.H{"error": "totp not enabled", "code": "totp_not_enabled"})
		case errors.Is(err, biz.ErrInvalidCredential):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "password incorrect"})
		case errors.Is(err, biz.ErrTOTPInvalidCode):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid code", "code": "totp_invalid_code"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
