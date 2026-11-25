package oauth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/middleware"
)

// MountUsageByRequest serves GET /usage-logs/by-request/:request_id: the
// exact cost of one gateway call, by the X-Request-Id it answered with.
// 404 until the usage row is written; the app retries a few times.
func MountUsageByRequest(rg *gin.RouterGroup, d *data.Data, limit gin.HandlerFunc) {
	rg.GET("/usage-logs/by-request/:request_id", limit, func(c *gin.Context) {
		uid, ok := middleware.UserIDFrom(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
			return
		}
		id := strings.TrimSpace(c.Param("request_id"))
		if id == "" || len(id) > 128 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request_id"})
			return
		}
		out, err := biz.UsageByRequest(c.Request.Context(), d, uid, id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if out == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, out)
	})
}
