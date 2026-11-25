package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// AgentProfile holds the per-agent commercial configuration. The
// presence of an AgentProfile row is independent from the user_roles
// "agent" grant: the row stores defaults (commission %), the role row
// controls access. Profiles auto-create lazily on first
// /v1/agent/* call so admins don't need to seed them by hand.
//
// The agent's incentive budget is 100 - platform_share_pct, capped by the
// site's max_incentive_pct. Both channels spend the same budget: redeem codes
// are wholesaled at platform_share_pct of face value, and the reusable coupon
// gives buyers coupon_discount_pct off while the agent earns the rest of the
// budget on the paid amount.
type AgentProfile struct{ ent.Schema }

func (AgentProfile) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AgentProfile) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}).Unique(),
		// platform_share_pct is the percentage of every sale the platform
		// claims back from the agent at settlement time. Default 70 means
		// agent keeps 30 net. Range [0, 100]; stored as numeric(5,2) so
		// fractional percents stay exact.
		field.Other("platform_share_pct", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(5,2)"}).
			Default(decimal.NewFromInt(70)),
		field.Enum("status").Values("active", "suspended").Default("active"),
		// coupon_code is the agent's single permanent reusable coupon
		// (C-XXXXXXXX). NULL until the agent creates it.
		field.String("coupon_code").Optional().Nillable().Unique(),
		// coupon_discount_pct is the buyer discount the coupon grants, taken
		// out of the agent's incentive budget.
		field.Other("coupon_discount_pct", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(5,2)"}).
			Default(decimal.Zero),
		field.String("note").Default(""),
	}
}

func (AgentProfile) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
	}
}
