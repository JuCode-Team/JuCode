package schema

import (
	"encoding/json"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ClientSetting is a per-user key/value store the JuCode Desktop syncs a
// few basic settings through. Values are opaque JSON owned by the client.
type ClientSetting struct{ ent.Schema }

func (ClientSetting) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ClientSetting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		field.String("key").MaxLen(64),
		field.JSON("value", json.RawMessage{}),
	}
}

func (ClientSetting) Indexes() []ent.Index {
	return []ent.Index{index.Fields("user_id", "key").Unique()}
}
