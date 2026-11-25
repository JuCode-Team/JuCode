package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// BillingHold is a pre-charge reservation against a spendable balance
// (user or organization). The reserve/settle/release lifecycle lives in
// internal/biz/billing_reserve.go:
//
//   - Reserve moves `amount` OUT of the account balance and inserts a row
//     with status=held. request_key is the request-level idempotency key,
//     so a retried reserve can never double-hold.
//   - Settle returns the unused part (amount - actual) to the balance,
//     writes ONE deduct ledger row for the actual charge, and flips the
//     row to status=settled with settled_amount recorded.
//   - Release returns the full amount and flips to status=released
//     (nothing billable happened).
//
// While a row is held, the account's ledger (balance_txes /
// organization_balance_txes) intentionally does NOT carry a matching row —
// the reconciliation invariant is:
//
//	balance == Σ(ledger amounts) − Σ(amount of holds with status=held)
//
// expires_at is the crash-safety net: a hold whose owner died is returned
// to the balance by BillingSvc.ReleaseExpiredHolds.
type BillingHold struct{ ent.Schema }

func (BillingHold) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (BillingHold) Fields() []ent.Field {
	return []ent.Field{
		// request_key is the request-level idempotency key (gateway request
		// id; WS turns suffix ":turn<N>"). Unique so a duplicate Reserve
		// cannot double-freeze funds.
		field.String("request_key").NotEmpty().Unique(),
		field.UUID("user_id", uuid.UUID{}),
		// scope decides which balance the amount was frozen from.
		field.Enum("scope").Values("user", "organization"),
		field.UUID("organization_id", uuid.UUID{}).Optional().Nillable(),
		// user_plan_id is carried for pay_as_you_go holds so the settle can
		// stamp usage_logs.user_plan_id / billing_source=plan_balance.
		field.UUID("user_plan_id", uuid.UUID{}).Optional().Nillable(),
		// billing_source is the usage_log billing_source the settle will
		// report: balance | plan_balance | organization_balance.
		field.String("billing_source").Default("balance"),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Other("settled_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().Nillable(),
		field.Enum("status").Values("held", "settled", "released").Default("held"),
		field.Time("expires_at"),
		field.Time("closed_at").Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (BillingHold) Indexes() []ent.Index {
	return []ent.Index{
		// Reaper scan.
		index.Fields("status", "expires_at"),
		// Organization limit occupation sums (held amounts per org / member).
		index.Fields("organization_id", "status"),
		index.Fields("user_id", "status"),
	}
}
