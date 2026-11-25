// admin_account_pools.go: account-pool CRUD and membership.
//
// A pool is an operator-defined named group of accounts that share a pool_mode
// / quota policy. The pool↔account link is the account-pool member edge
// (ent.AccountPoolMember). The biz service exposes CountByPool but no CRUD, so
// these handlers read/write the ent client the admin Handler already holds.
package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/accountpool"
	"github.com/jucode/jucode/internal/data/ent/accountpoolmember"
)

type accountPoolView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Note         string         `json:"note,omitempty"`
	SortOrder    int            `json:"sort_order"`
	PoolMode     string         `json:"pool_mode"`
	Enabled      bool           `json:"enabled"`
	QuotaPolicy  map[string]any `json:"quota_policy,omitempty"`
	AccountCount int            `json:"account_count"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

func toAccountPoolView(p *ent.AccountPool, accountCount int) accountPoolView {
	return accountPoolView{
		ID:           p.ID.String(),
		Name:         p.Name,
		Note:         p.Note,
		SortOrder:    p.SortOrder,
		PoolMode:     string(p.PoolMode),
		Enabled:      p.Enabled,
		QuotaPolicy:  p.QuotaPolicy,
		AccountCount: accountCount,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}
}

func (h *Handler) mountAccountPools(rg *gin.RouterGroup) {
	if h.users == nil {
		return
	}
	rg.GET("/account-pools", h.listAccountPools)
	rg.POST("/account-pools", h.createAccountPool)
	rg.GET("/account-pools/:id", h.getAccountPool)
	rg.PATCH("/account-pools/:id", h.updateAccountPool)
	rg.DELETE("/account-pools/:id", h.deleteAccountPool)
	rg.GET("/account-pools/:id/accounts", h.listAccountPoolMembers)
	rg.POST("/account-pools/:id/accounts", h.addAccountPoolMember)
	rg.DELETE("/account-pools/:id/accounts/:aid", h.removeAccountPoolMember)
}

func (h *Handler) listAccountPools(c *gin.Context) {
	ctx := c.Request.Context()
	pools, err := h.users.AccountPool.Query().
		Order(ent.Asc(accountpool.FieldSortOrder), ent.Asc(accountpool.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	counts := h.poolMemberCounts(ctx, pools)
	out := make([]accountPoolView, 0, len(pools))
	for _, p := range pools {
		out = append(out, toAccountPoolView(p, counts[p.ID]))
	}
	c.JSON(http.StatusOK, gin.H{"pools": out})
}

// poolMemberCounts tallies members per pool, preferring the service projection
// when the account service is wired.
func (h *Handler) poolMemberCounts(ctx context.Context, pools []*ent.AccountPool) map[uuid.UUID]int {
	out := make(map[uuid.UUID]int, len(pools))
	if len(pools) == 0 {
		return out
	}
	ids := make([]uuid.UUID, 0, len(pools))
	for _, p := range pools {
		ids = append(ids, p.ID)
	}
	if h.accounts != nil {
		if counts, err := h.accounts.CountByPool(ctx, ids); err == nil {
			return counts
		}
	}
	rows, err := h.users.AccountPoolMember.Query().
		Where(accountpoolmember.PoolIDIn(ids...)).
		All(ctx)
	if err != nil {
		return out
	}
	for _, row := range rows {
		out[row.PoolID]++
	}
	return out
}

type accountPoolRequest struct {
	Name      string `json:"name" binding:"required"`
	Note      string `json:"note"`
	SortOrder int    `json:"sort_order"`
	PoolMode  string `json:"pool_mode"`
	Enabled   *bool  `json:"enabled"`
}

func (h *Handler) createAccountPool(c *gin.Context) {
	var req accountPoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mode, ok := parsePoolModeOrDefault(req.PoolMode)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pool_mode must be balanced, conservative, lenient or observe"})
		return
	}
	q := h.users.AccountPool.Create().
		SetName(strings.TrimSpace(req.Name)).
		SetNote(req.Note).
		SetSortOrder(req.SortOrder).
		SetPoolMode(mode)
	if req.Enabled != nil {
		q = q.SetEnabled(*req.Enabled)
	}
	p, err := q.Save(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"pool": toAccountPoolView(p, 0)})
}

func (h *Handler) getAccountPool(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	p, err := h.users.AccountPool.Get(ctx, id)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	counts := h.poolMemberCounts(ctx, []*ent.AccountPool{p})
	c.JSON(http.StatusOK, gin.H{"pool": toAccountPoolView(p, counts[p.ID])})
}

type updateAccountPoolRequest struct {
	Name      *string `json:"name"`
	Note      *string `json:"note"`
	SortOrder *int    `json:"sort_order"`
	PoolMode  *string `json:"pool_mode"`
	Enabled   *bool   `json:"enabled"`
}

func (h *Handler) updateAccountPool(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateAccountPoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := h.users.AccountPool.UpdateOneID(id)
	if req.Name != nil {
		q = q.SetName(strings.TrimSpace(*req.Name))
	}
	if req.Note != nil {
		q = q.SetNote(*req.Note)
	}
	if req.SortOrder != nil {
		q = q.SetSortOrder(*req.SortOrder)
	}
	if req.PoolMode != nil {
		mode, ok := parsePoolModeOrDefault(*req.PoolMode)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pool_mode must be balanced, conservative, lenient or observe"})
			return
		}
		q = q.SetPoolMode(mode)
	}
	if req.Enabled != nil {
		q = q.SetEnabled(*req.Enabled)
	}
	p, err := q.Save(c.Request.Context())
	if err != nil {
		notFoundOr(c, err)
		return
	}
	counts := h.poolMemberCounts(c.Request.Context(), []*ent.AccountPool{p})
	c.JSON(http.StatusOK, gin.H{"pool": toAccountPoolView(p, counts[p.ID])})
}

func (h *Handler) deleteAccountPool(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if _, err := h.users.AccountPool.Get(ctx, id); err != nil {
		notFoundOr(c, err)
		return
	}
	// Drop the membership rows first so no dangling member outlives the pool.
	if _, err := h.users.AccountPoolMember.Delete().
		Where(accountpoolmember.PoolIDEQ(id)).
		Exec(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.users.AccountPool.DeleteOneID(id).Exec(ctx); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) listAccountPoolMembers(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if _, err := h.users.AccountPool.Get(ctx, id); err != nil {
		notFoundOr(c, err)
		return
	}
	members, err := h.users.AccountPoolMember.Query().
		Where(accountpoolmember.PoolIDEQ(id)).
		Order(ent.Asc(accountpoolmember.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(members))
	for _, m := range members {
		row := gin.H{"account_id": m.AccountID.String(), "created_at": m.CreatedAt}
		if acct, err := h.users.UpstreamAccount.Get(ctx, m.AccountID); err == nil {
			row["name"] = acct.Name
			row["status"] = string(acct.Status)
			row["health"] = string(acct.Health)
			row["schedulable"] = acct.Schedulable
			row["base_url"] = acct.BaseURL
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"accounts": out})
}

type poolMemberRequest struct {
	AccountID string `json:"account_id" binding:"required"`
}

func (h *Handler) addAccountPoolMember(c *gin.Context) {
	poolID, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req poolMemberRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
		return
	}
	ctx := c.Request.Context()
	if _, err := h.users.AccountPool.Get(ctx, poolID); err != nil {
		notFoundOr(c, err)
		return
	}
	if _, err := h.users.UpstreamAccount.Get(ctx, accountID); err != nil {
		notFoundOr(c, err)
		return
	}
	// Idempotent: adding a member that already exists is a no-op success.
	exists, err := h.users.AccountPoolMember.Query().
		Where(accountpoolmember.PoolIDEQ(poolID), accountpoolmember.AccountIDEQ(accountID)).
		Exist(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !exists {
		if _, err := h.users.AccountPoolMember.Create().
			SetPoolID(poolID).
			SetAccountID(accountID).
			Save(ctx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "pool_id": poolID.String(), "account_id": accountID.String()})
}

func (h *Handler) removeAccountPoolMember(c *gin.Context) {
	poolID, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	accountID, ok := parseUUID(c, "aid")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if _, err := h.users.AccountPoolMember.Delete().
		Where(accountpoolmember.PoolIDEQ(poolID), accountpoolmember.AccountIDEQ(accountID)).
		Exec(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// parsePoolModeOrDefault validates an admin-supplied pool mode, defaulting the
// empty string to "balanced".
func parsePoolModeOrDefault(raw string) (accountpool.PoolMode, bool) {
	mode, ok := biz.ParsePoolMode(raw)
	if !ok {
		return "", false
	}
	if mode == "" {
		return accountpool.DefaultPoolMode, true
	}
	return accountpool.PoolMode(mode), true
}
