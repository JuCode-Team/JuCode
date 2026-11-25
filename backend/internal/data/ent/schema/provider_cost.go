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

// ProviderCost records manual one-off channel costs that cannot be derived
// from token pricing, such as account rental, proxy fixes, or incident spend.
type ProviderCost struct{ ent.Schema }

func (ProviderCost) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ProviderCost) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("provider_id", uuid.UUID{}),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		field.Time("occurred_at"),
		field.String("note").Default(""),
	}
}

func (ProviderCost) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("provider", Provider.Type).
			Ref("manual_costs").
			Field("provider_id").
			Required().
			Unique(),
	}
}

func (ProviderCost) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("provider_id", "occurred_at"),
	}
}
