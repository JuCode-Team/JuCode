package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entaccountpoolmember "github.com/jucode/jucode/internal/data/ent/accountpoolmember"
	entgroupaccount "github.com/jucode/jucode/internal/data/ent/groupaccount"
	"github.com/jucode/jucode/internal/data/ent/predicate"
	"github.com/jucode/jucode/internal/data/ent/schema"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entaccountbinding "github.com/jucode/jucode/internal/data/ent/upstreamaccountbinding"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/providerkit"
)

// AccountPoolSvc owns the upstream account layer: credential storage and the
// runtime state the scheduler reads (stickiness, cooldowns, concurrency,
// health).
//
// Accounts are internal infrastructure. Nothing in this service may appear in
// a client-facing response, error, header, or model list, and no request input
// may select an account — selection is server-side only.
type AccountPoolSvc struct {
	d        *data.Data
	cipher   *cryptopkg.Cipher
	settings *SettingsSvc
	// definitions resolves provider definitions for account prefill and the
	// quota probe. Wired via SetDefinitions; nil disables both.
	definitions   *ProviderDefinitionSvc
	remoteCatalog *RemoteCatalogSvc

	// quotaSampledAt throttles passive quota-header sampling in-process, so
	// ordinary traffic does not even reach Redis for it.
	quotaSampledAt sync.Map // uuid.UUID -> time.Time
}

// Constructor signature kept explicit for the composition root.
func NewAccountPoolSvc(d *data.Data, cipher *cryptopkg.Cipher, settings *SettingsSvc) *AccountPoolSvc {
	return &AccountPoolSvc{d: d, cipher: cipher, settings: settings}
}

var (
	ErrAccountNotFound     = errors.New("account not found")
	ErrAccountNoCredential = errors.New("account has no credential")
	ErrAccountBadInput     = errors.New("invalid account input")
	// ErrProviderNotFound survives while the legacy finance tables
	// (provider_costs / provider_settlements) stay readable for history.
	ErrProviderNotFound = errors.New("provider not found")
)

const (
	// lastUsedThrottle bounds the per-request last_used_at write to one per
	// account per window.
	lastUsedThrottle = time.Minute
)

// ---- auth config (Provider.auth_config, non-secret) ----

// ---- CRUD ----

// CreateAccountInput is one account's worth of configuration. Endpoint,
// dialect and protocol declaration live here now: there is no channel above it.
type CreateAccountInput struct {
	Name      string
	BaseURL   string
	APIFormat string
	// DefinitionID, when set, prefills the endpoint shape, protocol
	// declaration and model list from that provider definition.
	// Callers that pass it may leave those fields empty.
	DefinitionID string
	// NativeProtocols lists the wire protocols the endpoint speaks natively.
	NativeProtocols []string
	DefaultProtocol string
	AllowConversion bool
	// EndpointPaths overrides the upstream POST path per protocol for vendors
	// outside the /v1/* conventions; empty means "gateway defaults".
	EndpointPaths map[string]string
	// HeaderRules rewrites upstream request headers (credential header name,
	// rejected headers or beta flags); nil means "the definition's, else none".
	HeaderRules  *schema.HeaderRules
	Organization *string
	// Request shaping inherited from the old channel layer.
	BlockImageGenerationTool bool
	ApiKey                   string
	Priority                 *int
	Weight                   *int
	MaxConcurrency           *int
	Note                     string
	PoolMode                 string
	// LegacyProviderID is set only by the migration command.
	LegacyProviderID *uuid.UUID
}

// ListAccountsInput filters the account list. Zero values mean "everything".
type ListAccountsInput struct {
	Status   string
	Health   string
	PoolID   *uuid.UUID
	GroupID  *uuid.UUID
	Protocol string
	Search   string
	Limit    int
	Offset   int
}

// List returns the account pool, newest scheduling inputs first.
func (s *AccountPoolSvc) List(ctx context.Context, in ListAccountsInput) ([]*ent.UpstreamAccount, error) {
	q := s.d.Ent.UpstreamAccount.Query().Where(entaccount.DeletedAtIsNil())
	if in.Status != "" {
		q = q.Where(entaccount.StatusEQ(entaccount.Status(in.Status)))
	}
	if in.Health != "" {
		q = q.Where(entaccount.HealthEQ(entaccount.Health(in.Health)))
	}
	if in.Search != "" {
		needle := strings.TrimSpace(in.Search)
		q = q.Where(entaccount.Or(
			entaccount.NameContainsFold(needle),
			entaccount.BaseURLContainsFold(needle),
		))
	}
	if in.PoolID != nil {
		members, err := s.d.Ent.AccountPoolMember.Query().
			Where(entaccountpoolmember.PoolIDEQ(*in.PoolID)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		ids := make([]uuid.UUID, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.AccountID)
		}
		q = q.Where(entaccount.IDIn(ids...))
	}
	if in.GroupID != nil {
		links, err := s.d.Ent.GroupAccount.Query().
			Where(entgroupaccount.GroupIDEQ(*in.GroupID)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		ids := make([]uuid.UUID, 0, len(links))
		for _, l := range links {
			ids = append(ids, l.AccountID)
		}
		q = q.Where(entaccount.IDIn(ids...))
	}
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := q.
		Order(ent.Asc(entaccount.FieldPriority), ent.Asc(entaccount.FieldCreatedAt)).
		Limit(limit).
		Offset(in.Offset).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if in.Protocol == "" {
		return rows, nil
	}
	// Protocol filtering stays in Go: the column is a JSON-ish string list, and
	// the set is tiny per deployment.
	out := make([]*ent.UpstreamAccount, 0, len(rows))
	for _, row := range rows {
		if AccountHasNativeProtocol(row, in.Protocol) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *AccountPoolSvc) Get(ctx context.Context, id uuid.UUID) (*ent.UpstreamAccount, error) {
	acct, err := s.d.Ent.UpstreamAccount.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}
	if acct.DeletedAt != nil {
		return nil, ErrAccountNotFound
	}
	return acct, nil
}

// Create adds an account to the pool. It is the single creation path: an
// account carries its own endpoint, dialect and credential.
func (s *AccountPoolSvc) Create(ctx context.Context, in CreateAccountInput) (*ent.UpstreamAccount, error) {
	key := strings.TrimSpace(in.ApiKey)
	if key == "" {
		return nil, fmt.Errorf("%w: api_key is required", ErrAccountBadInput)
	}
	return s.createWithCredential(ctx, in, providerkit.NewAPIKeyCredential(key))
}

func (s *AccountPoolSvc) createWithCredential(ctx context.Context, in CreateAccountInput, cred *providerkit.Credential) (*ent.UpstreamAccount, error) {
	definitionID := strings.TrimSpace(in.DefinitionID)
	var definitionModels []ProviderDefinitionModel
	if definitionID != "" {
		models, err := s.applyDefinition(ctx, definitionID, &in)
		if err != nil {
			return nil, err
		}
		definitionModels = models
	}
	if _, err := url.ParseRequestURI(strings.TrimSpace(in.BaseURL)); err != nil {
		return nil, fmt.Errorf("%w: base_url is required and must be absolute", ErrAccountBadInput)
	}
	protocols, err := NormalizeNativeProtocols(in.NativeProtocols)
	if err != nil {
		return nil, err
	}
	if len(protocols) == 0 {
		return nil, fmt.Errorf("%w: at least one native protocol is required", ErrAccountBadInput)
	}
	if defaultProtocol := strings.TrimSpace(in.DefaultProtocol); defaultProtocol != "" {
		normalized, ok := ParseNativeProtocol(defaultProtocol)
		if !ok {
			return nil, fmt.Errorf("%w: unknown default_protocol %q", ErrAccountBadInput, in.DefaultProtocol)
		}
		in.DefaultProtocol = normalized
	}
	endpointPaths, err := NormalizeEndpointPaths(in.EndpointPaths)
	if err != nil {
		return nil, err
	}
	headerRules, err := NormalizeHeaderRules(in.HeaderRules)
	if err != nil {
		return nil, err
	}
	enc, err := s.encodeCredential(cred)
	if err != nil {
		return nil, err
	}
	q := s.d.Ent.UpstreamAccount.Create().
		SetName(strings.TrimSpace(in.Name)).
		SetBaseURL(strings.TrimSpace(in.BaseURL)).
		SetAPIFormat(strings.TrimSpace(in.APIFormat)).
		SetNativeProtocols(protocols).
		SetDefaultProtocol(strings.TrimSpace(in.DefaultProtocol)).
		SetAllowConversion(in.AllowConversion).
		SetCredentialEnc(enc).
		SetStatus(entaccount.StatusActive).
		SetNote(in.Note)
	if len(endpointPaths) > 0 {
		q = q.SetEndpointPaths(endpointPaths)
	}
	if headerRules != nil {
		q = q.SetHeaderRules(*headerRules)
	}
	if in.Organization != nil {
		q = q.SetOrganization(*in.Organization)
	}
	if in.BlockImageGenerationTool {
		q = q.SetBlockImageGenerationTool(true)
	}
	if in.Priority != nil {
		q = q.SetPriority(*in.Priority)
	}
	if in.Weight != nil {
		q = q.SetWeight(*in.Weight)
	}
	if in.MaxConcurrency != nil {
		q = q.SetMaxConcurrency(*in.MaxConcurrency)
	}
	if raw := strings.TrimSpace(in.PoolMode); raw != "" {
		mode, ok := ParsePoolMode(raw)
		if !ok || mode == "" {
			return nil, fmt.Errorf("%w: pool_mode must be balanced, conservative, lenient or observe", ErrAccountBadInput)
		}
		q = q.SetPoolMode(string(mode))
	}
	acct, err := q.Save(ctx)
	if err != nil {
		return nil, err
	}
	if definitionID != "" {
		if acct, err = s.d.Ent.UpstreamAccount.UpdateOneID(acct.ID).SetDefinitionID(definitionID).Save(ctx); err != nil {
			return nil, err
		}
	}
	if len(definitionModels) > 0 {
		s.bindAccountModels(ctx, acct.ID, definitionModels)
	}
	return acct, nil
}

// SetDefinitions wires the definition service in after construction (the pool
// is built before it, and a nil service simply disables definition prefill).
func (s *AccountPoolSvc) SetDefinitions(defs *ProviderDefinitionSvc) { s.definitions = defs }

// SetRemoteCatalog wires the public model catalog used to fill display-only
// reference costs during model sync/import. Nil skips the supplement.
func (s *AccountPoolSvc) SetRemoteCatalog(cat *RemoteCatalogSvc) { s.remoteCatalog = cat }

// applyDefinition fills an account input from a provider definition, returning
// the models to bind once the account row exists. Caller-supplied fields win:
// an operator overriding the base URL for one account must not be overruled by
// the template.
func (s *AccountPoolSvc) applyDefinition(ctx context.Context, definitionID string, in *CreateAccountInput) ([]ProviderDefinitionModel, error) {
	if s.definitions == nil {
		return nil, fmt.Errorf("%w: provider definitions are not wired", ErrAccountBadInput)
	}
	def, err := s.definitions.Resolve(ctx, definitionID)
	if err != nil {
		return nil, err
	}
	in.DefinitionID = def.DefinitionID
	if strings.TrimSpace(in.BaseURL) == "" {
		in.BaseURL = def.BaseURL
	}
	if strings.TrimSpace(in.APIFormat) == "" {
		in.APIFormat = def.APIFormat
	}
	if len(in.NativeProtocols) == 0 {
		in.NativeProtocols = def.NativeProtocols
	}
	if strings.TrimSpace(in.DefaultProtocol) == "" {
		in.DefaultProtocol = def.DefaultProtocol
	}
	if len(in.EndpointPaths) == 0 && len(def.EndpointPaths) > 0 {
		paths := make(map[string]string, len(def.EndpointPaths))
		for key, value := range def.EndpointPaths {
			if text, ok := value.(string); ok {
				paths[key] = text
			}
		}
		in.EndpointPaths = paths
	}
	if in.HeaderRules == nil && !HeaderRulesEmpty(def.HeaderRules) {
		rules := def.HeaderRules
		in.HeaderRules = &rules
	}
	return def.Models, nil
}

// bindAccountModels binds a definition's model list to a fresh account. A model
// that is already bound is not an error here: creating an account twice from
// the same definition must not fail on the second run.
func (s *AccountPoolSvc) bindAccountModels(ctx context.Context, accountID uuid.UUID, models []ProviderDefinitionModel) {
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		if name == "" {
			continue
		}
		if _, err := s.AddAccountModel(ctx, accountID, AccountModelInput{ModelName: name}); err != nil {
			slog.Debug("account.definition_model_bind_failed",
				"account_id", accountID.String(), "model", name, "err", err.Error())
		}
	}
}

type UpdateProviderAccountInput struct {
	Name           *string
	ApiKey         *string // rotate the credential
	Status         *string // "active" | "disabled"
	Priority       *int
	Weight         *int
	MaxConcurrency *int
	Note           *string
	// PoolMode: "" clears the override and inherits the site default.
	PoolMode *string
	// --- endpoint & dialect ---
	// BaseURL, NativeProtocols, DefaultProtocol and AllowConversion are
	// editable after creation: an upstream that moved, or a declaration that
	// was wrong, must not force the operator to delete and rebuild an account
	// that traffic is pinned to.
	BaseURL         *string
	APIFormat       *string
	NativeProtocols *[]string
	DefaultProtocol *string
	AllowConversion *bool
	// EndpointPaths replaces the whole override map (an empty map clears it).
	EndpointPaths *map[string]string
	// HeaderRules replaces the whole rule set (empty rules clear it).
	HeaderRules  *schema.HeaderRules
	Organization *string
	// --- request shaping ---
	BlockImageGenerationTool   *bool
	EstimateCacheWrite         *bool
	CacheWriteMinContextTokens *int
}

func (s *AccountPoolSvc) Update(ctx context.Context, id uuid.UUID, in UpdateProviderAccountInput) (*ent.UpstreamAccount, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	q := s.d.Ent.UpstreamAccount.UpdateOneID(id)
	if in.Name != nil {
		q = q.SetName(strings.TrimSpace(*in.Name))
	}
	if in.ApiKey != nil {
		key := strings.TrimSpace(*in.ApiKey)
		if key == "" {
			return nil, fmt.Errorf("%w: api_key cannot be cleared", ErrAccountBadInput)
		}
		enc, err := s.encodeCredential(providerkit.NewAPIKeyCredential(key))
		if err != nil {
			return nil, err
		}
		q = q.SetCredentialEnc(enc)
	}
	if in.Status != nil {
		switch entaccount.Status(*in.Status) {
		case entaccount.StatusActive, entaccount.StatusDisabled:
			q = q.SetStatus(entaccount.Status(*in.Status))
			// Turning an account back on means it takes traffic again: a
			// health disable also cleared schedulable, and leaving that off
			// would show an active account that is never routed to.
			if *in.Status == string(entaccount.StatusActive) && cur.Status != entaccount.StatusActive {
				q = q.SetSchedulable(true)
			}
		default:
			return nil, fmt.Errorf("%w: status must be active or disabled", ErrAccountBadInput)
		}
	}
	if in.BaseURL != nil {
		base := strings.TrimSpace(*in.BaseURL)
		if _, err := url.ParseRequestURI(base); err != nil {
			return nil, fmt.Errorf("%w: base_url must be absolute", ErrAccountBadInput)
		}
		q = q.SetBaseURL(base)
	}
	if in.APIFormat != nil {
		q = q.SetAPIFormat(strings.TrimSpace(*in.APIFormat))
	}
	if in.NativeProtocols != nil {
		protocols, err := NormalizeNativeProtocols(*in.NativeProtocols)
		if err != nil {
			return nil, err
		}
		if len(protocols) == 0 {
			return nil, fmt.Errorf("%w: at least one native protocol is required", ErrAccountBadInput)
		}
		q = q.SetNativeProtocols(protocols)
	}
	if in.DefaultProtocol != nil {
		raw := strings.TrimSpace(*in.DefaultProtocol)
		if raw == "" {
			q = q.SetDefaultProtocol("")
		} else if proto, ok := ParseNativeProtocol(raw); ok {
			q = q.SetDefaultProtocol(proto)
		} else {
			return nil, fmt.Errorf("%w: unknown default_protocol %q", ErrAccountBadInput, raw)
		}
	}
	if in.AllowConversion != nil {
		q = q.SetAllowConversion(*in.AllowConversion)
	}
	if in.EndpointPaths != nil {
		// Whole-map replacement: an empty map clears every override, which is
		// how an operator reverts a custom account to the gateway defaults.
		paths, err := NormalizeEndpointPaths(*in.EndpointPaths)
		if err != nil {
			return nil, err
		}
		q = q.SetEndpointPaths(paths)
	}
	if in.HeaderRules != nil {
		rules, err := NormalizeHeaderRules(in.HeaderRules)
		if err != nil {
			return nil, err
		}
		if rules == nil {
			q = q.ClearHeaderRules()
		} else {
			q = q.SetHeaderRules(*rules)
		}
	}
	if in.Organization != nil {
		if org := strings.TrimSpace(*in.Organization); org == "" {
			q = q.ClearOrganization()
		} else {
			q = q.SetOrganization(org)
		}
	}
	if in.BlockImageGenerationTool != nil {
		q = q.SetBlockImageGenerationTool(*in.BlockImageGenerationTool)
	}
	if in.EstimateCacheWrite != nil {
		q = q.SetEstimateCacheWrite(*in.EstimateCacheWrite)
	}
	if in.CacheWriteMinContextTokens != nil {
		if *in.CacheWriteMinContextTokens < 0 {
			return nil, fmt.Errorf("%w: cache_write_min_context_tokens cannot be negative", ErrAccountBadInput)
		}
		q = q.SetCacheWriteMinContextTokens(*in.CacheWriteMinContextTokens)
	}
	if in.Priority != nil {
		q = q.SetPriority(*in.Priority)
	}
	if in.Weight != nil {
		q = q.SetWeight(*in.Weight)
	}
	if in.MaxConcurrency != nil {
		q = q.SetMaxConcurrency(*in.MaxConcurrency)
	}
	if in.Note != nil {
		q = q.SetNote(*in.Note)
	}
	if in.PoolMode != nil {
		raw := strings.TrimSpace(*in.PoolMode)
		mode, ok := ParsePoolMode(raw)
		if !ok {
			return nil, fmt.Errorf("%w: pool_mode must be balanced, conservative, lenient, observe or empty", ErrAccountBadInput)
		}
		q = q.SetPoolMode(string(mode))
	}
	acct, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}
	return acct, nil
}

// Delete soft-deletes an account. Usage history keeps its provider_account_id.
func (s *AccountPoolSvc) Delete(ctx context.Context, id uuid.UUID) error {
	n, err := s.d.Ent.UpstreamAccount.Update().
		Where(entaccount.IDEQ(id), entaccount.DeletedAtIsNil()).
		SetDeletedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAccountNotFound
	}
	_ = s.d.Redis.Del(ctx, accountCooldownKey(id)).Err()
	return nil
}

func (s *AccountPoolSvc) encodeCredential(cred *providerkit.Credential) (string, error) {
	plain, err := cred.Encode()
	if err != nil {
		return "", err
	}
	return s.cipher.Encrypt(plain)
}

func (s *AccountPoolSvc) decodeCredential(acct *ent.UpstreamAccount) (*providerkit.Credential, error) {
	if acct == nil || strings.TrimSpace(acct.CredentialEnc) == "" {
		return nil, ErrAccountNoCredential
	}
	plain, err := s.cipher.Decrypt(acct.CredentialEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt provider account credential: %w", err)
	}
	return providerkit.DecodeCredential(plain)
}

// ---- credentials at request time ----

// RequestToken resolves the API key to present upstream.
func (s *AccountPoolSvc) RequestToken(ctx context.Context, acct *ent.UpstreamAccount) (string, error) {
	cred, err := s.decodeCredential(acct)
	if err != nil {
		return "", err
	}
	token := cred.RequestToken()
	if strings.TrimSpace(token) == "" {
		return "", ErrAccountNoCredential
	}
	return token, nil
}

// StartMaintenanceLoop recomputes account health and probes stale quota
// snapshots every tick. Leader-only: running the sweep everywhere only adds
// load on the vendors' usage endpoints.
func (s *AccountPoolSvc) StartMaintenanceLoop(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = 5 * time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshHealth(ctx)
			// Remaining-quota probes for accounts the passive sampler has not
			// heard from (idle accounts would otherwise never learn their
			// windows).
			if probed, err := s.RefreshStaleQuotas(ctx, 15*time.Minute, 4); err != nil {
				slog.Warn("provider_account.quota_loop_failed", "err", err.Error())
			} else if probed > 0 {
				slog.Info("provider_account.quota_probed", "count", probed)
			}
		}
	}
}

// CountByPool reports how many live accounts each pool holds, for the pool
// list. The member table is tiny, so the count is a projection in Go.
func (s *AccountPoolSvc) CountByPool(ctx context.Context, poolIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	out := make(map[uuid.UUID]int, len(poolIDs))
	if len(poolIDs) == 0 {
		return out, nil
	}
	rows, err := s.d.Ent.AccountPoolMember.Query().
		Where(entaccountpoolmember.PoolIDIn(poolIDs...)).
		Select(entaccountpoolmember.FieldPoolID).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.PoolID]++
	}
	return out, nil
}

// ---- scheduling runtime ----

func accountCooldownKey(id uuid.UUID) string {
	return "provider_account:runtime_cooldown:" + id.String()
}

func accountLastUsedKey(id uuid.UUID) string {
	return "provider_account:last_used:" + id.String()
}

func accountInflightKey(id uuid.UUID) string {
	return "provider_account:inflight:" + id.String()
}

func providerAccountStickyKeyUserGroup(userID, modelGroupID uuid.UUID) string {
	return "acct:sticky:ug:" + userID.String() + ":" + modelGroupID.String()
}

func providerAccountStickyKeyConv(conversationID string) string {
	return "acct:sticky:conv:" + conversationID
}

// AllAccounts loads every live account (the admin list and the pool counts).
func (s *AccountPoolSvc) AllAccounts(ctx context.Context) ([]*ent.UpstreamAccount, error) {
	return s.d.Ent.UpstreamAccount.Query().
		Where(entaccount.DeletedAtIsNil()).
		Order(ent.Asc(entaccount.FieldPriority), ent.Asc(entaccount.FieldCreatedAt)).
		All(ctx)
}

// ResolveBinding returns the account a binding pins this scope to, when one
// exists for the provider. Specificity order: conversation > api_key > user >
// organization. uuid.Nil means "no binding".
func (s *AccountPoolSvc) ResolveBinding(ctx context.Context, userID uuid.UUID, apiKeyID *uuid.UUID, organizationID *uuid.UUID, conversationID string) (uuid.UUID, error) {
	conds := make([]predicate.UpstreamAccountBinding, 0, 4)
	if conv := strings.TrimSpace(conversationID); conv != "" {
		conds = append(conds, entaccountbinding.And(
			entaccountbinding.ScopeTypeEQ(entaccountbinding.ScopeTypeConversation),
			entaccountbinding.ScopeIDEQ(conv),
		))
	}
	if apiKeyID != nil && *apiKeyID != uuid.Nil {
		conds = append(conds, entaccountbinding.And(
			entaccountbinding.ScopeTypeEQ(entaccountbinding.ScopeTypeAPIKey),
			entaccountbinding.ScopeIDEQ(apiKeyID.String()),
		))
	}
	if userID != uuid.Nil {
		conds = append(conds, entaccountbinding.And(
			entaccountbinding.ScopeTypeEQ(entaccountbinding.ScopeTypeUser),
			entaccountbinding.ScopeIDEQ(userID.String()),
		))
	}
	if organizationID != nil && *organizationID != uuid.Nil {
		conds = append(conds, entaccountbinding.And(
			entaccountbinding.ScopeTypeEQ(entaccountbinding.ScopeTypeOrganization),
			entaccountbinding.ScopeIDEQ(organizationID.String()),
		))
	}
	if len(conds) == 0 {
		return uuid.Nil, nil
	}
	now := time.Now()
	rows, err := s.d.Ent.UpstreamAccountBinding.Query().
		Where(
			entaccountbinding.Or(
				entaccountbinding.ExpiresAtIsNil(),
				entaccountbinding.ExpiresAtGT(now),
			),
			entaccountbinding.Or(conds...),
		).
		All(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	rank := map[entaccountbinding.ScopeType]int{
		entaccountbinding.ScopeTypeConversation: 0,
		entaccountbinding.ScopeTypeAPIKey:       1,
		entaccountbinding.ScopeTypeUser:         2,
		entaccountbinding.ScopeTypeOrganization: 3,
	}
	best := uuid.Nil
	bestRank := len(rank)
	for _, row := range rows {
		r, ok := rank[row.ScopeType]
		if !ok || r >= bestRank {
			continue
		}
		best, bestRank = row.AccountID, r
	}
	return best, nil
}

// LookupSticky returns the account a (user, group) or conversation window is
// pinned to, or uuid.Nil. Mirrors StickySvc, one level down.
func (s *AccountPoolSvc) LookupSticky(ctx context.Context, userID, modelGroupID uuid.UUID, conversationID string) (uuid.UUID, error) {
	ttl, err := s.stickyTTL(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if ttl <= 0 {
		return uuid.Nil, nil
	}
	for _, key := range providerAccountStickyKeys(userID, modelGroupID, conversationID) {
		value, err := s.d.Redis.Get(ctx, key).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			return uuid.Nil, err
		}
		id, perr := uuid.Parse(strings.TrimSpace(value))
		if perr != nil {
			_ = s.d.Redis.Del(ctx, key).Err()
			continue
		}
		return id, nil
	}
	return uuid.Nil, nil
}

// RememberSticky pins the account that just served a request.
func (s *AccountPoolSvc) RememberSticky(ctx context.Context, userID, modelGroupID uuid.UUID, conversationID string, accountID uuid.UUID) error {
	ttl, err := s.stickyTTL(ctx)
	if err != nil {
		return err
	}
	if ttl <= 0 || accountID == uuid.Nil {
		return nil
	}
	keys := providerAccountStickyKeys(userID, modelGroupID, conversationID)
	if len(keys) == 0 {
		return nil
	}
	dur := time.Duration(ttl) * time.Second
	pipe := s.d.Redis.Pipeline()
	for _, key := range keys {
		pipe.Set(ctx, key, accountID.String(), dur)
		pipe.Del(ctx, key+":failures")
	}
	_, err = pipe.Exec(ctx)
	return err
}

// RecordStickyFailure counts consecutive failures on the pinned account and
// drops the pin once threshold is reached.
func (s *AccountPoolSvc) RecordStickyFailure(ctx context.Context, userID, modelGroupID, accountID uuid.UUID, conversationID string, threshold int) (bool, error) {
	if accountID == uuid.Nil {
		return false, nil
	}
	if threshold <= 0 {
		threshold = 2
	}
	ttl, err := s.stickyTTL(ctx)
	if err != nil {
		return false, err
	}
	if ttl <= 0 {
		return false, nil
	}
	dur := time.Duration(ttl) * time.Second
	active := make([]string, 0, 2)
	drop := false
	for _, key := range providerAccountStickyKeys(userID, modelGroupID, conversationID) {
		value, err := s.d.Redis.Get(ctx, key).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			return false, err
		}
		if parsed, perr := uuid.Parse(strings.TrimSpace(value)); perr != nil || parsed != accountID {
			continue
		}
		active = append(active, key)
		n, err := s.d.Redis.Incr(ctx, key+":failures").Result()
		if err != nil {
			return false, err
		}
		_ = s.d.Redis.Expire(ctx, key+":failures", dur).Err()
		if int(n) >= threshold {
			drop = true
		}
	}
	if len(active) == 0 {
		return false, nil
	}
	if drop {
		pipe := s.d.Redis.Pipeline()
		for _, key := range active {
			pipe.Del(ctx, key, key+":failures")
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func providerAccountStickyKeys(userID, modelGroupID uuid.UUID, conversationID string) []string {
	if conv := strings.TrimSpace(conversationID); conv != "" {
		return []string{providerAccountStickyKeyConv(conv)}
	}
	if userID != uuid.Nil && modelGroupID != uuid.Nil {
		return []string{providerAccountStickyKeyUserGroup(userID, modelGroupID)}
	}
	return nil
}

func (s *AccountPoolSvc) stickyTTL(ctx context.Context) (int, error) {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return 0, err
	}
	return cfg.StickyTTLSeconds, nil
}

// MarkCooldown parks an account after a retryable failure.
func (s *AccountPoolSvc) MarkCooldown(ctx context.Context, id uuid.UUID, reason string, ttl time.Duration) error {
	if id == uuid.Nil || ttl <= 0 {
		return nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "runtime_failure"
	}
	return s.d.Redis.Set(ctx, accountCooldownKey(id), reason, ttl).Err()
}

// ClearCooldown ends a cooldown early (operator action).
func (s *AccountPoolSvc) ClearCooldown(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return nil
	}
	return s.d.Redis.Del(ctx, accountCooldownKey(id)).Err()
}

// CooldownInfo reports the recorded reason and remaining TTL.
func (s *AccountPoolSvc) CooldownInfo(ctx context.Context, id uuid.UUID) (bool, string, time.Duration, error) {
	if id == uuid.Nil {
		return false, "", 0, nil
	}
	key := accountCooldownKey(id)
	reason, err := s.d.Redis.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return false, "", 0, nil
	}
	if err != nil {
		return false, "", 0, err
	}
	var ttl time.Duration
	if d, terr := s.d.Redis.TTL(ctx, key).Result(); terr == nil && d > 0 {
		ttl = d
	}
	return true, reason, ttl, nil
}

// Acquire takes an in-flight slot when the account caps concurrency. A limit
// of 0 (or negative) means unlimited and never touches Redis; Redis trouble
// fails open, because an outage must not stall traffic.
func (s *AccountPoolSvc) Acquire(ctx context.Context, id uuid.UUID, limit int) bool {
	if id == uuid.Nil || limit <= 0 {
		return true
	}
	key := accountInflightKey(id)
	n, err := s.d.Redis.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	_ = s.d.Redis.Expire(ctx, key, time.Hour).Err()
	if n > int64(limit) {
		_ = s.d.Redis.Decr(ctx, key).Err()
		return false
	}
	return true
}

// Release returns an in-flight slot taken by Acquire.
func (s *AccountPoolSvc) Release(ctx context.Context, id uuid.UUID, limit int) {
	if id == uuid.Nil || limit <= 0 {
		return
	}
	key := accountInflightKey(id)
	if n, err := s.d.Redis.Decr(ctx, key).Result(); err == nil && n <= 0 {
		_ = s.d.Redis.Del(ctx, key).Err()
	}
}

// RecordOutcome feeds one attempt result into the account health window. The
// counters are cheap Redis increments; the health column is recomputed by
// RefreshHealth on the leader loop, never on the request path.
func (s *AccountPoolSvc) RecordOutcome(ctx context.Context, id uuid.UUID, ok bool) {
	if id == uuid.Nil || s.d.Redis == nil {
		return
	}
	base := fmt.Sprintf("ahealth:%s:%d", id.String(), time.Now().Unix()/60)
	ttl := 6 * time.Minute
	pipe := s.d.Redis.Pipeline()
	pipe.Incr(ctx, base+":total")
	pipe.Expire(ctx, base+":total", ttl)
	if !ok {
		pipe.Incr(ctx, base+":fail")
		pipe.Expire(ctx, base+":fail", ttl)
	}
	_, _ = pipe.Exec(ctx)
}

// RefreshHealth recomputes the health column for every live account from its
// rolling window. Runs on the leader loop (seconds-scale lag is invisible for
// a scheduling weight) so no request pays a recompute.
func (s *AccountPoolSvc) RefreshHealth(ctx context.Context) {
	accounts, err := s.d.Ent.UpstreamAccount.Query().
		Where(entaccount.DeletedAtIsNil()).
		Select(entaccount.FieldID).
		All(ctx)
	if err != nil {
		slog.Debug("provider_account.health_query_failed", "err", err.Error())
		return
	}
	now := time.Now().Unix() / 60
	for _, acct := range accounts {
		keys := make([]string, 0, 10)
		for i := 0; i < 5; i++ {
			base := fmt.Sprintf("ahealth:%s:%d", acct.ID.String(), now-int64(i))
			keys = append(keys, base+":total", base+":fail")
		}
		values, err := s.d.Redis.MGet(ctx, keys...).Result()
		if err != nil || len(values) != len(keys) {
			continue
		}
		total, failed := 0, 0
		for i := 0; i < len(values); i += 2 {
			total += redisInt(values[i])
			failed += redisInt(values[i+1])
		}
		current, err := s.d.Ent.UpstreamAccount.Get(ctx, acct.ID)
		if err != nil {
			continue
		}
		if total < 5 {
			// Too few attempts to judge — and a mark without fresh evidence is
			// stale (e.g. inherited verbatim by the pool migration, or left by
			// a failure burst whose traffic has since stopped). Roll degraded/
			// unhealthy back to unknown so a quiet account is not pinned
			// forever by evidence that no longer exists.
			if current.Health == entaccount.HealthUnhealthy || current.Health == entaccount.HealthDegraded {
				_, _ = s.d.Ent.UpstreamAccount.UpdateOneID(acct.ID).SetHealth(entaccount.HealthUnknown).Save(ctx)
			}
			continue
		}
		rate := float64(failed) / float64(total)
		next := entaccount.HealthHealthy
		switch {
		case rate >= 0.90:
			next = entaccount.HealthUnhealthy
		case rate >= 0.60:
			next = entaccount.HealthDegraded
		}
		if current.Health == next {
			continue
		}
		_, _ = s.d.Ent.UpstreamAccount.UpdateOneID(acct.ID).SetHealth(next).Save(ctx)
	}
}

func redisInt(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		n := 0
		for i := 0; i < len(t); i++ {
			if t[i] < '0' || t[i] > '9' {
				return n
			}
			n = n*10 + int(t[i]-'0')
		}
		return n
	case int64:
		return int(t)
	case int:
		return t
	default:
		return 0
	}
}

// TouchLastUsed records that an account just carried a request. The write is
// throttled to one per account per window: the scheduling row is read by every
// request, so a per-request UPDATE would turn a small table into a hot row.
func (s *AccountPoolSvc) TouchLastUsed(ctx context.Context, id uuid.UUID) {
	if id == uuid.Nil || s.d.Redis == nil {
		return
	}
	throttled, err := s.d.Redis.SetNX(ctx, accountLastUsedKey(id), "1", lastUsedThrottle).Result()
	if err != nil || !throttled {
		return
	}
	go func() {
		updateCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = s.d.Ent.UpstreamAccount.UpdateOneID(id).SetLastUsedAt(time.Now()).Save(updateCtx)
	}()
}

// ---- bindings ----

type CreateAccountBindingInput struct {
	ScopeType  string
	ScopeID    string
	ProviderID uuid.UUID
	AccountID  uuid.UUID
	ExpiresAt  *time.Time
	CreatedBy  uuid.UUID
	Note       string
}

func (s *AccountPoolSvc) CreateBinding(ctx context.Context, in CreateAccountBindingInput) (*ent.UpstreamAccountBinding, error) {
	scopeType := entaccountbinding.ScopeType(strings.TrimSpace(in.ScopeType))
	switch scopeType {
	case entaccountbinding.ScopeTypeConversation, entaccountbinding.ScopeTypeUser,
		entaccountbinding.ScopeTypeAPIKey, entaccountbinding.ScopeTypeOrganization:
	default:
		return nil, fmt.Errorf("%w: scope_type must be conversation, user, api_key or organization", ErrAccountBadInput)
	}
	scopeID := strings.TrimSpace(in.ScopeID)
	if scopeID == "" {
		return nil, fmt.Errorf("%w: scope_id is required", ErrAccountBadInput)
	}
	if scopeType != entaccountbinding.ScopeTypeConversation {
		if _, err := uuid.Parse(scopeID); err != nil {
			return nil, fmt.Errorf("%w: scope_id must be a uuid for %s bindings", ErrAccountBadInput, scopeType)
		}
	}
	if _, err := s.Get(ctx, in.AccountID); err != nil {
		return nil, err
	}
	q := s.d.Ent.UpstreamAccountBinding.Create().
		SetScopeType(scopeType).
		SetScopeID(scopeID).
		SetAccountID(in.AccountID).
		SetNote(in.Note)
	if in.ExpiresAt != nil {
		q = q.SetExpiresAt(*in.ExpiresAt)
	}
	if in.CreatedBy != uuid.Nil {
		q = q.SetCreatedBy(in.CreatedBy)
	}
	return q.Save(ctx)
}

// ListBindings returns the explicit scope → account pins. accountID narrows
// the list to one account (the admin dialog's view); nil means "all".
func (s *AccountPoolSvc) ListBindings(ctx context.Context, accountID *uuid.UUID) ([]*ent.UpstreamAccountBinding, error) {
	q := s.d.Ent.UpstreamAccountBinding.Query()
	if accountID != nil {
		q = q.Where(entaccountbinding.AccountIDEQ(*accountID))
	}
	return q.Order(ent.Desc(entaccountbinding.FieldCreatedAt)).All(ctx)
}

func (s *AccountPoolSvc) DeleteBinding(ctx context.Context, id uuid.UUID) error {
	err := s.d.Ent.UpstreamAccountBinding.DeleteOneID(id).Exec(ctx)
	if err != nil && ent.IsNotFound(err) {
		return ErrAccountNotFound
	}
	return err
}

// ---- views ----

// AccountView is the admin-facing projection: no secrets, ever.
type AccountView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Endpoint & protocol declaration: everything that used to live on the
	// channel row is now part of the account.
	BaseURL         string   `json:"base_url"`
	APIFormat       string   `json:"api_format,omitempty"`
	NativeProtocols []string `json:"native_protocols"`
	DefaultProtocol string   `json:"default_protocol,omitempty"`
	AllowConversion bool     `json:"allow_conversion"`
	// EndpointPaths are the per-protocol upstream path overrides this account
	// declared ("" for the vast majority, which follow the gateway defaults).
	EndpointPaths map[string]string `json:"endpoint_paths,omitempty"`
	// HeaderRules are the upstream header rewrites (nil when none).
	HeaderRules *schema.HeaderRules `json:"header_rules,omitempty"`
	// Organization is non-secret configuration an admin edits. It is
	// returned so the edit dialog can prefill it: a form that cannot see a
	// value would submit its own default and silently overwrite the stored
	// one.
	Organization *string `json:"organization,omitempty"`
	// Request shaping.
	BlockImageGenerationTool   bool `json:"block_image_generation_tool"`
	EstimateCacheWrite         bool `json:"estimate_cache_write"`
	CacheWriteMinContextTokens int  `json:"cache_write_min_context_tokens"`
	// LegacyProviderID links a migrated account to the channel row it came
	// from ("" for accounts created after the migration).
	LegacyProviderID string     `json:"legacy_provider_id,omitempty"`
	Status           string     `json:"status"`
	Health           string     `json:"health"`
	Priority         int        `json:"priority"`
	Weight           int        `json:"weight"`
	MaxConcurrency   int        `json:"max_concurrency"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	Note             string     `json:"note,omitempty"`
	HasCredential    bool       `json:"has_credential"`
	CooldownActive   bool       `json:"cooldown_active"`
	CooldownReason   string     `json:"cooldown_reason,omitempty"`
	CooldownTTLSecs  int        `json:"cooldown_ttl_seconds,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`

	// --- health state (account state machine) ---
	// Schedulable is the runtime pause switch; Status stays as declared so an
	// operator can tell "parked" from "broken".
	Schedulable bool `json:"schedulable"`
	// PoolMode is the account override; empty means "inherit".
	PoolMode      string `json:"pool_mode,omitempty"`
	EffectiveMode string `json:"effective_pool_mode"`
	// Cooldowns: each is nil when inactive, so the UI can render a countdown
	// straight from the timestamp.
	RateLimitResetAt        *time.Time     `json:"rate_limit_reset_at,omitempty"`
	OverloadUntil           *time.Time     `json:"overload_until,omitempty"`
	TempUnschedulableUntil  *time.Time     `json:"temp_unschedulable_until,omitempty"`
	TempUnschedulableReason string         `json:"temp_unschedulable_reason,omitempty"`
	LastErrorClass          string         `json:"last_error_class,omitempty"`
	LastErrorAt             *time.Time     `json:"last_error_at,omitempty"`
	ConsecutiveFailures     int            `json:"consecutive_failures"`
	ModelCooldowns          map[string]any `json:"model_cooldowns,omitempty"`
	QuotaSnapshot           map[string]any `json:"quota_snapshot,omitempty"`
	QuotaCheckedAt          *time.Time     `json:"quota_checked_at,omitempty"`
	// HasUsageEndpoint tells the admin UI whether a quota refresh can work at
	// all: only some provider definitions have a usage endpoint.
	HasUsageEndpoint bool `json:"has_usage_endpoint"`
	// SchedulableNow is the single verdict the scheduler uses, so the admin UI
	// and the gateway can never disagree about whether an account is eligible.
	SchedulableNow bool `json:"schedulable_now"`
	// DefinitionID names the provider definition this account was created from.
	DefinitionID string `json:"definition_id,omitempty"`
}

// legacyProviderIDString renders the migration link ("" when absent).
func legacyProviderIDString(id *uuid.UUID) string {
	if id == nil || *id == uuid.Nil {
		return ""
	}
	return id.String()
}

func (s *AccountPoolSvc) View(ctx context.Context, acct *ent.UpstreamAccount) AccountView {
	view := AccountView{
		ID:               acct.ID.String(),
		Name:             acct.Name,
		BaseURL:          acct.BaseURL,
		APIFormat:        acct.APIFormat,
		NativeProtocols:  append([]string(nil), acct.NativeProtocols...),
		DefaultProtocol:  acct.DefaultProtocol,
		AllowConversion:  acct.AllowConversion,
		DefinitionID:     acct.DefinitionID,
		LegacyProviderID: legacyProviderIDString(acct.LegacyProviderID),
		Status:           string(acct.Status),
		Health:           string(acct.Health),
		Priority:         acct.Priority,
		Weight:           acct.Weight,
		MaxConcurrency:   acct.MaxConcurrency,

		Organization:               acct.Organization,
		BlockImageGenerationTool:   acct.BlockImageGenerationTool,
		EstimateCacheWrite:         acct.EstimateCacheWrite,
		CacheWriteMinContextTokens: acct.CacheWriteMinContextTokens,

		LastUsedAt:    acct.LastUsedAt,
		LastError:     acct.LastError,
		Note:          acct.Note,
		CreatedAt:     acct.CreatedAt,
		UpdatedAt:     acct.UpdatedAt,
		HasCredential: strings.TrimSpace(acct.CredentialEnc) != "",
	}
	if active, reason, ttl, err := s.CooldownInfo(ctx, acct.ID); err == nil && active {
		view.CooldownActive = true
		view.CooldownReason = reason
		view.CooldownTTLSecs = int(ttl.Seconds())
	}
	view.Schedulable = acct.Schedulable
	view.PoolMode = acct.PoolMode
	view.EffectiveMode = string(s.ResolvePoolMode(ctx, acct))
	view.RateLimitResetAt = acct.RateLimitResetAt
	view.OverloadUntil = acct.OverloadUntil
	view.TempUnschedulableUntil = acct.TempUnschedulableUntil
	view.TempUnschedulableReason = acct.TempUnschedulableReason
	view.LastErrorClass = acct.LastErrorClass
	view.LastErrorAt = acct.LastErrorAt
	view.ConsecutiveFailures = acct.ConsecutiveFailures
	view.ModelCooldowns = acct.ModelCooldowns
	view.EndpointPaths = EndpointPathsOf(acct)
	if !HeaderRulesEmpty(acct.HeaderRules) {
		rules := acct.HeaderRules
		view.HeaderRules = &rules
	}
	view.QuotaSnapshot = acct.QuotaSnapshot
	view.QuotaCheckedAt = acct.QuotaCheckedAt
	view.HasUsageEndpoint = s.usageProbeSupported(ctx, acct)
	view.SchedulableNow = AccountSchedulable(acct, "", time.Now())
	return view
}
