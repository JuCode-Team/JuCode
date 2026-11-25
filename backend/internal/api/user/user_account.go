// user_account.go: profile mutations that go through biz.Account
// (password & email change). Verification flow lives in biz.VerificationSvc.
package user

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (h *Handler) handleChangePassword(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req changePasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.NewPassword) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new password must be at least 8 characters"})
		return
	}
	if err := h.account.ChangePassword(c.Request.Context(), uid, req.OldPassword, req.NewPassword); err != nil {
		if errors.Is(err, biz.ErrInvalidCredential) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "old password incorrect"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "password changed; other sessions logged out"})
}

type changeEmailReq struct {
	NewEmail string `json:"new_email"`
}

func (h *Handler) handleSendEmailVerification(c *gin.Context) {
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
	if err := h.account.RequestEmailVerification(c.Request.Context(), u, c.ClientIP()); err != nil {
		switch {
		case errors.Is(err, biz.ErrEmailAlreadyVerified):
			c.JSON(http.StatusConflict, gin.H{"error": "邮箱已经验证。", "code": "email_already_verified"})
		case errors.Is(err, biz.ErrVerifyCodeTooFrequent):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "请稍后重新发送验证邮件。", "cooldown_seconds": 60})
		case errors.Is(err, biz.ErrEmailDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "邮件服务未配置，请联系管理员。"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "email": u.Email, "cooldown_seconds": 60})
}

func (h *Handler) handleChangeEmail(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req changeEmailReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.account.RequestEmailChange(c.Request.Context(), u, req.NewEmail, c.ClientIP()); err != nil {
		switch {
		case errors.Is(err, biz.ErrEmailExists):
			c.JSON(http.StatusConflict, gin.H{"error": "email already in use"})
		case errors.Is(err, biz.ErrEmailDisabled):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email feature disabled"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "verification email sent to new address"})
}
