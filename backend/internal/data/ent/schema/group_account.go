package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// GroupAccount binds one upstream account to one business model group.
// Routing tries lower priority tiers first; accounts within the same priority
// are ordered by weighted random shuffle so retries can fall through the tier.
type GroupAccount struct{ ent.Schema }

func (GroupAccount) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (GroupAccount) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("account_id", uuid.UUID{}),
		// Lower = tried first.
		field.Int("priority").Default(100),
		field.Int("weight").Default(100),
		field.Bool("enabled").Default(true),
		// model_names is the optional per-group whitelist of logical model
		// names (the same key upstream_account_models.model_name uses).
		// Empty means the account serves every model it is configured for;
		// a non-empty list narrows that account to the named models inside
		// this group only.
		field.JSON("model_names", []string{}).Optional(),
	}
}

func (GroupAccount) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", ModelGroup.Type).
			Ref("account_links").
			Field("group_id").
			Required().
			Unique(),
		edge.From("account", UpstreamAccount.Type).
			Ref("group_links").
			Field("account_id").
			Required().
			Unique(),
	}
}

func (GroupAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "account_id").Unique(),
		index.Fields("group_id", "priority", "weight"),
	}
}
