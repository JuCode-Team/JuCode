package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// AccountPool groups upstream accounts for management (a "号池"). Membership
// never affects routing or billing; the one operational thing a pool carries is
// pool_mode, the cooldown/disable policy its accounts inherit unless they
// override it.
type AccountPool struct{ ent.Schema }

func (AccountPool) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AccountPool) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.Enum("pool_mode").
			Values("balanced", "conservative", "lenient", "observe").
			Default("balanced"),
		// quota_policy: pool-level auto-pause thresholds, e.g.
		// {"pause_at_percent": 95, "windows": ["5h", "7d"]}.
		field.JSON("quota_policy", map[string]any{}).Optional(),
		field.Int("sort_order").Default(0),
		field.Bool("enabled").Default(true),
		field.String("note").Default(""),
	}
}

func (AccountPool) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("members", AccountPoolMember.Type),
	}
}

func (AccountPool) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("sort_order"),
	}
}
