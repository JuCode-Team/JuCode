package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Computer is a desktop daemon reachable through the relay (biz/computer.go):
// one daemon identity key, registered to the account whose OAuth token the
// daemon presented on its latest host connection.
type Computer struct{ ent.Schema }

func (Computer) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (Computer) Fields() []ent.Field {
	return []ent.Field{
		// base64url of the first 16 bytes of SHA-256(identity_pub).
		field.String("host_id").Unique().NotEmpty().MaxLen(22),
		field.UUID("user_id", uuid.UUID{}),
		// The daemon's OAuth device.
		field.UUID("authorization_id", uuid.UUID{}).Optional().Nillable(),
		// Ed25519 identity key and X25519 Noise static key, base64url.
		field.String("identity_pub"),
		field.String("static_pub"),
		// The daemon's hostname until the user renames it (name_custom).
		field.String("name").Default(""),
		field.Bool("name_custom").Default(false),
		// macos / windows / linux.
		field.String("platform").Default(""),
		field.String("version").Default(""),
		field.Time("last_seen_at").Default(time.Now),
	}
}

func (Computer) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id"),
	}
}
