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

type Organization struct{ ent.Schema }

func (Organization) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (Organization) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.UUID("owner_user_id", uuid.UUID{}),
		field.Other("balance", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.String("currency").Default("CNY"),
		field.Other("monthly_spend_limit", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
	}
}

func (Organization) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("owner", User.Type).
			Ref("owned_organizations").
			Field("owner_user_id").
			Required().
			Unique(),
		edge.To("members", OrganizationMember.Type),
		edge.To("balance_txs", OrganizationBalanceTx.Type),
		edge.To("api_keys", ApiKey.Type),
		edge.To("usage_logs", UsageLog.Type),
	}
}

func (Organization) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("owner_user_id"),
		index.Fields("name"),
	}
}
