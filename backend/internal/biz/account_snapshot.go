package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entaccountmodel "github.com/jucode/jucode/internal/data/ent/upstreamaccountmodel"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/providerkit/catalog"
	"github.com/jucode/jucode/internal/region"
)

// RegionalSnapshot projects the account pool for the edge nodes.
//
// The wire struct is the historical region.ProviderSnapshot: its ID is now the
// *account* id and its Kind is derived from the account's native protocols, so
// an edge node keeps working with the same protocol while the main site has one
// layer instead of two. An edge schedules by the id it is given and forwards to
// the endpoint it is told about, so nothing else about it changes.
func (s *AccountPoolSvc) RegionalSnapshot(ctx context.Context, transport *cryptopkg.Cipher) ([]region.ProviderSnapshot, error) {
	if transport == nil {
		return nil, errors.New("regional transport cipher is not configured")
	}
	accounts, err := s.d.Ent.UpstreamAccount.Query().
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	models, err := s.d.Ent.UpstreamAccountModel.Query().
		Where(entaccountmodel.AccountIDIn(ids...), entaccountmodel.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	modelsByAccount := make(map[uuid.UUID][]*ent.UpstreamAccountModel, len(accounts))
	for _, m := range models {
		modelsByAccount[m.AccountID] = append(modelsByAccount[m.AccountID], m)
	}

	out := make([]region.ProviderSnapshot, 0, len(accounts))
	for _, acct := range accounts {
		if acct.DeletedAt != nil {
			continue
		}
		envelope := ""
		if enc := acct.CredentialEnc; enc != "" {
			plain, err := s.cipher.Decrypt(enc)
			if err != nil {
				return nil, fmt.Errorf("decrypt account %s: %w", acct.ID, err)
			}
			envelope, err = transport.Encrypt(plain)
			if err != nil {
				return nil, fmt.Errorf("wrap account %s credential: %w", acct.ID, err)
			}
		}
		snap := region.ProviderSnapshot{
			ID:             acct.ID,
			Name:           acct.Name,
			Kind:           accountKindForSnapshot(acct),
			BaseURL:        acct.BaseURL,
			Protocols:      accountSnapshotProtocols(acct),
			APIFormat:      acct.APIFormat,
			APIKeyEnvelope: envelope,
			Enabled:        acct.Status == "active" && acct.Schedulable,
			Weight:         acct.Weight,
			UpstreamModels: map[string]string{},
			Prices:         map[string]region.ModelPrice{},
			Version:        acct.UpdatedAt.UnixNano(),
			UpdatedAt:      acct.UpdatedAt,
		}
		if acct.Organization != nil {
			snap.Organization = *acct.Organization
		}
		for _, model := range modelsByAccount[acct.ID] {
			snap.Models = append(snap.Models, model.ModelName)
			if model.UpstreamModel != "" {
				snap.UpstreamModels[model.ModelName] = model.UpstreamModel
			}
			price := region.ModelPrice{}
			if model.PricePerRequestCost != nil {
				price.PerRequestCNY = model.PricePerRequestCost.String()
			}
			if model.InputUnitCost != nil {
				price.InputPer1K = model.InputUnitCost.String()
			}
			if model.OutputUnitCost != nil {
				price.OutputPer1K = model.OutputUnitCost.String()
			}
			snap.Prices[model.ModelName] = price
		}
		out = append(out, snap)
	}
	return out, nil
}

// accountKindForSnapshot derives the legacy "kind" the edge still switches on.
func accountKindForSnapshot(acct *ent.UpstreamAccount) string {
	if AccountHasNativeProtocol(acct, NativeProtocolMessages) {
		return "anthropic"
	}
	return "openai_compatible"
}

// accountSnapshotProtocols maps native protocols onto the legacy protocol
// vocabulary the edge understands ("openai" = OpenAI family).
func accountSnapshotProtocols(acct *ent.UpstreamAccount) []string {
	out := make([]string, 0, len(acct.NativeProtocols))
	for _, proto := range AccountProtocols(acct) {
		switch proto {
		case NativeProtocolMessages:
			out = append(out, "anthropic")
		default:
			out = append(out, "openai")
		}
	}
	return out
}

// ---- account model bindings ----

// AccountModelInput is one model binding under an account: which logical model
// the account serves, an optional upstream rename, and what the account pays
// for it (the cost basis the finance reports read).
type AccountModelInput struct {
	ModelName     string
	UpstreamModel string
	// UpstreamProtocol pins the wire dialect for this model on this account
	// ("chat_completions" | "responses" | "messages"); empty inherits the
	// account-level rules. Aliases are accepted and canonicalized on write.
	UpstreamProtocol     string
	Enabled              *bool
	CostMode             string
	Currency             string
	PricePerRequestCost  *decimal.Decimal
	InputUnitCost        *decimal.Decimal
	OutputUnitCost       *decimal.Decimal
	CacheReadUnitCost    *decimal.Decimal
	CacheWriteUnitCost   *decimal.Decimal
	CacheWrite1hUnitCost *decimal.Decimal
	// RefInputCost / RefOutputCost are display-only reference prices (USD
	// per 1M tokens) copied from the public catalog or the upstream list.
	// They never feed billing — the *UnitCost fields above do.
	RefInputCost  *decimal.Decimal
	RefOutputCost *decimal.Decimal
	// ContextWindow overrides models.context_window for this account:
	// nil = no change, 0 = clear (inherit the model default).
	ContextWindow *int
}

func applyAccountModelCreate(q *ent.UpstreamAccountModelCreate, in AccountModelInput) *ent.UpstreamAccountModelCreate {
	if in.ContextWindow != nil && *in.ContextWindow > 0 {
		q = q.SetContextWindow(*in.ContextWindow)
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	if in.CostMode != "" {
		q = q.SetCostMode(entaccountmodel.CostMode(in.CostMode))
	}
	if in.Currency != "" {
		q = q.SetCurrency(in.Currency)
	}
	if in.PricePerRequestCost != nil {
		q = q.SetPricePerRequestCost(*in.PricePerRequestCost)
	}
	if in.InputUnitCost != nil {
		q = q.SetInputUnitCost(*in.InputUnitCost)
	}
	if in.OutputUnitCost != nil {
		q = q.SetOutputUnitCost(*in.OutputUnitCost)
	}
	if in.CacheReadUnitCost != nil {
		q = q.SetCacheReadInputCost(*in.CacheReadUnitCost)
	}
	if in.CacheWriteUnitCost != nil {
		q = q.SetCacheWriteInputCost(*in.CacheWriteUnitCost)
	}
	if in.CacheWrite1hUnitCost != nil {
		q = q.SetCacheWrite1hInputCost(*in.CacheWrite1hUnitCost)
	}
	if in.RefInputCost != nil {
		q = q.SetRefInputCost(*in.RefInputCost)
	}
	if in.RefOutputCost != nil {
		q = q.SetRefOutputCost(*in.RefOutputCost)
	}
	return q
}

func applyAccountModelUpdate(q *ent.UpstreamAccountModelUpdateOne, in AccountModelInput) *ent.UpstreamAccountModelUpdateOne {
	if in.ContextWindow != nil {
		if *in.ContextWindow > 0 {
			q = q.SetContextWindow(*in.ContextWindow)
		} else {
			q = q.ClearContextWindow()
		}
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	if in.CostMode != "" {
		q = q.SetCostMode(entaccountmodel.CostMode(in.CostMode))
	}
	if in.Currency != "" {
		q = q.SetCurrency(in.Currency)
	}
	if in.PricePerRequestCost != nil {
		q = q.SetPricePerRequestCost(*in.PricePerRequestCost)
	}
	if in.InputUnitCost != nil {
		q = q.SetInputUnitCost(*in.InputUnitCost)
	}
	if in.OutputUnitCost != nil {
		q = q.SetOutputUnitCost(*in.OutputUnitCost)
	}
	if in.CacheReadUnitCost != nil {
		q = q.SetCacheReadInputCost(*in.CacheReadUnitCost)
	}
	if in.CacheWriteUnitCost != nil {
		q = q.SetCacheWriteInputCost(*in.CacheWriteUnitCost)
	}
	if in.CacheWrite1hUnitCost != nil {
		q = q.SetCacheWrite1hInputCost(*in.CacheWrite1hUnitCost)
	}
	if in.RefInputCost != nil {
		q = q.SetRefInputCost(*in.RefInputCost)
	}
	if in.RefOutputCost != nil {
		q = q.SetRefOutputCost(*in.RefOutputCost)
	}
	return q
}

// normalizeModelProtocol canonicalizes an admin-supplied per-model protocol
// pin; "" stays "". Unknown values are rejected so a typo cannot silently
// disable a route.
func normalizeModelProtocol(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	proto, ok := ParseNativeProtocol(raw)
	if !ok {
		return "", fmt.Errorf("%w: %v", ErrAccountBadInput, ErrUnknownProtocol)
	}
	return proto, nil
}

// AddAccountModel binds one model to an account (the routing table the
// scheduler reads).
func (s *AccountPoolSvc) AddAccountModel(ctx context.Context, accountID uuid.UUID, in AccountModelInput) (*ent.UpstreamAccountModel, error) {
	if _, err := s.Get(ctx, accountID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.ModelName) == "" {
		return nil, fmt.Errorf("%w: model_name is required", ErrAccountBadInput)
	}
	proto, err := normalizeModelProtocol(in.UpstreamProtocol)
	if err != nil {
		return nil, err
	}
	q := s.d.Ent.UpstreamAccountModel.Create().
		SetAccountID(accountID).
		SetModelName(strings.TrimSpace(in.ModelName)).
		SetUpstreamModel(strings.TrimSpace(in.UpstreamModel)).
		SetUpstreamProtocol(proto)
	q = applyAccountModelCreate(q, in)
	row, err := q.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, fmt.Errorf("%w: model %q is already bound to this account", ErrAccountBadInput, in.ModelName)
		}
		return nil, err
	}
	return row, nil
}

// UpdateAccountModel patches one binding.
func (s *AccountPoolSvc) UpdateAccountModel(ctx context.Context, id uuid.UUID, in AccountModelInput) (*ent.UpstreamAccountModel, error) {
	proto, err := normalizeModelProtocol(in.UpstreamProtocol)
	if err != nil {
		return nil, err
	}
	q := s.d.Ent.UpstreamAccountModel.UpdateOneID(id)
	if strings.TrimSpace(in.ModelName) != "" {
		q = q.SetModelName(strings.TrimSpace(in.ModelName))
	}
	q = q.SetUpstreamModel(strings.TrimSpace(in.UpstreamModel)).
		SetUpstreamProtocol(proto)
	q = applyAccountModelUpdate(q, in)
	row, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}
	return row, nil
}

// ListAccountModels returns every model bound to one account.
func (s *AccountPoolSvc) ListAccountModels(ctx context.Context, accountID uuid.UUID) ([]*ent.UpstreamAccountModel, error) {
	return s.d.Ent.UpstreamAccountModel.Query().
		Where(entaccountmodel.AccountIDEQ(accountID)).
		Order(ent.Asc(entaccountmodel.FieldModelName)).
		All(ctx)
}

// RemoveAccountModel unbinds one model.
func (s *AccountPoolSvc) RemoveAccountModel(ctx context.Context, id uuid.UUID) error {
	if err := s.d.Ent.UpstreamAccountModel.DeleteOneID(id).Exec(ctx); err != nil {
		if ent.IsNotFound(err) {
			return ErrAccountNotFound
		}
		return err
	}
	return nil
}

// CatalogProviderDefaults maps a vendored-catalog provider id onto this
// gateway's account fields: base URL, the dialect family, the account-side
// native protocols. ok=false when the catalog has no
// such provider or its dialect is one the gateway cannot speak yet.
func CatalogProviderDefaults(catalogID string) (baseURL, kind string, nativeProtocols []string, ok bool) {
	cat := catalog.MustLoad()
	p := cat.AuthProvider(catalogID)
	if p == nil {
		return "", "", nil, false
	}
	model := cat.DefaultModel(catalogID)
	if model == "" {
		if models := cat.SupportedModels(catalogID); len(models) > 0 {
			model = models[0].ID
		}
	}
	proto, found := cat.ProtocolFor(catalogID, model)
	if !found {
		return "", "", nil, false
	}
	switch proto {
	case catalog.ProtocolAnthropicMessages:
		kind, nativeProtocols = "anthropic", []string{NativeProtocolMessages}
	case catalog.ProtocolOpenAIChatCompletions:
		kind, nativeProtocols = "openai_compatible", []string{NativeProtocolChat}
	case catalog.ProtocolOpenAIResponses:
		kind, nativeProtocols = "openai_compatible", []string{NativeProtocolResponses}
	default:
		// Azure Responses shares the Responses body but needs its own
		// path/auth handling; Azure accounts use the azure-openai definition.
		return "", "", nil, false
	}
	baseURL = cat.DefaultBaseURL(catalogID)
	if baseURL == "" {
		return "", "", nil, false
	}
	return baseURL, kind, nativeProtocols, true
}
