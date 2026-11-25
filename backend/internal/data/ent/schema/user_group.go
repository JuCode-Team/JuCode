package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// UserGroup is a coarse-grained user bucket (VIP / 普通 / 内测 …) used to
// gate access to ModelGroups. A user belongs to at most one UserGroup;
// UserGroupless users can still access any ModelGroup whose
// allowed_user_group_ids is empty (public by default).
type UserGroup struct{ ent.Schema }

func (UserGroup) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UserGroup) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().Unique(),
		field.String("description").Default(""),
		// Lower sort_order shows first in admin pickers.
		field.Int("sort_order").Default(0),
		// Max simultaneous in-flight LLM requests for users in this group
		// when their request is billed against `balance_only`. 0 = unlimited.
		field.Int("concurrency_limit").Default(0).NonNegative(),
		// Rate caps over a rolling 60s window for `balance_only` billing.
		// 0 = unlimited, matching concurrency_limit.
		field.Int("rpm_limit").Default(0).NonNegative(),
		field.Int("tpm_limit").Default(0).NonNegative(),
		// Exactly one row should have is_default=true at any time; the biz
		// layer enforces this in a transaction (DB-level partial-unique
		// index would also work but adds Postgres-specific schema).
		// Users with NULL user_group_id are gated by this row.
		field.Bool("is_default").Default(false),
	}
}

func (UserGroup) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("users", User.Type),
		edge.To("model_group_grants", ModelGroupUserGroupGrant.Type),
	}
}
