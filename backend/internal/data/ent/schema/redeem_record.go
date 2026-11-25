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

// RedeemRecord is the ledger of every successful claim against a
// RedeemCode. Used for audit / refunds / dispute investigation.
type RedeemRecord struct{ ent.Schema }

func (RedeemRecord) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (RedeemRecord) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("code_id", uuid.UUID{}),
		field.UUID("user_id", uuid.UUID{}),
		field.Enum("kind").Values("balance", "plan"),
		// kind=balance: amount credited (positive).
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.String("currency").Default("CNY"),
		// kind=plan: the UserPlan row created on redemption.
		field.UUID("user_plan_id", uuid.UUID{}).Optional().Nillable(),
		field.String("ip").Default(""),
	}
}

func (RedeemRecord) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("code", RedeemCode.Type).
			Ref("records").
			Field("code_id").
			Required().
			Unique(),
	}
}

func (RedeemRecord) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "created_at"),
		index.Fields("code_id"),
		// Each (code, user) pair is unique: the same user cannot
		// redeem the same code twice, even if the code's global
		// max_uses still has room. Belt-and-braces — biz.Redeem()
		// also pre-checks for a nicer error, but the DB-level
		// constraint is the source of truth under concurrency.
		index.Fields("code_id", "user_id").Unique(),
	}
}
