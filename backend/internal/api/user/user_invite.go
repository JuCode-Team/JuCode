// user_invite.go: GET /v1/me/invite — the self-service invite dashboard
// (code + URL, public program rules, stats, paged invitee/reward/rebate
// lists). The first call lazily allocates the user's invite code.
package user

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) handleMyInvite(c *gin.Context) {
	if h.invites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invite service not available"})
		return
	}
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	limit := 20
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	view, err := h.invites.MyInvite(c.Request.Context(), uid, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, view)
}
