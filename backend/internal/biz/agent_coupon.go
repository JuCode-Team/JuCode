package biz

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entagentprofile "github.com/jucode/jucode/internal/data/ent/agentprofile"
	entagententry "github.com/jucode/jucode/internal/data/ent/agentsettlemententry"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entusergrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergrant"
	entuggrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergroupgrant"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
)

// The agent coupon is the second channel an agent sells through: a reusable
// C-XXXXXXXX code that buyers enter on a site order. The platform collects
// the money; the agent earns a rebate that lands in the same settlement
// ledger as redeem-code sales (as a negative platform_payable), so one
// settlement nets both directions.
//
// Budget: the agent's incentive budget R = 100 - effective platform share.
// The buyer gets coupon_discount_pct (capped at R) off, and the agent earns
// the remaining R - discount percent of the paid amount.

var (
	ErrCouponCodeMalformed = errors.New("coupon code is malformed")
	ErrCouponCodeUnknown   = errors.New("coupon does not exist")
	ErrCouponCodeExists    = errors.New("agent already has a coupon")
	ErrCouponSelfUse       = errors.New("cannot use own coupon")
	ErrCouponsPaused       = errors.New("coupons are paused")
	// Exclusive groups already carry a negotiated low rate; a coupon on
	// top would give the margin away twice.
	ErrCouponExclusiveBuyer = errors.New("members of an exclusive group cannot use coupons")
)

const couponCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func normalizeCouponCode(raw string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) < 5 || len(code) > 32 || !strings.HasPrefix(code, "C-") {
		return "", ErrCouponCodeMalformed
	}
	for i, r := range code {
		if i < 2 {
			continue
		}
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return "", ErrCouponCodeMalformed
		}
	}
	if strings.HasSuffix(code, "-") {
		return "", ErrCouponCodeMalformed
	}
	return code, nil
}

func randomCouponCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = couponCodeAlphabet[int(b[i])%len(couponCodeAlphabet)]
	}
	return "C-" + string(b), nil
}

// CreateCoupon creates the agent's single permanent coupon. An empty raw
// value generates a C-XXXXXXXX code; custom codes use the same prefix.
func (s *AgentSvc) CreateCoupon(ctx context.Context, agentID uuid.UUID, raw string) (*ent.AgentProfile, error) {
	p, err := s.EnsureProfile(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if p.Status != entagentprofile.StatusActive {
		return nil, ErrAgentSuspended
	}
	raw = strings.TrimSpace(raw)
	if p.CouponCode != nil {
		if raw == "" || strings.EqualFold(*p.CouponCode, raw) {
			return p, nil
		}
		return nil, ErrCouponCodeExists
	}
	if raw != "" {
		code, err := normalizeCouponCode(raw)
		if err != nil {
			return nil, err
		}
		return s.setCouponCode(ctx, p.ID, code)
	}
	for range 10 {
		code, err := randomCouponCode()
		if err != nil {
			return nil, err
		}
		p, err := s.setCouponCode(ctx, p.ID, code)
		if errors.Is(err, ErrCouponCodeExists) {
			continue
		}
		return p, err
	}
	return nil, errors.New("failed to generate unique coupon code")
}

// setCouponCode writes the code only while the profile has none, so two
// concurrent creates cannot overwrite each other. ErrCouponCodeExists covers
// both "another agent owns this code" and "this agent already set one".
func (s *AgentSvc) setCouponCode(ctx context.Context, profileID uuid.UUID, code string) (*ent.AgentProfile, error) {
	n, err := s.d.Ent.AgentProfile.Update().
		Where(entagentprofile.IDEQ(profileID), entagentprofile.CouponCodeIsNil()).
		SetCouponCode(code).
		Save(ctx)
	if ent.IsConstraintError(err) {
		return nil, ErrCouponCodeExists
	}
	if err != nil {
		return nil, err
	}
	p, err := s.d.Ent.AgentProfile.Get(ctx, profileID)
	if err != nil {
		return nil, err
	}
	if n == 0 && (p.CouponCode == nil || *p.CouponCode != code) {
		return nil, ErrCouponCodeExists
	}
	return p, nil
}

// SetCouponDiscount sets the buyer discount the agent's coupon grants. It
// must fit inside the agent's current budget; whatever is left of the
// budget becomes the agent's rebate.
func (s *AgentSvc) SetCouponDiscount(ctx context.Context, agentID uuid.UUID, pct decimal.Decimal) (*ent.AgentProfile, error) {
	p, err := s.EnsureProfile(ctx, agentID)
	if err != nil {
		return nil, err
	}
	share, err := s.EffectiveShare(ctx, p)
	if err != nil {
		return nil, err
	}
	budget := hundred.Sub(share)
	if pct.IsNegative() || pct.GreaterThan(budget) {
		return nil, fmt.Errorf("discount_pct must be within 0-%s", budget)
	}
	if !pct.Equal(pct.Truncate(2)) {
		return nil, errors.New("discount_pct must have at most 2 decimal places")
	}
	return s.d.Ent.AgentProfile.UpdateOneID(p.ID).SetCouponDiscountPct(pct).Save(ctx)
}

// CouponTerms is the split a coupon applies right now: DiscountPct off for
// the buyer, RebatePct of the paid amount to the agent.
type CouponTerms struct {
	Code        string
	AgentID     uuid.UUID
	DiscountPct decimal.Decimal
	RebatePct   decimal.Decimal
}

// CouponTermsFor derives a profile's current coupon split under the site cap.
func (s *AgentSvc) CouponTermsFor(ctx context.Context, p *ent.AgentProfile) (CouponTerms, error) {
	share, err := s.EffectiveShare(ctx, p)
	if err != nil {
		return CouponTerms{}, err
	}
	budget := hundred.Sub(share)
	discount := decimal.Min(p.CouponDiscountPct, budget)
	out := CouponTerms{AgentID: p.UserID, DiscountPct: discount, RebatePct: budget.Sub(discount)}
	if p.CouponCode != nil {
		out.Code = *p.CouponCode
	}
	return out, nil
}

// QuoteCoupon resolves a buyer-entered code to the terms it would apply.
// Only active agents' coupons resolve; an agent cannot use their own.
func (s *AgentSvc) QuoteCoupon(ctx context.Context, buyerID uuid.UUID, raw string) (CouponTerms, error) {
	code, err := normalizeCouponCode(raw)
	if err != nil {
		return CouponTerms{}, err
	}
	st, err := s.settings.GetSite(ctx)
	if err != nil {
		return CouponTerms{}, err
	}
	if !st.AgentCouponsEnabled {
		return CouponTerms{}, ErrCouponsPaused
	}
	p, err := s.d.Ent.AgentProfile.Query().
		Where(entagentprofile.CouponCodeEQ(code), entagentprofile.StatusEQ(entagentprofile.StatusActive)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return CouponTerms{}, ErrCouponCodeUnknown
	}
	if err != nil {
		return CouponTerms{}, err
	}
	if p.UserID == buyerID {
		return CouponTerms{}, ErrCouponSelfUse
	}
	exclusive, err := hasExclusiveGroup(ctx, s.d.Ent, buyerID)
	if err != nil {
		return CouponTerms{}, err
	}
	if exclusive {
		return CouponTerms{}, ErrCouponExclusiveBuyer
	}
	return s.CouponTermsFor(ctx, p)
}

// hasExclusiveGroup reports whether the user may enter any enabled
// exclusive model group, by personal grant or through their user group.
// Such users already pay a negotiated rate, so neither agent coupons nor
// invite topup rebates apply to their purchases.
func hasExclusiveGroup(ctx context.Context, client *ent.Client, userID uuid.UUID) (bool, error) {
	u, err := client.User.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	granted := entgroup.HasUserGrantsWith(entusergrant.UserIDEQ(userID))
	if u.UserGroupID != nil {
		granted = entgroup.Or(granted, entgroup.HasUserGroupGrantsWith(entuggrant.UserGroupIDEQ(*u.UserGroupID)))
	}
	return client.ModelGroup.Query().
		Where(entgroup.ExclusiveEQ(true), entgroup.EnabledEQ(true), granted).
		Exist(ctx)
}

func couponPaidAmount(face, discount decimal.Decimal) (decimal.Decimal, error) {
	return normalizeOrderAmount(face.Mul(hundred.Sub(discount)).Div(hundred))
}

func couponRebateAmount(base, rate decimal.Decimal) decimal.Decimal {
	return base.Mul(rate).Div(hundred).Truncate(2)
}

// couponRefundAdjustment is the next reversal for a refund. Targeting the
// cumulative refund makes partial-refund rounding converge exactly to the
// original rebate when the order is fully refunded.
func couponRefundAdjustment(cumulativeRefund, rate, originalRebate, reversed decimal.Decimal) decimal.Decimal {
	target := decimal.Min(couponRebateAmount(cumulativeRefund, rate), originalRebate)
	delta := target.Sub(reversed)
	if delta.Sign() <= 0 {
		return decimal.Zero
	}
	return delta
}

// recordCouponOrderTx writes the agent's rebate for a coupon order inside
// the order's paid transaction. The rate comes from the order's snapshot,
// not the live profile.
func (s *AgentSvc) recordCouponOrderTx(ctx context.Context, tx *ent.Tx, order *ent.SiteOrder) error {
	if order.PromotionPromoterUserID == nil || order.PromotionRebatePct.Sign() <= 0 {
		return nil
	}
	amount := couponRebateAmount(order.Amount, order.PromotionRebatePct)
	if amount.Sign() <= 0 {
		return nil
	}
	return tx.AgentSettlementEntry.Create().
		SetAgentID(*order.PromotionPromoterUserID).
		SetKind(entagententry.KindCouponRebate).
		SetSaleAmount(order.Amount).
		SetPlatformPayable(amount.Neg()).
		SetSharePctAtEvent(order.PromotionRebatePct).
		SetCurrency(order.Currency).
		SetSourceType(entagententry.SourceTypeSiteOrder).
		SetSourceID(order.ID).
		SetOrderID(order.ID).
		SetUserID(order.UserID).
		SetNote("优惠码订单返利: " + order.OrderNo).
		Exec(ctx)
}

// reconcileCouponRefundTx appends one reversal for an approved refund on a
// coupon order. Idempotent per refund id.
func (s *AgentSvc) reconcileCouponRefundTx(ctx context.Context, tx *ent.Tx, orderID, refundID uuid.UUID, refundAmount, cumulativeRefund decimal.Decimal) error {
	rebate, err := tx.AgentSettlementEntry.Query().
		Where(entagententry.OrderIDEQ(orderID), entagententry.KindEQ(entagententry.KindCouponRebate)).
		ForUpdate().
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	reversals, err := tx.AgentSettlementEntry.Query().
		Where(entagententry.OrderIDEQ(orderID), entagententry.KindEQ(entagententry.KindCouponRefund)).
		All(ctx)
	if err != nil {
		return err
	}
	reversed := decimal.Zero
	for _, row := range reversals {
		if row.SourceID == refundID {
			return nil
		}
		reversed = reversed.Add(row.PlatformPayable)
	}
	delta := couponRefundAdjustment(cumulativeRefund, rebate.SharePctAtEvent, rebate.PlatformPayable.Neg(), reversed)
	if delta.Sign() <= 0 {
		return nil
	}
	return tx.AgentSettlementEntry.Create().
		SetAgentID(rebate.AgentID).
		SetKind(entagententry.KindCouponRefund).
		SetSaleAmount(refundAmount.Neg()).
		SetPlatformPayable(delta).
		SetSharePctAtEvent(rebate.SharePctAtEvent).
		SetCurrency(rebate.Currency).
		SetSourceType(entagententry.SourceTypeSiteRefund).
		SetSourceID(refundID).
		SetOrderID(orderID).
		SetNillableUserID(rebate.UserID).
		SetNote("订单退款冲减返利").
		Exec(ctx)
}

// CouponBuyerCount is the number of distinct buyers who have paid an order
// with the agent's coupon.
func (s *AgentSvc) CouponBuyerCount(ctx context.Context, agentID uuid.UUID) (int, error) {
	var buyerIDs []uuid.UUID
	err := s.d.Ent.SiteOrder.Query().
		Where(
			entsiteorder.PromotionPromoterUserIDEQ(agentID),
			entsiteorder.StatusIn(entsiteorder.StatusPaid, entsiteorder.StatusPartiallyRefunded, entsiteorder.StatusRefunded),
		).
		GroupBy(entsiteorder.FieldUserID).
		Scan(ctx, &buyerIDs)
	return len(buyerIDs), err
}
