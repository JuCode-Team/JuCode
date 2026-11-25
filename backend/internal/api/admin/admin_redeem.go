// admin_redeem.go: redeem-code generation, listing, and revocation.
// Issuance / validation logic lives in biz.RedeemSvc; this file only
// shapes HTTP I/O.
package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

type redeemCodeView struct {
	ID        string     `json:"id"`
	Code      string     `json:"code"`
	Kind      string     `json:"kind"`
	Amount    *string    `json:"amount,omitempty"`
	Currency  string     `json:"currency"`
	PlanID    *string    `json:"plan_id,omitempty"`
	MaxUses   int        `json:"max_uses"`
	UsedCount int        `json:"used_count"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Status    string     `json:"status"`
	BatchID   string     `json:"batch_id,omitempty"`
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

func toRedeemCodeView(rc *ent.RedeemCode) redeemCodeView {
	v := redeemCodeView{
		ID: rc.ID.String(), Code: rc.Code, Kind: string(rc.Kind),
		Currency: rc.Currency, MaxUses: rc.MaxUses, UsedCount: rc.UsedCount,
		ExpiresAt: rc.ExpiresAt, Status: string(rc.Status),
		BatchID: rc.BatchID, Note: rc.Note, CreatedAt: rc.CreatedAt,
	}
	if rc.Amount != nil {
		s := rc.Amount.String()
		v.Amount = &s
	}
	if rc.PlanID != nil {
		s := rc.PlanID.String()
		v.PlanID = &s
	}
	return v
}

func (h *Handler) listRedeemCodes(c *gin.Context) {
	// biz.RedeemSvc.List caps Limit at 500. We request the max here so that
	// handler-level q + offset paging operates over the most-recent 500 rows
	// matching status/kind/batch_id. For larger working sets, narrow with
	// status/kind/batch_id first.
	codes, err := h.redeem.List(c.Request.Context(), biz.ListFilter{
		Status:  c.Query("status"),
		Kind:    c.Query("kind"),
		BatchID: c.Query("batch_id"),
		Limit:   500,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		filtered := make([]*ent.RedeemCode, 0, len(codes))
		for _, rc := range codes {
			if strings.Contains(strings.ToLower(rc.Code), q) {
				filtered = append(filtered, rc)
			}
		}
		codes = filtered
	}
	total := len(codes)
	limit, offset := parsePage(c, 500)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := codes[start:end]
	out := make([]redeemCodeView, 0, len(page))
	for _, rc := range page {
		out = append(out, toRedeemCodeView(rc))
	}
	c.JSON(http.StatusOK, gin.H{"codes": out, "total": total, "limit": limit, "offset": offset})
}

type generateRedeemReq struct {
	Kind      string  `json:"kind" binding:"required,oneof=balance plan"`
	Amount    *string `json:"amount"`
	Currency  string  `json:"currency"`
	PlanID    *string `json:"plan_id"`
	Count     int     `json:"count"`
	MaxUses   int     `json:"max_uses"`
	ExpiresAt *string `json:"expires_at"`
	BatchID   string  `json:"batch_id"`
	Note      string  `json:"note"`
}

func (h *Handler) generateRedeemCodes(c *gin.Context) {
	var req generateRedeemReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.GenerateInput{
		Kind: req.Kind, Currency: req.Currency,
		Count: req.Count, MaxUses: req.MaxUses,
		BatchID: req.BatchID, Note: req.Note,
	}
	if uid, ok := middleware.UserIDFrom(c); ok {
		in.CreatedBy = uid
	}
	if req.Amount != nil {
		d, err := decimal.NewFromString(*req.Amount)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		in.Amount = &d
	}
	if req.PlanID != nil && *req.PlanID != "" {
		pid, err := uuid.Parse(*req.PlanID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_id"})
			return
		}
		in.PlanID = &pid
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expires_at must be RFC3339"})
			return
		}
		in.ExpiresAt = &t
	}
	codes, err := h.redeem.GenerateBatch(c.Request.Context(), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	out := make([]redeemCodeView, 0, len(codes))
	for _, rc := range codes {
		out = append(out, toRedeemCodeView(rc))
	}
	c.JSON(http.StatusCreated, gin.H{"codes": out, "batch_id": codes[0].BatchID})
}

func (h *Handler) revokeRedeemCode(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.redeem.Revoke(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) revokeRedeemBatch(c *gin.Context) {
	batchID := c.Param("batch_id")
	n, err := h.redeem.RevokeBatch(c.Request.Context(), batchID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"revoked": n})
}
