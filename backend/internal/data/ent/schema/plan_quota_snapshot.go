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

// PlanQuotaSnapshot is the cold-backup of the per-window quota usage.
// The hot path is Redis (keyed by user_plan_id + window + window_start);
// a periodic job dumps the current counters here so that, on a Redis
// flush/restart, we can re-seed counters from this table.
type PlanQuotaSnapshot struct{ ent.Schema }

func (PlanQuotaSnapshot) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (PlanQuotaSnapshot) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_plan_id", uuid.UUID{}),
		field.Enum("window").Values("5h", "weekly", "monthly"),
		field.Time("window_start"),
		field.Other("used", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
	}
}

func (PlanQuotaSnapshot) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user_plan", UserPlan.Type).
			Ref("quota_snapshots").
			Field("user_plan_id").
			Required().
			Unique(),
	}
}

func (PlanQuotaSnapshot) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_plan_id", "window", "window_start").Unique(),
	}
}
