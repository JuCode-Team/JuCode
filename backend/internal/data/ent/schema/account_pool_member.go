package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AccountPoolMember puts one upstream account into one pool. Pure management
// grouping: it never participates in routing or billing.
type AccountPoolMember struct{ ent.Schema }

func (AccountPoolMember) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AccountPoolMember) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("pool_id", uuid.UUID{}),
		field.UUID("account_id", uuid.UUID{}),
	}
}

func (AccountPoolMember) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("pool", AccountPool.Type).
			Ref("members").
			Field("pool_id").
			Required().
			Unique(),
		edge.From("account", UpstreamAccount.Type).
			Ref("pool_members").
			Field("account_id").
			Required().
			Unique(),
	}
}

func (AccountPoolMember) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("pool_id", "account_id").Unique(),
	}
}
