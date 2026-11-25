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

type OrganizationBalanceTx struct{ ent.Schema }

func (OrganizationBalanceTx) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (OrganizationBalanceTx) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("organization_id", uuid.UUID{}),
		field.UUID("actor_user_id", uuid.UUID{}).Optional().Nillable(),
		field.Enum("kind").Values("topup", "deduct", "adjust"),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Other("balance_after", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		field.UUID("related_usage_log_id", uuid.UUID{}).Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (OrganizationBalanceTx) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("organization", Organization.Type).
			Ref("balance_txs").
			Field("organization_id").
			Required().
			Unique(),
		edge.From("actor", User.Type).
			Ref("organization_balance_txs").
			Field("actor_user_id").
			Unique(),
	}
}

func (OrganizationBalanceTx) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("organization_id", "created_at"),
		// Per-member spend lives on the charge hot path: every org-billed
		// request sums this member's deducts to test their monthly and total
		// caps, while holding the organization row lock. Without this index
		// that is a scan of the whole org's ledger, inside the lock, once per
		// request — and the cumulative cap makes it a scan of ALL history
		// rather than of the current month.
		index.Fields("organization_id", "actor_user_id", "created_at"),
	}
}
