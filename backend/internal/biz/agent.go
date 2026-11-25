package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entagentprofile "github.com/jucode/jucode/internal/data/ent/agentprofile"
	entagentrefund "github.com/jucode/jucode/internal/data/ent/agentrefund"
	entagentsettlement "github.com/jucode/jucode/internal/data/ent/agentsettlement"
	entagententry "github.com/jucode/jucode/internal/data/ent/agentsettlemententry"
	entplanquotasnapshot "github.com/jucode/jucode/internal/data/ent/planquotasnapshot"
	entredeem "github.com/jucode/jucode/internal/data/ent/redeemcode"
	entredeemrec "github.com/jucode/jucode/internal/data/ent/redeemrecord"
)

// AgentSvc owns the agent-side surfaces: profile bookkeeping, code
// generation, settlement-entry writes (sale + refund), and the settle
// transaction that closes a batch of entries.
//
// Two related but separate flows:
//   - Sale entry is written automatically when a code with agent_id is
//     redeemed (see RedeemSvc.recordAgentSale called from Redeem()).
//   - Refund entry is written here (RegisterRefund), alongside the
//     compensating user-side action (drop user_plan / nothing for
//     balance refund — refunding the user is handled out-of-band by
//     the agent, the platform only adjusts settlement bookkeeping per
//     the user's plan).
type AgentSvc struct {
	d        *data.Data
	settings *SettingsSvc
}

func NewAgentSvc(d *data.Data, settings *SettingsSvc) *AgentSvc {
	return &AgentSvc{d: d, settings: settings}
}

// agentRefundTerms are the site's refund settings in the form the refund
// math uses (see SiteSettings.AgentPlanRefundDayDivisor).
type agentRefundTerms struct {
	planDayDivisor decimal.Decimal
	planRate       decimal.Decimal // fraction, e.g. 0.7
	balanceRate    decimal.Decimal // fraction, e.g. 0.9
}

func (s *AgentSvc) refundTerms(ctx context.Context) (agentRefundTerms, error) {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return agentRefundTerms{}, err
	}
	divisor := cfg.AgentPlanRefundDayDivisor
	if divisor < 1 {
		divisor = siteDefaults.AgentPlanRefundDayDivisor
	}
	return agentRefundTerms{
		planDayDivisor: decimal.NewFromInt(int64(divisor)),
		planRate:       decimal.NewFromFloat(cfg.AgentPlanRefundRatePct).Div(hundred),
		balanceRate:    decimal.NewFromFloat(cfg.AgentBalanceRefundRatePct).Div(hundred),
	}, nil
}

// planRefund is the prorated, rate-reduced refund of a plan with
// remainingDays left.
func (t agentRefundTerms) planRefund(price decimal.Decimal, remainingDays float64) decimal.Decimal {
	return price.
		Mul(decimal.NewFromFloat(remainingDays)).
		Div(t.planDayDivisor).
		Mul(t.planRate).
		Round(8)
}

// Errors.
var (
	ErrAgentRefundAlreadyDone  = errors.New("redeem record already refunded")
	ErrAgentRefundUserPlanGone = errors.New("user_plan no longer exists")
	ErrAgentNotEligible        = errors.New("user is not an agent")
	ErrRefundCodeMismatch      = errors.New("redeem code does not belong to this agent")
	ErrAgentSuspended          = errors.New("agent is suspended")
)

var hundred = decimal.NewFromInt(100)

// maxIncentive is the site-wide cap on an agent's incentive budget.
func (s *AgentSvc) maxIncentive(ctx context.Context) (decimal.Decimal, error) {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromFloat(cfg.MaxIncentivePct), nil
}

// effectiveSharePct is the platform share actually applied to a sale: the
// profile's share, raised to 100 - maxIncentive when the site cap has been
// lowered below the budget the profile was configured with.
func effectiveSharePct(share, maxIncentive decimal.Decimal) decimal.Decimal {
	return decimal.Max(share, hundred.Sub(maxIncentive))
}

// EffectiveShare is the platform share new sales are booked at for p.
func (s *AgentSvc) EffectiveShare(ctx context.Context, p *ent.AgentProfile) (decimal.Decimal, error) {
	maxIncentive, err := s.maxIncentive(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	return effectiveSharePct(p.PlatformSharePct, maxIncentive), nil
}

// EnsureProfile fetches the agent's profile, creating a default one (whose
// budget is the full max_incentive_pct) if missing. Idempotent.
func (s *AgentSvc) EnsureProfile(ctx context.Context, userID uuid.UUID) (*ent.AgentProfile, error) {
	p, err := s.d.Ent.AgentProfile.Query().
		Where(entagentprofile.UserIDEQ(userID)).
		Only(ctx)
	if err == nil {
		return p, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	maxIncentive, err := s.maxIncentive(ctx)
	if err != nil {
		return nil, err
	}
	created, err := s.d.Ent.AgentProfile.Create().
		SetUserID(userID).
		SetPlatformSharePct(hundred.Sub(maxIncentive)).
		Save(ctx)
	if err != nil {
		// Race against another goroutine — return the existing row.
		if ent.IsConstraintError(err) {
			return s.d.Ent.AgentProfile.Query().
				Where(entagentprofile.UserIDEQ(userID)).
				Only(ctx)
		}
		return nil, err
	}
	return created, nil
}

// UpdateSharePct overrides an agent's platform share. Admin-only — the
// caller layer enforces that. The resulting budget (100 - share) must not
// exceed max_incentive_pct; a coupon discount larger than the new budget is
// lowered to it.
func (s *AgentSvc) UpdateSharePct(ctx context.Context, agentID uuid.UUID, share decimal.Decimal) (*ent.AgentProfile, error) {
	maxIncentive, err := s.maxIncentive(ctx)
	if err != nil {
		return nil, err
	}
	if share.GreaterThan(hundred) || share.LessThan(hundred.Sub(maxIncentive)) {
		return nil, fmt.Errorf("share must be in [%s, 100] (max incentive %s%%)", hundred.Sub(maxIncentive), maxIncentive)
	}
	p, err := s.EnsureProfile(ctx, agentID)
	if err != nil {
		return nil, err
	}
	q := s.d.Ent.AgentProfile.Update().
		Where(entagentprofile.UserIDEQ(agentID)).
		SetPlatformSharePct(share)
	if budget := hundred.Sub(share); p.CouponDiscountPct.GreaterThan(budget) {
		q = q.SetCouponDiscountPct(budget)
	}
	if _, err := q.Save(ctx); err != nil {
		return nil, err
	}
	return s.d.Ent.AgentProfile.Query().
		Where(entagentprofile.UserIDEQ(agentID)).
		Only(ctx)
}

// SetStatus toggles active/suspended on a profile. A suspended agent
// can still see their dashboard but new code generation + refund
// registration are blocked.
func (s *AgentSvc) SetStatus(ctx context.Context, agentID uuid.UUID, status string) (*ent.AgentProfile, error) {
	switch status {
	case "active", "suspended":
	default:
		return nil, errors.New("status must be active|suspended")
	}
	if _, err := s.EnsureProfile(ctx, agentID); err != nil {
		return nil, err
	}
	_, err := s.d.Ent.AgentProfile.Update().
		Where(entagentprofile.UserIDEQ(agentID)).
		SetStatus(entagentprofile.Status(status)).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return s.d.Ent.AgentProfile.Query().
		Where(entagentprofile.UserIDEQ(agentID)).
		Only(ctx)
}

// RecordSale is called from RedeemSvc.Redeem inside the redemption tx.
// Returns the entry (mostly for testing — callers ignore it).
//
// `tx` is the in-flight ent.Tx so we don't break atomicity. saleAmount
// is positive (the agent's gross take from this redemption).
func (s *AgentSvc) RecordSale(ctx context.Context, tx *ent.Tx, agentID, codeID, recordID, userID uuid.UUID, saleAmount decimal.Decimal, currency string) error {
	prof, err := s.profileWithinTx(ctx, tx, agentID)
	if err != nil {
		return fmt.Errorf("agent profile: %w", err)
	}
	share, err := s.EffectiveShare(ctx, prof)
	if err != nil {
		return err
	}
	payable := saleAmount.Mul(share).Div(hundred)
	_, err = tx.AgentSettlementEntry.Create().
		SetAgentID(agentID).
		SetKind(entagententry.KindSale).
		SetSaleAmount(saleAmount).
		SetPlatformPayable(payable).
		SetSharePctAtEvent(share).
		SetCurrency(currency).
		SetSourceType(entagententry.SourceTypeRedeemRecord).
		SetSourceID(recordID).
		SetRedeemCodeID(codeID).
		SetUserID(userID).
		Save(ctx)
	return err
}

func (s *AgentSvc) profileWithinTx(ctx context.Context, tx *ent.Tx, userID uuid.UUID) (*ent.AgentProfile, error) {
	p, err := tx.AgentProfile.Query().
		Where(entagentprofile.UserIDEQ(userID)).
		Only(ctx)
	if err == nil {
		return p, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	maxIncentive, err := s.maxIncentive(ctx)
	if err != nil {
		return nil, err
	}
	return tx.AgentProfile.Create().
		SetUserID(userID).
		SetPlatformSharePct(hundred.Sub(maxIncentive)).
		Save(ctx)
}

// RefundResult is what RegisterRefund returns to the API layer.
type RefundResult struct {
	RefundID        uuid.UUID
	Kind            string
	RefundAmount    decimal.Decimal
	PlatformDeduct  decimal.Decimal
	UserPlanRemoved bool
}

// RegisterRefund is the agent-driven refund endpoint. Validates that
// the redeem record was generated through this agent, computes the
// refund amount per policy, applies the user-side compensation (delete
// user_plan for kind=plan; no user-side action for kind=balance — the
// agent pays out-of-band), and writes the negative settlement entry.
//
// The refund math (terms from site settings, see agentRefundTerms):
//   - plan refund amount = plan.price × remaining_days /
//     agent_plan_refund_day_divisor × agent_plan_refund_rate_pct / 100
//   - balance refund amount = redeem_record.amount ×
//     agent_balance_refund_rate_pct / 100
//   - platform_deduct (always negative) = -refund_amount × share / 100
//
// All side effects share one transaction.
func (s *AgentSvc) RegisterRefund(ctx context.Context, agentID, recordID uuid.UUID, reason string) (*RefundResult, error) {
	prof, err := s.EnsureProfile(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if prof.Status == entagentprofile.StatusSuspended {
		return nil, ErrAgentSuspended
	}
	terms, err := s.refundTerms(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func() { _ = tx.Rollback() }

	// Pre-check: no double refund for the same record.
	dup, err := tx.AgentRefund.Query().
		Where(entagentrefund.RedeemRecordIDEQ(recordID)).
		Exist(ctx)
	if err != nil {
		rollback()
		return nil, err
	}
	if dup {
		rollback()
		return nil, ErrAgentRefundAlreadyDone
	}

	rec, err := tx.RedeemRecord.Get(ctx, recordID)
	if err != nil {
		rollback()
		if ent.IsNotFound(err) {
			return nil, ErrRedeemCodeNotFound
		}
		return nil, err
	}
	code, err := tx.RedeemCode.Get(ctx, rec.CodeID)
	if err != nil {
		rollback()
		return nil, err
	}
	if code.AgentID == nil || *code.AgentID != agentID {
		rollback()
		return nil, ErrRefundCodeMismatch
	}

	share, err := saleSharePct(ctx, tx.Client(), rec.ID)
	if err != nil {
		rollback()
		return nil, err
	}
	result := &RefundResult{Kind: string(rec.Kind)}

	switch rec.Kind {
	case entredeemrec.KindBalance:
		if rec.Amount == nil {
			rollback()
			return nil, ErrRedeemKindMismatch
		}
		// Refund the user based on the original face value.
		result.RefundAmount = rec.Amount.Mul(terms.balanceRate).Round(8)
	case entredeemrec.KindPlan:
		if rec.UserPlanID == nil {
			rollback()
			return nil, ErrRedeemKindMismatch
		}
		up, err := tx.UserPlan.Get(ctx, *rec.UserPlanID)
		if err != nil {
			rollback()
			if ent.IsNotFound(err) {
				return nil, ErrAgentRefundUserPlanGone
			}
			return nil, err
		}
		plan, err := tx.Plan.Get(ctx, up.PlanID)
		if err != nil {
			rollback()
			return nil, err
		}
		remaining := time.Until(up.ExpireAt).Hours() / 24
		if remaining < 0 {
			remaining = 0
		}
		// plan.Price is already in points (1 point = 1 CNY) — refund
		// math runs directly on it, no exchange rate.
		result.RefundAmount = terms.planRefund(plan.Price, remaining)
		// Drop snapshots first (FK) then the user_plan itself. We
		// intentionally do NOT touch usage_logs / balance_txes — they
		// keep their bare user_plan_id like the admin HardDeletePlan
		// path does (see plan.go for rationale).
		if _, err := tx.PlanQuotaSnapshot.Delete().
			Where(entplanquotasnapshot.UserPlanIDEQ(up.ID)).
			Exec(ctx); err != nil {
			rollback()
			return nil, err
		}
		if err := tx.UserPlan.DeleteOneID(up.ID).Exec(ctx); err != nil {
			rollback()
			return nil, err
		}
		result.UserPlanRemoved = true
	default:
		rollback()
		return nil, ErrRedeemKindMismatch
	}

	// platform_deduct is always non-positive: refunds shrink what the
	// agent owes us. Round to 8 places to match the schema scale.
	deduct := result.RefundAmount.
		Mul(share).
		Div(hundred).
		Neg().
		Round(8)
	result.PlatformDeduct = deduct

	refundRow, err := tx.AgentRefund.Create().
		SetAgentID(agentID).
		SetRedeemCodeID(code.ID).
		SetRedeemRecordID(rec.ID).
		SetUserID(rec.UserID).
		SetKind(entagentrefund.Kind(string(rec.Kind))).
		SetRefundAmount(result.RefundAmount).
		SetPlatformDeduct(result.PlatformDeduct).
		SetCurrency(CurrencyCNY).
		SetNillableUserPlanID(rec.UserPlanID).
		SetReason(reason).
		Save(ctx)
	if err != nil {
		rollback()
		return nil, err
	}
	result.RefundID = refundRow.ID

	// Companion settlement entry. sale_amount mirrors the negative
	// refund so net revenue (Σ sale_amount) matches the gross sales
	// minus refunds. platform_payable is the deduct (already negative).
	if _, err := tx.AgentSettlementEntry.Create().
		SetAgentID(agentID).
		SetKind(entagententry.KindRefund).
		SetSaleAmount(result.RefundAmount.Neg()).
		SetPlatformPayable(result.PlatformDeduct).
		SetSharePctAtEvent(share).
		SetCurrency(CurrencyCNY).
		SetSourceType(entagententry.SourceTypeAgentRefund).
		SetSourceID(refundRow.ID).
		SetRedeemCodeID(code.ID).
		SetUserID(rec.UserID).
		Save(ctx); err != nil {
		rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// saleSharePct is the platform share the redemption was sold at. Refunds
// reverse at that rate, not the profile's current one: a share or site
// cap changed since the sale must not refund more than was collected.
func saleSharePct(ctx context.Context, client *ent.Client, recordID uuid.UUID) (decimal.Decimal, error) {
	sale, err := client.AgentSettlementEntry.Query().
		Where(
			entagententry.SourceTypeEQ(entagententry.SourceTypeRedeemRecord),
			entagententry.SourceIDEQ(recordID),
		).
		Only(ctx)
	if err != nil {
		return decimal.Zero, fmt.Errorf("agent sale entry: %w", err)
	}
	return sale.SharePctAtEvent, nil
}

// --- listing ---

type AgentEntryFilter struct {
	AgentID uuid.UUID
	Kind    string // "sale" | "refund" | "coupon_rebate" | "coupon_refund" | "" (any)
	Status  string // "pending" | "settled" | "" (any)
	Limit   int
	Offset  int
}

type AgentEntryListResult struct {
	Entries []*ent.AgentSettlementEntry
	Total   int
}

func (s *AgentSvc) ListEntries(ctx context.Context, f AgentEntryFilter) (*AgentEntryListResult, error) {
	q := s.d.Ent.AgentSettlementEntry.Query().
		Where(entagententry.AgentIDEQ(f.AgentID))
	if f.Kind != "" && entagententry.KindValidator(entagententry.Kind(f.Kind)) == nil {
		q = q.Where(entagententry.KindEQ(entagententry.Kind(f.Kind)))
	}
	switch f.Status {
	case "pending":
		q = q.Where(entagententry.SettlementIDIsNil())
	case "settled":
		q = q.Where(entagententry.SettlementIDNotNil())
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Order(ent.Desc(entagententry.FieldCreatedAt)).
		Limit(f.Limit).Offset(f.Offset).All(ctx)
	if err != nil {
		return nil, err
	}
	return &AgentEntryListResult{Entries: rows, Total: total}, nil
}

// PendingSummary returns the aggregate platform_payable across every
// open (settlement_id NULL) entry for the agent, and the count of
// entries. Returned with zero values when the agent has no pending rows.
// TotalPayable is net: redeem-code sales the agent owes minus coupon
// rebates the platform owes, so it goes negative when rebates dominate.
type PendingSummary struct {
	TotalPayable decimal.Decimal
	EntriesCount int
	Sales        decimal.Decimal
	Refunds      decimal.Decimal
	// CouponRebates is the net coupon rebate owed to the agent (rebates
	// minus refund reversals), as a positive figure.
	CouponRebates decimal.Decimal
	// Withdrawn is commission already moved into the agent's balance.
	Withdrawn decimal.Decimal
}

func (s *AgentSvc) PendingSummary(ctx context.Context, agentID uuid.UUID) (*PendingSummary, error) {
	rows, err := s.d.Ent.AgentSettlementEntry.Query().
		Where(entagententry.AgentIDEQ(agentID), entagententry.SettlementIDIsNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	sum := &PendingSummary{}
	for _, r := range rows {
		sum.TotalPayable = sum.TotalPayable.Add(r.PlatformPayable)
		switch r.Kind {
		case entagententry.KindSale:
			sum.Sales = sum.Sales.Add(r.SaleAmount)
		case entagententry.KindRefund:
			sum.Refunds = sum.Refunds.Add(r.SaleAmount.Neg())
		case entagententry.KindCouponRebate, entagententry.KindCouponRefund:
			sum.CouponRebates = sum.CouponRebates.Sub(r.PlatformPayable)
		case entagententry.KindBalanceWithdraw:
			sum.Withdrawn = sum.Withdrawn.Add(r.PlatformPayable)
		}
	}
	sum.EntriesCount = len(rows)
	return sum, nil
}

// Settle bundles every pending entry for the agent into a new
// AgentSettlement. settledBy = the admin user id. Returns the newly
// created settlement plus the entries it contains.
func (s *AgentSvc) Settle(ctx context.Context, agentID, settledBy uuid.UUID, note string) (*ent.AgentSettlement, int, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, 0, err
	}
	rollback := func() { _ = tx.Rollback() }

	// Same lock WithdrawToBalance takes, so a withdrawal never computes its
	// limit from entries this settlement is closing.
	if _, err := tx.AgentProfile.Query().
		Where(entagentprofile.UserIDEQ(agentID)).
		ForUpdate().
		Only(ctx); err != nil && !ent.IsNotFound(err) {
		rollback()
		return nil, 0, err
	}
	entries, err := tx.AgentSettlementEntry.Query().
		Where(entagententry.AgentIDEQ(agentID), entagententry.SettlementIDIsNil()).
		All(ctx)
	if err != nil {
		rollback()
		return nil, 0, err
	}
	if len(entries) == 0 {
		rollback()
		return nil, 0, errors.New("no pending entries to settle")
	}
	total := decimal.Zero
	currency := entries[0].Currency
	for _, e := range entries {
		total = total.Add(e.PlatformPayable)
	}
	settlement, err := tx.AgentSettlement.Create().
		SetAgentID(agentID).
		SetTotalPayable(total).
		SetCurrency(currency).
		SetEntriesCount(len(entries)).
		SetSettledByUserID(settledBy).
		SetNote(note).
		Save(ctx)
	if err != nil {
		rollback()
		return nil, 0, err
	}
	// Stamp settlement_id onto every entry we just bundled. Done in
	// one UPDATE … WHERE id IN (…) for speed.
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	if _, err := tx.AgentSettlementEntry.Update().
		Where(entagententry.IDIn(ids...)).
		SetSettlementID(settlement.ID).
		Save(ctx); err != nil {
		rollback()
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return settlement, len(entries), nil
}

// ListSettlements returns historical settlements for the agent, newest
// first.
func (s *AgentSvc) ListSettlements(ctx context.Context, agentID uuid.UUID, limit, offset int) ([]*ent.AgentSettlement, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := s.d.Ent.AgentSettlement.Query().
		Where(entagentsettlement.AgentIDEQ(agentID))
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Order(ent.Desc(entagentsettlement.FieldCreatedAt)).
		Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// ListRefunds returns refund rows for the agent, newest first.
func (s *AgentSvc) ListRefunds(ctx context.Context, agentID uuid.UUID, limit, offset int) ([]*ent.AgentRefund, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := s.d.Ent.AgentRefund.Query().
		Where(entagentrefund.AgentIDEQ(agentID))
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Order(ent.Desc(entagentrefund.FieldCreatedAt)).
		Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// LookupCode is what the agent's "register refund" form calls to fetch
// the redeem code, the most recent record by this code, and a preview
// of the computed refund amounts without actually performing the
// refund. Returns ErrRefundCodeMismatch if the code wasn't created by
// this agent.
type RefundPreview struct {
	Code            *ent.RedeemCode
	Record          *ent.RedeemRecord
	UserPlan        *ent.UserPlan
	Plan            *ent.Plan
	RefundAmount    decimal.Decimal
	PlatformDeduct  decimal.Decimal
	AlreadyRefunded bool
	RemainingDays   float64 // valid only for kind=plan
}

func (s *AgentSvc) PreviewRefund(ctx context.Context, agentID uuid.UUID, code string) (*RefundPreview, error) {
	rc, err := s.d.Ent.RedeemCode.Query().
		Where(entredeem.CodeEQ(code)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrRedeemCodeNotFound
		}
		return nil, err
	}
	if rc.AgentID == nil || *rc.AgentID != agentID {
		return nil, ErrRefundCodeMismatch
	}
	// Pick the most recent record on this code (in practice MaxUses=1
	// for agent-issued codes, so this is just "the one record").
	rec, err := s.d.Ent.RedeemRecord.Query().
		Where(entredeemrec.CodeIDEQ(rc.ID)).
		Order(ent.Desc(entredeemrec.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, errors.New("code has not been redeemed yet")
		}
		return nil, err
	}
	previewed, err := s.d.Ent.AgentRefund.Query().
		Where(entagentrefund.RedeemRecordIDEQ(rec.ID)).
		Exist(ctx)
	if err != nil {
		return nil, err
	}
	out := &RefundPreview{Code: rc, Record: rec, AlreadyRefunded: previewed}
	terms, err := s.refundTerms(ctx)
	if err != nil {
		return nil, err
	}

	share, err := saleSharePct(ctx, s.d.Ent, rec.ID)
	if err != nil {
		return nil, err
	}
	switch rec.Kind {
	case entredeemrec.KindBalance:
		if rec.Amount != nil {
			out.RefundAmount = rec.Amount.Mul(terms.balanceRate).Round(8)
		}
	case entredeemrec.KindPlan:
		if rec.UserPlanID != nil {
			up, err := s.d.Ent.UserPlan.Get(ctx, *rec.UserPlanID)
			if err == nil {
				out.UserPlan = up
				if plan, err := s.d.Ent.Plan.Get(ctx, up.PlanID); err == nil {
					out.Plan = plan
					remaining := time.Until(up.ExpireAt).Hours() / 24
					if remaining < 0 {
						remaining = 0
					}
					out.RemainingDays = remaining
					// plan.Price is already in points (1 point = 1 CNY);
					// no exchange rate on the refund path.
					out.RefundAmount = terms.planRefund(plan.Price, remaining)
				}
			}
		}
	}
	out.PlatformDeduct = out.RefundAmount.
		Mul(share).
		Div(hundred).
		Neg().
		Round(8)
	return out, nil
}

// ListAllProfiles returns every agent profile (used by admin /agents).
func (s *AgentSvc) ListAllProfiles(ctx context.Context) ([]*ent.AgentProfile, error) {
	return s.d.Ent.AgentProfile.Query().
		Order(ent.Desc(entagentprofile.FieldCreatedAt)).
		All(ctx)
}
