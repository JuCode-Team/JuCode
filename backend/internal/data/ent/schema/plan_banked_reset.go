package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PlanBankedReset is a quota reset an admin granted to a user plan for the
// user to spend when they choose. Spending it clears the used quota of its
// window; it is void once the plan is no longer active. No FK: deleting a
// user plan must not be blocked by its unspent resets.
type PlanBankedReset struct{ ent.Schema }

func (PlanBankedReset) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (PlanBankedReset) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_plan_id", uuid.UUID{}),
		field.Enum("window").Values("all", "5h", "weekly", "monthly"),
		field.UUID("granted_by", uuid.UUID{}),
		field.Time("used_at").Optional().Nillable(),
	}
}

func (PlanBankedReset) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_plan_id", "used_at"),
	}
}
