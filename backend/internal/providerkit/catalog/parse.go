package catalog

import "encoding/json"

// raw* types mirror the vendored artifact's JSON shape; the public types in
// catalog.go are the normalized view (same split as the Rust parser).

type rawCatalog struct {
	OMPVersion    string                                `json:"ompVersion"`
	AuthProviders []rawAuthProvider                     `json:"authProviders"`
	APIRoutes     []rawProviderRoutes                   `json:"apiRoutes"`
	DefaultModels map[string]string                     `json:"defaultModels"`
	Models        map[string]map[string]rawCatalogModel `json:"models"`
}

type rawEnv struct {
	Vars []string `json:"vars"`
	Hook string   `json:"hook"`
}

type rawAuthProvider struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	APIKeyFormat        string          `json:"apiKeyFormat"`
	Login               json.RawMessage `json:"login"`
	Env                 *rawEnv         `json:"env"`
	AllowsMissingAPIKey bool            `json:"allowsMissingApiKey"`
	StoreAs             string          `json:"storeAs"`
}

func (r rawAuthProvider) toAuthProvider() *AuthProvider {
	if r.ID == "" {
		return nil
	}
	format := r.APIKeyFormat
	if format == "" {
		format = "bearer"
	}
	p := &AuthProvider{
		ID:                  r.ID,
		Name:                r.Name,
		APIKeyFormat:        format,
		AllowsMissingAPIKey: r.AllowsMissingAPIKey,
		StoreAs:             r.StoreAs,
	}
	if r.Env != nil {
		p.EnvVars = r.Env.Vars
		p.EnvHook = r.Env.Hook
	}
	if len(r.Login) > 0 {
		p.Login = parseLogin(r.Login)
	}
	return p
}

type rawLogin struct {
	Kind string `json:"kind"`
	Hook string `json:"hook"`
	// api-key
	AuthURL      string             `json:"authUrl"`
	Instructions string             `json:"instructions"`
	Prompt       string             `json:"prompt"`
	Placeholder  string             `json:"placeholder"`
	Normalize    string             `json:"normalize"`
	Validate     *rawAPIKeyValidate `json:"validate"`
}

type rawAPIKeyValidate struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	URL     string `json:"url"`
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
}

type rawProviderRoutes struct {
	Provider string        `json:"provider"`
	Routes   []rawAPIRoute `json:"routes"`
	Default  string        `json:"default"`
}

type rawAPIRoute struct {
	API         string    `json:"api"`
	Match       *rawMatch `json:"match"`
	StripPrefix bool      `json:"stripPrefix"`
}

type rawMatch struct {
	Exact  []string `json:"exact"`
	Prefix []string `json:"prefix"`
	Glob   []string `json:"glob"`
}

type rawCatalogModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	API           string   `json:"api"`
	BaseURL       string   `json:"baseUrl"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	ContextWindow uint64   `json:"contextWindow"`
	MaxTokens     uint64   `json:"maxTokens"`
	Cost          *rawCost `json:"cost"`
}

type rawCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// parseLogin keeps the API-key prompt rules. The vendored artifact only carries
// API-key and custom (hook-driven) logins; anything else is ignored.
func parseLogin(raw json.RawMessage) *LoginRule {
	var in rawLogin
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	switch in.Kind {
	case "api-key":
		rule := &APIKeyLogin{
			AuthURL:      in.AuthURL,
			Instructions: in.Instructions,
			Prompt:       in.Prompt,
			Placeholder:  in.Placeholder,
			Normalize:    in.Normalize,
		}
		if in.Validate != nil {
			rule.Validate = &APIKeyValidate{
				Kind:    in.Validate.Kind,
				Label:   in.Validate.Label,
				URL:     in.Validate.URL,
				BaseURL: in.Validate.BaseURL,
				Model:   in.Validate.Model,
			}
		}
		return &LoginRule{Kind: in.Kind, APIKey: rule}
	case "custom":
		return &LoginRule{Kind: in.Kind, Hook: in.Hook}
	default:
		return nil
	}
}

func (r rawProviderRoutes) toProviderRoutes() *ProviderRoutes {
	if r.Provider == "" {
		return nil
	}
	out := &ProviderRoutes{Provider: r.Provider, Default: r.Default}
	for _, route := range r.Routes {
		if route.API == "" || route.Match == nil {
			continue
		}
		out.Routes = append(out.Routes, APIRoute{
			API:         route.API,
			Exact:       route.Match.Exact,
			Prefix:      route.Match.Prefix,
			Glob:        route.Match.Glob,
			StripPrefix: route.StripPrefix,
		})
	}
	return out
}

func (r rawCatalogModel) toCatalogModel() *CatalogModel {
	if r.ID == "" {
		return nil
	}
	m := &CatalogModel{
		ID:            r.ID,
		Name:          r.Name,
		API:           r.API,
		BaseURL:       r.BaseURL,
		Reasoning:     r.Reasoning,
		Input:         r.Input,
		ContextWindow: r.ContextWindow,
		MaxTokens:     r.MaxTokens,
	}
	if r.Cost != nil {
		m.InputCost = r.Cost.Input
		m.OutputCost = r.Cost.Output
		m.CachedInputCost = r.Cost.CacheRead
		m.CacheWriteCost = r.Cost.CacheWrite
	}
	return m
}
