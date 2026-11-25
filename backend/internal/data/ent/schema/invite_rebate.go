package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// InviteRebate is one rebate entry credited to an inviter for something
// their invitee did. Two source kinds share the table:
//
//	topup   — the live model: a percentage of one PAID balance order.
//	          One row per order (unique source_order_id). Refunding that
//	          order claws the rebate back proportionally.
//	consume — retired 2026-09-03. Was a percentage of every individual
//	          billed request, i.e. one row per usage log. Nothing writes
//	          these any more; existing rows stay readable so historical
//	          payouts remain explainable.
//
// Whichever source, the id column of that source is the idempotency key —
// the pipeline is retried at-least-once and the unique constraint is what
// makes a replay a no-op. Both columns are nullable because each row only
// has one of them; Postgres unique indexes ignore NULLs.
//
// Rate is snapshotted per row: changing the admin rate only affects orders
// paid after the change.
//
// Status machine: pending|held|paid|rejected|voided. `pending` normally
// exists only for the instant between row insert and payout; a lingering
// pending row means the payout transaction failed and an admin may retry
// via the pay endpoint.
type InviteRebate struct{ ent.Schema }

func (InviteRebate) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (InviteRebate) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("inviter_user_id", uuid.UUID{}),
		field.UUID("invitee_user_id", uuid.UUID{}),
		field.Enum("source_kind").
			Values("consume", "topup").
			Default("consume"),
		// source_order_id: the site_orders row (kind=balance) whose payment
		// produced this rebate. Set on source_kind=topup rows only.
		field.UUID("source_order_id", uuid.UUID{}).Optional().Nillable().Unique(),
		// source_usage_log_id: the usage log whose charge produced this
		// rebate. Set on retired source_kind=consume rows only. Plain
		// column, no FK: usage_logs inserts may be batched and land after
		// this row.
		field.UUID("source_usage_log_id", uuid.UUID{}).Optional().Nillable().Unique(),
		field.Other("source_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Other("rate_pct_snapshot", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Other("rebate_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		// clawed_back_amount: how much of a PAID rebate has been taken back
		// because the source order was refunded. Always 0 <= this <=
		// rebate_amount. The row stays `paid` and keeps its original
		// rebate_amount so the payout history is not rewritten; the net
		// credit is rebate_amount - clawed_back_amount.
		field.Other("clawed_back_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.String("currency").Default("CNY"),
		field.Enum("status").
			Values("pending", "held", "paid", "rejected", "voided").
			Default("pending"),
		field.JSON("risk_flags", []string{}).Optional(),
		field.Time("paid_at").Optional().Nillable(),
		field.UUID("paid_balance_tx_id", uuid.UUID{}).Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (InviteRebate) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("inviter_user_id", "created_at"),
		index.Fields("invitee_user_id", "created_at"),
		index.Fields("status", "created_at"),
	}
}
