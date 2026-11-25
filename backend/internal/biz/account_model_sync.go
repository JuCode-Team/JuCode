package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jucode/jucode/internal/data/ent"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	"github.com/jucode/jucode/internal/data/ent/schema"
	"github.com/shopspring/decimal"
)

// AccountModelsSyncResult summarizes one upstream /models pull: how many ids
// the endpoint returned, how many new bindings were created, and how many
// were already known.
type AccountModelsSyncResult struct {
	RemoteCount int      `json:"remote_count"`
	Added       int      `json:"added"`
	Existing    int      `json:"existing"`
	Updated     int      `json:"updated"`
	AddedModels []string `json:"added_models"`
	// UnmatchedModels are upstream ids no enabled platform model claims —
	// skipped rather than bound verbatim.
	UnmatchedModels []string `json:"unmatched_models,omitempty"`
}

// remoteModel is one entry from an upstream model list: the id plus the
// optional reference pricing the endpoint publishes (USD per 1M tokens).
type remoteModel struct {
	ID         string
	UpstreamID string // explicit upstream id for imports; "" = same as ID
	InCost     *decimal.Decimal
	OutCost    *decimal.Decimal
	Protocol   string // per-model endpoint hint when the list carries one
}

// SyncAccountModels pulls the account's upstream model list and binds the
// ids an enabled platform model claims. The list endpoint comes from the
// provider definition's models_path ("" = the /v1/models convention). Each
// remote id is first resolved onto a platform model name (vendor prefixes
// like "clinepass/" are stripped, and a platform name that prefixes the id
// at a separator claims it), so a binding's model_name stays a name clients
// actually request while upstream_model keeps the verbatim remote id. Ids
// no platform model claims are skipped and reported as unmatched. Existing
// rows are never touched — matching is by upstream_model, or by model_name
// when the row forwards verbatim.
func (s *AccountPoolSvc) SyncAccountModels(ctx context.Context, accountID uuid.UUID) (*AccountModelsSyncResult, error) {
	acct, err := s.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if models, ok := s.staticDefinitionModels(ctx, acct); ok {
		remote := make([]remoteModel, 0, len(models))
		for _, m := range models {
			remote = append(remote, remoteModel{ID: m.Name, Protocol: m.Protocol})
		}
		bound, unmatched, err := s.resolveRemoteModelNames(ctx, remote)
		if err != nil {
			return nil, err
		}
		res, err := s.bindRemoteModels(ctx, accountID, bound)
		if res != nil {
			res.UnmatchedModels = unmatched
		}
		return res, err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	token, err := s.RequestToken(ctx, acct)
	if err != nil {
		return nil, err
	}
	remote, err := fetchUpstreamModels(ctx, client, s.modelsListURL(ctx, acct), token, acct.HeaderRules)
	if err != nil {
		return nil, err
	}
	s.supplementRemoteCosts(ctx, remote)
	bound, unmatched, err := s.resolveRemoteModelNames(ctx, remote)
	if err != nil {
		return nil, err
	}
	res, err := s.bindRemoteModels(ctx, accountID, bound)
	if res != nil {
		res.UnmatchedModels = unmatched
	}
	return res, err
}

// resolveRemoteModelNames maps each remote id onto an enabled platform
// model name via resolvePlatformName. Mapped entries keep the verbatim
// remote id as UpstreamID; ids with no claimant come back as unmatched.
// When several remote ids land on the same platform model, the cleanest
// one (exact match, no "~" alias, no ":variant" suffix, shortest id) wins
// the upstream_model slot — the rest fall through to bindRemoteModels'
// dedup as already-known.
func (s *AccountPoolSvc) resolveRemoteModelNames(ctx context.Context, remote []remoteModel) ([]remoteModel, []string, error) {
	names, err := s.d.Ent.Model.Query().
		Where(entmodel.EnabledEQ(true)).
		Select(entmodel.FieldName).
		Strings(ctx)
	if err != nil {
		return nil, nil, err
	}
	idx := newPlatformModelIndex(names)

	type resolved struct {
		m     remoteModel
		exact bool
	}
	groups := make(map[string][]resolved)
	var order []string
	var unmatched []string
	for _, m := range remote {
		name, exact := idx.resolve(m.ID)
		if name == "" {
			unmatched = append(unmatched, m.ID)
			continue
		}
		if _, seen := groups[name]; !seen {
			order = append(order, name)
		}
		groups[name] = append(groups[name], resolved{m: m, exact: exact})
	}

	bound := make([]remoteModel, 0, len(remote))
	for _, name := range order {
		g := groups[name]
		sort.SliceStable(g, func(i, j int) bool {
			return remoteRank(g[i].m.ID, g[i].exact) < remoteRank(g[j].m.ID, g[j].exact)
		})
		for _, e := range g {
			if e.m.ID != name {
				e.m.UpstreamID = e.m.ID
				e.m.ID = name
			}
			bound = append(bound, e.m)
		}
	}
	return bound, unmatched, nil
}

// remoteRank orders remote ids claiming one platform model: exact hits
// first, then non-alias, non-variant, shortest id.
func remoteRank(id string, exact bool) int {
	rank := 0
	if !exact {
		rank += 1 << 20
	}
	if strings.HasPrefix(id, "~") {
		rank += 1 << 10
	}
	if strings.Contains(id, ":") {
		rank += 1 << 5
	}
	return rank + len(id)
}

// platformModelIndex maps normalized upstream id candidates onto platform
// model names. exact holds normalize(name) for every name; suffix holds
// normalize() of each "/" tail of a name, so a platform model that carries
// its own vendor prefix ("MiniMax/MiniMax-M3") is still claimed by a bare
// upstream id ("minimax-m3"). exact wins on key collisions.
type platformModelIndex struct {
	exact  map[string]string
	suffix map[string]string
}

func newPlatformModelIndex(names []string) *platformModelIndex {
	idx := &platformModelIndex{
		exact:  make(map[string]string, len(names)),
		suffix: make(map[string]string, len(names)),
	}
	for _, name := range names {
		for i, cand := range idSuffixes(name) {
			key := normalizeModelID(cand)
			if i == 0 {
				idx.exact[key] = name
			} else if _, taken := idx.exact[key]; !taken {
				if _, dup := idx.suffix[key]; !dup {
					idx.suffix[key] = name
				}
			}
		}
	}
	return idx
}

// resolve finds the platform model one upstream id serves. The id itself
// is tried first, then each "/" suffix (aggregator ids like
// "clinepass/deepseek-v4.1-flash" carry a vendor prefix the platform never
// names). A candidate wins on an exact hit, else on the longest index key
// that prefixes it at an id separator — "deepseek" claims
// "deepseek-v4.1-flash" but not "deepseeks". "" means no platform model
// claims the id.
func (idx *platformModelIndex) resolve(remoteID string) (name string, exact bool) {
	best := ""
	for _, cand := range idSuffixes(remoteID) {
		lc := normalizeModelID(cand)
		if name, ok := idx.exact[lc]; ok {
			return name, true
		}
		if name, ok := idx.suffix[lc]; ok {
			return name, true
		}
		for _, index := range []map[string]string{idx.exact, idx.suffix} {
			for lower, name := range index {
				if len(lower) >= len(lc) || len(lower) <= len(best) {
					continue
				}
				if strings.HasPrefix(lc, lower) && isModelIDBoundary(lc[len(lower)]) {
					best = name
				}
			}
		}
	}
	return best, false
}

// normalizeModelID folds the separators vendors disagree on (".", "_")
// into "-" and lowercases, so upstream "claude-sonnet-4.5" claims platform
// "claude-sonnet-4-5". "/" is kept: it marks the vendor boundary.
func normalizeModelID(s string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(strings.ToLower(s))
}

// idSuffixes returns the id plus every suffix after a "/", longest first:
// "a/b/c" → [a/b/c, b/c, c].
func idSuffixes(id string) []string {
	out := []string{id}
	for rest := id; ; {
		k := strings.IndexByte(rest, '/')
		if k < 0 || k+1 >= len(rest) {
			return out
		}
		rest = rest[k+1:]
		out = append(out, rest)
	}
}

func isModelIDBoundary(c byte) bool {
	switch c {
	case '-', '/', '.', ':', '@', '_':
		return true
	}
	return false
}

// supplementRemoteCosts fills missing display-only reference prices from the
// public catalog (matched by model id). Endpoint-reported prices always win;
// a catalog outage leaves the costs unset rather than failing the sync.
func (s *AccountPoolSvc) supplementRemoteCosts(ctx context.Context, remote []remoteModel) {
	if s.remoteCatalog == nil {
		return
	}
	costs := s.remoteCatalog.CostsByModelID(ctx)
	if len(costs) == 0 {
		return
	}
	for i := range remote {
		if remote[i].InCost != nil && remote[i].OutCost != nil {
			continue
		}
		c := costs[remote[i].ID]
		if c == nil {
			// Unprefixed ids sometimes miss; try the bare suffix of
			// vendor-prefixed ids (deepseek/x -> x) as a display hint.
			if idx := strings.LastIndex(remote[i].ID, "/"); idx >= 0 {
				c = costs[remote[i].ID[idx+1:]]
			}
		}
		if c == nil {
			continue
		}
		if remote[i].InCost == nil && c.Input > 0 {
			d := decimal.NewFromFloat(c.Input)
			remote[i].InCost = &d
		}
		if remote[i].OutCost == nil && c.Output > 0 {
			d := decimal.NewFromFloat(c.Output)
			remote[i].OutCost = &d
		}
	}
}

// ImportAccountModel is one entry of an explicit model list bound to an
// account — from the public catalog picker or a bulk paste. ModelName is the
// required id; UpstreamModel defaults to it when empty.
type ImportAccountModel struct {
	ModelName        string           `json:"model_name"`
	UpstreamModel    string           `json:"upstream_model,omitempty"`
	UpstreamProtocol string           `json:"upstream_protocol,omitempty"`
	RefInputCost     *decimal.Decimal `json:"ref_input_cost,omitempty"`
	RefOutputCost    *decimal.Decimal `json:"ref_output_cost,omitempty"`
}

// ImportAccountModels binds the given models to the account with the same
// non-destructive upsert rules as upstream sync: existing bindings (matched
// by model_name or upstream_model) are skipped untouched, new ids create
// enabled rows.
func (s *AccountPoolSvc) ImportAccountModels(ctx context.Context, accountID uuid.UUID, models []ImportAccountModel) (*AccountModelsSyncResult, error) {
	remote := make([]remoteModel, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ModelName)
		if id == "" {
			continue
		}
		upstream := strings.TrimSpace(m.UpstreamModel)
		proto, err := normalizeModelProtocol(m.UpstreamProtocol)
		if err != nil {
			return nil, fmt.Errorf("%w: model %s: %v", ErrAccountBadInput, id, err)
		}
		remote = append(remote, remoteModel{ID: id, UpstreamID: upstream, InCost: m.RefInputCost, OutCost: m.RefOutputCost, Protocol: proto})
	}
	return s.bindRemoteModels(ctx, accountID, remote)
}

// bindRemoteModels creates account-model rows for ids the account does not
// already serve. Matching is by model_name or upstream_model; existing rows
// (including disabled ones) are never touched and remote-absent rows are
// never deleted.
func (s *AccountPoolSvc) bindRemoteModels(ctx context.Context, accountID uuid.UUID, remote []remoteModel) (*AccountModelsSyncResult, error) {
	rows, err := s.ListAccountModels(ctx, accountID)
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(rows)*2)
	byID := make(map[string]*ent.UpstreamAccountModel, len(rows)*2)
	for _, row := range rows {
		known[row.ModelName] = struct{}{}
		byID[row.ModelName] = row
		if row.UpstreamModel != "" {
			known[row.UpstreamModel] = struct{}{}
			if _, taken := byID[row.UpstreamModel]; !taken {
				byID[row.UpstreamModel] = row
			}
		}
	}

	res := &AccountModelsSyncResult{RemoteCount: len(remote)}
	for _, m := range remote {
		if _, dup := known[m.ID]; dup {
			res.Existing++
			if enriched, err := s.enrichAccountModel(ctx, byID[m.ID], m); err != nil {
				return nil, err
			} else if enriched {
				res.Updated++
			}
			continue
		}
		upstream := m.UpstreamID
		if upstream == "" {
			upstream = m.ID
		}
		q := s.d.Ent.UpstreamAccountModel.Create().
			SetAccountID(accountID).
			SetModelName(m.ID).
			SetUpstreamModel(upstream).
			SetEnabled(true)
		if m.InCost != nil {
			q = q.SetRefInputCost(*m.InCost)
		}
		if m.OutCost != nil {
			q = q.SetRefOutputCost(*m.OutCost)
		}
		if proto, err := normalizeModelProtocol(m.Protocol); err == nil && proto != "" {
			q = q.SetUpstreamProtocol(proto)
		}
		if _, err := q.Save(ctx); err != nil {
			return nil, err
		}
		known[m.ID] = struct{}{}
		res.Added++
		res.AddedModels = append(res.AddedModels, m.ID)
	}
	return res, nil
}

// enrichAccountModel backfills fields a synced binding never got: an empty
// upstream_model takes the remote id, an unpinned row takes the remote's
// single-endpoint protocol hint, and missing ref costs take the remote's
// prices. Fields the operator set by hand are never touched — the update is
// fill-only, not overwrite. Returns true when a row was changed.
func (s *AccountPoolSvc) enrichAccountModel(ctx context.Context, row *ent.UpstreamAccountModel, m remoteModel) (bool, error) {
	if row == nil {
		return false, nil
	}
	q := s.d.Ent.UpstreamAccountModel.UpdateOneID(row.ID)
	changed := false
	if strings.TrimSpace(row.UpstreamModel) == "" {
		upstream := m.UpstreamID
		if upstream == "" {
			upstream = m.ID
		}
		q = q.SetUpstreamModel(upstream)
		changed = true
	}
	if strings.TrimSpace(row.UpstreamProtocol) == "" {
		if proto, err := normalizeModelProtocol(m.Protocol); err == nil && proto != "" {
			q = q.SetUpstreamProtocol(proto)
			changed = true
		}
	}
	if row.RefInputCost == nil && m.InCost != nil {
		q = q.SetRefInputCost(*m.InCost)
		changed = true
	}
	if row.RefOutputCost == nil && m.OutCost != nil {
		q = q.SetRefOutputCost(*m.OutCost)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if _, err := q.Save(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// staticDefinitionModels returns the definition's model list when the vendor
// declares it has no list endpoint (ModelsPathNone): sync then binds that list
// rather than fetching a URL that can only 404.
func (s *AccountPoolSvc) staticDefinitionModels(ctx context.Context, acct *ent.UpstreamAccount) ([]ProviderDefinitionModel, bool) {
	if s.definitions == nil || strings.TrimSpace(acct.DefinitionID) == "" {
		return nil, false
	}
	view, err := s.definitions.Resolve(ctx, acct.DefinitionID)
	if err != nil || view == nil || view.ModelsPath != ModelsPathNone {
		return nil, false
	}
	return view.Models, true
}

// modelsListURL resolves the endpoint the account's model list is fetched
// from: the provider definition's models_path when the account was created
// from one, else {base}/v1/models (or {base}/models when the base already
// ends in /v1). A definition may also declare an absolute URL.
func (s *AccountPoolSvc) modelsListURL(ctx context.Context, acct *ent.UpstreamAccount) string {
	base := strings.TrimRight(strings.TrimSpace(acct.BaseURL), "/")
	if s.definitions != nil && acct != nil && strings.TrimSpace(acct.DefinitionID) != "" {
		if view, err := s.definitions.Resolve(ctx, acct.DefinitionID); err == nil && view != nil {
			if path := strings.TrimSpace(view.ModelsPath); path != "" && path != ModelsPathNone {
				if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
					return path
				}
				return base + path
			}
		}
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// fetchUpstreamModels GETs the account's model-list endpoint and extracts the
// entries. The shapes handled, in order of specificity:
//
//	{"data": [{"id": …, "cost": {"input","output"}}, …]}   — OpenAI convention
//	{"models": [{"id"|"name": …}, …] or ["id", …]}          — Gemini-ish
//	any object/array containing "models": ["id", …]         — nested vendor
//	  shapes such as data.agents[].models[]
//
// ids are de-duplicated preserving first-seen order.
func fetchUpstreamModels(ctx context.Context, client *http.Client, url, token string, rules schema.HeaderRules) ([]remoteModel, error) {
	if url == "" {
		return nil, fmt.Errorf("%w: account has no base_url", ErrAccountBadInput)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	ApplyHeaderRules(req.Header, rules, token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch upstream models: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("upstream models endpoint returned %d: %s", resp.StatusCode, snippet)
	}
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("parse upstream models: %w", err)
	}
	models := extractRemoteModels(root)
	if len(models) == 0 {
		return nil, fmt.Errorf("upstream models endpoint returned no model ids")
	}
	return models, nil
}

// extractRemoteModels walks the decoded JSON and collects model entries from
// every recognized shape, de-duplicating by id.
func extractRemoteModels(root any) []remoteModel {
	var out []remoteModel
	seen := map[string]struct{}{}
	push := func(m remoteModel) {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" {
			return
		}
		if _, dup := seen[m.ID]; dup {
			return
		}
		seen[m.ID] = struct{}{}
		out = append(out, m)
	}

	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			for _, key := range []string{"data", "models"} {
				list, ok := node[key].([]any)
				if !ok {
					continue
				}
				for _, item := range list {
					switch entry := item.(type) {
					case string:
						push(remoteModel{ID: entry})
					case map[string]any:
						m := remoteModel{
							ID:      stringField(entry, "id", "name", "model"),
							InCost:  costField(entry, "input"),
							OutCost: costField(entry, "output"),
						}
						m.Protocol = protocolHint(entry)
						push(m)
					}
				}
			}
			for _, value := range node {
				walk(value)
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(root)
	return out
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// costField reads a per-1M USD price from common pricing containers:
// cost.input / pricing.input / price.input (and the *_per_million variants).
func costField(m map[string]any, dir string) *decimal.Decimal {
	for _, key := range []string{"cost", "pricing", "price", "billing"} {
		sub, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		for _, cand := range []string{dir, dir + "_per_million", dir + "_price"} {
			if v, ok := sub[cand]; ok {
				if d := toDecimal(v); d != nil {
					return d
				}
			}
		}
	}
	return nil
}

func toDecimal(v any) *decimal.Decimal {
	switch n := v.(type) {
	case float64:
		d := decimal.NewFromFloat(n)
		return &d
	case string:
		if d, err := decimal.NewFromString(strings.TrimSpace(n)); err == nil {
			return &d
		}
	case json.Number:
		if d, err := decimal.NewFromString(n.String()); err == nil {
			return &d
		}
	}
	return nil
}

// protocolHint maps a per-model endpoint declaration (like Command Code's
// supported_endpoints: ["/responses"]) onto the pin vocabulary.
func protocolHint(m map[string]any) string {
	list, ok := m["supported_endpoints"].([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	// When the endpoint set holds exactly one protocol, pinning it is safe;
	// multi-protocol models stay unpinned (client's choice).
	var protos []string
	for _, item := range list {
		s, _ := item.(string)
		switch {
		case strings.Contains(s, "/responses"):
			protos = append(protos, NativeProtocolResponses)
		case strings.Contains(s, "/chat/completions"):
			protos = append(protos, NativeProtocolChat)
		case strings.Contains(s, "/messages"):
			protos = append(protos, NativeProtocolMessages)
		}
	}
	if len(protos) == 1 {
		return protos[0]
	}
	return ""
}
