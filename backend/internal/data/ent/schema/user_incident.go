package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UserIncident is the user-facing complement to AdminAuditLog. It tracks
// abnormal events that originate from end-user traffic — policy keyword
// hits, quota breaches, auth failures, repeated 401s, etc. Rows are
// queryable in the admin console as "用户异常".
//
// kind is loose enum because new categories will arrive over time
// (vendor-side bans, anomaly detectors, ...); ent's Values() enforces it
// today and can be widened with a follow-up migration.
type UserIncident struct{ ent.Schema }

func (UserIncident) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UserIncident) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}).Optional().Nillable().
			Comment("nil when the request was unauthenticated"),
		field.UUID("api_key_id", uuid.UUID{}).Optional().Nillable(),
		field.Enum("kind").Values(
			"policy_keyword", // moderation dictionary hit
			"cyber_security", // upstream (OpenAI) cyber-security signal
			"rate_limit",     // per-user rate limit tripped
			"quota_exceeded", // plan quota exhausted
			"auth_failure",   // bad token / wrong api key
			"forbidden",      // permission denied (group access, etc.)
			"llm_moderation", // async LLM moderation verdict (sampling/recheck)
			"sanction",       // automated account sanction (cooldown/freeze/ban)
			"other",
		),
		field.Enum("severity").Values("info", "low", "medium", "high", "critical").Default("medium"),
		field.String("action").Default("").
			Comment("What enforcement did we run? blocked / logged / cooldown / freeze / ban / ..."),
		// review_label is the human review verdict, fed back from the admin
		// console to measure and tune false-positive rate (see admin annotate
		// route). "label" collides with a generated identifier, hence the name.
		field.Enum("review_label").Values("unreviewed", "true_positive", "false_positive").
			Default("unreviewed"),
		field.String("model").Default(""),
		field.String("endpoint").Default(""),
		field.String("request_id").Default(""),
		field.String("ip").Default(""),
		field.String("message").Default("").Comment("Short human summary surfaced in the admin list"),
		field.JSON("payload", map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Optional(),
	}
}

func (UserIncident) Indexes() []ent.Index {
	return []ent.Index{
		// Hot path queries: list by user, list by kind, both ordered by
		// recency. created_at trailing keeps the index aligned with the
		// SELECT ... ORDER BY created_at DESC LIMIT 50 pattern.
		index.Fields("user_id", "created_at"),
		index.Fields("kind", "created_at"),
		index.Fields("severity", "created_at"),
	}
}
