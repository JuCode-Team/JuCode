package user

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) handleCouponQuote(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	if h.agents == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "coupon service unavailable"})
		return
	}
	quote, err := h.agents.QuoteCoupon(c.Request.Context(), uid, c.Query("code"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, biz.ErrCouponCodeMalformed) ||
			errors.Is(err, biz.ErrCouponCodeUnknown) ||
			errors.Is(err, biz.ErrCouponSelfUse) ||
			errors.Is(err, biz.ErrCouponExclusiveBuyer) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{
		"coupon_code":  quote.Code,
		"discount_pct": quote.DiscountPct.String(),
	}
	if h.invites != nil {
		// Advisory only: a failed lookup must not make the coupon unusable.
		if conflict, err := h.invites.CouponConflict(c.Request.Context(), uid); err != nil {
			slog.Warn("coupon quote: invite conflict lookup failed", "user_id", uid.String(), "err", err.Error())
		} else {
			resp["invite_conflict"] = conflict
		}
	}
	c.JSON(http.StatusOK, resp)
}
