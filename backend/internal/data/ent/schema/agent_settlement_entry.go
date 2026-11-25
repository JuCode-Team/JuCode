package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// AgentSettlementEntry is the per-event ledger row that drives
// settlement. Two flavours:
//
//   - kind=sale: a redeem_code with agent_id was just redeemed.
//     sale_amount = the code's face value (kind=balance) or
//     the plan price (kind=plan). platform_payable =
//     sale_amount × profile.platform_share_pct / 100.
//
//   - kind=refund: agent registered a refund. sale_amount is the NEGATIVE
//     refund_amount (refunds shrink the net revenue),
//     platform_payable is the negative scaled value
//     (platform_payable = -refund_amount × share).
//
//   - kind=coupon_rebate: a site order paid with the agent's coupon.
//     sale_amount = the order's paid amount, platform_payable = -rebate
//     (the platform owes the agent), share_pct_at_event = the rebate rate
//     snapshotted on the order.
//
//   - kind=coupon_refund: an approved refund on such an order. Reverses the
//     matching share of the rebate, so platform_payable is positive and
//     sale_amount is the negative refund amount.
//
//   - kind=balance_withdraw: the agent moved commission the platform owed
//     into their own platform balance (AgentSvc.WithdrawToBalance).
//     platform_payable = +amount (the debt is paid), sale_amount = 0,
//     source_id = the balance_tx that credited it. Settlements net it like
//     any other row, so the same commission is never paid out twice.
//
// settlement_id is NULL until the entry is bundled into an
// AgentSettlement. Setting it is the act of "closing the books" for
// this entry.
type AgentSettlementEntry struct{ ent.Schema }

func (AgentSettlementEntry) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AgentSettlementEntry) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("agent_id", uuid.UUID{}),
		field.Enum("kind").Values("sale", "refund", "coupon_rebate", "coupon_refund", "balance_withdraw"),
		field.Other("sale_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Other("platform_payable", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Other("share_pct_at_event", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(5,2)"}),
		field.String("currency").Default("CNY"),
		field.Enum("source_type").
			Values("redeem_record", "agent_refund", "site_order", "site_refund", "balance_tx").
			// Mostly for human readability when scanning the table — the
			// FK lookup is via source_id anyway.
			Default("redeem_record"),
		field.UUID("source_id", uuid.UUID{}),
		// settlement_id may be NULL while the entry sits in the "pending"
		// pool waiting to be closed out by an admin click.
		field.UUID("settlement_id", uuid.UUID{}).Optional().Nillable(),
		// redeem_code_id and (optional) user_id give the UI enough info
		// to render the row without an extra round-trip.
		field.UUID("redeem_code_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("user_id", uuid.UUID{}).Optional().Nillable(),
		// order_id links coupon_rebate/coupon_refund rows to their site
		// order so refunds can find the rebate they reverse.
		field.UUID("order_id", uuid.UUID{}).Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (AgentSettlementEntry) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "settlement_id"),
		index.Fields("agent_id", "created_at"),
		// One ledger row per source event: replayed payment callbacks and
		// refund approvals hit this constraint instead of paying twice.
		index.Fields("source_type", "source_id").Unique(),
		index.Fields("order_id"),
	}
}
