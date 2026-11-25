package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PolicyKeyword is one entry in the moderation dictionary. The pattern is
// matched against request bodies via an Aho-Corasick automaton that
// PolicySvc rebuilds whenever the dictionary changes.
//
// Patterns are stored verbatim — case-folding is applied at scan time so
// the same row matches "API_KEY" and "api_key" without doubling the
// dictionary size.
type PolicyKeyword struct{ ent.Schema }

func (PolicyKeyword) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (PolicyKeyword) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("category_id", uuid.UUID{}),
		field.String("pattern").NotEmpty().Comment("Matched as a literal substring; case-folded at scan time"),
		// match_mode controls how `pattern` is matched on the block path:
		//   - substring: literal Aho-Corasick match anywhere (default)
		//   - word:      AC match that additionally requires word boundaries
		//                on both sides (kills the Scunthorpe problem for short
		//                ASCII words like "ass" inside "class")
		//   - regex:     RE2 pattern, compiled case-insensitively, run over the
		//                normalized text (use sparingly — not in the automaton)
		field.Enum("match_mode").Values("substring", "word", "regex").Default("substring"),
		// severity is normally inherited from the category but can be
		// overridden per-keyword for hot patterns. nil = inherit.
		field.Enum("severity").Values("low", "medium", "high", "critical").Optional().Nillable(),
		field.Bool("enabled").Default(true),
		field.String("note").Default("").Comment("Operator-facing reason this entry exists"),
	}
}

func (PolicyKeyword) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("category", PolicyCategory.Type).
			Ref("keywords").
			Field("category_id").
			Required().
			Unique(),
	}
}

func (PolicyKeyword) Indexes() []ent.Index {
	return []ent.Index{
		// (pattern, category_id) is unique — the same pattern can live in
		// two different categories (different actions / severities), but
		// not twice in the same one.
		index.Fields("pattern", "category_id").Unique(),
		index.Fields("category_id", "enabled"),
	}
}
