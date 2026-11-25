// Package catalog is the Go port of the Rust llm-provider-kit's provider
// metadata layer (src/omp.rs): the vendored `pi-catalog` artifact (from the
// MIT-licensed oh-my-pi project) carries the provider list with its API-key
// rules, the api-routes table, and the model catalog, so the gateway reuses
// ecosystem data instead of maintaining provider tables by hand. The sync
// script keeps only API-key providers; subscription logins are dropped.
//
// Only the wire dialects this module speaks are routable; providers whose
// models all resolve to unsupported APIs stay visible for BYOK config but
// cannot be picked.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed data/omp-catalog.json
var catalogJSON []byte

// Protocol is a wire protocol spoken with a provider endpoint.
type Protocol string

const (
	// ProtocolOpenAIResponses is the OpenAI Responses API
	// (POST {base}/responses).
	ProtocolOpenAIResponses Protocol = "responses"
	// ProtocolAzureOpenAIResponses is Azure OpenAI Responses
	// (POST {base}/responses?api-version=…), authenticated with an api-key
	// header.
	ProtocolAzureOpenAIResponses Protocol = "azure"
	// ProtocolAnthropicMessages is the Anthropic Messages API.
	ProtocolAnthropicMessages Protocol = "anthropic"
	// ProtocolOpenAIChatCompletions is the OpenAI Chat Completions API.
	ProtocolOpenAIChatCompletions Protocol = "chat"
)

// ProtocolForAPI maps an upstream wire-dialect id to a protocol this module
// speaks. google-*/bedrock-* and agent-specific dialects need request shaping
// this module does not have and are deliberately unmapped.
func ProtocolForAPI(api string) (Protocol, bool) {
	switch api {
	case "anthropic-messages":
		return ProtocolAnthropicMessages, true
	case "openai-completions", "openrouter":
		return ProtocolOpenAIChatCompletions, true
	case "openai-responses":
		return ProtocolOpenAIResponses, true
	case "azure-openai-responses":
		return ProtocolAzureOpenAIResponses, true
	default:
		return "", false
	}
}

// IsAnthropicModel reports the model-name heuristic (claude-*).
func IsAnthropicModel(model string) bool { return strings.HasPrefix(model, "claude-") }

// ResolveProtocol mirrors Protocol::resolve: an explicit provider protocol
// wins; otherwise fall back to the model heuristic (claude-* → anthropic,
// else responses).
func ResolveProtocol(explicit Protocol, model string) Protocol {
	switch explicit {
	case ProtocolAnthropicMessages, ProtocolOpenAIResponses,
		ProtocolAzureOpenAIResponses, ProtocolOpenAIChatCompletions:
		return explicit
	}
	if IsAnthropicModel(model) {
		return ProtocolAnthropicMessages
	}
	return ProtocolOpenAIResponses
}

// Catalog is the parsed view over the vendored artifact.
type Catalog struct {
	version       string
	providers     []*AuthProvider
	routes        []*ProviderRoutes
	defaultModels map[string]string
	models        map[string][]*CatalogModel
}

// AuthProvider is one `auth "<id>"` rule compiled by upstream — how a
// provider's API key is obtained and presented.
type AuthProvider struct {
	ID                  string
	Name                string
	APIKeyFormat        string // "bearer" or "structured"
	Login               *LoginRule
	EnvVars             []string
	EnvHook             string
	AllowsMissingAPIKey bool
	// StoreAs is the credential/model slot this provider shares.
	StoreAs string
}

// LoginKind is the wire name of the declared login flow ("" when none).
func (p *AuthProvider) LoginKind() string {
	if p == nil || p.Login == nil {
		return ""
	}
	return p.Login.Kind
}

// LoginRule is the key-entry flow declared by an auth rule.
type LoginRule struct {
	Kind   string // "api-key" | "custom"
	APIKey *APIKeyLogin
	// Hook is the whole-flow hook name for custom logins (unimplemented).
	Hook string
}

// APIKeyLogin describes how a manual key is obtained and normalized.
type APIKeyLogin struct {
	AuthURL      string
	Instructions string
	Prompt       string
	Placeholder  string
	Normalize    string // e.g. "strip-bearer"
	Validate     *APIKeyValidate
}

// APIKeyValidate is the declared key-validation probe.
type APIKeyValidate struct {
	Kind    string
	Label   string
	URL     string
	BaseURL string
	Model   string
}

// CatalogModel is one model entry from upstream's generated catalog.
type CatalogModel struct {
	ID            string
	Name          string
	API           string // wire dialect id, e.g. "anthropic-messages"
	Provider      string
	BaseURL       string
	ContextWindow uint64
	MaxTokens     uint64
	Reasoning     bool
	Input         []string
	// USD per 1M tokens; 0 = unknown.
	InputCost       float64
	CachedInputCost float64
	OutputCost      float64
	CacheWriteCost  float64
}

// ProviderRoutes is the per-provider api-routes table.
type ProviderRoutes struct {
	Provider string
	Routes   []APIRoute
	Default  string
}

// APIRoute matches model ids to a wire dialect.
type APIRoute struct {
	API         string
	Exact       []string
	Prefix      []string
	Glob        []string
	StripPrefix bool
}

// ResolvedRoute is the outcome of resolving (provider, model) through the
// api-routes table.
type ResolvedRoute struct {
	API string
	// WireModel is the model id to send upstream — prefix stripped when the
	// route says so.
	WireModel string
}

var (
	once     sync.Once
	instance *Catalog
	loadErr  error
)

// Load returns the vendored catalog, parsed once.
func Load() (*Catalog, error) {
	once.Do(func() {
		instance, loadErr = Parse(catalogJSON)
	})
	return instance, loadErr
}

// MustLoad is Load for callers that treat the vendored artifact as a build
// invariant (it ships with the binary).
func MustLoad() *Catalog {
	c, err := Load()
	if err != nil {
		panic(fmt.Sprintf("providerkit/catalog: vendored catalog is invalid: %v", err))
	}
	return c
}

// Parse decodes a catalog artifact.
func Parse(raw []byte) (*Catalog, error) {
	var root rawCatalog
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	c := &Catalog{
		version:       root.OMPVersion,
		defaultModels: map[string]string{},
		models:        map[string][]*CatalogModel{},
	}
	for i := range root.AuthProviders {
		if p := root.AuthProviders[i].toAuthProvider(); p != nil {
			c.providers = append(c.providers, p)
		}
	}
	for i := range root.APIRoutes {
		if r := root.APIRoutes[i].toProviderRoutes(); r != nil {
			c.routes = append(c.routes, r)
		}
	}
	for provider, entries := range root.Models {
		list := make([]*CatalogModel, 0, len(entries))
		for _, m := range entries {
			if cm := m.toCatalogModel(); cm != nil {
				list = append(list, cm)
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		c.models[provider] = list
	}
	for provider, model := range root.DefaultModels {
		if model != "" {
			c.defaultModels[provider] = model
		}
	}
	return c, nil
}

// Version is the pi-catalog release the artifact was generated from.
func (c *Catalog) Version() string { return c.version }

// AuthProvider returns the auth rule for a provider id.
func (c *Catalog) AuthProvider(id string) *AuthProvider {
	for _, p := range c.providers {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// AuthProviders lists all auth rules in upstream display order.
func (c *Catalog) AuthProviders() []*AuthProvider { return c.providers }

// Models returns the catalog models for a provider, following `storeAs` so
// aliased providers see the shared table.
func (c *Catalog) Models(provider string) []*CatalogModel {
	if list := c.models[provider]; len(list) > 0 {
		return list
	}
	alias := provider
	if p := c.AuthProvider(provider); p != nil && p.StoreAs != "" {
		alias = p.StoreAs
	}
	return c.models[alias]
}

// SupportedModels filters a provider's models down to dialects this module
// can serve.
func (c *Catalog) SupportedModels(provider string) []*CatalogModel {
	all := c.Models(provider)
	out := make([]*CatalogModel, 0, len(all))
	for _, m := range all {
		if _, ok := c.ProtocolFor(provider, m.ID); ok {
			out = append(out, m)
		}
	}
	return out
}

// DefaultBaseURL is the first supported model's baseUrl.
func (c *Catalog) DefaultBaseURL(provider string) string {
	for _, m := range c.SupportedModels(provider) {
		if m.BaseURL != "" {
			return m.BaseURL
		}
	}
	return ""
}

// APIFor is the wire dialect for (provider, model): api-routes rules win over
// the model's declared api.
func (c *Catalog) APIFor(provider, model string) string {
	if r, ok := c.Resolve(provider, model); ok {
		return r.API
	}
	return ""
}

// Resolve routes (provider, model) to a dialect via the provider's api-routes
// table, else the model's own `api` field.
func (c *Catalog) Resolve(provider, model string) (ResolvedRoute, bool) {
	for _, entry := range c.routes {
		if entry.Provider != provider {
			continue
		}
		for i := range entry.Routes {
			route := &entry.Routes[i]
			matched := false
			for _, e := range route.Exact {
				if e == model {
					matched = true
					break
				}
			}
			if !matched {
				for _, p := range route.Prefix {
					if strings.HasPrefix(model, p) {
						matched = true
						break
					}
				}
			}
			if !matched {
				for _, g := range route.Glob {
					if globMatch(g, model) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
			wireModel := model
			if route.StripPrefix {
				wireModel = model
				for _, p := range route.Prefix {
					if strings.HasPrefix(model, p) {
						wireModel = model[len(p):]
						break
					}
				}
			}
			return ResolvedRoute{API: route.API, WireModel: wireModel}, true
		}
		if entry.Default != "" {
			return ResolvedRoute{API: entry.Default, WireModel: model}, true
		}
		// Provider has a routes table but no default and no match: fall
		// through to the model's declared api.
		break
	}
	for _, m := range c.Models(provider) {
		if m.ID == model {
			return ResolvedRoute{API: m.API, WireModel: model}, true
		}
	}
	return ResolvedRoute{}, false
}

// ProtocolFor is the protocol for (provider, model) when the dialect is
// supported.
func (c *Catalog) ProtocolFor(provider, model string) (Protocol, bool) {
	api := c.APIFor(provider, model)
	if api == "" {
		return "", false
	}
	return ProtocolForAPI(api)
}

// DefaultModel is upstream's curated default model for a provider.
func (c *Catalog) DefaultModel(provider string) string {
	if m := c.defaultModels[provider]; m != "" {
		return m
	}
	if p := c.AuthProvider(provider); p != nil && p.StoreAs != "" {
		return c.defaultModels[p.StoreAs]
	}
	return ""
}

// globMatch supports `*` and `?` (upstream api-routes glob semantics).
func globMatch(pattern, text string) bool {
	pat, txt := []rune(pattern), []rune(text)
	p, t := 0, 0
	star, starT := -1, 0
	for t < len(txt) {
		switch {
		case p < len(pat) && (pat[p] == '?' || pat[p] == txt[t]):
			p++
			t++
		case p < len(pat) && pat[p] == '*':
			star = p
			starT = t
			p++
		case star >= 0:
			p = star + 1
			starT++
			t = starT
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}
