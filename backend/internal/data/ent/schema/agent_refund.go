package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// AgentRefund is the audit trail of an agent-initiated refund. The
// row's existence is the evidence that the side effects (user_plan
// removal, balance debit, settlement-entry write) all happened — they
// happen in a single transaction with the AgentRefund insert.
//
// For kind=plan, user_plan_id holds the row that was hard-deleted at
// refund time (kept as plain UUID, no FK, mirroring usage_log audit
// rows that survive plan deletion).
//
// For kind=balance, redeem_record_id pins which record we're unwinding
// — refund_amount = redeem_record.amount × 0.9 by policy (the residual
// 10% covers gateway / agent overhead, mirroring the per-user plan
// refund's 0.7 retention).
type AgentRefund struct{ ent.Schema }

func (AgentRefund) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AgentRefund) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("agent_id", uuid.UUID{}),
		field.UUID("redeem_code_id", uuid.UUID{}),
		field.UUID("redeem_record_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
		field.Enum("kind").Values("balance", "plan"),
		field.Other("refund_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		// platform_deduct is the value applied (as a negative) to the
		// agent's pending settlement: deduct = -refund_amount × share/100.
		field.Other("platform_deduct", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		field.UUID("user_plan_id", uuid.UUID{}).Optional().Nillable(),
		field.String("reason").Default(""),
	}
}

func (AgentRefund) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "created_at"),
		index.Fields("redeem_code_id"),
		index.Fields("redeem_record_id").Unique(),
	}
}
