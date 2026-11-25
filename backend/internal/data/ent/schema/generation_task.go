package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// GenerationTask is a long-running async generation job (today: video;
// extensible to async image / audio transcription). It backs the OpenAI-Sora-
// style client surface (POST /v1/videos -> job id, GET /v1/videos/{id} ->
// poll). The gateway submits the job to the upstream, stores the upstream's
// task id here, and on each client poll proxy-polls the upstream, updating
// status and settling billing when the job first reaches a terminal state.
// Neither the request nor the generated result is stored: a finished job's
// video URL is read from the upstream again on every poll.
//
// It is a brand-new, standalone table with no edges into existing entities
// (provider_id / user_id are plain analytics columns, matching usage_log's
// convention), so it cannot affect any existing query or migration.
type GenerationTask struct{ ent.Schema }

func (GenerationTask) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (GenerationTask) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		field.UUID("api_key_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("organization_id", uuid.UUID{}).Optional().Nillable(),
		field.String("request_id").Default(""),
		// endpoint kind this task serves (video_generation, ...). String, not
		// enum, so new async kinds don't need a schema enum migration.
		field.String("endpoint").Default("video_generation"),
		// Routing snapshot: the logical model, the group it routed through,
		// the provider that accepted the job and the upstream model name.
		field.String("model").NotEmpty(),
		field.UUID("group_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("provider_id", uuid.UUID{}).Optional().Nillable(),
		field.String("upstream_model").Default(""),
		// api_format of the provider (dashscope / volcengine / ...), captured
		// at submit time so polling uses the right adaptor even if the
		// provider's config later changes.
		field.String("provider_format").Default(""),
		// The upstream's own task/job identifier, used to poll it.
		field.String("upstream_task_id").Default(""),
		// Lifecycle: queued -> running -> {succeeded, failed}. queued/running
		// are non-terminal; the poll loop advances them.
		field.Enum("status").
			Values("queued", "running", "succeeded", "failed").
			Default("queued"),
		// Billing signal captured from the terminal upstream response.
		field.Int("billed_seconds").Default(0),
		field.Bool("billed").Default(false),
		field.Other("cost_final", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// error_message is the upstream failure code, or a fixed label when
		// the upstream answered with free text (biz.SanitizeTaskError).
		field.String("error_message").Default(""),
		field.Time("completed_at").Optional().Nillable(),
	}
}

func (GenerationTask) Indexes() []ent.Index {
	return []ent.Index{
		// Client polls by (user, id); the id PK covers lookup, this speeds
		// "list my tasks" and reconciliation scans over non-terminal rows.
		index.Fields("user_id", "status"),
		index.Fields("status", "updated_at"),
	}
}
