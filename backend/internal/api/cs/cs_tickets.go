package cs

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserrole "github.com/jucode/jucode/internal/data/ent/userrole"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) listTickets(c *gin.Context) {
	uid, _ := middleware.UserIDFrom(c)
	limit := parseIntQuery(c, "limit", 50, 200, 1)
	offset := parseIntQuery(c, "offset", 0, 100000, 0)
	var userID *uuid.UUID
	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		userID = &id
	}
	res, err := h.tickets.List(c.Request.Context(), biz.TicketListFilter{
		Scope:    "cs",
		ViewerID: uid,
		UserID:   userID,
		Status:   c.Query("status"),
		Assignee: c.Query("assignee"),
		Category: c.Query("category"),
		Q:        c.Query("q"),
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	emails := csTicketEmails(c, h, res.Tickets)
	out := make([]gin.H, 0, len(res.Tickets))
	for _, t := range res.Tickets {
		out = append(out, csTicketView(t, emails[t.UserID]))
	}
	c.JSON(http.StatusOK, gin.H{
		"tickets": out, "total": res.Total,
		"limit": limit, "offset": offset,
	})
}

func (h *Handler) ticketCounts(c *gin.Context) {
	counts, err := h.tickets.CountByStatus(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	unread, err := h.tickets.CountUnreadStaff(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"counts": counts, "unread": unread})
}

func (h *Handler) ticketAssignees(c *gin.Context) {
	if pr, _ := c.Get(middleware.CtxRole); pr != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin only"})
		return
	}
	rows, err := h.users.User.Query().
		Where(
			entuser.StatusEQ(entuser.StatusActive),
			entuser.HasRolesWith(entuserrole.RoleEQ(entuserrole.RoleCs)),
		).
		Order(ent.Asc(entuser.FieldEmail)).
		Limit(500).
		All(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, u := range rows {
		row := gin.H{
			"id":       u.ID.String(),
			"email":    u.Email,
			"role":     u.Role,
			"nickname": u.Nickname,
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{"assignees": out})
}

func (h *Handler) getTicket(c *gin.Context) {
	uid, _ := middleware.UserIDFrom(c)
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, true)
	if err != nil {
		ticketErr(c, err)
		return
	}
	h.tickets.MarkSeen(c.Request.Context(), t, true)
	msgs, err := h.tickets.Messages(c.Request.Context(), t.ID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, gin.H{
			"id":            m.ID.String(),
			"sender_role":   m.SenderRoleAtSend,
			"sender_id":     m.SenderUserID.String(),
			"body":          m.Body,
			"attachments":   biz.AttachmentViews(t.ID, m.Attachments, true),
			"internal_note": m.InternalNote,
			"created_at":    m.CreatedAt,
		})
	}
	var email string
	if u, err := h.users.User.Get(c.Request.Context(), t.UserID); err == nil {
		email = u.Email
	}
	tk := csTicketView(t, email)
	c.JSON(http.StatusOK, gin.H{
		"ticket":   tk,
		"messages": out,
	})
}

func (h *Handler) getTicketAttachment(c *gin.Context) {
	uid, _ := middleware.UserIDFrom(c)
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, true)
	if err != nil {
		ticketErr(c, err)
		return
	}
	path, typ, err := h.tickets.OpenAttachment(t.ID, c.Param("file"))
	if err != nil {
		ticketErr(c, err)
		return
	}
	c.Header("Content-Type", typ)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, max-age=86400")
	c.File(path)
}

type csReplyReq struct {
	Body         string              `json:"body"`
	InternalNote bool                `json:"internal_note"`
	Attachments  []map[string]string `json:"attachments"`
}

func (h *Handler) replyTicket(c *gin.Context) {
	uid, _ := middleware.UserIDFrom(c)
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, true)
	if err != nil {
		ticketErr(c, err)
		return
	}
	var req csReplyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	role := "cs"
	if pr, _ := c.Get(middleware.CtxRole); pr == "admin" {
		role = "admin"
	}
	msg, err := h.tickets.Reply(c.Request.Context(), biz.ReplyInput{
		TicketID:     t.ID,
		SenderUserID: uid,
		SenderRole:   role,
		Body:         req.Body,
		InternalNote: req.InternalNote,
		Attachments:  req.Attachments,
	})
	if err != nil {
		ticketErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":            msg.ID.String(),
		"sender_role":   msg.SenderRoleAtSend,
		"body":          msg.Body,
		"attachments":   biz.AttachmentViews(t.ID, msg.Attachments, true),
		"internal_note": msg.InternalNote,
		"created_at":    msg.CreatedAt,
	})
}

type csAssignReq struct {
	AssigneeUserID string `json:"assignee_user_id"` // "" = unassign, "me" = self, uuid = explicit
}

func (h *Handler) assignTicket(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	uid, _ := middleware.UserIDFrom(c)
	var req csAssignReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var target uuid.UUID
	switch req.AssigneeUserID {
	case "":
		target = uuid.Nil
	case "me":
		target = uid
	default:
		g, err := uuid.Parse(req.AssigneeUserID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid assignee_user_id"})
			return
		}
		target = g
	}
	if target != uuid.Nil && target != uid {
		if pr, _ := c.Get(middleware.CtxRole); pr != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "admin only"})
			return
		}
	}
	t, err := h.tickets.Assign(c.Request.Context(), id, target, uid)
	if err != nil {
		ticketErr(c, err)
		return
	}
	row := gin.H{"id": t.ID.String(), "status": t.Status}
	if t.AssigneeUserID != nil {
		row["assignee_user_id"] = t.AssigneeUserID.String()
	}
	c.JSON(http.StatusOK, row)
}

type csStatusReq struct {
	Status string `json:"status" binding:"required"`
}

func (h *Handler) setTicketStatus(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	uid, _ := middleware.UserIDFrom(c)
	var req csStatusReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	t, err := h.tickets.SetStatus(c.Request.Context(), id, req.Status, uid, true)
	if err != nil {
		ticketErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": t.ID.String(), "status": t.Status})
}

type createTicketRefundReq struct {
	Amount *string `json:"amount"`
	Note   string  `json:"note"`
}

func (h *Handler) createTicketRefund(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	uid, _ := middleware.UserIDFrom(c)
	t, err := h.tickets.Get(c.Request.Context(), id, uid, true)
	if err != nil {
		ticketErr(c, err)
		return
	}
	if t.Category != "refund" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a refund ticket"})
		return
	}
	if t.OrderID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refund ticket has no order"})
		return
	}
	var req createTicketRefundReq
	_ = c.ShouldBindJSON(&req)
	reason := t.Title
	if msgs, err := h.tickets.Messages(c.Request.Context(), id, false); err == nil && len(msgs) > 0 {
		if body := strings.TrimSpace(msgs[0].Body); body != "" {
			reason = body
		}
	}
	var amount *decimal.Decimal
	if req.Amount != nil && strings.TrimSpace(*req.Amount) != "" {
		v, err := decimal.NewFromString(strings.TrimSpace(*req.Amount))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		amount = &v
	}
	r, err := h.finance.CreateRefund(c.Request.Context(), biz.CreateRefundInput{
		OrderID: *t.OrderID,
		Amount:  amount,
		Reason:  reason,
		Note:    req.Note,
	})
	if err != nil {
		financeErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"refund": refundView(r)})
}

func csTicketView(t *ent.Ticket, email string) gin.H {
	no := biz.TicketNoValue(t.TicketNo)
	kind := biz.UnreadKind(t, true)
	row := gin.H{
		"id":              t.ID.String(),
		"ticket_no":       no,
		"ticket_no_label": biz.FormatTicketNo(no),
		"user_id":         t.UserID.String(),
		"user_email":      email,
		"title":           t.Title,
		"category":        t.Category,
		"status":          t.Status,
		"priority":        t.Priority,
		"last_reply_at":   t.LastReplyAt,
		"created_at":      t.CreatedAt,
		"unread":          kind != "",
		"unread_kind":     kind,
	}
	if t.OrderID != nil {
		row["order_id"] = t.OrderID.String()
	}
	if t.AssigneeUserID != nil {
		row["assignee_user_id"] = t.AssigneeUserID.String()
	}
	return row
}

func csTicketEmails(c *gin.Context, h *Handler, tickets []*ent.Ticket) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	if len(tickets) == 0 {
		return out
	}
	ids := make([]uuid.UUID, 0, len(tickets))
	seen := map[uuid.UUID]struct{}{}
	for _, t := range tickets {
		if _, ok := seen[t.UserID]; ok {
			continue
		}
		seen[t.UserID] = struct{}{}
		ids = append(ids, t.UserID)
	}
	users, err := h.users.User.Query().Where(entuser.IDIn(ids...)).All(c.Request.Context())
	if err != nil {
		return out
	}
	for _, u := range users {
		out[u.ID] = u.Email
	}
	return out
}

func ticketErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrTicketNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "ticket not found"})
	case errors.Is(err, biz.ErrTicketForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed"})
	case errors.Is(err, biz.ErrTicketInvalidStatus):
		c.JSON(http.StatusConflict, gin.H{"error": "invalid status transition"})
	case errors.Is(err, biz.ErrTicketInvalidAssignee):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid assignee"})
	case errors.Is(err, biz.ErrTicketAttachmentInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, biz.ErrTicketAttachmentGone):
		c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func parseIntQuery(c *gin.Context, name string, def, max, min int) int {
	v := c.Query(name)
	if v == "" {
		return def
	}
	var n int
	for _, r := range v {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
		if n > max {
			return max
		}
	}
	if n < min {
		return min
	}
	return n
}
