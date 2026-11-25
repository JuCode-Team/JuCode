package biz

// Web tools: web search and page fetch, served under /tools/v1 by vendor
// accounts (llm/web_tools.go). They are not models. Each engine has its own
// switch and per-call price, calls are billed to the balance, and a per-user
// rate limit applies. The vendor keys are ordinary upstream accounts on the
// "web" protocol, so proxies, health, cooldowns, concurrency caps and quota
// sampling work as for any account; an engine is served by every account
// whose api_format names one of its vendors.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entsetting "github.com/jucode/jucode/internal/data/ent/sitesetting"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

// Web tool kinds: the two /tools/v1 endpoints.
const (
	WebToolSearch = "search"
	WebToolFetch  = "fetch"
)

// WebEngine is one engine a caller can pick. The set is defined here; prices
// and switches are settings.
type WebEngine struct {
	// ID is "<kind>.<engine>", e.g. "search.auto". Usage rows record it.
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Engine string `json:"engine"`
	Name   string `json:"name"`
	// Vendors are the account api_formats that serve the engine.
	Vendors []string `json:"vendors"`
	// DefaultPrice is the per-call price (points, 1 point = 1 CNY) until an
	// operator sets one.
	DefaultPrice string `json:"-"`
}

// WebEngines lists every engine, in display order.
var WebEngines = []WebEngine{
	{ID: "search.auto", Kind: WebToolSearch, Engine: "auto", Name: "网页搜索 · 自动", Vendors: []string{"parallel", "brave"}, DefaultPrice: "0.05"},
	{ID: "search.parallel", Kind: WebToolSearch, Engine: "parallel", Name: "网页搜索 · Parallel", Vendors: []string{"parallel"}, DefaultPrice: "0.05"},
	{ID: "search.brave", Kind: WebToolSearch, Engine: "brave", Name: "网页搜索 · Brave", Vendors: []string{"brave"}, DefaultPrice: "0.05"},
	{ID: "fetch.jina", Kind: WebToolFetch, Engine: "jina", Name: "网页抓取 · Jina", Vendors: []string{"jina"}, DefaultPrice: "0.01"},
	{ID: "fetch.firecrawl", Kind: WebToolFetch, Engine: "firecrawl", Name: "网页抓取 · Firecrawl", Vendors: []string{"firecrawl"}, DefaultPrice: "0.04"},
	{ID: "fetch.parallel", Kind: WebToolFetch, Engine: "parallel", Name: "网页抓取 · Parallel", Vendors: []string{"parallel"}, DefaultPrice: "0.01"},
}

// Default engines when a request names none.
const (
	DefaultWebSearchEngine = "auto"
	DefaultWebFetchEngine  = "jina"
)

// WebEngineByKind finds the engine a request names; engine "" takes the kind's
// default.
func WebEngineByKind(kind, engine string) (WebEngine, bool) {
	engine = strings.ToLower(strings.TrimSpace(engine))
	if engine == "" {
		engine = DefaultWebSearchEngine
		if kind == WebToolFetch {
			engine = DefaultWebFetchEngine
		}
	}
	for _, e := range WebEngines {
		if e.Kind == kind && e.Engine == engine {
			return e, true
		}
	}
	return WebEngine{}, false
}

// WebEngineNames lists a kind's engine names, for error messages.
func WebEngineNames(kind string) []string {
	var out []string
	for _, e := range WebEngines {
		if e.Kind == kind {
			out = append(out, e.Engine)
		}
	}
	return out
}

// WebEngineSetting is an engine's operator settings.
type WebEngineSetting struct {
	Enabled bool            `json:"enabled"`
	Price   decimal.Decimal `json:"price"`
}

// WebToolSettings is the stored configuration (site setting "web_tools").
type WebToolSettings struct {
	Engines map[string]WebEngineSetting `json:"engines"`
	// UserRPM caps web tool calls per user per minute, across both kinds.
	UserRPM int `json:"user_rpm"`
}

const (
	keyWebTools          = "web_tools"
	defaultWebToolRPM    = 120
	maxWebToolRPM        = 100000
	webToolSettingsCache = 30 * time.Second
)

// DefaultWebToolSettings enables every engine at its default price.
func DefaultWebToolSettings() WebToolSettings {
	out := WebToolSettings{Engines: map[string]WebEngineSetting{}, UserRPM: defaultWebToolRPM}
	for _, e := range WebEngines {
		out.Engines[e.ID] = WebEngineSetting{Enabled: true, Price: decimal.RequireFromString(e.DefaultPrice)}
	}
	return out
}

// NormalizeWebToolSettings validates an update: unknown engines and negative
// prices are rejected, engines left out keep their defaults.
func NormalizeWebToolSettings(in WebToolSettings) (WebToolSettings, error) {
	out := DefaultWebToolSettings()
	for id, setting := range in.Engines {
		if _, ok := out.Engines[id]; !ok {
			return out, fmt.Errorf("%w: unknown web engine %q", ErrInvalidSiteSettings, id)
		}
		if setting.Price.IsNegative() {
			return out, fmt.Errorf("%w: price of %s must not be negative", ErrInvalidSiteSettings, id)
		}
		out.Engines[id] = setting
	}
	if in.UserRPM < 1 || in.UserRPM > maxWebToolRPM {
		return out, fmt.Errorf("%w: user_rpm must be between 1 and %d", ErrInvalidSiteSettings, maxWebToolRPM)
	}
	out.UserRPM = in.UserRPM
	return out, nil
}

// WebToolSvc reads and writes the web tool settings and picks vendor accounts.
type WebToolSvc struct {
	d *data.Data

	mu       sync.Mutex
	cached   *WebToolSettings
	cachedAt time.Time
}

func NewWebToolSvc(d *data.Data) *WebToolSvc { return &WebToolSvc{d: d} }

// Settings returns the current settings. Instances re-read them at most every
// 30 seconds, so an update reaches the whole fleet within that time.
func (s *WebToolSvc) Settings(ctx context.Context) (WebToolSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && time.Since(s.cachedAt) < webToolSettingsCache {
		return cloneWebToolSettings(*s.cached), nil
	}
	out := DefaultWebToolSettings()
	row, err := s.d.Ent.SiteSetting.Query().Where(entsetting.KeyEQ(keyWebTools)).Only(ctx)
	switch {
	case err == nil:
		var stored WebToolSettings
		if jsonErr := json.Unmarshal([]byte(row.Value), &stored); jsonErr == nil {
			if normalized, normErr := NormalizeWebToolSettings(stored); normErr == nil {
				out = normalized
			}
		}
	case !ent.IsNotFound(err):
		return out, err
	}
	s.cached, s.cachedAt = &out, time.Now()
	return cloneWebToolSettings(out), nil
}

// Update validates and stores new settings.
func (s *WebToolSvc) Update(ctx context.Context, in WebToolSettings) (WebToolSettings, error) {
	normalized, err := NormalizeWebToolSettings(in)
	if err != nil {
		return normalized, err
	}
	raw, _ := json.Marshal(normalized)
	if err := s.d.Ent.SiteSetting.Create().
		SetKey(keyWebTools).SetValue(string(raw)).
		OnConflictColumns(entsetting.FieldKey).
		UpdateNewValues().
		Exec(ctx); err != nil {
		return normalized, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = &normalized, time.Now()
	s.mu.Unlock()
	return cloneWebToolSettings(normalized), nil
}

func cloneWebToolSettings(in WebToolSettings) WebToolSettings {
	out := WebToolSettings{Engines: make(map[string]WebEngineSetting, len(in.Engines)), UserRPM: in.UserRPM}
	for k, v := range in.Engines {
		out.Engines[k] = v
	}
	return out
}

// WebToolPrice is one engine's public price.
type WebToolPrice struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Engine string `json:"engine"`
	Name   string `json:"name"`
	// Price is per successful call, in CNY.
	Price string `json:"price"`
}

// PublicPrices lists the enabled engines and their prices.
func (s *WebToolSvc) PublicPrices(ctx context.Context) ([]WebToolPrice, error) {
	settings, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WebToolPrice, 0, len(WebEngines))
	for _, e := range WebEngines {
		setting := settings.Engines[e.ID]
		if !setting.Enabled {
			continue
		}
		out = append(out, WebToolPrice{ID: e.ID, Kind: e.Kind, Engine: e.Engine, Name: e.Name, Price: setting.Price.String()})
	}
	return out, nil
}

// Accounts returns the accounts that can serve an engine right now, in
// attempt order: lower priority first, weighted by weight and health within a
// tier. Accounts parked by health state or a runtime cooldown are left out.
func (s *WebToolSvc) Accounts(ctx context.Context, engine WebEngine) ([]RouteAccount, error) {
	rows, err := s.d.Ent.UpstreamAccount.Query().
		Where(
			entaccount.APIFormatIn(engine.Vendors...),
			entaccount.StatusEQ(entaccount.StatusActive),
			entaccount.SchedulableEQ(true),
			entaccount.DeletedAtIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]RouteAccount, 0, len(rows))
	for _, acct := range rows {
		if !AccountHasNativeProtocol(acct, NativeProtocolWeb) || !AccountSchedulable(acct, engine.ID, now) {
			continue
		}
		out = append(out, RouteAccount{Account: acct, UpstreamModel: engine.ID, Priority: acct.Priority, Weight: acct.Weight})
	}
	out = s.dropCooledDown(ctx, out)
	orderRouteAccounts(out)
	return out, nil
}

// dropCooledDown removes accounts with a runtime cooldown in Redis. On Redis
// trouble it keeps them all: the attempt loop still fails over.
func (s *WebToolSvc) dropCooledDown(ctx context.Context, accounts []RouteAccount) []RouteAccount {
	if len(accounts) == 0 || s.d.Redis == nil {
		return accounts
	}
	keys := make([]string, len(accounts))
	for i, rp := range accounts {
		keys[i] = accountCooldownKey(rp.Account.ID)
	}
	vals, err := s.d.Redis.MGet(ctx, keys...).Result()
	if err != nil || len(vals) != len(keys) {
		return accounts
	}
	out := accounts[:0]
	for i, v := range vals {
		if v == nil {
			out = append(out, accounts[i])
		}
	}
	return out
}

// AllowCall counts one call against the user's per-minute web tool limit and
// reports whether it fits. It fails open on Redis trouble, like the other
// rate limiters: the limit guards cost, the balance check still applies.
func (s *WebToolSvc) AllowCall(ctx context.Context, userID uuid.UUID, rpm int) bool {
	if s.d.Redis == nil || rpm <= 0 {
		return true
	}
	key := fmt.Sprintf("rl:webtools:%s:%d", userID, time.Now().Unix()/60)
	rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	count, err := s.d.Redis.Incr(rctx, key).Result()
	if err != nil {
		return true
	}
	if count == 1 {
		_ = s.d.Redis.Expire(rctx, key, time.Minute).Err()
	}
	return count <= int64(rpm)
}

// WebVendorAccounts counts each vendor's web accounts: all that exist, and
// those that can be scheduled right now.
type WebVendorAccounts struct {
	Total       int `json:"total"`
	Schedulable int `json:"schedulable"`
}

// VendorAccounts reports account counts per vendor (api_format).
func (s *WebToolSvc) VendorAccounts(ctx context.Context) (map[string]WebVendorAccounts, error) {
	vendors := map[string]struct{}{}
	for _, e := range WebEngines {
		for _, v := range e.Vendors {
			vendors[v] = struct{}{}
		}
	}
	names := make([]string, 0, len(vendors))
	for v := range vendors {
		names = append(names, v)
	}
	rows, err := s.d.Ent.UpstreamAccount.Query().
		Where(entaccount.APIFormatIn(names...), entaccount.DeletedAtIsNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]WebVendorAccounts, len(names))
	for _, v := range names {
		out[v] = WebVendorAccounts{}
	}
	now := time.Now()
	for _, acct := range rows {
		if !AccountHasNativeProtocol(acct, NativeProtocolWeb) {
			continue
		}
		count := out[acct.APIFormat]
		count.Total++
		if AccountSchedulable(acct, "", now) {
			count.Schedulable++
		}
		out[acct.APIFormat] = count
	}
	return out, nil
}
