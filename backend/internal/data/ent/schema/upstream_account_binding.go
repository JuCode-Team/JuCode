package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UpstreamAccountBinding pins a scope (a conversation, a user, an API key, or
// an organization) to one account. Bindings are configuration written by
// admins only — no request input may select or override an account, and
// nothing about accounts is ever returned to gateway clients.
type UpstreamAccountBinding struct{ ent.Schema }

func (UpstreamAccountBinding) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UpstreamAccountBinding) Fields() []ent.Field {
	return []ent.Field{
		// conversation | user | api_key | organization, resolved most-specific
		// first when several bindings could apply.
		field.Enum("scope_type").Values("conversation", "user", "api_key", "organization"),
		// Scope identifier: conversation id (as sent by the client), or the
		// UUID of the user / api key / organization.
		field.String("scope_id").NotEmpty(),
		field.UUID("account_id", uuid.UUID{}),
		// Optional expiry; empty means the binding stays until deleted.
		field.Time("expires_at").Optional().Nillable(),
		// Admin user that created it (audit trail).
		field.UUID("created_by", uuid.UUID{}).Optional().Nillable(),
		field.String("note").Default(""),
	}
}

func (UpstreamAccountBinding) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("account", UpstreamAccount.Type).
			Ref("bindings").
			Field("account_id").
			Unique().
			Required(),
	}
}

func (UpstreamAccountBinding) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("scope_type", "scope_id"),
	}
}
