// admin_users.go: user CRUD + per-user API keys + balance topup.
// User-status mutations also invalidate the JWT auth-cache so a ban
// propagates before the 60s TTL would naturally expire it.
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
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entusergrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergrant"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) listUsers(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	limit, offset := parsePage(c, 200)

	query := h.users.User.Query()
	if q != "" {
		query = query.Where(entuser.Or(
			entuser.EmailContainsFold(q),
			entuser.UsernameContainsFold(q),
			entuser.NicknameContainsFold(q),
		))
	}
	if s := c.Query("status"); s != "" {
		switch s {
		case "active", "banned", "pending_approval", "rejected":
			query = query.Where(entuser.StatusEQ(entuser.Status(s)))
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
			return
		}
	}

	total, err := query.Clone().Count(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	users, err := query.
		Order(ent.Desc(entuser.FieldCreatedAt)).
		Limit(limit).Offset(offset).
		All(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(users))
	for _, u := range users {
		row := gin.H{
			"id":                         u.ID.String(),
			"email":                      u.Email,
			"username":                   u.Username,
			"role":                       u.Role,
			"status":                     u.Status,
			"balance":                    u.Balance.String(),
			"currency":                   u.Currency,
			"created_at":                 u.CreatedAt,
			"nickname":                   u.Nickname,
			"concurrency_limit_override": u.ConcurrencyLimitOverride,
			"rpm_limit_override":         u.RpmLimitOverride,
			"tpm_limit_override":         u.TpmLimitOverride,
			"priority":                   u.Priority,
		}
		if u.UserGroupID != nil {
			row["user_group_id"] = u.UserGroupID.String()
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"users":  out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

func (h *Handler) getUserDetail(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), id)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	// Active plan via PlanSvc (already handles snapshot lookups).
	row := gin.H{
		"id":                         u.ID.String(),
		"email":                      u.Email,
		"username":                   u.Username,
		"role":                       u.Role,
		"status":                     u.Status,
		"balance":                    u.Balance.String(),
		"currency":                   u.Currency,
		"created_at":                 u.CreatedAt,
		"nickname":                   u.Nickname,
		"concurrency_limit_override": u.ConcurrencyLimitOverride,
		"rpm_limit_override":         u.RpmLimitOverride,
		"tpm_limit_override":         u.TpmLimitOverride,
		"priority":                   u.Priority,
	}
	if u.UserGroupID != nil {
		row["user_group_id"] = u.UserGroupID.String()
	}
	if h.roles != nil {
		if rs, err := h.roles.ListForUser(c.Request.Context(), u.ID); err == nil {
			row["roles"] = rs
		}
	}
	if up, plan, err := h.plans.ActiveUserPlan(c.Request.Context(), u.ID); err == nil && up != nil && plan != nil {
		row["active_plan"] = gin.H{
			"user_plan_id": up.ID.String(),
			"plan_id":      plan.ID.String(),
			"name":         plan.Name,
			"start_at":     up.StartAt,
			"expire_at":    up.ExpireAt,
		}
	}
	c.JSON(http.StatusOK, gin.H{"user": row})
}

func (h *Handler) listUserApikeys(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	keys, err := h.apikeys.List(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var all []uuid.UUID
	for _, item := range keys {
		all = append(all, item.Key.AllowedGroupIds...)
	}
	live, err := h.apikeys.LiveGroups(c.Request.Context(), all)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(keys))
	for _, item := range keys {
		k := item.Key
		if k.System {
			continue
		}
		out = append(out, gin.H{
			"id":                          k.ID.String(),
			"prefix":                      k.Prefix,
			"name":                        k.Name,
			"last_used_at":                k.LastUsedAt,
			"expires_at":                  k.ExpiresAt,
			"created_at":                  k.CreatedAt,
			"allowed_group_ids":           uuidStrings(biz.VisibleGroupIDs(k.AllowedGroupIds, live)),
			"allowed_groups_unrestricted": len(k.AllowedGroupIds) == 0,
			"daily_cost_limit":            k.DailyCostLimit, "monthly_cost_limit": k.MonthlyCostLimit, "total_cost_limit": k.TotalCostLimit,
			"daily_cost_used": item.DailyUsed.String(), "monthly_cost_used": item.MonthlyUsed.String(), "total_cost_used": item.TotalUsed.String(),
			"system": k.System,
		})
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": out})
}

// revealUserApikey is the admin counterpart to /me/apikeys/:id/reveal.
// Pass uuid.Nil as the owner-filter so the lookup matches any user; the
// admin middleware already gated this endpoint to role=admin.
//
// We write an admin_audit_logs row explicitly here because the audit
// middleware skips all GET requests — and this is the one GET that
// MUST land in the audit trail (it returns a plaintext API key).
func (h *Handler) revealUserApikey(c *gin.Context) {
	kid, ok := parseUUID(c, "kid")
	if !ok {
		return
	}
	targetUID := c.Param("id") // owning user id from the route
	secret, err := h.apikeys.Reveal(c.Request.Context(), kid, uuid.Nil)
	outcome := "ok"
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrApiKeyNotFound):
			outcome = "not_found"
		case errors.Is(err, biz.ErrApiKeyUnrevealable):
			outcome = "legacy_key"
		default:
			outcome = "error"
		}
	}
	if actor, found := middleware.UserIDFrom(c); found && h.audit != nil {
		h.audit.RecordAsync(biz.AuditEntry{
			ActorUserID: actor,
			Action:      "apikey.reveal",
			Target:      "user=" + targetUID + "/key=" + kid.String(),
			Payload:     map[string]any{"outcome": outcome},
			IP:          c.ClientIP(),
			UA:          c.Request.UserAgent(),
		})
	}
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrApiKeyNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		case errors.Is(err, biz.ErrApiKeyUnrevealable):
			c.JSON(http.StatusGone, gin.H{"error": err.Error(), "code": "legacy_key"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"secret": secret})
}

type adminCreateApikeyReq struct {
	Name             string     `json:"name"`
	ExpiresAt        *time.Time `json:"expires_at"`
	AllowedGroupIDs  []string   `json:"allowed_group_ids"`
	DailyCostLimit   *string    `json:"daily_cost_limit"`
	MonthlyCostLimit *string    `json:"monthly_cost_limit"`
	TotalCostLimit   *string    `json:"total_cost_limit"`
}

func (h *Handler) createUserApikey(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req adminCreateApikeyReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	in := biz.CreateInput{Name: req.Name, ExpiresAt: req.ExpiresAt}
	// Cost limits: null / "" = no limit, numeric string = set. biz.Create
	// validates the parsed values (non-negative), so we only reject
	// unparseable strings here.
	for _, lim := range []struct {
		name string
		src  *string
		dst  **decimal.Decimal
	}{
		{"daily_cost_limit", req.DailyCostLimit, &in.DailyCostLimit},
		{"monthly_cost_limit", req.MonthlyCostLimit, &in.MonthlyCostLimit},
		{"total_cost_limit", req.TotalCostLimit, &in.TotalCostLimit},
	} {
		if lim.src == nil || *lim.src == "" {
			continue
		}
		d, err := decimal.NewFromString(*lim.src)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + lim.name})
			return
		}
		*lim.dst = &d
	}
	for _, s := range req.AllowedGroupIDs {
		gid, err := uuid.Parse(s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid allowed_group_id: " + s})
			return
		}
		in.AllowedGroupIDs = append(in.AllowedGroupIDs, gid)
	}
	// Loaded before Create so a failure cannot swallow the one-time secret.
	live, err := h.apikeys.LiveGroups(c.Request.Context(), in.AllowedGroupIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	res, err := h.apikeys.Create(c.Request.Context(), uid, in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"api_key": gin.H{
			"id":                          res.Record.ID.String(),
			"prefix":                      res.Record.Prefix,
			"name":                        res.Record.Name,
			"created_at":                  res.Record.CreatedAt,
			"expires_at":                  res.Record.ExpiresAt,
			"allowed_group_ids":           uuidStrings(biz.VisibleGroupIDs(res.Record.AllowedGroupIds, live)),
			"allowed_groups_unrestricted": len(res.Record.AllowedGroupIds) == 0,
			"daily_cost_limit":            res.Record.DailyCostLimit,
			"monthly_cost_limit":          res.Record.MonthlyCostLimit,
			"total_cost_limit":            res.Record.TotalCostLimit,
		},
		"secret": res.Secret,
	})
}

func (h *Handler) revokeUserApikey(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	kid, ok := parseUUID(c, "kid")
	if !ok {
		return
	}
	if err := h.apikeys.Revoke(c.Request.Context(), uid, kid); err != nil {
		if errors.Is(err, biz.ErrApiKeySystem) {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, biz.ErrApiKeyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
			return
		}
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type topupReq struct {
	Amount string `json:"amount" binding:"required"`
	Note   string `json:"note"`
}

func (h *Handler) adminTopupUser(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req topupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	amt, err := decimal.NewFromString(req.Amount)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}
	// Run inside a transaction with SELECT FOR UPDATE so concurrent
	// topups / charges to the same user serialize and don't lose a
	// write.
	ctx := c.Request.Context()
	tx, err := h.users.Tx(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	u, err := tx.User.Query().Where(entuser.IDEQ(uid)).ForUpdate().Only(ctx)
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
	if _, err := tx.User.UpdateOneID(uid).SetBalance(newBal).SetCurrency(biz.CurrencyCNY).Save(ctx); err != nil {
		_ = tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if _, err := tx.BalanceTx.Create().
		SetUserID(uid).SetKind("topup").SetAmount(amt).SetBalanceAfter(newBal).
		SetCurrency(biz.CurrencyCNY).SetNote(req.Note).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Admin topup counts toward an any_topup/min_topup signup condition.
	h.invites.EvaluateSignupAsync(uid)
	c.JSON(http.StatusOK, gin.H{"balance": newBal.String()})
}

type updateUserReq struct {
	Email    *string `json:"email"`
	Nickname *string `json:"nickname"`
	Role     *string `json:"role"`   // user | admin
	Status   *string `json:"status"` // active | banned
	// Balance is accepted only to reject it: setting it here would write the
	// column straight through, with no balance_tx row and — because this
	// endpoint is not behind the TOTP step-up gate — no second factor, which
	// made it an exact bypass of the guarded POST /users/:id/topup. Money
	// moves through topup (a signed amount, so it both credits and debits)
	// or the CS adjust endpoint; both are transactional and audited.
	Balance                  *string `json:"balance"`
	UserGroupID              *string `json:"user_group_id"`              // "" = clear, valid uuid = set, nil = no change
	ConcurrencyLimitOverride *int    `json:"concurrency_limit_override"` // 0 = use group/plan default
	RpmLimitOverride         *int    `json:"rpm_limit_override"`         // 0 = use group/plan default
	TpmLimitOverride         *int    `json:"tpm_limit_override"`         // 0 = use group/plan default
	Priority                 *int    `json:"priority"`                   // 0-100; admin override
}

func (h *Handler) updateUser(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateUserReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Rejected loudly rather than ignored, and before any write is staged:
	// a silent drop would leave the operator believing the balance they
	// typed was applied.
	if req.Balance != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "balance cannot be set here; use POST /v1/admin/users/:id/topup (negative amount to deduct)",
			"code":  "balance_not_editable",
		})
		return
	}
	// Role and email changes hand out access that outlives the credential
	// used (a new admin, or a password-reset mailbox), so they need a
	// signed-in session rather than a personal access token.
	if _, viaToken := c.Get(middleware.CtxAccessTokenID); viaToken && (req.Role != nil || req.Email != nil) {
		c.JSON(http.StatusForbidden, gin.H{"error": "changing role or email requires a signed-in session, not an access token", "code": "session_required"})
		return
	}
	q := h.users.User.UpdateOneID(id)
	if req.Email != nil {
		q = q.SetEmail(*req.Email)
	}
	if req.Nickname != nil {
		q = q.SetNickname(*req.Nickname)
	}
	if req.Role != nil {
		if *req.Role != "user" && *req.Role != "admin" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "role must be user|admin"})
			return
		}
		q = q.SetRole(entuser.Role(*req.Role))
	}
	if req.Status != nil {
		switch *req.Status {
		case "active", "banned", "pending_approval", "rejected":
			q = q.SetStatus(entuser.Status(*req.Status))
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "status must be active|banned|pending_approval|rejected"})
			return
		}
	}
	if req.UserGroupID != nil {
		if *req.UserGroupID == "" {
			q = q.ClearUserGroup()
		} else {
			gid, err := uuid.Parse(*req.UserGroupID)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_group_id"})
				return
			}
			q = q.SetUserGroupID(gid)
		}
	}
	if req.ConcurrencyLimitOverride != nil {
		if *req.ConcurrencyLimitOverride < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "concurrency_limit_override must be >= 0"})
			return
		}
		q = q.SetConcurrencyLimitOverride(*req.ConcurrencyLimitOverride)
	}
	if req.RpmLimitOverride != nil {
		if *req.RpmLimitOverride < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "rpm_limit_override must be >= 0"})
			return
		}
		q = q.SetRpmLimitOverride(*req.RpmLimitOverride)
	}
	if req.TpmLimitOverride != nil {
		if *req.TpmLimitOverride < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tpm_limit_override must be >= 0"})
			return
		}
		q = q.SetTpmLimitOverride(*req.TpmLimitOverride)
	}
	if req.Priority != nil {
		if *req.Priority < 0 || *req.Priority > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "priority must be in [0, 100]"})
			return
		}
		q = q.SetPriority(*req.Priority)
	}
	u, err := q.Save(c.Request.Context())
	if err != nil {
		notFoundOr(c, err)
		return
	}
	// Status / role may have changed → drop the auth cache so the next
	// JWT request observes the new value instead of waiting out the TTL.
	if h.account != nil && (req.Status != nil || req.Role != nil) {
		h.account.InvalidateUserActiveCache(c.Request.Context(), id)
	}
	if h.roles != nil && req.Role != nil {
		h.roles.InvalidateUserRoleCache(c.Request.Context(), id)
	}
	c.JSON(http.StatusOK, gin.H{
		"id": u.ID.String(), "email": u.Email, "role": u.Role, "status": u.Status,
		"balance": u.Balance.String(), "currency": u.Currency,
	})
}

// setUserRoles replaces a user's functional roles with the
// posted list. Empty list = strip every functional role. Admin (primary)
// role is unaffected — it's still mutated via PATCH /users/:id `role`.
//
// The diff (grant vs revoke) is recorded into admin_audit_logs via the
// surrounding middleware; we also invalidate the per-user role cache so
// the next request observes the change without waiting out the TTL.
type setRolesReq struct {
	Roles []string `json:"roles"`
}

func (h *Handler) setUserRoles(c *gin.Context) {
	if h.roles == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "role service unavailable"})
		return
	}
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req setRolesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	wanted := map[string]bool{}
	for _, r := range req.Roles {
		switch r {
		case biz.RoleCS, biz.RoleAgent:
			wanted[r] = true
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "role must be one of cs|agent"})
			return
		}
	}
	ctx := c.Request.Context()
	existing, err := h.roles.ListForUser(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	existingSet := map[string]bool{}
	for _, r := range existing {
		existingSet[r] = true
	}
	actor, _ := middleware.UserIDFrom(c)
	// Revoke roles no longer present.
	for r := range existingSet {
		if !wanted[r] {
			if err := h.roles.Revoke(ctx, id, r); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
	}
	// Grant new ones.
	for r := range wanted {
		if !existingSet[r] {
			if err := h.roles.Grant(ctx, id, r, actor); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
	}
	if wanted[biz.RoleAgent] && h.agents != nil {
		if _, err := h.agents.EnsureProfile(ctx, id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	out, err := h.roles.ListForUser(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": out})
}

func (h *Handler) getUserModelGroupGrants(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ids, err := h.models.UserModelGroupGrants(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"group_ids": uuidStrings(ids)})
}

func (h *Handler) putUserModelGroupGrants(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req modelGroupGrantsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	gids, ok := parseUUIDList(c, req.GroupIDs)
	if !ok {
		return
	}
	if err := h.models.ReplaceUserModelGroupGrants(c.Request.Context(), id, gids); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.cache != nil {
		h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat, cache.KeyStatusSnapshot)
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) deleteUser(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if _, err := h.users.ModelGroupUserGrant.Delete().Where(entusergrant.UserIDEQ(id)).Exec(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.users.User.DeleteOneID(id).Exec(c.Request.Context()); err != nil {
		notFoundOr(c, err)
		return
	}
	if h.account != nil {
		h.account.InvalidateUserActiveCache(c.Request.Context(), id)
	}
	c.Status(http.StatusNoContent)
}

// approveUser flips a pending_approval row to active. Idempotent against
// already-active users so the admin can confirm without checking state.
// Refuses to "approve" banned / rejected users (use updateUser to override).
func (h *Handler) approveUser(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), id)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	switch u.Status {
	case entuser.StatusActive:
		c.JSON(http.StatusOK, gin.H{
			"id": u.ID.String(), "email": u.Email, "status": u.Status,
		})
		return
	case entuser.StatusPendingApproval:
		// fall through
	default:
		c.JSON(http.StatusConflict, gin.H{
			"error": "user is " + string(u.Status) + ", refuse to overwrite; use PATCH /users/:id to force",
		})
		return
	}
	u, err = h.users.User.UpdateOneID(id).
		SetStatus(entuser.StatusActive).
		Save(c.Request.Context())
	if err != nil {
		notFoundOr(c, err)
		return
	}
	if h.account != nil {
		h.account.InvalidateUserActiveCache(c.Request.Context(), id)
	}
	// Approval makes the account usable — a pending signup reward whose
	// condition is already met may now qualify.
	h.invites.EvaluateSignupAsync(id)
	c.JSON(http.StatusOK, gin.H{
		"id": u.ID.String(), "email": u.Email, "status": u.Status,
	})
}

// rejectUser marks a pending_approval row as rejected. We keep the row so
// the same email cannot silently re-register; admin can reverse via PATCH.
func (h *Handler) rejectUser(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), id)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	if u.Status != entuser.StatusPendingApproval {
		c.JSON(http.StatusConflict, gin.H{
			"error": "user is " + string(u.Status) + ", refuse to overwrite; use PATCH /users/:id to force",
		})
		return
	}
	u, err = h.users.User.UpdateOneID(id).
		SetStatus(entuser.StatusRejected).
		Save(c.Request.Context())
	if err != nil {
		notFoundOr(c, err)
		return
	}
	if h.account != nil {
		h.account.InvalidateUserActiveCache(c.Request.Context(), id)
	}
	c.JSON(http.StatusOK, gin.H{
		"id": u.ID.String(), "email": u.Email, "status": u.Status,
	})
}

func (h *Handler) updateUserApikey(c *gin.Context) {
	uid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	kid, ok := parseUUID(c, "kid")
	if !ok {
		return
	}
	var raw map[string]*string
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	in := biz.UpdateInput{}
	assign := func(name string, dst ***decimal.Decimal) error {
		v, ok := raw[name]
		if !ok {
			return nil
		}
		if v == nil {
			var x *decimal.Decimal
			*dst = &x
			return nil
		}
		d, e := decimal.NewFromString(*v)
		if e != nil {
			return e
		}
		x := &d
		*dst = &x
		return nil
	}
	if err := assign("daily_cost_limit", &in.DailyCostLimit); err != nil {
		c.JSON(400, gin.H{"error": "invalid cost limit"})
		return
	}
	if err := assign("monthly_cost_limit", &in.MonthlyCostLimit); err != nil {
		c.JSON(400, gin.H{"error": "invalid cost limit"})
		return
	}
	if err := assign("total_cost_limit", &in.TotalCostLimit); err != nil {
		c.JSON(400, gin.H{"error": "invalid cost limit"})
		return
	}
	rec, err := h.apikeys.Update(c.Request.Context(), uid, kid, in)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(200, gin.H{"api_key": rec})
}
