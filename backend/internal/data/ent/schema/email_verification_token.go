package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// EmailVerificationToken backs three time-limited email flows:
//
//	kind=register        new account email confirmation
//	kind=change_email    user moved their email; verify the NEW address
//	kind=reset_password  forgot-password flow
//
// Lifecycle: created on send → marked used on click → DELETE-able once
// expires_at < now (a periodic vacuum keeps the table small).
// One unused row per (user, kind) — creating a new one revokes the
// previous via UNIQUE (user_id, kind, used_at IS NULL) handled at the
// biz layer (UPSERT semantics).
type EmailVerificationToken struct{ ent.Schema }

func (EmailVerificationToken) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (EmailVerificationToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		field.Enum("kind").Values("register", "change_email", "reset_password"),
		// code is the random URL-safe secret sent in the email link.
		// 32 bytes random → base64url (43 chars), unique so an
		// attacker can't trivially guess.
		field.String("code").NotEmpty().Unique(),
		// pending_email is the NEW address for kind=change_email.
		// Unused for kind=register (the user's current email is also
		// the target) and kind=reset_password.
		field.String("pending_email").Default(""),
		field.Time("expires_at"),
		field.Time("used_at").Optional().Nillable(),
		field.String("ip").Default("").Comment("IP that initiated the token request, for audit"),
	}
}

func (EmailVerificationToken) Indexes() []ent.Index {
	return []ent.Index{
		// Hot lookup: bearer hits POST /verify?code=...
		index.Fields("code"),
		// Vacuum scan.
		index.Fields("expires_at"),
		// Revoke previous unused tokens for the same (user, kind).
		index.Fields("user_id", "kind"),
	}
}
