package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// AgentSettlement is the closing row written when an admin runs
// "settle" on an agent's pending entries. It bundles every
// AgentSettlementEntry whose settlement_id was previously NULL and
// snapshots the aggregate so historical settlements stay valid even if
// the underlying entries are later edited.
type AgentSettlement struct{ ent.Schema }

func (AgentSettlement) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AgentSettlement) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("agent_id", uuid.UUID{}),
		// total_payable: net amount the agent owes the platform for the
		// included entries (Σ entries.platform_payable). May be negative
		// when refunds dominate — meaning the platform owes the agent.
		field.Other("total_payable", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.String("currency").Default("CNY"),
		field.Int("entries_count").Default(0),
		field.UUID("settled_by_user_id", uuid.UUID{}).Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (AgentSettlement) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("agent_id", "created_at"),
	}
}
