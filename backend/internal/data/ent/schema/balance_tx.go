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

type BalanceTx struct{ ent.Schema }

func (BalanceTx) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (BalanceTx) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		// transfer_out: balance leaving this account for another ledger —
		// an organization's balance (OrganizationSvc.TransferFromMember). It
		// is deliberately NOT `deduct`: deduct rows are LLM charges, and the
		// billing page reads them as such.
		//
		// gift: a plan's promotional balance grant (positive) or its
		// revocation on refund (negative). It is deliberately separate from
		// topup so refunds and invite qualification never treat it as cash.
		//
		// invite_reward / invite_rebate: movements from the invite program
		// (internal/biz/invite.go) — signup reward payout and topup rebate
		// payout respectively. Normally positive; they go NEGATIVE when a
		// refund on the invitee's order claws the payout back
		// (InviteSvc.ReconcileRefund), which can leave the inviter with a
		// negative balance until their next topup absorbs it.
		//
		// They are deliberately NOT `topup`: the signup-qualification sum and
		// finance views read `topup` as real money in, and invite credits are
		// marketing spend, not revenue. Equally they are not `refund`, which
		// inviteeTopupTotal nets against topups on the BUYER's ledger.
		//
		// agent_commission: an agent withdrawing owed commission into their
		// balance (AgentSvc.WithdrawToBalance). Not `topup` for the same
		// reason: no new money came in.
		field.Enum("kind").Values("topup", "gift", "refund", "deduct", "adjust", "transfer_out", "invite_reward", "invite_rebate", "agent_commission"),
		// amount: positive = increases user balance; negative = decreases.
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Other("balance_after", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		field.UUID("related_usage_log_id", uuid.UUID{}).Optional().Nillable(),
		// related_user_plan_id identifies the entitlement that issued a gift.
		// Both grant and revocation rows carry it, making the remaining gift
		// reproducible from the immutable balance ledger.
		field.UUID("related_user_plan_id", uuid.UUID{}).Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (BalanceTx) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("balance_txs").
			Field("user_id").
			Required().
			Unique(),
	}
}

func (BalanceTx) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "created_at"),
	}
}
