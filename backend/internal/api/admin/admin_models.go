// admin_models.go: model CRUD. Sibling files own ModelGroup and
// ModelVendor — keeping the three split mirrors the biz layer's
// domain seams.
package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
)

// priceTierView mirrors schema.PriceTier with string-encoded prices so
// the admin UI round-trips decimals without float drift, matching the
// other price fields in modelView.
type priceTierView struct {
	UpToTokens             int64   `json:"up_to_tokens"`
	InputUnitPrice         *string `json:"input_unit_price,omitempty"`
	OutputUnitPrice        *string `json:"output_unit_price,omitempty"`
	CacheReadInputPrice    *string `json:"cache_read_input_price,omitempty"`
	CacheWriteInputPrice   *string `json:"cache_write_input_price,omitempty"`
	CacheWrite1hInputPrice *string `json:"cache_write_1h_input_price,omitempty"`
}

type modelView struct {
	ID                     string          `json:"id"`
	Name                   string          `json:"name"`
	BillingMode            string          `json:"billing_mode"`
	PricePerRequest        *string         `json:"price_per_request,omitempty"`
	InputUnitPrice         *string         `json:"input_unit_price,omitempty"`
	OutputUnitPrice        *string         `json:"output_unit_price,omitempty"`
	CacheReadInputPrice    *string         `json:"cache_read_input_price,omitempty"`
	CacheWriteInputPrice   *string         `json:"cache_write_input_price,omitempty"`
	CacheWrite1hInputPrice *string         `json:"cache_write_1h_input_price,omitempty"`
	ImageInputUnitPrice    *string         `json:"image_input_unit_price,omitempty"`
	ImageOutputUnitPrice   *string         `json:"image_output_unit_price,omitempty"`
	AudioInputUnitPrice    *string         `json:"audio_input_unit_price,omitempty"`
	AudioOutputUnitPrice   *string         `json:"audio_output_unit_price,omitempty"`
	OutputImagePrice       *string         `json:"output_image_price,omitempty"`
	OutputSecondPrice      *string         `json:"output_second_price,omitempty"`
	InputCharacterPrice    *string         `json:"input_character_price,omitempty"`
	PriceTiers             []priceTierView `json:"price_tiers,omitempty"`
	Currency               string          `json:"currency"`
	Enabled                bool            `json:"enabled"`
	Description            string          `json:"description,omitempty"`
	VendorID               *string         `json:"vendor_id,omitempty"`
	SortOrder              int             `json:"sort_order"`
	DisplayName            *string         `json:"display_name,omitempty"`
	PickerHidden           bool            `json:"picker_hidden"`
	ContextWindow          *int            `json:"context_window,omitempty"`
	MaxOutputTokens        *int            `json:"max_output_tokens,omitempty"`
	ReasoningEfforts       []string        `json:"reasoning_efforts,omitempty"`
	// Advertised* is what /v1/models reports over all of the model's groups:
	// the smallest and largest window among the accounts serving it.
	AdvertisedContextWindow    int `json:"advertised_context_window,omitempty"`
	AdvertisedMaxContextWindow int `json:"advertised_max_context_window,omitempty"`
}

func decString(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

func toPriceTierViews(tiers []entschema.PriceTier) []priceTierView {
	if len(tiers) == 0 {
		return nil
	}
	out := make([]priceTierView, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, priceTierView{
			UpToTokens:             t.UpToTokens,
			InputUnitPrice:         decString(t.InputUnitPrice),
			OutputUnitPrice:        decString(t.OutputUnitPrice),
			CacheReadInputPrice:    decString(t.CacheReadInputPrice),
			CacheWriteInputPrice:   decString(t.CacheWriteInputPrice),
			CacheWrite1hInputPrice: decString(t.CacheWrite1hInputPrice),
		})
	}
	return out
}

// parsePriceTiers converts the request-body tier list to the biz type.
// Returns an error message suitable for a 400 body on invalid decimals.
func parsePriceTiers(in []priceTierView) ([]entschema.PriceTier, string) {
	out := make([]entschema.PriceTier, 0, len(in))
	parse := func(s *string, field string) (*decimal.Decimal, string) {
		if s == nil || *s == "" {
			return nil, ""
		}
		d, err := decimal.NewFromString(*s)
		if err != nil {
			return nil, "invalid price_tiers." + field
		}
		return &d, ""
	}
	for _, t := range in {
		tier := entschema.PriceTier{UpToTokens: t.UpToTokens}
		var msg string
		if tier.InputUnitPrice, msg = parse(t.InputUnitPrice, "input_unit_price"); msg != "" {
			return nil, msg
		}
		if tier.OutputUnitPrice, msg = parse(t.OutputUnitPrice, "output_unit_price"); msg != "" {
			return nil, msg
		}
		if tier.CacheReadInputPrice, msg = parse(t.CacheReadInputPrice, "cache_read_input_price"); msg != "" {
			return nil, msg
		}
		if tier.CacheWriteInputPrice, msg = parse(t.CacheWriteInputPrice, "cache_write_input_price"); msg != "" {
			return nil, msg
		}
		if tier.CacheWrite1hInputPrice, msg = parse(t.CacheWrite1hInputPrice, "cache_write_1h_input_price"); msg != "" {
			return nil, msg
		}
		out = append(out, tier)
	}
	return out, ""
}

// parseCreateDec treats omit and "" as unset. Used on create so the UI
// can always send optional price fields without failing NewFromString("").
func parseCreateDec(s *string, field string) (*decimal.Decimal, string) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil, ""
	}
	d, err := decimal.NewFromString(strings.TrimSpace(*s))
	if err != nil {
		return nil, "invalid " + field
	}
	return &d, ""
}

// parseRequiredDec is for columns that cannot be cleared (text unit
// prices, price_per_request): omit = no change, "" = 400.
func parseRequiredDec(s *string, field string) (*decimal.Decimal, string) {
	if s == nil {
		return nil, ""
	}
	if strings.TrimSpace(*s) == "" {
		return nil, "empty " + field + " not allowed; omit the field to leave unchanged"
	}
	d, err := decimal.NewFromString(strings.TrimSpace(*s))
	if err != nil {
		return nil, "invalid " + field
	}
	return &d, ""
}

// parseOptionalPrice: omit = no change, "" = clear to NULL, number = set.
func parseOptionalPrice(s *string, field string) (biz.PriceOp, string) {
	if s == nil {
		return biz.PriceOp{}, ""
	}
	if strings.TrimSpace(*s) == "" {
		return biz.ClearPrice(), ""
	}
	d, err := decimal.NewFromString(strings.TrimSpace(*s))
	if err != nil {
		return biz.PriceOp{}, "invalid " + field
	}
	return biz.SetPrice(d), ""
}

func toModelView(m *ent.Model) modelView {
	v := modelView{
		ID: m.ID.String(), Name: m.Name,
		BillingMode: string(m.BillingMode),
		Currency:    m.Currency, Enabled: m.Enabled, Description: m.Description,
		SortOrder:     m.SortOrder,
		ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
		ReasoningEfforts: m.ReasoningEfforts, DisplayName: m.DisplayName,
		PickerHidden: m.PickerHidden,
	}
	if m.VendorID != nil {
		s := m.VendorID.String()
		v.VendorID = &s
	}
	if m.PricePerRequest != nil {
		s := m.PricePerRequest.String()
		v.PricePerRequest = &s
	}
	if m.InputUnitPrice != nil {
		s := m.InputUnitPrice.String()
		v.InputUnitPrice = &s
	}
	if m.OutputUnitPrice != nil {
		s := m.OutputUnitPrice.String()
		v.OutputUnitPrice = &s
	}
	if m.CacheReadInputPrice != nil {
		s := m.CacheReadInputPrice.String()
		v.CacheReadInputPrice = &s
	}
	if m.CacheWriteInputPrice != nil {
		s := m.CacheWriteInputPrice.String()
		v.CacheWriteInputPrice = &s
	}
	if m.CacheWrite1hInputPrice != nil {
		s := m.CacheWrite1hInputPrice.String()
		v.CacheWrite1hInputPrice = &s
	}
	if m.ImageInputUnitPrice != nil {
		s := m.ImageInputUnitPrice.String()
		v.ImageInputUnitPrice = &s
	}
	if m.ImageOutputUnitPrice != nil {
		s := m.ImageOutputUnitPrice.String()
		v.ImageOutputUnitPrice = &s
	}
	if m.AudioInputUnitPrice != nil {
		s := m.AudioInputUnitPrice.String()
		v.AudioInputUnitPrice = &s
	}
	if m.AudioOutputUnitPrice != nil {
		s := m.AudioOutputUnitPrice.String()
		v.AudioOutputUnitPrice = &s
	}
	if m.OutputImagePrice != nil {
		s := m.OutputImagePrice.String()
		v.OutputImagePrice = &s
	}
	if m.OutputSecondPrice != nil {
		s := m.OutputSecondPrice.String()
		v.OutputSecondPrice = &s
	}
	if m.InputCharacterPrice != nil {
		s := m.InputCharacterPrice.String()
		v.InputCharacterPrice = &s
	}
	v.PriceTiers = toPriceTierViews(m.PriceTiers)
	return v
}

func (h *Handler) listModels(c *gin.Context) {
	ms, err := h.models.ListModels(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		filtered := make([]*ent.Model, 0, len(ms))
		for _, m := range ms {
			if strings.Contains(strings.ToLower(m.Name), q) ||
				strings.Contains(strings.ToLower(m.Description), q) {
				filtered = append(filtered, m)
			}
		}
		ms = filtered
	}
	total := len(ms)
	limit, offset := parsePage(c, 500)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := ms[start:end]
	groups, err := h.models.LoadGroupsForModels(c.Request.Context(), page)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	caps, err := h.models.ModelCapabilities(c.Request.Context(), page, groups)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]modelView, 0, len(page))
	for _, m := range page {
		v := toModelView(m)
		v.AdvertisedContextWindow = caps[m.ID].ContextWindow
		v.AdvertisedMaxContextWindow = caps[m.ID].MaxContextWindow
		out = append(out, v)
	}
	c.JSON(http.StatusOK, gin.H{"models": out, "total": total, "limit": limit, "offset": offset})
}

type createModelReq struct {
	Name                   string          `json:"name" binding:"required"`
	BillingMode            string          `json:"billing_mode" binding:"required,oneof=per_request per_token"`
	PricePerRequest        *string         `json:"price_per_request"`
	InputUnitPrice         *string         `json:"input_unit_price"`
	OutputUnitPrice        *string         `json:"output_unit_price"`
	CacheReadInputPrice    *string         `json:"cache_read_input_price"`
	CacheWriteInputPrice   *string         `json:"cache_write_input_price"`
	CacheWrite1hInputPrice *string         `json:"cache_write_1h_input_price"`
	ImageInputUnitPrice    *string         `json:"image_input_unit_price"`
	ImageOutputUnitPrice   *string         `json:"image_output_unit_price"`
	AudioInputUnitPrice    *string         `json:"audio_input_unit_price"`
	AudioOutputUnitPrice   *string         `json:"audio_output_unit_price"`
	OutputImagePrice       *string         `json:"output_image_price"`
	OutputSecondPrice      *string         `json:"output_second_price"`
	InputCharacterPrice    *string         `json:"input_character_price"`
	PriceTiers             []priceTierView `json:"price_tiers"`
	Currency               string          `json:"currency"`
	Description            string          `json:"description"`
	VendorID               *string         `json:"vendor_id"`
	ContextWindow          int             `json:"context_window"`
	MaxOutputTokens        int             `json:"max_output_tokens"`
	ReasoningEfforts       []string        `json:"reasoning_efforts"`
	DisplayName            string          `json:"display_name"`
	PickerHidden           bool            `json:"picker_hidden"`
}

func (h *Handler) createModel(c *gin.Context) {
	var req createModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.CreateModelInput{
		Name: req.Name, BillingMode: req.BillingMode,
		Currency: req.Currency, Description: req.Description,
		ContextWindow: req.ContextWindow, MaxOutputTokens: req.MaxOutputTokens,
		ReasoningEfforts: req.ReasoningEfforts, DisplayName: req.DisplayName,
		PickerHidden: req.PickerHidden,
	}
	setCreateDec := func(dst **decimal.Decimal, src *string, field string) bool {
		d, msg := parseCreateDec(src, field)
		if msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return false
		}
		*dst = d
		return true
	}
	if !setCreateDec(&in.PricePerRequest, req.PricePerRequest, "price_per_request") ||
		!setCreateDec(&in.InputUnitPrice, req.InputUnitPrice, "input_unit_price") ||
		!setCreateDec(&in.OutputUnitPrice, req.OutputUnitPrice, "output_unit_price") ||
		!setCreateDec(&in.CacheReadInputPrice, req.CacheReadInputPrice, "cache_read_input_price") ||
		!setCreateDec(&in.CacheWriteInputPrice, req.CacheWriteInputPrice, "cache_write_input_price") ||
		!setCreateDec(&in.CacheWrite1hInputPrice, req.CacheWrite1hInputPrice, "cache_write_1h_input_price") ||
		!setCreateDec(&in.ImageInputUnitPrice, req.ImageInputUnitPrice, "image_input_unit_price") ||
		!setCreateDec(&in.ImageOutputUnitPrice, req.ImageOutputUnitPrice, "image_output_unit_price") ||
		!setCreateDec(&in.AudioInputUnitPrice, req.AudioInputUnitPrice, "audio_input_unit_price") ||
		!setCreateDec(&in.AudioOutputUnitPrice, req.AudioOutputUnitPrice, "audio_output_unit_price") ||
		!setCreateDec(&in.OutputImagePrice, req.OutputImagePrice, "output_image_price") ||
		!setCreateDec(&in.OutputSecondPrice, req.OutputSecondPrice, "output_second_price") ||
		!setCreateDec(&in.InputCharacterPrice, req.InputCharacterPrice, "input_character_price") {
		return
	}
	if req.VendorID != nil && *req.VendorID != "" {
		vid, err := uuid.Parse(*req.VendorID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid vendor_id"})
			return
		}
		in.VendorID = &vid
	}
	if len(req.PriceTiers) > 0 {
		tiers, msg := parsePriceTiers(req.PriceTiers)
		if msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		if _, err := biz.NormalizePriceTiers(tiers); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		in.PriceTiers = tiers
	}
	if req.ContextWindow < 0 || req.MaxOutputTokens < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "context_window and max_output_tokens must be >= 0"})
		return
	}
	m, err := h.models.CreateModel(c.Request.Context(), in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.JSON(http.StatusCreated, gin.H{"model": toModelView(m)})
}

func (h *Handler) deleteModel(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.models.DeleteModel(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

// reorderReq carries an ordered id list. It is a page's worth of ids, not the
// whole table — the service splices them back into the global ordering.
type reorderReq struct {
	IDs []string `json:"ids"`
}

// parseReorderIDs rejects the whole request on the first unparseable id rather
// than silently dropping it: a partially-applied reorder is worse than none,
// because the admin sees a different order than the one they dragged.
func parseReorderIDs(c *gin.Context) ([]uuid.UUID, bool) {
	var req reorderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return nil, false
	}
	ids := make([]uuid.UUID, 0, len(req.IDs))
	for _, raw := range req.IDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id: " + raw})
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

func (h *Handler) reorderModels(c *gin.Context) {
	ids, ok := parseReorderIDs(c)
	if !ok {
		return
	}
	if err := h.models.ReorderModels(c.Request.Context(), ids); err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

type updateModelReq struct {
	BillingMode     *string `json:"billing_mode"`
	PricePerRequest *string `json:"price_per_request"`
	InputUnitPrice  *string `json:"input_unit_price"`
	OutputUnitPrice *string `json:"output_unit_price"`
	// Optional prices: nil = no change; "" = clear to NULL (fall back
	// to the text unit price, or no surcharge); number = set.
	CacheReadInputPrice    *string `json:"cache_read_input_price"`
	CacheWriteInputPrice   *string `json:"cache_write_input_price"`
	CacheWrite1hInputPrice *string `json:"cache_write_1h_input_price"`
	ImageInputUnitPrice    *string `json:"image_input_unit_price"`
	ImageOutputUnitPrice   *string `json:"image_output_unit_price"`
	AudioInputUnitPrice    *string `json:"audio_input_unit_price"`
	AudioOutputUnitPrice   *string `json:"audio_output_unit_price"`
	OutputImagePrice       *string `json:"output_image_price"`
	OutputSecondPrice      *string `json:"output_second_price"`
	InputCharacterPrice    *string `json:"input_character_price"`
	// PriceTiers semantics: absent = no change; [] = clear back to flat
	// pricing; non-empty = replace the whole tier list.
	PriceTiers  *[]priceTierView `json:"price_tiers"`
	Currency    *string          `json:"currency"`
	Description *string          `json:"description"`
	Enabled     *bool            `json:"enabled"`
	// VendorID semantics: nil = no change. "" = clear. valid uuid = set.
	VendorID *string `json:"vendor_id"`
	// Capabilities: absent = no change; 0 / [] = clear (not configured).
	ContextWindow    *int      `json:"context_window"`
	MaxOutputTokens  *int      `json:"max_output_tokens"`
	ReasoningEfforts *[]string `json:"reasoning_efforts"`
	// DisplayName: absent = no change; "" = clear.
	DisplayName *string `json:"display_name"`
	// PickerHidden: absent = no change.
	PickerHidden *bool `json:"picker_hidden"`
}

func (h *Handler) updateModel(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.UpdateModelInput{
		BillingMode: req.BillingMode, Currency: req.Currency,
		Description: req.Description, Enabled: req.Enabled,
		ContextWindow: req.ContextWindow, MaxOutputTokens: req.MaxOutputTokens,
		ReasoningEfforts: req.ReasoningEfforts, DisplayName: req.DisplayName,
		PickerHidden: req.PickerHidden,
	}
	setRequired := func(dst **decimal.Decimal, src *string, field string) bool {
		d, msg := parseRequiredDec(src, field)
		if msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return false
		}
		*dst = d
		return true
	}
	setOptional := func(dst *biz.PriceOp, src *string, field string) bool {
		op, msg := parseOptionalPrice(src, field)
		if msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return false
		}
		*dst = op
		return true
	}
	if !setRequired(&in.PricePerRequest, req.PricePerRequest, "price_per_request") ||
		!setRequired(&in.InputUnitPrice, req.InputUnitPrice, "input_unit_price") ||
		!setRequired(&in.OutputUnitPrice, req.OutputUnitPrice, "output_unit_price") ||
		!setOptional(&in.CacheReadInputPrice, req.CacheReadInputPrice, "cache_read_input_price") ||
		!setOptional(&in.CacheWriteInputPrice, req.CacheWriteInputPrice, "cache_write_input_price") ||
		!setOptional(&in.CacheWrite1hInputPrice, req.CacheWrite1hInputPrice, "cache_write_1h_input_price") ||
		!setOptional(&in.ImageInputUnitPrice, req.ImageInputUnitPrice, "image_input_unit_price") ||
		!setOptional(&in.ImageOutputUnitPrice, req.ImageOutputUnitPrice, "image_output_unit_price") ||
		!setOptional(&in.AudioInputUnitPrice, req.AudioInputUnitPrice, "audio_input_unit_price") ||
		!setOptional(&in.AudioOutputUnitPrice, req.AudioOutputUnitPrice, "audio_output_unit_price") ||
		!setOptional(&in.OutputImagePrice, req.OutputImagePrice, "output_image_price") ||
		!setOptional(&in.OutputSecondPrice, req.OutputSecondPrice, "output_second_price") ||
		!setOptional(&in.InputCharacterPrice, req.InputCharacterPrice, "input_character_price") {
		return
	}
	if req.PriceTiers != nil {
		tiers, msg := parsePriceTiers(*req.PriceTiers)
		if msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		if _, err := biz.NormalizePriceTiers(tiers); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		in.PriceTiers = &tiers
	}
	if req.VendorID != nil {
		if *req.VendorID == "" {
			nilUUID := uuid.Nil
			in.VendorID = &nilUUID
		} else {
			vid, err := uuid.Parse(*req.VendorID)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid vendor_id"})
				return
			}
			in.VendorID = &vid
		}
	}
	if (req.ContextWindow != nil && *req.ContextWindow < 0) || (req.MaxOutputTokens != nil && *req.MaxOutputTokens < 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "context_window and max_output_tokens must be >= 0"})
		return
	}
	m, err := h.models.UpdateModel(c.Request.Context(), id, in)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.JSON(http.StatusOK, gin.H{"model": toModelView(m)})
}
