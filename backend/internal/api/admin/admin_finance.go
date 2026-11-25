package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entsiterefund "github.com/jucode/jucode/internal/data/ent/siterefund"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) financeSummary(c *gin.Context) {
	from, to := parseTimeRange(c)
	out, err := h.finance.Summary(c.Request.Context(), from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// The breakdown dimension is the account now. The outer key stays
	// "providers" and each row keeps provider_* aliases so the existing
	// finance page keeps rendering while it migrates.
	providers := make([]gin.H, 0, len(out.AccountBreakdown))
	for _, p := range out.AccountBreakdown {
		row := gin.H{
			"account_id":    p.AccountID.String(),
			"account_name":  p.AccountName,
			"provider_id":   p.AccountID.String(),
			"provider_name": p.AccountName,
			"fixed_cost":    p.FixedCost.String(),
			"manual_cost":   p.ManualCost.String(),
			"total_cost":    p.TotalCost.String(),
		}
		if p.LegacyProviderHistory {
			row["legacy_provider_history"] = true
		}
		providers = append(providers, row)
	}
	days := make([]gin.H, 0, len(out.Days))
	for _, d := range out.Days {
		days = append(days, gin.H{
			"date":    d.Date,
			"income":  d.Income.String(),
			"expense": d.Expense.String(),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"from":         out.From,
		"to":           out.To,
		"agent_income": out.AgentIncome.String(),
		"site_income":  out.SiteIncome.String(),
		"total_income": out.TotalIncome.String(),
		"fixed_cost":   out.FixedCost.String(),
		"manual_cost":  out.ManualCost.String(),
		"total_cost":   out.TotalCost.String(),
		"net_profit":   out.NetProfit.String(),
		"providers":    providers,
		"days":         days,
	})
}

func parseTimeRange(c *gin.Context) (time.Time, time.Time) {
	from, fromDate := parseTimeQuery(c.Query("from"))
	to, toDate := parseTimeQuery(c.Query("to"))
	return biz.NormalizeFinanceRange(from, to, fromDate, toDate)
}

func parseTimeQuery(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, false
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", v, biz.Location()); err == nil {
		return t, false
	}
	if t, err := time.ParseInLocation("2006-01-02", v, biz.Location()); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func parseMonthQuery(c *gin.Context) time.Time {
	raw := strings.TrimSpace(c.Query("month"))
	if raw == "" {
		return time.Now().UTC()
	}
	if t, err := time.Parse("2006-01", raw); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func (h *Handler) getRefundPolicy(c *gin.Context) {
	p, err := h.finance.GetRefundPolicy(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) updateRefundPolicy(c *gin.Context) {
	var req biz.RefundPolicy
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p, err := h.finance.SetRefundPolicy(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, p)
}

type createProviderCostReq struct {
	// AccountID is the live key; ProviderID is the legacy alias kept so the
	// existing finance page keeps posting unchanged.
	AccountID  string `json:"account_id"`
	ProviderID string `json:"provider_id"`
	Amount     string `json:"amount" binding:"required"`
	Currency   string `json:"currency"`
	OccurredAt string `json:"occurred_at"`
	Note       string `json:"note"`
}

// accountIDFromCostReq accepts the account id under either its new key
// (account_id) or the legacy provider_id alias.
func accountIDFromCostReq(req createProviderCostReq) (uuid.UUID, bool) {
	raw := strings.TrimSpace(req.AccountID)
	if raw == "" {
		raw = strings.TrimSpace(req.ProviderID)
	}
	if raw == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) listProviderCosts(c *gin.Context) {
	var accountID *uuid.UUID
	raw := strings.TrimSpace(c.Query("account_id"))
	if raw == "" {
		raw = strings.TrimSpace(c.Query("provider_id"))
	}
	if raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
			return
		}
		accountID = &id
	}
	from, to := parseTimeRange(c)
	limit, offset := parsePage(c, 500)
	rows, total, err := h.finance.ListAccountCosts(c.Request.Context(), accountID, from, to, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		row := gin.H{
			"id":          r.ID.String(),
			"account_id":  r.AccountID.String(),
			"provider_id": r.AccountID.String(),
			"amount":      r.Amount.String(),
			"currency":    r.Currency,
			"occurred_at": r.OccurredAt,
			"note":        r.Note,
			"created_at":  r.CreatedAt,
		}
		if r.LegacyProviderHistory {
			row["legacy_provider_history"] = true
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"costs": out, "total": total, "limit": limit, "offset": offset})
}

func (h *Handler) createProviderCost(c *gin.Context) {
	var req createProviderCostReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	accountID, ok := accountIDFromCostReq(req)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	occurredAt := time.Time{}
	if req.OccurredAt != "" {
		occurredAt, _ = time.Parse(time.RFC3339, req.OccurredAt)
	}
	row, err := h.finance.CreateAccountCost(c.Request.Context(), biz.CreateAccountCostInput{
		AccountID:  accountID,
		Amount:     amount,
		Currency:   req.Currency,
		OccurredAt: occurredAt,
		Note:       req.Note,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"cost": gin.H{
		"id":          row.ID.String(),
		"account_id":  row.AccountID.String(),
		"provider_id": row.AccountID.String(),
		"amount":      row.Amount.String(),
		"currency":    row.Currency,
		"occurred_at": row.OccurredAt,
		"note":        row.Note,
		"created_at":  row.CreatedAt,
	}})
}

func (h *Handler) deleteProviderCost(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.finance.DeleteAccountCost(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) listProviderSettlements(c *gin.Context) {
	month := parseMonthQuery(c)
	if month.IsZero() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid month"})
		return
	}
	rows, err := h.finance.ListAccountSettlements(c.Request.Context(), month)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		out = append(out, providerSettlementView(row))
	}
	from, to := biz.MonthRange(month)
	c.JSON(http.StatusOK, gin.H{
		"month":        from.Format("2006-01"),
		"period_start": from,
		"period_end":   to,
		"settlements":  out,
	})
}

type confirmProviderSettlementReq struct {
	ActualCost *string `json:"actual_cost"`
	Note       string  `json:"note"`
	Month      string  `json:"month"`
}

func (h *Handler) confirmProviderSettlement(c *gin.Context) {
	accountID, err := uuid.Parse(c.Param("provider_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid provider_id"})
		return
	}
	var req confirmProviderSettlementReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	month := parseMonthQuery(c)
	if req.Month != "" {
		if t, err := time.Parse("2006-01", strings.TrimSpace(req.Month)); err == nil {
			month = t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid month"})
			return
		}
	}
	var actual *decimal.Decimal
	if req.ActualCost != nil && strings.TrimSpace(*req.ActualCost) != "" {
		v, err := decimal.NewFromString(strings.TrimSpace(*req.ActualCost))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid actual_cost"})
			return
		}
		actual = &v
	}
	actor, _ := middleware.UserIDFrom(c)
	row, err := h.finance.ConfirmAccountSettlement(c.Request.Context(), biz.ConfirmAccountSettlementInput{
		AccountID:  accountID,
		Month:      month,
		ActualCost: actual,
		ActorID:    actor,
		Note:       req.Note,
	})
	if err != nil {
		financeErr(c, err)
		return
	}
	settlement := gin.H{
		"id":               row.ID.String(),
		"account_id":       row.AccountID.String(),
		"provider_id":      row.AccountID.String(),
		"period_start":     row.PeriodStart,
		"period_end":       row.PeriodEnd,
		"calculated_cost":  row.CalculatedCost.String(),
		"actual_cost":      row.ActualCost.String(),
		"correction_delta": row.ActualCost.Sub(row.CalculatedCost).String(),
		"currency":         row.Currency,
		"status":           "confirmed",
		"note":             row.Note,
	}
	if row.ConfirmedByUserID != nil {
		settlement["confirmed_by_user_id"] = row.ConfirmedByUserID.String()
	}
	if row.ConfirmedAt != nil {
		settlement["confirmed_at"] = row.ConfirmedAt
	}
	c.JSON(http.StatusOK, gin.H{"settlement": settlement})
}

func providerSettlementView(row biz.AccountSettlementRow) gin.H {
	usageDetails := make([]gin.H, 0, len(row.UsageDetails))
	for _, d := range row.UsageDetails {
		usageDetails = append(usageDetails, gin.H{
			"group_id":                 d.GroupID,
			"group_name":               d.GroupName,
			"model":                    d.Model,
			"requests":                 d.Requests,
			"tokens_in":                d.TokensIn,
			"tokens_out":               d.TokensOut,
			"cached_tokens_in":         d.CachedTokensIn,
			"cache_creation_tokens_in": d.CacheCreationTokensIn,
			"output_images":            d.OutputImages,
			"cost":                     d.Cost.String(),
		})
	}
	manualDetails := make([]gin.H, 0, len(row.ManualCostDetails))
	for _, d := range row.ManualCostDetails {
		detail := gin.H{
			"id":          d.ID.String(),
			"amount":      d.Amount.String(),
			"currency":    d.Currency,
			"amount_cny":  d.AmountCNY.String(),
			"occurred_at": d.OccurredAt,
			"note":        d.Note,
		}
		if d.LegacyProviderHistory {
			detail["legacy_provider_history"] = true
		}
		manualDetails = append(manualDetails, detail)
	}
	out := gin.H{
		"account_id":          row.AccountID.String(),
		"account_name":        row.AccountName,
		"provider_id":         row.AccountID.String(),
		"provider_name":       row.AccountName,
		"period_start":        row.PeriodStart,
		"period_end":          row.PeriodEnd,
		"calculated_cost":     row.CalculatedCost.String(),
		"actual_cost":         row.ActualCost.String(),
		"correction_delta":    row.CorrectionDelta.String(),
		"currency":            row.Currency,
		"status":              row.Status,
		"note":                row.Note,
		"usage_cost":          row.UsageCost.String(),
		"manual_cost":         row.ManualCost.String(),
		"usage_details":       usageDetails,
		"manual_cost_details": manualDetails,
	}
	if row.LegacyProviderHistory {
		out["legacy_provider_history"] = true
	}
	if row.ConfirmedByUserID != nil {
		out["confirmed_by_user_id"] = row.ConfirmedByUserID.String()
	}
	if row.ConfirmedAt != nil {
		out["confirmed_at"] = row.ConfirmedAt
	}
	return out
}

type createOrderReq struct {
	UserID   string  `json:"user_id" binding:"required"`
	Kind     string  `json:"kind" binding:"required"`
	PlanID   *string `json:"plan_id"`
	Amount   string  `json:"amount" binding:"required"`
	Currency string  `json:"currency"`
	Note     string  `json:"note"`
}

func (h *Handler) listOrders(c *gin.Context) {
	limit, offset := parsePage(c, 500)
	var userID *uuid.UUID
	if raw := c.Query("user_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		userID = &id
	}
	// Validate the enums here: passing an unknown value straight to ent
	// surfaces as a driver error and a 500, which reads as a server fault
	// rather than a bad filter.
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && entsiteorder.StatusValidator(entsiteorder.Status(status)) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
		return
	}
	kind := strings.TrimSpace(c.Query("kind"))
	if kind != "" && entsiteorder.KindValidator(entsiteorder.Kind(kind)) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid kind filter"})
		return
	}
	rows, total, err := h.finance.ListOrders(c.Request.Context(), biz.OrderFilter{
		Status:      status,
		Kind:        kind,
		UserID:      userID,
		Q:           strings.TrimSpace(c.Query("q")),
		LoadBuyer:   true,
		LoadRefunds: true,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, o := range rows {
		out = append(out, orderView(o))
	}
	c.JSON(http.StatusOK, gin.H{"orders": out, "total": total, "limit": limit, "offset": offset})
}

func orderView(o *ent.SiteOrder) gin.H {
	row := gin.H{
		"id":         o.ID.String(),
		"order_no":   o.OrderNo,
		"user_id":    o.UserID.String(),
		"kind":       o.Kind,
		"amount":     o.Amount.String(),
		"currency":   o.Currency,
		"status":     o.Status,
		"note":       o.Note,
		"created_at": o.CreatedAt,
	}
	if o.PaidAt != nil {
		row["paid_at"] = o.PaidAt
	}
	if o.PlanID != nil {
		row["plan_id"] = o.PlanID.String()
	}
	if o.UserPlanID != nil {
		row["user_plan_id"] = o.UserPlanID.String()
	}
	if o.PaymentChannel != "" {
		row["payment_channel"] = o.PaymentChannel
	}
	if o.PaymentTradeType != "" {
		row["payment_trade_type"] = o.PaymentTradeType
	}
	if o.PaymentTradeState != "" {
		row["payment_trade_state"] = o.PaymentTradeState
	}
	if o.PaymentTransactionID != "" {
		row["payment_transaction_id"] = o.PaymentTransactionID
	}
	if o.PaymentAmountCents > 0 {
		row["payment_amount_cents"] = o.PaymentAmountCents
	}
	// Only the listing eager-loads the buyer; the mutation responses
	// (create / pay / cancel) reuse this view without the edge.
	if o.Edges.User != nil {
		row["user_email"] = o.Edges.User.Email
		row["user_nickname"] = o.Edges.User.Nickname
	}
	if refunds := o.Edges.Refunds; len(refunds) > 0 {
		out := make([]gin.H, 0, len(refunds))
		for _, r := range refunds {
			out = append(out, gin.H{
				"id":         r.ID.String(),
				"amount":     r.Amount.String(),
				"status":     r.Status,
				"reason":     r.Reason,
				"created_at": r.CreatedAt,
			})
		}
		row["refunds"] = out
	}
	return row
}

func (h *Handler) createOrder(c *gin.Context) {
	var req createOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
		return
	}
	var planID *uuid.UUID
	if req.PlanID != nil && strings.TrimSpace(*req.PlanID) != "" {
		id, err := uuid.Parse(*req.PlanID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_id"})
			return
		}
		planID = &id
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	o, err := h.finance.CreateOrder(c.Request.Context(), biz.CreateOrderInput{
		UserID: userID, Kind: req.Kind, PlanID: planID, Amount: amount,
		Currency: req.Currency, Note: req.Note,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"order": orderView(o)})
}

func (h *Handler) markOrderPaid(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	o, err := h.finance.MarkOrderPaid(c.Request.Context(), id)
	if err != nil {
		financeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": orderView(o)})
}

func (h *Handler) syncOrderPayment(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	o, err := h.finance.SyncOrderPayment(c.Request.Context(), id)
	if err != nil {
		financeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": orderView(o)})
}

func (h *Handler) cancelOrder(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	o, err := h.finance.CancelOrder(c.Request.Context(), id)
	if err != nil {
		financeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": orderView(o)})
}

type createRefundReq struct {
	Amount *string `json:"amount"`
	Reason string  `json:"reason"`
	Note   string  `json:"note"`
}

func (h *Handler) listRefunds(c *gin.Context) {
	limit, offset := parsePage(c, 500)
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && entsiterefund.StatusValidator(entsiterefund.Status(status)) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
		return
	}
	rows, total, err := h.finance.ListRefunds(c.Request.Context(), biz.RefundFilter{
		Status:    status,
		Q:         strings.TrimSpace(c.Query("q")),
		LoadOrder: true,
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, refundView(r))
	}
	c.JSON(http.StatusOK, gin.H{"refunds": out, "total": total, "limit": limit, "offset": offset})
}

func refundView(r *ent.SiteRefund) gin.H {
	row := gin.H{
		"id":         r.ID.String(),
		"order_id":   r.OrderID.String(),
		"user_id":    r.UserID.String(),
		"amount":     r.Amount.String(),
		"currency":   r.Currency,
		"status":     r.Status,
		"reason":     r.Reason,
		"note":       r.Note,
		"created_at": r.CreatedAt,
	}
	if r.ReviewedByUserID != nil {
		row["reviewed_by_user_id"] = r.ReviewedByUserID.String()
	}
	if r.ReviewedAt != nil {
		row["reviewed_at"] = r.ReviewedAt
	}
	// Same as orderView: the edge is only loaded by the listing, so the
	// approve / reject responses fall through without these fields.
	if o := r.Edges.Order; o != nil {
		row["order_no"] = o.OrderNo
		if u := o.Edges.User; u != nil {
			row["user_email"] = u.Email
			row["user_nickname"] = u.Nickname
		}
	}
	return row
}

func (h *Handler) createRefund(c *gin.Context) {
	orderID, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req createRefundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var amount *decimal.Decimal
	if req.Amount != nil && strings.TrimSpace(*req.Amount) != "" {
		v, err := decimal.NewFromString(strings.TrimSpace(*req.Amount))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		amount = &v
	}
	r, err := h.finance.CreateRefund(c.Request.Context(), biz.CreateRefundInput{
		OrderID: orderID, Amount: amount, Reason: req.Reason, Note: req.Note,
	})
	if err != nil {
		financeErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"refund": refundView(r)})
}

type reviewRefundReq struct {
	Note string `json:"note"`
}

func (h *Handler) approveRefund(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req reviewRefundReq
	_ = c.ShouldBindJSON(&req)
	actor, _ := middleware.UserIDFrom(c)
	r, err := h.finance.ApproveRefund(c.Request.Context(), id, actor, req.Note)
	if err != nil {
		financeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"refund": refundView(r)})
}

func (h *Handler) rejectRefund(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req reviewRefundReq
	_ = c.ShouldBindJSON(&req)
	actor, _ := middleware.UserIDFrom(c)
	r, err := h.finance.RejectRefund(c.Request.Context(), id, actor, req.Note)
	if err != nil {
		financeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"refund": refundView(r)})
}

func financeErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrOrderNotFound), errors.Is(err, biz.ErrRefundNotFound), errors.Is(err, biz.ErrProviderNotFound), ent.IsNotFound(err):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, biz.ErrOrderInvalidState), errors.Is(err, biz.ErrRefundPending):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

// orderRefundable prefills the refund amount: what the order can return
// without refunding money the user already spent.
func (h *Handler) orderRefundable(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ref, err := h.finance.OrderRefundable(c.Request.Context(), id)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, biz.ErrOrderNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ref)
}
