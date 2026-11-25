package user

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entbillinghold "github.com/jucode/jucode/internal/data/ent/billinghold"
	entmember "github.com/jucode/jucode/internal/data/ent/organizationmember"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/middleware"
)

type myOrganizationView struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	OwnerUserID       string    `json:"owner_user_id"`
	Balance           string    `json:"balance"`
	Currency          string    `json:"currency"`
	MonthlySpendLimit string    `json:"monthly_spend_limit"`
	CreatedAt         time.Time `json:"created_at"`
	// Role is the querying user's own role in this organization
	// (owner|admin|member); only set on the list endpoint.
	Role    string  `json:"role,omitempty"`
	Members []gin.H `json:"members,omitempty"`
}

func toMyOrganizationView(org *ent.Organization, members []*ent.OrganizationMember) myOrganizationView {
	v := myOrganizationView{
		ID:                org.ID.String(),
		Name:              org.Name,
		OwnerUserID:       org.OwnerUserID.String(),
		Balance:           org.Balance.String(),
		Currency:          org.Currency,
		MonthlySpendLimit: org.MonthlySpendLimit.String(),
		CreatedAt:         org.CreatedAt,
	}
	if members != nil {
		v.Members = make([]gin.H, 0, len(members))
		for _, m := range members {
			row := gin.H{
				"user_id":             m.UserID.String(),
				"role":                m.Role,
				"monthly_spend_limit": m.MonthlySpendLimit.String(),
				"total_spend_limit":   m.TotalSpendLimit.String(),
				"created_at":          m.CreatedAt,
			}
			if u, err := m.Edges.UserOrErr(); err == nil {
				row["email"] = u.Email
				row["nickname"] = u.Nickname
			}
			v.Members = append(v.Members, row)
		}
	}
	return v
}

func (h *Handler) listMyOrganizations(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	orgs, err := h.organizations.ListForUser(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]myOrganizationView, 0, len(orgs))
	for _, m := range orgs {
		v := toMyOrganizationView(m.Org, nil)
		v.Role = m.Role
		out = append(out, v)
	}
	c.JSON(http.StatusOK, gin.H{"organizations": out})
}

func (h *Handler) getMyOrganization(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	orgID, ok := parseRouteUUID(c, "id")
	if !ok {
		return
	}
	if _, err := h.organizations.Member(c.Request.Context(), orgID, uid); err != nil {
		myOrganizationError(c, err)
		return
	}
	org, err := h.organizations.Get(c.Request.Context(), orgID)
	if err != nil {
		myOrganizationError(c, err)
		return
	}
	members, err := h.organizations.Members(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"organization": toMyOrganizationView(org, members)})
}

type updateMyOrganizationReq struct {
	Name              *string `json:"name"`
	MonthlySpendLimit *string `json:"monthly_spend_limit"`
}

func (h *Handler) updateMyOrganization(c *gin.Context) {
	_, orgID, ok := h.requireOrganizationManager(c)
	if !ok {
		return
	}
	var req updateMyOrganizationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.UpdateOrganizationInput{Name: req.Name}
	if req.MonthlySpendLimit != nil {
		limit, err := decimal.NewFromString(strings.TrimSpace(*req.MonthlySpendLimit))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid monthly_spend_limit"})
			return
		}
		in.MonthlySpendLimit = &limit
	}
	org, err := h.organizations.Update(c.Request.Context(), orgID, in)
	if err != nil {
		myOrganizationError(c, err)
		return
	}
	members, err := h.organizations.Members(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"organization": toMyOrganizationView(org, members)})
}

type topupMyOrganizationReq struct {
	Amount string `json:"amount" binding:"required"`
	Note   string `json:"note"`
}

func (h *Handler) topupMyOrganization(c *gin.Context) {
	uid, orgID, ok := h.requireOrganizationManager(c)
	if !ok {
		return
	}
	var req topupMyOrganizationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil || amount.Sign() <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	note := strings.TrimSpace(req.Note)
	if note == "" {
		note = "组织充值"
	}
	orgBalance, personalBalance, err := h.organizations.TransferFromMember(c.Request.Context(), orgID, uid, amount, note)
	if err != nil {
		myOrganizationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"balance":          orgBalance.String(),
		"personal_balance": personalBalance.String(),
	})
}

type upsertMyOrganizationMemberReq struct {
	Role              string `json:"role"`
	MonthlySpendLimit string `json:"monthly_spend_limit"`
	TotalSpendLimit   string `json:"total_spend_limit"`
}

func (h *Handler) upsertMyOrganizationMember(c *gin.Context) {
	_, orgID, ok := h.requireOrganizationManager(c)
	if !ok {
		return
	}
	memberID, ok := parseRouteUUID(c, "uid")
	if !ok {
		return
	}
	var req upsertMyOrganizationMemberReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	limit, ok := parseOptionalDecimal(c, req.MonthlySpendLimit, "monthly_spend_limit")
	if !ok {
		return
	}
	totalLimit, ok := parseOptionalDecimal(c, req.TotalSpendLimit, "total_spend_limit")
	if !ok {
		return
	}
	if err := h.organizations.UpsertMember(c.Request.Context(), orgID, biz.OrganizationMemberInput{
		UserID:            memberID,
		Role:              req.Role,
		MonthlySpendLimit: limit,
		TotalSpendLimit:   totalLimit,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) removeMyOrganizationMember(c *gin.Context) {
	_, orgID, ok := h.requireOrganizationManager(c)
	if !ok {
		return
	}
	memberID, ok := parseRouteUUID(c, "uid")
	if !ok {
		return
	}
	if err := h.organizations.RemoveMember(c.Request.Context(), orgID, memberID); err != nil {
		myOrganizationError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// getMyOrganizationOverview powers the console overview page in org
// context. Any member may call it; the org-wide financials (balance,
// total monthly spend, counts) are only included for owners/admins —
// members only learn their own spend against their own limit.
func (h *Handler) getMyOrganizationOverview(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	orgID, ok := parseRouteUUID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	m, err := h.organizations.Member(ctx, orgID, uid)
	if err != nil {
		myOrganizationError(c, err)
		return
	}
	org, err := h.organizations.Get(ctx, orgID)
	if err != nil {
		myOrganizationError(c, err)
		return
	}
	mine, err := h.organizations.MemberSpendFor(ctx, orgID, uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{
		"organization": gin.H{
			"id":         org.ID.String(),
			"name":       org.Name,
			"currency":   org.Currency,
			"created_at": org.CreatedAt,
		},
		"my_role":                string(m.Role),
		"my_monthly_spend":       mine.Month.String(),
		"my_monthly_spend_limit": m.MonthlySpendLimit.String(),
		"my_total_spend":         mine.Total.String(),
		"my_total_spend_limit":   m.TotalSpendLimit.String(),
	}
	if middleware.IsOrgManager(string(m.Role)) {
		orgSpend, err := h.organizations.MonthlyDeducted(ctx, orgID, nil)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		memberCount, err := h.users.OrganizationMember.Query().
			Where(entmember.OrganizationIDEQ(orgID)).Count(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		apiKeyCount, err := h.users.ApiKey.Query().
			Where(entapikey.OrganizationIDEQ(orgID), entapikey.RevokedAtIsNil()).Count(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		held, err := h.heldAmount(ctx, entbillinghold.And(
			entbillinghold.OrganizationIDEQ(orgID), entbillinghold.ScopeEQ(entbillinghold.ScopeOrganization)))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		resp["balance"] = org.Balance.String()
		resp["held_balance"] = held.String()
		resp["monthly_spend_limit"] = org.MonthlySpendLimit.String()
		resp["monthly_spend"] = orgSpend.String()
		resp["member_count"] = memberCount
		resp["api_key_count"] = apiKeyCount
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) getMyOrganizationBillingSummary(c *gin.Context) {
	_, orgID, ok := h.requireOrganizationManager(c)
	if !ok {
		return
	}
	from, to := parseBillingRange(c)
	sum, err := h.organizations.BillingSummary(c.Request.Context(), orgID, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ledgerSummaryJSON(sum))
}

// listMyOrganizationBilling pages the org's balance ledger (topups,
// per-request deducts, adjusts). Manager-only: balance_after reveals the
// org's running balance, which members must not see.
func (h *Handler) listMyOrganizationBilling(c *gin.Context) {
	_, orgID, ok := h.requireOrganizationManager(c)
	if !ok {
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	ctx := c.Request.Context()
	from, to := parseBillingRange(c)
	rows, total, err := h.organizations.ListBalanceTxs(ctx, orgID, biz.OrgBalanceTxFilter{
		Kind:   c.Query("kind"),
		Q:      c.Query("q"),
		From:   from,
		To:     to,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Resolve actor emails in one batch so the ledger can show who topped
	// up / spent without an N+1.
	actorIDs := make([]uuid.UUID, 0, len(rows))
	seen := map[uuid.UUID]struct{}{}
	for _, r := range rows {
		if r.ActorUserID != nil {
			if _, dup := seen[*r.ActorUserID]; !dup {
				seen[*r.ActorUserID] = struct{}{}
				actorIDs = append(actorIDs, *r.ActorUserID)
			}
		}
	}
	emailByUser := map[uuid.UUID]string{}
	if len(actorIDs) > 0 {
		users, err := h.users.User.Query().Where(entuser.IDIn(actorIDs...)).All(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, u := range users {
			emailByUser[u.ID] = u.Email
		}
	}
	notes := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Kind == "deduct" {
			notes = append(notes, r.Note)
		}
	}
	holds, err := h.settleHoldAmounts(ctx, entbillinghold.OrganizationIDEQ(orgID), notes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		row := gin.H{
			"id":            r.ID.String(),
			"created_at":    r.CreatedAt,
			"kind":          r.Kind,
			"amount":        r.Amount.String(),
			"balance_after": r.BalanceAfter.String(),
			"currency":      r.Currency,
			"note":          r.Note,
		}
		if id, ok := requestIDFromLedgerNote(r.Note); ok {
			if amt, found := holds[id]; found {
				row["hold_amount"] = amt.String()
			}
		}
		if r.ActorUserID != nil {
			row["actor_user_id"] = r.ActorUserID.String()
			row["actor_email"] = emailByUser[*r.ActorUserID]
		}
		if r.RelatedUsageLogID != nil {
			row["related_usage_log_id"] = r.RelatedUsageLogID.String()
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *Handler) requireOrganizationManager(c *gin.Context) (uuid.UUID, uuid.UUID, bool) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return uuid.Nil, uuid.Nil, false
	}
	orgID, ok := parseRouteUUID(c, "id")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	canManage, err := h.organizations.CanManage(c.Request.Context(), orgID, uid)
	if err != nil {
		myOrganizationError(c, err)
		return uuid.Nil, uuid.Nil, false
	}
	if !canManage {
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed"})
		return uuid.Nil, uuid.Nil, false
	}
	return uid, orgID, true
}

func parseRouteUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return uuid.Nil, false
	}
	return id, true
}

func parseOptionalDecimal(c *gin.Context, raw, field string) (decimal.Decimal, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return decimal.Zero, true
	}
	v, err := decimal.NewFromString(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + field})
		return decimal.Zero, false
	}
	return v, true
}

func myOrganizationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrOrganizationNotFound), ent.IsNotFound(err):
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
	case errors.Is(err, biz.ErrOrganizationForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed"})
	case errors.Is(err, biz.ErrInsufficientBalance):
		// The manager is funding the org out of their own balance, so this is
		// "top up your personal account first", not an org-level problem.
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error": "insufficient personal balance",
			"code":  "insufficient_personal_balance",
		})
	case errors.Is(err, biz.ErrInvalidAmount):
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount must be positive"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
