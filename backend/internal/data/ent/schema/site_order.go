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

// SiteOrder is the internal order ledger for on-site purchases. Gateway
// fields are optional so operators can still mark orders paid manually.
type SiteOrder struct{ ent.Schema }

func (SiteOrder) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (SiteOrder) Fields() []ent.Field {
	return []ent.Field{
		field.String("order_no").NotEmpty().Unique(),
		field.UUID("user_id", uuid.UUID{}),
		field.Enum("kind").Values("balance", "plan"),
		field.UUID("plan_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("user_plan_id", uuid.UUID{}).Optional().Nillable(),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		// credit_amount is the balance face value delivered after payment.
		// It equals amount for ordinary topups; discounted topups keep amount
		// as the cash charge and credit_amount as the full face value.
		// Plan orders keep it at zero.
		field.Other("credit_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		// Promotion terms are frozen on the order so later profile changes do
		// not rewrite either the buyer's price or the promoter's payable.
		field.UUID("promotion_promoter_user_id", uuid.UUID{}).Optional().Nillable(),
		field.String("promotion_code").Default(""),
		field.Other("promotion_discount_pct", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(5,2)"}).
			Default(decimal.Zero),
		field.Other("promotion_rebate_pct", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(5,2)"}).
			Default(decimal.Zero),
		field.String("currency").Default("CNY"),
		field.Enum("status").Values("pending", "paid", "canceled", "partially_refunded", "refunded").Default("pending"),
		field.String("payment_channel").Default("manual"),
		field.String("payment_trade_type").Default(""),
		field.String("payment_code_url").Default(""),
		field.String("payment_trade_state").Default(""),
		field.String("payment_transaction_id").Default(""),
		field.String("payment_payer_openid").Default(""),
		// PaymentAmountCents is the gateway's integer amount in fen.
		field.Int64("payment_amount_cents").Default(0),
		field.Time("paid_at").Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (SiteOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("refunds", SiteRefund.Type),
		edge.From("user", User.Type).
			Ref("site_orders").
			Field("user_id").
			Required().
			Unique(),
	}
}

func (SiteOrder) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "created_at"),
		index.Fields("status", "created_at"),
		index.Fields("payment_channel", "payment_transaction_id"),
		index.Fields("promotion_promoter_user_id", "created_at"),
		index.Fields("paid_at").
			Annotations(entsql.IndexWhere("paid_at IS NOT NULL AND status IN ('paid', 'partially_refunded', 'refunded')")),
	}
}
