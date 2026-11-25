// admin_plans.go: subscription-plan CRUD. Plan model is decimal-heavy
// (price + 5h/weekly/monthly quotas), and the update path PATCH-merges
// optional fields one decimal at a time — keeping this in its own file
// makes the helper visible at a glance.
package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
)

type planView struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Price               string   `json:"price"`
	Currency            string   `json:"currency"`
	Type                string   `json:"type"`
	PeriodDays          int      `json:"period_days"`
	Quota5h             string   `json:"quota_5h"`
	QuotaWeekly         string   `json:"quota_weekly"`
	QuotaMonthly        string   `json:"quota_monthly"`
	AllowedGroups       []string `json:"allowed_group_ids"`
	Status              string   `json:"status"`
	Description         string   `json:"description,omitempty"`
	ConcurrencyLimit    int      `json:"concurrency_limit"`
	RpmLimit            int      `json:"rpm_limit"`
	TpmLimit            int      `json:"tpm_limit"`
	MonthlySpendLimit   string   `json:"monthly_spend_limit"`
	InitialBalance      string   `json:"initial_balance"`
	AutoReloadEnabled   bool     `json:"auto_reload_enabled"`
	AutoReloadThreshold string   `json:"auto_reload_threshold"`
	AutoReloadAmount    string   `json:"auto_reload_amount"`
}

func toPlanView(p *ent.Plan) planView {
	groups := make([]string, 0, len(p.AllowedGroupIds))
	for _, g := range p.AllowedGroupIds {
		groups = append(groups, g.String())
	}
	return planView{
		ID: p.ID.String(), Name: p.Name, Price: p.Price.String(), Currency: p.Currency,
		Type: string(p.Type), PeriodDays: p.PeriodDays, Quota5h: p.Quota5h.String(),
		QuotaWeekly: p.QuotaWeekly.String(), QuotaMonthly: p.QuotaMonthly.String(),
		AllowedGroups: groups, Status: string(p.Status), Description: p.Description,
		ConcurrencyLimit:    p.ConcurrencyLimit,
		RpmLimit:            p.RpmLimit,
		TpmLimit:            p.TpmLimit,
		MonthlySpendLimit:   p.MonthlySpendLimit.String(),
		InitialBalance:      p.InitialBalance.String(),
		AutoReloadEnabled:   p.AutoReloadEnabled,
		AutoReloadThreshold: p.AutoReloadThreshold.String(),
		AutoReloadAmount:    p.AutoReloadAmount.String(),
	}
}

func (h *Handler) listPlans(c *gin.Context) {
	ps, err := h.plans.ListPlans(c.Request.Context(), false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		filtered := make([]*ent.Plan, 0, len(ps))
		for _, p := range ps {
			if strings.Contains(strings.ToLower(p.Name), q) {
				filtered = append(filtered, p)
			}
		}
		ps = filtered
	}
	total := len(ps)
	limit, offset := parsePage(c, 500)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := ps[start:end]
	out := make([]planView, 0, len(page))
	for _, p := range page {
		out = append(out, toPlanView(p))
	}
	c.JSON(http.StatusOK, gin.H{"plans": out, "total": total, "limit": limit, "offset": offset})
}

type createPlanReq struct {
	Name                string   `json:"name" binding:"required"`
	Price               string   `json:"price" binding:"required"`
	Currency            string   `json:"currency"`
	Type                string   `json:"type"`
	PeriodDays          int      `json:"period_days"`
	Quota5h             string   `json:"quota_5h"`
	QuotaWeekly         string   `json:"quota_weekly"`
	QuotaMonthly        string   `json:"quota_monthly"`
	AllowedGroupIDs     []string `json:"allowed_group_ids"`
	Description         string   `json:"description"`
	ConcurrencyLimit    int      `json:"concurrency_limit"`
	RpmLimit            int      `json:"rpm_limit"`
	TpmLimit            int      `json:"tpm_limit"`
	MonthlySpendLimit   string   `json:"monthly_spend_limit"`
	InitialBalance      string   `json:"initial_balance"`
	AutoReloadEnabled   bool     `json:"auto_reload_enabled"`
	AutoReloadThreshold string   `json:"auto_reload_threshold"`
	AutoReloadAmount    string   `json:"auto_reload_amount"`
}

func (h *Handler) createPlan(c *gin.Context) {
	var req createPlanReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	parseDec := func(s string) (decimal.Decimal, error) {
		if s == "" {
			return decimal.Zero, nil
		}
		return decimal.NewFromString(s)
	}
	price, err := parseDec(req.Price)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid price"})
		return
	}
	q5, _ := parseDec(req.Quota5h)
	qw, _ := parseDec(req.QuotaWeekly)
	qm, _ := parseDec(req.QuotaMonthly)
	msl, _ := parseDec(req.MonthlySpendLimit)
	ib, _ := parseDec(req.InitialBalance)
	art, _ := parseDec(req.AutoReloadThreshold)
	ara, _ := parseDec(req.AutoReloadAmount)
	groups := make([]uuid.UUID, 0, len(req.AllowedGroupIDs))
	for _, s := range req.AllowedGroupIDs {
		g, err := uuid.Parse(s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group id: " + s})
			return
		}
		groups = append(groups, g)
	}
	p, err := h.plans.CreatePlan(c.Request.Context(), biz.CreatePlanInput{
		Name: req.Name, Price: price, Currency: req.Currency, Type: req.Type,
		PeriodDays: req.PeriodDays, Quota5h: q5, QuotaWeekly: qw, QuotaMonthly: qm,
		AllowedGroupIDs: groups, Description: req.Description,
		ConcurrencyLimit:    req.ConcurrencyLimit,
		RpmLimit:            req.RpmLimit,
		TpmLimit:            req.TpmLimit,
		MonthlySpendLimit:   msl,
		InitialBalance:      ib,
		AutoReloadEnabled:   req.AutoReloadEnabled,
		AutoReloadThreshold: art,
		AutoReloadAmount:    ara,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyPlansActive)
	c.JSON(http.StatusCreated, gin.H{"plan": toPlanView(p)})
}

// deletePlan archives a plan by default. Pass ?hard=true to evaporate
// the row along with every UserPlan/snapshot referencing it — used by
// the admin UI's typed-confirmation "彻底删除" dialog. The default path
// (no hard flag) stays archive-only so the existing button keeps its
// safer semantics for callers that haven't been updated yet.
func (h *Handler) deletePlan(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	hard := c.Query("hard") == "true"
	if hard {
		res, err := h.plans.HardDeletePlan(c.Request.Context(), id)
		if err != nil {
			notFoundOr(c, err)
			return
		}
		h.cache.Invalidate(c.Request.Context(), cache.KeyPlansActive)
		c.JSON(http.StatusOK, gin.H{
			"hard":                    true,
			"user_plans_deleted":      res.UserPlansDeleted,
			"quota_snapshots_deleted": res.QuotaSnapshotsDeleted,
		})
		return
	}
	if err := h.plans.DeletePlan(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyPlansActive)
	c.Status(http.StatusNoContent)
}

type updatePlanReq struct {
	Name                *string   `json:"name"`
	Price               *string   `json:"price"`
	Currency            *string   `json:"currency"`
	Type                *string   `json:"type"`
	PeriodDays          *int      `json:"period_days"`
	Quota5h             *string   `json:"quota_5h"`
	QuotaWeekly         *string   `json:"quota_weekly"`
	QuotaMonthly        *string   `json:"quota_monthly"`
	AllowedGroupIDs     *[]string `json:"allowed_group_ids"`
	Description         *string   `json:"description"`
	Status              *string   `json:"status"`
	ConcurrencyLimit    *int      `json:"concurrency_limit"`
	RpmLimit            *int      `json:"rpm_limit"`
	TpmLimit            *int      `json:"tpm_limit"`
	MonthlySpendLimit   *string   `json:"monthly_spend_limit"`
	InitialBalance      *string   `json:"initial_balance"`
	AutoReloadEnabled   *bool     `json:"auto_reload_enabled"`
	AutoReloadThreshold *string   `json:"auto_reload_threshold"`
	AutoReloadAmount    *string   `json:"auto_reload_amount"`
}

func (h *Handler) updatePlan(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updatePlanReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.UpdatePlanInput{
		Name: req.Name, Currency: req.Currency, Type: req.Type, PeriodDays: req.PeriodDays,
		Description: req.Description, Status: req.Status,
		ConcurrencyLimit: req.ConcurrencyLimit,
		RpmLimit:         req.RpmLimit,
		TpmLimit:         req.TpmLimit,
	}
	parseDecPtr := func(s *string, field string) (*decimal.Decimal, bool) {
		if s == nil {
			return nil, true
		}
		d, err := decimal.NewFromString(*s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + field})
			return nil, false
		}
		return &d, true
	}
	if v, ok := parseDecPtr(req.Price, "price"); ok {
		in.Price = v
	} else {
		return
	}
	if v, ok := parseDecPtr(req.Quota5h, "quota_5h"); ok {
		in.Quota5h = v
	} else {
		return
	}
	if v, ok := parseDecPtr(req.QuotaWeekly, "quota_weekly"); ok {
		in.QuotaWeekly = v
	} else {
		return
	}
	if v, ok := parseDecPtr(req.QuotaMonthly, "quota_monthly"); ok {
		in.QuotaMonthly = v
	} else {
		return
	}
	if v, ok := parseDecPtr(req.MonthlySpendLimit, "monthly_spend_limit"); ok {
		in.MonthlySpendLimit = v
	} else {
		return
	}
	if v, ok := parseDecPtr(req.InitialBalance, "initial_balance"); ok {
		in.InitialBalance = v
	} else {
		return
	}
	if v, ok := parseDecPtr(req.AutoReloadThreshold, "auto_reload_threshold"); ok {
		in.AutoReloadThreshold = v
	} else {
		return
	}
	if v, ok := parseDecPtr(req.AutoReloadAmount, "auto_reload_amount"); ok {
		in.AutoReloadAmount = v
	} else {
		return
	}
	in.AutoReloadEnabled = req.AutoReloadEnabled
	if req.AllowedGroupIDs != nil {
		gs := make([]uuid.UUID, 0, len(*req.AllowedGroupIDs))
		for _, s := range *req.AllowedGroupIDs {
			g, err := uuid.Parse(s)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group id: " + s})
				return
			}
			gs = append(gs, g)
		}
		in.AllowedGroupIDs = &gs
	}
	p, err := h.plans.UpdatePlan(c.Request.Context(), id, in)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyPlansActive)
	c.JSON(http.StatusOK, gin.H{"plan": toPlanView(p)})
}
