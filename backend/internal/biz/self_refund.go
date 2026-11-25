package biz

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entsiterefund "github.com/jucode/jucode/internal/data/ent/siterefund"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

// Self-service refunds send a user's refundable balance back through the
// channel it was paid with, newest order first, and refund active quota
// subscriptions for the time left. Only channels with a refund gateway take
// part; whatever no gateway can return (other channels, manual credits,
// refusals) stays in the balance for staff to handle on request. Refund
// tickets can be approved the same way automatically (AutoApproveTicketRefund).

const (
	selfRefundReason = "self_refund"
	// Payment channels accept refunds for 12 months after payment.
	refundWindow = 365 * 24 * time.Hour
)

var (
	ErrSelfRefundDisabled       = errors.New("自助退款未开启")
	ErrSelfRefundBusy           = errors.New("退款正在处理，请稍后再试")
	ErrSelfRefundUnresolved     = errors.New("上一笔退款结果尚未确认，请联系客服")
	ErrRefundGatewayUnavailable = errors.New("没有可用的退款渠道")
)

// RefundRequest asks a payment channel to return money for one order.
// RequestNo identifies the attempt; resending it never refunds twice.
type RefundRequest struct {
	OrderNo     string
	RequestNo   string
	AmountCents int64
	Reason      string
}

// RefundOutcome: Refunded means the money moved. Rejected means the channel
// refused for good (Detail says why). Neither means the result is unknown;
// the caller must not treat it as either.
type RefundOutcome struct {
	Refunded bool
	Rejected bool
	Detail   string
}

type RefundGateway interface {
	Refund(ctx context.Context, in RefundRequest) (RefundOutcome, error)
	// QueryRefund settles an earlier attempt whose outcome was unknown.
	QueryRefund(ctx context.Context, orderNo, requestNo string) (RefundOutcome, error)
}

// SetRefundGateway lets orders paid through channel be refunded back
// through g.
func (s *FinanceSvc) SetRefundGateway(channel string, g RefundGateway) {
	if s.refundGateways == nil {
		s.refundGateways = map[string]RefundGateway{}
	}
	s.refundGateways[channel] = g
}

func (s *FinanceSvc) refundChannels() []string {
	out := make([]string, 0, len(s.refundGateways))
	for ch := range s.refundGateways {
		out = append(out, ch)
	}
	return out
}

// SelfRefundOrder is one order a self-service refund goes back through.
type SelfRefundOrder struct {
	OrderID uuid.UUID       `json:"order_id"`
	OrderNo string          `json:"order_no"`
	PaidAt  *time.Time      `json:"paid_at,omitempty"`
	Cash    decimal.Decimal `json:"cash"`   // returned to the buyer
	Credit  decimal.Decimal `json:"credit"` // taken off the balance
}

type SelfRefundPlan struct {
	Enabled bool `json:"enabled"`
	// Refundable is the paid-for part of the balance; Cash is what the
	// listed orders return, Unsupported what no gateway can return.
	Refundable  decimal.Decimal   `json:"refundable"`
	Orders      []SelfRefundOrder `json:"orders"`
	Cash        decimal.Decimal   `json:"cash"`
	Unsupported decimal.Decimal   `json:"unsupported"`
	// Plans are active quota subscriptions, refunded for the time left.
	// Auto ones go back through their channel; the rest need staff.
	Plans []SelfRefundSubscription `json:"plans"`

	balance decimal.Decimal
}

type SelfRefundSubscription struct {
	OrderID       uuid.UUID       `json:"order_id"`
	OrderNo       string          `json:"order_no"`
	PlanName      string          `json:"plan_name"`
	Channel       string          `json:"channel"`
	Cash          decimal.Decimal `json:"cash"`
	RemainingDays int             `json:"remaining_days"`
	TotalDays     int             `json:"total_days"`
	Auto          bool            `json:"auto"`
}

type SelfRefundResult struct {
	Refunded      []SelfRefundOrder        `json:"refunded"`
	RefundedPlans []SelfRefundSubscription `json:"refunded_plans"`
	Failed        []string                 `json:"failed,omitempty"`
	Unsupported   decimal.Decimal          `json:"unsupported"`
}

func (s *FinanceSvc) selfRefundEnabled(ctx context.Context) (bool, error) {
	if s.settings == nil {
		return false, nil
	}
	st, err := s.settings.GetSite(ctx)
	if err != nil {
		return false, err
	}
	return st.SelfRefundEnabled, nil
}

// orderCredit is the balance an order put on the account: the credited face
// value of a top-up, or the initial balance of a pay-as-you-go plan.
func (s *FinanceSvc) orderCredit(ctx context.Context, o *ent.SiteOrder) (decimal.Decimal, error) {
	if o.Kind == entsiteorder.KindBalance {
		if o.CreditAmount.Sign() > 0 {
			return o.CreditAmount, nil
		}
		return o.Amount, nil
	}
	var sum decimal.NullDecimal
	err := s.d.DB().QueryRowContext(ctx,
		`SELECT sum(amount) FROM balance_txes WHERE user_id = $1 AND amount > 0 AND note = $2`,
		o.UserID, "plan initial balance: "+o.OrderNo).Scan(&sum)
	if err != nil {
		return decimal.Zero, err
	}
	return sum.Decimal, nil
}

// PlanSelfRefund works out which orders a refund of the user's refundable
// balance would go back through. It changes nothing.
func (s *FinanceSvc) PlanSelfRefund(ctx context.Context, userID uuid.UUID) (*SelfRefundPlan, error) {
	enabled, err := s.selfRefundEnabled(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	split, err := s.BalanceSplitFor(ctx, u)
	if err != nil {
		return nil, err
	}
	plan := &SelfRefundPlan{Enabled: enabled, Refundable: split.Refundable, balance: split.Balance}
	left := split.Refundable
	if channels := s.refundChannels(); len(channels) > 0 && left.Sign() > 0 {
		orders, err := s.d.Ent.SiteOrder.Query().
			Where(
				entsiteorder.UserIDEQ(userID),
				entsiteorder.PaymentChannelIn(channels...),
				entsiteorder.StatusIn(entsiteorder.StatusPaid, entsiteorder.StatusPartiallyRefunded),
				entsiteorder.PaidAtGTE(time.Now().Add(-refundWindow)),
				entsiteorder.AmountGT(decimal.Zero),
			).
			All(ctx)
		if err != nil {
			return nil, err
		}
		sort.Slice(orders, func(i, j int) bool { return orders[i].PaidAt.After(*orders[j].PaidAt) })
		for _, o := range orders {
			if left.Sign() <= 0 {
				break
			}
			credit, err := s.orderCredit(ctx, o)
			if err != nil {
				return nil, err
			}
			if credit.Sign() <= 0 {
				continue // a quota subscription carries no balance
			}
			refunded, err := s.sumRefundedCash(ctx, o.ID)
			if err != nil {
				return nil, err
			}
			cashLeft := o.Amount.Sub(refunded)
			if cashLeft.Sign() <= 0 {
				continue
			}
			creditLeft := credit.Mul(cashLeft).Div(o.Amount)
			item := SelfRefundOrder{OrderID: o.ID, OrderNo: o.OrderNo, PaidAt: o.PaidAt}
			if left.GreaterThanOrEqual(creditLeft) {
				item.Cash, item.Credit = cashLeft, creditLeft
			} else {
				// Round the cash down to cents so the refund never exceeds
				// the balance it takes.
				item.Cash = left.Mul(o.Amount).Div(credit).RoundFloor(2)
				item.Credit = item.Cash.Mul(credit).Div(o.Amount)
			}
			if item.Cash.LessThan(decimal.New(1, -2)) {
				continue
			}
			left = left.Sub(item.Credit)
			plan.Cash = plan.Cash.Add(item.Cash)
			plan.Orders = append(plan.Orders, item)
		}
	}
	plan.Unsupported = decimal.Max(left, decimal.Zero).Round(2)
	if plan.Plans, err = s.planSubscriptionRefunds(ctx, userID); err != nil {
		return nil, err
	}
	return plan, nil
}

// planSubscriptionRefunds prices each active quota subscription the user paid for:
// what they paid times the share of the period still ahead.
func (s *FinanceSvc) planSubscriptionRefunds(ctx context.Context, userID uuid.UUID) ([]SelfRefundSubscription, error) {
	now := time.Now()
	ups, err := s.d.Ent.UserPlan.Query().
		Where(
			entuserplan.UserIDEQ(userID),
			entuserplan.StatusEQ(entuserplan.StatusActive),
			entuserplan.ExpireAtGT(now),
			entuserplan.HasPlanWith(entplan.TypeEQ(entplan.TypeQuotaSubscription)),
		).
		WithPlan().
		All(ctx)
	if err != nil {
		return nil, err
	}
	var out []SelfRefundSubscription
	for _, up := range ups {
		o, err := s.d.Ent.SiteOrder.Query().
			Where(
				entsiteorder.UserPlanIDEQ(up.ID),
				entsiteorder.StatusIn(entsiteorder.StatusPaid, entsiteorder.StatusPartiallyRefunded),
				entsiteorder.AmountGT(decimal.Zero),
			).
			First(ctx)
		if ent.IsNotFound(err) {
			continue // granted by a redeem code or by staff: nothing was paid here
		}
		if err != nil {
			return nil, err
		}
		taken, err := s.sumRefundedCash(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		if taken.Sign() > 0 {
			continue // a refund is already paid or on its way for this plan
		}
		total := up.ExpireAt.Sub(up.StartAt)
		if total <= 0 {
			continue
		}
		from := up.StartAt
		if now.After(from) {
			from = now
		}
		left := up.ExpireAt.Sub(from)
		cash := o.Amount.Mul(decimal.NewFromInt(int64(left))).Div(decimal.NewFromInt(int64(total))).RoundFloor(2)
		if cash.LessThan(decimal.New(1, -2)) {
			continue
		}
		name := ""
		if up.Edges.Plan != nil {
			name = up.Edges.Plan.Name
		}
		out = append(out, SelfRefundSubscription{
			OrderID:       o.ID,
			OrderNo:       o.OrderNo,
			PlanName:      name,
			Channel:       o.PaymentChannel,
			Cash:          cash,
			RemainingDays: int(left.Hours() / 24),
			TotalDays:     int(total.Hours()/24 + 0.5),
			Auto:          s.refundGateways[o.PaymentChannel] != nil,
		})
	}
	return out, nil
}

// refundSubscriptions refunds each planned subscription that its channel
// can refund automatically; the plan stops once the refund is booked.
// Subscriptions paid elsewhere are left for staff.
func (s *FinanceSvc) refundSubscriptions(ctx context.Context, plans []SelfRefundSubscription, actorID uuid.UUID, res *SelfRefundResult) {
	for _, p := range plans {
		gw := s.refundGateways[p.Channel]
		if gw == nil {
			continue
		}
		cash := p.Cash
		if err := s.takeOverRequests(ctx, p.OrderID); err != nil {
			res.Failed = append(res.Failed, p.OrderNo+": "+err.Error())
			continue
		}
		r, err := s.CreateRefund(ctx, CreateRefundInput{OrderID: p.OrderID, Amount: &cash, Reason: selfRefundReason, Note: "套餐按剩余时间退款"})
		if err != nil {
			res.Failed = append(res.Failed, p.OrderNo+": "+err.Error())
			continue
		}
		if _, err := s.approveGatewayRefund(ctx, gw, r.ID, actorID, ""); err != nil {
			res.Failed = append(res.Failed, p.OrderNo+": "+err.Error())
			continue
		}
		res.RefundedPlans = append(res.RefundedPlans, p)
	}
}

// sumRefundedCash counts approved refunds and attempts the system has under
// way (self-service refunds, reservations), so an attempt whose outcome is
// unknown is never planned again. A user's pending request is not counted:
// a self-service refund takes it over (takeOverRequestsTx).
func (s *FinanceSvc) sumRefundedCash(ctx context.Context, orderID uuid.UUID) (decimal.Decimal, error) {
	rows, err := s.d.Ent.SiteRefund.Query().
		Where(entsiterefund.OrderIDEQ(orderID), entsiterefund.StatusIn(entsiterefund.StatusApproved, entsiterefund.StatusPending)).
		All(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	sum := decimal.Zero
	for _, r := range rows {
		if r.Status == entsiterefund.StatusPending && r.Reason != selfRefundReason && !sentToGateway(r) {
			continue
		}
		sum = sum.Add(r.Amount)
	}
	return sum, nil
}

// takeOverRequestsTx cancels pending refund requests on an order that no
// one has sent to a payment channel yet, so a self-service refund can replace
// them instead of being blocked by them.
func takeOverRequestsTx(ctx context.Context, tx *ent.Tx, orderID uuid.UUID) error {
	rows, err := tx.SiteRefund.Query().
		Where(
			entsiterefund.OrderIDEQ(orderID),
			entsiterefund.StatusEQ(entsiterefund.StatusPending),
			entsiterefund.ReasonNEQ(selfRefundReason),
		).
		ForUpdate().
		All(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if sentToGateway(r) {
			continue
		}
		if err := tx.SiteRefund.UpdateOneID(r.ID).
			SetStatus(entsiterefund.StatusCanceled).
			SetNote(strings.TrimSpace(r.Note + "\n由自助退款接管")).
			Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteSelfRefund refunds the user's refundable balance and quota
// subscriptions through their payment channels and reports what is left for
// staff. The user is recorded as the reviewer of each refund.
func (s *FinanceSvc) ExecuteSelfRefund(ctx context.Context, userID uuid.UUID) (*SelfRefundResult, error) {
	enabled, err := s.selfRefundEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrSelfRefundDisabled
	}
	if len(s.refundGateways) == 0 {
		return nil, ErrRefundGatewayUnavailable
	}
	// One run per user at a time: two concurrent runs would each plan
	// against the same refundable balance.
	conn, err := s.d.DB().Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended('self-refund:' || $1::text, 0))`, userID).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrSelfRefundBusy
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended('self-refund:' || $1::text, 0))`, userID)
	}()
	// Money may leave during this call; a client hanging up must not stop
	// the bookkeeping halfway.
	ctx = context.WithoutCancel(ctx)

	if err := s.settlePendingSelfRefunds(ctx, userID); err != nil {
		return nil, err
	}
	plan, err := s.PlanSelfRefund(ctx, userID)
	if err != nil {
		return nil, err
	}
	res := &SelfRefundResult{}
	// Everything in the balance that is not refundable: gifts and manual
	// credits. Spending drains these first, so the refundable part only
	// shrinks once they are gone; a reservation must leave at least this
	// much behind or it would refund money already spent.
	keep := decimal.Max(plan.balance, decimal.Zero).Sub(plan.Refundable)
	for _, item := range plan.Orders {
		o, err := s.d.Ent.SiteOrder.Get(ctx, item.OrderID)
		if err != nil {
			return nil, err
		}
		gw := s.refundGateways[o.PaymentChannel]
		r, err := s.reserveSelfRefund(ctx, item, keep)
		if errors.Is(err, errRefundBalanceGone) {
			break
		}
		if err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", item.OrderNo, err))
			continue
		}
		out, err := gw.Refund(ctx, RefundRequest{
			OrderNo:     item.OrderNo,
			RequestNo:   requestNo(r.ID),
			AmountCents: item.Cash.Shift(2).IntPart(),
			Reason:      "余额退款",
		})
		switch {
		case err == nil && out.Refunded:
			if err := s.finishReservedRefund(ctx, r.ID, userID); err != nil {
				return nil, fmt.Errorf("refund %s went through on the payment channel but was not marked: %w", item.OrderNo, err)
			}
			res.Refunded = append(res.Refunded, item)
		case err == nil && out.Rejected:
			if err := s.reverseReservedRefund(ctx, r.ID, userID, "支付渠道拒绝："+out.Detail); err != nil {
				return nil, err
			}
			res.Failed = append(res.Failed, item.OrderNo+": "+out.Detail)
		default:
			// Unknown outcome: the balance stays reserved and the refund
			// pending, so the next run asks the channel before planning again.
			detail := "结果未知"
			if err != nil {
				detail = err.Error()
			}
			res.Failed = append(res.Failed, item.OrderNo+": "+detail)
			return res, s.fillSelfRefundRemainder(ctx, userID, res)
		}
	}
	s.refundSubscriptions(ctx, plan.Plans, userID, res)
	return res, s.fillSelfRefundRemainder(ctx, userID, res)
}

// fillSelfRefundRemainder reads what is still refundable once the gateways
// are done; staff refund it on request.
func (s *FinanceSvc) fillSelfRefundRemainder(ctx context.Context, userID uuid.UUID, res *SelfRefundResult) error {
	u, err := s.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return err
	}
	split, err := s.BalanceSplitFor(ctx, u)
	if err != nil {
		return err
	}
	res.Unsupported = split.Refundable.Round(2)
	return nil
}

// requestNo is the payment channel's refund request number for a refund row.
func requestNo(id uuid.UUID) string { return "RF" + strings.ReplaceAll(id.String(), "-", "") }

// settlePendingSelfRefunds asks the payment channels about attempts left
// pending by an earlier run (timeouts, crashes) and books or rejects them.
func (s *FinanceSvc) settlePendingSelfRefunds(ctx context.Context, userID uuid.UUID) error {
	pending, err := s.d.Ent.SiteRefund.Query().
		Where(
			entsiterefund.UserIDEQ(userID),
			entsiterefund.ReasonEQ(selfRefundReason),
			entsiterefund.StatusEQ(entsiterefund.StatusPending),
		).
		WithOrder().
		All(ctx)
	if err != nil {
		return err
	}
	for _, r := range pending {
		o := r.Edges.Order
		if o == nil {
			return ErrSelfRefundUnresolved
		}
		gw := s.refundGateways[o.PaymentChannel]
		if gw == nil {
			return ErrSelfRefundUnresolved
		}
		_, reserved, err := reservedCredit(r)
		if err != nil {
			return err
		}
		out, err := gw.QueryRefund(ctx, o.OrderNo, requestNo(r.ID))
		if err != nil {
			return ErrSelfRefundUnresolved
		}
		switch {
		case out.Refunded && reserved:
			err = s.finishReservedRefund(ctx, r.ID, userID)
		case out.Refunded:
			_, err = s.approveRefundBooked(ctx, r.ID, userID, "")
		case out.Rejected && reserved:
			err = s.reverseReservedRefund(ctx, r.ID, userID, "支付渠道无此退款："+out.Detail)
		case out.Rejected:
			_, err = s.rejectRefundRow(ctx, r.ID, userID, "支付渠道无此退款："+out.Detail)
		default:
			return ErrSelfRefundUnresolved
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var errRefundBalanceGone = errors.New("超过可退余额，已消费的部分不能退")

// reserveSelfRefund records a pending refund for item and reserves its
// credit, in one transaction.
func (s *FinanceSvc) reserveSelfRefund(ctx context.Context, item SelfRefundOrder, keep decimal.Decimal) (*ent.SiteRefund, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	o, err := tx.SiteOrder.Query().Where(entsiteorder.IDEQ(item.OrderID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := takeOverRequestsTx(ctx, tx, o.ID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	r, err := tx.SiteRefund.Create().
		SetOrderID(o.ID).
		SetUserID(o.UserID).
		SetAmount(item.Cash).
		SetCurrency(o.Currency).
		SetReason(selfRefundReason).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if r, err = reserveRefundTx(ctx, tx, r, o, item.Credit, keep); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// reserveRefundTx takes a pending refund's credit off the balance before
// any money moves, so the user cannot spend what is being refunded, and
// notes the amount on the refund. It refuses when the balance left would
// drop below keep (the part that is not refundable): that would refund
// money already spent. o must be locked by the caller.
func reserveRefundTx(ctx context.Context, tx *ent.Tx, r *ent.SiteRefund, o *ent.SiteOrder, credit, keep decimal.Decimal) (*ent.SiteRefund, error) {
	if o.Status != entsiteorder.StatusPaid && o.Status != entsiteorder.StatusPartiallyRefunded {
		return nil, ErrOrderInvalidState
	}
	taken, err := tx.SiteRefund.Query().
		Where(
			entsiterefund.OrderIDEQ(o.ID),
			entsiterefund.IDNEQ(r.ID),
			entsiterefund.StatusIn(entsiterefund.StatusApproved, entsiterefund.StatusPending),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	cashLeft := o.Amount
	for _, t := range taken {
		cashLeft = cashLeft.Sub(t.Amount)
	}
	if r.Amount.GreaterThan(cashLeft) {
		return nil, ErrOrderInvalidState
	}
	u, err := tx.User.Query().Where(entuser.IDEQ(o.UserID)).ForUpdate().Only(ctx)
	if err != nil {
		return nil, err
	}
	credit = credit.Truncate(8)
	newBal := u.Balance.Sub(credit)
	if newBal.LessThan(keep) {
		return nil, errRefundBalanceGone
	}
	snap := r.PolicySnapshot
	if snap == nil {
		snap = map[string]any{}
	}
	snap[reservedCreditKey] = credit.String()
	r, err = tx.SiteRefund.UpdateOneID(r.ID).SetPolicySnapshot(snap).Save(ctx)
	if err != nil {
		return nil, err
	}
	if err := refundBalanceTx(ctx, tx, u.ID, credit.Neg(), newBal, siteOrderRefundNote+o.OrderNo); err != nil {
		return nil, err
	}
	return r, nil
}

const (
	reservedCreditKey = "reserved_credit"
	// sentToGatewayKey marks a refund handed to a payment channel that took
	// no balance (a quota subscription): its money may move even though
	// nothing was reserved.
	sentToGatewayKey = "sent_to_gateway"
)

// sentToGateway reports whether r may already have moved money: a payment
// channel has been asked to pay it. Such a refund is settled by asking the
// channel, never cancelled, rejected by hand or planned again.
func sentToGateway(r *ent.SiteRefund) bool {
	if _, reserved, _ := reservedCredit(r); reserved {
		return true
	}
	sent, _ := r.PolicySnapshot[sentToGatewayKey].(bool)
	return sent
}

// markSentToGateway records that r is about to go to a payment channel. It
// refuses a refund that is no longer pending, so a cancelled or settled row
// is never sent.
func (s *FinanceSvc) markSentToGateway(ctx context.Context, id uuid.UUID) error {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	r, err := tx.SiteRefund.Query().Where(entsiterefund.IDEQ(id)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if r.Status != entsiterefund.StatusPending {
		_ = tx.Rollback()
		return ErrOrderInvalidState
	}
	snap := r.PolicySnapshot
	if snap == nil {
		snap = map[string]any{}
	}
	snap[sentToGatewayKey] = true
	if err := tx.SiteRefund.UpdateOneID(id).SetPolicySnapshot(snap).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// reservedCredit is the credit a reservation took; ok is false when the
// refund was never reserved.
func reservedCredit(r *ent.SiteRefund) (decimal.Decimal, bool, error) {
	v, ok := r.PolicySnapshot[reservedCreditKey].(string)
	if !ok {
		return decimal.Zero, false, nil
	}
	d, err := decimal.NewFromString(v)
	return d, true, err
}

func refundBalanceTx(ctx context.Context, tx *ent.Tx, userID uuid.UUID, amount, newBal decimal.Decimal, note string) error {
	if _, err := tx.User.UpdateOneID(userID).SetBalance(newBal).Save(ctx); err != nil {
		return err
	}
	_, err := tx.BalanceTx.Create().
		SetUserID(userID).
		SetKind(entbalancetx.KindRefund).
		SetAmount(amount).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetNote(note).
		Save(ctx)
	return err
}

// finishReservedRefund marks a reserved refund paid once the payment channel
// confirms it. The balance was already taken at reservation.
func (s *FinanceSvc) finishReservedRefund(ctx context.Context, refundID, reviewer uuid.UUID) error {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	r, err := tx.SiteRefund.Query().Where(entsiterefund.IDEQ(refundID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if r.Status != entsiterefund.StatusPending {
		_ = tx.Rollback()
		return ErrOrderInvalidState
	}
	o, err := tx.SiteOrder.Query().Where(entsiteorder.IDEQ(r.OrderID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.SiteRefund.UpdateOneID(r.ID).
		SetStatus(entsiterefund.StatusApproved).
		SetReviewedByUserID(reviewer).
		SetReviewedAt(time.Now()).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	approved, err := sumApprovedRefundsTx(ctx, tx, o.ID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	status := entsiteorder.StatusPartiallyRefunded
	if approved.GreaterThanOrEqual(o.Amount) {
		status = entsiteorder.StatusRefunded
	}
	if _, err := tx.SiteOrder.UpdateOneID(o.ID).SetStatus(status).Save(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if s.agents != nil {
		if err := s.agents.reconcileCouponRefundTx(ctx, tx, o.ID, r.ID, r.Amount, approved); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.invites != nil {
		rc, cancel := context.WithTimeout(ctx, 20*time.Second)
		s.invites.ReconcileRefund(rc, o.UserID, o.ID, approved)
		cancel()
	}
	s.resolveRefundTickets(ctx, o.ID)
	return nil
}

// reverseReservedRefund rejects a reserved refund the payment channel did
// not pay and puts its credit back. The credit row carries the
// "site refund: " note so it returns to the paid part of the balance.
func (s *FinanceSvc) reverseReservedRefund(ctx context.Context, refundID, reviewer uuid.UUID, note string) error {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	r, err := tx.SiteRefund.Query().Where(entsiterefund.IDEQ(refundID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if r.Status != entsiterefund.StatusPending {
		_ = tx.Rollback()
		return ErrOrderInvalidState
	}
	o, err := tx.SiteOrder.Get(ctx, r.OrderID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	credit, ok, err := reservedCredit(r)
	if err != nil || !ok {
		_ = tx.Rollback()
		return fmt.Errorf("refund %s has no reservation to reverse: %v", r.ID, err)
	}
	u, err := tx.User.Query().Where(entuser.IDEQ(r.UserID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := refundBalanceTx(ctx, tx, u.ID, credit, u.Balance.Add(credit), siteOrderRefundNote+o.OrderNo+" 未退成功，已退回余额"); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.SiteRefund.UpdateOneID(r.ID).
		SetStatus(entsiterefund.StatusRejected).
		SetReviewedByUserID(reviewer).
		SetReviewedAt(time.Now()).
		SetNote(strings.TrimSpace(r.Note + "\n" + note)).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// approveGatewayRefund is ApproveRefund for an order paid through a channel
// with a refund gateway: the money goes back the way it came instead of
// being paid by hand. Orders that put balance on the account reserve that
// balance first, the same way self-service refunds do; a quota subscription
// has no balance and keeps the usual plan bookkeeping once the channel has
// paid.
func (s *FinanceSvc) approveGatewayRefund(ctx context.Context, gw RefundGateway, id, reviewer uuid.UUID, note string) (*ent.SiteRefund, error) {
	ctx = context.WithoutCancel(ctx)
	r, err := s.d.Ent.SiteRefund.Query().Where(entsiterefund.IDEQ(id)).WithOrder().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrRefundNotFound
		}
		return nil, err
	}
	if r.Status != entsiterefund.StatusPending {
		return nil, ErrOrderInvalidState
	}
	o := r.Edges.Order
	credit, err := s.orderCredit(ctx, o)
	if err != nil {
		return nil, err
	}
	req := RefundRequest{OrderNo: o.OrderNo, RequestNo: requestNo(r.ID), AmountCents: r.Amount.Shift(2).IntPart(), Reason: "退款"}

	if credit.Sign() <= 0 {
		if err := s.markSentToGateway(ctx, id); err != nil {
			return nil, err
		}
		out, err := gw.Refund(ctx, req)
		if err == nil && out.Rejected {
			if _, err := s.rejectRefundRow(ctx, id, reviewer, "支付渠道拒绝："+out.Detail); err != nil {
				return nil, err
			}
		}
		if err := refundOutcomeErr(out, err); err != nil {
			return nil, err
		}
		return s.approveRefundBooked(ctx, id, reviewer, note)
	}

	_, reserved, err := reservedCredit(r)
	if err != nil {
		return nil, err
	}
	var out RefundOutcome
	if reserved {
		// An earlier approval reached the channel without a clear answer.
		out, err = gw.QueryRefund(ctx, o.OrderNo, req.RequestNo)
	} else {
		if err := s.reserveForApproval(ctx, r, o, credit.Mul(r.Amount).Div(o.Amount)); err != nil {
			return nil, err
		}
		out, err = gw.Refund(ctx, req)
	}
	switch {
	case err == nil && out.Refunded:
		if err := s.finishReservedRefund(ctx, r.ID, reviewer); err != nil {
			return nil, fmt.Errorf("支付渠道已退款，但记账失败：%w", err)
		}
	case err == nil && out.Rejected:
		if err := s.reverseReservedRefund(ctx, r.ID, reviewer, "支付渠道拒绝："+out.Detail); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("支付渠道拒绝退款：%s。余额已退回，退款已驳回", out.Detail)
	default:
		return nil, ErrRefundOutcomeUnknown
	}
	return s.d.Ent.SiteRefund.Get(ctx, id)
}

var ErrRefundOutcomeUnknown = errors.New("退款结果暂未确认，余额已预扣；稍后再次点击审批会向支付渠道查询结果")

func refundOutcomeErr(out RefundOutcome, err error) error {
	switch {
	case err == nil && out.Refunded:
		return nil
	case err == nil && out.Rejected:
		return fmt.Errorf("支付渠道拒绝退款：%s", out.Detail)
	default:
		return ErrRefundOutcomeUnknown
	}
}

// reserveForApproval reserves the credit of a refund an admin approves,
// refusing any part the user has already spent.
func (s *FinanceSvc) reserveForApproval(ctx context.Context, r *ent.SiteRefund, o *ent.SiteOrder, credit decimal.Decimal) error {
	u, err := s.d.Ent.User.Get(ctx, o.UserID)
	if err != nil {
		return err
	}
	split, err := s.BalanceSplitFor(ctx, u)
	if err != nil {
		return err
	}
	keep := decimal.Max(split.Balance, decimal.Zero).Sub(split.Refundable)
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	lr, err := tx.SiteRefund.Query().Where(entsiterefund.IDEQ(r.ID)).ForUpdate().Only(ctx)
	if err == nil {
		// Locked: a second approval running at the same time sees the
		// reservation and stops here.
		if lr.Status != entsiterefund.StatusPending {
			err = ErrOrderInvalidState
		} else if _, reserved, _ := reservedCredit(lr); reserved {
			err = ErrRefundOutcomeUnknown
		}
	}
	var lo *ent.SiteOrder
	if err == nil {
		lo, err = tx.SiteOrder.Query().Where(entsiteorder.IDEQ(o.ID)).ForUpdate().Only(ctx)
	}
	if err == nil {
		_, err = reserveRefundTx(ctx, tx, lr, lo, credit, keep)
	}
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, errRefundBalanceGone) {
			return fmt.Errorf("%w（当前可退 ¥%s）", err, split.Refundable.StringFixed(2))
		}
		return err
	}
	return tx.Commit()
}

// OrderRefundable is how much of one order can be refunded now: what is
// left of the order, capped by the user's refundable balance so money
// already spent is never refunded. A quota subscription carries no balance
// (BalanceBearing=false) and keeps the time-based refund policy.
type OrderRefundable struct {
	OrderID        uuid.UUID       `json:"order_id"`
	Cash           decimal.Decimal `json:"cash"`
	BalanceBearing bool            `json:"balance_bearing"`
	// Refundable is the paid-for part of the user's balance.
	Refundable decimal.Decimal `json:"refundable"`
}

func (s *FinanceSvc) OrderRefundableFor(ctx context.Context, o *ent.SiteOrder) (*OrderRefundable, error) {
	out := &OrderRefundable{OrderID: o.ID}
	credit, err := s.orderCredit(ctx, o)
	if err != nil {
		return nil, err
	}
	u, err := s.d.Ent.User.Get(ctx, o.UserID)
	if err != nil {
		return nil, err
	}
	split, err := s.BalanceSplitFor(ctx, u)
	if err != nil {
		return nil, err
	}
	out.Refundable = split.Refundable
	if credit.Sign() <= 0 {
		return out, nil
	}
	out.BalanceBearing = true
	if o.Status != entsiteorder.StatusPaid && o.Status != entsiteorder.StatusPartiallyRefunded {
		return out, nil
	}
	taken, err := s.sumRefundedCash(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	cashLeft := decimal.Max(o.Amount.Sub(taken), decimal.Zero)
	creditLeft := credit.Mul(cashLeft).Div(o.Amount)
	if split.Refundable.GreaterThanOrEqual(creditLeft) {
		out.Cash = cashLeft
	} else {
		out.Cash = split.Refundable.Mul(o.Amount).Div(credit).RoundFloor(2)
	}
	return out, nil
}

func (s *FinanceSvc) OrderRefundable(ctx context.Context, orderID uuid.UUID) (*OrderRefundable, error) {
	o, err := s.d.Ent.SiteOrder.Get(ctx, orderID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	return s.OrderRefundableFor(ctx, o)
}

var ErrNothingRefundable = errors.New("该订单当前没有可退金额，已消费的部分不能退")

// checkBookedRefund refuses a by-hand refund that would take more balance
// than the user has refundable.
func (s *FinanceSvc) checkBookedRefund(ctx context.Context, id uuid.UUID) error {
	r, err := s.d.Ent.SiteRefund.Query().Where(entsiterefund.IDEQ(id)).WithOrder().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return ErrRefundNotFound
		}
		return err
	}
	o := r.Edges.Order
	credit, err := s.orderCredit(ctx, o)
	if err != nil || credit.Sign() <= 0 {
		return err
	}
	u, err := s.d.Ent.User.Get(ctx, o.UserID)
	if err != nil {
		return err
	}
	split, err := s.BalanceSplitFor(ctx, u)
	if err != nil {
		return err
	}
	if credit.Mul(r.Amount).Div(o.Amount).Sub(split.Refundable).GreaterThan(decimal.New(1, -6)) {
		return fmt.Errorf("%w（当前可退 ¥%s）", errRefundBalanceGone, split.Refundable.StringFixed(2))
	}
	return nil
}

func (s *FinanceSvc) takeOverRequests(ctx context.Context, orderID uuid.UUID) error {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	if err := takeOverRequestsTx(ctx, tx, orderID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
