// admin_accounts.go: the account-pool admin surface — /v1/admin/accounts*.
//
// The "provider/channel" layer is gone as an entity. One ent.UpstreamAccount
// carries endpoint, dialect, credential, request shaping, scheduling, health
// and quota. This file owns account CRUD, per-account model bindings, the
// health actions, the explicit scope→account bindings, and the catalog
// browser the create wizard reads.
//
// Everything here is admin-only (JWT + AdminOnly + AdminAudit at the router
// level) and reachable from no client-facing route. Account identity is
// internal infrastructure: the API returns non-secret views only — a
// has_credential boolean, never the credential itself.
package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/schema"
	"github.com/jucode/jucode/internal/llm"
	"github.com/jucode/jucode/internal/middleware"
	"github.com/jucode/jucode/internal/providerkit/catalog"
)

// mountAccounts registers the account-pool routes. Nil service hides them.
func (h *Handler) mountAccounts(rg *gin.RouterGroup) {
	if h.accounts == nil {
		return
	}
	rg.GET("/accounts", h.listAccounts)
	rg.POST("/accounts", h.createAccount)
	rg.GET("/accounts/:id", h.getAccount)
	rg.PATCH("/accounts/:id", h.updateAccount)
	rg.DELETE("/accounts/:id", h.deleteAccount)

	rg.GET("/accounts/:id/models", h.listAccountModels)
	rg.POST("/accounts/:id/models", h.addAccountModel)
	rg.POST("/accounts/:id/models/sync", h.syncAccountModels)
	rg.POST("/accounts/:id/models/import", h.importAccountModels)
	rg.PATCH("/accounts/:id/models/:mid", h.updateAccountModel)
	rg.DELETE("/accounts/:id/models/:mid", h.removeAccountModel)
	rg.GET("/accounts/:id/usage", h.accountUsage)

	// Health / quota actions.
	rg.POST("/accounts/:id/recover", h.recoverAccount)
	rg.PUT("/accounts/:id/schedulable", h.setAccountSchedulable)
	rg.POST("/accounts/:id/cooldown", h.setAccountCooldown)
	rg.DELETE("/accounts/:id/cooldown", h.clearAccountCooldown)
	rg.POST("/accounts/:id/quota/refresh", h.refreshAccountQuota)
	rg.PUT("/accounts/:id/quota/policy", h.setAccountQuotaPolicy)
	rg.DELETE("/accounts/:id/model-cooldowns", h.clearAccountModelCooldowns)
	rg.POST("/accounts/:id/test", h.testAccount)

	// Catalog browser the wizard reads (read-only, account-independent).
	rg.GET("/provider-catalog", h.listProviderCatalog)
	rg.GET("/provider-catalog/:catalog_id", h.getProviderCatalogEntry)

	// Explicit scope → account pins. The old provider-scoped path is kept as
	// an alias for one release so the frontend can migrate on its own clock.
	rg.GET("/account-bindings", h.listAccountBindings)
	rg.POST("/account-bindings", h.createAccountBinding)
	rg.DELETE("/account-bindings/:bid", h.deleteAccountBinding)
	rg.GET("/provider-account-bindings", h.listAccountBindings)
	rg.POST("/provider-account-bindings", h.createAccountBinding)
	rg.DELETE("/provider-account-bindings/:bid", h.deleteAccountBinding)
}

// writeAccountError maps account-layer rejections to status codes.
func writeAccountError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrAccountNotFound), errors.Is(err, biz.ErrProviderNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, biz.ErrAccountBadInput),
		errors.Is(err, biz.ErrAccountNoCredential):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// --- catalog ---

type providerCatalogEntry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	LoginKind    string `json:"login_kind"`
	APIKeyFormat string `json:"api_key_format"`
	BaseURL      string `json:"base_url"`
	Protocol     string `json:"protocol"`
	DefaultModel string `json:"default_model,omitempty"`
	ModelCount   int    `json:"model_count"`
	Usable       bool   `json:"usable"`
}

// providerCatalogModel is one model of a catalog provider, with the dialect
// mapping resolved so the UI can tell which ones this gateway can serve.
type providerCatalogModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// API is the upstream wire-dialect id (e.g. "anthropic-messages").
	API string `json:"api"`
	// Protocol is the mapped protocol ("" = unsupported by this gateway).
	Protocol      string   `json:"protocol,omitempty"`
	Supported     bool     `json:"supported"`
	BaseURL       string   `json:"base_url,omitempty"`
	ContextWindow uint64   `json:"context_window"`
	MaxTokens     uint64   `json:"max_tokens"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input,omitempty"`
	// USD per 1M tokens (upstream published numbers, 0 = unknown).
	InputCost       float64 `json:"input_cost,omitempty"`
	OutputCost      float64 `json:"output_cost,omitempty"`
	CachedInputCost float64 `json:"cached_input_cost,omitempty"`
	CacheWriteCost  float64 `json:"cache_write_cost,omitempty"`
}

func catalogEntryView(cat *catalog.Catalog, p *catalog.AuthProvider) providerCatalogEntry {
	entry := providerCatalogEntry{
		ID:           p.ID,
		Name:         p.Name,
		LoginKind:    p.LoginKind(),
		APIKeyFormat: p.APIKeyFormat,
		BaseURL:      cat.DefaultBaseURL(p.ID),
		DefaultModel: cat.DefaultModel(p.ID),
		ModelCount:   len(cat.SupportedModels(p.ID)),
	}
	if entry.DefaultModel != "" {
		if proto, ok := cat.ProtocolFor(p.ID, entry.DefaultModel); ok {
			entry.Protocol = string(proto)
		}
	}
	// Usable must mean "the create path accepts this id". The catalog's own
	// ProviderUsable (any supported model) and biz.CatalogProviderDefaults
	// (the default model's dialect + a base URL) disagreed for a few entries,
	// which let the wizard offer a template that always failed on submit.
	_, _, _, ok := biz.CatalogProviderDefaults(p.ID)
	entry.Usable = ok
	return entry
}

// listProviderCatalog exposes the vendored provider catalog so the admin UI
// can offer "add account from template".
func (h *Handler) listProviderCatalog(c *gin.Context) {
	cat := catalog.MustLoad()
	loginKind := c.Query("login_kind")
	out := make([]providerCatalogEntry, 0, len(cat.AuthProviders()))
	for _, p := range cat.AuthProviders() {
		if loginKind != "" && p.LoginKind() != loginKind {
			continue
		}
		out = append(out, catalogEntryView(cat, p))
	}
	c.JSON(http.StatusOK, gin.H{"catalog_version": cat.Version(), "providers": out})
}

// getProviderCatalogEntry returns one catalog provider with its model list —
// what the create wizard needs to pre-select the account's model bindings.
func (h *Handler) getProviderCatalogEntry(c *gin.Context) {
	id := c.Param("catalog_id")
	cat := catalog.MustLoad()
	provider := cat.AuthProvider(id)
	if provider == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown catalog provider"})
		return
	}
	models := make([]providerCatalogModel, 0, len(cat.Models(id)))
	for _, m := range cat.Models(id) {
		view := providerCatalogModel{
			ID:              m.ID,
			Name:            m.Name,
			API:             m.API,
			BaseURL:         m.BaseURL,
			ContextWindow:   m.ContextWindow,
			MaxTokens:       m.MaxTokens,
			Reasoning:       m.Reasoning,
			Input:           m.Input,
			InputCost:       m.InputCost,
			OutputCost:      m.OutputCost,
			CachedInputCost: m.CachedInputCost,
			CacheWriteCost:  m.CacheWriteCost,
		}
		if proto, ok := cat.ProtocolFor(id, m.ID); ok {
			view.Protocol = string(proto)
			view.Supported = true
		}
		models = append(models, view)
	}
	c.JSON(http.StatusOK, gin.H{
		"catalog_version": cat.Version(),
		"provider":        catalogEntryView(cat, provider),
		"models":          models,
	})
}

// --- accounts ---

func (h *Handler) listAccounts(c *gin.Context) {
	in := biz.ListAccountsInput{
		Status:   strings.TrimSpace(c.Query("status")),
		Health:   strings.TrimSpace(c.Query("health")),
		Protocol: strings.TrimSpace(c.Query("protocol")),
		Search:   strings.TrimSpace(c.Query("q")),
	}
	if in.Search == "" {
		in.Search = strings.TrimSpace(c.Query("search"))
	}
	limit, offset := parsePage(c, 500)
	in.Limit, in.Offset = limit, offset
	if raw := strings.TrimSpace(c.Query("pool_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool_id"})
			return
		}
		in.PoolID = &id
	}
	if raw := strings.TrimSpace(c.Query("group_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group_id"})
			return
		}
		in.GroupID = &id
	}
	accounts, err := h.accounts.List(c.Request.Context(), in)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	views := make([]biz.AccountView, 0, len(accounts))
	for _, acct := range accounts {
		views = append(views, h.accounts.View(c.Request.Context(), acct))
	}
	c.JSON(http.StatusOK, gin.H{"accounts": views, "limit": limit, "offset": offset})
}

type createAccountRequest struct {
	Name string `json:"name"`
	// DefinitionID prefills the endpoint shape, protocols and model list from
	// a provider definition; explicit fields in this body still win.
	DefinitionID             string              `json:"definition_id"`
	BaseURL                  string              `json:"base_url"`
	APIFormat                string              `json:"api_format"`
	NativeProtocols          []string            `json:"native_protocols"`
	DefaultProtocol          string              `json:"default_protocol"`
	AllowConversion          bool                `json:"allow_conversion"`
	EndpointPaths            map[string]string   `json:"endpoint_paths"`
	HeaderRules              *schema.HeaderRules `json:"header_rules"`
	Organization             *string             `json:"organization"`
	BlockImageGenerationTool bool                `json:"block_image_generation_tool"`
	APIKey                   string              `json:"api_key"`
	Priority                 *int                `json:"priority"`
	Weight                   *int                `json:"weight"`
	MaxConcurrency           *int                `json:"max_concurrency"`
	Note                     string              `json:"note"`
	PoolMode                 string              `json:"pool_mode"`
	// LegacyProviderID is migration bookkeeping; normal creates leave it empty.
	LegacyProviderID string `json:"legacy_provider_id"`
}

func (h *Handler) createAccount(c *gin.Context) {
	var req createAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	in := biz.CreateAccountInput{
		Name:                     req.Name,
		DefinitionID:             strings.TrimSpace(req.DefinitionID),
		BaseURL:                  req.BaseURL,
		APIFormat:                req.APIFormat,
		NativeProtocols:          req.NativeProtocols,
		DefaultProtocol:          req.DefaultProtocol,
		AllowConversion:          req.AllowConversion,
		EndpointPaths:            req.EndpointPaths,
		HeaderRules:              req.HeaderRules,
		Organization:             req.Organization,
		BlockImageGenerationTool: req.BlockImageGenerationTool,
		ApiKey:                   req.APIKey,
		Priority:                 req.Priority,
		Weight:                   req.Weight,
		MaxConcurrency:           req.MaxConcurrency,
		Note:                     req.Note,
		PoolMode:                 req.PoolMode,
	}
	if raw := strings.TrimSpace(req.LegacyProviderID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid legacy_provider_id"})
			return
		}
		in.LegacyProviderID = &id
	}
	acct, err := h.accounts.Create(c.Request.Context(), in)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusCreated, h.accounts.View(c.Request.Context(), acct))
}

func (h *Handler) getAccount(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	acct, err := h.accounts.Get(c.Request.Context(), id)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.accounts.View(c.Request.Context(), acct))
}

type updateAccountRequest struct {
	Name           *string `json:"name"`
	ApiKey         *string `json:"api_key"`
	Status         *string `json:"status"`
	Priority       *int    `json:"priority"`
	Weight         *int    `json:"weight"`
	MaxConcurrency *int    `json:"max_concurrency"`
	Note           *string `json:"note"`
	// PoolMode overrides the cooldown/disable policy; "" inherits the site
	// default. Validated by the service.
	PoolMode *string `json:"pool_mode"`
	// Endpoint & dialect. Editable after creation: an upstream that moved, or
	// a declaration that was wrong, must not force the operator to rebuild an
	// account that traffic is pinned to.
	BaseURL         *string             `json:"base_url"`
	APIFormat       *string             `json:"api_format"`
	NativeProtocols *[]string           `json:"native_protocols"`
	DefaultProtocol *string             `json:"default_protocol"`
	AllowConversion *bool               `json:"allow_conversion"`
	EndpointPaths   *map[string]string  `json:"endpoint_paths"`
	HeaderRules     *schema.HeaderRules `json:"header_rules"`
	Organization    *string             `json:"organization"`
	// Request shaping.
	BlockImageGenerationTool   *bool `json:"block_image_generation_tool"`
	EstimateCacheWrite         *bool `json:"estimate_cache_write"`
	CacheWriteMinContextTokens *int  `json:"cache_write_min_context_tokens"`
}

func (h *Handler) updateAccount(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	// The audit row never carries the body, so the policy fields this PATCH
	// changed are recorded as before/after.
	before, _ := h.accounts.Get(c.Request.Context(), id)
	acct, err := h.accounts.Update(c.Request.Context(), id, biz.UpdateProviderAccountInput{
		Name:           req.Name,
		ApiKey:         req.ApiKey,
		Status:         req.Status,
		Priority:       req.Priority,
		Weight:         req.Weight,
		MaxConcurrency: req.MaxConcurrency,
		Note:           req.Note,
		PoolMode:       req.PoolMode,

		BaseURL:                    req.BaseURL,
		APIFormat:                  req.APIFormat,
		NativeProtocols:            req.NativeProtocols,
		DefaultProtocol:            req.DefaultProtocol,
		AllowConversion:            req.AllowConversion,
		EndpointPaths:              req.EndpointPaths,
		HeaderRules:                req.HeaderRules,
		Organization:               req.Organization,
		BlockImageGenerationTool:   req.BlockImageGenerationTool,
		EstimateCacheWrite:         req.EstimateCacheWrite,
		CacheWriteMinContextTokens: req.CacheWriteMinContextTokens,
	})
	if err != nil {
		writeAccountError(c, err)
		return
	}
	if before != nil {
		detail := map[string]any{}
		if before.PoolMode != acct.PoolMode {
			detail["pool_mode"] = map[string]any{"from": before.PoolMode, "to": acct.PoolMode}
		}
		if before.Status != acct.Status {
			detail["account_status"] = map[string]any{"from": string(before.Status), "to": string(acct.Status)}
		}
		if len(detail) > 0 {
			c.Set(middleware.CtxAuditDetail, detail)
		}
	}
	c.JSON(http.StatusOK, h.accounts.View(c.Request.Context(), acct))
}

func (h *Handler) deleteAccount(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.accounts.Delete(c.Request.Context(), id); err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// --- account models ---

type accountModelView struct {
	ID                    string `json:"id"`
	AccountID             string `json:"account_id"`
	ModelName             string `json:"model_name"`
	UpstreamModel         string `json:"upstream_model,omitempty"`
	UpstreamProtocol      string `json:"upstream_protocol,omitempty"`
	Enabled               bool   `json:"enabled"`
	CostMode              string `json:"cost_mode"`
	PricePerRequestCost   string `json:"price_per_request_cost,omitempty"`
	InputUnitCost         string `json:"input_unit_cost,omitempty"`
	OutputUnitCost        string `json:"output_unit_cost,omitempty"`
	CacheReadInputCost    string `json:"cache_read_input_cost,omitempty"`
	CacheWriteInputCost   string `json:"cache_write_input_cost,omitempty"`
	CacheWrite1hInputCost string `json:"cache_write_1h_input_cost,omitempty"`
	Currency              string `json:"currency"`
	// RefInputCost / RefOutputCost are display-only reference prices (USD
	// per 1M tokens) — never read by billing.
	RefInputCost  string `json:"ref_input_cost,omitempty"`
	RefOutputCost string `json:"ref_output_cost,omitempty"`
	ContextWindow *int   `json:"context_window,omitempty"`
}

// accountModelRequest is one model binding under an account. Cost fields are
// decimal strings so the wire format matches the rest of the admin surface.
type accountModelRequest struct {
	ModelName            string  `json:"model_name"`
	UpstreamModel        string  `json:"upstream_model"`
	UpstreamProtocol     string  `json:"upstream_protocol"`
	Enabled              *bool   `json:"enabled"`
	CostMode             string  `json:"cost_mode"`
	Currency             string  `json:"currency"`
	PricePerRequestCost  *string `json:"price_per_request_cost"`
	InputUnitCost        *string `json:"input_unit_cost"`
	OutputUnitCost       *string `json:"output_unit_cost"`
	CacheReadUnitCost    *string `json:"cache_read_unit_cost"`
	CacheWriteUnitCost   *string `json:"cache_write_unit_cost"`
	CacheWrite1hUnitCost *string `json:"cache_write_1h_unit_cost"`
	RefInputCost         *string `json:"ref_input_cost"`
	RefOutputCost        *string `json:"ref_output_cost"`
	// ContextWindow: absent = no change, 0 = inherit the model default.
	ContextWindow *int `json:"context_window"`
}

// accountModelInputFromReq parses and cross-validates an account model body.
// On bad input it writes a 400 and returns ok=false.
func accountModelInputFromReq(c *gin.Context, req accountModelRequest) (biz.AccountModelInput, bool) {
	parse := func(s *string, field string) (*decimal.Decimal, bool) {
		if s == nil || strings.TrimSpace(*s) == "" {
			return nil, true
		}
		v, err := decimal.NewFromString(strings.TrimSpace(*s))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + field})
			return nil, false
		}
		return &v, true
	}
	price, ok := parse(req.PricePerRequestCost, "price_per_request_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	input, ok := parse(req.InputUnitCost, "input_unit_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	output, ok := parse(req.OutputUnitCost, "output_unit_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	cacheRead, ok := parse(req.CacheReadUnitCost, "cache_read_unit_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	cacheWrite, ok := parse(req.CacheWriteUnitCost, "cache_write_unit_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	cacheWrite1h, ok := parse(req.CacheWrite1hUnitCost, "cache_write_1h_unit_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	refInput, ok := parse(req.RefInputCost, "ref_input_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	refOutput, ok := parse(req.RefOutputCost, "ref_output_cost")
	if !ok {
		return biz.AccountModelInput{}, false
	}
	if req.ContextWindow != nil && *req.ContextWindow < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "context_window must be >= 0"})
		return biz.AccountModelInput{}, false
	}
	switch req.CostMode {
	case "", "none":
		price, input, output, cacheRead, cacheWrite, cacheWrite1h = nil, nil, nil, nil, nil, nil
	case "per_request":
		if price == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "price_per_request_cost required for per_request cost_mode"})
			return biz.AccountModelInput{}, false
		}
		input, output, cacheRead, cacheWrite, cacheWrite1h = nil, nil, nil, nil, nil
	case "per_usage":
		if input == nil || output == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "input_unit_cost and output_unit_cost required for per_usage cost_mode"})
			return biz.AccountModelInput{}, false
		}
		price = nil
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "cost_mode must be one of none|per_request|per_usage"})
		return biz.AccountModelInput{}, false
	}
	return biz.AccountModelInput{
		ModelName:            strings.TrimSpace(req.ModelName),
		UpstreamModel:        strings.TrimSpace(req.UpstreamModel),
		UpstreamProtocol:     strings.TrimSpace(req.UpstreamProtocol),
		Enabled:              req.Enabled,
		CostMode:             req.CostMode,
		Currency:             req.Currency,
		PricePerRequestCost:  price,
		InputUnitCost:        input,
		OutputUnitCost:       output,
		CacheReadUnitCost:    cacheRead,
		CacheWriteUnitCost:   cacheWrite,
		CacheWrite1hUnitCost: cacheWrite1h,
		RefInputCost:         refInput,
		RefOutputCost:        refOutput,
		ContextWindow:        req.ContextWindow,
	}, true
}

func toAccountModelView(m *ent.UpstreamAccountModel) accountModelView {
	out := accountModelView{
		ID:               m.ID.String(),
		AccountID:        m.AccountID.String(),
		ModelName:        m.ModelName,
		UpstreamModel:    m.UpstreamModel,
		UpstreamProtocol: m.UpstreamProtocol,
		Enabled:          m.Enabled,
		CostMode:         string(m.CostMode),
		Currency:         m.Currency,
		ContextWindow:    m.ContextWindow,
	}
	if m.PricePerRequestCost != nil {
		out.PricePerRequestCost = m.PricePerRequestCost.String()
	}
	if m.InputUnitCost != nil {
		out.InputUnitCost = m.InputUnitCost.String()
	}
	if m.OutputUnitCost != nil {
		out.OutputUnitCost = m.OutputUnitCost.String()
	}
	if m.CacheReadInputCost != nil {
		out.CacheReadInputCost = m.CacheReadInputCost.String()
	}
	if m.CacheWriteInputCost != nil {
		out.CacheWriteInputCost = m.CacheWriteInputCost.String()
	}
	if m.CacheWrite1hInputCost != nil {
		out.CacheWrite1hInputCost = m.CacheWrite1hInputCost.String()
	}
	if m.RefInputCost != nil {
		out.RefInputCost = m.RefInputCost.String()
	}
	if m.RefOutputCost != nil {
		out.RefOutputCost = m.RefOutputCost.String()
	}
	return out
}

func (h *Handler) listAccountModels(c *gin.Context) {
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ms, err := h.accounts.ListAccountModels(c.Request.Context(), aid)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	out := make([]accountModelView, 0, len(ms))
	for _, m := range ms {
		out = append(out, toAccountModelView(m))
	}
	c.JSON(http.StatusOK, gin.H{"models": out})
}

// accountUsage aggregates the account's usage_logs per (model,
// upstream_model) for the queried window — per-model token counts, the
// account cost basis and what users were charged.
func (h *Handler) accountUsage(c *gin.Context) {
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	acct, err := h.accounts.Get(c.Request.Context(), aid)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	from, to := parseTimeRange(c)
	rows, err := h.finance.AccountModelUsage(c.Request.Context(), acct, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"model":                    r.Model,
			"upstream_model":           r.UpstreamModel,
			"requests":                 r.Requests,
			"tokens_in":                r.TokensIn,
			"tokens_out":               r.TokensOut,
			"cached_tokens_in":         r.CachedTokensIn,
			"cache_creation_tokens_in": r.CacheCreationTokensIn,
			"output_images":            r.OutputImages,
			"cost":                     r.Cost.String(),
			"revenue":                  r.Revenue.String(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"from": from, "to": to, "rows": out})
}

// syncAccountModels pulls the upstream /models catalog and binds any id the
// account does not already serve (see biz.SyncAccountModels).
func (h *Handler) syncAccountModels(c *gin.Context) {
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	res, err := h.accounts.SyncAccountModels(c.Request.Context(), aid)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) addAccountModel(c *gin.Context) {
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req accountModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in, ok := accountModelInputFromReq(c, req)
	if !ok {
		return
	}
	m, err := h.accounts.AddAccountModel(c.Request.Context(), aid, in)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	// The account's window feeds /api/pricing's context range.
	h.cache.Invalidate(c.Request.Context(), cache.KeyPricingCompat)
	c.JSON(http.StatusCreated, gin.H{"model": toAccountModelView(m)})
}

func (h *Handler) updateAccountModel(c *gin.Context) {
	mid, ok := parseUUID(c, "mid")
	if !ok {
		return
	}
	var req accountModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in, ok := accountModelInputFromReq(c, req)
	if !ok {
		return
	}
	m, err := h.accounts.UpdateAccountModel(c.Request.Context(), mid, in)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	// The account's window feeds /api/pricing's context range.
	h.cache.Invalidate(c.Request.Context(), cache.KeyPricingCompat)
	c.JSON(http.StatusOK, gin.H{"model": toAccountModelView(m)})
}

func (h *Handler) removeAccountModel(c *gin.Context) {
	mid, ok := parseUUID(c, "mid")
	if !ok {
		return
	}
	if err := h.accounts.RemoveAccountModel(c.Request.Context(), mid); err != nil {
		writeAccountError(c, err)
		return
	}
	// The account's window feeds /api/pricing's context range.
	h.cache.Invalidate(c.Request.Context(), cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

// --- health / quota ---

func (h *Handler) recoverAccount(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.accounts.RecoverAccountState(c.Request.Context(), id); err != nil {
		writeAccountError(c, err)
		return
	}
	h.respondAccountView(c, id)
}

type schedulableRequest struct {
	Schedulable *bool `json:"schedulable"`
}

// setAccountSchedulable parks or unpauses an account without touching its
// lifecycle status, so the UI can distinguish "paused by an operator" from
// "broken".
func (h *Handler) setAccountSchedulable(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req schedulableRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Schedulable == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "schedulable is required"})
		return
	}
	if err := h.accounts.SetSchedulable(c.Request.Context(), id, *req.Schedulable); err != nil {
		writeAccountError(c, err)
		return
	}
	c.Set(middleware.CtxAuditDetail, map[string]any{"schedulable": *req.Schedulable})
	h.respondAccountView(c, id)
}

// setAccountCooldown puts an account into runtime cooldown by hand: the
// operator knows the upstream is bad (or is draining it) and wants traffic off
// it without disabling it. The mark lands in the same Redis key the failure
// path writes, so routing skips the account exactly as it would after an
// upstream fault, and it expires on its own.
func (h *Handler) setAccountCooldown(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req setCooldownRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be valid JSON"})
		return
	}
	ttl, err := cooldownSeconds(req.Seconds, c.Query("seconds"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, err := h.accounts.Get(c.Request.Context(), id); err != nil {
		writeAccountError(c, err)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "manual"
	}
	if err := h.accounts.MarkCooldown(c.Request.Context(), id, reason, ttl); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.respondAccountView(c, id)
}

// clearAccountCooldown lifts a runtime cooldown early. It reports success
// whether or not one was active — "the account is not cooled now" is the
// postcondition the caller wants, and a 404 for a no-op would only make the UI
// race the TTL.
func (h *Handler) clearAccountCooldown(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.accounts.ClearCooldown(c.Request.Context(), id); err != nil {
		writeAccountError(c, err)
		return
	}
	h.respondAccountView(c, id)
}

// refreshAccountQuota asks the vendor for the account's remaining quota right
// now (the passive sampler fills the snapshot on ordinary traffic).
func (h *Handler) refreshAccountQuota(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	snapshot, err := h.accounts.RefreshQuota(c.Request.Context(), id)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	acct, err := h.accounts.Get(c.Request.Context(), id)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	view := h.accounts.View(c.Request.Context(), acct)
	view.QuotaSnapshot = snapshotMapForView(snapshot)
	c.JSON(http.StatusOK, view)
}

type quotaPolicyRequest struct {
	PauseAtPercent *float64 `json:"pause_at_percent"`
	Windows        []string `json:"windows"`
}

func (h *Handler) setAccountQuotaPolicy(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req quotaPolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.PauseAtPercent == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pause_at_percent is required"})
		return
	}
	if err := h.accounts.SetQuotaPolicy(c.Request.Context(), id, biz.QuotaPolicy{
		PauseAtPercent: *req.PauseAtPercent,
		Windows:        req.Windows,
	}); err != nil {
		writeAccountError(c, err)
		return
	}
	h.respondAccountView(c, id)
}

// testAccount runs one targeted health probe: this account, this protocol,
// this model, sent straight to the account's own upstream. The answer is a
// verdict, not a mutation — health state, cooldowns and billing are
// untouched, which is why an operator can fire it against a parked account
// to see whether it deserves a slot back.
func (h *Handler) testAccount(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var in struct {
		Protocol string `json:"protocol"`
		Model    string `json:"model"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	proto, ok := biz.ParseNativeProtocol(in.Protocol)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown protocol " + strconv.Quote(in.Protocol)})
		return
	}
	// A web tool account is probed with a real search or fetch; "model"
	// names which of the two ("search" / "fetch").
	if proto == biz.NativeProtocolWeb {
		acct, err := h.accounts.Get(c.Request.Context(), id)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		result, err := llm.ProbeWebAccount(c.Request.Context(), h.accounts, acct, strings.TrimSpace(in.Model))
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	acct, err := h.accounts.Get(c.Request.Context(), id)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	// A bound model may rewrite the name sent upstream; the probe sends what
	// the gateway would send, so a mapping typo shows up as the upstream's
	// own model-not-found instead of a fabricated success.
	if models, err := h.accounts.ListAccountModels(c.Request.Context(), id); err == nil {
		for _, m := range models {
			if m.ModelName == model && strings.TrimSpace(m.UpstreamModel) != "" {
				model = m.UpstreamModel
			}
		}
	}
	result, err := llm.ProbeAccount(c.Request.Context(), h.accounts, acct, proto, model)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// clearAccountModelCooldowns drops every model-scoped cooldown of an account.
func (h *Handler) clearAccountModelCooldowns(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.accounts.ClearModelCooldown(c.Request.Context(), id); err != nil {
		writeAccountError(c, err)
		return
	}
	h.respondAccountView(c, id)
}

// respondAccountView re-reads an account and writes its non-secret view.
func (h *Handler) respondAccountView(c *gin.Context, id uuid.UUID) {
	acct, err := h.accounts.Get(c.Request.Context(), id)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.accounts.View(c.Request.Context(), acct))
}

func snapshotMapForView(s *biz.QuotaSnapshot) map[string]any {
	if s == nil {
		return nil
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// --- bindings ---

func (h *Handler) listAccountBindings(c *gin.Context) {
	var accountID *uuid.UUID
	if raw := strings.TrimSpace(c.Query("account_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
			return
		}
		accountID = &id
	}
	rows, err := h.accounts.ListBindings(c.Request.Context(), accountID)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	views := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		view := gin.H{
			"id":         row.ID.String(),
			"scope_type": string(row.ScopeType),
			"scope_id":   row.ScopeID,
			"account_id": row.AccountID.String(),
			"note":       row.Note,
			"created_at": row.CreatedAt,
		}
		if row.ExpiresAt != nil {
			view["expires_at"] = row.ExpiresAt
		}
		views = append(views, view)
	}
	c.JSON(http.StatusOK, gin.H{"bindings": views})
}

type createAccountBindingRequest struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	AccountID string `json:"account_id"`
	ExpiresAt string `json:"expires_at"`
	Note      string `json:"note"`
}

func (h *Handler) createAccountBinding(c *gin.Context) {
	var req createAccountBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
		return
	}
	in := biz.CreateAccountBindingInput{
		ScopeType: req.ScopeType,
		ScopeID:   req.ScopeID,
		AccountID: accountID,
		CreatedBy: adminActor(c),
		Note:      req.Note,
	}
	if strings.TrimSpace(req.ExpiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expires_at must be RFC3339"})
			return
		}
		in.ExpiresAt = &parsed
	}
	row, err := h.accounts.CreateBinding(c.Request.Context(), in)
	if err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": row.ID.String()})
}

func (h *Handler) deleteAccountBinding(c *gin.Context) {
	bid, ok := parseUUID(c, "bid")
	if !ok {
		return
	}
	if err := h.accounts.DeleteBinding(c.Request.Context(), bid); err != nil {
		writeAccountError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// adminActor is the acting admin's user id when the auth middleware recorded
// one.
func adminActor(c *gin.Context) uuid.UUID {
	v, ok := c.Get(middleware.CtxUserID)
	if !ok {
		return uuid.Nil
	}
	switch id := v.(type) {
	case uuid.UUID:
		return id
	case string:
		if parsed, err := uuid.Parse(id); err == nil {
			return parsed
		}
	}
	return uuid.Nil
}
