// admin_invite.go: invite-program moderation — list bindings/rewards/rebates
// and pay / reject / void actions. All mutating routes are recorded by the
// AdminAudit middleware; the handlers themselves stay thin over InviteSvc.
package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

// inviteFilterFromQuery maps the shared list query params (q / status /
// inviter_id / invitee_id / limit / offset) onto the biz filter. Returns
// ok=false after writing a 400 for malformed UUIDs.
func inviteFilterFromQuery(c *gin.Context) (biz.InviteAdminFilter, bool) {
	limit, offset := parsePage(c, 200)
	f := biz.InviteAdminFilter{
		Q:      strings.TrimSpace(c.Query("q")),
		Status: strings.TrimSpace(c.Query("status")),
		Limit:  limit,
		Offset: offset,
	}
	for name, dst := range map[string]**uuid.UUID{
		"inviter_id": &f.InviterID,
		"invitee_id": &f.InviteeID,
	} {
		if v := strings.TrimSpace(c.Query(name)); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
				return f, false
			}
			*dst = &id
		}
	}
	return f, true
}

// requireInvites guards routes when the invite service was not wired
// (should not happen in main; possible in stripped-down test routers).
func (h *Handler) requireInvites(c *gin.Context) bool {
	if h.invites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invite service not available"})
		return false
	}
	return true
}

func inviteErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrInviteNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, biz.ErrInviteInvalidState):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (h *Handler) listInviteBindings(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	f, ok := inviteFilterFromQuery(c)
	if !ok {
		return
	}
	rows, total, err := h.invites.ListBindings(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, inviteBindingView(r))
	}
	c.JSON(http.StatusOK, gin.H{"bindings": out, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func inviteBindingView(r biz.InviteAdminBinding) gin.H {
	b := r.Binding
	inviterID := b.InviterUserID.String()
	inviteeID := b.InviteeUserID.String()
	return gin.H{
		"id":              b.ID.String(),
		"inviter_id":      inviterID,
		"invitee_id":      inviteeID,
		"inviter_user_id": inviterID,
		"invitee_user_id": inviteeID,
		"inviter_email":   r.InviterEmail,
		"invitee_email":   r.InviteeEmail,
		"invite_code":     b.InviteCodeUsed,
		"ip":              b.RegisterIP,
		"register_ip":     b.RegisterIP,
		"status":          b.Status,
		"created_at":      b.CreatedAt,
	}
}

func (h *Handler) listInviteRewards(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	f, ok := inviteFilterFromQuery(c)
	if !ok {
		return
	}
	rows, total, err := h.invites.ListRewards(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, inviteRewardView(r.Reward, r.InviterEmail, r.InviteeEmail))
	}
	c.JSON(http.StatusOK, gin.H{"rewards": out, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func inviteRewardView(r *ent.InviteReward, inviterEmail, inviteeEmail string) gin.H {
	inviterID := r.InviterUserID.String()
	inviteeID := r.InviteeUserID.String()
	row := gin.H{
		"id":                 r.ID.String(),
		"binding_id":         r.BindingID.String(),
		"inviter_id":         inviterID,
		"invitee_id":         inviteeID,
		"inviter_user_id":    inviterID,
		"invitee_user_id":    inviteeID,
		"inviter_email":      inviterEmail,
		"invitee_email":      inviteeEmail,
		"amount":             r.Amount.String(),
		"currency":           r.Currency,
		"condition_snapshot": r.ConditionSnapshot,
		"status":             r.Status,
		"risk_flags":         r.RiskFlags,
		"note":               r.Note,
		"created_at":         r.CreatedAt,
	}
	if r.PaidAt != nil {
		row["paid_at"] = r.PaidAt
	}
	if r.PaidBalanceTxID != nil {
		row["paid_balance_tx_id"] = r.PaidBalanceTxID.String()
	}
	return row
}

func (h *Handler) listInviteRebates(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	f, ok := inviteFilterFromQuery(c)
	if !ok {
		return
	}
	rows, total, err := h.invites.ListRebates(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, inviteRebateView(r.Rebate, r.InviterEmail, r.InviteeEmail))
	}
	c.JSON(http.StatusOK, gin.H{"rebates": out, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func inviteRebateView(r *ent.InviteRebate, inviterEmail, inviteeEmail string) gin.H {
	inviterID := r.InviterUserID.String()
	inviteeID := r.InviteeUserID.String()
	amount := r.RebateAmount.String()
	base := r.SourceAmount.String()
	rate := r.RatePctSnapshot.String()
	row := gin.H{
		"id":                 r.ID.String(),
		"inviter_id":         inviterID,
		"invitee_id":         inviteeID,
		"inviter_user_id":    inviterID,
		"invitee_user_id":    inviteeID,
		"inviter_email":      inviterEmail,
		"invitee_email":      inviteeEmail,
		"source_kind":        r.SourceKind,
		"source_amount":      base,
		"base_amount":        base,
		"clawed_back_amount": r.ClawedBackAmount.String(),
		"rate_pct_snapshot":  rate,
		"rate_pct":           rate,
		"rebate_amount":      amount,
		"amount":             amount,
		"currency":           r.Currency,
		"status":             r.Status,
		"risk_flags":         r.RiskFlags,
		"note":               r.Note,
		"created_at":         r.CreatedAt,
	}
	// Each amount and id is emitted under both spellings on purpose — the
	// admin list reads one, older callers read the other.
	//
	// A rebate carries exactly one source: the order it rebated (live) or
	// the usage log it rebated (retired consume rows). source_usage_log_id
	// is conditional now that it is nullable; emitting it unconditionally
	// would panic on every topup row.
	if r.SourceOrderID != nil {
		row["source_order_id"] = r.SourceOrderID.String()
	}
	if r.SourceUsageLogID != nil {
		row["source_usage_log_id"] = r.SourceUsageLogID.String()
	}
	if r.PaidAt != nil {
		row["paid_at"] = r.PaidAt
	}
	if r.PaidBalanceTxID != nil {
		row["paid_balance_tx_id"] = r.PaidBalanceTxID.String()
	}
	return row
}

type inviteActionReq struct {
	Note string `json:"note"`
}

func (h *Handler) payInviteReward(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req inviteActionReq
	_ = c.ShouldBindJSON(&req)
	r, err := h.invites.PayReward(c.Request.Context(), id, req.Note)
	if err != nil {
		inviteErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reward": inviteRewardView(r, "", "")})
}

func (h *Handler) rejectInviteReward(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req inviteActionReq
	_ = c.ShouldBindJSON(&req)
	r, err := h.invites.RejectReward(c.Request.Context(), id, req.Note)
	if err != nil {
		inviteErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"reward": inviteRewardView(r, "", "")})
}

func (h *Handler) payInviteRebate(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req inviteActionReq
	_ = c.ShouldBindJSON(&req)
	r, err := h.invites.PayRebate(c.Request.Context(), id, req.Note)
	if err != nil {
		inviteErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rebate": inviteRebateView(r, "", "")})
}

func (h *Handler) rejectInviteRebate(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req inviteActionReq
	_ = c.ShouldBindJSON(&req)
	r, err := h.invites.RejectRebate(c.Request.Context(), id, req.Note)
	if err != nil {
		inviteErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rebate": inviteRebateView(r, "", "")})
}

func (h *Handler) voidInviteBinding(c *gin.Context) {
	if !h.requireInvites(c) {
		return
	}
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req inviteActionReq
	_ = c.ShouldBindJSON(&req)
	b, err := h.invites.VoidBinding(c.Request.Context(), id, req.Note)
	if err != nil {
		inviteErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"binding": inviteBindingView(biz.InviteAdminBinding{Binding: b})})
}
