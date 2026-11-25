package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type UserPlan struct{ ent.Schema }

func (UserPlan) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UserPlan) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		field.UUID("plan_id", uuid.UUID{}),
		field.Time("start_at"),
		field.Time("expire_at"),
		field.Enum("status").Values("active", "expired", "canceled").Default("active"),
		// Each quota window counts from its own anchor once an admin has
		// brought its next reset forward; nil means it counts from start_at.
		// The shipped quota_anchor_at column (one anchor for all windows) was
		// never set in production and is left unused in the table.
		field.Time("quota_anchor_5h_at").Optional().Nillable(),
		field.Time("quota_anchor_weekly_at").Optional().Nillable(),
		field.Time("quota_anchor_monthly_at").Optional().Nillable(),
	}
}

func (UserPlan) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("user_plans").
			Field("user_id").
			Required().
			Unique(),
		edge.To("plan", Plan.Type).
			Field("plan_id").
			Required().
			Unique(),
		edge.To("quota_snapshots", PlanQuotaSnapshot.Type),
	}
}

func (UserPlan) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "status", "expire_at"),
		index.Fields("user_id", "status", "expire_at", "start_at"),
	}
}
