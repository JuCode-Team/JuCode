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

// ProviderModel declares which logical model names a Provider serves, and
// optionally rewrites the model name sent upstream (e.g. user asks for
// "gpt-5.3-codex" but this provider needs "gpt-4o-mini").
type ProviderModel struct{ ent.Schema }

func (ProviderModel) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ProviderModel) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("provider_id", uuid.UUID{}),
		field.String("model_name").NotEmpty(),
		// If empty, model_name is forwarded verbatim.
		field.String("upstream_model").Default(""),
		field.Bool("enabled").Default(true),
		field.Enum("cost_mode").Values("none", "per_request", "per_usage").Default("none"),
		field.Other("price_per_request_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("input_unit_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("output_unit_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("cache_read_input_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("cache_write_input_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("cache_write_1h_input_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.String("currency").Default("CNY"),
	}
}

func (ProviderModel) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("provider", Provider.Type).
			Ref("models").
			Field("provider_id").
			Required().
			Unique(),
	}
}

func (ProviderModel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("provider_id", "model_name").Unique(),
	}
}
