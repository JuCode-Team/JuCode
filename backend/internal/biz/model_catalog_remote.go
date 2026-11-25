package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// remoteCatalogURL is the public models.dev catalog feed. It is a ~5MB JSON
// of {provider_id: {name, models: {model_id: {name, cost, limit, …}}}}.
const remoteCatalogURL = "https://models.dev/api.json"

const remoteCatalogTTL = time.Hour

// RemoteModelCost is the catalog's per-1M-token USD pricing for one model.
type RemoteModelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// RemoteCatalogModel is one model in the public catalog, trimmed to what the
// admin console displays. In the merged canonical view Providers lists every
// catalog provider carrying the model; per-provider listings leave it empty.
type RemoteCatalogModel struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Context   int              `json:"context,omitempty"`
	Cost      *RemoteModelCost `json:"cost,omitempty"`
	Providers []string         `json:"providers,omitempty"`
}

// RemoteCatalogProvider is one vendor entry in the catalog summary.
type RemoteCatalogProvider struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Models int    `json:"models"`
}

// RemoteCatalogSvc fetches and caches the public model catalog. The payload
// is large, so it is held in memory and refreshed at most once per TTL; a
// refresh failure keeps serving the last good copy.
type RemoteCatalogSvc struct {
	mu        sync.Mutex
	client    *http.Client
	url       string
	fetched   time.Time
	providers []RemoteCatalogProvider
	byID      map[string][]RemoteCatalogModel
}

func NewRemoteCatalogSvc() *RemoteCatalogSvc {
	return &RemoteCatalogSvc{
		client: &http.Client{Timeout: 30 * time.Second},
		url:    remoteCatalogURL,
	}
}

// Providers lists every vendor in the catalog with a model count.
func (s *RemoteCatalogSvc) Providers(ctx context.Context) ([]RemoteCatalogProvider, error) {
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RemoteCatalogProvider, len(s.providers))
	copy(out, s.providers)
	return out, nil
}

// Models returns the model list for one provider id.
func (s *RemoteCatalogSvc) Models(ctx context.Context, providerID string) ([]RemoteCatalogModel, error) {
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	models, ok := s.byID[strings.TrimSpace(providerID)]
	if !ok {
		return nil, fmt.Errorf("unknown catalog provider %q", providerID)
	}
	out := make([]RemoteCatalogModel, len(models))
	copy(out, models)
	return out, nil
}

// CanonicalModelID reduces a catalog or upstream model id to the canonical
// platform name: the vendor prefix is routing detail, not identity, so
// deepseek/deepseek-v4.1-flash (aggregator-style) and deepseek-v4.1-flash
// (original-vendor style) collapse to the same model.
func CanonicalModelID(id string) string {
	name := strings.TrimSpace(id)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// CanonicalModels merges every provider's model list into one entry per
// canonical model id. When the same model appears under several providers,
// the original-vendor listing (bare id, no prefix) wins the metadata; among
// prefixed listings the first one carrying a price wins. Each entry records
// the provider ids it was seen under.
func (s *RemoteCatalogSvc) CanonicalModels(ctx context.Context) ([]RemoteCatalogModel, error) {
	if err := s.load(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type entry struct {
		model    RemoteCatalogModel
		prefixed bool
		seen     map[string]struct{}
	}
	merged := make(map[string]*entry, 4096)
	for _, pid := range sortedKeys(s.byID) {
		for _, m := range s.byID[pid] {
			canon := CanonicalModelID(m.ID)
			if canon == "" {
				continue
			}
			e := merged[canon]
			if e == nil {
				e = &entry{seen: map[string]struct{}{}}
				merged[canon] = e
			}
			prefixed := m.ID != canon
			switch {
			case e.model.ID == "":
				e.model, e.prefixed = m, prefixed
			case e.prefixed && !prefixed:
				e.model, e.prefixed = m, prefixed
			case e.prefixed == prefixed && e.model.Cost == nil && m.Cost != nil:
				e.model = m
			}
			e.model.ID = canon
			e.seen[pid] = struct{}{}
		}
	}
	out := make([]RemoteCatalogModel, 0, len(merged))
	for id, e := range merged {
		m := e.model
		m.ID = id
		for pid := range e.seen {
			m.Providers = append(m.Providers, pid)
		}
		sort.Strings(m.Providers)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CostsByModelID maps every catalog model id onto its reference pricing.
// Ids collide across vendors occasionally; the alphabetically-first provider
// wins — acceptable for a display-only hint. Returns nil when the catalog is
// unreachable, so callers simply skip the supplement.
func (s *RemoteCatalogSvc) CostsByModelID(ctx context.Context) map[string]*RemoteModelCost {
	if err := s.load(ctx); err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*RemoteModelCost, 4096)
	for _, pid := range sortedKeys(s.byID) {
		for _, m := range s.byID[pid] {
			if m.Cost == nil {
				continue
			}
			if _, dup := out[m.ID]; !dup {
				out[m.ID] = m.Cost
			}
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// load refreshes the cache when stale. Callers hold no lock while fetching.
func (s *RemoteCatalogSvc) load(ctx context.Context) error {
	s.mu.Lock()
	fresh := time.Since(s.fetched) < remoteCatalogTTL && s.byID != nil
	s.mu.Unlock()
	if fresh {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if s.byID != nil {
			return nil // serve stale on fetch failure
		}
		return fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if s.byID != nil {
			return nil
		}
		return fmt.Errorf("model catalog returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	providers, byID, err := parseRemoteCatalog(body)
	if err != nil {
		if s.byID != nil {
			return nil
		}
		return err
	}
	s.mu.Lock()
	s.providers, s.byID, s.fetched = providers, byID, time.Now()
	s.mu.Unlock()
	return nil
}

// parseRemoteCatalog trims the feed to the fields the console needs.
func parseRemoteCatalog(body []byte) ([]RemoteCatalogProvider, map[string][]RemoteCatalogModel, error) {
	var raw map[string]struct {
		Name   string `json:"name"`
		Models map[string]struct {
			ID    string           `json:"id"`
			Name  string           `json:"name"`
			Cost  *RemoteModelCost `json:"cost"`
			Limit struct {
				Context int `json:"context"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse model catalog: %w", err)
	}
	providers := make([]RemoteCatalogProvider, 0, len(raw))
	byID := make(map[string][]RemoteCatalogModel, len(raw))
	for pid, p := range raw {
		models := make([]RemoteCatalogModel, 0, len(p.Models))
		for mid, m := range p.Models {
			id := m.ID
			if id == "" {
				id = mid
			}
			models = append(models, RemoteCatalogModel{
				ID:      id,
				Name:    m.Name,
				Context: m.Limit.Context,
				Cost:    m.Cost,
			})
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		byID[pid] = models
		name := p.Name
		if name == "" {
			name = pid
		}
		providers = append(providers, RemoteCatalogProvider{ID: pid, Name: name, Models: len(models)})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers, byID, nil
}
