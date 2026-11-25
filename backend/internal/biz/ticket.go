package biz

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/predicate"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entticket "github.com/jucode/jucode/internal/data/ent/ticket"
	entticketmsg "github.com/jucode/jucode/internal/data/ent/ticketmessage"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// Errors returned by TicketSvc.
var (
	ErrTicketNotFound        = errors.New("ticket not found")
	ErrTicketForbidden       = errors.New("not allowed to access this ticket")
	ErrTicketInvalidStatus   = errors.New("invalid status transition")
	ErrTicketInternalScoped  = errors.New("only cs/admin may post internal notes")
	ErrTicketInvalidAssignee = errors.New("invalid ticket assignee")
	ErrTicketRefundNoOrder   = errors.New("refund tickets require an order")
	ErrTicketRefundNoReason  = errors.New("refund reason is required")
)

// ErrRefundTicketOpen is returned when the user already has an open
// refund ticket for the same order. The existing ticket id is included
// so the UI can jump to it instead of creating a duplicate.
type ErrRefundTicketOpen struct {
	TicketID uuid.UUID
}

func (e *ErrRefundTicketOpen) Error() string {
	return "an open refund ticket already exists for this order"
}

// TicketSvc owns the ticket + ticket_messages business rules. It does
// not enforce role gating itself — middleware handles route-level
// permission. The svc only enforces ownership (a user may only see their
// own tickets) and the status state machine.
type TicketSvc struct {
	d         *data.Data
	roles     *RoleSvc
	settings  *SettingsSvc
	email     *EmailSvc
	logger    *slog.Logger
	attachDir string
}

func NewTicketSvc(d *data.Data, roles *RoleSvc, settings *SettingsSvc, email *EmailSvc, logger *slog.Logger) *TicketSvc {
	if logger == nil {
		logger = slog.Default()
	}
	return &TicketSvc{d: d, roles: roles, settings: settings, email: email, logger: logger.With("svc", "ticket")}
}

// --- listing ---

type TicketListFilter struct {
	// Scope picks the lens:
	//   - "user":   only tickets where user_id = ViewerID (user self-view).
	//   - "cs":     all tickets; cs / admin view.
	// Unknown values default to "user".
	Scope    string
	ViewerID uuid.UUID
	// UserID is an extra CS-only filter: one customer's ticket history.
	UserID   *uuid.UUID
	Status   string
	Assignee string // "me", "unassigned", "any" (cs scope only)
	Category string
	Q        string
	Limit    int
	Offset   int
}

type TicketListResult struct {
	Tickets []*ent.Ticket
	Total   int
}

func (s *TicketSvc) List(ctx context.Context, f TicketListFilter) (*TicketListResult, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	q := s.d.Ent.Ticket.Query()
	switch f.Scope {
	case "cs":
		if f.UserID != nil {
			q = q.Where(entticket.UserIDEQ(*f.UserID))
		}
	default:
		q = q.Where(entticket.UserIDEQ(f.ViewerID))
	}
	if f.Status != "" {
		switch f.Status {
		case "open", "in_progress", "waiting_user", "resolved", "closed":
			q = q.Where(entticket.StatusEQ(entticket.Status(f.Status)))
		}
	}
	if cat := parseTicketCategory(f.Category); cat != "" {
		q = q.Where(entticket.CategoryEQ(cat))
	}
	if f.Scope == "cs" {
		switch f.Assignee {
		case "me":
			q = q.Where(entticket.AssigneeUserIDEQ(f.ViewerID))
		case "unassigned":
			q = q.Where(entticket.AssigneeUserIDIsNil())
		}
	}
	if pred := s.searchPredicate(ctx, f); pred != nil {
		q = q.Where(pred)
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.Order(ent.Desc(entticket.FieldLastReplyAt)).
		Limit(f.Limit).Offset(f.Offset).All(ctx)
	if err != nil {
		return nil, err
	}
	return &TicketListResult{Tickets: rows, Total: total}, nil
}

func (s *TicketSvc) searchPredicate(ctx context.Context, f TicketListFilter) predicate.Ticket {
	term := strings.TrimSpace(f.Q)
	if term == "" {
		return nil
	}
	preds := []predicate.Ticket{
		entticket.TitleContainsFold(term),
		entticket.HasMessagesWith(entticketmsg.BodyContainsFold(term)),
	}
	if n, ok := ParseTicketNo(term); ok {
		preds = append(preds, entticket.TicketNoEQ(n))
	}
	if id, err := uuid.Parse(term); err == nil {
		preds = append(preds, entticket.IDEQ(id), entticket.OrderIDEQ(id))
	}
	if f.Scope == "cs" {
		userIDs, err := s.d.Ent.User.Query().
			Where(entuser.Or(
				entuser.EmailContainsFold(term),
				entuser.UsernameContainsFold(term),
				entuser.NicknameContainsFold(term),
			)).
			Limit(50).
			IDs(ctx)
		if err == nil && len(userIDs) > 0 {
			preds = append(preds, entticket.UserIDIn(userIDs...))
		}
		orderIDs, err := s.d.Ent.SiteOrder.Query().
			Where(entsiteorder.OrderNoContainsFold(term)).
			Limit(50).
			IDs(ctx)
		if err == nil && len(orderIDs) > 0 {
			preds = append(preds, entticket.OrderIDIn(orderIDs...))
		}
	}
	return entticket.Or(preds...)
}

// UnreadKind is "" when the viewer is caught up, "unread" when they have
// never opened the ticket, and "reply" when the other side wrote since
// they last opened it.
func UnreadKind(t *ent.Ticket, staff bool) string {
	if t == nil {
		return ""
	}
	var seen, last *time.Time
	if staff {
		seen, last = t.StaffSeenAt, t.LastUserReplyAt
	} else {
		seen, last = t.UserSeenAt, t.LastStaffReplyAt
	}
	if last == nil {
		return ""
	}
	if seen == nil {
		return "unread"
	}
	if last.After(*seen) {
		return "reply"
	}
	return ""
}

func (s *TicketSvc) MarkSeen(ctx context.Context, t *ent.Ticket, staff bool) {
	if t == nil {
		return
	}
	now := time.Now()
	upd := s.d.Ent.Ticket.UpdateOneID(t.ID)
	if staff {
		upd = upd.SetStaffSeenAt(now)
		t.StaffSeenAt = &now
	} else {
		upd = upd.SetUserSeenAt(now)
		t.UserSeenAt = &now
	}
	if _, err := upd.Save(ctx); err != nil {
		s.logger.Warn("ticket.mark_seen_failed", "ticket_id", t.ID.String(), "err", err.Error())
	}
}

func (s *TicketSvc) GetByRef(ctx context.Context, ref string, viewerID uuid.UUID, allowCS bool) (*ent.Ticket, error) {
	ref = strings.TrimSpace(ref)
	if id, err := uuid.Parse(ref); err == nil {
		return s.Get(ctx, id, viewerID, allowCS)
	}
	n, ok := ParseTicketNo(ref)
	if !ok {
		return nil, ErrTicketNotFound
	}
	t, err := s.d.Ent.Ticket.Query().Where(entticket.TicketNoEQ(n)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	if !allowCS && t.UserID != viewerID {
		return nil, ErrTicketForbidden
	}
	return t, nil
}

func (s *TicketSvc) CountUnreadStaff(ctx context.Context) (int, error) {
	var n int
	err := s.d.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tickets
		WHERE last_user_reply_at IS NOT NULL
		  AND (staff_seen_at IS NULL OR last_user_reply_at > staff_seen_at)
	`).Scan(&n)
	return n, err
}

// --- creation ---

type CreateTicketInput struct {
	UserID      uuid.UUID
	OrderID     *uuid.UUID
	Title       string
	Body        string
	Category    string // optional; defaults to "general"
	Priority    string // optional; defaults to "normal"
	Attachments []map[string]string
}

func parseTicketCategory(raw string) entticket.Category {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "refund":
		return entticket.CategoryRefund
	case "billing":
		return entticket.CategoryBilling
	case "technical":
		return entticket.CategoryTechnical
	case "account":
		return entticket.CategoryAccount
	case "general":
		return entticket.CategoryGeneral
	default:
		return ""
	}
}

func (s *TicketSvc) Create(ctx context.Context, in CreateTicketInput) (*ent.Ticket, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	cat := parseTicketCategory(in.Category)
	if cat == "" {
		cat = entticket.CategoryGeneral
	}
	if cat == entticket.CategoryRefund {
		if in.OrderID == nil {
			return nil, ErrTicketRefundNoOrder
		}
		if in.Body == "" {
			return nil, ErrTicketRefundNoReason
		}
		if in.Title == "" {
			in.Title = "申请退款"
		}
	}
	if in.Title == "" {
		return nil, errors.New("title is required")
	}
	if in.Body == "" && len(in.Attachments) == 0 {
		return nil, errors.New("body or attachments are required")
	}
	body := in.Body
	if body == "" {
		body = " "
	}
	if in.OrderID != nil {
		o, err := s.d.Ent.SiteOrder.Get(ctx, *in.OrderID)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil, ErrOrderNotFound
			}
			return nil, err
		}
		if o.UserID != in.UserID {
			return nil, ErrOrderNotFound
		}
		if cat == entticket.CategoryRefund {
			if existing, err := s.openRefundTicket(ctx, in.UserID, *in.OrderID); err != nil {
				return nil, err
			} else if existing != nil {
				return nil, &ErrRefundTicketOpen{TicketID: existing.ID}
			}
		}
	}
	prio := entticket.PriorityNormal
	switch in.Priority {
	case "low":
		prio = entticket.PriorityLow
	case "normal":
		prio = entticket.PriorityNormal
	case "high":
		prio = entticket.PriorityHigh
	case "urgent":
		prio = entticket.PriorityUrgent
	}
	no, err := s.nextTicketNo(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tq := tx.Ticket.Create().
		SetUserID(in.UserID).
		SetTitle(in.Title).
		SetCategory(cat).
		SetPriority(prio).
		SetTicketNo(no).
		SetLastReplyAt(now).
		SetUserSeenAt(now).
		SetLastUserReplyAt(now)
	if in.OrderID != nil {
		tq = tq.SetOrderID(*in.OrderID)
	}
	t, err := tq.Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	atts, err := s.persistAttachments(t.ID, in.Attachments)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if _, err := tx.TicketMessage.Create().
		SetTicketID(t.ID).
		SetSenderUserID(in.UserID).
		SetSenderRoleAtSend(entticketmsg.SenderRoleAtSendUser).
		SetBody(body).
		SetAttachments(atts).
		Save(ctx); err != nil {
		s.discardAttachments(t.ID, atts)
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		s.discardAttachments(t.ID, atts)
		return nil, err
	}
	return t, nil
}

// --- detail ---

// Get returns a ticket if the viewer is permitted to see it.
//   - allowCS=true: viewer is cs/admin; any ticket is allowed.
//   - allowCS=false: only the ticket's owner may view.
func (s *TicketSvc) Get(ctx context.Context, id uuid.UUID, viewerID uuid.UUID, allowCS bool) (*ent.Ticket, error) {
	t, err := s.d.Ent.Ticket.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	if !allowCS && t.UserID != viewerID {
		return nil, ErrTicketForbidden
	}
	return t, nil
}

// Messages returns the conversation, oldest first. internal=false hides
// internal_note=true rows (used by the user-facing endpoint).
func (s *TicketSvc) Messages(ctx context.Context, ticketID uuid.UUID, includeInternal bool) ([]*ent.TicketMessage, error) {
	q := s.d.Ent.TicketMessage.Query().
		Where(entticketmsg.TicketIDEQ(ticketID))
	if !includeInternal {
		q = q.Where(entticketmsg.InternalNoteEQ(false))
	}
	rows, err := q.Order(ent.Asc(entticketmsg.FieldCreatedAt)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		if !attachmentsNeedPersist(m.Attachments) {
			continue
		}
		next, perr := s.persistAttachments(ticketID, m.Attachments)
		if perr != nil {
			s.logger.Warn("ticket.attachment_migrate_failed", "message_id", m.ID.String(), "err", perr.Error())
			continue
		}
		if err := s.d.Ent.TicketMessage.UpdateOneID(m.ID).SetAttachments(next).Exec(ctx); err != nil {
			s.logger.Warn("ticket.attachment_migrate_save_failed", "message_id", m.ID.String(), "err", err.Error())
			continue
		}
		m.Attachments = next
	}
	return rows, nil
}

func attachmentsNeedPersist(atts []map[string]string) bool {
	for _, a := range atts {
		if a == nil {
			continue
		}
		if strings.TrimSpace(a["data_url"]) != "" && strings.TrimSpace(a["id"]) == "" {
			return true
		}
	}
	return false
}

// --- replies ---

type ReplyInput struct {
	TicketID     uuid.UUID
	SenderUserID uuid.UUID
	SenderRole   string // "user" | "cs" | "admin"
	Body         string
	InternalNote bool
	Attachments  []map[string]string
}

// Reply appends a message to the ticket and advances its status:
//   - user reply: in_progress (if previously waiting_user / open / resolved).
//   - cs/admin reply (non-internal): waiting_user (signals the user must respond).
//   - internal note: status unchanged.
//
// last_reply_at always bumps so the cs queue stays sorted.
func (s *TicketSvc) Reply(ctx context.Context, in ReplyInput) (*ent.TicketMessage, error) {
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" && len(in.Attachments) == 0 {
		return nil, errors.New("body or attachments are required")
	}
	body := in.Body
	if body == "" {
		body = " "
	}
	if in.InternalNote && in.SenderRole == "user" {
		return nil, ErrTicketInternalScoped
	}
	role := entticketmsg.SenderRoleAtSendUser
	switch in.SenderRole {
	case "cs":
		role = entticketmsg.SenderRoleAtSendCs
	case "admin":
		role = entticketmsg.SenderRoleAtSendAdmin
	}

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	t, err := tx.Ticket.Get(ctx, in.TicketID)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	if t.Status == entticket.StatusClosed {
		_ = tx.Rollback()
		return nil, ErrTicketInvalidStatus
	}

	atts, err := s.persistAttachments(in.TicketID, in.Attachments)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	msg, err := tx.TicketMessage.Create().
		SetTicketID(in.TicketID).
		SetSenderUserID(in.SenderUserID).
		SetSenderRoleAtSend(role).
		SetBody(body).
		SetAttachments(atts).
		SetInternalNote(in.InternalNote).
		Save(ctx)
	if err != nil {
		s.discardAttachments(in.TicketID, atts)
		_ = tx.Rollback()
		return nil, err
	}

	now := time.Now()
	upd := tx.Ticket.UpdateOneID(in.TicketID).SetLastReplyAt(now)
	if !in.InternalNote {
		switch in.SenderRole {
		case "user":
			upd = upd.SetStatus(entticket.StatusInProgress).SetLastUserReplyAt(now)
		case "cs", "admin":
			upd = upd.SetStatus(entticket.StatusWaitingUser).SetLastStaffReplyAt(now)
		}
	}
	if _, err := upd.Save(ctx); err != nil {
		s.discardAttachments(in.TicketID, atts)
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		s.discardAttachments(in.TicketID, atts)
		return nil, err
	}
	if !in.InternalNote {
		s.notifyTicketReply(ctx, t, in)
	}
	return msg, nil
}

// --- status transitions / assignment (cs) ---

// SetStatus moves the ticket through the lifecycle. Allowed transitions
// match the diagram in ticket.go. Owners (the requesting user) may only
// "close" their own ticket; cs/admin may set any status.
func (s *TicketSvc) SetStatus(ctx context.Context, ticketID uuid.UUID, target string, viewerID uuid.UUID, allowCS bool) (*ent.Ticket, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	switch target {
	case "open", "in_progress", "waiting_user", "resolved", "closed":
	default:
		return nil, ErrTicketInvalidStatus
	}
	t, err := s.d.Ent.Ticket.Get(ctx, ticketID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	if !allowCS {
		if t.UserID != viewerID {
			return nil, ErrTicketForbidden
		}
		// User self-service can close, or reopen a closed/resolved ticket.
		if target != "closed" && target != "open" {
			return nil, ErrTicketInvalidStatus
		}
		if target == "open" && t.Status != entticket.StatusClosed && t.Status != entticket.StatusResolved {
			return nil, ErrTicketInvalidStatus
		}
	}
	upd := s.d.Ent.Ticket.UpdateOneID(ticketID).SetStatus(entticket.Status(target))
	if target == "open" {
		now := time.Now()
		upd = upd.SetLastReplyAt(now)
		if !allowCS {
			upd = upd.SetLastUserReplyAt(now)
		}
	}
	updated, err := upd.Save(ctx)
	if err != nil {
		return nil, err
	}
	if allowCS && t.UserID != viewerID && t.Status != entticket.Status(target) {
		s.notifyTicketStatus(ctx, updated, target)
	}
	return updated, nil
}

// Assign sets the ticket's assignee. Passing uuid.Nil clears it back to
// the unassigned queue. The caller must already be authorised as cs.
func (s *TicketSvc) Assign(ctx context.Context, ticketID uuid.UUID, assignee uuid.UUID, actor uuid.UUID) (*ent.Ticket, error) {
	var target *ent.User
	if assignee != uuid.Nil {
		u, err := s.d.Ent.User.Get(ctx, assignee)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil, ErrTicketInvalidAssignee
			}
			return nil, err
		}
		if u.Status != entuser.StatusActive {
			return nil, ErrTicketInvalidAssignee
		}
		if u.Role != entuser.RoleAdmin {
			if s.roles == nil {
				return nil, ErrTicketInvalidAssignee
			}
			ok, err := s.roles.HasAny(ctx, assignee, RoleCS)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, ErrTicketInvalidAssignee
			}
		}
		target = u
	}
	current, err := s.d.Ent.Ticket.Get(ctx, ticketID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	if assignee == uuid.Nil {
		if current.AssigneeUserID == nil {
			return current, nil
		}
	} else if current.AssigneeUserID != nil && *current.AssigneeUserID == assignee {
		return current, nil
	}
	upd := s.d.Ent.Ticket.UpdateOneID(ticketID)
	if assignee == uuid.Nil {
		upd = upd.ClearAssigneeUserID()
	} else {
		upd = upd.SetAssigneeUserID(assignee)
	}
	t, err := upd.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrTicketNotFound
		}
		return nil, err
	}
	if target != nil && target.ID != actor {
		s.notifyTicketAssigned(ctx, t, target)
	}
	return t, nil
}

func (s *TicketSvc) notifyTicketAssigned(ctx context.Context, t *ent.Ticket, assignee *ent.User) {
	if s.email == nil || s.settings == nil {
		return
	}
	siteName, link, err := s.ticketNotificationSite(ctx, true)
	if err != nil {
		s.logger.Warn("ticket.assign_notify_config_failed", "ticket_id", t.ID.String(), "assignee_user_id", assignee.ID.String(), "err", err.Error())
		return
	}
	subject, body := TicketAssignedEmail(siteName, t.Title, link)
	if err := s.email.Send(ctx, assignee.Email, subject, body); err != nil {
		if !errors.Is(err, ErrEmailDisabled) {
			s.logger.Warn("ticket.assign_notify_send_failed", "ticket_id", t.ID.String(), "assignee_user_id", assignee.ID.String(), "err", err.Error())
		}
	}
}

func (s *TicketSvc) notifyTicketReply(ctx context.Context, t *ent.Ticket, in ReplyInput) {
	if s.email == nil || s.settings == nil {
		return
	}
	staffRecipient := false
	var recipient *ent.User
	switch in.SenderRole {
	case "user":
		if t.AssigneeUserID == nil || *t.AssigneeUserID == in.SenderUserID {
			return
		}
		u, err := s.d.Ent.User.Get(ctx, *t.AssigneeUserID)
		if err != nil {
			s.logger.Warn("ticket.reply_notify_recipient_failed", "ticket_id", t.ID.String(), "assignee_user_id", t.AssigneeUserID.String(), "err", err.Error())
			return
		}
		recipient = u
		staffRecipient = true
	case "cs", "admin":
		if t.UserID == in.SenderUserID {
			return
		}
		u, err := s.d.Ent.User.Get(ctx, t.UserID)
		if err != nil {
			s.logger.Warn("ticket.reply_notify_recipient_failed", "ticket_id", t.ID.String(), "user_id", t.UserID.String(), "err", err.Error())
			return
		}
		recipient = u
	default:
		return
	}
	if strings.TrimSpace(recipient.Email) == "" {
		return
	}
	siteName, link, err := s.ticketNotificationSite(ctx, staffRecipient)
	if err != nil {
		s.logger.Warn("ticket.reply_notify_config_failed", "ticket_id", t.ID.String(), "recipient_user_id", recipient.ID.String(), "err", err.Error())
		return
	}
	subject, body := TicketReplyEmail(siteName, t.Title, link, staffRecipient)
	if err := s.email.Send(ctx, recipient.Email, subject, body); err != nil {
		if !errors.Is(err, ErrEmailDisabled) {
			s.logger.Warn("ticket.reply_notify_send_failed", "ticket_id", t.ID.String(), "recipient_user_id", recipient.ID.String(), "err", err.Error())
		}
	}
}

func (s *TicketSvc) notifyTicketStatus(ctx context.Context, t *ent.Ticket, status string) {
	if s.email == nil || s.settings == nil {
		return
	}
	owner, err := s.d.Ent.User.Get(ctx, t.UserID)
	if err != nil {
		s.logger.Warn("ticket.status_notify_recipient_failed", "ticket_id", t.ID.String(), "user_id", t.UserID.String(), "err", err.Error())
		return
	}
	if strings.TrimSpace(owner.Email) == "" {
		return
	}
	siteName, link, err := s.ticketNotificationSite(ctx, false)
	if err != nil {
		s.logger.Warn("ticket.status_notify_config_failed", "ticket_id", t.ID.String(), "user_id", owner.ID.String(), "err", err.Error())
		return
	}
	subject, body := TicketStatusEmail(siteName, t.Title, status, link)
	if err := s.email.Send(ctx, owner.Email, subject, body); err != nil {
		if !errors.Is(err, ErrEmailDisabled) {
			s.logger.Warn("ticket.status_notify_send_failed", "ticket_id", t.ID.String(), "user_id", owner.ID.String(), "err", err.Error())
		}
	}
}

func (s *TicketSvc) ticketNotificationSite(ctx context.Context, staff bool) (string, string, error) {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return "", "", err
	}
	link := ""
	if base := strings.TrimRight(strings.TrimSpace(cfg.SiteURL), "/"); base != "" {
		if staff {
			link = base + "/cs/tickets"
		} else {
			link = base + "/me/tickets"
		}
	}
	return cfg.SiteName, link, nil
}

func (s *TicketSvc) openRefundTicket(ctx context.Context, userID, orderID uuid.UUID) (*ent.Ticket, error) {
	t, err := s.d.Ent.Ticket.Query().
		Where(
			entticket.UserIDEQ(userID),
			entticket.OrderIDEQ(orderID),
			entticket.CategoryEQ(entticket.CategoryRefund),
			entticket.StatusIn(
				entticket.StatusOpen,
				entticket.StatusInProgress,
				entticket.StatusWaitingUser,
			),
		).
		Order(ent.Desc(entticket.FieldCreatedAt)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// LatestRefundTickets maps order id → the newest refund ticket for that
// order. Used by the order list so "申请退款" can deep-link to an already
// open request instead of opening a second thread.
func (s *TicketSvc) LatestRefundTickets(ctx context.Context, orderIDs []uuid.UUID) (map[uuid.UUID]*ent.Ticket, error) {
	out := make(map[uuid.UUID]*ent.Ticket, len(orderIDs))
	if len(orderIDs) == 0 {
		return out, nil
	}
	rows, err := s.d.Ent.Ticket.Query().
		Where(
			entticket.OrderIDIn(orderIDs...),
			entticket.CategoryEQ(entticket.CategoryRefund),
		).
		Order(ent.Desc(entticket.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		if t.OrderID == nil {
			continue
		}
		if _, exists := out[*t.OrderID]; exists {
			continue
		}
		out[*t.OrderID] = t
	}
	return out, nil
}

// --- count helpers (for cs queue badges) ---

func (s *TicketSvc) CountByStatus(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, st := range []string{"open", "in_progress", "waiting_user", "resolved", "closed"} {
		n, err := s.d.Ent.Ticket.Query().
			Where(entticket.StatusEQ(entticket.Status(st))).
			Count(ctx)
		if err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, nil
}

// EnsureUnreadStamps backfills last-reply / seen columns on tickets that
// predate those fields. Only rows with both last-reply stamps still NULL
// are touched, so live tickets created after deploy keep their unread
// state. Seen is set to the last opposite-side reply so the queue does
// not light up every historical ticket on upgrade.
func (s *TicketSvc) EnsureUnreadStamps(ctx context.Context) error {
	_, err := s.d.DB().ExecContext(ctx, `
		WITH last AS (
			SELECT ticket_id,
				MAX(created_at) FILTER (
					WHERE NOT internal_note AND sender_role_at_send = 'user'
				) AS user_reply,
				MAX(created_at) FILTER (
					WHERE NOT internal_note AND sender_role_at_send IN ('cs', 'admin')
				) AS staff_reply
			FROM ticket_messages
			GROUP BY ticket_id
		)
		UPDATE tickets t
		SET
			last_user_reply_at = COALESCE(t.last_user_reply_at, last.user_reply),
			last_staff_reply_at = COALESCE(t.last_staff_reply_at, last.staff_reply),
			user_seen_at = COALESCE(t.user_seen_at, last.staff_reply, last.user_reply, t.created_at),
			staff_seen_at = COALESCE(t.staff_seen_at, last.user_reply, last.staff_reply, t.created_at)
		FROM last
		WHERE last.ticket_id = t.id
		  AND t.last_user_reply_at IS NULL
		  AND t.last_staff_reply_at IS NULL
	`)
	return err
}
