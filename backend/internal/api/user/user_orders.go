package user

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

type createMyOrderReq struct {
	Kind       string `json:"kind" binding:"required"`
	PlanID     string `json:"plan_id"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	Note       string `json:"note"`
	CouponCode string `json:"coupon_code"`
}

type createAlipayPagePayReq struct {
	ReturnURL string `json:"return_url"`
	// Client 为 "wap" 时走支付宝手机网站支付（能唤起 App）；其余值按电脑网站支付。
	Client  string `json:"client"`
	QuitURL string `json:"quit_url"`
}

func (h *Handler) listMyOrders(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	limit := parseIntQuery(c, "limit", 50, 200, 1)
	offset := parseIntQuery(c, "offset", 0, 100000, 0)
	rows, total, err := h.finance.ListOrders(c.Request.Context(), biz.OrderFilter{
		Status:      c.Query("status"),
		Kind:        c.Query("kind"),
		UserID:      &uid,
		LoadRefunds: true,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	orderIDs := make([]uuid.UUID, 0, len(rows))
	for _, o := range rows {
		orderIDs = append(orderIDs, o.ID)
	}
	tickets, err := h.tickets.LatestRefundTickets(c.Request.Context(), orderIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, o := range rows {
		row := myOrderView(o)
		if t := tickets[o.ID]; t != nil {
			row["refund_ticket"] = gin.H{
				"id":     t.ID.String(),
				"status": t.Status,
			}
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"orders": out, "total": total, "limit": limit, "offset": offset})
}

func myOrderView(o *ent.SiteOrder) gin.H {
	row := gin.H{
		"id":         o.ID.String(),
		"order_no":   o.OrderNo,
		"kind":       o.Kind,
		"amount":     o.Amount.String(),
		"currency":   o.Currency,
		"status":     o.Status,
		"created_at": o.CreatedAt,
	}
	if o.PaidAt != nil {
		row["paid_at"] = o.PaidAt
	}
	if o.CreditAmount.Sign() > 0 {
		row["credit_amount"] = o.CreditAmount.String()
	}
	if o.PromotionDiscountPct.Sign() > 0 {
		row["coupon_discount_pct"] = o.PromotionDiscountPct.String()
	}
	if o.PromotionCode != "" {
		row["coupon_code"] = o.PromotionCode
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
	if o.PaymentCodeURL != "" {
		row["payment_code_url"] = o.PaymentCodeURL
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

func (h *Handler) createMyOrder(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req createMyOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var planID *uuid.UUID
	var amount decimal.Decimal
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	// Orders are denominated in points (1 point = 1 CNY); USD or other
	// input currencies are no longer accepted.
	if currency != "" && currency != biz.CurrencyCNY {
		c.JSON(http.StatusBadRequest, gin.H{"error": "currency must be CNY"})
		return
	}
	currency = biz.CurrencyCNY
	switch req.Kind {
	case "plan":
		id, err := uuid.Parse(req.PlanID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_id"})
			return
		}
		plan, err := h.users.Plan.Get(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "plan not found"})
			return
		}
		planID = &id
		// plan.Price is in points; order currency is always CNY.
		amount = plan.Price
	case "balance":
		v, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
		if err != nil || v.Sign() <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		amount = v
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be balance or plan"})
		return
	}
	o, err := h.finance.CreateOrder(c.Request.Context(), biz.CreateOrderInput{
		UserID: uid, Kind: req.Kind, PlanID: planID, Amount: amount, Currency: currency, Note: req.Note,
		CouponCode: req.CouponCode,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"order": myOrderView(o)})
}

func (h *Handler) createAlipayPagePay(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	o, err := h.users.SiteOrder.Get(c.Request.Context(), id)
	if err != nil || o.UserID != uid {
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
		return
	}
	var req createAlipayPagePayReq
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	returnURL := strings.TrimSpace(req.ReturnURL)
	if returnURL != "" {
		u, err := url.Parse(returnURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid return_url"})
			return
		}
	}
	quitURL := strings.TrimSpace(req.QuitURL)
	if quitURL != "" {
		u, err := url.Parse(quitURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "https" && u.Scheme != "http") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quit_url"})
			return
		}
	}
	res, err := h.finance.CreateAlipayPay(c.Request.Context(), biz.AlipayPayInput{
		OrderID:   id,
		ReturnURL: returnURL,
		Wap:       strings.EqualFold(strings.TrimSpace(req.Client), "wap"),
		QuitURL:   quitURL,
	})
	if err != nil {
		writeOrderPaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"order":    myOrderView(res.Order),
		"code_url": res.CodeURL,
		"pay_url":  res.CodeURL,
		"pay_type": res.PayType,
	})
}

func (h *Handler) syncOrderPayment(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	o, err := h.users.SiteOrder.Get(c.Request.Context(), id)
	if err != nil || o.UserID != uid {
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
		return
	}
	o, err = h.finance.SyncOrderPayment(c.Request.Context(), id)
	if err != nil {
		writeOrderPaymentError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": myOrderView(o)})
}

func writeOrderPaymentError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrPaymentUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment unavailable"})
	case errors.Is(err, biz.ErrOrderInvalidState):
		c.JSON(http.StatusBadRequest, gin.H{"error": "order state does not allow this operation"})
	case errors.Is(err, biz.ErrPaymentAmountMismatch):
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment amount mismatch"})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

type createMyRefundReq struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

func (h *Handler) createMyRefund(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req createMyRefundReq
	_ = c.ShouldBindJSON(&req)
	reason := strings.TrimSpace(req.Reason)
	if note := strings.TrimSpace(req.Note); note != "" {
		if reason != "" {
			reason = reason + "\n\n" + note
		} else {
			reason = note
		}
	}
	t, err := h.tickets.Create(c.Request.Context(), biz.CreateTicketInput{
		UserID:   uid,
		OrderID:  &id,
		Title:    "申请退款",
		Body:     reason,
		Category: "refund",
	})
	if err != nil {
		writeTicketCreateError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ticket": myTicketView(h.autoApproveRefundTicket(c, t, reason))})
}
