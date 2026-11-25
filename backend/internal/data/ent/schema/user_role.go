package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UserRole is a many-to-many bridge between user and a functional role.
// The existing `user.role` enum (user | admin) stays untouched — it
// captures the "primary identity" used by the JWT-side AdminOnly gate
// and the legacy code paths that compare role == "admin" directly.
//
// This table holds the *additional* functional roles a user can hold
// simultaneously (cs, agent). One user may carry several. Admins implicitly
// inherit every functional role at the middleware layer, so we do NOT
// write redundant cs/agent rows for them.
type UserRole struct{ ent.Schema }

func (UserRole) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UserRole) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		field.Enum("role").Values("cs", "agent"),
		// granted_by tracks the admin who assigned the role. Nullable so
		// that bootstrap / migration code can grant without spoofing an
		// admin identity.
		field.UUID("granted_by_user_id", uuid.UUID{}).Optional().Nillable(),
	}
}

func (UserRole) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("roles").
			Field("user_id").
			Required().
			Unique(),
	}
}

func (UserRole) Indexes() []ent.Index {
	return []ent.Index{
		// One row per (user, role). Granting the same role twice is a no-op.
		index.Fields("user_id", "role").Unique(),
		index.Fields("role"),
	}
}
