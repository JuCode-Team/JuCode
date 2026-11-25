package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

type ModelGroup struct{ ent.Schema }

// GroupProfileDim is one axis of the six-dimension radar chart shown on the
// public /models page. Value is a raw score on a 0-100 scale; admins set both
// the axis label and the score per group, and unset dimensions are simply not
// stored (the chart omits them rather than guessing).
type GroupProfileDim struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

func (ModelGroup) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ModelGroup) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().Unique(),
		// plan_only:    only deducts from active UserPlan windows
		// balance_only: only deducts from user.balance
		field.Enum("billing_source").Values("plan_only", "balance_only"),
		// Applied to the model's base cost: cost_final = cost_base * rate_multiplier.
		// Stored as float because admin sets it as a percent-ish lever; pricing
		// precision is preserved in the model unit prices and decimal math below.
		field.Float("rate_multiplier").Default(1.0),
		field.String("description").Default(""),
		// False = the group is retired: it stops routing and is hidden from
		// every user-facing surface (public /models, status page,
		// /me/available-groups). Admin still sees it in the group list so it
		// can be re-enabled. Equivalent to Model.enabled / Provider.enabled.
		field.Bool("enabled").Default(true),
		// exclusive: only users (or their UserGroup) listed in the grant
		// tables may use this group. Public groups ignore those tables.
		// An exclusive group with zero grants is locked — nobody routes
		// through it. Default false so existing rows stay public.
		field.Bool("exclusive").Default(false),
		// Legacy JSON allowlist. Empty after migrate. Non-empty is still
		// honored by CheckAccess until jucode-migrate copies surviving IDs
		// into model_group_user_group_grants and clears this column.
		field.JSON("allowed_user_group_ids", []uuid.UUID{}).Default([]uuid.UUID{}),
		// Lower sort_order shows first in the admin group list and on /models.
		// Ties fall back to name, so a fresh install (all zeroes) keeps the
		// alphabetical order it had before this field existed.
		field.Int("sort_order").Default(0),
		// Optional radar-chart dimensions (max 6) for the group intro card on
		// the public catalog. NULL/empty = the card shows text only.
		field.JSON("profile", []GroupProfileDim{}).Optional(),
	}
}

func (ModelGroup) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("model_links", ModelGroupModel.Type),
		edge.To("provider_links", ModelGroupProvider.Type),
		// account_links is the pool-era routing binding: groups bind upstream
		// accounts directly (provider_links stays for the legacy channel rows).
		edge.To("account_links", GroupAccount.Type),
		edge.To("user_grants", ModelGroupUserGrant.Type),
		edge.To("user_group_grants", ModelGroupUserGroupGrant.Type),
	}
}
