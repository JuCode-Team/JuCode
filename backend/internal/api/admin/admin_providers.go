// admin_providers.go: the legacy read-only provider surface.
//
// The provider/channel entity no longer exists: accounts (ent.UpstreamAccount)
// carry endpoint, dialect, credential and scheduling. Only the two GET
// endpoints are kept for one release so the frontend can move to
// /v1/admin/accounts; they project accounts into the JSON shape the old
// provider handler returned (an account's legacy_provider_id is the old
// provider id). Every provider WRITE endpoint answers 410 Gone pointing at
// /v1/admin/accounts.
package admin

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

const providerPerformanceWindowSeconds = 3600

type providerPerformanceView struct {
	WindowSeconds int      `json:"window_seconds"`
	RequestCount  int      `json:"request_count"`
	SuccessCount  int      `json:"success_count"`
	FailureCount  int      `json:"failure_count"`
	FailureRate   float64  `json:"failure_rate"`
	TPS           *float64 `json:"tps,omitempty"`
	TTFTCount     int      `json:"ttft_count"`
	AvgTTFTMs     *int     `json:"avg_ttft_ms,omitempty"`
	P50TTFTMs     *int     `json:"p50_ttft_ms,omitempty"`
	P95TTFTMs     *int     `json:"p95_ttft_ms,omitempty"`
	P99TTFTMs     *int     `json:"p99_ttft_ms,omitempty"`
}

// providerView is the legacy provider wire shape. It is now populated from an
// account; the field names are frozen so the old frontend keeps working.
type providerView struct {
	ID                         string     `json:"id"`
	Name                       string     `json:"name"`
	Kind                       string     `json:"kind"`
	BaseURL                    string     `json:"base_url"`
	ApiFormat                  string     `json:"api_format,omitempty"`
	Protocols                  []string   `json:"protocols,omitempty"`
	Organization               *string    `json:"organization,omitempty"`
	Health                     string     `json:"health"`
	LastHealthAt               *time.Time `json:"last_health_at,omitempty"`
	Weight                     int        `json:"weight"`
	Enabled                    bool       `json:"enabled"`
	BlockImageGenerationTool   bool       `json:"block_image_generation_tool"`
	EstimateCacheWrite         bool       `json:"estimate_cache_write"`
	CacheWriteMinContextTokens int        `json:"cache_write_min_context_tokens"`
	Note                       string     `json:"note,omitempty"`
	APIKeySet                  bool       `json:"api_key_set"`
	AnthropicAPIKeySet         bool       `json:"anthropic_api_key_set"`
	SupportedModelCount        int        `json:"supported_model_count"`
	// The account this legacy provider row now maps to.
	LegacyProviderID string                   `json:"legacy_provider_id,omitempty"`
	AccountID        string                   `json:"account_id,omitempty"`
	AccountCount     int                      `json:"account_count"`
	Performance      *providerPerformanceView `json:"performance,omitempty"`
	CreatedAt        time.Time                `json:"created_at"`
}

// accountLegacyKind derives the legacy "kind" the old provider table used.
func accountLegacyKind(acct *ent.UpstreamAccount) string {
	if biz.AccountHasNativeProtocol(acct, biz.NativeProtocolMessages) {
		return "anthropic"
	}
	return "openai_compatible"
}

// accountLegacyProtocols maps native protocols onto the legacy protocol
// vocabulary the old frontend understands.
func accountLegacyProtocols(acct *ent.UpstreamAccount) []string {
	out := make([]string, 0, 2)
	for _, proto := range biz.AccountProtocols(acct) {
		legacy := "openai"
		if proto == biz.NativeProtocolMessages {
			legacy = "anthropic"
		}
		out = append(out, legacy)
	}
	return out
}

func toProviderViewFromAccount(acct *ent.UpstreamAccount, supportedModelCount int) providerView {
	id := acct.ID.String()
	legacyID := ""
	if acct.LegacyProviderID != nil && *acct.LegacyProviderID != uuid.Nil {
		legacyID = acct.LegacyProviderID.String()
		id = legacyID
	}
	view := providerView{
		ID:                         id,
		Name:                       acct.Name,
		Kind:                       accountLegacyKind(acct),
		BaseURL:                    acct.BaseURL,
		ApiFormat:                  acct.APIFormat,
		Protocols:                  accountLegacyProtocols(acct),
		Organization:               acct.Organization,
		Health:                     string(acct.Health),
		Weight:                     acct.Weight,
		Enabled:                    acct.Status == entaccount.StatusActive && acct.Schedulable,
		BlockImageGenerationTool:   acct.BlockImageGenerationTool,
		EstimateCacheWrite:         acct.EstimateCacheWrite,
		CacheWriteMinContextTokens: acct.CacheWriteMinContextTokens,
		Note:                       acct.Note,
		APIKeySet:                  strings.TrimSpace(acct.CredentialEnc) != "",
		SupportedModelCount:        supportedModelCount,
		LegacyProviderID:           legacyID,
		AccountID:                  acct.ID.String(),
		AccountCount:               1,
		CreatedAt:                  acct.CreatedAt,
	}
	return view
}

// findAccountByLegacyProvider resolves the account whose legacy_provider_id is
// the old provider id.
func (h *Handler) findAccountByLegacyProvider(ctx context.Context, id uuid.UUID) (*ent.UpstreamAccount, error) {
	if h.users == nil {
		return nil, biz.ErrAccountNotFound
	}
	acct, err := h.users.UpstreamAccount.Query().
		Where(entaccount.LegacyProviderID(id), entaccount.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, biz.ErrAccountNotFound
		}
		return nil, err
	}
	return acct, nil
}

func (h *Handler) legacyProviderViews(ctx context.Context, accts []*ent.UpstreamAccount) ([]providerView, error) {
	keys := make([]string, 0, len(accts))
	for _, acct := range accts {
		if acct.LegacyProviderID != nil && *acct.LegacyProviderID != uuid.Nil {
			keys = append(keys, acct.LegacyProviderID.String())
		} else {
			keys = append(keys, acct.ID.String())
		}
	}
	perf, err := h.loadProviderPerformance(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make([]providerView, 0, len(accts))
	for _, acct := range accts {
		count := 0
		if ms, merr := h.accounts.ListAccountModels(ctx, acct.ID); merr == nil {
			for _, m := range ms {
				if m.Enabled {
					count++
				}
			}
		}
		v := toProviderViewFromAccount(acct, count)
		v.Performance = perf[v.ID]
		out = append(out, v)
	}
	return out, nil
}

// listLegacyProviders projects the live accounts into the old providers array.
// An account that carries a legacy_provider_id reports under that old id; one
// created after the migration reports under its own account id.
func (h *Handler) listLegacyProviders(c *gin.Context) {
	if h.accounts == nil || h.users == nil {
		c.JSON(http.StatusOK, gin.H{"providers": []providerView{}, "total": 0, "limit": 0, "offset": 0})
		return
	}
	ctx := c.Request.Context()
	accts, err := h.users.UpstreamAccount.Query().
		Where(entaccount.DeletedAtIsNil()).
		Order(ent.Asc(entaccount.FieldPriority), ent.Asc(entaccount.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if q := strings.ToLower(strings.TrimSpace(c.Query("q"))); q != "" {
		filtered := make([]*ent.UpstreamAccount, 0, len(accts))
		for _, a := range accts {
			if strings.Contains(strings.ToLower(a.Name), q) ||
				strings.Contains(strings.ToLower(a.BaseURL), q) ||
				strings.Contains(strings.ToLower(a.Note), q) {
				filtered = append(filtered, a)
			}
		}
		accts = filtered
	}
	total := len(accts)
	limit, offset := parsePage(c, 500)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	views, err := h.legacyProviderViews(ctx, accts[start:end])
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": views, "total": total, "limit": limit, "offset": offset})
}

// getLegacyProvider resolves one old provider id: first as an account's
// legacy_provider_id, then as the account's own id.
func (h *Handler) getLegacyProvider(c *gin.Context) {
	if h.accounts == nil || h.users == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	acct, err := h.findAccountByLegacyProvider(ctx, id)
	if err != nil {
		// Fall back to the account's own id (an account created after the
		// migration has no legacy link).
		if a2, e2 := h.users.UpstreamAccount.Get(ctx, id); e2 == nil && a2.DeletedAt == nil {
			acct = a2
		} else {
			notFoundOr(c, err)
			return
		}
	}
	views, err := h.legacyProviderViews(ctx, []*ent.UpstreamAccount{acct})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"provider": views[0]})
}

// providerGone answers an old provider write endpoint with 410 Gone.
func providerGone(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{
		"error":    "the provider API has been removed; use the account API instead",
		"moved_to": "/v1/admin/accounts",
	})
}

// mountProviderWriteGone registers every removed provider write route (plus
// the read-only model/account/oauth sub-resources that moved under accounts) so
// callers get an actionable 410 rather than a 404.
func (h *Handler) mountProviderWriteGone(rg *gin.RouterGroup) {
	rg.POST("/providers", providerGone)
	rg.PATCH("/providers/:id", providerGone)
	rg.DELETE("/providers/:id", providerGone)

	rg.GET("/providers/:id/models", providerGone)
	rg.POST("/providers/:id/models", providerGone)
	rg.PATCH("/providers/:id/models/:mid", providerGone)
	rg.DELETE("/providers/:id/models/:mid", providerGone)

	rg.GET("/providers/:id/accounts", providerGone)
	rg.POST("/providers/:id/accounts", providerGone)
	rg.PATCH("/providers/:id/accounts/:aid", providerGone)
	rg.DELETE("/providers/:id/accounts/:aid", providerGone)
	rg.POST("/providers/:id/accounts/:aid/recover", providerGone)
	rg.PUT("/providers/:id/accounts/:aid/schedulable", providerGone)
	rg.DELETE("/providers/:id/accounts/:aid/cooldown", providerGone)
	rg.POST("/providers/:id/accounts/:aid/quota/refresh", providerGone)

	rg.POST("/providers/:id/cooldown", providerGone)
	rg.DELETE("/providers/:id/cooldown", providerGone)
}

// providerPerformanceTTL bounds how stale the provider table's latency
// numbers may be. They summarize a rolling hour, so half a minute of lag is
// invisible, and it collapses a page refresh loop onto one execution.
const providerPerformanceTTL = 30 * time.Second

// loadProviderPerformance returns the rolling-hour performance view for the
// requested provider ids, with a zero-valued entry for any id that saw no
// traffic. The underlying aggregation is computed for ALL providers at once and
// cached under a single key, then sliced here.
func (h *Handler) loadProviderPerformance(ctx context.Context, providerIDs []string) (map[string]*providerPerformanceView, error) {
	out := make(map[string]*providerPerformanceView, len(providerIDs))
	for _, id := range providerIDs {
		out[id] = &providerPerformanceView{WindowSeconds: providerPerformanceWindowSeconds}
	}
	if len(providerIDs) == 0 || h.rawDB == nil {
		return out, nil
	}

	all, _, err := cache.GetOrCompute(ctx, h.cache, cache.KeyProviderPerformance, providerPerformanceTTL,
		func(ctx context.Context) (map[string]*providerPerformanceView, error) {
			return h.queryProviderPerformance(ctx)
		})
	if err != nil {
		return nil, err
	}
	for _, id := range providerIDs {
		if v, ok := all[id]; ok && v != nil {
			out[id] = v
		}
	}
	return out, nil
}

// queryProviderPerformance runs the three-percentile aggregation over the
// rolling window for every provider. Callers should go through
// loadProviderPerformance so the result is shared.
func (h *Handler) queryProviderPerformance(ctx context.Context) (map[string]*providerPerformanceView, error) {
	out := map[string]*providerPerformanceView{}
	args := []any{time.Now().Add(-time.Duration(providerPerformanceWindowSeconds) * time.Second)}

	rows, err := h.rawDB.QueryContext(ctx, `
		SELECT
		  provider_id::text,
		  COUNT(*)::bigint AS request_count,
		  COUNT(*) FILTER (WHERE status = 'ok')::bigint AS success_count,
		  COUNT(*) FILTER (WHERE status <> 'ok')::bigint AS failure_count,
		  COUNT(ttft_ms)::bigint AS ttft_count,
		  AVG(ttft_ms)::float8 AS avg_ttft_ms,
		  percentile_cont(0.50) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL) AS p50_ttft_ms,
		  percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL) AS p95_ttft_ms,
		  percentile_cont(0.99) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL) AS p99_ttft_ms,
		  `+obsTPSExpr+` AS avg_tps
		FROM usage_logs
		WHERE created_at >= $1
		  AND provider_id IS NOT NULL
		GROUP BY provider_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			providerID   string
			requestCount int64
			successCount int64
			failureCount int64
			ttftCount    int64
			avgTTFT      sql.NullFloat64
			p50TTFT      sql.NullFloat64
			p95TTFT      sql.NullFloat64
			p99TTFT      sql.NullFloat64
			avgTPS       sql.NullFloat64
		)
		if err := rows.Scan(&providerID, &requestCount, &successCount, &failureCount, &ttftCount, &avgTTFT, &p50TTFT, &p95TTFT, &p99TTFT, &avgTPS); err != nil {
			return nil, err
		}
		view := &providerPerformanceView{
			WindowSeconds: providerPerformanceWindowSeconds,
			RequestCount:  int(requestCount),
			SuccessCount:  int(successCount),
			FailureCount:  int(failureCount),
			TTFTCount:     int(ttftCount),
		}
		if requestCount > 0 {
			view.FailureRate = float64(failureCount) / float64(requestCount)
		}
		if avgTTFT.Valid {
			view.AvgTTFTMs = roundedIntPtr(avgTTFT.Float64)
		}
		if p50TTFT.Valid {
			view.P50TTFTMs = roundedIntPtr(p50TTFT.Float64)
		}
		if p95TTFT.Valid {
			view.P95TTFTMs = roundedIntPtr(p95TTFT.Float64)
		}
		if p99TTFT.Valid {
			view.P99TTFTMs = roundedIntPtr(p99TTFT.Float64)
		}
		if avgTPS.Valid {
			tps := math.Round(avgTPS.Float64*100) / 100
			view.TPS = &tps
		}
		out[providerID] = view
	}
	return out, rows.Err()
}

func roundedIntPtr(v float64) *int {
	n := int(math.Round(v))
	return &n
}
