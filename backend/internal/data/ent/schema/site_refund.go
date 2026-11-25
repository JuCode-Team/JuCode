package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// SiteRefund is a customer-service approval record for an on-site order
// refund. Approval also revokes the entitlement created by the order.
type SiteRefund struct{ ent.Schema }

func (SiteRefund) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (SiteRefund) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("order_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		field.Enum("status").Values("pending", "approved", "rejected", "canceled").Default("pending"),
		field.String("reason").Default(""),
		field.String("note").Default(""),
		field.JSON("policy_snapshot", map[string]any{}).Default(map[string]any{}),
		field.UUID("reviewed_by_user_id", uuid.UUID{}).Optional().Nillable(),
		field.Time("reviewed_at").Optional().Nillable(),
	}
}

func (SiteRefund) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("order", SiteOrder.Type).
			Ref("refunds").
			Field("order_id").
			Required().
			Unique(),
	}
}

func (SiteRefund) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("order_id", "status"),
		index.Fields("user_id", "created_at"),
		index.Fields("status", "created_at"),
		index.Fields("reviewed_at").
			Annotations(entsql.IndexWhere("reviewed_at IS NOT NULL AND status = 'approved'")),
	}
}
