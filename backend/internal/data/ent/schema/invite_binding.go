package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// InviteBinding is the authoritative invitee→inviter record created at
// registration when a valid invite code was supplied. One row per invitee
// forever (unique invitee_user_id) — re-binding is impossible even after a
// void, so a voided invitee cannot be re-farmed under another code.
type InviteBinding struct{ ent.Schema }

func (InviteBinding) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (InviteBinding) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("invitee_user_id", uuid.UUID{}).Unique(),
		field.UUID("inviter_user_id", uuid.UUID{}),
		// invite_code_used: snapshot of the code as entered, kept even if the
		// inviter's code ever changes, so the bind stays explainable.
		field.String("invite_code_used"),
		// register_ip / register_user_agent feed the risk engine (same-IP
		// clustering). Optional: proxies may hide the IP entirely.
		field.String("register_ip").Optional().Default(""),
		field.String("register_user_agent").Optional().Default(""),
		// voided: admin decision — unpaid rewards/rebates under this binding
		// are voided and no future rebates accrue. Paid amounts are NOT
		// clawed back (v1).
		field.Enum("status").Values("bound", "voided").Default("bound"),
	}
}

func (InviteBinding) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("inviter_user_id", "created_at"),
		// Same-IP daily cap counts bindings by (inviter, ip) — keep it an
		// index scan rather than a per-registration seq scan.
		index.Fields("inviter_user_id", "register_ip", "created_at"),
	}
}
