package agent

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

type withdrawReq struct {
	Amount string `json:"amount" binding:"required"`
}

// withdrawToBalance moves owed commission into the agent's own balance.
func (h *Handler) withdrawToBalance(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req withdrawReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": biz.ErrWithdrawAmountInvalid.Error()})
		return
	}
	btx, err := h.agents.WithdrawToBalance(c.Request.Context(), uid, amount)
	switch {
	case err == nil:
	case errors.Is(err, biz.ErrWithdrawAmountInvalid), errors.Is(err, biz.ErrWithdrawExceeds):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	case errors.Is(err, biz.ErrAgentSuspended), errors.Is(err, biz.ErrAgentNotEligible):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"amount":        btx.Amount.StringFixed(2),
		"balance_after": btx.BalanceAfter.String(),
	})
}
