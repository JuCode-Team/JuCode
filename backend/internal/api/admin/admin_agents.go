// admin_agents.go owns the /v1/admin/agents/* surface — the platform
// operator's view of every agent's commercial state: who they are,
// what they've sold, what's pending, settle when ready, and the
// historical settlement audit.
package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) listAgents(c *gin.Context) {
	if h.agents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent service unavailable"})
		return
	}
	profiles, err := h.agents.ListAllProfiles(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(profiles))
	for _, p := range profiles {
		row := gin.H{
			"agent_id":           p.UserID.String(),
			"platform_share_pct": p.PlatformSharePct.String(),
			"status":             p.Status,
			"created_at":         p.CreatedAt,
		}
		// Annotate each agent with their email and pending summary so
		// the admin table renders without N extra round-trips. The
		// extra COUNT/SUM per row is bounded — agent counts are small
		// (operator-managed) so this is fine.
		if u, err := h.users.User.Get(c.Request.Context(), p.UserID); err == nil {
			row["email"] = u.Email
			row["nickname"] = u.Nickname
		}
		if sum, err := h.agents.PendingSummary(c.Request.Context(), p.UserID); err == nil {
			row["pending_total_payable"] = sum.TotalPayable.String()
			row["pending_entries"] = sum.EntriesCount
			row["pending_sales"] = sum.Sales.String()
			row["pending_refunds"] = sum.Refunds.String()
			row["pending_coupon_rebates"] = sum.CouponRebates.String()
		}
		terms, err := h.agents.CouponTermsFor(c.Request.Context(), p)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		row["coupon_discount_pct"] = terms.DiscountPct.String()
		row["coupon_rebate_rate_pct"] = terms.RebatePct.String()
		if p.CouponCode != nil {
			row["coupon_code"] = *p.CouponCode
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"agents": out})
}

func (h *Handler) getAgentEntries(c *gin.Context) {
	if h.agents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent service unavailable"})
		return
	}
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	res, err := h.agents.ListEntries(c.Request.Context(), biz.AgentEntryFilter{
		AgentID: aid,
		Kind:    c.Query("kind"),
		Status:  c.Query("status"),
		Limit:   adminParseInt(c, "limit", 100, 500, 1),
		Offset:  adminParseInt(c, "offset", 0, 100000, 0),
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
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"entries": out, "total": res.Total})
}

func (h *Handler) getAgentRefunds(c *gin.Context) {
	if h.agents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent service unavailable"})
		return
	}
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	rows, total, err := h.agents.ListRefunds(c.Request.Context(), aid,
		adminParseInt(c, "limit", 50, 200, 1),
		adminParseInt(c, "offset", 0, 100000, 0))
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

func (h *Handler) getAgentSettlements(c *gin.Context) {
	if h.agents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent service unavailable"})
		return
	}
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	rows, total, err := h.agents.ListSettlements(c.Request.Context(), aid,
		adminParseInt(c, "limit", 50, 200, 1),
		adminParseInt(c, "offset", 0, 100000, 0))
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

type updateAgentReq struct {
	PlatformSharePct  *string `json:"platform_share_pct"`
	CouponDiscountPct *string `json:"coupon_discount_pct"`
	Status            *string `json:"status"`
}

func (h *Handler) updateAgent(c *gin.Context) {
	if h.agents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent service unavailable"})
		return
	}
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateAgentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.PlatformSharePct != nil {
		share, err := decimal.NewFromString(*req.PlatformSharePct)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid platform_share_pct"})
			return
		}
		if _, err := h.agents.UpdateSharePct(c.Request.Context(), aid, share); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if req.CouponDiscountPct != nil {
		pct, err := decimal.NewFromString(*req.CouponDiscountPct)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid coupon_discount_pct"})
			return
		}
		if _, err := h.agents.SetCouponDiscount(c.Request.Context(), aid, pct); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if req.Status != nil {
		if _, err := h.agents.SetStatus(c.Request.Context(), aid, *req.Status); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	p, err := h.agents.EnsureProfile(c.Request.Context(), aid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"agent_id":            p.UserID.String(),
		"platform_share_pct":  p.PlatformSharePct.String(),
		"coupon_discount_pct": p.CouponDiscountPct.String(),
		"status":              p.Status,
	})
}

type settleReq struct {
	Note string `json:"note"`
}

func (h *Handler) settleAgent(c *gin.Context) {
	if h.agents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent service unavailable"})
		return
	}
	aid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req settleReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	actor, _ := middleware.UserIDFrom(c)
	settlement, entries, err := h.agents.Settle(c.Request.Context(), aid, actor, req.Note)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":            settlement.ID.String(),
		"total_payable": settlement.TotalPayable.String(),
		"entries_count": entries,
		"created_at":    settlement.CreatedAt,
	})
}

// adminParseInt clones parseIntQuery from sibling files so each
// domain file stays self-contained; the helper here name-shadows
// nothing and keeps admin_pool.go etc. unchanged.
func adminParseInt(c *gin.Context, name string, def, max, min int) int {
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
