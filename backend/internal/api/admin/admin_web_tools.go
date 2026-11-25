package admin

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
)

// SetWebTools wires the web tool settings; nil hides the /web-tools routes.
func (h *Handler) SetWebTools(svc *biz.WebToolSvc) { h.webTools = svc }

func (h *Handler) mountWebTools(rg *gin.RouterGroup) {
	if h.webTools == nil {
		return
	}
	rg.GET("/web-tools", h.getWebTools)
	rg.PUT("/web-tools", h.updateWebTools)
}

// webToolsView is the admin page: every engine with its settings, the per-user
// rate limit, and how many accounts each vendor has.
type webToolsView struct {
	Engines  []webEngineView                  `json:"engines"`
	UserRPM  int                              `json:"user_rpm"`
	Accounts map[string]biz.WebVendorAccounts `json:"accounts"`
}

type webEngineView struct {
	biz.WebEngine
	Enabled bool   `json:"enabled"`
	Price   string `json:"price"`
}

func (h *Handler) webToolsView(c *gin.Context, settings biz.WebToolSettings) {
	accounts, err := h.webTools.VendorAccounts(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	view := webToolsView{UserRPM: settings.UserRPM, Accounts: accounts}
	for _, e := range biz.WebEngines {
		s := settings.Engines[e.ID]
		view.Engines = append(view.Engines, webEngineView{WebEngine: e, Enabled: s.Enabled, Price: s.Price.String()})
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handler) getWebTools(c *gin.Context) {
	settings, err := h.webTools.Settings(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.webToolsView(c, settings)
}

func (h *Handler) updateWebTools(c *gin.Context) {
	var in biz.WebToolSettings
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	settings, err := h.webTools.Update(c.Request.Context(), in)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, biz.ErrInvalidSiteSettings) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	h.webToolsView(c, settings)
}
