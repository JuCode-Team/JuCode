package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// ModelVendor is the brand/vendor a Model belongs to — OpenAI, Anthropic,
// Google, xAI, etc. Public-facing only: powers the "filter by vendor" sidebar
// on /models. Independent from Provider (which is an HTTP upstream / access
// point) — one vendor can be served by many providers and vice versa.
type ModelVendor struct{ ent.Schema }

func (ModelVendor) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ModelVendor) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().Unique(),
		// URL-safe identifier used in the public API (?vendor=openai). Defaults
		// to the lowercased name with non-[a-z0-9] runs collapsed to '-'.
		field.String("slug").NotEmpty().Unique(),
		field.String("description").Default(""),
		// Lower sort_order shows first in the vendor sidebar.
		field.Int("sort_order").Default(0),
	}
}

func (ModelVendor) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("models", Model.Type),
	}
}
