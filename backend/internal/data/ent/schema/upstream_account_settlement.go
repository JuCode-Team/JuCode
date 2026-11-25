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

// UpstreamAccountSettlement stores the monthly operator-confirmed cost of one
// account. Usage/model/manual-cost details remain derived from source rows for
// audit. Replaces the old per-channel ProviderSettlement.
type UpstreamAccountSettlement struct{ ent.Schema }

func (UpstreamAccountSettlement) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UpstreamAccountSettlement) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("account_id", uuid.UUID{}),
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

func (UpstreamAccountSettlement) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("account", UpstreamAccount.Type).
			Ref("settlements").
			Field("account_id").
			Required().
			Unique(),
	}
}

func (UpstreamAccountSettlement) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id", "period_start").Unique(),
		index.Fields("period_start"),
	}
}
