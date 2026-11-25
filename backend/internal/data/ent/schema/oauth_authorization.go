package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// OAuthAuthorization is a single authorized device/session created by the
// first-party OAuth login flow (CLI / Desktop). Unlike an ApiKey, the
// client never holds a long-lived secret: it holds a short-lived JWT
// access token (kind=oauth_access carrying this row's id) plus an opaque
// refresh token whose SHA-256 hash is stored here. Revoking a device sets
// revoked_at, which instantly invalidates both refresh and (within the
// resolve cache TTL) access. This row IS what the frontend shows under
// "授权设备管理".
type OAuthAuthorization struct{ ent.Schema }

func (OAuthAuthorization) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (OAuthAuthorization) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		// A first-party client id from config oauth.clients (e.g. "jucode-cli");
		// no dynamic client registration.
		field.String("client_id").NotEmpty(),
		// Human-facing device label, e.g. "JuCode CLI · hostname (darwin)".
		field.String("name").Default(""),
		// Granted scopes. Kept minimal for first-party login; today every
		// device gets inference + account:read.
		field.JSON("scopes", []string{}).Default([]string{}),
		// SHA-256 hash of the current (rotating) refresh token. Unique so a
		// presented refresh token resolves to exactly one row. Never blanked
		// on revoke (revoked_at gates use) to avoid a unique collision on "".
		field.String("refresh_token_hash").NotEmpty().Unique().Sensitive(),
		field.Time("refresh_expires_at"),
		field.Time("last_used_at").Optional().Nillable(),
		field.String("last_ip").Default("").MaxLen(45),
		field.String("last_user_agent").Default("").MaxLen(512),
		field.Time("revoked_at").Optional().Nillable(),
	}
}

func (OAuthAuthorization) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("oauth_authorizations").
			Field("user_id").
			Required().
			Unique(),
	}
}

func (OAuthAuthorization) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "revoked_at"),
	}
}
