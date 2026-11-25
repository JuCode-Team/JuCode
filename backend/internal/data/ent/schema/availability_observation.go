package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AvailabilityObservation is the public status page's source of truth.
//
// It stores exactly one final, group-attributed outcome for a business
// request. Provider attempts and probes intentionally live elsewhere: retries
// must not inflate the denominator, and synthetic traffic must never make an
// idle service look healthy.
type AvailabilityObservation struct{ ent.Schema }

func (AvailabilityObservation) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (AvailabilityObservation) Fields() []ent.Field {
	return []ent.Field{
		field.String("request_id").Default("").MaxLen(128),
		field.UUID("group_id", uuid.UUID{}),
		// Stable logical model identity. The display name is kept alongside it
		// so historical rows remain readable if an administrator renames a
		// model.
		field.UUID("model_id", uuid.UUID{}).Optional().Nillable(),
		field.String("model").Default("").MaxLen(256),
		field.String("endpoint").Default("").MaxLen(64),
		field.Enum("outcome").Values("good", "bad", "excluded"),
		field.String("failure_class").Default("").MaxLen(64),
		field.Int("http_status").Default(0),
		field.Int("latency_ms").Default(0),
		field.Int("ttft_ms").Optional().Nillable(),
		field.Time("started_at").Default(time.Now).Immutable(),
		field.Enum("traffic_class").Values("business", "internal", "probe").Default("business"),
	}
}

func (AvailabilityObservation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("group_id", "started_at"),
		index.Fields("started_at"),
		// Request IDs are present on public gateway requests. Keeping the
		// empty legacy value outside this constraint avoids unrelated blank-ID
		// observations colliding while still enforcing one final observation
		// per real request and group.
		index.Fields("request_id", "group_id").
			Unique().
			Annotations(entsql.IndexWhere("request_id <> ''")),
	}
}
