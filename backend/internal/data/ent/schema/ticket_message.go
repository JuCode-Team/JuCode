package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// TicketMessage is one reply in a ticket thread.
//
// sender_role_at_send snapshots the sender's effective role at the
// moment of writing. This matters because cs can be granted/revoked
// later — but the audit trail should still show "this was a cs reply",
// not "this user, who used to be cs, is now just a user".
//
// internal_note marks a message that only cs/admin can read — useful
// for handoff notes between agents without polluting the user view.
type TicketMessage struct{ ent.Schema }

func (TicketMessage) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (TicketMessage) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("ticket_id", uuid.UUID{}),
		field.UUID("sender_user_id", uuid.UUID{}),
		field.Enum("sender_role_at_send").
			Values("user", "cs", "admin", "system"),
		field.Text("body").NotEmpty(),
		field.JSON("attachments", []map[string]string{}).Optional(),
		field.Bool("internal_note").Default(false),
	}
}

func (TicketMessage) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("ticket", Ticket.Type).
			Ref("messages").
			Field("ticket_id").
			Required().
			Unique(),
	}
}

func (TicketMessage) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("ticket_id", "created_at"),
	}
}
