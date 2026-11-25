// Package biz — Model service.
//
// ModelSvc is the umbrella service for every entity that participates
// in routing: Model, ModelGroup, ModelVendor, UserGroup. To keep this
// file readable the sibling files split off lifecycle methods by
// entity, but the receiver and shared errors all live here.
//
//	model.go         — Model CRUD + Resolve (the routing core) + access
//	                   checks. This file.
//	model_groups.go  — ModelGroup CRUD + attach/detach edges
//	model_vendors.go — ModelVendor CRUD + LoadVendors batch helper
//	user_groups.go   — UserGroup CRUD + SetDefault transaction
package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroupaccount "github.com/jucode/jucode/internal/data/ent/groupaccount"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entgrpmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entaccountmodel "github.com/jucode/jucode/internal/data/ent/upstreamaccountmodel"
)

var (
	ErrModelNotFound        = errors.New("model not found")
	ErrGroupNotFound        = errors.New("model group not found")
	ErrVendorNotFound       = errors.New("model vendor not found")
	ErrUserGroupNotFound    = errors.New("user group not found")
	ErrNoProvider           = errors.New("no provider available for model")
	ErrModelNotInGroup      = errors.New("model not associated with any group")
	ErrGroupDeniedForUser   = errors.New("user not allowed to access this model group")
	ErrGroupDeniedForApiKey = errors.New("api key not allowed to access this model group")
)

// NoProviderError keeps the group that would have served the request attached
// to a routing failure. This lets the availability recorder count the final
// request once without creating a synthetic usage log or multiplying it by
// the number of rejected providers.
type NoProviderError struct {
	GroupID uuid.UUID
	ModelID uuid.UUID
	Detail  string
}

func (e *NoProviderError) Error() string {
	if e.Detail == "" {
		return ErrNoProvider.Error()
	}
	return ErrNoProvider.Error() + ": " + e.Detail
}

func (e *NoProviderError) Unwrap() error { return ErrNoProvider }

func NoProviderGroupID(err error) (uuid.UUID, bool) {
	var target *NoProviderError
	if errors.As(err, &target) && target.GroupID != uuid.Nil {
		return target.GroupID, true
	}
	return uuid.Nil, false
}

func NoProviderModelID(err error) (uuid.UUID, bool) {
	var target *NoProviderError
	if errors.As(err, &target) && target.ModelID != uuid.Nil {
		return target.ModelID, true
	}
	return uuid.Nil, false
}

// ModelSvc resolves model → group → account. It needs the settings service for
// the one global switch routing depends on: whether protocol conversion is
// allowed (which widens the candidate set).
type ModelSvc struct {
	d           *data.Data
	settings    *SettingsSvc
	signalMu    sync.Mutex
	signalCache map[string]routeSignalCacheEntry
}

func NewModelSvc(d *data.Data) *ModelSvc { return &ModelSvc{d: d} }

// SetSettings wires the settings service (the composition root calls it).
func (s *ModelSvc) SetSettings(settings *SettingsSvc) { s.settings = settings }

// --- Model CRUD ---

type CreateModelInput struct {
	Name                   string
	BillingMode            string // per_request | per_token
	PricePerRequest        *decimal.Decimal
	InputUnitPrice         *decimal.Decimal // per 1K tokens
	OutputUnitPrice        *decimal.Decimal
	CacheReadInputPrice    *decimal.Decimal // per 1K cached input tokens (cheaper)
	CacheWriteInputPrice   *decimal.Decimal // per 1K 5m cache-creation tokens (Anthropic ephemeral_5m / GPT cache_write)
	CacheWrite1hInputPrice *decimal.Decimal // per 1K 1h cache-creation tokens (Anthropic ephemeral_1h); nil → 5m price
	ImageInputUnitPrice    *decimal.Decimal // per 1K image-generation tool input tokens
	ImageOutputUnitPrice   *decimal.Decimal // per 1K image-generation tool output tokens
	AudioInputUnitPrice    *decimal.Decimal // per 1K audio input tokens (Realtime / audio-preview chat)
	AudioOutputUnitPrice   *decimal.Decimal // per 1K audio output tokens
	OutputImagePrice       *decimal.Decimal // flat per-image surcharge; stacks on top of token billing
	OutputSecondPrice      *decimal.Decimal // per second of generated audio/video (video, TTS)
	InputCharacterPrice    *decimal.Decimal // per input character (TTS)
	// PriceTiers: nil = no change semantics don't apply on create (absent
	// = flat pricing). Normalized via NormalizePriceTiers before save.
	PriceTiers  []entschema.PriceTier
	Currency    string
	Description string
	VendorID    *uuid.UUID
	// Enabled: nil keeps the schema default (true). Catalog imports pass
	// false so unpriced rows stay off the public list until priced.
	Enabled *bool
	// Capability metadata (see ModelCapabilities); 0 / empty = not set.
	ContextWindow    int
	MaxOutputTokens  int
	ReasoningEfforts []string
	// What clients show; empty = the name.
	DisplayName string
	// Left out of client model pickers; the model stays callable.
	PickerHidden bool
}

// validCapability rejects a negative token count; 0 means "not set".
func validCapability(field string, v int) error {
	if v < 0 {
		return fmt.Errorf("%s must be >= 0", field)
	}
	return nil
}

// normalizeEfforts trims and de-duplicates an effort ladder, keeping order;
// an empty result means "not set".
func normalizeEfforts(in []string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		if e = strings.TrimSpace(e); e != "" && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizePriceTiers validates and canonicalizes a tier list before it
// is persisted: negative prices and non-positive bounds are rejected,
// tiers are sorted ascending by up_to_tokens with the single unbounded
// tier (up_to_tokens=0) forced last, and duplicate bounds are rejected.
// An empty list normalizes to nil (flat pricing).
func NormalizePriceTiers(tiers []entschema.PriceTier) ([]entschema.PriceTier, error) {
	if len(tiers) == 0 {
		return nil, nil
	}
	out := make([]entschema.PriceTier, 0, len(tiers))
	unbounded := 0
	for _, t := range tiers {
		if t.UpToTokens < 0 {
			return nil, errors.New("price tier up_to_tokens must be >= 0")
		}
		if t.UpToTokens == 0 {
			unbounded++
		}
		for _, p := range []*decimal.Decimal{t.InputUnitPrice, t.OutputUnitPrice, t.CacheReadInputPrice, t.CacheWriteInputPrice, t.CacheWrite1hInputPrice} {
			if p != nil && p.IsNegative() {
				return nil, errors.New("price tier prices must be >= 0")
			}
		}
		if t.InputUnitPrice == nil && t.OutputUnitPrice == nil && t.CacheReadInputPrice == nil && t.CacheWriteInputPrice == nil && t.CacheWrite1hInputPrice == nil {
			return nil, errors.New("price tier must set at least one price")
		}
		out = append(out, t)
	}
	if unbounded > 1 {
		return nil, errors.New("at most one unbounded price tier (up_to_tokens=0) allowed")
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].UpToTokens, out[j].UpToTokens
		if a == 0 {
			return false // unbounded sorts last
		}
		if b == 0 {
			return true
		}
		return a < b
	})
	for i := 1; i < len(out); i++ {
		if out[i].UpToTokens != 0 && out[i].UpToTokens == out[i-1].UpToTokens {
			return nil, errors.New("duplicate price tier up_to_tokens bound")
		}
	}
	return out, nil
}

func (s *ModelSvc) CreateModel(ctx context.Context, in CreateModelInput) (*ent.Model, error) {
	q := s.d.Ent.Model.Create().
		SetName(in.Name).
		SetBillingMode(entmodel.BillingMode(in.BillingMode))
	if in.PricePerRequest != nil {
		q = q.SetPricePerRequest(*in.PricePerRequest)
	}
	if in.InputUnitPrice != nil {
		q = q.SetInputUnitPrice(*in.InputUnitPrice)
	}
	if in.OutputUnitPrice != nil {
		q = q.SetOutputUnitPrice(*in.OutputUnitPrice)
	}
	if in.CacheReadInputPrice != nil {
		q = q.SetCacheReadInputPrice(*in.CacheReadInputPrice)
	}
	if in.CacheWriteInputPrice != nil {
		q = q.SetCacheWriteInputPrice(*in.CacheWriteInputPrice)
	}
	if in.CacheWrite1hInputPrice != nil {
		q = q.SetCacheWrite1hInputPrice(*in.CacheWrite1hInputPrice)
	}
	if in.ImageInputUnitPrice != nil {
		q = q.SetImageInputUnitPrice(*in.ImageInputUnitPrice)
	}
	if in.ImageOutputUnitPrice != nil {
		q = q.SetImageOutputUnitPrice(*in.ImageOutputUnitPrice)
	}
	if in.AudioInputUnitPrice != nil {
		q = q.SetAudioInputUnitPrice(*in.AudioInputUnitPrice)
	}
	if in.AudioOutputUnitPrice != nil {
		q = q.SetAudioOutputUnitPrice(*in.AudioOutputUnitPrice)
	}
	if in.OutputImagePrice != nil {
		q = q.SetOutputImagePrice(*in.OutputImagePrice)
	}
	if in.OutputSecondPrice != nil {
		q = q.SetOutputSecondPrice(*in.OutputSecondPrice)
	}
	if in.InputCharacterPrice != nil {
		q = q.SetInputCharacterPrice(*in.InputCharacterPrice)
	}
	if len(in.PriceTiers) > 0 {
		tiers, err := NormalizePriceTiers(in.PriceTiers)
		if err != nil {
			return nil, err
		}
		q = q.SetPriceTiers(tiers)
	}
	if in.Currency != "" {
		q = q.SetCurrency(in.Currency)
	}
	if in.Description != "" {
		q = q.SetDescription(in.Description)
	}
	if in.VendorID != nil {
		q = q.SetVendorID(*in.VendorID)
	}
	if err := validCapability("context_window", in.ContextWindow); err != nil {
		return nil, err
	}
	if err := validCapability("max_output_tokens", in.MaxOutputTokens); err != nil {
		return nil, err
	}
	if in.ContextWindow > 0 {
		q = q.SetContextWindow(in.ContextWindow)
	}
	if name := strings.TrimSpace(in.DisplayName); name != "" {
		q = q.SetDisplayName(name)
	}
	if in.PickerHidden {
		q = q.SetPickerHidden(true)
	}
	if in.MaxOutputTokens > 0 {
		q = q.SetMaxOutputTokens(in.MaxOutputTokens)
	}
	if efforts := normalizeEfforts(in.ReasoningEfforts); efforts != nil {
		q = q.SetReasoningEfforts(efforts)
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	return q.Save(ctx)
}

// ListModels returns models in admin-defined order. Name is the tiebreaker so
// the listing stays stable and deterministic while every row still has the
// default sort_order of 0.
func (s *ModelSvc) ListModels(ctx context.Context) ([]*ent.Model, error) {
	return s.d.Ent.Model.Query().
		Order(ent.Asc(entmodel.FieldSortOrder), ent.Asc(entmodel.FieldName)).
		All(ctx)
}

// ReorderModels applies a new relative order for the given ids.
//
// The admin list is paginated, so `ids` is normally just one page. Numbering
// them 0..len-1 directly would make every page restart at 0 and collide with
// the others, so the submitted ids are spliced back into the full ordering and
// the whole table is renumbered densely. Only rows whose value actually moved
// are written.
func (s *ModelSvc) ReorderModels(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	all, err := s.ListModels(ctx)
	if err != nil {
		return err
	}
	current := make([]uuid.UUID, len(all))
	sortOrders := make(map[uuid.UUID]int, len(all))
	for i, m := range all {
		current[i] = m.ID
		sortOrders[m.ID] = m.SortOrder
	}
	next, err := spliceOrder(current, ids)
	if err != nil {
		return ErrModelNotFound
	}

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }
	for i, id := range next {
		if sortOrders[id] == i {
			continue
		}
		if err := tx.Model.UpdateOneID(id).SetSortOrder(i).Exec(ctx); err != nil {
			rollback()
			if ent.IsNotFound(err) {
				return ErrModelNotFound
			}
			return err
		}
	}
	return tx.Commit()
}

// spliceOrder rearranges the members of subset within current, each landing in
// one of the slots the subset already occupied. Everything not in subset keeps
// its position, which is what makes reordering one page of a paginated list
// leave the other pages untouched.
func spliceOrder(current, subset []uuid.UUID) ([]uuid.UUID, error) {
	want := make(map[uuid.UUID]struct{}, len(subset))
	for _, id := range subset {
		if _, dup := want[id]; dup {
			return nil, fmt.Errorf("duplicate id %s in reorder request", id)
		}
		want[id] = struct{}{}
	}
	slots := make([]int, 0, len(subset))
	for i, id := range current {
		if _, ok := want[id]; ok {
			slots = append(slots, i)
		}
	}
	if len(slots) != len(subset) {
		return nil, fmt.Errorf("reorder request references %d unknown ids", len(subset)-len(slots))
	}
	out := append([]uuid.UUID(nil), current...)
	for k, slot := range slots {
		out[slot] = subset[k]
	}
	return out, nil
}

// PriceOp patches a nillable price column. The zero value leaves the
// column unchanged; ClearPrice() writes NULL (fall back to the text
// unit price / no surcharge); SetPrice writes the value.
type PriceOp struct {
	Value *decimal.Decimal
	Clear bool
}

func SetPrice(d decimal.Decimal) PriceOp { return PriceOp{Value: &d} }
func ClearPrice() PriceOp                { return PriceOp{Clear: true} }

func (p PriceOp) apply(
	q *ent.ModelUpdateOne,
	set func(decimal.Decimal) *ent.ModelUpdateOne,
	clear func() *ent.ModelUpdateOne,
) *ent.ModelUpdateOne {
	switch {
	case p.Clear:
		return clear()
	case p.Value != nil:
		return set(*p.Value)
	default:
		return q
	}
}

type UpdateModelInput struct {
	BillingMode     *string
	PricePerRequest *decimal.Decimal
	InputUnitPrice  *decimal.Decimal
	OutputUnitPrice *decimal.Decimal
	// Optional prices: zero PriceOp = no change. The admin API maps
	// omit → zero, "" → ClearPrice, a number → SetPrice.
	CacheReadInputPrice    PriceOp
	CacheWriteInputPrice   PriceOp
	CacheWrite1hInputPrice PriceOp
	ImageInputUnitPrice    PriceOp
	ImageOutputUnitPrice   PriceOp
	AudioInputUnitPrice    PriceOp
	AudioOutputUnitPrice   PriceOp
	OutputImagePrice       PriceOp
	OutputSecondPrice      PriceOp
	InputCharacterPrice    PriceOp
	// PriceTiers: nil = no change; empty non-nil slice = clear (back to
	// flat pricing); non-empty = replace (after NormalizePriceTiers).
	PriceTiers  *[]entschema.PriceTier
	Currency    *string
	Description *string
	Enabled     *bool
	// VendorID: nil = no change; *VendorID = set to this id; uuid.Nil = clear.
	VendorID *uuid.UUID
	// Capabilities: nil = no change; 0 / empty = clear (not configured).
	ContextWindow    *int
	MaxOutputTokens  *int
	ReasoningEfforts *[]string
	// DisplayName: nil = no change; "" = clear.
	DisplayName *string
	// PickerHidden: nil = no change.
	PickerHidden *bool
}

func (s *ModelSvc) UpdateModel(ctx context.Context, id uuid.UUID, in UpdateModelInput) (*ent.Model, error) {
	q := s.d.Ent.Model.UpdateOneID(id)
	if in.BillingMode != nil {
		q = q.SetBillingMode(entmodel.BillingMode(*in.BillingMode))
	}
	if in.PricePerRequest != nil {
		q = q.SetPricePerRequest(*in.PricePerRequest)
	}
	if in.InputUnitPrice != nil {
		q = q.SetInputUnitPrice(*in.InputUnitPrice)
	}
	if in.OutputUnitPrice != nil {
		q = q.SetOutputUnitPrice(*in.OutputUnitPrice)
	}
	q = in.CacheReadInputPrice.apply(q, q.SetCacheReadInputPrice, q.ClearCacheReadInputPrice)
	q = in.CacheWriteInputPrice.apply(q, q.SetCacheWriteInputPrice, q.ClearCacheWriteInputPrice)
	q = in.CacheWrite1hInputPrice.apply(q, q.SetCacheWrite1hInputPrice, q.ClearCacheWrite1hInputPrice)
	q = in.ImageInputUnitPrice.apply(q, q.SetImageInputUnitPrice, q.ClearImageInputUnitPrice)
	q = in.ImageOutputUnitPrice.apply(q, q.SetImageOutputUnitPrice, q.ClearImageOutputUnitPrice)
	q = in.AudioInputUnitPrice.apply(q, q.SetAudioInputUnitPrice, q.ClearAudioInputUnitPrice)
	q = in.AudioOutputUnitPrice.apply(q, q.SetAudioOutputUnitPrice, q.ClearAudioOutputUnitPrice)
	q = in.OutputImagePrice.apply(q, q.SetOutputImagePrice, q.ClearOutputImagePrice)
	q = in.OutputSecondPrice.apply(q, q.SetOutputSecondPrice, q.ClearOutputSecondPrice)
	q = in.InputCharacterPrice.apply(q, q.SetInputCharacterPrice, q.ClearInputCharacterPrice)
	if in.PriceTiers != nil {
		tiers, err := NormalizePriceTiers(*in.PriceTiers)
		if err != nil {
			return nil, err
		}
		if len(tiers) == 0 {
			q = q.ClearPriceTiers()
		} else {
			q = q.SetPriceTiers(tiers)
		}
	}
	if in.Currency != nil {
		q = q.SetCurrency(*in.Currency)
	}
	if in.Description != nil {
		q = q.SetDescription(*in.Description)
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	if in.VendorID != nil {
		if *in.VendorID == uuid.Nil {
			q = q.ClearVendor()
		} else {
			q = q.SetVendorID(*in.VendorID)
		}
	}
	if in.DisplayName != nil {
		if name := strings.TrimSpace(*in.DisplayName); name == "" {
			q = q.ClearDisplayName()
		} else {
			q = q.SetDisplayName(name)
		}
	}
	if in.PickerHidden != nil {
		q = q.SetPickerHidden(*in.PickerHidden)
	}
	if in.ContextWindow != nil {
		if err := validCapability("context_window", *in.ContextWindow); err != nil {
			return nil, err
		}
		if *in.ContextWindow == 0 {
			q = q.ClearContextWindow()
		} else {
			q = q.SetContextWindow(*in.ContextWindow)
		}
	}
	if in.MaxOutputTokens != nil {
		if err := validCapability("max_output_tokens", *in.MaxOutputTokens); err != nil {
			return nil, err
		}
		if *in.MaxOutputTokens == 0 {
			q = q.ClearMaxOutputTokens()
		} else {
			q = q.SetMaxOutputTokens(*in.MaxOutputTokens)
		}
	}
	if in.ReasoningEfforts != nil {
		if efforts := normalizeEfforts(*in.ReasoningEfforts); efforts == nil {
			q = q.ClearReasoningEfforts()
		} else {
			q = q.SetReasoningEfforts(efforts)
		}
	}
	m, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrModelNotFound
		}
		return nil, err
	}
	return m, nil
}

func (s *ModelSvc) DeleteModel(ctx context.Context, id uuid.UUID) error {
	// Silently drop the model's group bindings (model_group_models), then
	// hard delete. A model carries no financial records of its own — usage
	// logs reference it by name, not by FK — so nothing needs preserving.
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }
	if _, err := tx.ModelGroupModel.Delete().
		Where(entgrpmodel.ModelIDEQ(id)).
		Exec(ctx); err != nil {
		rollback()
		return err
	}
	if err := tx.Model.DeleteOneID(id).Exec(ctx); err != nil {
		rollback()
		if ent.IsNotFound(err) {
			return ErrModelNotFound
		}
		return err
	}
	return tx.Commit()
}

// --- Routing ---

// Route is the decision returned for a single inference request.
type Route struct {
	Model     *ent.Model
	Group     *ent.ModelGroup
	Providers []RouteAccount // ordered: try [0], on failure [1], etc.
	// CandidateGroups is how many groups auto-route had to choose from and
	// GroupIndex which one it landed on (0 = the cheapest). The gateway
	// copies both into the usage log, so a request that quietly skipped the
	// cheap group leaves a trace even when it then succeeds — without them
	// the only observable routing decisions were the ones that failed.
	CandidateGroups int
	GroupIndex      int
}

// RouteAccount is one candidate attempt: an account from the pool, the model
// name to send upstream, and the group-level scheduling inputs. The account
// carries everything else (endpoint, dialect, protocol declaration, credential,
// request shaping, health).
type RouteAccount struct {
	Account       *ent.UpstreamAccount
	UpstreamModel string
	// UpstreamProtocol is the per-model pin from upstream_account_models —
	// when set, the upstream is spoken to in this protocol regardless of the
	// client's dialect (empty follows the account-level rules).
	UpstreamProtocol string
	Priority         int
	Weight           int
	// ContextWindow is what this account gives the model (binding override,
	// else model default); 0 = not configured. The gateway uses it to fail a
	// context-too-long rejection over to a larger-window account.
	ContextWindow int
}

// LegacyProviderID exposes the channel id a migrated account came from, for
// usage attribution of pre-migration rows (nil for accounts created after the
// migration).
func (r RouteAccount) LegacyProviderID() *uuid.UUID {
	if r.Account == nil || r.Account.LegacyProviderID == nil || *r.Account.LegacyProviderID == uuid.Nil {
		return nil
	}
	id := *r.Account.LegacyProviderID
	return &id
}

// Resolve picks ordered providers for the given user-facing model name.
// Returns ErrModelNotFound / ErrModelNotInGroup / ErrNoProvider as
// appropriate so the caller can map to a clean HTTP error.
//
// apiKey is optional — when non-nil and apiKey.AutoRoute=true, the
// group selection step tries groups by lowest rate first (filtered by
// apiKey.AllowedGroupIDs and apiKey.AutoRouteMaxRate), skipping groups
// with no healthy provider. When apiKey is nil OR auto_route disabled,
// falls back to the legacy "first group by name" deterministic pick.
// The optional request protocol filters each group's Provider chain before
// group selection. Omitting it keeps the legacy all-provider behavior for
// non-request callers.
// RoutePreviewProvider is one candidate channel for a model, with the
// routing inputs an operator needs to understand "why did it pick this".
type RoutePreviewProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Protocols is the effective client-facing protocol set (declared or
	// kind-derived) — shows why a provider is/isn't reachable per endpoint.
	Protocols       []string `json:"protocols"`
	Health          string   `json:"health"`
	Priority        int      `json:"priority"`
	Weight          int      `json:"weight"`
	EffectiveWeight int      `json:"effective_weight"` // Weight × health factor (the value the weighted shuffle actually uses)
	UpstreamModel   string   `json:"upstream_model"`
	// UpstreamProtocol is the per-model pin ("" follows account rules).
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
}

// RoutePreview is the resolved candidate set for a model — the configured
// priority/weight order BEFORE the per-request cooldown reorder. Callers
// (admin) overlay live cooldown state separately.
type RoutePreview struct {
	Group     string                 `json:"group"`
	GroupID   string                 `json:"group_id"`
	Providers []RoutePreviewProvider `json:"providers"`
	// Groups is the whole auto-route candidate chain in the order an
	// unrestricted auto-route key would try it, each annotated with why it
	// was or was not taken. Without this the preview could only ever show
	// one group and silently hid every fall-through decision.
	Groups []RoutePreviewGroup `json:"groups"`
}

// Reasons a candidate group was not the one auto-route would pick.
const (
	RouteSkipNoProviders  = "no_providers"
	RouteSkipAllCooling   = "all_providers_cooling"
	RouteSkipOtherBilling = "billing_source_mismatch"
)

type RoutePreviewGroup struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	BillingSource  string                 `json:"billing_source"`
	RateMultiplier float64                `json:"rate_multiplier"`
	Chosen         bool                   `json:"chosen"`
	SkipReason     string                 `json:"skip_reason,omitempty"`
	Providers      []RoutePreviewProvider `json:"providers"`
}

// RoutePreview resolves a model the same way a real request would (legacy
// no-api-key path) and reports the ordered candidate providers with their
// effective weights and health — a read-only "what would routing do" view.
// It models an unrestricted auto-route key (no allowed-group list, no rate
// cap) so an operator sees the full chain; a real key with those limits set
// would see a prefix of it.
func (s *ModelSvc) RoutePreview(ctx context.Context, modelName string, protocols ...RequestProtocol) (*RoutePreview, error) {
	protocol := NormalizeRequestProtocol(protocols...)
	m, err := s.d.Ent.Model.Query().Where(entmodel.NameEQ(modelName), entmodel.EnabledEQ(true)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrModelNotFound
		}
		return nil, err
	}
	candidates, err := s.modelGroups(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	// Same ordering auto-route uses: cheapest first, name breaks ties.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].RateMultiplier != candidates[j].RateMultiplier {
			return candidates[i].RateMultiplier < candidates[j].RateMultiplier
		}
		return candidates[i].Name < candidates[j].Name
	})
	out := &RoutePreview{Groups: make([]RoutePreviewGroup, 0, len(candidates))}
	var stats resolveRejectStats
	var primaryBilling entgroup.BillingSource
	anchored := false
	chosen := false
	// Mirrors chooseRouteGroup: the billing anchor is the first group that is
	// actually usable, so the preview and the resolver skip the same groups.
	anchor := func(g *ent.ModelGroup) {
		if !anchored {
			primaryBilling = g.BillingSource
			anchored = true
		}
	}
	for _, group := range candidates {
		row := RoutePreviewGroup{
			ID:             group.ID.String(),
			Name:           group.Name,
			BillingSource:  string(group.BillingSource),
			RateMultiplier: group.RateMultiplier,
		}
		providers, err := s.groupRouteAccounts(ctx, m, group, &stats, protocol)
		if err != nil {
			return nil, err
		}
		orderRouteAccounts(providers)
		for _, rp := range providers {
			if rp.Account == nil {
				continue
			}
			row.Providers = append(row.Providers, RoutePreviewProvider{
				ID:               rp.Account.ID.String(),
				Name:             rp.Account.Name,
				Kind:             accountPreviewKind(rp.Account),
				Protocols:        AccountProtocols(rp.Account),
				Health:           string(rp.Account.Health),
				Priority:         rp.Priority,
				Weight:           rp.Weight,
				EffectiveWeight:  effectiveRouteWeight(rp),
				UpstreamModel:    rp.UpstreamModel,
				UpstreamProtocol: rp.UpstreamProtocol,
			})
		}
		switch {
		case anchored && group.BillingSource != primaryBilling:
			row.SkipReason = RouteSkipOtherBilling
		case len(providers) == 0:
			row.SkipReason = RouteSkipNoProviders
		case chosen:
			// A cheaper group already won; this one is only a fallback.
			anchor(group)
		case len(candidates) == 1 || s.anyProviderSchedulable(ctx, providers, m.Name):
			anchor(group)
			row.Chosen = true
			chosen = true
			out.Group = group.Name
			out.GroupID = row.ID
			out.Providers = row.Providers
		default:
			anchor(group)
			row.SkipReason = RouteSkipAllCooling
		}
		out.Groups = append(out.Groups, row)
	}
	if !chosen {
		// Every candidate was cooling: Resolve falls back to the first
		// configured group, so the preview must show the same thing.
		for i := range out.Groups {
			if out.Groups[i].SkipReason == RouteSkipAllCooling {
				out.Groups[i].Chosen = true
				out.Groups[i].SkipReason = ""
				out.Group = out.Groups[i].Name
				out.GroupID = out.Groups[i].ID
				out.Providers = out.Groups[i].Providers
				break
			}
		}
	}
	if out.GroupID == "" {
		return nil, stats.err(modelName, candidates[0].ID, m.ID)
	}
	return out, nil
}

// ctxGroupOverrideKey carries an explicit per-request ModelGroup choice
// (set from the X-JuCode-Group header by the gateway middleware). The
// override narrows Resolve's candidate set to that single group; it does
// NOT widen access — the API key's allowed_group_ids filter still applies
// inside selectGroups.
type ctxGroupOverrideKey struct{}

// WithGroupOverride returns a context that pins Resolve to one group.
func WithGroupOverride(ctx context.Context, groupID uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxGroupOverrideKey{}, groupID)
}

func groupOverrideFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxGroupOverrideKey{}).(uuid.UUID)
	return id, ok
}

// ctxBillingSourceKey narrows Resolve to groups of one billing source (set
// from the X-JuCode-Billing-Source header). Auto-routing then still picks by
// price and availability, just never across plan and balance billing.
type ctxBillingSourceKey struct{}

// WithBillingSourceFilter returns a context that keeps only groups billed
// from source.
func WithBillingSourceFilter(ctx context.Context, source entgroup.BillingSource) context.Context {
	return context.WithValue(ctx, ctxBillingSourceKey{}, source)
}

func billingSourceFilterFrom(ctx context.Context) (entgroup.BillingSource, bool) {
	source, ok := ctx.Value(ctxBillingSourceKey{}).(entgroup.BillingSource)
	return source, ok
}

type ctxStickyGroupKey struct{}

type stickyGroupIDs struct {
	session uuid.UUID
	model   uuid.UUID
}

// WithStickyGroups asks Resolve to keep the conversation on these groups.
// session is the group the conversation was given; model is set only when
// this model does not use that group.
func WithStickyGroups(ctx context.Context, session, model uuid.UUID) context.Context {
	if session == uuid.Nil && model == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, ctxStickyGroupKey{}, stickyGroupIDs{session: session, model: model})
}

func stickyGroupsFrom(ctx context.Context) (session, model uuid.UUID) {
	ids, _ := ctx.Value(ctxStickyGroupKey{}).(stickyGroupIDs)
	return ids.session, ids.model
}

func groupIDsOf(groups []*ent.ModelGroup) []uuid.UUID {
	ids := make([]uuid.UUID, len(groups))
	for i, group := range groups {
		ids[i] = group.ID
	}
	return ids
}

func routePrefFrom(model *ent.Model, signals map[uuid.UUID]GroupRouteSignal, session, modelSticky uuid.UUID) routePref {
	pref := routePref{Signals: signals, Sticky: session, ModelSticky: modelSticky}
	if model == nil {
		return pref
	}
	pref.ModelName = model.Name
	if model.InputUnitPrice != nil {
		pref.HasInput = true
		pref.Input, _ = model.InputUnitPrice.Float64()
		pref.CacheRead = pref.Input
	}
	if model.CacheReadInputPrice != nil {
		pref.CacheRead, _ = model.CacheReadInputPrice.Float64()
	}
	if model.OutputUnitPrice != nil {
		pref.Output, _ = model.OutputUnitPrice.Float64()
	}
	return pref
}

func (s *ModelSvc) Resolve(ctx context.Context, modelName string, user *ent.User, apiKey *ent.ApiKey, protocols ...RequestProtocol) (*Route, error) {
	protocol := NormalizeRequestProtocol(protocols...)
	m, err := s.d.Ent.Model.Query().Where(entmodel.NameEQ(modelName), entmodel.EnabledEQ(true)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrModelNotFound
		}
		return nil, err
	}

	groups, err := s.modelGroups(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if override, ok := groupOverrideFrom(ctx); ok {
		narrowed := groups[:0]
		for _, g := range groups {
			if g.ID == override {
				narrowed = append(narrowed, g)
			}
		}
		if len(narrowed) == 0 {
			return nil, ErrModelNotInGroup
		}
		groups = narrowed
	}
	if source, ok := billingSourceFilterFrom(ctx); ok {
		narrowed := groups[:0]
		for _, g := range groups {
			if g.BillingSource == source {
				narrowed = append(narrowed, g)
			}
		}
		if len(narrowed) == 0 {
			return nil, ErrModelNotInGroup
		}
		groups = narrowed
	}

	accessible, err := s.filterGroupsForUser(ctx, user, groups)
	if err != nil {
		return nil, err
	}
	if len(accessible) == 0 {
		if len(groups) > 0 {
			return nil, ErrGroupDeniedForUser
		}
		return nil, ErrModelNotInGroup
	}
	groups = accessible

	// Pick the ordered group candidates. Auto-route may fall through to
	// the next group when a cheaper group has no currently healthy provider.
	sessionGroup, modelGroup := stickyGroupsFrom(ctx)
	groupCandidates, err := selectGroups(groups, apiKey, routePrefFrom(m, s.routeSignals(ctx, m.Name, groupIDsOf(groups)), sessionGroup, modelGroup))
	if err != nil {
		return nil, err
	}

	stats := resolveRejectStats{groups: len(groupCandidates)}
	// Providers are loaded lazily and memoized: chooseRouteGroup may ask
	// about a group twice (once for "is it configured", once to fall back to
	// it) and each load is several queries.
	loaded := make(map[int][]RouteAccount, len(groupCandidates))
	var loadErr error
	probe := routeGroupProbe{
		providers: func(i int) []RouteAccount {
			if ps, ok := loaded[i]; ok {
				return ps
			}
			ps, err := s.groupRouteAccounts(ctx, m, groupCandidates[i], &stats, protocol)
			if err != nil {
				loadErr = err
				ps = nil
			}
			orderRouteAccounts(ps)
			loaded[i] = ps
			return ps
		},
		schedulable: func(i int) bool {
			// Only pay for the lookup when there is somewhere to fall to.
			if len(groupCandidates) == 1 {
				return true
			}
			return s.anyProviderSchedulable(ctx, loaded[i], m.Name)
		},
	}
	holdID := uuid.Nil
	pin := uuid.Nil
	if apiKey != nil {
		pin = pinnedGroup(apiKey.ModelPins, m.Name)
	}
	if held := heldRouteGroup(groupCandidates, modelGroup, pin, sessionGroup); held != nil {
		holdID = held.ID
	}
	idx, cooling, heldOK := stayOnHeldGroup(groupCandidates, holdID, probe)
	if !heldOK {
		idx, cooling = chooseRouteGroup(groupCandidates, probe, &stats)
	}
	if loadErr != nil {
		return nil, loadErr
	}
	if idx < 0 {
		groupID := uuid.Nil
		if len(groupCandidates) > 0 {
			groupID = groupCandidates[0].ID
		}
		return nil, stats.err(modelName, groupID, m.ID)
	}
	// Log the decision whenever auto-route did something other than take the
	// cheapest group, which is the case an operator has to be able to explain
	// after the fact. The happy path stays silent.
	if cooling || idx > 0 {
		slog.Info("route.group_fallthrough",
			"model", modelName,
			"group", groupCandidates[idx].Name,
			"group_index", idx,
			"candidate_groups", len(groupCandidates),
			"all_cooling", cooling,
			"groups_unusable", stats.groupsFullyCooling,
			"groups_without_providers", stats.groupsWithoutProviderLinks,
			"groups_other_billing", stats.groupsWithOtherBilling,
		)
	}
	return &Route{
		Model: m, Group: groupCandidates[idx], Providers: loaded[idx],
		CandidateGroups: len(groupCandidates), GroupIndex: idx,
	}, nil
}

// routeGroupProbe supplies chooseRouteGroup with the two facts it needs about
// a candidate group, injected so the walk can be tested without a database.
type routeGroupProbe struct {
	providers   func(i int) []RouteAccount
	schedulable func(i int) bool
}

// stayOnHeldGroup keeps a conversation on the group it was already given.
// A cooldown is not a reason to leave. No configured provider is: the
// caller then walks the remaining candidates.
func stayOnHeldGroup(candidates []*ent.ModelGroup, hold uuid.UUID, probe routeGroupProbe) (idx int, cooling bool, ok bool) {
	if hold == uuid.Nil {
		return 0, false, false
	}
	for i, group := range candidates {
		if group == nil || group.ID != hold {
			continue
		}
		if len(probe.providers(i)) == 0 {
			return 0, false, false
		}
		return i, !probe.schedulable(i), true
	}
	return 0, false, false
}

// chooseRouteGroup implements auto-route's group fall-through. Candidates
// arrive cheapest-first; it returns the index of the group to use.
//
// A group is skipped when it has no usable providers, or when not one of its
// accounts can serve right now (see anyProviderSchedulable — health state and
// runtime cooldown together) — the latter is the whole point: before this
// existed, a group whose only channel was parked still won and the request
// died there instead of trying the next group.
//
// Fall-through never crosses the first candidate's billing source, so a
// plan-only group cannot spill into a balance-only one and start charging.
//
// When no candidate can serve there is no better choice than the cheapest
// configured one, so that is returned with cooling=true rather than failing
// the request outright. Returns -1 when nothing is configured.
func chooseRouteGroup(candidates []*ent.ModelGroup, probe routeGroupProbe, stats *resolveRejectStats) (idx int, cooling bool) {
	firstConfigured := -1
	var primaryBilling entgroup.BillingSource
	for i, group := range candidates {
		// The billing anchor is the first group that is actually usable, not
		// simply the first candidate: an unconfigured group at the head of
		// the list must not veto every group that bills differently.
		if firstConfigured >= 0 && group.BillingSource != primaryBilling {
			stats.groupsWithOtherBilling++
			continue
		}
		if len(probe.providers(i)) == 0 {
			continue
		}
		if firstConfigured < 0 {
			firstConfigured = i
			primaryBilling = group.BillingSource
		}
		if probe.schedulable(i) {
			return i, false
		}
		stats.groupsFullyCooling++
	}
	if firstConfigured >= 0 {
		return firstConfigured, true
	}
	return -1, false
}

// modelGroups returns every group the model is linked to, unfiltered.
func (s *ModelSvc) modelGroups(ctx context.Context, modelID uuid.UUID) ([]*ent.ModelGroup, error) {
	links, err := s.d.Ent.ModelGroupModel.Query().
		Where(entgrpmodel.ModelIDEQ(modelID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, ErrModelNotInGroup
	}
	groupIDs := make([]uuid.UUID, 0, len(links))
	for _, l := range links {
		groupIDs = append(groupIDs, l.GroupID)
	}
	groups, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.IDIn(groupIDs...), entgroup.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, ErrModelNotInGroup
	}
	return groups, nil
}

// groupRouteAccounts resolves one group's enabled providers for this model,
// applying the provider-model mapping. Rejection counts go into stats so a
// failed resolve can explain which step dropped everything.
func (s *ModelSvc) groupRouteAccounts(ctx context.Context, m *ent.Model, group *ent.ModelGroup, stats *resolveRejectStats, protocol RequestProtocol) ([]RouteAccount, error) {
	links, err := s.d.Ent.GroupAccount.Query().
		Where(entgroupaccount.GroupIDEQ(group.ID), entgroupaccount.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		stats.groupsWithoutProviderLinks++
		return nil, nil
	}
	stats.providerLinks += len(links)

	accountIDs := make([]uuid.UUID, 0, len(links))
	for _, link := range links {
		accountIDs = append(accountIDs, link.AccountID)
	}
	accounts, err := s.d.Ent.UpstreamAccount.Query().
		Where(entaccount.IDIn(accountIDs...), entaccount.DeletedAtIsNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	accountByID := make(map[uuid.UUID]*ent.UpstreamAccount, len(accounts))
	for _, acct := range accounts {
		accountByID[acct.ID] = acct
	}
	models, err := s.d.Ent.UpstreamAccountModel.Query().
		Where(entaccountmodel.AccountIDIn(accountIDs...),
			entaccountmodel.ModelNameEQ(m.Name),
			entaccountmodel.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	modelByAccountID := make(map[uuid.UUID]*ent.UpstreamAccountModel, len(models))
	for _, row := range models {
		modelByAccountID[row.AccountID] = row
	}

	// The group's own window for the model, between the model default and an
	// account's override (AccountContextWindow).
	var groupWindow *int
	if link, err := s.d.Ent.ModelGroupModel.Query().
		Where(entgrpmodel.GroupIDEQ(group.ID), entgrpmodel.ModelIDEQ(m.ID)).
		Only(ctx); err == nil {
		groupWindow = link.ContextWindow
	} else if !ent.IsNotFound(err) {
		return nil, err
	}

	conversionEnabled := s.conversionEnabled(ctx)
	candidates := make([]RouteAccount, 0, len(links))
	for _, link := range links {
		acct, ok := accountByID[link.AccountID]
		if !ok {
			stats.missingProviders++
			continue
		}
		if acct.Status != entaccount.StatusActive || !acct.Schedulable {
			stats.disabledProviders++
			continue
		}
		bound, ok := modelByAccountID[acct.ID]
		if !ok {
			stats.providerModelMismatches++
			continue
		}
		// A member whitelist narrows this account to the named models inside
		// this group; an empty list means every configured model serves.
		if len(link.ModelNames) > 0 && !slices.Contains(link.ModelNames, m.Name) {
			stats.providerModelMismatches++
			continue
		}
		if !AccountServesModelRequest(acct, bound.UpstreamProtocol, protocol, conversionEnabled) {
			stats.providerProtocolMismatches++
			continue
		}
		upstream := m.Name
		if bound.UpstreamModel != "" {
			upstream = bound.UpstreamModel
		}
		// The group link carries the per-group override; a zero on either side
		// means "inherit the account's own scheduling input", so an operator can
		// set the account's priority/weight once and have every group follow it.
		priority, weight := link.Priority, link.Weight
		if priority == 0 {
			priority = acct.Priority
		}
		if weight == 0 {
			weight = acct.Weight
		}
		candidates = append(candidates, RouteAccount{
			Account:          acct,
			UpstreamModel:    upstream,
			UpstreamProtocol: bound.UpstreamProtocol,
			Priority:         priority,
			Weight:           weight,
			ContextWindow:    AccountContextWindow(m, groupWindow, bound),
		})
	}
	return candidates, nil
}

// conversionEnabled reports whether the site allows protocol conversion — the
// switch that decides if a non-native account is still a candidate.
func (s *ModelSvc) conversionEnabled(ctx context.Context) bool {
	if s.settings == nil {
		return false
	}
	site, err := s.settings.GetSite(ctx)
	if err != nil || site == nil {
		return false
	}
	return site.FormatConversionEnabled
}

// anyProviderSchedulable reports whether at least one account in the group can
// actually serve right now — the signal that decides whether auto-route skips
// the whole group. An account qualifies only if BOTH halves agree:
//
//   - its own health state allows scheduling (AccountSchedulable: the 429
//     window in rate_limit_reset_at, overload_until, temp_unschedulable_until,
//     the per-model cooldown map, and credential expiry);
//   - it is not in Redis runtime cooldown.
//
// The two are written by different paths and neither implies the other. This
// used to consult the Redis half alone, so a group whose every account was
// parked by an upstream 429 still won auto-route on price and the request
// died inside it instead of falling through to the next group — the single
// biggest reason auto-route looked broken to users.
//
// The Redis half still fails open: on cache trouble the answer falls back to
// the health state alone, which has already said yes, so a hiccup can never
// make resolution worse than it was before fall-through existed.
func (s *ModelSvc) anyProviderSchedulable(ctx context.Context, providers []RouteAccount, model string) bool {
	now := time.Now()
	usable := make([]RouteAccount, 0, len(providers))
	for _, rp := range providers {
		if rp.Account != nil && AccountSchedulable(rp.Account, model, now) {
			usable = append(usable, rp)
		}
	}
	if len(usable) == 0 {
		return false
	}
	if s.d == nil || s.d.Redis == nil {
		return true
	}
	keys := make([]string, 0, len(usable))
	for _, rp := range usable {
		keys = append(keys, accountCooldownKey(rp.Account.ID))
	}
	vals, err := s.d.Redis.MGet(ctx, keys...).Result()
	if err != nil || len(vals) != len(keys) {
		return true
	}
	for _, v := range vals {
		if v == nil {
			return true
		}
	}
	return false
}

// accountPreviewKind keeps the routing board's existing "kind" label: the
// dialect family the account speaks natively.
func accountPreviewKind(acct *ent.UpstreamAccount) string {
	if acct == nil {
		return ""
	}
	for _, proto := range AccountProtocols(acct) {
		if proto == NativeProtocolMessages {
			return "anthropic"
		}
	}
	return "openai_compatible"
}

func orderRouteAccounts(providers []RouteAccount) {
	orderRouteAccountsWithPicker(providers, rand.Intn)
}

func orderRouteAccountsWithPicker(providers []RouteAccount, pick func(int) int) {
	sort.SliceStable(providers, func(i, j int) bool {
		return providers[i].Priority < providers[j].Priority
	})
	for start := 0; start < len(providers); {
		end := start + 1
		for end < len(providers) && providers[end].Priority == providers[start].Priority {
			end++
		}
		weightedShuffleRouteAccounts(providers[start:end], pick)
		start = end
	}
}

// healthWeightFactor scales a provider's routing weight by its observed
// health. degraded/unhealthy providers are de-prioritized within their
// priority tier but never excluded (factor stays ≥1) so health flapping
// can't starve a provider entirely and a recovering provider keeps a
// small chance of being probed. healthy/unknown share the full factor so
// behavior is unchanged until a provider is actually marked degraded.
func healthWeightFactor(h entaccount.Health) int {
	switch h {
	case entaccount.HealthDegraded:
		return 3
	case entaccount.HealthUnhealthy:
		return 1
	default: // healthy / unknown
		return 10
	}
}

// effectiveRouteWeight is the weight used for in-tier weighted shuffle:
// the configured Weight scaled by the health factor. A non-positive
// configured weight stays 0 (last-resort, unchanged), so when every
// provider is healthy the relative distribution is identical to before.
func effectiveRouteWeight(rp RouteAccount) int {
	if rp.Weight <= 0 {
		return 0
	}
	if rp.Account == nil {
		return rp.Weight * 10
	}
	return rp.Weight * healthWeightFactor(rp.Account.Health)
}

func weightedShuffleRouteAccounts(providers []RouteAccount, pick func(int) int) {
	for i := 0; i < len(providers)-1; i++ {
		total := 0
		for j := i; j < len(providers); j++ {
			if w := effectiveRouteWeight(providers[j]); w > 0 {
				total += w
			}
		}
		if total <= 0 {
			chosen := i + pick(len(providers)-i)
			providers[i], providers[chosen] = providers[chosen], providers[i]
			continue
		}
		n := pick(total)
		chosen := i
		seen := 0
		for j := i; j < len(providers); j++ {
			w := effectiveRouteWeight(providers[j])
			if w <= 0 {
				continue
			}
			seen += w
			if n < seen {
				chosen = j
				break
			}
		}
		providers[i], providers[chosen] = providers[chosen], providers[i]
	}
}

type resolveRejectStats struct {
	groups                     int
	groupsWithoutProviderLinks int
	groupsWithOtherBilling     int
	groupsFullyCooling         int
	providerLinks              int
	missingProviders           int
	disabledProviders          int
	unhealthyProviders         int
	providerModelMismatches    int
	providerProtocolMismatches int
}

func (s resolveRejectStats) err(modelName string, groupID, modelID uuid.UUID) error {
	parts := []string{"no available provider for model " + modelName}
	if s.groups > 0 {
		parts = append(parts, "candidate_groups="+itoa(s.groups))
	}
	if s.groupsWithoutProviderLinks > 0 {
		parts = append(parts, "groups_without_provider_links="+itoa(s.groupsWithoutProviderLinks))
	}
	if s.providerLinks > 0 {
		parts = append(parts, "provider_links="+itoa(s.providerLinks))
	}
	if s.missingProviders > 0 {
		parts = append(parts, "missing_providers="+itoa(s.missingProviders))
	}
	if s.disabledProviders > 0 {
		parts = append(parts, "disabled_providers="+itoa(s.disabledProviders))
	}
	if s.unhealthyProviders > 0 {
		parts = append(parts, "unhealthy_providers="+itoa(s.unhealthyProviders))
	}
	if s.providerModelMismatches > 0 {
		parts = append(parts, "provider_model_mismatches="+itoa(s.providerModelMismatches))
	}
	if s.providerProtocolMismatches > 0 {
		parts = append(parts, "provider_protocol_mismatches="+itoa(s.providerProtocolMismatches))
	}
	return &NoProviderError{GroupID: groupID, ModelID: modelID, Detail: strings.Join(parts, "; ")}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := [20]byte{}
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

// --- Access checks ---

// CheckAccess gates a routed request through exclusive-group grants
// (and the legacy JSON allowlist) plus the api-key scope filter.
//   - public group → anyone; exclusive / legacy-restricted → grant or JSON.
//   - apiKey.AllowedGroupIds empty → no key-level restriction; non-empty →
//     route.Group.ID must be in the list.
//
// Resolve already drops inaccessible groups; this remains the last gate
// for pinned headers and listing endpoints.
func (s *ModelSvc) CheckAccess(ctx context.Context, user *ent.User, apiKey *ent.ApiKey, group *ent.ModelGroup) error {
	if group == nil {
		return nil
	}
	if user == nil {
		if groupIsRestricted(group) {
			return ErrGroupDeniedForUser
		}
	} else {
		granted, err := s.grantedGroups(ctx, user)
		if err != nil {
			return err
		}
		if !userMayEnterGroup(user, group, granted) {
			return ErrGroupDeniedForUser
		}
	}
	if apiKey != nil && len(apiKey.AllowedGroupIds) > 0 {
		ok := false
		for _, id := range apiKey.AllowedGroupIds {
			if id == group.ID {
				ok = true
				break
			}
		}
		if !ok {
			return ErrGroupDeniedForApiKey
		}
	}
	return nil
}

// AccessibleModels returns the enabled models the caller may actually route
// to, i.e. those linked to at least one enabled ModelGroup that passes
// CheckAccess for this user + api key. Powers the model-listing endpoints so
// they agree with what a generation request would accept.
//
// A model attached to no enabled group is dropped: Resolve would reject it
// with ErrModelNotInGroup, so advertising it is a lie.
func (s *ModelSvc) AccessibleModels(ctx context.Context, user *ent.User, apiKey *ent.ApiKey) ([]*ent.Model, error) {
	models, _, err := s.AccessibleModelGroups(ctx, user, apiKey)
	return models, err
}

// AccessibleModelGroups is AccessibleModels plus, per returned model, the
// groups the caller passes CheckAccess on — the set its capabilities
// (ModelCapabilities) are computed over.
func (s *ModelSvc) AccessibleModelGroups(ctx context.Context, user *ent.User, apiKey *ent.ApiKey) ([]*ent.Model, map[uuid.UUID][]*ent.ModelGroup, error) {
	all, err := s.ListModels(ctx)
	if err != nil {
		return nil, nil, err
	}
	enabled := make([]*ent.Model, 0, len(all))
	for _, m := range all {
		if m.Enabled {
			enabled = append(enabled, m)
		}
	}
	// LoadGroupsForModels already restricts to enabled groups.
	byModel, err := s.LoadGroupsForModels(ctx, enabled)
	if err != nil {
		return nil, nil, err
	}
	granted, err := s.LoadGrantedGroupIDs(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	ctx = WithGrantedGroups(ctx, granted)
	models := s.filterAccessibleModels(ctx, enabled, byModel, user, apiKey)
	groups := make(map[uuid.UUID][]*ent.ModelGroup, len(models))
	for _, m := range models {
		for _, g := range byModel[m.ID] {
			if s.CheckAccess(ctx, user, apiKey, g) == nil {
				groups[m.ID] = append(groups[m.ID], g)
			}
		}
	}
	return models, groups, nil
}

// filterAccessibleModels keeps the models with at least one group the caller
// passes CheckAccess on, preserving the input order.
func (s *ModelSvc) filterAccessibleModels(ctx context.Context, models []*ent.Model, byModel map[uuid.UUID][]*ent.ModelGroup, user *ent.User, apiKey *ent.ApiKey) []*ent.Model {
	out := make([]*ent.Model, 0, len(models))
	for _, m := range models {
		for _, g := range byModel[m.ID] {
			if err := s.CheckAccess(ctx, user, apiKey, g); err == nil {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// AccessibleGroups returns the model groups the given user (with optional
// per-key restriction) may currently access. Powers the /me/available-groups
// endpoint that drives the API key creation picker.
func (s *ModelSvc) AccessibleGroups(ctx context.Context, user *ent.User) ([]*ent.ModelGroup, error) {
	all, err := s.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	granted, err := s.LoadGrantedGroupIDs(ctx, user)
	if err != nil {
		return nil, err
	}
	ctx = WithGrantedGroups(ctx, granted)
	out := make([]*ent.ModelGroup, 0, len(all))
	for _, g := range all {
		if !g.Enabled {
			continue
		}
		if err := s.CheckAccess(ctx, user, nil, g); err == nil {
			out = append(out, g)
		}
	}
	return out, nil
}
