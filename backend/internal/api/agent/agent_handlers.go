package agent

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) getProfile(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	p, err := h.agents.EnsureProfile(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	share, err := h.agents.EffectiveShare(c.Request.Context(), p)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"agent_id":           p.UserID.String(),
		"platform_share_pct": share.String(),
		"status":             p.Status,
		"note":               p.Note,
		"created_at":         p.CreatedAt,
	})
}

// getDashboard merges the pending summary with the recent sales/refunds
// so the agent landing page renders without three extra round-trips.
func (h *Handler) getDashboard(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	p, err := h.agents.EnsureProfile(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	share, err := h.agents.EffectiveShare(c.Request.Context(), p)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	sum, err := h.agents.PendingSummary(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"profile": gin.H{
			"platform_share_pct": share.String(),
			"status":             p.Status,
		},
		"pending": gin.H{
			"total_payable":  sum.TotalPayable.String(),
			"entries_count":  sum.EntriesCount,
			"sales":          sum.Sales.String(),
			"refunds":        sum.Refunds.String(),
			"coupon_rebates": sum.CouponRebates.String(),
			"withdrawn":      sum.Withdrawn.String(),
			"withdrawable":   sum.Withdrawable().StringFixed(2),
		},
	})
}

type generateCodesReq struct {
	Kind      string  `json:"kind" binding:"required"` // balance | plan
	Amount    string  `json:"amount"`                  // balance only
	PlanID    string  `json:"plan_id"`                 // plan only
	Count     int     `json:"count"`
	ExpiresAt *string `json:"expires_at"`
	Note      string  `json:"note"`
}

func (h *Handler) generateCodes(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	prof, err := h.agents.EnsureProfile(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if prof.Status != "active" {
		c.JSON(http.StatusForbidden, gin.H{"error": "agent is suspended"})
		return
	}
	var req generateCodesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.GenerateInput{
		Kind:      req.Kind,
		Count:     req.Count,
		MaxUses:   1,
		BatchID:   "agent-" + uid.String()[:8] + "-" + time.Now().Format("20060102150405"),
		Note:      req.Note,
		CreatedBy: uid,
		AgentID:   &uid,
	}
	switch req.Kind {
	case "balance":
		amt, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
		if err != nil || amt.Sign() <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		in.Amount = &amt
	case "plan":
		pid, err := uuid.Parse(strings.TrimSpace(req.PlanID))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_id"})
			return
		}
		in.PlanID = &pid
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be balance|plan"})
		return
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expires_at"})
			return
		}
		in.ExpiresAt = &t
	}
	codes, err := h.redeem.GenerateBatch(c.Request.Context(), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(codes))
	for _, code := range codes {
		row := gin.H{
			"id":         code.ID.String(),
			"code":       code.Code,
			"kind":       code.Kind,
			"status":     code.Status,
			"created_at": code.CreatedAt,
			"batch_id":   code.BatchID,
		}
		if code.Amount != nil {
			row["amount"] = code.Amount.String()
		}
		if code.PlanID != nil {
			row["plan_id"] = code.PlanID.String()
		}
		out = append(out, row)
	}
	c.JSON(http.StatusCreated, gin.H{"codes": out, "count": len(out)})
}

func (h *Handler) listMyCodes(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	// Reuse RedeemSvc.List with a manual agent filter via raw query.
	// Easier than threading agent_id into ListFilter — the agent dashboard
	// is the only consumer of this view.
	codes, err := h.redeem.List(c.Request.Context(), biz.ListFilter{Limit: 500})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0)
	for _, code := range codes {
		if code.AgentID == nil || *code.AgentID != uid {
			continue
		}
		row := gin.H{
			"id":         code.ID.String(),
			"code":       code.Code,
			"kind":       code.Kind,
			"status":     code.Status,
			"used_count": code.UsedCount,
			"max_uses":   code.MaxUses,
			"created_at": code.CreatedAt,
			"batch_id":   code.BatchID,
		}
		if code.Amount != nil {
			row["amount"] = code.Amount.String()
		}
		if code.PlanID != nil {
			row["plan_id"] = code.PlanID.String()
		}
		if code.ExpiresAt != nil {
			row["expires_at"] = code.ExpiresAt
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"codes": out})
}

func (h *Handler) listSales(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	res, err := h.agents.ListEntries(c.Request.Context(), biz.AgentEntryFilter{
		AgentID: uid,
		Kind:    c.Query("kind"),
		Status:  c.Query("status"),
		Limit:   parseIntQuery(c, "limit", 100, 500, 1),
		Offset:  parseIntQuery(c, "offset", 0, 100000, 0),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(res.Entries))
	for _, e := range res.Entries {
		row := gin.H{
			"id":                 e.ID.String(),
			"kind":               e.Kind,
			"sale_amount":        e.SaleAmount.String(),
			"platform_payable":   e.PlatformPayable.String(),
			"share_pct_at_event": e.SharePctAtEvent.String(),
			"currency":           e.Currency,
			"source_type":        e.SourceType,
			"source_id":          e.SourceID.String(),
			"created_at":         e.CreatedAt,
		}
		if e.RedeemCodeID != nil {
			row["redeem_code_id"] = e.RedeemCodeID.String()
		}
		if e.UserID != nil {
			row["user_id"] = e.UserID.String()
		}
		if e.OrderID != nil {
			row["order_id"] = e.OrderID.String()
		}
		if e.SettlementID != nil {
			row["settlement_id"] = e.SettlementID.String()
		} else {
			row["settlement_id"] = nil
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"entries": out, "total": res.Total})
}

func (h *Handler) listSettlements(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	rows, total, err := h.agents.ListSettlements(c.Request.Context(), uid,
		parseIntQuery(c, "limit", 50, 200, 1),
		parseIntQuery(c, "offset", 0, 100000, 0))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, s := range rows {
		row := gin.H{
			"id":            s.ID.String(),
			"total_payable": s.TotalPayable.String(),
			"entries_count": s.EntriesCount,
			"currency":      s.Currency,
			"created_at":    s.CreatedAt,
			"note":          s.Note,
		}
		if s.SettledByUserID != nil {
			row["settled_by_user_id"] = s.SettledByUserID.String()
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"settlements": out, "total": total})
}

func (h *Handler) listRefunds(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	rows, total, err := h.agents.ListRefunds(c.Request.Context(), uid,
		parseIntQuery(c, "limit", 50, 200, 1),
		parseIntQuery(c, "offset", 0, 100000, 0))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"id":               r.ID.String(),
			"redeem_code_id":   r.RedeemCodeID.String(),
			"redeem_record_id": r.RedeemRecordID.String(),
			"user_id":          r.UserID.String(),
			"kind":             r.Kind,
			"refund_amount":    r.RefundAmount.String(),
			"platform_deduct":  r.PlatformDeduct.String(),
			"currency":         r.Currency,
			"reason":           r.Reason,
			"created_at":       r.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"refunds": out, "total": total})
}

func (h *Handler) previewRefund(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing code"})
		return
	}
	preview, err := h.agents.PreviewRefund(c.Request.Context(), uid, code)
	if err != nil {
		refundErr(c, err)
		return
	}
	// refund_amount + platform_deduct are denominated in points
	// (1 point = 1 CNY) by biz.AgentSvc.PreviewRefund. Surfacing them
	// with the code's original currency tag would let the UI display
	// point numbers under a wrong symbol, so the wire-level currency is
	// always CNY. plan_price_original / plan_currency carry the
	// admin-entered value for reference; the UI shouldn't perform math
	// on them.
	row := gin.H{
		"code":             preview.Code.Code,
		"redeem_record_id": preview.Record.ID.String(),
		"user_id":          preview.Record.UserID.String(),
		"kind":             preview.Record.Kind,
		"refund_amount":    preview.RefundAmount.String(),
		"platform_deduct":  preview.PlatformDeduct.String(),
		"already_refunded": preview.AlreadyRefunded,
		"currency":         biz.CurrencyCNY,
	}
	if preview.Plan != nil {
		row["plan_name"] = preview.Plan.Name
		row["plan_price"] = preview.Plan.Price.String()
		row["plan_currency"] = preview.Plan.Currency
		row["remaining_days"] = preview.RemainingDays
	}
	c.JSON(http.StatusOK, row)
}

type registerRefundReq struct {
	RedeemRecordID string `json:"redeem_record_id" binding:"required"`
	Reason         string `json:"reason"`
}

func (h *Handler) registerRefund(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req registerRefundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rid, err := uuid.Parse(req.RedeemRecordID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid redeem_record_id"})
		return
	}
	res, err := h.agents.RegisterRefund(c.Request.Context(), uid, rid, req.Reason)
	if err != nil {
		refundErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"refund_id":         res.RefundID.String(),
		"kind":              res.Kind,
		"refund_amount":     res.RefundAmount.String(),
		"platform_deduct":   res.PlatformDeduct.String(),
		"user_plan_removed": res.UserPlanRemoved,
	})
}

func refundErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrRedeemCodeNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "redeem code/record not found"})
	case errors.Is(err, biz.ErrRefundCodeMismatch):
		c.JSON(http.StatusForbidden, gin.H{"error": "code does not belong to this agent"})
	case errors.Is(err, biz.ErrAgentRefundAlreadyDone):
		c.JSON(http.StatusConflict, gin.H{"error": "this record was already refunded"})
	case errors.Is(err, biz.ErrAgentRefundUserPlanGone):
		c.JSON(http.StatusConflict, gin.H{"error": "user plan no longer exists"})
	case errors.Is(err, biz.ErrAgentSuspended):
		c.JSON(http.StatusForbidden, gin.H{"error": "agent is suspended"})
	case errors.Is(err, biz.ErrRedeemKindMismatch):
		c.JSON(http.StatusBadRequest, gin.H{"error": "redeem record kind mismatch"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func parseIntQuery(c *gin.Context, name string, def, max, min int) int {
	v := c.Query(name)
	if v == "" {
		return def
	}
	var n int
	for _, r := range v {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
		if n > max {
			return max
		}
	}
	if n < min {
		return min
	}
	return n
}
