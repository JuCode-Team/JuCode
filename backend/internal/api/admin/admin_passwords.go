package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type resetUserPasswordReq struct {
	NewPassword string `json:"new_password"`
}

func (h *Handler) resetUserPassword(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req resetUserPasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.NewPassword) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new password must be at least 8 characters"})
		return
	}
	if h.account == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
		return
	}
	if err := h.account.ResetUserPassword(c.Request.Context(), id, req.NewPassword); err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "password reset; user sessions revoked"})
}
