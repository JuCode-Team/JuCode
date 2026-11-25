package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PolicyCategory (external term: "policy group") groups keywords by the
// kind of content they describe (e.g. "politically sensitive",
// "self-harm", "pii"). The group — not the keyword — controls the
// runtime action (block vs log-only), severity floor, and scan scope
// so an operator can tune a whole class at once.
//
// The schema name is kept as PolicyCategory for ent code-gen stability
// (renaming a table is more migration risk than it's worth); the
// admin API / UI / docs all use "group" as the user-facing term.
type PolicyCategory struct{ ent.Schema }

func (PolicyCategory) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (PolicyCategory) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.String("slug").NotEmpty().Comment("URL/CSS-safe identifier, e.g. politics, pii"),
		field.String("description").Default(""),
		// default_action is what happens when a keyword in this group
		// fires. "block" rejects the request before it hits a provider;
		// "log_only" records an incident but lets traffic through (useful
		// for staging a new group before turning it on).
		field.Enum("default_action").Values("block", "log_only").Default("block"),
		field.Enum("default_severity").Values("low", "medium", "high", "critical").Default("medium"),
		field.Bool("enabled").Default(true),
		field.Int("sort_order").Default(0),
		// scope determines where the scanner runs for this group's
		// keywords:
		//   input_only            scan inbound request body only (cheapest, default)
		//   input_output          scan inbound + assistant text outbound (skips tool calls)
		//   input_output_tools    scan inbound + outbound text + tool_use.input + tool_result.content
		// Backwards-compatible default keeps existing groups in input-only mode.
		field.Enum("scope").
			Values("input_only", "input_output", "input_output_tools").
			Default("input_only"),
	}
}

func (PolicyCategory) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("keywords", PolicyKeyword.Type),
	}
}

func (PolicyCategory) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug").Unique(),
		index.Fields("name").Unique(),
	}
}
