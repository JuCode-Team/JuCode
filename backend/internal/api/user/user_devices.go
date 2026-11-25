package user

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

// listDevices returns the caller's active OAuth device authorizations
// (授权设备管理). These are the CLI / Desktop installs that logged in via
// the OAuth flow — distinct from user-managed API keys.
func (h *Handler) listDevices(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	devices, err := h.oauth.ListDevices(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Over a device token, mark the caller's own device.
	current, _ := middleware.AuthzFrom(c)
	out := make([]gin.H, 0, len(devices))
	for _, d := range devices {
		v := toDeviceView(d)
		if current != nil && current.ID == d.ID {
			v["current"] = true
		}
		out = append(out, v)
	}
	c.JSON(http.StatusOK, gin.H{"devices": out})
}

func (h *Handler) revokeDevice(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid device id"})
		return
	}
	if err := h.oauth.RevokeDevice(c.Request.Context(), uid, id); err != nil {
		if errors.Is(err, biz.ErrOAuthAuthorizationNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"revoked": true})
}

func toDeviceView(d *ent.OAuthAuthorization) gin.H {
	v := gin.H{
		"id":              d.ID.String(),
		"client_id":       d.ClientID,
		"name":            d.Name,
		"scopes":          d.Scopes,
		"created_at":      d.CreatedAt,
		"last_ip":         d.LastIP,
		"last_user_agent": d.LastUserAgent,
	}
	if d.LastUsedAt != nil {
		v["last_used_at"] = d.LastUsedAt
	}
	return v
}

// MountReadOnly registers the read endpoints reachable with an OAuth
// device access token. They reuse the same handlers as /v1/me/* (which
// only read the user id from context), so the CLI/Desktop can query
// 套餐 / 用量 / 余额 / 调用详情 without a separate account JWT. Mounted under
// a group guarded by middleware.OAuthAccess.
func (h *Handler) MountReadOnly(rg *gin.RouterGroup) {
	rg.GET("/userinfo", h.handleMe)              // profile + balance + active plan
	rg.GET("/usage", h.handleUsage)              // plan quota (5h / weekly / monthly)
	rg.GET("/stats/usage", h.handleMyStatsUsage) // token/cost time series
	rg.GET("/usage-logs", h.handleMyUsageLogs)   // per-call details
}
