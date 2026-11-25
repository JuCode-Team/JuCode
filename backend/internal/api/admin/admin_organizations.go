package admin

import (
	"errors"
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

type organizationView struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	OwnerUserID       string    `json:"owner_user_id"`
	Balance           string    `json:"balance"`
	Currency          string    `json:"currency"`
	MonthlySpendLimit string    `json:"monthly_spend_limit"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Members           []gin.H   `json:"members,omitempty"`
}

func toOrganizationView(org *ent.Organization, members []*ent.OrganizationMember) organizationView {
	v := organizationView{
		ID:                org.ID.String(),
		Name:              org.Name,
		OwnerUserID:       org.OwnerUserID.String(),
		Balance:           org.Balance.String(),
		Currency:          org.Currency,
		MonthlySpendLimit: org.MonthlySpendLimit.String(),
		CreatedAt:         org.CreatedAt,
		UpdatedAt:         org.UpdatedAt,
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

func (h *Handler) listOrganizations(c *gin.Context) {
	limit, offset := parsePage(c, 200)
	orgs, total, err := h.organizations.List(c.Request.Context(), c.Query("q"), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]organizationView, 0, len(orgs))
	for _, org := range orgs {
		out = append(out, toOrganizationView(org, nil))
	}
	c.JSON(http.StatusOK, gin.H{"organizations": out, "total": total, "limit": limit, "offset": offset})
}

type createOrganizationReq struct {
	Name              string `json:"name" binding:"required"`
	OwnerUserID       string `json:"owner_user_id" binding:"required"`
	MonthlySpendLimit string `json:"monthly_spend_limit"`
}

func (h *Handler) createOrganization(c *gin.Context) {
	var req createOrganizationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ownerID, err := uuid.Parse(req.OwnerUserID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid owner_user_id"})
		return
	}
	limit, ok := parseOptionalDecimal(c, req.MonthlySpendLimit, "monthly_spend_limit")
	if !ok {
		return
	}
	org, err := h.organizations.Create(c.Request.Context(), biz.CreateOrganizationInput{
		Name:              req.Name,
		OwnerUserID:       ownerID,
		MonthlySpendLimit: limit,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	members, _ := h.organizations.Members(c.Request.Context(), org.ID)
	c.JSON(http.StatusCreated, gin.H{"organization": toOrganizationView(org, members)})
}

func (h *Handler) getOrganization(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	org, err := h.organizations.Get(c.Request.Context(), id)
	if err != nil {
		organizationError(c, err)
		return
	}
	members, err := h.organizations.Members(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"organization": toOrganizationView(org, members)})
}

type updateOrganizationReq struct {
	Name              *string `json:"name"`
	OwnerUserID       *string `json:"owner_user_id"`
	MonthlySpendLimit *string `json:"monthly_spend_limit"`
}

func (h *Handler) updateOrganization(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateOrganizationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if _, viaToken := c.Get(middleware.CtxAccessTokenID); viaToken && req.OwnerUserID != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "changing an organization's owner requires a signed-in session, not an access token", "code": "session_required"})
		return
	}
	in := biz.UpdateOrganizationInput{Name: req.Name}
	if req.OwnerUserID != nil {
		ownerID, err := uuid.Parse(*req.OwnerUserID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid owner_user_id"})
			return
		}
		in.OwnerUserID = &ownerID
	}
	if req.MonthlySpendLimit != nil {
		limit, err := decimal.NewFromString(strings.TrimSpace(*req.MonthlySpendLimit))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid monthly_spend_limit"})
			return
		}
		in.MonthlySpendLimit = &limit
	}
	org, err := h.organizations.Update(c.Request.Context(), id, in)
	if err != nil {
		organizationError(c, err)
		return
	}
	members, _ := h.organizations.Members(c.Request.Context(), id)
	c.JSON(http.StatusOK, gin.H{"organization": toOrganizationView(org, members)})
}

type topupOrganizationReq struct {
	Amount string `json:"amount" binding:"required"`
	Note   string `json:"note"`
}

func (h *Handler) topupOrganization(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req topupOrganizationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil || amount.Sign() <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	balance, err := h.organizations.Grant(c.Request.Context(), id, nil, amount, req.Note)
	if err != nil {
		organizationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"balance": balance.String()})
}

type upsertOrganizationMemberReq struct {
	Role              string `json:"role"`
	MonthlySpendLimit string `json:"monthly_spend_limit"`
	TotalSpendLimit   string `json:"total_spend_limit"`
}

func (h *Handler) upsertOrganizationMember(c *gin.Context) {
	orgID, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	userID, ok := parseUUID(c, "uid")
	if !ok {
		return
	}
	var req upsertOrganizationMemberReq
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
		UserID:            userID,
		Role:              req.Role,
		MonthlySpendLimit: limit,
		TotalSpendLimit:   totalLimit,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) removeOrganizationMember(c *gin.Context) {
	orgID, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	userID, ok := parseUUID(c, "uid")
	if !ok {
		return
	}
	if err := h.organizations.RemoveMember(c.Request.Context(), orgID, userID); err != nil {
		organizationError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
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

func organizationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrOrganizationNotFound), ent.IsNotFound(err):
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
	case errors.Is(err, biz.ErrOrganizationForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
