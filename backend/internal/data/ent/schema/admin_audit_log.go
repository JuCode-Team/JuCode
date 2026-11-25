package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type AdminAuditLog struct{ ent.Schema }

func (AdminAuditLog) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AdminAuditLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("actor_user_id", uuid.UUID{}),
		field.String("action").NotEmpty(),
		field.String("target").Default(""),
		field.JSON("payload", map[string]any{}).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Optional(),
		field.String("ip").Default(""),
		field.String("ua").Default(""),
	}
}

func (AdminAuditLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("actor_user_id", "created_at"),
	}
}
