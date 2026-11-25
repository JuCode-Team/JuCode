package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// InviteReward is the (at most one) signup reward attached to an
// InviteBinding. Amount and qualification condition are snapshotted at
// creation so later admin settings changes are never retroactive.
//
// Status machine:
//
//	pending   → created at bind, condition not met yet
//	qualified → condition met, no risk hold; normally auto-paid in the
//	            same evaluation pass — a lingering `qualified` row means
//	            the payout transaction failed and an admin may retry
//	held      → condition met but the risk engine flagged it; admin pays
//	            or rejects manually
//	paid      → credited to the inviter (paid_balance_tx_id set)
//	rejected  → admin refusal; terminal
//	voided    → binding voided before payout; terminal
type InviteReward struct{ ent.Schema }

func (InviteReward) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (InviteReward) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("binding_id", uuid.UUID{}).Unique(),
		field.UUID("inviter_user_id", uuid.UUID{}),
		field.UUID("invitee_user_id", uuid.UUID{}),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		// condition_snapshot: JSON {"condition":"min_topup","min_topup_amount":"10"}
		// captured at creation. Evaluation always reads this, never the live
		// settings, so flipping the admin condition later cannot re-qualify
		// or dis-qualify rewards already on the books.
		field.String("condition_snapshot").Default(""),
		field.Enum("status").
			Values("pending", "qualified", "held", "paid", "rejected", "voided").
			Default("pending"),
		field.JSON("risk_flags", []string{}).Optional(),
		field.Time("paid_at").Optional().Nillable(),
		field.UUID("paid_balance_tx_id", uuid.UUID{}).Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (InviteReward) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("inviter_user_id", "created_at"),
		index.Fields("status", "created_at"),
	}
}
