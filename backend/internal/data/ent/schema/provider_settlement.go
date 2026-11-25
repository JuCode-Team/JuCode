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

// ProviderSettlement stores the monthly operator-confirmed provider cost.
// Usage/model/manual-cost details remain derived from source rows for audit.
type ProviderSettlement struct{ ent.Schema }

func (ProviderSettlement) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ProviderSettlement) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("provider_id", uuid.UUID{}),
		field.Time("period_start"),
		field.Time("period_end"),
		field.Other("calculated_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Other("actual_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.String("currency").Default("CNY"),
		field.UUID("confirmed_by_user_id", uuid.UUID{}).Optional().Nillable(),
		field.Time("confirmed_at").Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (ProviderSettlement) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("provider", Provider.Type).
			Ref("settlements").
			Field("provider_id").
			Required().
			Unique(),
	}
}

func (ProviderSettlement) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("provider_id", "period_start").Unique(),
		index.Fields("period_start"),
	}
}
