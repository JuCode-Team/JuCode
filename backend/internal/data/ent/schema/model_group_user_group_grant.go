package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ModelGroupUserGroupGrant lets every member of a UserGroup into an
// exclusive ModelGroup. Unassigned users (NULL user_group_id) do not
// inherit the default UserGroup's grants.
type ModelGroupUserGroupGrant struct{ ent.Schema }

func (ModelGroupUserGroupGrant) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ModelGroupUserGroupGrant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("user_group_id", uuid.UUID{}),
	}
}

func (ModelGroupUserGroupGrant) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", ModelGroup.Type).
			Ref("user_group_grants").
			Field("group_id").
			Required().
			Unique(),
		edge.From("user_group", UserGroup.Type).
			Ref("model_group_grants").
			Field("user_group_id").
			Required().
			Unique(),
	}
}

func (ModelGroupUserGroupGrant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "user_group_id").Unique(),
		index.Fields("user_group_id"),
	}
}
