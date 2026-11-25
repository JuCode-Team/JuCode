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

// RedeemCode is a single redemption token that, when claimed by a user,
// either credits balance (kind=balance, amount required) or activates
// a plan (kind=plan, plan_id required).
//
// Admins typically batch-generate codes with max_uses=1 (each code claimed
// once). max_uses>1 supports a single "shared" code (e.g. event promo).
type RedeemCode struct{ ent.Schema }

func (RedeemCode) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (RedeemCode) Fields() []ent.Field {
	return []ent.Field{
		field.String("code").NotEmpty().Unique(),
		field.Enum("kind").Values("balance", "plan"),
		field.Other("amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.String("currency").Default("CNY"),
		field.UUID("plan_id", uuid.UUID{}).Optional().Nillable(),
		field.Int("max_uses").Default(1),
		field.Int("used_count").Default(0),
		field.Time("expires_at").Optional().Nillable(),
		field.Enum("status").
			Values("active", "exhausted", "revoked", "expired").
			Default("active"),
		// batch_id groups codes generated together; admin can filter / revoke
		// the whole batch with one query.
		field.String("batch_id").Default(""),
		field.String("note").Default(""),
		field.UUID("created_by_user_id", uuid.UUID{}).Optional().Nillable(),
		// agent_id pins the agent who generated the code (created_by_user_id
		// is the human-style "who clicked the button" — admin and agent
		// share that column). Settlement logic switches on this field:
		// only codes with agent_id != nil produce settlement entries.
		field.UUID("agent_id", uuid.UUID{}).Optional().Nillable(),
	}
}

func (RedeemCode) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("records", RedeemRecord.Type),
	}
}

func (RedeemCode) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "batch_id", "created_at"),
		index.Fields("kind", "status"),
		index.Fields("agent_id", "status"),
	}
}
