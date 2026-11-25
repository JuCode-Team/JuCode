package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ModelGroupModel attaches a Model to a ModelGroup. Many-to-many: a model
// can live in multiple groups (e.g. a coding-plan group and a pay-per-use
// group), and a group can contain many models.
type ModelGroupModel struct{ ent.Schema }

func (ModelGroupModel) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ModelGroupModel) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("group_id", uuid.UUID{}),
		field.UUID("model_id", uuid.UUID{}),
		// Synthetic availability probing is per-link, not per-model: the same
		// model reached through two groups rides different providers, so their
		// health is independent and each link needs its own switch and cadence.
		// Off by default — every enabled link costs real upstream tokens on a
		// fixed schedule whether or not anyone is using the model.
		field.Bool("probe_enabled").Default(false),
		// Kept below statusStateWindow so each state window contains at least
		// one probe; see biz.statusStateWindow.
		field.Int("probe_interval_seconds").Default(900),
		// Reasoning models ignore max_tokens=1 for their thinking block, which
		// is where a probe's cost actually goes — measured at 129 output tokens
		// on glm-5.2 against 1 on claude. The vendor switches that turn it off
		// are per-vendor and not universally tolerated: xAI rejects them with
		// 400, which would paint the channel red on the very page this feeds.
		// So it is opt-in per link, enabled only where it has been measured to
		// both work and pay for itself.
		field.Bool("probe_disable_thinking").Default(false),
		// The model's context window through this group (its channel may give
		// a different one than other groups). Between the model default and an
		// account binding's own override; nil inherits the model default.
		field.Int("context_window").Optional().Nillable(),
	}
}

func (ModelGroupModel) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("group", ModelGroup.Type).
			Ref("model_links").
			Field("group_id").
			Required().
			Unique(),
		edge.From("model", Model.Type).
			Ref("group_links").
			Field("model_id").
			Required().
			Unique(),
	}
}

func (ModelGroupModel) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "model_id").Unique(),
		index.Fields("model_id"),
	}
}
