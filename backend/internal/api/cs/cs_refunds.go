package cs

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) listRefunds(c *gin.Context) {
	limit := parseIntQuery(c, "limit", 50, 200, 1)
	offset := parseIntQuery(c, "offset", 0, 100000, 0)
	rows, total, err := h.finance.ListRefunds(c.Request.Context(), biz.RefundFilter{
		Status: c.Query("status"),
		Limit:  limit,
		Offset: offset,
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

type reviewRefundReq struct {
	Note string `json:"note"`
}

func (h *Handler) approveRefund(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
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
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
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
	return row
}

func financeErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrOrderNotFound), errors.Is(err, biz.ErrRefundNotFound), ent.IsNotFound(err):
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
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
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
