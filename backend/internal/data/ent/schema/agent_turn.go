package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// AgentTurn is one turn of the JuCode Desktop agent, kept apart from
// usage_logs (which also carries the user's other tools). The client
// uploads the token counts it observed (reported=true); the gateway adds
// what it charged for requests tagged with X-JuCode-Turn (gw_*). Either
// side may create the row first. All writes go through raw upserts in
// biz/agent_usage.go that supply every column, so no DDL default is relied on.
type AgentTurn struct{ ent.Schema }

func (AgentTurn) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AgentTurn) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		// Client-generated, [A-Za-z0-9_-]{1,64}.
		field.String("turn_id").MaxLen(64),
		// Device that ran the turn (oauth_authorizations.id).
		field.UUID("authorization_id", uuid.UUID{}).Optional().Nillable(),
		field.String("session_id").Default(""),
		// jucode | claude | codex | acp
		field.String("engine").Default(""),
		// jucode | third_party | local
		field.String("channel_kind").Default(""),
		// Provider id inside the channel kind.
		field.String("channel").Default(""),
		field.String("model").Default(""),
		// OpenAI semantics: cached ⊆ input, reasoning ⊆ output.
		field.Int64("input_tokens").Default(0),
		field.Int64("cached_input_tokens").Default(0),
		field.Int64("cache_write_tokens").Default(0),
		field.Int64("output_tokens").Default(0),
		field.Int64("reasoning_tokens").Default(0),
		field.Int("requests").Default(0),
		field.Time("started_at").Optional().Nillable(),
		field.Time("ended_at").Optional().Nillable(),
		field.Bool("reported").Default(false),
		// Gateway side: money actually charged (usage_logs.cost_final unit),
		// request count and the group that served the turn's requests.
		field.Other("gw_cost", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Int("gw_requests").Default(0),
		field.String("gw_group").Default(""),
	}
}

func (AgentTurn) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "turn_id").Unique(),
		index.Fields("user_id", "started_at"),
	}
}
