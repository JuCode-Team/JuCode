package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PersonalAccessToken lets a user call the console APIs (/v1/me, /v1/cs,
// /v1/admin …) from scripts and agents with that user's current role,
// without a browser session. It never satisfies two-factor step-up.
type PersonalAccessToken struct{ ent.Schema }

func (PersonalAccessToken) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (PersonalAccessToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		field.String("name").NotEmpty(),
		field.String("prefix").NotEmpty(),
		// sha256 of the full token; the plaintext is shown once at creation.
		field.String("hash").NotEmpty().Unique().Sensitive(),
		field.Time("expires_at").Optional().Nillable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("revoked_at").Optional().Nillable(),
	}
}

func (PersonalAccessToken) Indexes() []ent.Index {
	return []ent.Index{index.Fields("user_id")}
}
