// admin_group_accounts.go: the per-model-group account bindings —
// /v1/admin/model-groups/:id/accounts*.
//
// A business model group binds accounts directly through ent.GroupAccount
// (group_id, account_id, priority, weight, enabled). The biz service has no
// CRUD for this edge, so these handlers read/write the ent client the admin
// Handler already holds, and overlay each bound account's health summary.
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
	entgroupaccount "github.com/jucode/jucode/internal/data/ent/groupaccount"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

// groupAccountView is one account bound to a group: the link's scheduling
// inputs (priority/weight/enabled) plus the account's health summary.
type groupAccountView struct {
	BindingID   string   `json:"binding_id"`
	GroupID     string   `json:"group_id"`
	AccountID   string   `json:"account_id"`
	AccountName string   `json:"account_name"`
	Priority    int      `json:"priority"`
	Weight      int      `json:"weight"`
	Enabled     bool     `json:"enabled"`
	Status      string   `json:"status"`
	Health      string   `json:"health"`
	Schedulable bool     `json:"schedulable"`
	BaseURL     string   `json:"base_url,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Protocols   []string `json:"protocols,omitempty"`
	// HasCredential is a presence flag only; the credential itself is never
	// returned.
	HasCredential  bool   `json:"has_credential"`
	CooldownActive bool   `json:"cooldown_active"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	SchedulableNow bool   `json:"schedulable_now"`
	// ModelNames is the optional per-group model whitelist; empty means the
	// account serves every model it is configured for.
	ModelNames []string `json:"model_names,omitempty"`
}

func (h *Handler) mountGroupAccounts(rg *gin.RouterGroup) {
	if h.users == nil {
		return
	}
	rg.GET("/model-groups/:id/accounts", h.listGroupAccounts)
	rg.POST("/model-groups/:id/accounts", h.addGroupAccount)
	rg.PATCH("/model-groups/:id/accounts/:aid", h.updateGroupAccount)
	rg.DELETE("/model-groups/:id/accounts/:aid", h.removeGroupAccount)
}

func (h *Handler) groupAccountView(ctx context.Context, link *ent.GroupAccount, acct *ent.UpstreamAccount) groupAccountView {
	view := groupAccountView{
		BindingID:  link.ID.String(),
		GroupID:    link.GroupID.String(),
		AccountID:  link.AccountID.String(),
		Priority:   link.Priority,
		Weight:     link.Weight,
		Enabled:    link.Enabled,
		ModelNames: link.ModelNames,
	}
	if acct == nil {
		return view
	}
	view.AccountName = acct.Name
	view.Status = string(acct.Status)
	view.Health = string(acct.Health)
	view.Schedulable = acct.Schedulable
	view.BaseURL = acct.BaseURL
	view.Kind = accountLegacyKind(acct)
	view.Protocols = biz.AccountProtocols(acct)
	view.HasCredential = strings.TrimSpace(acct.CredentialEnc) != ""
	view.SchedulableNow = biz.AccountSchedulable(acct, "", time.Now())
	if h.accounts != nil {
		if active, reason, _, err := h.accounts.CooldownInfo(ctx, acct.ID); err == nil && active {
			view.CooldownActive = true
			view.CooldownReason = reason
		}
	}
	return view
}

func (h *Handler) listGroupAccounts(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	links, err := h.users.GroupAccount.Query().
		Where(entgroupaccount.GroupIDEQ(gid)).
		Order(ent.Asc(entgroupaccount.FieldPriority), ent.Asc(entgroupaccount.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ids := make([]uuid.UUID, 0, len(links))
	for _, link := range links {
		ids = append(ids, link.AccountID)
	}
	byID := map[uuid.UUID]*ent.UpstreamAccount{}
	if len(ids) > 0 {
		accts, err := h.users.UpstreamAccount.Query().Where(entaccount.IDIn(ids...)).All(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, a := range accts {
			byID[a.ID] = a
		}
	}
	out := make([]groupAccountView, 0, len(links))
	for _, link := range links {
		out = append(out, h.groupAccountView(ctx, link, byID[link.AccountID]))
	}
	c.JSON(http.StatusOK, gin.H{"accounts": out})
}

type groupAccountRequest struct {
	AccountID string `json:"account_id"`
	Priority  *int   `json:"priority"`
	Weight    *int   `json:"weight"`
	Enabled   *bool  `json:"enabled"`
	// ModelNames is the optional per-group model whitelist. Absent means "no
	// change"/"serve everything the account is configured for"; an explicit
	// empty array on PATCH clears the whitelist back to all.
	ModelNames *[]string `json:"model_names"`
}

// cleanModelNames normalizes a whitelist payload: trimmed, deduplicated, nil
// when empty so the column keeps a single representation of "all models".
func cleanModelNames(raw []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (h *Handler) addGroupAccount(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req groupAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	accountID, err := uuid.Parse(strings.TrimSpace(req.AccountID))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account_id"})
		return
	}
	ctx := c.Request.Context()
	if _, err := h.users.ModelGroup.Get(ctx, gid); err != nil {
		notFoundOr(c, err)
		return
	}
	acct, err := h.users.UpstreamAccount.Get(ctx, accountID)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	priority, weight := 0, 0
	if req.Priority != nil {
		priority = *req.Priority
	}
	if req.Weight != nil {
		weight = *req.Weight
	}
	// Upsert: a second bind of the same account updates the link.
	existing, err := h.users.GroupAccount.Query().
		Where(entgroupaccount.GroupIDEQ(gid), entgroupaccount.AccountIDEQ(accountID)).
		Only(ctx)
	if err == nil {
		q := h.users.GroupAccount.UpdateOneID(existing.ID).
			SetPriority(priority).
			SetWeight(weight)
		if req.Enabled != nil {
			q = q.SetEnabled(*req.Enabled)
		}
		if req.ModelNames != nil {
			if names := cleanModelNames(*req.ModelNames); len(names) > 0 {
				q = q.SetModelNames(names)
			} else {
				q = q.ClearModelNames()
			}
		}
		updated, err := q.Save(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"account": h.groupAccountView(ctx, updated, acct)})
		return
	}
	if !ent.IsNotFound(err) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	q := h.users.GroupAccount.Create().
		SetGroupID(gid).
		SetAccountID(accountID).
		SetPriority(priority).
		SetWeight(weight)
	if req.Enabled != nil {
		q = q.SetEnabled(*req.Enabled)
	}
	if req.ModelNames != nil {
		if names := cleanModelNames(*req.ModelNames); len(names) > 0 {
			q = q.SetModelNames(names)
		}
	}
	link, err := q.Save(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"account": h.groupAccountView(ctx, link, acct)})
}

func (h *Handler) updateGroupAccount(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	aid, ok := parseUUID(c, "aid")
	if !ok {
		return
	}
	var req groupAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	link, err := h.users.GroupAccount.Query().
		Where(entgroupaccount.GroupIDEQ(gid), entgroupaccount.AccountIDEQ(aid)).
		Only(ctx)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	q := h.users.GroupAccount.UpdateOneID(link.ID)
	if req.Priority != nil {
		q = q.SetPriority(*req.Priority)
	}
	if req.Weight != nil {
		q = q.SetWeight(*req.Weight)
	}
	if req.Enabled != nil {
		q = q.SetEnabled(*req.Enabled)
	}
	if req.ModelNames != nil {
		if names := cleanModelNames(*req.ModelNames); len(names) > 0 {
			q = q.SetModelNames(names)
		} else {
			q = q.ClearModelNames()
		}
	}
	updated, err := q.Save(ctx)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	acct, _ := h.users.UpstreamAccount.Get(ctx, aid)
	c.JSON(http.StatusOK, gin.H{"account": h.groupAccountView(ctx, updated, acct)})
}

func (h *Handler) removeGroupAccount(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	aid, ok := parseUUID(c, "aid")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if _, err := h.users.GroupAccount.Delete().
		Where(entgroupaccount.GroupIDEQ(gid), entgroupaccount.AccountIDEQ(aid)).
		Exec(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
