package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ModelGroupProvider declares which providers serve a group. Routing tries
// lower priority tiers first; providers within the same priority are ordered
// by weighted random shuffle so retries can fall through the same tier.
type ModelGroupProvider struct{ ent.Schema }

func (ModelGroupProvider) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ModelGroupProvider) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("provider_id", uuid.UUID{}),
		// Lower = tried first.
		field.Int("priority").Default(100),
		field.Int("weight").Default(100),
		field.Bool("enabled").Default(true),
	}
}

func (ModelGroupProvider) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", ModelGroup.Type).
			Ref("provider_links").
			Field("group_id").
			Required().
			Unique(),
		edge.From("provider", Provider.Type).
			Ref("group_links").
			Field("provider_id").
			Required().
			Unique(),
	}
}

func (ModelGroupProvider) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "provider_id").Unique(),
		index.Fields("group_id", "priority", "weight"),
	}
}
