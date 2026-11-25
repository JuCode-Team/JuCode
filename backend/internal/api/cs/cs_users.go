// cs_users.go gives the cs role enough surface to satisfy the typical
// support workflow: search a user, look at their balance / active plan,
// nudge balance (top-up or deduct), cancel / extend plans.
//
// Sensitive admin-only surfaces (API key reveal, role management, site
// settings) are deliberately NOT mirrored here. Anything mutating goes
// through the same AdminAudit middleware as the admin routes so the
// trail looks identical in admin_audit_logs.
package cs

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

func (h *Handler) listUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	limit := parseIntQuery(c, "limit", 50, 200, 1)
	offset := parseIntQuery(c, "offset", 0, 100000, 0)

	query := h.users.User.Query()
	if q != "" {
		// cs sees a slimmer view; reuse the same email-contains match the
		// admin endpoint uses so muscle memory transfers.
		query = query.Where(entuser.Or(
			entuser.EmailContainsFold(q),
			entuser.UsernameContainsFold(q),
			entuser.NicknameContainsFold(q),
		))
	}
	total, err := query.Clone().Count(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, err := query.Order(ent.Desc(entuser.FieldCreatedAt)).
		Limit(limit).Offset(offset).All(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, u := range rows {
		row := gin.H{
			"id":         u.ID.String(),
			"email":      u.Email,
			"username":   u.Username,
			"status":     u.Status,
			"balance":    u.Balance.String(),
			"currency":   u.Currency,
			"nickname":   u.Nickname,
			"created_at": u.CreatedAt,
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"users": out, "total": total,
		"limit": limit, "offset": offset,
	})
}

func (h *Handler) getUserDetail(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), id)
	if err != nil {
		if ent.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	row := gin.H{
		"id":         u.ID.String(),
		"email":      u.Email,
		"username":   u.Username,
		"status":     u.Status,
		"balance":    u.Balance.String(),
		"currency":   u.Currency,
		"nickname":   u.Nickname,
		"created_at": u.CreatedAt,
	}
	// All active+upcoming user plans so cs can pick which one to extend or cancel.
	ups, err := h.users.UserPlan.Query().
		Where(entuserplan.UserIDEQ(u.ID)).
		Order(ent.Desc(entuserplan.FieldStartAt)).
		All(c.Request.Context())
	if err == nil {
		planRows := make([]gin.H, 0, len(ups))
		for _, up := range ups {
			pr := gin.H{
				"user_plan_id": up.ID.String(),
				"plan_id":      up.PlanID.String(),
				"status":       up.Status,
				"start_at":     up.StartAt,
				"expire_at":    up.ExpireAt,
			}
			if p, err := h.users.Plan.Get(c.Request.Context(), up.PlanID); err == nil {
				pr["plan_name"] = p.Name
				pr["price"] = p.Price.String()
				pr["period_days"] = p.PeriodDays
			}
			planRows = append(planRows, pr)
		}
		row["plans"] = planRows
	}
	c.JSON(http.StatusOK, gin.H{"user": row})
}

type adjustBalanceReq struct {
	Amount string `json:"amount" binding:"required"`
	Note   string `json:"note"`
	// Kind defaults to "adjust" (signed amount). Action kinds normalize the
	// amount direction so a positive "deduct" request actually debits balance.
	Kind string `json:"kind"`
}

func signedBalanceAmount(kind string, amt decimal.Decimal) decimal.Decimal {
	switch kind {
	case "topup", "refund":
		return amt.Abs()
	case "deduct":
		return amt.Abs().Neg()
	default:
		return amt
	}
}

// adjustBalance updates the user's balance by a signed amount and writes
// a balance_txes ledger row. Caller's role decides the audit label —
// admin requests show admin, cs requests show cs.
//
// No upper bound is enforced (per user requirement). Negative amounts
// can drive the balance below zero — that's intentional for cs refund
// scenarios where the user gets debited for an over-credit.
func (h *Handler) adjustBalance(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var req adjustBalanceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	amt, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	kind := strings.TrimSpace(req.Kind)
	switch kind {
	case "topup", "refund", "deduct", "adjust", "":
		if kind == "" {
			kind = "adjust"
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be one of topup|refund|deduct|adjust"})
		return
	}
	amt = signedBalanceAmount(kind, amt)

	ctx := c.Request.Context()
	tx, err := h.users.Tx(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	u, err := tx.User.Query().Where(entuser.IDEQ(id)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	newBal := u.Balance.Add(amt)
	if _, err := tx.User.UpdateOneID(id).SetBalance(newBal).SetCurrency(biz.CurrencyCNY).Save(ctx); err != nil {
		_ = tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if _, err := tx.BalanceTx.Create().
		SetUserID(id).
		SetKind(entbalancetx.Kind(kind)).
		SetAmount(amt).
		SetBalanceAfter(newBal).
		SetCurrency(biz.CurrencyCNY).
		SetNote(req.Note).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"balance": newBal.String()})
}

// cancelUserPlan flips a user_plan to "canceled". The user keeps access
// until expire_at unless cs also nudges expire_at via /extend with a
// negative delta — but the standard cancel just stops auto-renewal /
// flags the row for admin review.
func (h *Handler) cancelUserPlan(c *gin.Context) {
	upID, err := uuid.Parse(c.Param("user_plan_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_plan_id"})
		return
	}
	up, err := h.users.UserPlan.UpdateOneID(upID).
		SetStatus(entuserplan.StatusCanceled).
		Save(c.Request.Context())
	if err != nil {
		if ent.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user_plan not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user_plan_id": up.ID.String(), "status": up.Status,
	})
}

type extendPlanReq struct {
	// Days may be negative to shorten. Caller must pick a sensible bound
	// — backend permits any int, mirroring the rest of cs powers.
	Days int `json:"days" binding:"required"`
}

// extendUserPlan bumps expire_at by Days. Used for compensation or
// support-driven SLA make-goods. No status side effects.
func (h *Handler) extendUserPlan(c *gin.Context) {
	upID, err := uuid.Parse(c.Param("user_plan_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_plan_id"})
		return
	}
	var req extendPlanReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	up, err := h.users.UserPlan.Get(c.Request.Context(), upID)
	if err != nil {
		if ent.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user_plan not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	newExpire := up.ExpireAt.AddDate(0, 0, req.Days)
	if newExpire.Before(time.Unix(0, 0)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "resulting expire_at is invalid"})
		return
	}
	up, err = h.users.UserPlan.UpdateOneID(upID).
		SetExpireAt(newExpire).
		Save(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user_plan_id": up.ID.String(),
		"expire_at":    up.ExpireAt,
	})
}
