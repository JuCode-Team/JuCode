package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ModelGroupUserGrant lets one user into an exclusive ModelGroup.
// Public (non-exclusive) groups ignore this table.
type ModelGroupUserGrant struct{ ent.Schema }

func (ModelGroupUserGrant) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ModelGroupUserGrant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
	}
}

func (ModelGroupUserGrant) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", ModelGroup.Type).
			Ref("user_grants").
			Field("group_id").
			Required().
			Unique(),
		edge.From("user", User.Type).
			Ref("model_group_grants").
			Field("user_id").
			Required().
			Unique(),
	}
}

func (ModelGroupUserGrant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "user_id").Unique(),
		index.Fields("user_id"),
	}
}
