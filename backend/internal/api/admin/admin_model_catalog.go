package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/shopspring/decimal"
)

// admin_model_catalog.go exposes the cached public model catalog
// (models.dev) and the bulk-import endpoint that binds catalog models to an
// account. The catalog is reference data — costs are display-only.

// SetRemoteCatalog wires the public-catalog service; nil hides the routes.
func (h *Handler) SetRemoteCatalog(svc *biz.RemoteCatalogSvc) { h.remoteCatalog = svc }

// listCatalogProviders returns every vendor in the cached public catalog.
func (h *Handler) listCatalogProviders(c *gin.Context) {
	if h.remoteCatalog == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "model catalog not configured"})
		return
	}
	providers, err := h.remoteCatalog.Providers(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// listCatalogModels returns the model list of one catalog provider.
func (h *Handler) listCatalogModels(c *gin.Context) {
	if h.remoteCatalog == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "model catalog not configured"})
		return
	}
	models, err := h.remoteCatalog.Models(c.Request.Context(), c.Param("pid"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"models": models})
}

// importModelsRequest is the bulk-bind body: catalog rows the admin picked,
// optionally with adjusted names/protocols.
type importModelsRequest struct {
	Models []struct {
		ModelName        string  `json:"model_name"`
		UpstreamModel    string  `json:"upstream_model"`
		UpstreamProtocol string  `json:"upstream_protocol"`
		RefInputCost     *string `json:"ref_input_cost"`
		RefOutputCost    *string `json:"ref_output_cost"`
	} `json:"models"`
}

// importAccountModels binds an explicit list of models to the account — the
// non-destructive counterpart of upstream sync for sources that carry richer
// metadata (the public catalog) or hand-picked ids.
func (h *Handler) importAccountModels(c *gin.Context) {
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req importModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "models is required"})
		return
	}
	models := make([]biz.ImportAccountModel, 0, len(req.Models))
	for _, m := range req.Models {
		in := biz.ImportAccountModel{
			ModelName:        m.ModelName,
			UpstreamModel:    m.UpstreamModel,
			UpstreamProtocol: m.UpstreamProtocol,
		}
		for _, pair := range []struct {
			src *string
			dst **decimal.Decimal
		}{
			{m.RefInputCost, &in.RefInputCost},
			{m.RefOutputCost, &in.RefOutputCost},
		} {
			if pair.src == nil || *pair.src == "" {
				continue
			}
			v, err := decimal.NewFromString(*pair.src)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ref cost for " + m.ModelName})
				return
			}
			*pair.dst = &v
		}
		models = append(models, in)
	}
	res, err := h.accounts.ImportAccountModels(c.Request.Context(), aid, models)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// importPlatformModelsRequest is the bulk-create body for platform Model rows.
// Prices arrive in storage units (per-1K points) exactly like createModel; the
// caller decides any currency conversion before sending — catalog costs are
// USD and are never implied as points here.
type importPlatformModelsRequest struct {
	Models []struct {
		Name                 string  `json:"name"`
		BillingMode          string  `json:"billing_mode"`
		Description          string  `json:"description"`
		Currency             string  `json:"currency"`
		Enabled              *bool   `json:"enabled"`
		PricePerRequest      *string `json:"price_per_request"`
		InputUnitPrice       *string `json:"input_unit_price"`
		OutputUnitPrice      *string `json:"output_unit_price"`
		CacheReadInputPrice  *string `json:"cache_read_input_price"`
		CacheWriteInputPrice *string `json:"cache_write_input_price"`
		// The catalog's window for the model; a starting default the
		// operator can then override per account.
		ContextWindow int `json:"context_window"`
	} `json:"models"`
}

// listCanonicalCatalogModels returns the model-centric catalog view: every
// provider's models merged into one entry per canonical name. This is the
// shape the platform-model directory actually works in — platform models are
// vendor-agnostic, so the picker should not force a per-provider read.
func (h *Handler) listCanonicalCatalogModels(c *gin.Context) {
	if h.remoteCatalog == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "model catalog not configured"})
		return
	}
	models, err := h.remoteCatalog.CanonicalModels(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"models": models})
}

// importPlatformModels bulk-creates platform models from the public catalog
// picker. Non-destructive: an existing name is reported as skipped, never
// overwritten — manual edits stay authoritative.
func (h *Handler) importPlatformModels(c *gin.Context) {
	var req importPlatformModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "models is required"})
		return
	}
	type failure struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	var addedNames []string
	var existing int
	var failed []failure
	for _, m := range req.Models {
		name := biz.CanonicalModelID(m.Name)
		if name == "" {
			failed = append(failed, failure{Name: m.Name, Error: "name is required"})
			continue
		}
		mode := m.BillingMode
		if mode == "" {
			mode = "per_token"
		}
		in := biz.CreateModelInput{
			Name: name, BillingMode: mode,
			Currency: m.Currency, Description: m.Description, Enabled: m.Enabled,
			ContextWindow: max(m.ContextWindow, 0),
		}
		var bad string
		for _, f := range []struct {
			src *string
			dst **decimal.Decimal
			key string
		}{
			{m.PricePerRequest, &in.PricePerRequest, "price_per_request"},
			{m.InputUnitPrice, &in.InputUnitPrice, "input_unit_price"},
			{m.OutputUnitPrice, &in.OutputUnitPrice, "output_unit_price"},
			{m.CacheReadInputPrice, &in.CacheReadInputPrice, "cache_read_input_price"},
			{m.CacheWriteInputPrice, &in.CacheWriteInputPrice, "cache_write_input_price"},
		} {
			v, msg := parseCreateDec(f.src, f.key)
			if msg != "" {
				bad = msg
				break
			}
			*f.dst = v
		}
		if bad != "" {
			failed = append(failed, failure{Name: name, Error: bad})
			continue
		}
		if _, err := h.models.CreateModel(c.Request.Context(), in); err != nil {
			if ent.IsConstraintError(err) {
				existing++
			} else {
				failed = append(failed, failure{Name: name, Error: err.Error()})
			}
			continue
		}
		addedNames = append(addedNames, name)
	}
	if len(addedNames) > 0 {
		h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	}
	c.JSON(http.StatusOK, gin.H{
		"added":        len(addedNames),
		"existing":     existing,
		"failed":       failed,
		"added_models": addedNames,
	})
}
