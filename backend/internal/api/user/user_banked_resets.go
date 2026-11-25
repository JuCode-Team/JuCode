package user

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// useBankedReset spends one of the caller's banked resets, clearing the used
// quota of the window it was granted for.
func (h *Handler) useBankedReset(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid banked reset id"})
		return
	}
	if err := h.plans.UseBankedReset(c.Request.Context(), uid, id); err != nil {
		if errors.Is(err, biz.ErrBankedResetUnavailable) {
			c.JSON(http.StatusNotFound, gin.H{"error": "重置次数不存在、已使用或所属套餐已失效"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"used": true})
}
