package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Ticket is a user-initiated support thread handled by customer-service
// (cs role) or admin. The lifecycle is intentionally simple:
//
//	open ─► in_progress ─┬─► waiting_user ─► in_progress
//	                     ├─► resolved
//	                     └─► closed
//
// Messages live in ticket_messages with a sender_role_at_send snapshot
// so historical conversations stay coherent even if a user's role
// changes later (e.g. cs gets revoked → old replies still labelled cs).
type Ticket struct{ ent.Schema }

func (Ticket) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (Ticket) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		// assignee is the cs/admin currently handling the ticket. Nil =
		// unassigned (sitting in the cs queue).
		field.UUID("assignee_user_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable(),
		// ticket_no is the human-facing id (rendered "T-1842"). Optional
		// so pre-existing rows can stay NULL until backfill; UNIQUE allows
		// multiple NULLs in Postgres.
		field.Int("ticket_no").Optional().Nillable().Unique(),
		field.String("title").NotEmpty(),
		// category is the support queue the ticket belongs to. refund
		// tickets must bind an order and carry a reason in the first
		// message — users cannot create a SiteRefund directly.
		field.Enum("category").
			Values("general", "refund", "billing", "technical", "account").
			Default("general"),
		field.Enum("status").
			Values("open", "in_progress", "waiting_user", "resolved", "closed").
			Default("open"),
		field.Enum("priority").
			Values("low", "normal", "high", "urgent").
			Default("normal"),
		// last_reply_at lets cs sort the queue by "most recently active"
		// without a join against ticket_messages on every list call.
		// Defaults to created_at on insert; bumped on every new message.
		field.Time("last_reply_at"),
		// Seen + last-reply stamps drive unread / "新回复" without a
		// per-message read table. Internal notes do not bump last_staff_reply_at.
		field.Time("user_seen_at").Optional().Nillable(),
		field.Time("staff_seen_at").Optional().Nillable(),
		field.Time("last_user_reply_at").Optional().Nillable(),
		field.Time("last_staff_reply_at").Optional().Nillable(),
	}
}

func (Ticket) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("messages", TicketMessage.Type),
	}
}

func (Ticket) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "status"),
		index.Fields("status", "last_reply_at"),
		index.Fields("assignee_user_id", "status"),
		index.Fields("order_id"),
		index.Fields("category", "status"),
		index.Fields("user_id", "order_id", "category"),
	}
}
