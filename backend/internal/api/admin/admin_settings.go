// admin_settings.go: read/write the singleton site_settings row that
// drives registration toggles, branding strings, etc.
package admin

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
)

func (h *Handler) getSiteSettings(c *gin.Context) {
	s, err := h.settings.GetSite(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

func (h *Handler) updateSiteSettings(c *gin.Context) {
	var in biz.SiteSettings
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.settings.UpdateSite(c.Request.Context(), in); err != nil {
		if errors.Is(err, biz.ErrInvalidSiteSettings) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Bust public caches so anonymous visitors see branding, registration,
	// and status-page config changes immediately.
	h.cache.Invalidate(c.Request.Context(), cache.KeySiteInfo, cache.KeyStatusSnapshot)
	out, _ := h.settings.GetSite(c.Request.Context())
	c.JSON(http.StatusOK, out)
}
