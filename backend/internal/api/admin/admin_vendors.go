// admin_vendors.go: ModelVendor CRUD. Vendors are the brand grouping
// (Anthropic, OpenAI, …) that fan out into Models. Small surface, but
// kept separate so the type definitions don't sprawl into admin_models.go.
package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
)

type vendorView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
	SortOrder   int    `json:"sort_order"`
}

func toVendorView(v *ent.ModelVendor) vendorView {
	return vendorView{
		ID: v.ID.String(), Name: v.Name, Slug: v.Slug,
		Description: v.Description, SortOrder: v.SortOrder,
	}
}

func (h *Handler) listVendors(c *gin.Context) {
	vs, err := h.models.ListVendors(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// In-handler search/paging: vendor count is small (< 100) so a full
	// load + slice is cheaper than refactoring the biz layer.
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		filtered := make([]*ent.ModelVendor, 0, len(vs))
		for _, v := range vs {
			if strings.Contains(strings.ToLower(v.Name), q) || strings.Contains(strings.ToLower(v.Slug), q) {
				filtered = append(filtered, v)
			}
		}
		vs = filtered
	}
	total := len(vs)
	limit, offset := parsePage(c, 500)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := vs[start:end]
	out := make([]vendorView, 0, len(page))
	for _, v := range page {
		out = append(out, toVendorView(v))
	}
	c.JSON(http.StatusOK, gin.H{"vendors": out, "total": total, "limit": limit, "offset": offset})
}

type createVendorReq struct {
	Name        string `json:"name" binding:"required"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	SortOrder   int    `json:"sort_order"`
}

func (h *Handler) createVendor(c *gin.Context) {
	var req createVendorReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	v, err := h.models.CreateVendor(c.Request.Context(), biz.CreateVendorInput{
		Name: req.Name, Slug: req.Slug, Description: req.Description, SortOrder: req.SortOrder,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Vendor changes appear in the public models list (vendor join), so
	// bust both keys.
	h.cache.Invalidate(c.Request.Context(), cache.KeyVendors, cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.JSON(http.StatusCreated, gin.H{"vendor": toVendorView(v)})
}

type updateVendorReq struct {
	Name        *string `json:"name"`
	Slug        *string `json:"slug"`
	Description *string `json:"description"`
	SortOrder   *int    `json:"sort_order"`
}

func (h *Handler) updateVendor(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateVendorReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	v, err := h.models.UpdateVendor(c.Request.Context(), id, biz.UpdateVendorInput{
		Name: req.Name, Slug: req.Slug, Description: req.Description, SortOrder: req.SortOrder,
	})
	if err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyVendors, cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.JSON(http.StatusOK, gin.H{"vendor": toVendorView(v)})
}

func (h *Handler) deleteVendor(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.models.DeleteVendor(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyVendors, cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}
