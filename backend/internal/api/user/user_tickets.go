// user_tickets.go owns /v1/me/tickets — the self-service portion of the
// support ticket system. The cs-facing counterpart lives in
// internal/api/cs/cs_tickets.go and shares the same TicketSvc.
package user

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entticket "github.com/jucode/jucode/internal/data/ent/ticket"
	"github.com/jucode/jucode/internal/middleware"
)

type createTicketReq struct {
	Title       string              `json:"title"`
	Body        string              `json:"body"`
	Category    string              `json:"category"`
	Priority    string              `json:"priority"`
	OrderID     string              `json:"order_id"`
	Attachments []map[string]string `json:"attachments"`
}

type replyTicketReq struct {
	Body        string              `json:"body"`
	Attachments []map[string]string `json:"attachments"`
}

func (h *Handler) listMyTickets(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	limit := parseIntQuery(c, "limit", 50, 200, 1)
	offset := parseIntQuery(c, "offset", 0, 100000, 0)
	res, err := h.tickets.List(c.Request.Context(), biz.TicketListFilter{
		Scope:    "user",
		ViewerID: uid,
		Status:   c.Query("status"),
		Category: c.Query("category"),
		Q:        c.Query("q"),
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(res.Tickets))
	for _, t := range res.Tickets {
		out = append(out, myTicketView(t))
	}
	c.JSON(http.StatusOK, gin.H{
		"tickets": out, "total": res.Total,
		"limit": limit, "offset": offset,
	})
}

// MountFeedback registers ticket creation for an OAuth device token, so
// JuCode Desktop can send a bug report or suggestion (with screenshots and
// a log file) as the signed-in user. `guards` run first (rate and size limits).
func (h *Handler) MountFeedback(rg *gin.RouterGroup, guards ...gin.HandlerFunc) {
	rg.POST("/tickets", append(guards, h.createMyTicket)...)
}

func (h *Handler) createMyTicket(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req createTicketReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var orderID *uuid.UUID
	if req.OrderID != "" {
		id, err := uuid.Parse(req.OrderID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order_id"})
			return
		}
		orderID = &id
	}
	t, err := h.tickets.Create(c.Request.Context(), biz.CreateTicketInput{
		UserID:      uid,
		OrderID:     orderID,
		Title:       req.Title,
		Body:        req.Body,
		Category:    req.Category,
		Priority:    req.Priority,
		Attachments: req.Attachments,
	})
	if err != nil {
		writeTicketCreateError(c, err)
		return
	}
	c.JSON(http.StatusCreated, myTicketView(h.autoApproveRefundTicket(c, t, req.Body)))
}

func (h *Handler) getMyTicket(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, false)
	if err != nil {
		ticketErr(c, err)
		return
	}
	h.tickets.MarkSeen(c.Request.Context(), t, false)
	msgs, err := h.tickets.Messages(c.Request.Context(), t.ID, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, gin.H{
			"id":          m.ID.String(),
			"sender_role": m.SenderRoleAtSend,
			"sender_id":   m.SenderUserID.String(),
			"body":        m.Body,
			"attachments": biz.AttachmentViews(t.ID, m.Attachments, false),
			"created_at":  m.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"ticket":   myTicketView(t),
		"messages": out,
	})
}

func (h *Handler) replyMyTicket(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, false)
	if err != nil {
		ticketErr(c, err)
		return
	}
	var req replyTicketReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	msg, err := h.tickets.Reply(c.Request.Context(), biz.ReplyInput{
		TicketID:     t.ID,
		SenderUserID: uid,
		SenderRole:   "user",
		Body:         req.Body,
		Attachments:  req.Attachments,
	})
	if err != nil {
		ticketErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":          msg.ID.String(),
		"sender_role": msg.SenderRoleAtSend,
		"body":        msg.Body,
		"attachments": biz.AttachmentViews(t.ID, msg.Attachments, false),
		"created_at":  msg.CreatedAt,
	})
}

func (h *Handler) closeMyTicket(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, false)
	if err != nil {
		ticketErr(c, err)
		return
	}
	t, err = h.tickets.SetStatus(c.Request.Context(), t.ID, "closed", uid, false)
	if err != nil {
		ticketErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id": t.ID.String(), "status": t.Status,
	})
}

func (h *Handler) reopenMyTicket(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, false)
	if err != nil {
		ticketErr(c, err)
		return
	}
	t, err = h.tickets.SetStatus(c.Request.Context(), t.ID, "open", uid, false)
	if err != nil {
		ticketErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id": t.ID.String(), "status": t.Status,
	})
}

func (h *Handler) getMyTicketAttachment(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	t, err := h.tickets.GetByRef(c.Request.Context(), c.Param("id"), uid, false)
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

func myTicketView(t *ent.Ticket) gin.H {
	no := biz.TicketNoValue(t.TicketNo)
	kind := biz.UnreadKind(t, false)
	return gin.H{
		"id":              t.ID.String(),
		"ticket_no":       no,
		"ticket_no_label": biz.FormatTicketNo(no),
		"title":           t.Title,
		"category":        t.Category,
		"status":          t.Status,
		"priority":        t.Priority,
		"last_reply_at":   t.LastReplyAt,
		"created_at":      t.CreatedAt,
		"order_id":        uuidPtrString(t.OrderID),
		"unread":          kind != "",
		"unread_kind":     kind,
	}
}

func writeTicketCreateError(c *gin.Context, err error) {
	var open *biz.ErrRefundTicketOpen
	if errors.As(err, &open) {
		c.JSON(http.StatusConflict, gin.H{
			"error":     err.Error(),
			"ticket_id": open.TicketID.String(),
		})
		return
	}
	switch {
	case errors.Is(err, biz.ErrOrderNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
	case errors.Is(err, biz.ErrTicketRefundNoOrder), errors.Is(err, biz.ErrTicketRefundNoReason):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

func ticketErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrTicketNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "ticket not found"})
	case errors.Is(err, biz.ErrTicketForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed"})
	case errors.Is(err, biz.ErrTicketInvalidStatus):
		c.JSON(http.StatusConflict, gin.H{"error": "invalid status transition"})
	case errors.Is(err, biz.ErrTicketAttachmentInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, biz.ErrTicketAttachmentGone):
		c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func uuidPtrString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// parseIntQuery is shared by the user-side handlers that need ?limit/?offset.
// Clamped to [min, max]; bad values fall back to def.
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

// autoApproveRefundTicket lets refund auto-approval handle a refund ticket
// the user just opened and returns the ticket as it now stands. Like
// self-service refunds, moving money needs a signed-in session: tickets
// opened with a personal access token or a device token stay with staff.
func (h *Handler) autoApproveRefundTicket(c *gin.Context, t *ent.Ticket, reason string) *ent.Ticket {
	if t.Category != entticket.CategoryRefund || t.OrderID == nil {
		return t
	}
	if _, viaToken := c.Get(middleware.CtxAccessTokenID); viaToken {
		return t
	}
	if _, viaDevice := c.Get(middleware.CtxAuthz); viaDevice {
		return t
	}
	h.finance.AutoApproveTicketRefund(c.Request.Context(), t, reason)
	if fresh, err := h.users.Ticket.Get(c.Request.Context(), t.ID); err == nil {
		return fresh
	}
	return t
}
