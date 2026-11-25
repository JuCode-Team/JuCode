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

type OrganizationMember struct{ ent.Schema }

func (OrganizationMember) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (OrganizationMember) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("organization_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
		field.Enum("role").Values("owner", "admin", "member").Default("member"),
		// Rolling monthly cap. 0 = unlimited. Window is the UTC calendar
		// month, matching Organization.monthly_spend_limit.
		field.Other("monthly_spend_limit", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		// Cumulative cap over the member's whole history in this org.
		// 0 = unlimited.
		//
		// Unlike the monthly limit this never resets, so it is the lever for
		// "this person may spend at most N of our money, ever" — handing a
		// contractor a bounded slice of the shared pool without moving money
		// into a sub-wallet.
		//
		// Counted from the organization ledger (organization_balance_txes),
		// not from usage_logs, so it agrees with monthly_spend_limit and with
		// the balance the org actually sees. Removing and re-adding a member
		// does NOT reset it: the ledger rows survive, which is the intent —
		// otherwise the cap would be trivially bypassable by a manager.
		// The DefaultExpr annotation is load-bearing, not decoration:
		// field.Other's Default applies in Go only, so without a SQL-level
		// default the ALTER TABLE that adds this NOT NULL column to a table
		// with existing rows fails with 23502 (it did, on the first deploy
		// attempt). Existing member rows need to come out as 0 = unlimited.
		field.Other("total_spend_limit", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero).
			Annotations(entsql.Annotation{DefaultExpr: "0"}),
	}
}

func (OrganizationMember) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("organization", Organization.Type).
			Ref("members").
			Field("organization_id").
			Required().
			Unique(),
		edge.From("user", User.Type).
			Ref("organization_memberships").
			Field("user_id").
			Required().
			Unique(),
	}
}

func (OrganizationMember) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("organization_id", "user_id").Unique(),
		index.Fields("user_id"),
	}
}
