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

// UpstreamAccountModel declares which logical model names one account serves,
// optionally rewriting the model name sent upstream, and carries the
// account-side cost basis (what this account pays for the model).
type UpstreamAccountModel struct{ ent.Schema }

func (UpstreamAccountModel) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UpstreamAccountModel) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("account_id", uuid.UUID{}),
		field.String("model_name").NotEmpty(),
		// If empty, model_name is forwarded verbatim.
		field.String("upstream_model").Default(""),
		// Optional per-model pin for the wire protocol the upstream is spoken
		// to in ("chat_completions" | "responses" | "messages"). Empty follows
		// the account-level protocol rules; when set it must be one of the
		// account's native_protocols for the route to be usable.
		field.String("upstream_protocol").Default(""),
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
		// Reference pricing in USD per 1M tokens, captured from the upstream
		// model list or the public model catalog. Display-only: billing always
		// uses the cost fields above.
		field.Other("ref_input_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("ref_output_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// Context window this account actually gives the model, when it
		// differs from the model's default (e.g. a Codex account capping a
		// 1M model at 272K). Nil inherits models.context_window.
		field.Int("context_window").Optional().Nillable(),
	}
}

func (UpstreamAccountModel) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("account", UpstreamAccount.Type).
			Ref("models").
			Field("account_id").
			Required().
			Unique(),
	}
}

func (UpstreamAccountModel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("account_id", "model_name").Unique(),
	}
}
