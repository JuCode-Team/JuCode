package openapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
)

// GET /api/pricing — the new-api-compatible model catalog.
//
// ## Why the ratios look the way they do
//
// new-api prices a model as a *ratio*, not an amount: `model_ratio` 1 means
// "$2 per 1M input tokens", and `completion_ratio` multiplies that for
// output. JuCode instead stores an absolute price per 1K tokens in 积分
// (1 积分 = 1 元). The two are reconcilable because both sides are linear:
//
//	model_ratio = 元每1K × 1000 ÷ 2 = 元每1K × 500
//
// so a client that renders new-api's `model_ratio × 2` gets 元 per 1M tokens
// instead of 美元 per 1M tokens. The number is right; the currency label a
// hardcoded client prints beside it is not. That is unavoidable — the format
// has no currency field — so every response also carries the absolute prices
// under `jucode_pricing`, which is what an integration should actually read.
//
// ## Deliberate deviations from new-api
//
//   - `vendor_id` is absent and `vendor` (an object) takes its place.
//     new-api's vendor ids are small ints; JuCode's are UUIDs, and stuffing a
//     UUID into a field clients parse as an int is worse than omitting it.
//   - `owner_by` carries the vendor name. new-api declares the field but
//     never assigns it, so it is always "" upstream — anything we put there
//     is strictly more informative and cannot break a reader.
//   - `context_window`, `max_output_tokens`, `reasoning_efforts` are JuCode
//     extensions. new-api has no per-model capability metadata at all: not in
//     /api/pricing, not in /v1/models, and its Gemini-shaped listing returns
//     inputTokenLimit: null. Unknown fields are ignored by every client, so
//     adding them costs compatibility nothing.
//
// ## Scope
//
// This is the unauthenticated *catalog*: every enabled model, with the groups
// it belongs to in `enable_groups`. It is not a per-key entitlement list —
// what your key may actually call is GET /v1/models, which filters by the
// key's allowed groups. Keeping this caller-independent is what lets one
// cache entry serve everyone.
func (h *Handler) handlePricing(c *gin.Context) {
	payload, hit, err := cache.GetOrCompute[pricingPayload](c.Request.Context(), h.cache,
		cache.KeyPricingCompat, 5*time.Minute, h.buildPricing)
	setCacheHeader(c, hit)
	if err != nil {
		// new-api's /api/* envelope: HTTP 200, success=false. Clients switch
		// on the body, not the status.
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "pricing temporarily unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":            true,
		"data":               payload.Data,
		"vendors":            payload.Vendors,
		"group_ratio":        payload.GroupRatio,
		"usable_group":       payload.UsableGroup,
		"supported_endpoint": supportedEndpoints,
		"auto_groups":        []string{},
		"pricing_version":    payload.Version,
	})
}

// pricingPayload is the cached, fully-projected response body. Projecting
// before caching (rather than caching the ent rows) keeps the ratio math off
// the hot path and makes the cached value self-describing.
type pricingPayload struct {
	Data        []pricingEntry     `json:"data"`
	Vendors     []pricingVendor    `json:"vendors"`
	GroupRatio  map[string]float64 `json:"group_ratio"`
	UsableGroup map[string]string  `json:"usable_group"`
	Version     string             `json:"version"`
}

// pricingEntry mirrors new-api's model/pricing.go Pricing struct field-for-
// field, in declaration order, with the JuCode extensions appended at the end.
//
// The fields without omitempty are the ones new-api always emits; a client
// that does `data[0].completion_ratio` without a presence check must keep
// working, so they stay even when zero.
type pricingEntry struct {
	ModelName              string   `json:"model_name"`
	Description            string   `json:"description,omitempty"`
	QuotaType              int      `json:"quota_type"`
	ModelRatio             float64  `json:"model_ratio"`
	ModelPrice             float64  `json:"model_price"`
	OwnerBy                string   `json:"owner_by"`
	CompletionRatio        float64  `json:"completion_ratio"`
	CacheRatio             *float64 `json:"cache_ratio,omitempty"`
	CreateCacheRatio       *float64 `json:"create_cache_ratio,omitempty"`
	ImageRatio             *float64 `json:"image_ratio,omitempty"`
	EnableGroup            []string `json:"enable_groups"`
	SupportedEndpointTypes []string `json:"supported_endpoint_types"`
	BillingMode            string   `json:"billing_mode,omitempty"`

	// ---- JuCode extensions ----

	Vendor           *pricingVendor `json:"vendor,omitempty"`
	DisplayName      string         `json:"display_name,omitempty"`
	ContextWindow    int            `json:"context_window,omitempty"`
	MaxContextWindow int            `json:"max_context_window,omitempty"`
	MaxOutputTokens  int            `json:"max_output_tokens,omitempty"`
	ReasoningEfforts []string       `json:"reasoning_efforts,omitempty"`
	// JuCodePricing is the authoritative absolute price list, in 积分 per
	// 1K tokens (per request for quota_type 1). Read this, not the ratios,
	// if you are building something JuCode-specific.
	JuCodePricing map[string]string `json:"jucode_pricing,omitempty"`
	PriceTiers    []schemaPriceTier `json:"price_tiers,omitempty"`
}

// schemaPriceTier restates ent's PriceTier for the wire so the cached payload
// does not depend on the generated type's JSON tags.
type schemaPriceTier struct {
	UpToTokens             int64   `json:"up_to_tokens"`
	InputUnitPrice         *string `json:"input_unit_price,omitempty"`
	OutputUnitPrice        *string `json:"output_unit_price,omitempty"`
	CacheReadInputPrice    *string `json:"cache_read_input_price,omitempty"`
	CacheWriteInputPrice   *string `json:"cache_write_input_price,omitempty"`
	CacheWrite1hInputPrice *string `json:"cache_write_1h_input_price,omitempty"`
}

type pricingVendor struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
}

// supportedEndpoints maps endpoint type → where to POST it, mirroring
// new-api's /api/pricing "supported_endpoint" block. Type names reuse
// new-api's vocabulary where one exists so a client's switch statement is
// portable; the audio types have no upstream equivalent and are JuCode's own.
var supportedEndpoints = map[string]gin.H{
	"openai":                  {"path": "/v1/chat/completions", "method": "POST"},
	"openai-response":         {"path": "/v1/responses", "method": "POST"},
	"openai-response-compact": {"path": "/v1/responses/compact", "method": "POST"},
	"anthropic":               {"path": "/anthropic/v1/messages", "method": "POST"},
	"anthropic-messages":      {"path": "/v1/messages", "method": "POST"},
	"embeddings":              {"path": "/v1/embeddings", "method": "POST"},
	"image-generation":        {"path": "/v1/images/generations", "method": "POST"},
	"jina-rerank":             {"path": "/v1/rerank", "method": "POST"},
	"openai-video":            {"path": "/v1/videos", "method": "POST"},
	"audio-speech":            {"path": "/v1/audio/speech", "method": "POST"},
	"audio-transcription":     {"path": "/v1/audio/transcriptions", "method": "POST"},
}

// ratioPerUnit converts a JuCode unit price (积分 per 1K tokens) into a
// new-api model_ratio. See the handler comment for the derivation.
const ratioPerUnit = 500

func (h *Handler) buildPricing(ctx context.Context) (pricingPayload, error) {
	models, err := h.models.ListModels(ctx)
	if err != nil {
		return pricingPayload{}, err
	}
	enabled := make([]*ent.Model, 0, len(models))
	for _, m := range models {
		if m.Enabled {
			enabled = append(enabled, m)
		}
	}
	vendorByID, err := h.models.LoadVendors(ctx, enabled)
	if err != nil {
		return pricingPayload{}, err
	}
	groupsByModel, err := h.models.LoadGroupsForModels(ctx, enabled)
	if err != nil {
		return pricingPayload{}, err
	}
	groups, err := h.models.ListGroups(ctx)
	if err != nil {
		return pricingPayload{}, err
	}
	// Capabilities over the public catalog groups only — the same groups
	// each entry lists in enable_group.
	publicByModel := make(map[uuid.UUID][]*ent.ModelGroup, len(groupsByModel))
	for id, gs := range groupsByModel {
		publicByModel[id] = biz.PublicCatalogGroups(gs)
	}
	caps, err := h.models.ModelCapabilities(ctx, enabled, publicByModel)
	if err != nil {
		return pricingPayload{}, err
	}

	out := pricingPayload{
		Data:        make([]pricingEntry, 0, len(enabled)),
		Vendors:     make([]pricingVendor, 0, len(vendorByID)),
		GroupRatio:  map[string]float64{},
		UsableGroup: map[string]string{},
	}
	for _, g := range biz.PublicCatalogGroups(groups) {
		out.GroupRatio[g.Name] = g.RateMultiplier
		desc := g.Description
		if desc == "" {
			desc = g.Name
		}
		out.UsableGroup[g.Name] = desc
	}

	seenVendor := map[uuid.UUID]bool{}
	for _, m := range enabled {
		var vendor *pricingVendor
		if m.VendorID != nil {
			if v, ok := vendorByID[*m.VendorID]; ok {
				pv := pricingVendor{ID: v.ID.String(), Name: v.Name, Slug: v.Slug, Description: v.Description}
				vendor = &pv
				if !seenVendor[v.ID] {
					seenVendor[v.ID] = true
					out.Vendors = append(out.Vendors, pv)
				}
			}
		}
		out.Data = append(out.Data, buildPricingEntry(m, vendor, groupsByModel[m.ID], caps[m.ID]))
	}

	// Stable ordering. new-api's own array order is a Go map range and thus
	// nondeterministic; ordering here means the version hash below is a
	// function of the data rather than of the iteration.
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].ModelName < out.Data[j].ModelName })
	sort.Slice(out.Vendors, func(i, j int) bool { return out.Vendors[i].Name < out.Vendors[j].Name })

	// pricing_version lets a poller skip re-parsing an unchanged catalog.
	// Hashing the projected entries (not the DB rows) means it changes
	// exactly when the response body would.
	if raw, err := json.Marshal(out.Data); err == nil {
		sum := sha256.Sum256(raw)
		out.Version = hex.EncodeToString(sum[:16])
	}
	return out, nil
}

func buildPricingEntry(m *ent.Model, vendor *pricingVendor, groups []*ent.ModelGroup, meta biz.ModelCapabilities) pricingEntry {
	e := pricingEntry{
		ModelName:        m.Name,
		Description:      m.Description,
		OwnerBy:          "jucode",
		BillingMode:      string(m.BillingMode),
		Vendor:           vendor,
		ContextWindow:    meta.ContextWindow,
		MaxContextWindow: meta.MaxContextWindow,
		MaxOutputTokens:  meta.MaxOutputTokens,
		ReasoningEfforts: meta.ReasoningEfforts,
		EnableGroup:      make([]string, 0, len(groups)),
		JuCodePricing:    map[string]string{},
	}
	if m.DisplayName != nil {
		e.DisplayName = *m.DisplayName
	}
	if vendor != nil {
		e.OwnerBy = vendor.Name
	}
	for _, g := range biz.PublicCatalogGroups(groups) {
		e.EnableGroup = append(e.EnableGroup, g.Name)
	}
	sort.Strings(e.EnableGroup)

	// quota_type: new-api's 1 = charged per request, 0 = charged per token.
	if m.BillingMode == "per_request" {
		e.QuotaType = 1
		if m.PricePerRequest != nil {
			e.ModelPrice = m.PricePerRequest.InexactFloat64()
			e.JuCodePricing["price_per_request"] = m.PricePerRequest.String()
		}
	}

	input := decimal.Zero
	if m.InputUnitPrice != nil {
		input = *m.InputUnitPrice
		e.ModelRatio = input.Mul(decimal.NewFromInt(ratioPerUnit)).InexactFloat64()
		e.JuCodePricing["input_unit_price"] = input.String()
	}
	if m.OutputUnitPrice != nil {
		e.JuCodePricing["output_unit_price"] = m.OutputUnitPrice.String()
		e.CompletionRatio = relativeRatio(*m.OutputUnitPrice, input)
	}
	if m.CacheReadInputPrice != nil {
		e.JuCodePricing["cache_read_input_price"] = m.CacheReadInputPrice.String()
		e.CacheRatio = ratioPtr(*m.CacheReadInputPrice, input)
	}
	if m.CacheWriteInputPrice != nil {
		e.JuCodePricing["cache_write_input_price"] = m.CacheWriteInputPrice.String()
		e.CreateCacheRatio = ratioPtr(*m.CacheWriteInputPrice, input)
	}
	if m.CacheWrite1hInputPrice != nil {
		e.JuCodePricing["cache_write_1h_input_price"] = m.CacheWrite1hInputPrice.String()
	}
	if m.ImageInputUnitPrice != nil {
		e.JuCodePricing["image_input_unit_price"] = m.ImageInputUnitPrice.String()
		e.ImageRatio = ratioPtr(*m.ImageInputUnitPrice, input)
	}
	for k, v := range map[string]*decimal.Decimal{
		"image_output_unit_price": m.ImageOutputUnitPrice,
		"audio_input_unit_price":  m.AudioInputUnitPrice,
		"audio_output_unit_price": m.AudioOutputUnitPrice,
		"output_image_price":      m.OutputImagePrice,
		"output_second_price":     m.OutputSecondPrice,
		"input_character_price":   m.InputCharacterPrice,
	} {
		if v != nil {
			e.JuCodePricing[k] = v.String()
		}
	}
	for _, t := range m.PriceTiers {
		e.PriceTiers = append(e.PriceTiers, schemaPriceTier{
			UpToTokens:             t.UpToTokens,
			InputUnitPrice:         decPtrStr(t.InputUnitPrice),
			OutputUnitPrice:        decPtrStr(t.OutputUnitPrice),
			CacheReadInputPrice:    decPtrStr(t.CacheReadInputPrice),
			CacheWriteInputPrice:   decPtrStr(t.CacheWriteInputPrice),
			CacheWrite1hInputPrice: decPtrStr(t.CacheWrite1hInputPrice),
		})
	}
	e.SupportedEndpointTypes = supportedEndpointTypes(m)
	return e
}

// relativeRatio expresses price as a multiple of base, which is how new-api
// encodes every non-input price. A zero base makes the ratio undefined —
// return 0 (new-api's own default for "not configured") rather than dividing;
// jucode_pricing still carries the absolute number.
func relativeRatio(price, base decimal.Decimal) float64 {
	if base.IsZero() {
		return 0
	}
	return price.Div(base).InexactFloat64()
}

func ratioPtr(price, base decimal.Decimal) *float64 {
	if base.IsZero() {
		return nil
	}
	r := relativeRatio(price, base)
	return &r
}

// supportedEndpointTypes derives which endpoints a model can be called on.
//
// This is a heuristic, and it is the one field here that is not read from a
// column: JuCode has no per-model endpoint registry. What it does have is a
// price list, and prices are only configured for the modalities a model
// actually serves — a model with an output-per-second price is a video model,
// nothing else bills that way. Text is the fallback because the gateway
// genuinely accepts any model on all four text endpoints; it does not
// pre-validate the pairing.
func supportedEndpointTypes(m *ent.Model) []string {
	switch {
	case m.OutputSecondPrice != nil:
		return []string{"openai-video"}
	// ImageOutputUnitPrice used to be OR-ed in here, which was wrong: per
	// schema/model.go it prices the image tokens a *chat* model emits from its
	// built-in image_gen tool, so having it is evidence the model is a
	// multimodal chat model, not an image endpoint. It mislabelled the whole
	// gpt-5.x family (1M+ real calls on responses/chat_completions, 1 on
	// image_generation) and third-party consumers took the label at face value.
	//
	// Still imperfect the other way: gpt-image-2* price exactly like text
	// models, so they land in the text bucket. Only a real modality column on
	// Model can fix that direction — this heuristic cannot.
	case m.OutputImagePrice != nil:
		return []string{"image-generation"}
	case m.InputCharacterPrice != nil:
		return []string{"audio-speech"}
	}
	return []string{"openai", "openai-response", "openai-response-compact", "anthropic", "anthropic-messages"}
}

// setCacheHeader mirrors internal/api.setCacheHeader — X-Cache: HIT|MISS for
// the just-served response. Duplicated rather than exported because the two
// packages have no other reason to depend on each other.
func setCacheHeader(c *gin.Context, hit bool) {
	if hit {
		c.Header("X-Cache", "HIT")
	} else {
		c.Header("X-Cache", "MISS")
	}
}
