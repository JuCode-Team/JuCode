package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// SystemIncident persists the lifecycle of an SLA degradation for one
// status-page model group. model is reserved for finer-grained incidents.
type SystemIncident struct{ ent.Schema }

func (SystemIncident) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (SystemIncident) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("group_id", uuid.UUID{}),
		field.String("group_name").MaxLen(256),
		field.String("model").Default(""),
		field.Enum("severity").Values("degraded", "serious", "down"),
		field.Enum("worst_severity").Values("degraded", "serious", "down"),
		field.Time("started_at"),
		field.Time("detected_at"),
		field.Time("resolved_at").Optional().Nillable(),
		field.Float("open_availability_pct").Optional().Nillable(),
		field.Float("worst_availability_pct").Optional().Nillable(),
		field.Float("last_availability_pct").Optional().Nillable(),
		field.Int("sample_count").Default(0),
		field.String("resolution").Default(""),
		field.String("note").Default("").MaxLen(2000),
	}
}

func (SystemIncident) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "model").
			Unique().
			Annotations(entsql.IndexWhere("resolved_at IS NULL")),
		index.Fields("group_id", "started_at"),
		index.Fields("started_at"),
	}
}
