package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entprovider "github.com/jucode/jucode/internal/data/ent/provider"
	entprovidercost "github.com/jucode/jucode/internal/data/ent/providercost"
	entprovidersettlement "github.com/jucode/jucode/internal/data/ent/providersettlement"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entsiterefund "github.com/jucode/jucode/internal/data/ent/siterefund"
	entsetting "github.com/jucode/jucode/internal/data/ent/sitesetting"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entaccountcost "github.com/jucode/jucode/internal/data/ent/upstreamaccountcost"
	entaccountsettlement "github.com/jucode/jucode/internal/data/ent/upstreamaccountsettlement"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

const keyRefundPolicy = "refund_policy"

var (
	ErrOrderNotFound         = errors.New("order not found")
	ErrRefundNotFound        = errors.New("refund not found")
	ErrRefundPending         = errors.New("a pending refund already exists for this order")
	ErrOrderInvalidState     = errors.New("order state does not allow this operation")
	ErrPaymentUnavailable    = errors.New("payment channel unavailable")
	ErrRefundManaged         = errors.New("这笔退款已发往支付渠道，不能直接驳回；再次点击审批会向支付渠道查询结果")
	ErrPaymentAmountMismatch = errors.New("payment amount mismatch")
)

type NativePaymentClient interface {
	NativePrepay(ctx context.Context, in NativePrepayInput) (*NativePrepayResult, error)
	Query(ctx context.Context, in PaymentQueryInput) (*PaymentQueryResult, error)
}

type PaymentQueryInput struct {
	OrderNo       string
	TransactionID string
}

type NativePrepayInput struct {
	OrderNo     string
	Description string
	AmountCents int64
	Attach      string
	ReturnURL   string
	// Wap 让渠道改用手机端产品下单（支付宝走手机网站支付，能唤起 App）。
	Wap     bool
	QuitURL string
}

type NativePrepayResult struct {
	CodeURL     string
	AmountCents int64
}

type PaymentQueryResult struct {
	OrderNo       string
	TradeState    string
	TransactionID string
	PayerOpenID   string
	AmountCents   int64
}

type PaymentCallbackResult struct {
	OrderNo       string
	TradeState    string
	TransactionID string
	PayerOpenID   string
	AmountCents   int64
}

type RefundPolicy struct {
	PlanNoReasonDays    int     `json:"plan_no_reason_days"`
	BalanceNoReasonDays int     `json:"balance_no_reason_days"`
	PlanRefundRatio     float64 `json:"plan_refund_ratio"`
	BalanceRefundRatio  float64 `json:"balance_refund_ratio"`
	MinRefundAmount     string  `json:"min_refund_amount"`
}

var defaultRefundPolicy = RefundPolicy{
	PlanNoReasonDays:    7,
	BalanceNoReasonDays: 1,
	PlanRefundRatio:     0.7,
	BalanceRefundRatio:  1.0,
	MinRefundAmount:     "0",
}

type FinanceSvc struct {
	d        *data.Data
	settings *SettingsSvc
	alipay   NativePaymentClient
	invites  *InviteSvc // optional; re-evaluates signup rewards after topups
	agents   *AgentSvc  // optional; nil rejects coupon orders
	// refundGateways send refunds back through the channel an order was
	// paid with, keyed by payment channel (self_refund.go).
	refundGateways map[string]RefundGateway
}

func NewFinanceSvc(d *data.Data, settings *SettingsSvc) *FinanceSvc {
	return &FinanceSvc{d: d, settings: settings}
}

// SetInvites injects the invite service (late-wired; see Account.SetInvites).
func (s *FinanceSvc) SetInvites(inv *InviteSvc) { s.invites = inv }

func (s *FinanceSvc) SetAgents(a *AgentSvc) { s.agents = a }

func (s *FinanceSvc) SetAlipay(client NativePaymentClient) {
	s.alipay = client
}

// cnyRate is used ONLY by the account-settlement path below
// (accountSettlementUsageDetails / accountSettlementManualDetails /
// sumCNYByCurrency), where channel costs may be entered in USD and must
// be converted at settlement time. User-facing flows (orders, topups,
// refunds) run entirely in points (1 point = 1 CNY) and must NOT call it.
func (s *FinanceSvc) cnyRate() decimal.Decimal {
	if s.settings == nil {
		return decimal.Zero
	}
	return s.settings.CNYPerUSD()
}

func (s *FinanceSvc) paymentChannelEnabled(ctx context.Context, channel string) (bool, error) {
	if s.settings == nil {
		return true, nil
	}
	st, err := s.settings.GetSite(ctx)
	if err != nil {
		return false, err
	}
	switch channel {
	case "alipay":
		return st.PaymentAlipayEnabled, nil
	default:
		return false, nil
	}
}

func (s *FinanceSvc) GetRefundPolicy(ctx context.Context) (RefundPolicy, error) {
	row, err := s.d.Ent.SiteSetting.Query().
		Where(entsetting.KeyEQ(keyRefundPolicy)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return defaultRefundPolicy, nil
		}
		return RefundPolicy{}, err
	}
	out := defaultRefundPolicy
	if err := json.Unmarshal([]byte(row.Value), &out); err != nil {
		return RefundPolicy{}, err
	}
	return normalizeRefundPolicy(out), nil
}

func (s *FinanceSvc) SetRefundPolicy(ctx context.Context, in RefundPolicy) (RefundPolicy, error) {
	p := normalizeRefundPolicy(in)
	b, _ := json.Marshal(p)
	err := s.d.Ent.SiteSetting.Create().
		SetKey(keyRefundPolicy).
		SetValue(string(b)).
		OnConflictColumns(entsetting.FieldKey).
		UpdateNewValues().
		Exec(ctx)
	return p, err
}

func normalizeRefundPolicy(p RefundPolicy) RefundPolicy {
	if p.PlanNoReasonDays < 0 {
		p.PlanNoReasonDays = 0
	}
	if p.BalanceNoReasonDays < 0 {
		p.BalanceNoReasonDays = 0
	}
	if p.PlanRefundRatio < 0 {
		p.PlanRefundRatio = 0
	}
	if p.BalanceRefundRatio < 0 {
		p.BalanceRefundRatio = 0
	}
	if strings.TrimSpace(p.MinRefundAmount) == "" {
		p.MinRefundAmount = "0"
	}
	return p
}

// Balance-ledger note prefixes written for site order payments and refunds.
const (
	siteOrderTopupNote  = "site order: "
	siteOrderRefundNote = "site refund: "
)

type CreateOrderInput struct {
	UserID     uuid.UUID
	Kind       string
	PlanID     *uuid.UUID
	Amount     decimal.Decimal
	Currency   string
	Note       string
	CouponCode string
}

func (s *FinanceSvc) CreateOrder(ctx context.Context, in CreateOrderInput) (*ent.SiteOrder, error) {
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = CurrencyCNY
	}
	if currency != CurrencyCNY {
		return nil, errors.New("order currency must be CNY")
	}
	amount, err := normalizeOrderAmount(in.Amount)
	if err != nil {
		return nil, err
	}
	faceAmount := amount
	creditAmount := decimal.Zero
	if in.Kind == string(entsiteorder.KindBalance) {
		creditAmount = faceAmount
	}
	var coupon *CouponTerms
	if strings.TrimSpace(in.CouponCode) != "" {
		if s.agents == nil {
			return nil, errors.New("coupon service unavailable")
		}
		terms, err := s.agents.QuoteCoupon(ctx, in.UserID, in.CouponCode)
		if err != nil {
			return nil, err
		}
		if amount, err = couponPaidAmount(faceAmount, terms.DiscountPct); err != nil {
			return nil, err
		}
		coupon = &terms
	}
	q := s.d.Ent.SiteOrder.Create().
		SetOrderNo(newOrderNo()).
		SetUserID(in.UserID).
		SetKind(entsiteorder.Kind(in.Kind)).
		SetAmount(amount).
		SetCreditAmount(creditAmount).
		SetCurrency(currency).
		SetNote(in.Note)
	if coupon != nil {
		q = q.
			SetPromotionPromoterUserID(coupon.AgentID).
			SetPromotionCode(coupon.Code).
			SetPromotionDiscountPct(coupon.DiscountPct).
			SetPromotionRebatePct(coupon.RebatePct)
	}
	if in.PlanID != nil {
		q = q.SetPlanID(*in.PlanID)
	}
	return q.Save(ctx)
}

type OrderPaymentResult struct {
	Order   *ent.SiteOrder
	CodeURL string
	PayType string
}

type AlipayPayInput struct {
	OrderID   uuid.UUID
	ReturnURL string
	// Wap 走手机网站支付。PC 收银台在手机上只能扫码，唤不起支付宝 App，
	// 所以移动端必须走这条，两种链接互不通用。
	Wap     bool
	QuitURL string
}

func (s *FinanceSvc) CreateAlipayPay(ctx context.Context, in AlipayPayInput) (*OrderPaymentResult, error) {
	if s.alipay == nil {
		return nil, ErrPaymentUnavailable
	}
	o, err := s.d.Ent.SiteOrder.Get(ctx, in.OrderID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	if o.Status != entsiteorder.StatusPending {
		return nil, ErrOrderInvalidState
	}
	tradeType := "PAGE"
	if in.Wap {
		tradeType = "WAP"
	}
	// 复用旧链接前必须确认产品一致：把 PC 收银台地址发给手机就是唤不起 App 的老毛病。
	if o.PaymentChannel == "alipay" && o.PaymentTradeType == tradeType &&
		strings.TrimSpace(o.PaymentCodeURL) != "" && o.PaymentAmountCents > 0 {
		return &OrderPaymentResult{Order: o, CodeURL: o.PaymentCodeURL, PayType: tradeType}, nil
	}
	enabled, err := s.paymentChannelEnabled(ctx, "alipay")
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrPaymentUnavailable
	}
	cents, err := s.paymentAmountCents(o.Amount)
	if err != nil {
		return nil, err
	}
	res, err := s.alipay.NativePrepay(ctx, NativePrepayInput{
		OrderNo:     o.OrderNo,
		Description: s.orderDescription(ctx, o),
		AmountCents: cents,
		ReturnURL:   in.ReturnURL,
		Wap:         in.Wap,
		QuitURL:     in.QuitURL,
	})
	if err != nil {
		return nil, err
	}
	o, err = s.d.Ent.SiteOrder.UpdateOneID(o.ID).
		SetPaymentChannel("alipay").
		SetPaymentTradeType(tradeType).
		SetPaymentCodeURL(res.CodeURL).
		SetPaymentAmountCents(res.AmountCents).
		SetPaymentTradeState("WAIT_BUYER_PAY").
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return &OrderPaymentResult{Order: o, CodeURL: res.CodeURL, PayType: tradeType}, nil
}

func (s *FinanceSvc) SyncOrderPayment(ctx context.Context, id uuid.UUID) (*ent.SiteOrder, error) {
	o, err := s.d.Ent.SiteOrder.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	if o.Status != entsiteorder.StatusPending {
		return o, nil
	}
	switch o.PaymentChannel {
	case "alipay":
		if s.alipay == nil {
			return nil, ErrPaymentUnavailable
		}
		res, err := s.alipay.Query(ctx, PaymentQueryInput{OrderNo: o.OrderNo, TransactionID: o.PaymentTransactionID})
		if err != nil {
			return nil, err
		}
		return s.applyAlipayResult(ctx, PaymentCallbackResult{
			OrderNo:       o.OrderNo,
			TradeState:    res.TradeState,
			TransactionID: res.TransactionID,
			PayerOpenID:   res.PayerOpenID,
			AmountCents:   res.AmountCents,
		})
	default:
		return o, nil
	}
}

func (s *FinanceSvc) ApplyAlipayCallback(ctx context.Context, result PaymentCallbackResult) (*ent.SiteOrder, error) {
	return s.applyAlipayResult(ctx, result)
}

func (s *FinanceSvc) applyAlipayResult(ctx context.Context, result PaymentCallbackResult) (*ent.SiteOrder, error) {
	return s.applyNativeResult(ctx, result, "alipay", func(*ent.SiteOrder) string { return "PAGE" }, "TRADE_SUCCESS", "TRADE_FINISHED")
}

func orderSettled(o *ent.SiteOrder) bool {
	return o.Status == entsiteorder.StatusPaid ||
		o.Status == entsiteorder.StatusPartiallyRefunded ||
		o.Status == entsiteorder.StatusRefunded
}

// applyNativeResult books a verified payment notification or query result:
// it records the channel's payment details and, on a success state with the
// right amount, credits a pending order. Once an order is credited its
// payment details stay as they were: a late notice (a closed trade, a
// refund-finished trade) must not overwrite them.
func (s *FinanceSvc) applyNativeResult(ctx context.Context, result PaymentCallbackResult, channel string, tradeType func(*ent.SiteOrder) string, success ...string) (*ent.SiteOrder, error) {
	o, err := s.d.Ent.SiteOrder.Query().Where(entsiteorder.OrderNoEQ(result.OrderNo)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	if orderSettled(o) {
		if slices.Contains(success, result.TradeState) && result.TransactionID != "" && result.TransactionID != o.PaymentTransactionID {
			// The same order paid a second time (another channel's code, a
			// second scan): nothing is credited twice, staff refund it.
			slog.Warn("payment for an order that is already credited",
				"order_no", o.OrderNo, "channel", channel, "transaction_id", result.TransactionID,
				"credited_transaction_id", o.PaymentTransactionID, "amount_cents", result.AmountCents)
		}
		return o, nil
	}
	// Only while not credited: a success notice racing this one must not
	// have its details overwritten once it has credited the order.
	upd := s.d.Ent.SiteOrder.Update().
		Where(entsiteorder.IDEQ(o.ID), entsiteorder.StatusIn(entsiteorder.StatusPending, entsiteorder.StatusCanceled)).
		SetPaymentChannel(channel).
		SetPaymentTradeType(tradeType(o)).
		SetPaymentTradeState(result.TradeState).
		SetPaymentTransactionID(result.TransactionID).
		SetPaymentPayerOpenid(result.PayerOpenID)
	if result.AmountCents > 0 {
		upd = upd.SetPaymentAmountCents(result.AmountCents)
	}
	if _, err := upd.Save(ctx); err != nil {
		return nil, err
	}
	if !slices.Contains(success, result.TradeState) {
		return s.d.Ent.SiteOrder.Get(ctx, o.ID)
	}
	expected, err := s.paymentAmountCents(o.Amount)
	if err != nil {
		return nil, err
	}
	if result.AmountCents != expected {
		return nil, ErrPaymentAmountMismatch
	}
	if o.Status != entsiteorder.StatusPending {
		// Paid after the order was cancelled: the payment is on record for
		// staff to refund, but nothing is credited.
		return nil, ErrOrderInvalidState
	}
	paid, err := s.MarkOrderPaid(ctx, o.ID)
	if !errors.Is(err, ErrOrderInvalidState) {
		return paid, err
	}
	// A concurrent delivery may have credited this order after our read
	// but before MarkOrderPaid acquired its row lock.
	current, getErr := s.d.Ent.SiteOrder.Get(ctx, o.ID)
	if getErr != nil {
		return nil, getErr
	}
	if orderSettled(current) {
		return current, nil
	}
	return nil, err
}

func (s *FinanceSvc) paymentAmountCents(amount decimal.Decimal) (int64, error) {
	// Order amounts are points (1 point = 1 CNY); user-side checkout no
	// longer applies the exchange rate — payment fen = amount × 100.
	return orderAmountCents(amount)
}

func orderAmountCents(amount decimal.Decimal) (int64, error) {
	if amount.Sign() <= 0 {
		return 0, errors.New("payment amount must be positive")
	}
	cents := amount.Mul(decimal.NewFromInt(100)).Round(0)
	if cents.Sign() <= 0 {
		return 0, errors.New("payment amount must be at least one cent")
	}
	if cents.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, errors.New("payment amount too large")
	}
	return cents.IntPart(), nil
}

func normalizeOrderAmount(amount decimal.Decimal) (decimal.Decimal, error) {
	// Persist the exact decimal represented by the integer fen sent to the
	// payment provider. This also canonicalizes legacy high-precision amounts
	// before they are credited or become refundable.
	cents, err := orderAmountCents(amount)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromInt(cents).Shift(-2), nil
}

// orderDescription is the order title shown by the payment channel: the
// site name followed by what was bought.
func (s *FinanceSvc) orderDescription(ctx context.Context, o *ent.SiteOrder) string {
	what := "站内订单"
	switch o.Kind {
	case entsiteorder.KindPlan:
		what = "套餐订阅"
	case entsiteorder.KindBalance:
		what = "余额充值"
	}
	return s.siteName(ctx) + " " + what
}

// siteName is the configured site name, or the default one.
func (s *FinanceSvc) siteName(ctx context.Context) string {
	if s.settings != nil {
		if st, err := s.settings.GetSite(ctx); err == nil && strings.TrimSpace(st.SiteName) != "" {
			return strings.TrimSpace(st.SiteName)
		}
	}
	return siteDefaults.SiteName
}

func newOrderNo() string {
	return "SO" + time.Now().UTC().Format("20060102150405") + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}

type OrderFilter struct {
	Status string
	Kind   string
	UserID *uuid.UUID
	// Q is a free-text term matched against the order number and the
	// buyer's email / username / nickname. Admin-only in practice: the
	// user-facing listing is already scoped to one UserID.
	Q string
	// LoadBuyer eager-loads Edges.User so the caller can render who
	// placed the order. Off by default: /v1/me/orders is already scoped
	// to one user and would only pay for a pointless join.
	LoadBuyer bool
	// LoadRefunds eager-loads Edges.Refunds so the order list can show
	// refund status without a second round trip. Used by both admin
	// finance and the user order tab.
	LoadRefunds bool
	Limit       int
	Offset      int
}

func (s *FinanceSvc) ListOrders(ctx context.Context, f OrderFilter) ([]*ent.SiteOrder, int, error) {
	q := s.d.Ent.SiteOrder.Query()
	if term := strings.TrimSpace(f.Q); term != "" {
		q = q.Where(entsiteorder.Or(
			entsiteorder.OrderNoContainsFold(term),
			entsiteorder.HasUserWith(entuser.Or(
				entuser.EmailContainsFold(term),
				entuser.UsernameContainsFold(term),
				entuser.NicknameContainsFold(term),
			)),
		))
	}
	if f.Status != "" {
		q = q.Where(entsiteorder.StatusEQ(entsiteorder.Status(f.Status)))
	}
	if f.Kind != "" {
		q = q.Where(entsiteorder.KindEQ(entsiteorder.Kind(f.Kind)))
	}
	if f.UserID != nil {
		q = q.Where(entsiteorder.UserIDEQ(*f.UserID))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rowQ := q.Order(ent.Desc(entsiteorder.FieldCreatedAt)).
		Limit(f.Limit).
		Offset(f.Offset)
	if f.LoadBuyer {
		rowQ = rowQ.WithUser()
	}
	if f.LoadRefunds {
		rowQ = rowQ.WithRefunds()
	}
	rows, err := rowQ.All(ctx)
	return rows, total, err
}

func (s *FinanceSvc) MarkOrderPaid(ctx context.Context, id uuid.UUID) (*ent.SiteOrder, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	o, err := tx.SiteOrder.Query().
		Where(entsiteorder.IDEQ(id)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	if o.Status != entsiteorder.StatusPending {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	amount, err := normalizeOrderAmount(o.Amount)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	paidCents, err := s.paymentAmountCents(amount)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	now := time.Now()
	upd := tx.SiteOrder.Update().
		Where(entsiteorder.IDEQ(id), entsiteorder.StatusEQ(entsiteorder.StatusPending)).
		SetStatus(entsiteorder.StatusPaid).
		SetPaidAt(now).
		SetAmount(amount)
	// An order an admin marks paid never went through a payment channel,
	// so nothing else ever writes payment_amount_cents.
	upd = upd.SetPaymentAmountCents(paidCents)
	switch o.Kind {
	case entsiteorder.KindBalance:
		creditAmount := amount
		if o.CreditAmount.Sign() > 0 {
			creditAmount, err = normalizeOrderAmount(o.CreditAmount)
			if err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			upd = upd.SetCreditAmount(creditAmount)
		}
		u, err := tx.User.Query().Where(entuser.IDEQ(o.UserID)).ForUpdate().Only(ctx)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		newBal := u.Balance.Add(creditAmount)
		if _, err := tx.User.UpdateOneID(o.UserID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if _, err := tx.BalanceTx.Create().
			SetUserID(o.UserID).
			SetKind(entbalancetx.KindTopup).
			SetAmount(creditAmount).
			SetBalanceAfter(newBal).
			SetCurrency(CurrencyCNY).
			SetNote(siteOrderTopupNote + o.OrderNo).
			Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	case entsiteorder.KindPlan:
		if o.PlanID == nil {
			_ = tx.Rollback()
			return nil, errors.New("plan order requires plan_id")
		}
		plan, err := tx.Plan.Get(ctx, *o.PlanID)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		expire := now.AddDate(0, 0, plan.PeriodDays)
		if plan.PeriodDays <= 0 {
			expire = now.AddDate(0, 0, 90)
		}
		up, err := tx.UserPlan.Create().
			SetUserID(o.UserID).
			SetPlanID(*o.PlanID).
			SetStartAt(now).
			SetExpireAt(expire).
			Save(ctx)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		u, err := tx.User.Query().Where(entuser.IDEQ(o.UserID)).ForUpdate().Only(ctx)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if err := grantPlanInitialBalance(ctx, tx, u, plan, up.ID, "plan initial balance: "+o.OrderNo); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		upd = upd.SetUserPlanID(up.ID)
	}
	n, err := upd.Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if n != 1 {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	o, err = tx.SiteOrder.Get(ctx, id)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if s.agents != nil {
		if err := s.agents.recordCouponOrderTx(ctx, tx, o); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Both balance and plan payments count toward the signup threshold.
	// Percentage rebates remain limited to balance orders.
	if s.invites != nil {
		s.invites.EvaluateSignupAsync(o.UserID)
		if o.Kind == entsiteorder.KindBalance {
			s.invites.ProcessTopupRebateAsync(o.UserID, o.ID, o.Amount)
		}
	}
	return o, nil
}

func (s *FinanceSvc) CancelOrder(ctx context.Context, id uuid.UUID) (*ent.SiteOrder, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	o, err := tx.SiteOrder.Query().
		Where(entsiteorder.IDEQ(id)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	if o.Status != entsiteorder.StatusPending {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	n, err := tx.SiteOrder.Update().
		Where(entsiteorder.IDEQ(id), entsiteorder.StatusEQ(entsiteorder.StatusPending)).
		SetStatus(entsiteorder.StatusCanceled).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if n != 1 {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	o, err = tx.SiteOrder.Get(ctx, id)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return o, nil
}

type CreateRefundInput struct {
	OrderID uuid.UUID
	Amount  *decimal.Decimal
	Reason  string
	Note    string
}

func (s *FinanceSvc) CreateRefund(ctx context.Context, in CreateRefundInput) (*ent.SiteRefund, error) {
	o, err := s.d.Ent.SiteOrder.Get(ctx, in.OrderID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	if o.Status != entsiteorder.StatusPaid && o.Status != entsiteorder.StatusPartiallyRefunded {
		return nil, ErrOrderInvalidState
	}
	policy, err := s.GetRefundPolicy(ctx)
	if err != nil {
		return nil, err
	}
	var amount decimal.Decimal
	if in.Amount != nil {
		amount = *in.Amount
	} else {
		// Orders that put balance on the account default to what is
		// refundable now; only quota subscriptions use the time-based policy.
		ref, err := s.OrderRefundableFor(ctx, o)
		if err != nil {
			return nil, err
		}
		if ref.BalanceBearing {
			if ref.Cash.Sign() <= 0 {
				return nil, ErrNothingRefundable
			}
			amount = ref.Cash
		} else if amount, err = s.defaultRefundAmount(ctx, o, policy); err != nil {
			return nil, err
		}
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	locked, err := tx.SiteOrder.Query().Where(entsiteorder.IDEQ(o.ID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if locked.Status != entsiteorder.StatusPaid && locked.Status != entsiteorder.StatusPartiallyRefunded {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	pending, err := tx.SiteRefund.Query().
		Where(entsiterefund.OrderIDEQ(locked.ID), entsiterefund.StatusEQ(entsiterefund.StatusPending)).
		Exist(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if pending {
		_ = tx.Rollback()
		return nil, ErrRefundPending
	}
	remain, err := remainingRefundableOn(ctx, tx.SiteRefund, locked)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if amount.GreaterThan(remain) {
		amount = remain
	}
	if amount.Sign() <= 0 {
		_ = tx.Rollback()
		return nil, errors.New("refund amount must be positive")
	}
	b, _ := json.Marshal(policy)
	var snapshot map[string]any
	_ = json.Unmarshal(b, &snapshot)
	r, err := tx.SiteRefund.Create().
		SetOrderID(locked.ID).
		SetUserID(locked.UserID).
		SetAmount(amount).
		SetCurrency(locked.Currency).
		SetReason(in.Reason).
		SetNote(in.Note).
		SetPolicySnapshot(snapshot).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *FinanceSvc) defaultRefundAmount(ctx context.Context, o *ent.SiteOrder, policy RefundPolicy) (decimal.Decimal, error) {
	daysSinceBuy := int(time.Since(o.CreatedAt).Hours() / 24)
	switch o.Kind {
	case entsiteorder.KindBalance:
		if daysSinceBuy <= policy.BalanceNoReasonDays {
			return o.Amount, nil
		}
		return o.Amount.Mul(decimal.NewFromFloat(policy.BalanceRefundRatio)), nil
	case entsiteorder.KindPlan:
		if daysSinceBuy <= policy.PlanNoReasonDays {
			return o.Amount, nil
		}
		ratio := decimal.NewFromFloat(policy.PlanRefundRatio)
		if o.PlanID == nil || o.UserPlanID == nil {
			return o.Amount.Mul(ratio), nil
		}
		plan, err := s.d.Ent.Plan.Get(ctx, *o.PlanID)
		if err != nil {
			return decimal.Zero, err
		}
		up, err := s.d.Ent.UserPlan.Get(ctx, *o.UserPlanID)
		if err != nil {
			return decimal.Zero, err
		}
		totalDays := plan.PeriodDays
		if totalDays <= 0 {
			totalDays = int(up.ExpireAt.Sub(up.StartAt).Hours() / 24)
		}
		if totalDays <= 0 {
			return o.Amount.Mul(ratio), nil
		}
		remainingDays := int(time.Until(up.ExpireAt).Hours() / 24)
		if remainingDays < 0 {
			remainingDays = 0
		}
		return o.Amount.Mul(decimal.NewFromInt(int64(remainingDays))).Div(decimal.NewFromInt(int64(totalDays))).Mul(ratio), nil
	default:
		return decimal.Zero, nil
	}
}

func remainingRefundableOn(ctx context.Context, refunds *ent.SiteRefundClient, o *ent.SiteOrder) (decimal.Decimal, error) {
	rows, err := refunds.Query().
		Where(
			entsiterefund.OrderIDEQ(o.ID),
			entsiterefund.StatusIn(entsiterefund.StatusPending, entsiterefund.StatusApproved),
		).
		All(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	used := decimal.Zero
	for _, r := range rows {
		used = used.Add(r.Amount)
	}
	remain := o.Amount.Sub(used)
	if remain.Sign() < 0 {
		return decimal.Zero, nil
	}
	return remain, nil
}

type RefundFilter struct {
	Status string
	// OrderID restricts to one order. Used by the ticket-side "create
	// refund" action and by order-detail views.
	OrderID *uuid.UUID
	// Q mirrors OrderFilter.Q — a refund has no identifier of its own
	// that an operator would type, so the term is matched against the
	// refunded order's number and its buyer.
	Q string
	// LoadOrder eager-loads Edges.Order (and its buyer) so the caller can
	// show the order number and who asked for the refund.
	LoadOrder bool
	Limit     int
	Offset    int
}

func (s *FinanceSvc) ListRefunds(ctx context.Context, f RefundFilter) ([]*ent.SiteRefund, int, error) {
	q := s.d.Ent.SiteRefund.Query()
	if term := strings.TrimSpace(f.Q); term != "" {
		q = q.Where(entsiterefund.HasOrderWith(
			entsiteorder.Or(
				entsiteorder.OrderNoContainsFold(term),
				entsiteorder.HasUserWith(entuser.Or(
					entuser.EmailContainsFold(term),
					entuser.UsernameContainsFold(term),
					entuser.NicknameContainsFold(term),
				)),
			),
		))
	}
	if f.Status != "" {
		q = q.Where(entsiterefund.StatusEQ(entsiterefund.Status(f.Status)))
	}
	if f.OrderID != nil {
		q = q.Where(entsiterefund.OrderIDEQ(*f.OrderID))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rowQ := q.Order(ent.Desc(entsiterefund.FieldCreatedAt)).
		Limit(f.Limit).
		Offset(f.Offset)
	if f.LoadOrder {
		rowQ = rowQ.WithOrder(func(oq *ent.SiteOrderQuery) { oq.WithUser() })
	}
	rows, err := rowQ.All(ctx)
	return rows, total, err
}

// ApproveRefund pays out a pending refund. Orders paid through a channel
// with a refund gateway are refunded back through it (approveGatewayRefund);
// others are booked here and paid by hand.
func (s *FinanceSvc) ApproveRefund(ctx context.Context, id, reviewer uuid.UUID, note string) (*ent.SiteRefund, error) {
	if len(s.refundGateways) > 0 {
		r, err := s.d.Ent.SiteRefund.Query().Where(entsiterefund.IDEQ(id)).WithOrder().Only(ctx)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil, ErrRefundNotFound
			}
			return nil, err
		}
		if r.Edges.Order != nil {
			if gw := s.refundGateways[r.Edges.Order.PaymentChannel]; gw != nil {
				return s.approveGatewayRefund(ctx, gw, id, reviewer, note)
			}
		}
	}
	if err := s.checkBookedRefund(ctx, id); err != nil {
		return nil, err
	}
	return s.approveRefundBooked(ctx, id, reviewer, note)
}

func (s *FinanceSvc) approveRefundBooked(ctx context.Context, id, reviewer uuid.UUID, note string) (*ent.SiteRefund, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := tx.SiteRefund.Query().Where(entsiterefund.IDEQ(id)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrRefundNotFound
		}
		return nil, err
	}
	if r.Status != entsiterefund.StatusPending {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	o, err := tx.SiteOrder.Query().Where(entsiteorder.IDEQ(r.OrderID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if o.Status != entsiteorder.StatusPaid && o.Status != entsiteorder.StatusPartiallyRefunded {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	approvedBefore, err := sumApprovedRefundsTx(ctx, tx, o.ID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	switch o.Kind {
	case entsiteorder.KindBalance:
		// A discounted order refunds cash but revokes the proportional face
		// value that was credited. Cumulative targets make partial refunds
		// converge to the exact full credit without rounding drift.
		creditAmount := o.CreditAmount
		if creditAmount.Sign() <= 0 {
			creditAmount = o.Amount
		}
		beforeTarget := creditAmount.Mul(approvedBefore).Div(o.Amount).Truncate(8)
		approvedAfter := approvedBefore.Add(r.Amount)
		afterTarget := creditAmount
		if approvedAfter.LessThan(o.Amount) {
			afterTarget = creditAmount.Mul(approvedAfter).Div(o.Amount).Truncate(8)
		}
		amount := afterTarget.Sub(beforeTarget)
		u, err := tx.User.Query().Where(entuser.IDEQ(o.UserID)).ForUpdate().Only(ctx)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		newBal := u.Balance.Sub(amount)
		if _, err := tx.User.UpdateOneID(o.UserID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if _, err := tx.BalanceTx.Create().
			SetUserID(o.UserID).
			SetKind(entbalancetx.KindRefund).
			SetAmount(amount.Neg()).
			SetBalanceAfter(newBal).
			SetCurrency(CurrencyCNY).
			SetNote(siteOrderRefundNote + o.OrderNo).
			Save(ctx); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	case entsiteorder.KindPlan:
		if o.UserPlanID != nil {
			if _, err := tx.UserPlan.UpdateOneID(*o.UserPlanID).
				SetStatus(entuserplan.StatusCanceled).
				SetExpireAt(time.Now()).
				Save(ctx); err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			u, err := tx.User.Query().Where(entuser.IDEQ(o.UserID)).ForUpdate().Only(ctx)
			if err != nil {
				_ = tx.Rollback()
				return nil, err
			}
			if _, err := revokeRemainingPlanGift(
				ctx,
				tx,
				u,
				*o.UserPlanID,
				"plan gift revoked by refund: "+o.OrderNo,
			); err != nil {
				_ = tx.Rollback()
				return nil, err
			}
		}
	}
	now := time.Now()
	if note != "" {
		r.Note = strings.TrimSpace(r.Note + "\n" + note)
	}
	n, err := tx.SiteRefund.Update().
		Where(entsiterefund.IDEQ(id), entsiterefund.StatusEQ(entsiterefund.StatusPending)).
		SetStatus(entsiterefund.StatusApproved).
		SetReviewedByUserID(reviewer).
		SetReviewedAt(now).
		SetNote(r.Note).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if n != 1 {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	r, err = tx.SiteRefund.Get(ctx, id)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	approved, err := sumApprovedRefundsTx(ctx, tx, o.ID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	status := entsiteorder.StatusPartiallyRefunded
	if approved.GreaterThanOrEqual(o.Amount) {
		status = entsiteorder.StatusRefunded
	}
	n, err = tx.SiteOrder.Update().
		Where(
			entsiteorder.IDEQ(o.ID),
			entsiteorder.StatusIn(entsiteorder.StatusPaid, entsiteorder.StatusPartiallyRefunded),
		).
		SetStatus(status).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if n != 1 {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	if s.agents != nil {
		if err := s.agents.reconcileCouponRefundTx(ctx, tx, o.ID, r.ID, r.Amount, approved); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Invite reconciliation runs after the refund is durable and never
	// fails it: money already left the buyer's balance, and reporting an
	// error here would misdescribe what happened. Both halves are
	// idempotent and derived from `approved` (the CUMULATIVE approved
	// refund total), so a later partial refund re-reconciles to the right
	// figure rather than double-charging the inviter.
	if s.invites != nil {
		// Detached from the request context on purpose: the refund is
		// already durable, and an admin closing the tab must not be able to
		// skip the clawback (gin cancels Request.Context on disconnect).
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		s.invites.ReconcileRefund(rc, o.UserID, o.ID, approved)
		cancel()
	}
	s.resolveRefundTickets(ctx, o.ID)
	return r, nil
}

func sumApprovedRefundsTx(ctx context.Context, tx *ent.Tx, orderID uuid.UUID) (decimal.Decimal, error) {
	rows, err := tx.SiteRefund.Query().
		Where(entsiterefund.OrderIDEQ(orderID), entsiterefund.StatusEQ(entsiterefund.StatusApproved)).
		All(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, r := range rows {
		total = total.Add(r.Amount)
	}
	return total, nil
}

// RejectRefund turns down a pending refund. One already sent to a refund
// gateway cannot be rejected by hand: its money may have moved.
func (s *FinanceSvc) RejectRefund(ctx context.Context, id, reviewer uuid.UUID, note string) (*ent.SiteRefund, error) {
	r, err := s.d.Ent.SiteRefund.Query().Where(entsiterefund.IDEQ(id)).WithOrder().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrRefundNotFound
		}
		return nil, err
	}
	if sentToGateway(r) ||
		(r.Reason == selfRefundReason && r.Edges.Order != nil && s.refundGateways[r.Edges.Order.PaymentChannel] != nil) {
		return nil, ErrRefundManaged
	}
	return s.rejectRefundRow(ctx, id, reviewer, note)
}

func (s *FinanceSvc) rejectRefundRow(ctx context.Context, id, reviewer uuid.UUID, note string) (*ent.SiteRefund, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := tx.SiteRefund.Query().
		Where(entsiterefund.IDEQ(id)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrRefundNotFound
		}
		return nil, err
	}
	if r.Status != entsiterefund.StatusPending {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	n, err := tx.SiteRefund.Update().
		Where(entsiterefund.IDEQ(id), entsiterefund.StatusEQ(entsiterefund.StatusPending)).
		SetStatus(entsiterefund.StatusRejected).
		SetReviewedByUserID(reviewer).
		SetReviewedAt(time.Now()).
		SetNote(note).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if n != 1 {
		_ = tx.Rollback()
		return nil, ErrOrderInvalidState
	}
	r, err = tx.SiteRefund.Get(ctx, id)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// ErrLegacyProviderCostReadOnly is returned when an operator tries to delete a
// manual cost that lives in the retired provider_costs table. That table is
// READ-ONLY history: new finance rows are written against upstream_account_costs
// and pre-migration rows are only ever read (and fused by account) for reports.
var ErrLegacyProviderCostReadOnly = errors.New("manual cost predates the account migration and is read-only")

type CreateAccountCostInput struct {
	AccountID  uuid.UUID
	Amount     decimal.Decimal
	Currency   string
	OccurredAt time.Time
	Note       string
}

// CreateAccountCost records one manual account cost (rental, proxy fix, …)
// against an ACCOUNT. This is the only manual-cost writer: the account tables
// are the live ones.
func (s *FinanceSvc) CreateAccountCost(ctx context.Context, in CreateAccountCostInput) (*ent.UpstreamAccountCost, error) {
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now()
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = CurrencyCNY
	}
	return s.d.Ent.UpstreamAccountCost.Create().
		SetAccountID(in.AccountID).
		SetAmount(in.Amount).
		SetCurrency(currency).
		SetOccurredAt(in.OccurredAt).
		SetNote(in.Note).
		Save(ctx)
}

// AccountCostRow is one manual cost as the finance page sees it: the live
// account rows and the READ-ONLY legacy channel rows, fused under one shape.
// Legacy marks a row that only exists in provider_costs — its channel was never
// migrated to an account, so AccountID holds the legacy channel id.
type AccountCostRow struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	// Amount / Currency / OccurredAt / Note / CreatedAt mirror the source row;
	// an Amount already in CNY is what the page renders.
	Amount                decimal.Decimal
	Currency              string
	OccurredAt            time.Time
	Note                  string
	CreatedAt             time.Time
	LegacyProviderHistory bool
}

// ListAccountCosts returns manual costs for one account (all accounts when
// accountID is nil), newest first, fusing both tables by account. Paging is done
// after the fusion so a page can mix live and legacy rows in occurrence order;
// each side is bounded by offset+limit, which is enough to contain the fused
// top-N because both sides are ordered by the same key.
func (s *FinanceSvc) ListAccountCosts(ctx context.Context, accountID *uuid.UUID, from, to time.Time, limit, offset int) ([]AccountCostRow, int, error) {
	if limit <= 0 {
		limit = 50
	}
	window := limit + offset

	q := s.d.Ent.UpstreamAccountCost.Query()
	if accountID != nil {
		q = q.Where(entaccountcost.AccountIDEQ(*accountID))
	}
	if !from.IsZero() {
		q = q.Where(entaccountcost.OccurredAtGTE(from))
	}
	if !to.IsZero() {
		q = q.Where(entaccountcost.OccurredAtLTE(to))
	}
	accountTotal, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	accountRows, err := q.Order(ent.Desc(entaccountcost.FieldOccurredAt)).
		Limit(window).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}

	// Legacy rows: provider_costs is history, so it is only read. A row whose
	// channel was migrated is reported under that account; one whose channel was
	// never migrated keeps its own dimension (Legacy).
	nameByAccount, legacyProviderIDs, err := s.accountNames(ctx)
	if err != nil {
		return nil, 0, err
	}
	legacyQ := s.d.Ent.ProviderCost.Query()
	if !from.IsZero() {
		legacyQ = legacyQ.Where(entprovidercost.OccurredAtGTE(from))
	}
	if !to.IsZero() {
		legacyQ = legacyQ.Where(entprovidercost.OccurredAtLTE(to))
	}
	if accountID != nil {
		// A legacy row belongs to the requested account when it was charged to
		// that account's channel.
		providerIDs := []uuid.UUID{}
		for providerID, acctID := range legacyProviderIDs {
			if acctID == *accountID {
				providerIDs = append(providerIDs, providerID)
			}
		}
		if _, ok := nameByAccount[*accountID]; !ok {
			// Unknown account id: keep the legacy dimension so an operator can
			// still see pre-migration rows for a channel that was never migrated.
			providerIDs = append(providerIDs, *accountID)
		}
		if len(providerIDs) == 0 {
			return s.pageAccountCosts(accountRows, accountTotal, offset, limit)
		}
		legacyQ = legacyQ.Where(entprovidercost.ProviderIDIn(providerIDs...))
	}
	legacyTotal, err := legacyQ.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	legacyRows, err := legacyQ.Order(ent.Desc(entprovidercost.FieldOccurredAt)).
		Limit(window).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}

	fused := make([]AccountCostRow, 0, len(accountRows)+len(legacyRows))
	for _, row := range accountRows {
		fused = append(fused, AccountCostRow{
			ID: row.ID, AccountID: row.AccountID, Amount: row.Amount, Currency: row.Currency,
			OccurredAt: row.OccurredAt, Note: row.Note, CreatedAt: row.CreatedAt,
		})
	}
	for _, row := range legacyRows {
		// Fold the legacy channel onto its account when one exists, so the two
		// halves of an account's history land in the same list position.
		fused = append(fused, AccountCostRow{
			ID: row.ID, AccountID: row.ProviderID, Amount: row.Amount, Currency: row.Currency,
			OccurredAt: row.OccurredAt, Note: row.Note, CreatedAt: row.CreatedAt,
			LegacyProviderHistory: true,
		})
		if acctID, ok := legacyProviderIDs[row.ProviderID]; ok {
			fused[len(fused)-1].AccountID = acctID
			fused[len(fused)-1].LegacyProviderHistory = false
		}
	}
	sort.SliceStable(fused, func(i, j int) bool {
		return fused[i].OccurredAt.After(fused[j].OccurredAt)
	})
	if offset > len(fused) {
		offset = len(fused)
	}
	end := offset + limit
	if end > len(fused) {
		end = len(fused)
	}
	return fused[offset:end], accountTotal + legacyTotal, nil
}

// pageAccountCosts returns the account-table page alone, used when the requested
// account has no legacy channel to fuse with.
func (s *FinanceSvc) pageAccountCosts(rows []*ent.UpstreamAccountCost, total, offset, limit int) ([]AccountCostRow, int, error) {
	fused := make([]AccountCostRow, 0, len(rows))
	for _, row := range rows {
		fused = append(fused, AccountCostRow{
			ID: row.ID, AccountID: row.AccountID, Amount: row.Amount, Currency: row.Currency,
			OccurredAt: row.OccurredAt, Note: row.Note, CreatedAt: row.CreatedAt,
		})
	}
	if offset > len(fused) {
		offset = len(fused)
	}
	end := offset + limit
	if end > len(fused) {
		end = len(fused)
	}
	return fused[offset:end], total, nil
}

// accountNames loads every account (including soft-deleted ones, so history
// keeps its label) plus the legacy provider_id → account_id map the fusion needs.
func (s *FinanceSvc) accountNames(ctx context.Context) (map[uuid.UUID]string, map[uuid.UUID]uuid.UUID, error) {
	accounts, err := s.d.Ent.UpstreamAccount.Query().All(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[uuid.UUID]string, len(accounts))
	byLegacyProvider := make(map[uuid.UUID]uuid.UUID, len(accounts))
	for _, acct := range accounts {
		names[acct.ID] = acct.Name
		if acct.LegacyProviderID != nil {
			byLegacyProvider[*acct.LegacyProviderID] = acct.ID
		}
	}
	return names, byLegacyProvider, nil
}

// DeleteAccountCost removes one manual cost from the ACCOUNT table. Legacy
// provider_costs rows are read-only history: they are reported but never
// written, so deleting one reports ErrLegacyProviderCostReadOnly instead of
// silently editing the retired table.
func (s *FinanceSvc) DeleteAccountCost(ctx context.Context, id uuid.UUID) error {
	err := s.d.Ent.UpstreamAccountCost.DeleteOneID(id).Exec(ctx)
	if err == nil {
		return nil
	}
	if !ent.IsNotFound(err) {
		return err
	}
	legacy, legacyErr := s.d.Ent.ProviderCost.Query().Where(entprovidercost.IDEQ(id)).Exist(ctx)
	if legacyErr != nil {
		return legacyErr
	}
	if legacy {
		return ErrLegacyProviderCostReadOnly
	}
	return err
}

// AccountSettlementRow is one month of one account's cost reconciliation.
//
// The dimension is the ACCOUNT. The saved confirmation is read from
// upstream_account_settlements and, for a month confirmed before the migration,
// from the read-only provider_settlements row of the account's legacy channel
// (fused by account). Usage and manual cost details are derived from both
// generations of source rows, fused the same way.
type AccountSettlementRow struct {
	// AccountID is the account id; when LegacyProviderHistory is set the account
	// was never migrated and the value is its legacy channel id.
	AccountID   uuid.UUID `json:"account_id"`
	AccountName string    `json:"account_name"`
	// LegacyProviderHistory marks a row that exists only in the pre-migration
	// provider_settlements history.
	LegacyProviderHistory bool                             `json:"legacy_provider_history,omitempty"`
	PeriodStart           time.Time                        `json:"period_start"`
	PeriodEnd             time.Time                        `json:"period_end"`
	CalculatedCost        decimal.Decimal                  `json:"calculated_cost"`
	ActualCost            decimal.Decimal                  `json:"actual_cost"`
	CorrectionDelta       decimal.Decimal                  `json:"correction_delta"`
	Currency              string                           `json:"currency"`
	Status                string                           `json:"status"`
	ConfirmedByUserID     *uuid.UUID                       `json:"confirmed_by_user_id,omitempty"`
	ConfirmedAt           *time.Time                       `json:"confirmed_at,omitempty"`
	Note                  string                           `json:"note"`
	UsageDetails          []AccountSettlementUsageDetail   `json:"usage_details"`
	ManualCostDetails     []AccountSettlementManualCostRow `json:"manual_cost_details"`
	UsageCost             decimal.Decimal                  `json:"usage_cost"`
	ManualCost            decimal.Decimal                  `json:"manual_cost"`
}

type AccountSettlementUsageDetail struct {
	GroupID               string          `json:"group_id"`
	GroupName             string          `json:"group_name"`
	Model                 string          `json:"model"`
	Requests              int64           `json:"requests"`
	TokensIn              int64           `json:"tokens_in"`
	TokensOut             int64           `json:"tokens_out"`
	CachedTokensIn        int64           `json:"cached_tokens_in"`
	CacheCreationTokensIn int64           `json:"cache_creation_tokens_in"`
	OutputImages          int64           `json:"output_images"`
	Cost                  decimal.Decimal `json:"cost"`
}

type AccountSettlementManualCostRow struct {
	ID         uuid.UUID       `json:"id"`
	Amount     decimal.Decimal `json:"amount"`
	Currency   string          `json:"currency"`
	AmountCNY  decimal.Decimal `json:"amount_cny"`
	OccurredAt time.Time       `json:"occurred_at"`
	Note       string          `json:"note"`
	// LegacyProviderHistory marks a manual cost read from provider_costs.
	LegacyProviderHistory bool `json:"legacy_provider_history,omitempty"`
}

type ConfirmAccountSettlementInput struct {
	AccountID  uuid.UUID
	Month      time.Time
	ActualCost *decimal.Decimal
	ActorID    uuid.UUID
	Note       string
}

func MonthRange(month time.Time) (time.Time, time.Time) {
	if month.IsZero() {
		month = time.Now().UTC()
	}
	loc := month.Location()
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, loc)
	return from, from.AddDate(0, 1, 0)
}

// ListAccountSettlements builds the monthly 对账 report keyed by account.
//
// Two generations of rows feed it:
//
//   - the account tables (upstream_account_settlements / upstream_account_costs
//     and usage_logs.provider_account_id) for everything after the migration;
//   - the retired provider tables (provider_settlements / provider_costs /
//     usage_logs.provider_id) for everything before it, folded onto the account
//     migrated from that channel.
//
// An account whose channel was never migrated has nothing to fold into and is
// still reported (LegacyProviderHistory) when it has a saved settlement row for
// the month, so a confirmed pre-migration month cannot silently disappear.
func (s *FinanceSvc) ListAccountSettlements(ctx context.Context, month time.Time) ([]AccountSettlementRow, error) {
	from, to := MonthRange(month)
	accounts, err := s.d.Ent.UpstreamAccount.Query().
		Order(ent.Asc(entaccount.FieldName)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	nameByAccount := make(map[uuid.UUID]string, len(accounts))
	accountByLegacyProvider := make(map[uuid.UUID]*ent.UpstreamAccount, len(accounts))
	for _, acct := range accounts {
		nameByAccount[acct.ID] = acct.Name
		if acct.LegacyProviderID != nil {
			accountByLegacyProvider[*acct.LegacyProviderID] = acct
		}
	}

	// Saved confirmations, both generations. The account table wins when both
	// exist for the same account+month: it is the live one, and a re-confirm
	// after the migration writes there.
	savedAccounts, err := s.d.Ent.UpstreamAccountSettlement.Query().
		Where(entaccountsettlement.PeriodStartEQ(from)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	savedByAccount := make(map[uuid.UUID]*ent.UpstreamAccountSettlement, len(savedAccounts))
	for _, row := range savedAccounts {
		savedByAccount[row.AccountID] = row
	}
	savedLegacy, err := s.d.Ent.ProviderSettlement.Query().
		Where(entprovidersettlement.PeriodStartEQ(from)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	savedLegacyByProvider := make(map[uuid.UUID]*ent.ProviderSettlement, len(savedLegacy))
	for _, row := range savedLegacy {
		savedLegacyByProvider[row.ProviderID] = row
	}

	out := make([]AccountSettlementRow, 0, len(accounts))
	for _, acct := range accounts {
		row, err := s.accountSettlementRow(ctx, acct, from, to, savedByAccount[acct.ID], s.legacySettlementFor(acct, savedLegacyByProvider))
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	// Confirmed pre-migration months whose channel was never migrated.
	legacyOnly := make([]uuid.UUID, 0, len(savedLegacyByProvider))
	for providerID := range savedLegacyByProvider {
		if _, ok := accountByLegacyProvider[providerID]; ok {
			continue
		}
		legacyOnly = append(legacyOnly, providerID)
	}
	if len(legacyOnly) == 0 {
		return out, nil
	}
	legacyNames, err := s.legacyProviderNames(ctx, legacyOnly)
	if err != nil {
		return nil, err
	}
	sort.Slice(legacyOnly, func(i, j int) bool { return legacyNames[legacyOnly[i]] < legacyNames[legacyOnly[j]] })
	for _, providerID := range legacyOnly {
		saved := savedLegacyByProvider[providerID]
		calculated := saved.CalculatedCost
		out = append(out, AccountSettlementRow{
			AccountID:             providerID,
			AccountName:           legacyNames[providerID],
			LegacyProviderHistory: true,
			PeriodStart:           from,
			PeriodEnd:             to,
			CalculatedCost:        calculated,
			ActualCost:            saved.ActualCost,
			CorrectionDelta:       saved.ActualCost.Sub(calculated),
			Currency:              CurrencyCNY,
			Status:                "confirmed",
			ConfirmedByUserID:     saved.ConfirmedByUserID,
			ConfirmedAt:           saved.ConfirmedAt,
			Note:                  saved.Note,
			UsageDetails:          []AccountSettlementUsageDetail{},
			ManualCostDetails:     []AccountSettlementManualCostRow{},
		})
	}
	return out, nil
}

// legacySettlementFor returns the pre-migration confirmation of an account's
// channel, if that month was confirmed before the account existed.
func (s *FinanceSvc) legacySettlementFor(acct *ent.UpstreamAccount, saved map[uuid.UUID]*ent.ProviderSettlement) *ent.ProviderSettlement {
	if acct.LegacyProviderID == nil {
		return nil
	}
	return saved[*acct.LegacyProviderID]
}

func (s *FinanceSvc) legacyProviderNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := s.d.Ent.Provider.Query().Where(entprovider.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, row := range rows {
		out[row.ID] = row.Name
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			out[id] = ""
		}
	}
	return out, nil
}

func (s *FinanceSvc) accountSettlementRow(ctx context.Context, acct *ent.UpstreamAccount, from, to time.Time, saved *ent.UpstreamAccountSettlement, legacy *ent.ProviderSettlement) (AccountSettlementRow, error) {
	usageDetails, usageCost, err := s.accountSettlementUsageDetails(ctx, acct, from, to)
	if err != nil {
		return AccountSettlementRow{}, err
	}
	manualDetails, manualCost, err := s.accountSettlementManualDetails(ctx, acct, from, to)
	if err != nil {
		return AccountSettlementRow{}, err
	}
	calculated := usageCost.Add(manualCost)
	actual := calculated
	status := "pending"
	var confirmedBy *uuid.UUID
	var confirmedAt *time.Time
	note := ""
	switch {
	case saved != nil:
		actual = saved.ActualCost
		status = "confirmed"
		confirmedBy = saved.ConfirmedByUserID
		confirmedAt = saved.ConfirmedAt
		note = saved.Note
	case legacy != nil:
		// Confirmed before the migration; the account table has no row for this
		// month yet, so the historical confirmation is what the report shows.
		actual = legacy.ActualCost
		status = "confirmed"
		confirmedBy = legacy.ConfirmedByUserID
		confirmedAt = legacy.ConfirmedAt
		note = legacy.Note
	}
	return AccountSettlementRow{
		AccountID:         acct.ID,
		AccountName:       acct.Name,
		PeriodStart:       from,
		PeriodEnd:         to,
		CalculatedCost:    calculated,
		ActualCost:        actual,
		CorrectionDelta:   actual.Sub(calculated),
		Currency:          CurrencyCNY,
		Status:            status,
		ConfirmedByUserID: confirmedBy,
		ConfirmedAt:       confirmedAt,
		Note:              note,
		UsageDetails:      usageDetails,
		ManualCostDetails: manualDetails,
		UsageCost:         usageCost,
		ManualCost:        manualCost,
	}, nil
}

// ConfirmAccountSettlement writes the operator's actual cost for one month
// against the ACCOUNT table. Pre-migration confirmations stay in the read-only
// provider_settlements table; re-confirming such a month writes the account row
// (which then wins in the report) and leaves the historical one untouched.
func (s *FinanceSvc) ConfirmAccountSettlement(ctx context.Context, in ConfirmAccountSettlementInput) (*ent.UpstreamAccountSettlement, error) {
	acct, err := s.d.Ent.UpstreamAccount.Get(ctx, in.AccountID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}
	from, to := MonthRange(in.Month)
	details, err := s.ListAccountSettlements(ctx, from)
	if err != nil {
		return nil, err
	}
	calculated := decimal.Zero
	found := false
	for _, row := range details {
		if row.AccountID == acct.ID && !row.LegacyProviderHistory {
			calculated = row.CalculatedCost
			found = true
			break
		}
	}
	if !found {
		return nil, ErrAccountNotFound
	}
	actual := calculated
	if in.ActualCost != nil {
		actual = *in.ActualCost
	}
	now := time.Now()
	existing, err := s.d.Ent.UpstreamAccountSettlement.Query().
		Where(
			entaccountsettlement.AccountIDEQ(acct.ID),
			entaccountsettlement.PeriodStartEQ(from),
		).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, err
	}
	if existing != nil {
		return s.d.Ent.UpstreamAccountSettlement.UpdateOneID(existing.ID).
			SetPeriodEnd(to).
			SetCalculatedCost(calculated).
			SetActualCost(actual).
			SetCurrency(CurrencyCNY).
			SetConfirmedByUserID(in.ActorID).
			SetConfirmedAt(now).
			SetNote(in.Note).
			Save(ctx)
	}
	return s.d.Ent.UpstreamAccountSettlement.Create().
		SetAccountID(acct.ID).
		SetPeriodStart(from).
		SetPeriodEnd(to).
		SetCalculatedCost(calculated).
		SetActualCost(actual).
		SetCurrency(CurrencyCNY).
		SetConfirmedByUserID(in.ActorID).
		SetConfirmedAt(now).
		SetNote(in.Note).
		Save(ctx)
}

// accountSettlementUsageDetails totals one account's usage for the window,
// fusing its whole history: rows attributed to the account by
// usage_logs.provider_account_id, plus pre-migration rows that only carry the
// channel id the account was migrated from. Pricing follows the same rule as the
// finance page (see fusedUsageCostExpr): the account's cost basis for current
// rows, the legacy channel basis for rows that predate the migration.
func (s *FinanceSvc) accountSettlementUsageDetails(ctx context.Context, acct *ent.UpstreamAccount, from, to time.Time) ([]AccountSettlementUsageDetail, decimal.Decimal, error) {
	query := fmt.Sprintf(`
		SELECT
			COALESCE(ul.group_id::text, ''),
			COALESCE(mg.name, ''),
			ul.model,
			COUNT(*),
			COALESCE(SUM(ul.tokens_in + ul.audio_tokens_in), 0),
			COALESCE(SUM(ul.tokens_out + ul.audio_tokens_out), 0),
			COALESCE(SUM(ul.cached_tokens_in), 0),
			COALESCE(SUM(ul.cache_creation_tokens_in), 0),
			COALESCE(SUM(ul.output_images), 0),
			COALESCE(NULLIF(UPPER(am.currency), ''), NULLIF(UPPER(pm.currency), ''), 'CNY'),
			COALESCE(SUM(%s), 0)
		FROM usage_logs ul
		%s
		LEFT JOIN model_groups mg ON mg.id = ul.group_id
		WHERE $1::uuid IN (ul.provider_account_id, legacy_acct.id)
		  AND ul.created_at >= $2 AND ul.created_at < $3
		GROUP BY COALESCE(ul.group_id::text, ''), COALESCE(mg.name, ''), ul.model,
		         COALESCE(NULLIF(UPPER(am.currency), ''), NULLIF(UPPER(pm.currency), ''), 'CNY')
		ORDER BY COALESCE(mg.name, ''), ul.model
	`, fusedUsageCostExpr(), usageAccountJoin)
	rows, err := s.d.DB().QueryContext(ctx, query, acct.ID, from, to)
	if err != nil {
		return nil, decimal.Zero, err
	}
	defer rows.Close()

	rate := s.cnyRate()
	total := decimal.Zero
	out := []AccountSettlementUsageDetail{}
	for rows.Next() {
		var detail AccountSettlementUsageDetail
		var currency string
		var cost decimal.Decimal
		if err := rows.Scan(
			&detail.GroupID,
			&detail.GroupName,
			&detail.Model,
			&detail.Requests,
			&detail.TokensIn,
			&detail.TokensOut,
			&detail.CachedTokensIn,
			&detail.CacheCreationTokensIn,
			&detail.OutputImages,
			&currency,
			&cost,
		); err != nil {
			return nil, decimal.Zero, err
		}
		// Channel-settlement ONLY: usage costs may be recorded in USD (the
		// price tables' currency column); the exchange rate converts them to CNY
		// at settlement time. This is the legitimate use of ToCNY.
		detail.Cost = ToCNY(cost, currency, rate)
		total = total.Add(detail.Cost)
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, decimal.Zero, err
	}
	return out, total, nil
}

// AccountModelUsageRow is one (model, upstream_model) slice of an account's
// traffic for a window. Cost is the account cost basis (what the account's
// binding prices say we owe the upstream); Revenue is what users were
// actually charged for the same rows (usage_logs.cost_final).
type AccountModelUsageRow struct {
	Model                 string          `json:"model"`
	UpstreamModel         string          `json:"upstream_model"`
	Requests              int64           `json:"requests"`
	TokensIn              int64           `json:"tokens_in"`
	TokensOut             int64           `json:"tokens_out"`
	CachedTokensIn        int64           `json:"cached_tokens_in"`
	CacheCreationTokensIn int64           `json:"cache_creation_tokens_in"`
	OutputImages          int64           `json:"output_images"`
	Cost                  decimal.Decimal `json:"cost"`
	Revenue               decimal.Decimal `json:"revenue"`
}

// AccountModelUsage totals one account's usage per (model, upstream_model)
// for a window — the account detail page's 用量 view. Same attribution and
// pricing rules as accountSettlementUsageDetails (direct provider_account_id
// plus pre-migration channel rows, fused cost basis, CNY); only the grouping
// differs.
func (s *FinanceSvc) AccountModelUsage(ctx context.Context, acct *ent.UpstreamAccount, from, to time.Time) ([]AccountModelUsageRow, error) {
	costExpr := fusedUsageCostExpr()
	query := fmt.Sprintf(`
		SELECT
			ul.model,
			COALESCE(ul.upstream_model, ''),
			COUNT(*),
			COALESCE(SUM(ul.tokens_in + ul.audio_tokens_in), 0),
			COALESCE(SUM(ul.tokens_out + ul.audio_tokens_out), 0),
			COALESCE(SUM(ul.cached_tokens_in), 0),
			COALESCE(SUM(ul.cache_creation_tokens_in), 0),
			COALESCE(SUM(ul.output_images), 0),
			COALESCE(NULLIF(UPPER(am.currency), ''), NULLIF(UPPER(pm.currency), ''), 'CNY'),
			COALESCE(SUM(%s), 0),
			COALESCE(SUM(ul.cost_final), 0)
		FROM usage_logs ul
		%s
		WHERE $1::uuid IN (ul.provider_account_id, legacy_acct.id)
		  AND ul.created_at >= $2 AND ul.created_at < $3
		GROUP BY ul.model, COALESCE(ul.upstream_model, ''),
		         COALESCE(NULLIF(UPPER(am.currency), ''), NULLIF(UPPER(pm.currency), ''), 'CNY')
		ORDER BY SUM(ul.cost_final) DESC, ul.model
	`, costExpr, usageAccountJoin)
	rows, err := s.d.DB().QueryContext(ctx, query, acct.ID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rate := s.cnyRate()
	out := []AccountModelUsageRow{}
	for rows.Next() {
		var row AccountModelUsageRow
		var currency string
		var cost decimal.Decimal
		if err := rows.Scan(
			&row.Model,
			&row.UpstreamModel,
			&row.Requests,
			&row.TokensIn,
			&row.TokensOut,
			&row.CachedTokensIn,
			&row.CacheCreationTokensIn,
			&row.OutputImages,
			&currency,
			&cost,
			&row.Revenue,
		); err != nil {
			return nil, err
		}
		// Same rule as settlement: the price-table currency converts to CNY.
		row.Cost = ToCNY(cost, currency, rate)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// accountSettlementManualDetails fuses the account's manual costs from the live
// account table and the read-only legacy channel table.
func (s *FinanceSvc) accountSettlementManualDetails(ctx context.Context, acct *ent.UpstreamAccount, from, to time.Time) ([]AccountSettlementManualCostRow, decimal.Decimal, error) {
	rows, err := s.d.Ent.UpstreamAccountCost.Query().
		Where(
			entaccountcost.AccountIDEQ(acct.ID),
			entaccountcost.OccurredAtGTE(from),
			entaccountcost.OccurredAtLT(to),
		).
		Order(ent.Asc(entaccountcost.FieldOccurredAt)).
		All(ctx)
	if err != nil {
		return nil, decimal.Zero, err
	}
	out := make([]AccountSettlementManualCostRow, 0, len(rows))
	rate := s.cnyRate()
	total := decimal.Zero
	for _, row := range rows {
		amountCNY := ToCNY(row.Amount, row.Currency, rate)
		total = total.Add(amountCNY)
		out = append(out, AccountSettlementManualCostRow{
			ID:         row.ID,
			Amount:     row.Amount,
			Currency:   row.Currency,
			AmountCNY:  amountCNY,
			OccurredAt: row.OccurredAt,
			Note:       row.Note,
		})
	}
	if acct.LegacyProviderID == nil {
		return out, total, nil
	}
	legacyRows, err := s.d.Ent.ProviderCost.Query().
		Where(
			entprovidercost.ProviderIDEQ(*acct.LegacyProviderID),
			entprovidercost.OccurredAtGTE(from),
			entprovidercost.OccurredAtLT(to),
		).
		Order(ent.Asc(entprovidercost.FieldOccurredAt)).
		All(ctx)
	if err != nil {
		return nil, decimal.Zero, err
	}
	for _, row := range legacyRows {
		// Channel-settlement ONLY: manually entered channel costs may be in USD;
		// converted to CNY via the exchange rate at settlement.
		amountCNY := ToCNY(row.Amount, row.Currency, rate)
		total = total.Add(amountCNY)
		out = append(out, AccountSettlementManualCostRow{
			ID:                    row.ID,
			Amount:                row.Amount,
			Currency:              row.Currency,
			AmountCNY:             amountCNY,
			OccurredAt:            row.OccurredAt,
			Note:                  row.Note,
			LegacyProviderHistory: true,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, total, nil
}
