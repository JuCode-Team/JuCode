package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// UpstreamAccountCost records manual one-off account costs that cannot be
// derived from token pricing, such as account rental, proxy fixes, or incident
// spend. Replaces the old per-channel ProviderCost.
type UpstreamAccountCost struct{ ent.Schema }

func (UpstreamAccountCost) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UpstreamAccountCost) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("account_id", uuid.UUID{}),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		field.Time("occurred_at"),
		field.String("note").Default(""),
	}
}

func (UpstreamAccountCost) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("account", UpstreamAccount.Type).
			Ref("manual_costs").
			Field("account_id").
			Required().
			Unique(),
	}
}

func (UpstreamAccountCost) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id", "occurred_at"),
	}
}
