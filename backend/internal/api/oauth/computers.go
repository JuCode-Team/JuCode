package oauth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// ComputersHandler lists, renames and removes the account's computers
// (desktops reachable through the relay). Mounted behind
// middleware.OAuthAccess (/v1/oauth) and the console session (/v1/me).
type ComputersHandler struct {
	svc   *biz.ComputerSvc
	redis *redis.Client
}

func NewComputers(svc *biz.ComputerSvc, rdb *redis.Client) *ComputersHandler {
	return &ComputersHandler{svc: svc, redis: rdb}
}

// Clients poll the list every 30 s while it is visible.
const computersPerMinute = 120

func (h *ComputersHandler) Mount(rg *gin.RouterGroup) {
	limit := middleware.UserScopedRateLimit(h.redis, "computers", computersPerMinute, 60)
	rg.GET("/computers", limit, h.list)
	rg.PATCH("/computers/:host_id", limit, middleware.BodyLimit(4<<10), h.rename)
	rg.DELETE("/computers/:host_id", limit, h.remove)
}

func (h *ComputersHandler) list(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	list, err := h.svc.List(c.Request.Context(), uid)
	if err != nil {
		computerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"computers": list})
}

func (h *ComputersHandler) rename(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	var req struct {
		Name *string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	v, err := h.svc.Rename(c.Request.Context(), uid, c.Param("host_id"), *req.Name)
	if err != nil {
		computerError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}

func (h *ComputersHandler) remove(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	if err := h.svc.Delete(c.Request.Context(), uid, c.Param("host_id")); err != nil {
		computerError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func computerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrComputerInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, biz.ErrComputerNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
