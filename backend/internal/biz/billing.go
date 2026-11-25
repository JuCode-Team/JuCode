package biz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entmember "github.com/jucode/jucode/internal/data/ent/organizationmember"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

var (
	ErrInsufficientBalance       = errors.New("insufficient balance")
	ErrUnsupportedBillingSource  = errors.New("unsupported billing source")
	ErrMonthlySpendLimitExceeded = errors.New("monthly spend limit exceeded")
	ErrApiKeyCostLimitExceeded   = ErrApiKeyCostLimit
)

type BillingSvc struct {
	d        *data.Data
	plans    *PlanSvc
	settings *SettingsSvc
	// usageWriter batches usage_log writes. nil keeps the original
	// one-transaction-per-request behavior, which is what the rollback
	// switch (usage_log.batch_enabled) flips back to.
	usageWriter *UsageLogWriter
	// invites triggers consumption rebates off the usage-log write path.
	// Optional; nil disables rebates entirely (the async entry points are
	// nil-receiver-safe, so no call-site guards are needed).
	invites *InviteSvc
	// alerts receives provider business-health edges after persistence.
	// Optional; nil disables external operations notifications.
	alerts *OpsAlertSvc
}

// PlanChargeState captures the active user_plan + plan snapshot used for
// both precheck and post-response quota charging. Passing the same snapshot
// through the whole request avoids a second ActiveUserPlan lookup after the
// upstream response has already landed.
type PlanChargeState struct {
	UserPlan *ent.UserPlan
	Plan     *ent.Plan
}

func NewBillingSvc(d *data.Data, plans *PlanSvc, settings *SettingsSvc) *BillingSvc {
	return &BillingSvc{d: d, plans: plans, settings: settings}
}

// SetUsageLogWriter routes usage_log persistence through the async batch
// writer. Optional — leaving it unset keeps the synchronous per-request
// transaction.
func (s *BillingSvc) SetUsageLogWriter(w *UsageLogWriter) {
	s.usageWriter = w
}

// SetInvites injects the invite service (late-wired; see Account.SetInvites).
func (s *BillingSvc) SetInvites(inv *InviteSvc) { s.invites = inv }

func (s *BillingSvc) SetOpsAlerts(alerts *OpsAlertSvc) { s.alerts = alerts }

// TokenUsage is the token breakdown delivered by the gateway after parsing
// an upstream's usage block. CalcCost takes it (instead of bare ints) so
// the cache fields can be priced separately.
type TokenUsage struct {
	TokensIn              int // non-cached input tokens
	TokensOut             int
	CachedTokensIn        int // cache-hit input tokens
	CacheCreationTokensIn int // cache-write input tokens (5m+1h or aggregate)
	// CacheCreation5m/1hTokensIn is Anthropic's TTL split. Both zero with
	// CacheCreationTokensIn > 0 means the upstream only reported the
	// aggregate — CacheWriteSplit bills that total at the 5m rate.
	CacheCreation5mTokensIn int
	CacheCreation1hTokensIn int
	ImageTokensIn           int // built-in image-generation tool input tokens
	ImageTokensOut          int // built-in image-generation tool output tokens
	// AudioTokens*: the audio modality subset, already REMOVED from
	// TokensIn / TokensOut by the caller so the two never double-count.
	// Priced at Model.Audio*UnitPrice, falling back to the text price.
	AudioTokensIn  int
	AudioTokensOut int
	// OutputImages: count of images the model emitted in this
	// response. Charged at Model.OutputImagePrice each, ON TOP of
	// any token-based cost (text + image pricing stack so a model
	// that emits "two sentences + one image" pays for both).
	OutputImages int
	// OutputSeconds: seconds of generated audio/video, charged at
	// Model.OutputSecondPrice each (non-chat generation endpoints).
	OutputSeconds int
	// InputCharacters: input character count for TTS, charged at
	// Model.InputCharacterPrice each (audio_speech).
	InputCharacters int
}

// InputContextTokens is the input-side context size used to select a
// price tier: everything the model had to hold as prompt context,
// regardless of cache state or modality.
func (u TokenUsage) InputContextTokens() int64 {
	return int64(u.TokensIn) + int64(u.CachedTokensIn) + int64(u.CacheCreationTokensIn) + int64(u.AudioTokensIn)
}

// CacheWriteSplit returns the 5-minute and 1-hour cache-write counts
// used for pricing. Anthropic reports both in usage.cache_creation;
// when that nested object is missing (older Anthropic, GPT
// cache_write_tokens, or a gateway estimate) the aggregate
// CacheCreationTokensIn is billed entirely at the 5m rate.
func (u TokenUsage) CacheWriteSplit() (fiveM, oneH int) {
	if u.CacheCreation5mTokensIn == 0 && u.CacheCreation1hTokensIn == 0 {
		return u.CacheCreationTokensIn, 0
	}
	return u.CacheCreation5mTokensIn, u.CacheCreation1hTokensIn
}

// ApplyCacheWriteEstimate fills in a missing cache-write count for
// providers that don't report one (provider.estimate_cache_write on):
// the non-cached input remainder is RECLASSIFIED from TokensIn into
// CacheCreationTokensIn, so those tokens bill once at the cache-write
// rate instead of the input rate — never both. This mirrors the
// official Anthropic and OpenAI GPT-5.6+ semantics where input /
// cache-read / cache-write are disjoint sets, and it keeps
// InputContextTokens() (tier selection) unchanged.
//
// The switch lives on the PROVIDER (not the model) because whether
// cache_write_tokens is reported is an upstream trait: the same model
// served through another provider may report it.
//
// Guards, in order:
//   - flag off → untouched
//   - provider reported a real write count (CacheCreationTokensIn > 0)
//     → real data always wins, never overwritten
//   - nothing to reclassify (text+audio remainder <= 0) → untouched
//   - input-side context below the provider's cache minimum → untouched.
//     Upstreams only cache prompts at/above a minimum length (OpenAI
//     1024 tokens; Anthropic 1024/2048/4096 by model); a shorter request
//     writes NOTHING to cache, so estimating one would over-charge the
//     input at the cache-write rate. A non-positive threshold disables
//     this guard (always estimate).
//
// Deliberately NOT gated on CachedTokensIn > 0: a first turn (or an
// upstream that never reports cache reads) estimates the full input as
// a write, exactly matching what Anthropic bills on a cache-enabled
// first turn. Returns the adjusted usage and whether estimation fired.
func ApplyCacheWriteEstimate(acct *ent.UpstreamAccount, u TokenUsage) (TokenUsage, bool) {
	if acct == nil || !acct.EstimateCacheWrite {
		return u, false
	}
	if u.CacheCreationTokensIn > 0 || u.CacheCreation5mTokensIn > 0 || u.CacheCreation1hTokensIn > 0 {
		return u, false
	}
	// TokensIn is the text remainder after the audio split. A voice first
	// turn is almost all AudioTokensIn; leaving that behind would bill it
	// at the audio rate instead of the cache-write rate this flag exists
	// to reconstruct.
	remainder := u.TokensIn + u.AudioTokensIn
	if remainder <= 0 {
		return u, false
	}
	if min := int64(acct.CacheWriteMinContextTokens); min > 0 && u.InputContextTokens() < min {
		return u, false
	}
	u.CacheCreationTokensIn = remainder
	u.CacheCreation5mTokensIn = remainder
	u.CacheCreation1hTokensIn = 0
	u.TokensIn = 0
	u.AudioTokensIn = 0
	return u, true
}

// pickPriceTier returns the first tier whose up_to_tokens bound
// contains ctxTokens (bounds are inclusive; 0 = unbounded). Tiers are
// stored sorted ascending with any unbounded tier last — see
// NormalizePriceTiers. nil when no tier matches (flat pricing).
func pickPriceTier(tiers []entschema.PriceTier, ctxTokens int64) *entschema.PriceTier {
	for i := range tiers {
		if tiers[i].UpToTokens == 0 || ctxTokens <= tiers[i].UpToTokens {
			return &tiers[i]
		}
	}
	return nil
}

// CalcCost returns (base, final). For per_token mode:
//
//	base = (in × input_price + cached × cache_read_price + cache_write × cache_write_price + out × output_price
//	     + image_in × image_input_price + image_out × image_output_price) / 1000
//	     + output_images × output_image_price
//
// When the model has price_tiers, the tier matching the request's
// input-side context size overrides the base unit prices; a nil price
// inside the tier falls back to the base column of the same name.
//
// Cache prices fall back to the (tier-effective) input price when nil
// (so a misconfigured model never over- or under-charges silently).
// Image token prices fall back to the corresponding text token prices when
// nil, preserving legacy behavior where image-generation tool tokens were
// merged into ordinary input/output counts.
//
// output_image_price being nil means "no flat per-image surcharge" — image
// counts are still recorded in usage_log.output_images. Operators can set
// either token prices, per-image price, or both depending on the upstream's
// official billing model.
func (s *BillingSvc) CalcCost(m *ent.Model, group *ent.ModelGroup, u TokenUsage) (base, final decimal.Decimal, err error) {
	switch m.BillingMode {
	case entmodel.BillingModePerRequest:
		if m.PricePerRequest == nil {
			return decimal.Zero, decimal.Zero, fmt.Errorf("model %s: per_request mode requires price_per_request", m.Name)
		}
		base = *m.PricePerRequest
	case entmodel.BillingModePerToken:
		if m.InputUnitPrice == nil || m.OutputUnitPrice == nil {
			return decimal.Zero, decimal.Zero, fmt.Errorf("model %s: per_token mode requires input/output unit prices", m.Name)
		}
		thousand := decimal.NewFromInt(1000)
		perK := func(p *decimal.Decimal, n int) decimal.Decimal {
			if p == nil || n == 0 {
				return decimal.Zero
			}
			return p.Mul(decimal.NewFromInt(int64(n))).Div(thousand)
		}
		inputPrice := m.InputUnitPrice
		outputPrice := m.OutputUnitPrice
		cacheReadPrice := m.CacheReadInputPrice
		cacheWritePrice := m.CacheWriteInputPrice
		cacheWrite1hPrice := m.CacheWrite1hInputPrice
		imageInputPrice := m.ImageInputUnitPrice
		imageOutputPrice := m.ImageOutputUnitPrice
		audioInputPrice := m.AudioInputUnitPrice
		audioOutputPrice := m.AudioOutputUnitPrice
		if tier := pickPriceTier(m.PriceTiers, u.InputContextTokens()); tier != nil {
			if tier.InputUnitPrice != nil {
				inputPrice = tier.InputUnitPrice
			}
			if tier.OutputUnitPrice != nil {
				outputPrice = tier.OutputUnitPrice
			}
			if tier.CacheReadInputPrice != nil {
				cacheReadPrice = tier.CacheReadInputPrice
			}
			if tier.CacheWriteInputPrice != nil {
				cacheWritePrice = tier.CacheWriteInputPrice
			}
			if tier.CacheWrite1hInputPrice != nil {
				cacheWrite1hPrice = tier.CacheWrite1hInputPrice
			}
		}
		if cacheReadPrice == nil {
			cacheReadPrice = inputPrice
		}
		if cacheWritePrice == nil {
			cacheWritePrice = inputPrice
		}
		if cacheWrite1hPrice == nil {
			cacheWrite1hPrice = cacheWritePrice
		}
		if imageInputPrice == nil {
			imageInputPrice = inputPrice
		}
		if imageOutputPrice == nil {
			imageOutputPrice = outputPrice
		}
		if audioInputPrice == nil {
			audioInputPrice = inputPrice
		}
		if audioOutputPrice == nil {
			audioOutputPrice = outputPrice
		}
		cacheWrite5m, cacheWrite1h := u.CacheWriteSplit()
		base = perK(inputPrice, u.TokensIn).
			Add(perK(cacheReadPrice, u.CachedTokensIn)).
			Add(perK(cacheWritePrice, cacheWrite5m)).
			Add(perK(cacheWrite1hPrice, cacheWrite1h)).
			Add(perK(outputPrice, u.TokensOut)).
			Add(perK(imageInputPrice, u.ImageTokensIn)).
			Add(perK(imageOutputPrice, u.ImageTokensOut)).
			Add(perK(audioInputPrice, u.AudioTokensIn)).
			Add(perK(audioOutputPrice, u.AudioTokensOut))
	default:
		return decimal.Zero, decimal.Zero, fmt.Errorf("model %s: unknown billing_mode %s", m.Name, m.BillingMode)
	}
	// Image-output surcharge stacks on top of whichever base mode
	// fired above. Applied PRE rate-multiplier so the group rate
	// scales both text and image cost consistently.
	if u.OutputImages > 0 && m.OutputImagePrice != nil {
		base = base.Add(m.OutputImagePrice.Mul(decimal.NewFromInt(int64(u.OutputImages))))
	}
	// Duration surcharge (video/audio seconds) and character surcharge (TTS
	// input) stack the same way — additive on top of the base mode, PRE
	// rate-multiplier. Both default off (nil price / zero count), so chat
	// billing is unchanged.
	if u.OutputSeconds > 0 && m.OutputSecondPrice != nil {
		base = base.Add(m.OutputSecondPrice.Mul(decimal.NewFromInt(int64(u.OutputSeconds))))
	}
	if u.InputCharacters > 0 && m.InputCharacterPrice != nil {
		base = base.Add(m.InputCharacterPrice.Mul(decimal.NewFromInt(int64(u.InputCharacters))))
	}
	// Platform billing unit is 积分 (points), 1 point = 1 CNY. Model
	// prices are entered directly in points — NO currency conversion
	// happens here (m.Currency is a legacy column, not read by billing;
	// the exchange rate is reserved for provider settlement only).
	mult := decimal.NewFromFloat(group.RateMultiplier)
	final = base.Mul(mult)
	return base, final, nil
}

// PreCheck rejects requests up-front based on the group's billing_source.
// For plan_only it also enforces the plan's allowed_group_ids.
func (s *BillingSvc) ResolvePlanState(ctx context.Context, u *ent.User, apiKey *ent.ApiKey, group *ent.ModelGroup) (*PlanChargeState, error) {
	if apiKey != nil && apiKey.ID != uuid.Nil {
		if err := NewApiKeySvc(s.d).CheckCostLimit(ctx, apiKey, time.Now()); err != nil {
			return nil, err
		}
	}
	switch group.BillingSource {
	case entgroup.BillingSourceBalanceOnly:
		if apiKey != nil && apiKey.BillingScope == entapikey.BillingScopeOrganization {
			return nil, s.precheckOrganizationBalance(ctx, u, apiKey)
		}
		if u.Balance.Sign() <= 0 {
			return nil, ErrInsufficientBalance
		}
		return nil, nil
	case entgroup.BillingSourcePlanOnly:
		up, plan, err := s.plans.ActiveUserPlan(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		if !PlanAllowsGroup(plan, group.ID) {
			return nil, ErrPlanNotAllowed
		}
		if plan.Type == "pay_as_you_go" {
			if u.Balance.Sign() <= 0 {
				return nil, ErrInsufficientBalance
			}
			if plan.MonthlySpendLimit.Sign() > 0 {
				used, err := s.plans.PayAsYouGoUsageThisMonth(ctx, up)
				if err != nil {
					return nil, err
				}
				// Limit and usage are both in points (1 point = 1 CNY);
				// no currency conversion.
				if used.GreaterThanOrEqual(plan.MonthlySpendLimit) {
					return nil, ErrMonthlySpendLimitExceeded
				}
			}
			return &PlanChargeState{UserPlan: up, Plan: plan}, nil
		}
		// Best-effort early reject if any window is already at or beyond
		// its cap. Avoids the "free one over the line" hole on every
		// request once a user is over quota. The atomic check inside the
		// Lua still owns correctness — this is just a UX nicety.
		//
		// Plan quota counters are denominated in points (1 point = 1
		// CNY), same as usage logs and the balance ledger, so the
		// windows compare directly against plan.Quota* with no
		// currency conversion.
		if view, err := s.plans.PeekQuota(ctx, up); err == nil {
			if plan.Quota5h.Sign() > 0 && view.Used5h.GreaterThanOrEqual(plan.Quota5h) {
				return nil, ErrPlanQuotaExhausted
			}
			if plan.QuotaWeekly.Sign() > 0 && view.UsedWeekly.GreaterThanOrEqual(plan.QuotaWeekly) {
				return nil, ErrPlanQuotaExhausted
			}
			if plan.QuotaMonthly.Sign() > 0 && view.UsedMonthly.GreaterThanOrEqual(plan.QuotaMonthly) {
				return nil, ErrPlanQuotaExhausted
			}
		}
		return &PlanChargeState{UserPlan: up, Plan: plan}, nil
	default:
		return nil, ErrUnsupportedBillingSource
	}
}

// PrecheckBalance admits a balance-billed call that belongs to no model group
// (the web tools): the key's cost limit first, then the balance that pays for
// the key — the organization's for an organization key, the user's otherwise.
func (s *BillingSvc) PrecheckBalance(ctx context.Context, u *ent.User, apiKey *ent.ApiKey) error {
	if apiKey != nil && apiKey.ID != uuid.Nil {
		if err := NewApiKeySvc(s.d).CheckCostLimit(ctx, apiKey, time.Now()); err != nil {
			return err
		}
		if apiKey.BillingScope == entapikey.BillingScopeOrganization {
			return s.precheckOrganizationBalance(ctx, u, apiKey)
		}
	}
	if u.Balance.Sign() <= 0 {
		return ErrInsufficientBalance
	}
	return nil
}

// ChargeBalance deducts a flat amount from the balance that pays for apiKey,
// with the same row locking and ledger entry as a balance-billed model call.
func (s *BillingSvc) ChargeBalance(ctx context.Context, u *ent.User, apiKey *ent.ApiKey, amount decimal.Decimal, note string) (*ChargeResult, error) {
	if apiKey != nil && apiKey.BillingScope == entapikey.BillingScopeOrganization {
		return s.chargeOrganizationBalance(ctx, u.ID, apiKey, amount, amount, note)
	}
	return s.chargeBalance(ctx, u.ID, nil, entusagelog.BillingSourceBalance, amount, amount, note)
}

type ChargeResult struct {
	BillingSource  entusagelog.BillingSource
	UserPlanID     *uuid.UUID
	OrganizationID *uuid.UUID
	CostBase       decimal.Decimal
	CostFinal      decimal.Decimal
	Currency       string
}

func (s *BillingSvc) Charge(ctx context.Context, u *ent.User, apiKey *ent.ApiKey, group *ent.ModelGroup, base, final decimal.Decimal, state *PlanChargeState) (*ChargeResult, error) {
	switch group.BillingSource {
	case entgroup.BillingSourceBalanceOnly:
		if apiKey != nil && apiKey.BillingScope == entapikey.BillingScopeOrganization {
			return s.chargeOrganizationBalance(ctx, u.ID, apiKey, base, final, "llm gateway organization charge")
		}
		return s.chargeBalance(ctx, u.ID, nil, entusagelog.BillingSourceBalance, base, final, "llm gateway charge")
	case entgroup.BillingSourcePlanOnly:
		if state == nil || state.UserPlan == nil || state.Plan == nil {
			var err error
			state, err = s.ResolvePlanState(ctx, u, apiKey, group)
			if err != nil {
				return nil, err
			}
		}
		up := state.UserPlan
		plan := state.Plan
		if !PlanAllowsGroup(plan, group.ID) {
			return nil, ErrPlanNotAllowed
		}
		if plan.Type == "pay_as_you_go" {
			upID := up.ID
			return s.chargeBalance(ctx, u.ID, &upID, entusagelog.BillingSourcePlanBalance, base, final, "pay-as-you-go plan charge: "+plan.Name)
		}
		if _, err := s.plans.ConsumeQuota(ctx, up, plan, final); err != nil {
			return nil, err
		}
		upID := up.ID
		return &ChargeResult{
			BillingSource: entusagelog.BillingSourcePlan,
			UserPlanID:    &upID,
			CostBase:      base, CostFinal: final, Currency: CurrencyCNY,
		}, nil
	default:
		return nil, ErrUnsupportedBillingSource
	}
}

// chargeBalance is the direct (non-reserved) post-response charge. It
// deducts final in full; the balance may go negative. There is no cash
// shortfall error. Hot paths reserve up-front (see billing_reserve.go),
// which makes this a rare last resort rather than a business-as-usual event.
func (s *BillingSvc) chargeBalance(ctx context.Context, userID uuid.UUID, userPlanID *uuid.UUID, source entusagelog.BillingSource, base, final decimal.Decimal, note string) (*ChargeResult, error) {
	// Lock the user row, re-read balance, then deduct: the caller's
	// user snapshot is stale, so direct writes can lose concurrent topups.
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	locked, err := tx.User.Query().Where(entuser.IDEQ(userID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	charged := final
	newBalance := locked.Balance.Sub(charged)
	if _, err := tx.User.UpdateOneID(userID).SetBalance(newBalance).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if charged.Sign() > 0 {
		if _, err := tx.BalanceTx.Create().
			SetUserID(userID).
			SetKind("deduct").
			SetAmount(charged.Neg()).
			SetBalanceAfter(newBalance).
			SetCurrency(CurrencyCNY).
			SetNote(note).
			Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ChargeResult{
		BillingSource: source,
		UserPlanID:    userPlanID,
		CostBase:      base, CostFinal: charged, Currency: CurrencyCNY,
	}, nil
}

func (s *BillingSvc) precheckOrganizationBalance(ctx context.Context, u *ent.User, apiKey *ent.ApiKey) error {
	if apiKey.OrganizationID == nil {
		return ErrOrganizationForbidden
	}
	org, err := s.d.Ent.Organization.Get(ctx, *apiKey.OrganizationID)
	if err != nil {
		if ent.IsNotFound(err) {
			return ErrOrganizationNotFound
		}
		return err
	}
	if org.Balance.Sign() <= 0 {
		return ErrInsufficientBalance
	}
	member, err := s.d.Ent.OrganizationMember.Query().
		Where(entmember.OrganizationIDEQ(org.ID), entmember.UserIDEQ(u.ID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return ErrOrganizationForbidden
		}
		return err
	}
	orgUsed, err := organizationMonthSpend(ctx, s.d.DB(), org.ID)
	if err != nil {
		return err
	}
	if org.MonthlySpendLimit.Sign() > 0 && orgUsed.GreaterThanOrEqual(org.MonthlySpendLimit) {
		return ErrOrganizationLimit
	}
	memberMonth, memberTotal, err := organizationMemberSpend(ctx, s.d.DB(), org.ID, u.ID)
	if err != nil {
		return err
	}
	if member.MonthlySpendLimit.Sign() > 0 && memberMonth.GreaterThanOrEqual(member.MonthlySpendLimit) {
		return memberLimitErr("monthly spend limit reached")
	}
	if member.TotalSpendLimit.Sign() > 0 && memberTotal.GreaterThanOrEqual(member.TotalSpendLimit) {
		return memberLimitErr("total spend limit reached")
	}
	return nil
}

func (s *BillingSvc) chargeOrganizationBalance(ctx context.Context, userID uuid.UUID, apiKey *ent.ApiKey, base, final decimal.Decimal, note string) (*ChargeResult, error) {
	if apiKey == nil || apiKey.OrganizationID == nil {
		return nil, ErrOrganizationForbidden
	}
	orgID := *apiKey.OrganizationID
	// Raw *sql.Tx rather than an ent transaction: the limit checks are SQL
	// aggregates and must run inside the same transaction that holds the
	// organization row lock. Mirrors control.SQLLedger.ReleaseLease, which also
	// writes this table the same way.
	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ChargeResult, error) {
		_ = tx.Rollback()
		return nil, e
	}
	// The organization row lock is the serialization point for all org spend:
	// every writer of a deduct row for this org takes it, so the aggregates
	// below cannot race with a concurrent charge.
	var balance, orgMonthlyLimit decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT balance, monthly_spend_limit FROM organizations WHERE id=$1 FOR UPDATE`,
		orgID).Scan(&balance, &orgMonthlyLimit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(ErrOrganizationNotFound)
		}
		return rollback(err)
	}
	var memberMonthlyLimit, memberTotalLimit decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT monthly_spend_limit, total_spend_limit FROM organization_members WHERE organization_id=$1 AND user_id=$2`,
		orgID, userID).Scan(&memberMonthlyLimit, &memberTotalLimit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(ErrOrganizationForbidden)
		}
		return rollback(err)
	}
	// Limit overruns at direct-charge time no longer skip the deduction.
	// This function runs AFTER the response was delivered; refusing to
	// record the spend here is exactly the "output already produced, usage
	// counter unchanged, next request rides free" hole. Hot paths occupy
	// the limits up-front via Reserve; if a racing request still lands here
	// over the line, the spend is recorded (so the limit closes) and the
	// overrun is reported to the caller's usage log.
	var limitErr error
	orgUsed, err := organizationMonthSpend(ctx, tx, orgID)
	if err != nil {
		return rollback(err)
	}
	if orgMonthlyLimit.Sign() > 0 && orgUsed.Add(final).GreaterThan(orgMonthlyLimit) {
		limitErr = ErrOrganizationLimit
	}
	memberMonth, memberTotal, err := organizationMemberSpend(ctx, tx, orgID, userID)
	if err != nil {
		return rollback(err)
	}
	if limitErr == nil && memberMonthlyLimit.Sign() > 0 && memberMonth.Add(final).GreaterThan(memberMonthlyLimit) {
		limitErr = memberLimitErr("monthly spend limit reached")
	}
	if limitErr == nil && memberTotalLimit.Sign() > 0 && memberTotal.Add(final).GreaterThan(memberTotalLimit) {
		limitErr = memberLimitErr("total spend limit reached")
	}
	charged := final
	newBalance := balance.Sub(charged)
	if _, err := tx.ExecContext(ctx,
		`UPDATE organizations SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`,
		newBalance, orgID); err != nil {
		return rollback(err)
	}
	if charged.Sign() > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO organization_balance_txes
			  (id, created_at, updated_at, organization_id, actor_user_id, kind, amount, balance_after, currency, note)
			VALUES ($1,NOW(),NOW(),$2,$3,'deduct',$4,$5,'CNY',$6)`,
			uuid.New(), orgID, userID, charged.Neg(), newBalance, note); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ChargeResult{
		BillingSource:  entusagelog.BillingSourceOrganizationBalance,
		OrganizationID: &orgID,
		CostBase:       base,
		CostFinal:      charged,
		Currency:       CurrencyCNY,
	}, limitErr
}

// orgSpendQuerier is satisfied by both *sql.DB and *sql.Tx, so the spend
// helpers below can run either standalone (precheck) or inside the charge
// transaction that already holds the organization row lock.
type orgSpendQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// organizationMonthSpend sums what an organization has spent this UTC month.
//
// The month boundary is computed in Go and passed as a parameter rather than
// written as date_trunc('month', NOW()): NOW() is timestamptz and date_trunc
// on it resolves against the session TimeZone, so the same query would return
// different answers depending on connection settings. Organization and member
// limits are documented as UTC-month, so the boundary has to be pinned here.
func organizationMonthSpend(ctx context.Context, q orgSpendQuerier, orgID uuid.UUID) (decimal.Decimal, error) {
	var month decimal.Decimal
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(ABS(amount)) FILTER (WHERE created_at >= $2), 0)
		FROM organization_balance_txes
		WHERE organization_id=$1 AND kind='deduct'`,
		orgID, monthStartUTC()).Scan(&month)
	return month, err
}

// organizationMemberSpend returns one member's spend this UTC month and over
// all time, in a single index scan.
//
// Both numbers come from the organization ledger rather than usage_logs so
// they agree with the balance the organization actually sees — a request that
// errored before billing writes a usage_log row but never a deduct, and must
// not count against anyone's cap.
func organizationMemberSpend(ctx context.Context, q orgSpendQuerier, orgID, userID uuid.UUID) (month, total decimal.Decimal, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(ABS(amount)) FILTER (WHERE created_at >= $3), 0),
		  COALESCE(SUM(ABS(amount)), 0)
		FROM organization_balance_txes
		WHERE organization_id=$1 AND kind='deduct' AND actor_user_id=$2`,
		orgID, userID, monthStartUTC()).Scan(&month, &total)
	return month, total, err
}

// memberLimitErr distinguishes which member cap was hit while still matching
// errors.Is(err, ErrMemberLimit), so the six existing HTTP/availability
// mappings keep working untouched.
func memberLimitErr(which string) error {
	return fmt.Errorf("%w: %s", ErrMemberLimit, which)
}

type WriteUsageLogInput struct {
	// LogID is the usage_log row id, pre-assigned by normalizeUsageLogInput
	// when the caller leaves it zero. Pre-assigning (rather than letting ent
	// default it at insert) lets the consumption-rebate hook reference the
	// row before the batch writer has flushed it.
	LogID           uuid.UUID
	UserID          uuid.UUID
	ApiKeyID        *uuid.UUID
	AuthorizationID *uuid.UUID
	OrganizationID  *uuid.UUID
	RequestID       string
	Endpoint        entusagelog.Endpoint
	Model           string
	ModelID         *uuid.UUID
	GroupID         *uuid.UUID
	// ProviderID is the LEGACY channel attribution: pre-migration rows set it,
	// and a new row sets it only when its account carries a legacy_provider_id.
	// Reports must never group by it — group by ProviderAccountID (fusing the
	// legacy rows through the account's legacy_provider_id) and use this column
	// only to resolve a display name for history whose account is gone.
	ProviderID *uuid.UUID
	// ProviderAccountID is the primary attribution: the upstream ACCOUNT that
	// served the attempt/request. It is a plain analytics column (no FK).
	ProviderAccountID       *uuid.UUID
	UpstreamModel           string
	TokensIn                int
	TokensOut               int
	CachedTokensIn          int
	CacheCreationTokensIn   int
	CacheCreation5mTokensIn int
	CacheCreation1hTokensIn int
	ImageTokensIn           int
	ImageTokensOut          int
	// AudioTokens*: audio subset, already excluded from TokensIn/TokensOut.
	AudioTokensIn  int
	AudioTokensOut int
	// CacheWriteEstimated marks CacheCreationTokensIn as gateway-derived
	// (see ApplyCacheWriteEstimate) rather than upstream-reported.
	CacheWriteEstimated bool
	OutputImages        int
	CostBase            decimal.Decimal
	CostFinal           decimal.Decimal
	// RateMultiplierApplied is the effective multiplier that turned
	// CostBase into CostFinal — group.RateMultiplier × any dynamic
	// pool-throttle factor. Default 1.0 when writing a log row before
	// pricing (e.g. cost-calc error paths). Persisted to
	// usage_log.rate_multiplier_applied for post-hoc audits.
	RateMultiplierApplied float64
	Currency              string
	BillingSource         entusagelog.BillingSource
	UserPlanID            *uuid.UUID
	Status                entusagelog.Status
	HTTPStatus            int
	LatencyMs             int
	// TtftMs is time-to-first-token in milliseconds, set whenever the gateway
	// streamed the upstream response. That is not the same as Stream: the
	// Anthropic translation bridge streams upstream and buffers a single JSON
	// body for the client, so it reports a TTFT on a Stream=false row. nil
	// when nothing was streamed and there is no first token to time.
	TtftMs *int
	// TtfbMs is time-to-first-byte from request entry until this gateway
	// first writes or flushes the downstream response.
	TtfbMs        *int
	RetryCount    int
	ProviderChain []string
	LatencyDetail map[string]int
	ErrorMessage  string
	// ErrorClass / UpstreamRequestID / UpstreamRawError are admin-only
	// diagnostics. ErrorClass is the structured outcome label;
	// UpstreamRequestID is the provider's own request id; UpstreamRawError
	// is the upstream error body as the gateway read it. Only its
	// SanitizeUpstreamError summary is stored, and ErrorMessage goes
	// through SanitizeErrorMessage (error_redact.go).
	ErrorClass        string
	UpstreamRequestID string
	UpstreamRawError  string
	IP                string
	UserAgent         string
	ReasoningEffort   string
	Stream            bool
	ApiKeyName        string
	StartedAt         time.Time
	// Delivered marks a complete response that already reached the caller.
	// It keeps post-response billing/audit failures from being reported as a
	// service availability failure.
	Delivered bool
	// WebSearchRequests is the built-in web-search count reported by the
	// upstream. Persisted on usage_log.latency_detail["web_search_requests"].
	WebSearchRequests int
}

func (s *BillingSvc) WriteUsageLog(ctx context.Context, in WriteUsageLogInput) (*ent.UsageLog, error) {
	// A usage_log row must land even when the client has already disconnected
	// (mid-stream stall, client give-up) — otherwise exactly the abnormal
	// requests we most want to see vanish from the backend. Detach from the
	// request context (keep its values, drop its cancellation) and re-bound the
	// write with its own deadline so a slow DB can't leak the goroutine.
	in = normalizeUsageLogInput(in)

	// Batch path: hand the row to the writer goroutine and return. Everything
	// that depends on the request context must be resolved HERE — by flush
	// time the ctx is long gone.
	if s.usageWriter != nil {
		entry := usageLogEntry{log: in}
		if observation, ok := availabilityInputFromUsage(in); ok {
			normalized := normalizeAvailabilityInput(ctx, observation)
			entry.observation = &normalized
		}
		// There is deliberately no health bookkeeping here: account health is
		// owned by the account state machine (account_health.go) and its rolling
		// window is fed per attempt by AccountPoolSvc.RecordOutcome, then
		// recomputed into upstream_accounts.health by AccountPoolSvc.RefreshHealth
		// on the leader loop. The retired provider-level window (phealth:*) is
		// gone — it graded a channel that no longer exists as a routing entity.
		if !s.usageWriter.Submit(entry) {
			return nil, ErrUsageLogDropped
		}
		// Counters are bumped on accepted-for-write, not on flushed. A flush
		// is up to flush_max_age away, and the dashboard polls every 2s; the
		// alternative is a chart that visibly lags its own log view.
		s.incrUsageBucketsAsync(ctx, in)
		// No row is returned: the insert has not happened yet. Every caller
		// discards the returned *ent.UsageLog already.
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	row, err := createUsageLog(ctx, tx.Client(), in)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if observation, ok := availabilityInputFromUsage(in); ok {
		if err := createAvailabilityObservation(ctx, tx.Client(), observation); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Health bookkeeping is owned by the account state machine; see the note on
	// the batch path above.
	s.incrUsageBucketsAsync(ctx, in)
	return row, nil
}

// normalizeUsageLogInput fills the defaults that must be decided at call time
// rather than at persist time, so the synchronous and batched paths store
// identical rows.
func normalizeUsageLogInput(in WriteUsageLogInput) WriteUsageLogInput {
	if in.LogID == uuid.Nil {
		in.LogID = uuid.New()
	}
	if in.BillingSource == "" {
		in.BillingSource = entusagelog.BillingSourceNone
	}
	if in.RateMultiplierApplied <= 0 {
		in.RateMultiplierApplied = 1.0
	}
	// Materialize the 5m/1h split so new rows always store it. Aggregate-only
	// writes (no nested cache_creation) land entirely in 5m, matching billing.
	if in.CacheCreation5mTokensIn == 0 && in.CacheCreation1hTokensIn == 0 && in.CacheCreationTokensIn > 0 {
		in.CacheCreation5mTokensIn = in.CacheCreationTokensIn
	}
	if sum := in.CacheCreation5mTokensIn + in.CacheCreation1hTokensIn; sum > in.CacheCreationTokensIn {
		in.CacheCreationTokensIn = sum
	}
	return in
}

func createUsageLog(ctx context.Context, client *ent.Client, in WriteUsageLogInput) (*ent.UsageLog, error) {
	return usageLogCreateBuilder(client, in).Save(ctx)
}

// usageLogCreateBuilder is the single place that maps WriteUsageLogInput onto
// column setters. Both the one-row path (createUsageLog) and the batch writer
// (UsageLogWriter.flush, which feeds these into CreateBulk) go through it, so
// a new field can never be persisted by one path and silently dropped by the
// other.
func usageLogCreateBuilder(client *ent.Client, in WriteUsageLogInput) *ent.UsageLogCreate {
	q := client.UsageLog.Create().
		SetID(in.LogID).
		SetUserID(in.UserID).
		SetRequestID(in.RequestID).
		SetEndpoint(in.Endpoint).
		SetModel(in.Model).
		SetUpstreamModel(in.UpstreamModel).
		SetTokensIn(in.TokensIn).
		SetTokensOut(in.TokensOut).
		SetCachedTokensIn(in.CachedTokensIn).
		SetCacheCreationTokensIn(in.CacheCreationTokensIn).
		SetCacheCreation5mTokensIn(in.CacheCreation5mTokensIn).
		SetCacheCreation1hTokensIn(in.CacheCreation1hTokensIn).
		SetImageTokensIn(in.ImageTokensIn).
		SetImageTokensOut(in.ImageTokensOut).
		SetAudioTokensIn(in.AudioTokensIn).
		SetAudioTokensOut(in.AudioTokensOut).
		SetCacheWriteEstimated(in.CacheWriteEstimated).
		SetOutputImages(in.OutputImages).
		SetCostBase(in.CostBase).
		SetCostFinal(in.CostFinal).
		SetRateMultiplierApplied(in.RateMultiplierApplied).
		SetCurrency(in.Currency).
		SetBillingSource(in.BillingSource).
		SetStatus(in.Status).
		SetHTTPStatus(in.HTTPStatus).
		SetLatencyMs(in.LatencyMs).
		SetNillableTtftMs(in.TtftMs).
		SetNillableTtfbMs(in.TtfbMs).
		SetRetryCount(in.RetryCount).
		SetProviderChain(in.ProviderChain).
		SetErrorMessage(SanitizeErrorMessage(in.ErrorMessage)).
		SetErrorClass(in.ErrorClass).
		SetUpstreamRequestID(in.UpstreamRequestID).
		SetUpstreamRawError(SanitizeUpstreamError(in.UpstreamRawError)).
		SetIP(in.IP).
		SetUserAgent(in.UserAgent).
		SetReasoningEffort(in.ReasoningEffort).
		SetStream(in.Stream).
		SetAPIKeyName(in.ApiKeyName)
	if in.WebSearchRequests > 0 || in.LatencyDetail != nil {
		detail := in.LatencyDetail
		if in.WebSearchRequests > 0 {
			if detail == nil {
				detail = map[string]int{}
			} else {
				copied := make(map[string]int, len(detail)+1)
				for k, v := range detail {
					copied[k] = v
				}
				detail = copied
			}
			detail["web_search_requests"] = in.WebSearchRequests
		}
		if detail != nil {
			q = q.SetLatencyDetail(detail)
		}
	}
	if in.ApiKeyID != nil {
		q = q.SetAPIKeyID(*in.ApiKeyID)
	}
	if in.AuthorizationID != nil {
		q = q.SetAuthorizationID(*in.AuthorizationID)
	}
	if in.OrganizationID != nil {
		q = q.SetOrganizationID(*in.OrganizationID)
	}
	if in.GroupID != nil {
		q = q.SetGroupID(*in.GroupID)
	}
	if in.ProviderID != nil {
		q = q.SetProviderID(*in.ProviderID)
	}
	if in.ProviderAccountID != nil {
		q = q.SetProviderAccountID(*in.ProviderAccountID)
	}
	if in.UserPlanID != nil {
		q = q.SetUserPlanID(*in.UserPlanID)
	}
	return q
}
