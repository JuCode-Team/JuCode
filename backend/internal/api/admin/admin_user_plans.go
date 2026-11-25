package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
	"github.com/shopspring/decimal"
)

func (h *Handler) listUserPlans(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if _, err := h.users.User.Get(ctx, uid); err != nil {
		notFoundOr(c, err)
		return
	}
	ups, err := h.users.UserPlan.Query().Where(entuserplan.UserIDEQ(uid)).WithPlan().Order(ent.Desc(entuserplan.FieldStartAt)).All(ctx)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	out := make([]gin.H, 0, len(ups))
	now := time.Now()
	current := false
	for _, up := range ups {
		p := up.Edges.Plan
		row := gin.H{"user_plan_id": up.ID, "plan_id": up.PlanID, "name": p.Name, "type": p.Type, "status": up.Status,
			"start_at": up.StartAt, "expire_at": up.ExpireAt, "quota_5h": p.Quota5h.String(), "quota_weekly": p.QuotaWeekly.String(),
			"quota_monthly": p.QuotaMonthly.String(), "monthly_spend_limit": p.MonthlySpendLimit.String(), "current": false}
		windows := biz.QuotaWindowsAt(up, now)
		row["reset_5h_at"], row["reset_weekly_at"], row["reset_monthly_at"] = windows[0].End, windows[1].End, windows[2].End
		if !current && up.Status == entuserplan.StatusActive && up.ExpireAt.After(now) {
			row["current"] = true
			current = true
		}
		if p.Type == "quota_subscription" {
			usage, err := h.plans.PeekQuota(ctx, up)
			if err != nil {
				notFoundOr(c, err)
				return
			}
			row["used_5h"], row["used_weekly"], row["used_monthly"] = usage.Used5h.String(), usage.UsedWeekly.String(), usage.UsedMonthly.String()
		} else {
			used, err := h.plans.PayAsYouGoUsageThisMonth(ctx, up)
			if err != nil {
				notFoundOr(c, err)
				return
			}
			row["used_monthly"] = used.String()
		}
		var requests int64
		var cost decimal.Decimal
		err := h.rawDB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(cost_final), 0) FROM usage_logs WHERE user_plan_id = $1 AND billing_source IN ('plan', 'plan_balance') AND status = 'ok'`, up.ID).Scan(&requests, &cost)
		if err != nil {
			notFoundOr(c, err)
			return
		}
		row["requests"], row["total_cost"] = requests, cost.String()
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"plans": out})
}

type userPlanReq struct {
	RequestID uuid.UUID  `json:"request_id"`
	PlanID    *uuid.UUID `json:"plan_id"`
	ExpireAt  *time.Time `json:"expire_at"`
}

func userPlanError(c *gin.Context, err error) {
	if errors.Is(err, biz.ErrInvalidUserPlan) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "套餐调整无效：请选择可用的同类套餐，到期时间须晚于当前时间；已删除套餐不能修改"})
	} else {
		notFoundOr(c, err)
	}
}

func (h *Handler) grantUserPlan(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req userPlanReq
	if err := c.ShouldBindJSON(&req); err != nil || req.PlanID == nil || *req.PlanID == uuid.Nil || req.RequestID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择套餐并填写有效时间"})
		return
	}
	up, err := h.plans.GrantUserPlan(c.Request.Context(), uid, *req.PlanID, req.RequestID, req.ExpireAt)
	if err != nil {
		userPlanError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"user_plan_id": up.ID})
}

func (h *Handler) updateUserPlan(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	id, ok := parseUUID(c, "upid")
	if !ok {
		return
	}
	var req userPlanReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	up, err := h.plans.UpdateUserPlan(c.Request.Context(), uid, id, req.PlanID, req.ExpireAt, false)
	if err != nil {
		userPlanError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user_plan_id": up.ID})
}

func (h *Handler) deleteUserPlan(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	id, ok := parseUUID(c, "upid")
	if !ok {
		return
	}
	if _, err := h.plans.UpdateUserPlan(c.Request.Context(), uid, id, nil, nil, true); err != nil {
		userPlanError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) resetUserPlanQuota(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	id, ok := parseUUID(c, "upid")
	if !ok {
		return
	}
	var req struct {
		Window string `json:"window" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	up, err := h.users.UserPlan.Query().Where(entuserplan.IDEQ(id), entuserplan.UserIDEQ(uid)).WithPlan().Only(c.Request.Context())
	if err != nil {
		notFoundOr(c, err)
		return
	}
	if up.Status != entuserplan.StatusActive || !up.ExpireAt.After(time.Now()) || up.Edges.Plan.Type != "quota_subscription" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅有效的订阅套餐支持额度重置"})
		return
	}
	if err := h.plans.ResetQuota(c.Request.Context(), up, req.Window); err != nil {
		userPlanError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
