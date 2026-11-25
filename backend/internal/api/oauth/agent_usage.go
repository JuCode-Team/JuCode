package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// DeviceHandler serves the JuCode Desktop's agent usage and settings sync.
// Mounted behind middleware.OAuthAccess, so the token names both the user
// and the device (authorization) making the call.
type DeviceHandler struct {
	svc   *biz.AgentUsageSvc
	redis *redis.Client
}

func NewDevice(svc *biz.AgentUsageSvc, rdb *redis.Client) *DeviceHandler {
	return &DeviceHandler{svc: svc, redis: rdb}
}

// Per user and minute. A daemon uploads at most once a minute plus once per
// finished turn, and each computer syncs settings every few minutes; the caps
// leave room for several computers and stop a leaked token from hammering
// the summary queries or filling the tables.
const (
	agentUploadPerMinute   = 60
	agentReadPerMinute     = 60
	settingsPerMinute      = 60
	agentUploadMaxBytes    = 1 << 20
	settingsUploadMaxBytes = 512 << 10
)

func (h *DeviceHandler) Mount(rg *gin.RouterGroup) {
	upload := middleware.UserScopedRateLimit(h.redis, "agent_usage_upload", agentUploadPerMinute, 60)
	read := middleware.UserScopedRateLimit(h.redis, "agent_usage_read", agentReadPerMinute, 60)
	settings := middleware.UserScopedRateLimit(h.redis, "client_settings", settingsPerMinute, 60)
	rg.POST("/agent-usage", upload, middleware.BodyLimit(agentUploadMaxBytes), h.uploadAgentUsage)
	rg.GET("/agent-usage/summary", read, h.agentUsageSummary)
	rg.GET("/agent-usage/recent", read, h.recentAgentTurns)
	rg.POST("/agent-usage/charges", read, middleware.BodyLimit(agentUploadMaxBytes), h.agentTurnCharges)
	rg.GET("/settings", settings, h.getSettings)
	rg.PUT("/settings", settings, middleware.BodyLimit(settingsUploadMaxBytes), h.putSettings)
}

func (h *DeviceHandler) uploadAgentUsage(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	var req struct {
		Turns []biz.AgentTurnUpload `json:"turns"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	n, err := h.svc.UploadTurns(c.Request.Context(), uid, middleware.AuthzIDFrom(c), req.Turns)
	if err != nil {
		if errors.Is(err, biz.ErrAgentUsageInvalid) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accepted": n})
}

// intQuery parses an optional integer query parameter within [lo, hi].
func intQuery(c *gin.Context, name string, def, lo, hi int) (int, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo || v > hi {
		return 0, false
	}
	return v, true
}

func (h *DeviceHandler) agentUsageSummary(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	days, ok := intQuery(c, "days", 30, 1, 400)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "days must be 1..400"})
		return
	}
	// Date.getTimezoneOffset() spans UTC-12..UTC+14.
	tz, ok := intQuery(c, "tz_offset", 0, -14*60, 12*60)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tz_offset must be minutes in -840..720"})
		return
	}
	out, err := h.svc.Summary(c.Request.Context(), uid, days, tz)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *DeviceHandler) recentAgentTurns(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	limit, ok := intQuery(c, "limit", 20, 1, 100)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be 1..100"})
		return
	}
	items, err := h.svc.Recent(c.Request.Context(), uid, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *DeviceHandler) agentTurnCharges(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	var req struct {
		Turns []string `json:"turn_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	items, err := h.svc.Charges(c.Request.Context(), uid, req.Turns)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, biz.ErrAgentUsageInvalid) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "currency": "CNY"})
}

func (h *DeviceHandler) getSettings(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	out, err := h.svc.GetSettings(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": out})
}

func (h *DeviceHandler) putSettings(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	var req struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Settings == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "settings is required"})
		return
	}
	out, err := h.svc.PutSettings(c.Request.Context(), uid, req.Settings)
	if err != nil {
		if errors.Is(err, biz.ErrClientSettingsInvalid) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": out})
}
