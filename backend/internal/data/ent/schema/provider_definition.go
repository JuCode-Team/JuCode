package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ProviderDefinition is the per-vendor definition an account is created from:
// the endpoint shape, the credential it asks for, and which models exist.
//
// Two sources feed this table:
//
//   - built-in definitions ship in code (biz/provider_definitions_builtin.go)
//     and are seeded into the table; the code copy stays authoritative until an
//     operator edits one, at which point the row wins.
//   - operator-authored rows for vendors that are not worth shipping.
type ProviderDefinition struct{ ent.Schema }

func (ProviderDefinition) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ProviderDefinition) Fields() []ent.Field {
	return []ent.Field{
		// Stable, human-readable id ("azure-openai"): accounts reference it, so it
		// must stay stable across edits.
		field.String("definition_id").NotEmpty(),
		field.String("name").Default(""),
		field.String("vendor").Default(""),
		// --- endpoint shape the account inherits ---
		field.String("base_url").Default(""),
		field.String("api_format").Default(""),
		field.Strings("native_protocols"),
		field.String("default_protocol").Default(""),
		field.Bool("allow_conversion").Default(false),
		field.JSON("endpoint_paths", map[string]any{}).Optional(),
		// Header rewrites accounts created from this definition start with.
		field.JSON("header_rules", HeaderRules{}).Optional(),
		// --- payloads ---
		// models_json: [{"name": "...", "protocol": "chat_completions"}]
		field.String("models_json").Default(""),
		// models_path overrides the upstream model-list endpoint used by
		// account model sync ("" = the /v1/models convention).
		field.String("models_path").Default(""),
		// credential_label / credential_hint name the secret this vendor wants
		// ("Subscription Token" rather than a generic "API Key"), so the console
		// asks for the right thing instead of implying every vendor takes a key.
		field.String("credential_label").Default(""),
		field.String("credential_hint").Default(""),
		// --- lifecycle ---
		field.Bool("enabled").Default(true),
		// builtin marks rows seeded from code, so the UI can offer "reset to
		// shipped" and so a seed run never clobbers an operator's edit.
		field.Bool("builtin").Default(false),
		field.Int("version").Default(1),
		field.String("note").Default(""),
	}
}

func (ProviderDefinition) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("definition_id").Unique(),
		index.Fields("vendor", "enabled"),
	}
}
