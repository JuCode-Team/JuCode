package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PolicyAllowPhrase is an allow-list entry: a phrase that legitimately
// contains a policy keyword (e.g. "class action" containing "ass", or a
// brand/library name that embeds a flagged token). When a keyword hit falls
// entirely inside an allow-phrase span, the hit is suppressed — the operator
// escape hatch for the substring-match false positives that hurt precision.
type PolicyAllowPhrase struct{ ent.Schema }

func (PolicyAllowPhrase) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (PolicyAllowPhrase) Fields() []ent.Field {
	return []ent.Field{
		field.String("phrase").NotEmpty().Unique().
			Comment("Matched case-folded/normalized; suppresses keyword hits within its span"),
		field.Bool("enabled").Default(true),
		field.String("note").Default("").Comment("Operator-facing reason this entry exists"),
	}
}

func (PolicyAllowPhrase) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("enabled"),
	}
}
