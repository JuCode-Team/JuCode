package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entgrpmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entmember "github.com/jucode/jucode/internal/data/ent/organizationmember"
	"github.com/jucode/jucode/internal/data/ent/predicate"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/pkg/idgen"
	"github.com/jucode/jucode/internal/region"
)

var (
	ErrApiKeyNotFound     = errors.New("api key not found")
	ErrApiKeyExpired      = errors.New("api key expired")
	ErrApiKeyUnrevealable = errors.New("api key was created before plaintext storage was enabled; please rotate the key to enable reveal")
	ErrApiKeyOrgDenied    = errors.New("user is not a member of this organization")
	ErrApiKeyCostLimit    = errors.New("api key cost limit exceeded")
	ErrInvalidCostLimit   = errors.New("cost limit must not be negative")
	ErrApiKeyIPBlocked    = errors.New("client ip is not in the api key's ip whitelist")
	ErrApiKeySystem       = errors.New("实验室密钥不能修改或删除，也不能当作 API Key 调用")
	// ErrPlanGroupsNeedSubscription rejects restricting a key to plan-only
	// groups when its owner has no active subscription. A credit pack
	// ("额度套餐") only grants balance, so it cannot back a 套餐专属 key.
	ErrPlanGroupsNeedSubscription = errors.New("套餐专属分组仅限有效的订阅套餐使用。额度套餐发放的额度与余额相同，请改用余额分组")
	ErrApiKeyNotSystem            = errors.New("只有系统密钥可以重新生成")
)

const labKeyName = "实验室"

type ApiKeySvc struct {
	d      *data.Data
	cipher *cryptopkg.Cipher

	cacheMu sync.Mutex
	cache   map[string]apiKeyCacheEntry
}

type apiKeyCacheEntry struct {
	apiKey    *ent.ApiKey
	user      *ent.User
	expiresAt time.Time
}

func NewApiKeySvc(d *data.Data) *ApiKeySvc {
	return &ApiKeySvc{d: d, cache: make(map[string]apiKeyCacheEntry)}
}

// RegionalSnapshot resolves an API-key hash on the authoritative node and
// returns the minimum account policy needed by an edge. The plaintext key is
// deliberately not part of the response; the edge already has the caller's
// token and compares its SHA-256 hash locally.
func (s *ApiKeySvc) RegionalSnapshot(ctx context.Context, hash string) (*region.AccountSnapshot, error) {
	rec, err := s.d.Ent.ApiKey.Query().Where(entapikey.HashEQ(hash)).WithUser().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrApiKeyNotFound
		}
		return nil, err
	}
	user, err := rec.Edges.UserOrErr()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if rec.RevokedAt != nil || (rec.ExpiresAt != nil && !now.Before(*rec.ExpiresAt)) {
		return nil, ErrApiKeyNotFound
	}

	billingScope := string(rec.BillingScope)
	organizationID := rec.OrganizationID
	balance := user.Balance
	if rec.BillingScope == entapikey.BillingScopeOrganization {
		if organizationID == nil || *organizationID == uuid.Nil {
			return nil, ErrApiKeyOrgDenied
		}
		if _, err := s.d.Ent.OrganizationMember.Query().
			Where(entmember.OrganizationIDEQ(*organizationID), entmember.UserIDEQ(user.ID)).
			Only(ctx); err != nil {
			if ent.IsNotFound(err) {
				return nil, ErrApiKeyOrgDenied
			}
			return nil, err
		}
		organization, err := s.d.Ent.Organization.Get(ctx, *organizationID)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil, ErrApiKeyOrgDenied
			}
			return nil, err
		}
		balance = organization.Balance
	}
	allowedModels, modelPrices, err := s.regionalAllowedModels(ctx, user, rec)
	if err != nil {
		return nil, err
	}
	// The key's cost limits travel with the snapshot so the edge can trip
	// locally; control.SQLLedger.GrantLease independently re-checks them against the
	// authoritative usage history before freezing credit.
	costLimit := func(v *decimal.Decimal) string {
		if v == nil || v.Sign() <= 0 {
			return ""
		}
		return v.String()
	}
	return &region.AccountSnapshot{
		Version: rec.UpdatedAt.UnixNano(), UserID: user.ID, APIKeyID: rec.ID,
		APIKeyHash: rec.Hash, APIKeyName: rec.Name, BillingScope: billingScope,
		OrganizationID: organizationID, Status: string(user.Status),
		Revoked: rec.RevokedAt != nil, ExpiresAt: rec.ExpiresAt,
		AllowedGroupIDs: rec.AllowedGroupIds, IPWhitelist: rec.IPWhitelist,
		AllowedModels: allowedModels, ModelPrices: modelPrices,
		BalanceCNY:        balance.String(),
		DailyCostLimitCNY: costLimit(rec.DailyCostLimit), MonthlyCostLimitCNY: costLimit(rec.MonthlyCostLimit),
		TotalCostLimitCNY: costLimit(rec.TotalCostLimit), UpdatedAt: rec.UpdatedAt,
	}, nil
}

// regionalAllowedModels narrows the account to enabled models in accessible
// balance_only groups. Plan-only groups are intentionally excluded: their
// quota state lives on the control plane and is not replicated to edges.
func (s *ApiKeySvc) regionalAllowedModels(ctx context.Context, user *ent.User, key *ent.ApiKey) ([]string, map[string]region.ModelPrice, error) {
	groups, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.EnabledEQ(true), entgroup.BillingSourceEQ(entgroup.BillingSourceBalanceOnly)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	type candidate struct {
		model  *ent.Model
		groups []*ent.ModelGroup
	}
	candidates := make(map[uuid.UUID]*candidate)
	granted, err := NewModelSvc(s.d).LoadGrantedGroupIDs(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	for _, group := range groups {
		if !userMayEnterGroup(user, group, granted) {
			continue
		}
		if len(key.AllowedGroupIds) > 0 && !containsUUID(key.AllowedGroupIds, group.ID) {
			continue
		}
		links, err := s.d.Ent.ModelGroupModel.Query().
			Where(entgrpmodel.GroupIDEQ(group.ID)).WithModel().All(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, link := range links {
			model, err := link.Edges.ModelOrErr()
			if err != nil || !model.Enabled {
				continue
			}
			c := candidates[model.ID]
			if c == nil {
				c = &candidate{model: model}
				candidates[model.ID] = c
			}
			c.groups = append(c.groups, group)
		}
	}
	out := make([]string, 0, len(candidates))
	prices := make(map[string]region.ModelPrice, len(candidates))
	for _, c := range candidates {
		group, err := selectGroup(c.groups, key, routePrefFrom(c.model, nil, uuid.Nil, uuid.Nil))
		if err != nil {
			continue
		}
		out = append(out, c.model.Name)
		prices[c.model.Name] = regionalModelPrice(c.model, group.RateMultiplier)
	}
	sort.Strings(out)
	return out, prices, nil
}

func regionalModelPrice(model *ent.Model, multiplier float64) region.ModelPrice {
	mult := decimal.NewFromFloat(multiplier)
	scale := func(value *decimal.Decimal) string {
		if value == nil {
			return ""
		}
		return value.Mul(mult).String()
	}
	price := region.ModelPrice{
		BillingMode:       string(model.BillingMode),
		InputPer1K:        scale(model.InputUnitPrice),
		OutputPer1K:       scale(model.OutputUnitPrice),
		CacheReadPer1K:    scale(model.CacheReadInputPrice),
		CacheWritePer1K:   scale(model.CacheWriteInputPrice),
		CacheWrite1hPer1K: scale(model.CacheWrite1hInputPrice),
		AudioInputPer1K:   scale(model.AudioInputUnitPrice),
		AudioOutputPer1K:  scale(model.AudioOutputUnitPrice),
		PerRequestCNY:     scale(model.PricePerRequest),
	}
	for _, tier := range model.PriceTiers {
		price.PriceTiers = append(price.PriceTiers, region.ModelPriceTier{
			UpToTokens: tier.UpToTokens, InputPer1K: scale(tier.InputUnitPrice), OutputPer1K: scale(tier.OutputUnitPrice),
			CacheReadPer1K: scale(tier.CacheReadInputPrice), CacheWritePer1K: scale(tier.CacheWriteInputPrice),
			CacheWrite1hPer1K: scale(tier.CacheWrite1hInputPrice),
		})
	}
	return price
}

func containsUUID(values []uuid.UUID, want uuid.UUID) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// SetCipher wires the AES key used to encrypt full API-key plaintext at
// rest. Called once from cmd/jucode/main.go right after cipher init so
// the existing NewApiKeySvc signature stays untouched (it is used in
// other places and a breaking change cascades).
func (s *ApiKeySvc) SetCipher(c *cryptopkg.Cipher) { s.cipher = c }

// CreateResult is returned ONCE — the plaintext Secret never leaves the
// server again. Callers must surface it to the end user immediately.
type CreateResult struct {
	Record *ent.ApiKey
	Secret string // full plaintext "sk-juc-..." — only returned at creation
}

// CreateInput bundles optional knobs for ApiKeySvc.Create.
type CreateInput struct {
	Name                                             string
	ExpiresAt                                        *time.Time
	AllowedGroupIDs                                  []uuid.UUID
	IPWhitelist                                      []string
	AutoRoute                                        bool
	AutoRouteMaxRate                                 float64 // 0 = no ceiling. Price mode only.
	AutoRouteMode                                    string  // price, stable, or fast. Empty means price.
	ModelPins                                        []entschema.ModelPin
	BillingScope                                     string
	OrganizationID                                   *uuid.UUID
	DailyCostLimit, MonthlyCostLimit, TotalCostLimit *decimal.Decimal
	System                                           bool
}

// UpdateInput is the partial PATCH payload for an existing ApiKey.
// Nil fields leave the stored value unchanged. Used by the user-self
// edit endpoint (/v1/me/apikeys/:id PATCH) so a key can flip the
// auto_route knob without recreating + losing the secret.
// ExpiresAt is doubly-pointered so callers can distinguish "no change"
// (nil) from "clear the expiry" (*nil).
type UpdateInput struct {
	Name                                             *string
	ExpiresAt                                        **time.Time
	AllowedGroupIDs                                  *[]uuid.UUID
	IPWhitelist                                      *[]string
	AutoRoute                                        *bool
	AutoRouteMaxRate                                 *float64
	AutoRouteMode                                    *string
	ModelPins                                        *[]entschema.ModelPin
	BillingScope                                     *string
	OrganizationID                                   *uuid.UUID
	DailyCostLimit, MonthlyCostLimit, TotalCostLimit **decimal.Decimal
}

func (s *ApiKeySvc) Create(ctx context.Context, userID uuid.UUID, in CreateInput) (*CreateResult, error) {
	if err := validateCostLimits(in.DailyCostLimit, in.MonthlyCostLimit, in.TotalCostLimit); err != nil {
		return nil, err
	}
	whitelist, err := NormalizeIPWhitelist(in.IPWhitelist)
	if err != nil {
		return nil, err
	}
	allowed, err := s.onlyEnabledGroups(ctx, in.AllowedGroupIDs)
	if err != nil {
		return nil, err
	}
	in.AllowedGroupIDs = allowed
	if err := s.checkPlanGroups(ctx, userID, in.AllowedGroupIDs, nil); err != nil {
		return nil, err
	}
	pins, err := s.cleanModelPins(ctx, in.ModelPins, in.AllowedGroupIDs)
	if err != nil {
		return nil, err
	}
	in.ModelPins = pins
	billingScope, orgID, err := s.resolveBillingScope(ctx, userID, in.BillingScope, in.OrganizationID)
	if err != nil {
		return nil, err
	}
	full, prefix, err := idgen.NewAPIKeySecret()
	if err != nil {
		return nil, err
	}
	hash := hashSecret(full)
	q := s.d.Ent.ApiKey.Create().
		SetUserID(userID).
		SetPrefix(prefix).
		SetHash(hash).
		SetName(in.Name).
		SetAllowedGroupIds(in.AllowedGroupIDs).
		SetIPWhitelist(whitelist).
		SetAutoRoute(in.AutoRoute).
		SetAutoRouteMode(routeMode(in.AutoRouteMode)).
		SetAutoRouteMaxRate(in.AutoRouteMaxRate).
		SetModelPins(in.ModelPins).
		SetBillingScope(billingScope).
		SetSystem(in.System)
	if in.DailyCostLimit != nil {
		q = q.SetDailyCostLimit(*in.DailyCostLimit)
	}
	if in.MonthlyCostLimit != nil {
		q = q.SetMonthlyCostLimit(*in.MonthlyCostLimit)
	}
	if in.TotalCostLimit != nil {
		q = q.SetTotalCostLimit(*in.TotalCostLimit)
	}
	if orgID != nil {
		q = q.SetOrganizationID(*orgID)
	}
	// Encrypt the full plaintext for the "reveal later" admin feature.
	// Fail closed: if cipher isn't wired, the key still gets created
	// (hash + prefix are enough for gateway auth), it just can't be
	// revealed later. That mirrors the legacy behaviour pre-secret_enc.
	if s.cipher != nil {
		enc, encErr := s.cipher.Encrypt(full)
		if encErr == nil {
			q = q.SetSecretEnc(enc)
		}
	}
	if in.ExpiresAt != nil {
		q = q.SetExpiresAt(*in.ExpiresAt)
	}
	rec, err := q.Save(ctx)
	if err != nil {
		return nil, err
	}
	return &CreateResult{Record: rec, Secret: full}, nil
}

// Reveal returns the plaintext API key for a row created after
// secret_enc support landed. When userID is non-Nil we enforce
// ownership (used by the /me endpoint); admin handlers pass uuid.Nil
// to skip the ownership check. Returns ErrApiKeyUnrevealable for
// legacy rows with empty secret_enc so the UI can show a clear hint.
func (s *ApiKeySvc) Reveal(ctx context.Context, apiKeyID uuid.UUID, userID uuid.UUID) (string, error) {
	if s.cipher == nil {
		return "", ErrApiKeyUnrevealable
	}
	q := s.d.Ent.ApiKey.Query().Where(entapikey.IDEQ(apiKeyID))
	if userID != uuid.Nil {
		q = q.Where(entapikey.UserIDEQ(userID))
	}
	rec, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return "", ErrApiKeyNotFound
		}
		return "", err
	}
	if rec.System {
		return "", ErrApiKeySystem
	}
	if rec.SecretEnc == "" {
		return "", ErrApiKeyUnrevealable
	}
	plain, err := s.cipher.Decrypt(rec.SecretEnc)
	if err != nil {
		return "", err
	}
	return plain, nil
}

type ApiKeyUsage struct {
	Key                               *ent.ApiKey
	DailyUsed, MonthlyUsed, TotalUsed decimal.Decimal
}

// keepLiveGroupIDs drops group ids that are not in live. An empty list means
// the key may use every group, so a non-empty input that would lose every id
// is returned unchanged: the key stays bound to groups it cannot enter
// instead of being opened to all of them. Views hide those ids.
func keepLiveGroupIDs(ids []uuid.UUID, live map[uuid.UUID]struct{}) ([]uuid.UUID, bool) {
	if len(ids) == 0 {
		return ids, false
	}
	next := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := live[id]; ok {
			next = append(next, id)
		}
	}
	if len(next) == len(ids) || len(next) == 0 {
		return ids, false
	}
	return next, true
}

// LiveGroups returns the subset of ids that refer to an enabled group, in one
// query. Handlers pass it to VisibleGroupIDs.
func (s *ApiKeySvc) LiveGroups(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]struct{}, error) {
	live := make(map[uuid.UUID]struct{}, len(ids))
	if len(ids) == 0 {
		return live, nil
	}
	rows, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.IDIn(ids...), entgroup.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, group := range rows {
		live[group.ID] = struct{}{}
	}
	return live, nil
}

// VisibleGroupIDs is the allow-list shown to clients: disabled and deleted
// groups are left out. An empty result does not mean unrestricted; callers
// report that from the stored list (allowed_groups_unrestricted).
func VisibleGroupIDs(ids []uuid.UUID, live map[uuid.UUID]struct{}) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := live[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// deadRestriction returns the stored allow-list when it is non-empty with no
// enabled group left, else nil. The client saw an empty list for such a key;
// saving that list back must not open the key to every group.
// checkPlanGroups rejects restricting a key to plan-only groups unless the
// key owner has an active subscription. Groups the key already had are
// grandfathered, so older keys stay editable (rename, limits, ...) even
// after their owner's plan changed.
func (s *ApiKeySvc) checkPlanGroups(ctx context.Context, ownerID uuid.UUID, requested, existing []uuid.UUID) error {
	had := make(map[uuid.UUID]struct{}, len(existing))
	for _, id := range existing {
		had[id] = struct{}{}
	}
	added := make([]uuid.UUID, 0, len(requested))
	for _, id := range requested {
		if _, ok := had[id]; !ok {
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil
	}
	n, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.IDIn(added...), entgroup.BillingSourceEQ(entgroup.BillingSourcePlanOnly)).
		Count(ctx)
	if err != nil || n == 0 {
		return err
	}
	_, plan, err := activeUserPlan(ctx, s.d.Ent, ownerID)
	if errors.Is(err, ErrNoActivePlan) || (err == nil && !IsSubscriptionPlan(plan)) {
		return ErrPlanGroupsNeedSubscription
	}
	return err
}

// storedKey loads a live key under the given predicates.
func (s *ApiKeySvc) storedKey(ctx context.Context, apiKeyID uuid.UUID, preds ...predicate.ApiKey) (*ent.ApiKey, error) {
	k, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.IDEQ(apiKeyID), entapikey.RevokedAtIsNil()).
		Where(preds...).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, ErrApiKeyNotFound
	}
	return k, err
}

func (s *ApiKeySvc) deadRestriction(ctx context.Context, sc ApiKeyScope, apiKeyID uuid.UUID) ([]uuid.UUID, error) {
	stored, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.IDEQ(apiKeyID), entapikey.RevokedAtIsNil()).
		Where(sc.predicates()...).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrApiKeyNotFound
		}
		return nil, err
	}
	if len(stored.AllowedGroupIds) == 0 {
		return nil, nil
	}
	live, err := s.LiveGroups(ctx, stored.AllowedGroupIds)
	if err != nil || len(live) > 0 {
		return nil, err
	}
	return stored.AllowedGroupIds, nil
}

func keepLivePins(pins []entschema.ModelPin, live map[uuid.UUID]struct{}) ([]entschema.ModelPin, bool) {
	if len(pins) == 0 {
		return pins, false
	}
	next := make([]entschema.ModelPin, 0, len(pins))
	changed := false
	for _, pin := range pins {
		if _, ok := live[pin.GroupID]; ok {
			next = append(next, pin)
			continue
		}
		changed = true
	}
	if !changed {
		return pins, false
	}
	return next, true
}

func dropPinGroup(pins []entschema.ModelPin, groupID uuid.UUID) ([]entschema.ModelPin, bool) {
	if len(pins) == 0 {
		return pins, false
	}
	next := make([]entschema.ModelPin, 0, len(pins))
	changed := false
	for _, pin := range pins {
		if pin.GroupID == groupID {
			changed = true
			continue
		}
		next = append(next, pin)
	}
	if !changed {
		return pins, false
	}
	return next, true
}

// onlyEnabledGroups keeps ids that still refer to an enabled group. Disabled
// and deleted groups are omitted so a key cannot stay bound to them.
func (s *ApiKeySvc) onlyEnabledGroups(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return ids, nil
	}
	live, err := s.LiveGroups(ctx, ids)
	if err != nil {
		return nil, err
	}
	next, _ := keepLiveGroupIDs(ids, live)
	return next, nil
}

// pruneRetiredGroups rewrites keys whose allow-list still names a disabled or
// deleted group. Listing is the backfill for groups retired before unbind
// existed.
func (s *ApiKeySvc) pruneRetiredGroups(ctx context.Context, keys []*ent.ApiKey) error {
	seen := make(map[uuid.UUID]struct{})
	var ids []uuid.UUID
	for _, key := range keys {
		for _, id := range key.AllowedGroupIds {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		for _, pin := range key.ModelPins {
			if _, ok := seen[pin.GroupID]; ok || pin.GroupID == uuid.Nil {
				continue
			}
			seen[pin.GroupID] = struct{}{}
			ids = append(ids, pin.GroupID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	live, err := s.LiveGroups(ctx, ids)
	if err != nil {
		return err
	}
	changed := false
	for _, key := range keys {
		next, dirty := keepLiveGroupIDs(key.AllowedGroupIds, live)
		nextPins, pinDirty := keepLivePins(key.ModelPins, live)
		if !dirty && !pinDirty {
			continue
		}
		update := s.d.Ent.ApiKey.UpdateOneID(key.ID)
		if dirty {
			update.SetAllowedGroupIds(next)
		}
		if pinDirty {
			update.SetModelPins(nextPins)
		}
		if err := update.Exec(ctx); err != nil {
			return err
		}
		key.AllowedGroupIds = next
		key.ModelPins = nextPins
		changed = true
	}
	if changed {
		s.clearResolveCache()
	}
	return nil
}

// UnbindGroup removes groupID from every live key's allow-list. Called when
// a group is disabled or deleted so the binding does not survive the group.
// A key bound only to this group keeps it: an empty list would open the key
// to every group.
func (s *ApiKeySvc) UnbindGroup(ctx context.Context, groupID uuid.UUID) error {
	keys, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.RevokedAtIsNil()).
		All(ctx)
	if err != nil {
		return err
	}
	changed := false
	for _, key := range keys {
		next := make([]uuid.UUID, 0, len(key.AllowedGroupIds))
		for _, id := range key.AllowedGroupIds {
			if id != groupID {
				next = append(next, id)
			}
		}
		allowDirty := len(next) > 0 && len(next) < len(key.AllowedGroupIds)
		nextPins, pinDirty := dropPinGroup(key.ModelPins, groupID)
		if !allowDirty && !pinDirty {
			continue
		}
		update := s.d.Ent.ApiKey.UpdateOneID(key.ID)
		if allowDirty {
			update.SetAllowedGroupIds(next)
		}
		if pinDirty {
			update.SetModelPins(nextPins)
		}
		if err := update.Exec(ctx); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		s.clearResolveCache()
	}
	return nil
}

func routeMode(mode string) entapikey.AutoRouteMode {
	switch entapikey.AutoRouteMode(mode) {
	case entapikey.AutoRouteModeStable, entapikey.AutoRouteModeFast:
		return entapikey.AutoRouteMode(mode)
	default:
		return entapikey.AutoRouteModePrice
	}
}

// cleanModelPins drops blank models, disabled groups, and — when the key has
// an allow-list — groups outside that list. The last pin for a model wins.
func (s *ApiKeySvc) cleanModelPins(ctx context.Context, pins []entschema.ModelPin, allowed []uuid.UUID) ([]entschema.ModelPin, error) {
	if len(pins) == 0 {
		return []entschema.ModelPin{}, nil
	}
	ids := make([]uuid.UUID, 0, len(pins))
	for _, pin := range pins {
		if pin.GroupID != uuid.Nil {
			ids = append(ids, pin.GroupID)
		}
	}
	live := map[uuid.UUID]struct{}{}
	if len(ids) > 0 {
		rows, err := s.d.Ent.ModelGroup.Query().Where(entgroup.IDIn(ids...), entgroup.EnabledEQ(true)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, group := range rows {
			live[group.ID] = struct{}{}
		}
	}
	allowedSet := map[uuid.UUID]struct{}{}
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}
	restrict := len(allowed) > 0
	out := make([]entschema.ModelPin, 0, len(pins))
	index := map[string]int{}
	for _, pin := range pins {
		model := strings.TrimSpace(pin.Model)
		if model == "" {
			continue
		}
		if _, ok := live[pin.GroupID]; !ok {
			continue
		}
		if restrict {
			if _, ok := allowedSet[pin.GroupID]; !ok {
				continue
			}
		}
		pin.Model = model
		if i, ok := index[strings.ToLower(model)]; ok {
			out[i] = pin
			continue
		}
		index[strings.ToLower(model)] = len(out)
		out = append(out, pin)
	}
	return out, nil
}

func (s *ApiKeySvc) prepareRoutePins(ctx context.Context, in *UpdateInput) error {
	if in.ModelPins == nil {
		return nil
	}
	var allowed []uuid.UUID
	if in.AllowedGroupIDs != nil {
		allowed = *in.AllowedGroupIDs
	}
	pins, err := s.cleanModelPins(ctx, *in.ModelPins, allowed)
	if err != nil {
		return err
	}
	in.ModelPins = &pins
	return nil
}

func (s *ApiKeySvc) List(ctx context.Context, userID uuid.UUID) ([]ApiKeyUsage, error) {
	keys, err := s.d.Ent.ApiKey.Query().Where(entapikey.UserIDEQ(userID), entapikey.RevokedAtIsNil()).Order(ent.Desc(entapikey.FieldCreatedAt)).All(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pruneRetiredGroups(ctx, keys); err != nil {
		return nil, err
	}
	return s.withUsage(ctx, keys)
}

// ApiKeyScope is the account context a /me apikey operation runs under.
// The generated WHERE predicates are the whole authorization story: a key
// outside the scope is simply not found, so there is no separate check to
// forget (and no TOCTOU between check and mutation).
//
//   - personal (OrgID nil): the caller's own personal-scope keys — org
//     keys are managed from the org context only.
//   - org member: the caller's own keys in that organization.
//   - org owner/admin (OrgManager): every key in the organization.
type ApiKeyScope struct {
	UserID     uuid.UUID
	OrgID      *uuid.UUID
	OrgManager bool
}

func (sc ApiKeyScope) predicates() []predicate.ApiKey {
	if sc.OrgID == nil {
		return []predicate.ApiKey{
			entapikey.UserIDEQ(sc.UserID),
			entapikey.BillingScopeEQ(entapikey.BillingScopePersonal),
		}
	}
	if sc.OrgManager {
		return []predicate.ApiKey{entapikey.OrganizationIDEQ(*sc.OrgID)}
	}
	return []predicate.ApiKey{
		entapikey.OrganizationIDEQ(*sc.OrgID),
		entapikey.UserIDEQ(sc.UserID),
	}
}

// ListScoped lists the live keys visible in the given account context.
// The manager view eager-loads the owning user so the console can show
// who created each key.
func (s *ApiKeySvc) ListScoped(ctx context.Context, sc ApiKeyScope) ([]ApiKeyUsage, error) {
	q := s.d.Ent.ApiKey.Query().
		Where(entapikey.RevokedAtIsNil(), entapikey.SystemEQ(false)).
		Where(sc.predicates()...).
		Order(ent.Desc(entapikey.FieldCreatedAt))
	if sc.OrgID != nil && sc.OrgManager {
		q = q.WithUser()
	}
	keys, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pruneRetiredGroups(ctx, keys); err != nil {
		return nil, err
	}
	return s.withUsage(ctx, keys)
}

func (s *ApiKeySvc) withUsage(ctx context.Context, keys []*ent.ApiKey) ([]ApiKeyUsage, error) {
	ids := make([]uuid.UUID, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	usage, err := s.costUsageByKey(ctx, ids, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]ApiKeyUsage, 0, len(keys))
	for _, key := range keys {
		used := usage[key.ID]
		out = append(out, ApiKeyUsage{Key: key, DailyUsed: used.Daily, MonthlyUsed: used.Monthly, TotalUsed: used.Total})
	}
	return out, nil
}

// costUsageByKey is the batched form of CostUsage: one GROUP BY over
// usage_logs for the whole key set. Keys with no usage are simply absent
// from the map (the zero CostUsage is all-zero, which is what callers
// want). List used to call CostUsage in a loop — one full aggregation
// per key.
func (s *ApiKeySvc) costUsageByKey(ctx context.Context, ids []uuid.UUID, now time.Time) (map[uuid.UUID]CostUsage, error) {
	out := make(map[uuid.UUID]CostUsage, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	d, m := CalendarPeriods(now)
	args := []any{d, m}
	ph := make([]string, len(ids))
	for i, id := range ids {
		args = append(args, id)
		ph[i] = fmt.Sprintf("$%d", len(args))
	}
	q := fmt.Sprintf(`SELECT api_key_id,
		COALESCE(SUM(cost_final) FILTER (WHERE created_at >= $1),0),
		COALESCE(SUM(cost_final) FILTER (WHERE created_at >= $2),0),
		COALESCE(SUM(cost_final),0)
	FROM usage_logs WHERE status='ok' AND api_key_id IN (%s)
	GROUP BY api_key_id`, strings.Join(ph, ","))
	rows, err := s.d.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var u CostUsage
		if err := rows.Scan(&id, &u.Daily, &u.Monthly, &u.Total); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// Update applies a PATCH-style partial update. userID is enforced so
// callers can't modify another user's key. Returns ErrApiKeyNotFound
// when no matching row exists.
func (s *ApiKeySvc) Update(ctx context.Context, userID, apiKeyID uuid.UUID, in UpdateInput) (*ent.ApiKey, error) {
	if in.AllowedGroupIDs != nil {
		filtered, err := s.onlyEnabledGroups(ctx, *in.AllowedGroupIDs)
		if err != nil {
			return nil, err
		}
		stored, err := s.storedKey(ctx, apiKeyID, entapikey.UserIDEQ(userID))
		if err != nil {
			return nil, err
		}
		if err := s.checkPlanGroups(ctx, stored.UserID, filtered, stored.AllowedGroupIds); err != nil {
			return nil, err
		}
		in.AllowedGroupIDs = &filtered
	}
	if err := s.prepareRoutePins(ctx, &in); err != nil {
		return nil, err
	}
	if err := validateUpdateCostLimits(in.DailyCostLimit, in.MonthlyCostLimit, in.TotalCostLimit); err != nil {
		return nil, err
	}
	q := s.d.Ent.ApiKey.UpdateOneID(apiKeyID).Where(entapikey.UserIDEQ(userID), entapikey.RevokedAtIsNil())
	if err := applyApiKeyUpdate(q, in); err != nil {
		return nil, err
	}
	if in.BillingScope != nil || in.OrganizationID != nil {
		scope := ""
		if in.BillingScope != nil {
			scope = *in.BillingScope
		}
		billingScope, orgID, err := s.resolveBillingScope(ctx, userID, scope, in.OrganizationID)
		if err != nil {
			return nil, err
		}
		q = q.SetBillingScope(billingScope)
		if orgID != nil {
			q = q.SetOrganizationID(*orgID)
		} else {
			q = q.ClearOrganizationID()
		}
	}
	rec, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrApiKeyNotFound
		}
		return nil, err
	}
	s.clearResolveCache()
	return rec, nil
}

// applyApiKeyUpdate copies the non-billing PATCH fields onto the update
// builder. Shared by the admin Update (which may also move billing scope)
// and the user-facing UpdateScoped (which must not).
func applyApiKeyUpdate(q *ent.ApiKeyUpdateOne, in UpdateInput) error {
	apply := func(v **decimal.Decimal, set func(decimal.Decimal), clear func()) {
		if v != nil {
			if *v == nil {
				clear()
			} else {
				set(**v)
			}
		}
	}
	apply(in.DailyCostLimit, func(v decimal.Decimal) { q.SetDailyCostLimit(v) }, func() { q.ClearDailyCostLimit() })
	apply(in.MonthlyCostLimit, func(v decimal.Decimal) { q.SetMonthlyCostLimit(v) }, func() { q.ClearMonthlyCostLimit() })
	apply(in.TotalCostLimit, func(v decimal.Decimal) { q.SetTotalCostLimit(v) }, func() { q.ClearTotalCostLimit() })
	if in.Name != nil {
		q.SetName(*in.Name)
	}
	if in.ExpiresAt != nil {
		if *in.ExpiresAt == nil {
			q.ClearExpiresAt()
		} else {
			q.SetExpiresAt(**in.ExpiresAt)
		}
	}
	if in.AllowedGroupIDs != nil {
		q.SetAllowedGroupIds(*in.AllowedGroupIDs)
	}
	if in.IPWhitelist != nil {
		whitelist, err := NormalizeIPWhitelist(*in.IPWhitelist)
		if err != nil {
			return err
		}
		q.SetIPWhitelist(whitelist)
	}
	if in.AutoRoute != nil {
		q.SetAutoRoute(*in.AutoRoute)
	}
	if in.AutoRouteMode != nil {
		q.SetAutoRouteMode(routeMode(*in.AutoRouteMode))
	}
	if in.AutoRouteMaxRate != nil {
		q.SetAutoRouteMaxRate(*in.AutoRouteMaxRate)
	}
	if in.ModelPins != nil {
		q.SetModelPins(*in.ModelPins)
	}
	return nil
}

// UpdateScoped is the account-context PATCH: ownership comes from the
// scope predicates, and a key can never migrate between billing scopes —
// that would silently change whose balance pays for it.
func (s *ApiKeySvc) UpdateScoped(ctx context.Context, sc ApiKeyScope, apiKeyID uuid.UUID, in UpdateInput) (*ent.ApiKey, error) {
	if in.BillingScope != nil || in.OrganizationID != nil {
		return nil, errors.New("billing scope of an existing key cannot be changed")
	}
	if err := s.rejectSystemKey(ctx, sc.predicates(), apiKeyID); err != nil {
		return nil, err
	}
	if in.AllowedGroupIDs != nil {
		filtered, err := s.onlyEnabledGroups(ctx, *in.AllowedGroupIDs)
		if err != nil {
			return nil, err
		}
		stored, err := s.storedKey(ctx, apiKeyID, sc.predicates()...)
		if err != nil {
			return nil, err
		}
		if err := s.checkPlanGroups(ctx, stored.UserID, filtered, stored.AllowedGroupIds); err != nil {
			return nil, err
		}
		in.AllowedGroupIDs = &filtered
		if len(filtered) == 0 {
			dead, err := s.deadRestriction(ctx, sc, apiKeyID)
			if err != nil {
				return nil, err
			}
			if dead != nil {
				in.AllowedGroupIDs = &dead
			}
		}
	}
	if err := s.prepareRoutePins(ctx, &in); err != nil {
		return nil, err
	}
	if err := validateUpdateCostLimits(in.DailyCostLimit, in.MonthlyCostLimit, in.TotalCostLimit); err != nil {
		return nil, err
	}
	q := s.d.Ent.ApiKey.UpdateOneID(apiKeyID).
		Where(entapikey.RevokedAtIsNil()).
		Where(sc.predicates()...)
	if err := applyApiKeyUpdate(q, in); err != nil {
		return nil, err
	}
	rec, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrApiKeyNotFound
		}
		return nil, err
	}
	s.clearResolveCache()
	return rec, nil
}

// RevokeScoped revokes a key visible in the given account context (an org
// manager may revoke any member's org key).
func (s *ApiKeySvc) RevokeScoped(ctx context.Context, sc ApiKeyScope, apiKeyID uuid.UUID) error {
	if err := s.rejectSystemKey(ctx, sc.predicates(), apiKeyID); err != nil {
		return err
	}
	n, err := s.d.Ent.ApiKey.Update().
		Where(entapikey.IDEQ(apiKeyID), entapikey.RevokedAtIsNil()).
		Where(sc.predicates()...).
		SetRevokedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrApiKeyNotFound
	}
	s.clearResolveCache()
	return nil
}

func (s *ApiKeySvc) resolveBillingScope(ctx context.Context, userID uuid.UUID, raw string, orgID *uuid.UUID) (entapikey.BillingScope, *uuid.UUID, error) {
	switch raw {
	case "", "personal":
		return entapikey.BillingScopePersonal, nil, nil
	case "organization":
		if orgID == nil || *orgID == uuid.Nil {
			return "", nil, errors.New("organization_id is required")
		}
		ok, err := s.d.Ent.OrganizationMember.Query().
			Where(entmember.OrganizationIDEQ(*orgID), entmember.UserIDEQ(userID)).
			Exist(ctx)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			return "", nil, ErrApiKeyOrgDenied
		}
		id := *orgID
		return entapikey.BillingScopeOrganization, &id, nil
	default:
		return "", nil, errors.New("billing_scope must be personal|organization")
	}
}

func (s *ApiKeySvc) Revoke(ctx context.Context, userID, apiKeyID uuid.UUID) error {
	if err := s.rejectSystemKey(ctx, []predicate.ApiKey{entapikey.UserIDEQ(userID)}, apiKeyID); err != nil {
		return err
	}
	n, err := s.d.Ent.ApiKey.Update().
		Where(entapikey.IDEQ(apiKeyID), entapikey.UserIDEQ(userID), entapikey.RevokedAtIsNil()).
		SetRevokedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrApiKeyNotFound
	}
	s.clearResolveCache()
	return nil
}

// EnsureLabKey returns the caller's lab key, creating it when missing.
// An existing unrestricted key named 实验室 is adopted in place. The
// plaintext is returned only when this call creates the row. Org managers
// do not adopt another member's key. One live system key is allowed per
// owner; a concurrent create loses and returns the row that won.
func (s *ApiKeySvc) EnsureLabKey(ctx context.Context, sc ApiKeyScope) (*CreateResult, error) {
	own := sc
	own.OrgManager = false
	if res, err := s.readySystemKey(ctx, own); res != nil || err != nil {
		return res, err
	}
	legacy, err := s.d.Ent.ApiKey.Query().
		Where(
			entapikey.RevokedAtIsNil(),
			entapikey.SystemEQ(false),
			entapikey.NameEQ(labKeyName),
		).
		Where(own.predicates()...).
		Order(ent.Desc(entapikey.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	var adopt *ent.ApiKey
	for _, key := range legacy {
		if len(key.AllowedGroupIds) == 0 {
			adopt = key
			break
		}
	}
	if adopt != nil {
		rec, err := s.d.Ent.ApiKey.UpdateOneID(adopt.ID).
			SetSystem(true).
			SetAutoRoute(true).
			Save(ctx)
		if ent.IsConstraintError(err) {
			if res, rerr := s.readySystemKey(ctx, own); res != nil || rerr != nil {
				return res, rerr
			}
		}
		if err != nil {
			return nil, err
		}
		s.clearResolveCache()
		return &CreateResult{Record: rec}, nil
	}
	in := CreateInput{Name: labKeyName, AutoRoute: true, System: true, BillingScope: "personal"}
	if sc.OrgID != nil {
		in.BillingScope = "organization"
		in.OrganizationID = sc.OrgID
	}
	res, err := s.Create(ctx, sc.UserID, in)
	if ent.IsConstraintError(err) {
		if existing, rerr := s.readySystemKey(ctx, own); existing != nil || rerr != nil {
			return existing, rerr
		}
	}
	return res, err
}

func (s *ApiKeySvc) readySystemKey(ctx context.Context, sc ApiKeyScope) (*CreateResult, error) {
	rec, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.RevokedAtIsNil(), entapikey.SystemEQ(true)).
		Where(sc.predicates()...).
		Order(ent.Desc(entapikey.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if rec.AutoRoute && rec.AutoRouteMaxRate == 0 && (rec.AutoRouteMode == "" || rec.AutoRouteMode == entapikey.AutoRouteModePrice) && len(rec.ModelPins) == 0 && len(rec.AllowedGroupIds) == 0 && rec.ExpiresAt == nil && len(rec.IPWhitelist) == 0 && rec.DailyCostLimit == nil && rec.MonthlyCostLimit == nil && rec.TotalCostLimit == nil {
		return &CreateResult{Record: rec}, nil
	}
	updated, err := s.d.Ent.ApiKey.UpdateOneID(rec.ID).
		SetAutoRoute(true).
		SetAutoRouteMode(entapikey.AutoRouteModePrice).
		SetAutoRouteMaxRate(0).
		SetModelPins([]entschema.ModelPin{}).
		SetAllowedGroupIds([]uuid.UUID{}).
		ClearExpiresAt().
		ClearIPWhitelist().
		ClearDailyCostLimit().
		ClearMonthlyCostLimit().
		ClearTotalCostLimit().
		Save(ctx)
	if err != nil {
		return nil, err
	}
	s.clearResolveCache()
	return &CreateResult{Record: updated}, nil
}

// RotateScoped is closed. Laboratory keys are not client credentials, so
// regenerating the secret is not offered.
func (s *ApiKeySvc) RotateScoped(ctx context.Context, sc ApiKeyScope, apiKeyID uuid.UUID) (*CreateResult, error) {
	own := sc
	own.OrgManager = false
	rec, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.IDEQ(apiKeyID), entapikey.RevokedAtIsNil()).
		Where(own.predicates()...).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrApiKeyNotFound
		}
		return nil, err
	}
	if !rec.System {
		return nil, ErrApiKeyNotSystem
	}
	return nil, ErrApiKeySystem
}

func (s *ApiKeySvc) rejectSystemKey(ctx context.Context, preds []predicate.ApiKey, apiKeyID uuid.UUID) error {
	rec, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.IDEQ(apiKeyID), entapikey.RevokedAtIsNil()).
		Where(preds...).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return ErrApiKeyNotFound
		}
		return err
	}
	if rec.System {
		return ErrApiKeySystem
	}
	return nil
}

// Resolve looks up an API key by its full plaintext form and returns the
// record + owning user. Used by the gateway's apikey middleware.
func (s *ApiKeySvc) Resolve(ctx context.Context, secret string) (*ent.ApiKey, *ent.User, error) {
	hash := hashSecret(secret)
	now := time.Now()
	s.cacheMu.Lock()
	if ent, ok := s.cache[hash]; ok {
		if now.Before(ent.expiresAt) {
			s.cacheMu.Unlock()
			return ent.apiKey, ent.user, nil
		}
		delete(s.cache, hash)
	}
	s.cacheMu.Unlock()

	rec, err := s.d.Ent.ApiKey.Query().
		Where(entapikey.HashEQ(hash), entapikey.RevokedAtIsNil()).
		WithUser().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil, ErrApiKeyNotFound
		}
		return nil, nil, err
	}
	if len(rec.AllowedGroupIds) > 0 {
		if err := s.pruneRetiredGroups(ctx, []*ent.ApiKey{rec}); err != nil {
			return nil, nil, err
		}
	}
	if rec.ExpiresAt != nil && rec.ExpiresAt.Before(time.Now()) {
		return nil, nil, ErrApiKeyExpired
	}
	u, err := rec.Edges.UserOrErr()
	if err != nil {
		return nil, nil, err
	}
	ttl := 60 * time.Second
	if rec.ExpiresAt != nil {
		untilKeyExpires := time.Until(*rec.ExpiresAt)
		if untilKeyExpires <= 0 {
			return nil, nil, ErrApiKeyExpired
		}
		if untilKeyExpires < ttl {
			ttl = untilKeyExpires
		}
	}
	cachedRec := *rec
	cachedRec.LastUsedAt = &now
	s.cacheMu.Lock()
	s.cache[hash] = apiKeyCacheEntry{apiKey: &cachedRec, user: u, expiresAt: now.Add(ttl)}
	s.cacheMu.Unlock()
	return rec, u, nil
}

func (s *ApiKeySvc) clearResolveCache() {
	s.cacheMu.Lock()
	s.cache = make(map[string]apiKeyCacheEntry)
	s.cacheMu.Unlock()
}

// TouchLastUsed updates last_used_at. Best-effort; we don't fail the request
// if this write loses a race.
func (s *ApiKeySvc) TouchLastUsed(ctx context.Context, apiKeyID uuid.UUID) {
	_, _ = s.d.Ent.ApiKey.UpdateOneID(apiKeyID).SetLastUsedAt(time.Now()).Save(ctx)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NormalizeIPWhitelist validates and canonicalizes a user-supplied IP
// whitelist. Each entry must be a single IP ("1.2.3.4", "::1") or a CIDR
// ("10.0.0.0/8"). Blank entries are dropped, duplicates collapsed, and
// the parsed canonical form is stored so the gateway-side matcher never
// has to worry about formatting variants.
func NormalizeIPWhitelist(entries []string) ([]string, error) {
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		var canonical string
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, fmt.Errorf("invalid ip whitelist entry %q: expected IP or CIDR", raw)
			}
			canonical = p.Masked().String()
		} else {
			a, err := netip.ParseAddr(e)
			if err != nil {
				return nil, fmt.Errorf("invalid ip whitelist entry %q: expected IP or CIDR", raw)
			}
			canonical = a.Unmap().String()
		}
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
	}
	return out, nil
}

// APIKeyIPAllowed reports whether clientIP may use the key. An empty
// whitelist means unrestricted. Unparseable stored entries are skipped
// (they can't match anything); an unparseable client IP fails closed
// when a whitelist is configured.
func APIKeyIPAllowed(k *ent.ApiKey, clientIP string) bool {
	if k == nil || len(k.IPWhitelist) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(clientIP))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, e := range k.IPWhitelist {
		if strings.Contains(e, "/") {
			if p, err := netip.ParsePrefix(e); err == nil && p.Contains(addr) {
				return true
			}
		} else if a, err := netip.ParseAddr(e); err == nil && a.Unmap() == addr {
			return true
		}
	}
	return false
}

type CostUsage struct{ Daily, Monthly, Total decimal.Decimal }

func validateCostLimits(vs ...*decimal.Decimal) error {
	for _, v := range vs {
		if v != nil && v.Sign() < 0 {
			return ErrInvalidCostLimit
		}
	}
	return nil
}

func validateUpdateCostLimits(vs ...**decimal.Decimal) error {
	for _, v := range vs {
		if v != nil && *v != nil && (**v).Sign() < 0 {
			return ErrInvalidCostLimit
		}
	}
	return nil
}

func CostLimitExceeded(k *ent.ApiKey, u CostUsage) bool {
	return (k.DailyCostLimit != nil && k.DailyCostLimit.Sign() > 0 && u.Daily.GreaterThanOrEqual(*k.DailyCostLimit)) || (k.MonthlyCostLimit != nil && k.MonthlyCostLimit.Sign() > 0 && u.Monthly.GreaterThanOrEqual(*k.MonthlyCostLimit)) || (k.TotalCostLimit != nil && k.TotalCostLimit.Sign() > 0 && u.Total.GreaterThanOrEqual(*k.TotalCostLimit))
}

func (s *ApiKeySvc) CostUsage(ctx context.Context, id uuid.UUID, now time.Time) (CostUsage, error) {
	d, m := CalendarPeriods(now)
	var a, b, c decimal.Decimal
	err := s.d.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_final) FILTER (WHERE created_at >= $2),0), COALESCE(SUM(cost_final) FILTER (WHERE created_at >= $3),0), COALESCE(SUM(cost_final),0) FROM usage_logs WHERE api_key_id=$1 AND status='ok'`, id, d, m).Scan(&a, &b, &c)
	return CostUsage{a, b, c}, err
}

func (s *ApiKeySvc) CheckCostLimit(ctx context.Context, k *ent.ApiKey, now time.Time) error {
	if k == nil || k.ID == uuid.Nil {
		return nil
	}
	if (k.DailyCostLimit == nil || k.DailyCostLimit.Sign() <= 0) && (k.MonthlyCostLimit == nil || k.MonthlyCostLimit.Sign() <= 0) && (k.TotalCostLimit == nil || k.TotalCostLimit.Sign() <= 0) {
		return nil
	}
	u, err := s.costUsageCached(ctx, k.ID, now)
	if err != nil {
		return fmt.Errorf("check api key cost limit: %w", err)
	}
	if CostLimitExceeded(k, u) {
		return ErrApiKeyCostLimit
	}
	return nil
}

// costUsageCacheTTL bounds how far a key can overshoot its cost limit:
// CheckCostLimit runs on every LLM request for keys with a limit set,
// and the underlying CostUsage query SUMs the key's entire usage_logs
// history. ~30s of extra spend past the limit is acceptable for a
// budget guard (the Resolve cache already tolerates 60s on revoke).
const costUsageCacheTTL = 30 * time.Second

// costUsageCached serves CostUsage through a short Redis cache. Redis
// (not the in-process map) because billing constructs a fresh ApiKeySvc
// per request, so process-local state never survives between calls.
// Fail-open on Redis errors: fall through to the real query.
func (s *ApiKeySvc) costUsageCached(ctx context.Context, id uuid.UUID, now time.Time) (CostUsage, error) {
	key := "apikey:costusage:" + id.String()
	if s.d.Redis != nil {
		rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		raw, err := s.d.Redis.Get(rctx, key).Bytes()
		cancel()
		if err == nil {
			var u CostUsage
			if json.Unmarshal(raw, &u) == nil {
				return u, nil
			}
		}
	}
	u, err := s.CostUsage(ctx, id, now)
	if err != nil {
		return u, err
	}
	if s.d.Redis != nil {
		if b, mErr := json.Marshal(u); mErr == nil {
			rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			_ = s.d.Redis.Set(rctx, key, b, costUsageCacheTTL).Err()
			cancel()
		}
	}
	return u, nil
}
