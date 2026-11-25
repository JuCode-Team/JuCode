package cs

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type resetUserPasswordReq struct {
	NewPassword string `json:"new_password"`
}

func (h *Handler) resetUserPassword(c *gin.Context) {
	uid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
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
	if err := h.account.ResetUserPassword(c.Request.Context(), uid, req.NewPassword); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "password reset; user sessions revoked"})
}
