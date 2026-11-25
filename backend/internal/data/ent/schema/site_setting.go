package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// SiteSetting is a tiny key/value store for site-level configuration
// (display name, ICP number, contact email, etc). KV form so adding a
// new field never requires a migration — admin UI controls the schema
// at the application layer.
type SiteSetting struct{ ent.Schema }

func (SiteSetting) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (SiteSetting) Fields() []ent.Field {
	return []ent.Field{
		field.String("key").NotEmpty().Unique(),
		field.String("value").Default(""),
	}
}
