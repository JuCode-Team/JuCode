package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) listResettableUserPlans(c *gin.Context) {
	var f biz.UserPlanFilter
	f.Query = c.Query("q")
	if v := c.Query("plan_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plan_id"})
			return
		}
		f.PlanID = &id
	}
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"expire_after", &f.ExpireAfter}, {"expire_before", &f.ExpireBefore}} {
		if v := c.Query(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + p.name})
				return
			}
			*p.dst = &t
		}
	}
	limit, offset := parsePage(c, 200)
	ctx := c.Request.Context()
	ups, total, err := h.plans.ListResettableUserPlans(ctx, f, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ids := make([]uuid.UUID, len(ups))
	for i, up := range ups {
		ids[i] = up.ID
	}
	banked, err := h.plans.UnusedBankedResets(ctx, ids...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(ups))
	for _, up := range ups {
		usage, err := h.plans.PeekQuota(ctx, up)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		u, p := up.Edges.User, up.Edges.Plan
		out = append(out, gin.H{
			"user_plan_id": up.ID, "user_id": up.UserID, "email": u.Email, "username": u.Username,
			"plan_id": up.PlanID, "plan_name": p.Name, "start_at": up.StartAt, "expire_at": up.ExpireAt,
			"quota_5h": p.Quota5h.String(), "quota_weekly": p.QuotaWeekly.String(), "quota_monthly": p.QuotaMonthly.String(),
			"used_5h": usage.Used5h.String(), "used_weekly": usage.UsedWeekly.String(), "used_monthly": usage.UsedMonthly.String(),
			"banked_resets": bankedResetsJSON(banked[up.ID]),
		})
	}
	c.JSON(http.StatusOK, gin.H{"user_plans": out, "total": total, "limit": limit, "offset": offset})
}

func (h *Handler) startPlanReset(c *gin.Context) {
	var req struct {
		Action string              `json:"action" binding:"required"`
		Window string              `json:"window"`
		IDs    []uuid.UUID         `json:"ids"`
		Filter *biz.UserPlanFilter `json:"filter"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || (req.Filter == nil && len(req.IDs) == 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择要重置的套餐"})
		return
	}
	actor, _ := middleware.UserIDFrom(c)
	job, err := h.plans.StartPlanReset(c.Request.Context(), actor, req.Action, req.Window, req.IDs, req.Filter)
	switch {
	case errors.Is(err, biz.ErrInvalidUserPlan):
		c.JSON(http.StatusBadRequest, gin.H{"error": "重置方式无效"})
	case errors.Is(err, biz.ErrNoPlansToReset):
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选范围内没有有效的订阅套餐"})
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusAccepted, gin.H{"job": job})
	}
}

func (h *Handler) listPlanResetJobs(c *gin.Context) {
	jobs, err := h.plans.PlanResetJobs(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"jobs": jobs})
}

func bankedResetsJSON(rows []*ent.PlanBankedReset) []gin.H {
	out := make([]gin.H, len(rows))
	for i, r := range rows {
		out[i] = gin.H{"id": r.ID, "window": r.Window, "created_at": r.CreatedAt}
	}
	return out
}
