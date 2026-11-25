package agent

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// getCoupon returns the agent's coupon, the split it applies right now, and
// how many buyers have used it.
func (h *Handler) getCoupon(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	ctx := c.Request.Context()
	p, err := h.agents.EnsureProfile(ctx, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	terms, err := h.agents.CouponTermsFor(ctx, p)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	buyers, err := h.agents.CouponBuyerCount(ctx, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := gin.H{
		"status":          p.Status,
		"budget_pct":      terms.DiscountPct.Add(terms.RebatePct).String(),
		"discount_pct":    terms.DiscountPct.String(),
		"rebate_rate_pct": terms.RebatePct.String(),
		"buyers":          buyers,
	}
	if p.CouponCode != nil {
		out["coupon_code"] = *p.CouponCode
	}
	c.JSON(http.StatusOK, out)
}

type createCouponReq struct {
	CouponCode string `json:"coupon_code"`
}

func (h *Handler) createCoupon(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req createCouponReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	p, err := h.agents.CreateCoupon(c.Request.Context(), uid, req.CouponCode)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, biz.ErrAgentSuspended) {
			status = http.StatusForbidden
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"coupon_code": *p.CouponCode})
}

type updateCouponReq struct {
	DiscountPct string `json:"discount_pct" binding:"required"`
}

func (h *Handler) updateCoupon(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req updateCouponReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pct, err := decimal.NewFromString(req.DiscountPct)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid discount_pct"})
		return
	}
	p, err := h.agents.SetCouponDiscount(c.Request.Context(), uid, pct)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"discount_pct": p.CouponDiscountPct.String()})
}
