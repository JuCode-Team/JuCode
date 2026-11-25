package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type UsageLog struct{ ent.Schema }

func (UsageLog) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UsageLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		field.UUID("api_key_id", uuid.UUID{}).Optional().Nillable(),
		// Set instead of api_key_id when the caller authenticated via the
		// OAuth device flow (CLI / Desktop). Lets usage be attributed to a
		// specific authorized device. Plain analytics column — no FK edge.
		field.UUID("authorization_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("organization_id", uuid.UUID{}).Optional().Nillable(),
		field.String("request_id").Default(""),
		// endpoint identifies which gateway surface served the request.
		// The chat surfaces (chat_completions, responses, messages) are
		// the original streaming path; the rest are the unified non-chat
		// generation endpoints (see internal/llm/endpoints.go). Stored as
		// an ent enum, which Postgres materializes as a plain varchar (no
		// CHECK constraint), so appending values is backward-compatible:
		// old rows keep their value and no column DDL changes.
		field.Enum("endpoint").Values(
			"chat_completions",
			"responses",
			"messages",
			"embeddings",
			"image_generation",
			"image_edit",
			"image_variation",
			"audio_speech",
			"audio_transcription",
			"audio_translation",
			"rerank",
			"video_generation",
			"web_search",
			"web_fetch",
		),
		field.String("model").NotEmpty(),
		field.UUID("group_id", uuid.UUID{}).Optional().Nillable(),
		field.UUID("provider_id", uuid.UUID{}).Optional().Nillable(),
		// provider_account_id identifies which account under the provider
		// served the attempt (set only when the account layer handled the
		// request). Plain analytics column — no FK edge, on purpose: a
		// foreign key here would validate-scan the whole hot table under
		// ACCESS EXCLUSIVE on migration, and authorization_id already set
		// the no-FK precedent for attribution-only columns.
		field.UUID("provider_account_id", uuid.UUID{}).Optional().Nillable(),
		field.String("upstream_model").Default(""),
		// tokens_in: NON-cached, non-cache-write input tokens (the slice priced at input_unit_price).
		// tokens_out: output tokens priced at output_unit_price.
		// cached_tokens_in: cache-hit input tokens. OpenAI Chat: prompt_tokens_details.cached_tokens.
		//                   OpenAI Responses: input_tokens_details.cached_tokens.
		//                   Anthropic: cache_read_input_tokens.
		// cache_creation_tokens_in: cache write input tokens (Anthropic
		//                           cache_creation_input_tokens / GPT-5.6+
		//                           cache_write_tokens). May also be an
		//                           ESTIMATE — see cache_write_estimated.
		field.Int("tokens_in").Default(0),
		field.Int("tokens_out").Default(0),
		field.Int("cached_tokens_in").Default(0),
		field.Int("cache_creation_tokens_in").Default(0),
		// Anthropic TTL split. When both are 0 and cache_creation_tokens_in
		// > 0 the row is an aggregate-only write (older upstream / GPT
		// cache_write_tokens) and billing treats the total as 5m.
		field.Int("cache_creation_5m_tokens_in").Default(0),
		field.Int("cache_creation_1h_tokens_in").Default(0),
		// image_tokens_*: token usage reported by built-in image-generation
		// tools (Responses tool_usage.image_gen). Kept separate from text
		// tokens so models can price image generation independently.
		field.Int("image_tokens_in").Default(0),
		field.Int("image_tokens_out").Default(0),
		// audio_tokens_*: the audio-modality tokens, split out so they can be
		// priced at the audio rate. EXCLUDED from tokens_in / tokens_out —
		// like every other column here those sets are disjoint, so total
		// input = tokens_in + cached + cache_creation + audio_in.
		//
		// Readers therefore have to opt in. Money paths (user billing,
		// provider settlement) and any report with its own audio column keep
		// the modalities apart; a report that exposes a single input/output
		// pair adds audio back into it, because audio lived inside tokens_in
		// before this split and leaving it out makes voice traffic chart at a
		// fraction of its real volume. (The cache columns stay out of those
		// sums either way — they always did.)
		field.Int("audio_tokens_in").Default(0),
		field.Int("audio_tokens_out").Default(0),
		// cache_write_estimated: true when cache_creation_tokens_in was NOT
		// reported by the upstream but derived by the gateway (model has
		// estimate_cache_write enabled): the non-cached input remainder is
		// reclassified from tokens_in into cache_creation_tokens_in, so the
		// row bills those tokens once at the cache-write rate — mirroring
		// Anthropic / OpenAI GPT-5.6+ official semantics where the three
		// token sets are disjoint. Frontends render estimated values with
		// a "~" prefix.
		field.Bool("cache_write_estimated").Default(false),
		// output_images: count of images the upstream returned and we
		// surfaced to the client. Stays 0 for pure-text responses and
		// also stays 0 when SiteSettings.AllowImageOutput=false (images
		// are substituted with a notice before they reach the client).
		field.Int("output_images").Default(0),
		field.Other("cost_base", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Other("cost_final", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		// rate_multiplier_applied = the *effective* multiplier used to
		// derive cost_final from cost_base, INCLUDING any dynamic
		// pool-throttle factor stacked on top of group.RateMultiplier.
		// Stored per-row so audits don't have to reconstruct it from
		// the (volatile) group setting at the time of the request.
		// Default 1.0 keeps historical rows correct after the column
		// is added (ent ALTER fills with default).
		field.Float("rate_multiplier_applied").Default(1.0),
		field.String("currency").Default("CNY"),
		field.Enum("billing_source").Values("plan", "plan_balance", "balance", "organization_balance", "none").Default("none"),
		field.UUID("user_plan_id", uuid.UUID{}).Optional().Nillable(),
		field.Enum("status").Values("ok", "upstream_error", "quota_exceeded", "banned", "internal_error", "unauthorized"),
		field.Int("http_status").Default(0),
		// latency_ms: elapsed time from the gateway accepting the request to
		// the client holding the complete response. Measured before charging
		// and usage persistence, so the billing settle is not counted against
		// the request the caller experienced.
		field.Int("latency_ms").Default(0),
		// ttft_ms: time-to-first-token in milliseconds. Captured at the SSE
		// pipe layer when the first semantic model output lands: visible text,
		// reasoning/thinking, or tool-call deltas. Null when the gateway never
		// streamed the upstream response and there is therefore no first-token
		// time to measure. That is NOT the same as stream=false: the Anthropic
		// translation bridge streams upstream and buffers a single JSON body
		// for the client, so it writes a real ttft_ms on a stream=false row.
		// (/v1/responses also buffers, but forces stream=true into the
		// forwarded body and records that, so its rows read stream=true.)
		// avg_tps divides by latency_ms - ttft_ms and skips null rows.
		field.Int("ttft_ms").Optional().Nillable(),
		// ttfb_ms = time-to-first-byte: elapsed time until the gateway
		// first writes or flushes the downstream response. Captured
		// separately from ttft so response-start and first-token time
		// can be told apart in the audit log.
		field.Int("ttfb_ms").Optional().Nillable(),
		field.Int("retry_count").Default(0),
		field.JSON("provider_chain", []string{}).Default([]string{}),
		// latency_detail is admin-only diagnostic timing data for the
		// request hot path. It is intentionally omitted from user-facing
		// usage APIs so customers only see coarse latency fields.
		field.JSON("latency_detail", map[string]int{}).Optional(),
		// error_message is a short error with any upstream-written text
		// reduced by biz.SanitizeErrorMessage.
		field.String("error_message").Default(""),
		// error_class is the structured classification of the upstream
		// result (ok / provider_failure / rate_limit / timeout / auth /
		// user_request / user_policy / cyber_block / unknown). It drives
		// retry, health and safety decisions and is admin-only.
		field.String("error_class").Default("").MaxLen(32),
		// upstream_request_id is the provider's own request id, harvested
		// from response headers (x-request-id / cf-ray / x-codex-* /
		// openai-*). Admin-only; lets operators reconcile with upstream.
		field.String("upstream_request_id").Default("").MaxLen(256),
		// upstream_raw_error is a redacted summary of the upstream error
		// (type, code, parameter path, known failure phrases; see
		// biz.SanitizeUpstreamError), never the body itself. Admin-only;
		// users see the reason resolved from it.
		field.String("upstream_raw_error").Default("").
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		// Client context.
		field.String("ip").Default("").MaxLen(45),
		field.String("user_agent").Default("").MaxLen(512),
		// reasoning_effort = OpenAI Responses API field, sometimes
		// surfaced on Chat too. low | medium | high | auto | "" when
		// not provided. Anthropic equivalent (thinking.budget_tokens)
		// is normalized into the same column when present.
		field.String("reasoning_effort").Default("").MaxLen(20),
		// stream = whether the response was streamed to the client.
		field.Bool("stream").Default(false),
		// api_key_name is the snapshot of the API key's display name
		// at request time, so the audit log doesn't break when the
		// key is renamed or revoked later.
		field.String("api_key_name").Default("").MaxLen(128),
	}
}

func (UsageLog) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("usage_logs").
			Field("user_id").
			Required().
			Unique(),
		edge.From("api_key", ApiKey.Type).
			Ref("usage_logs").
			Field("api_key_id").
			Unique(),
		edge.From("organization", Organization.Type).
			Ref("usage_logs").
			Field("organization_id").
			Unique(),
		edge.From("provider", Provider.Type).
			Ref("usage_logs").
			Field("provider_id").
			Unique(),
	}
}

// Indexes: usage_logs is by far the largest table and every INSERT sits
// on the LLM response path, so each index here is real write
// amplification. Six former (dim, created_at) indexes — authorization_id,
// organization_id, provider_id, model, status, and (provider_id,
// error_class) — showed idx_scan≈0 in production (their queries are
// served by the created_at range index) and were dropped on 2026-07-27.
// Don't re-add a dimension index without checking pg_stat_user_indexes
// first.
//
// (organization_id, created_at) was re-added when the console gained the
// org account-context switch: the org-manager view aggregates
// `WHERE organization_id = $1 AND created_at >= .. < ..` and nothing else
// can serve it. It is partial (organization_id IS NOT NULL) so the
// personal-traffic majority of inserts pays no write amplification.
// Deploy note: create it with CREATE INDEX CONCURRENTLY before running
// jucode-migrate — the migrator's plain CREATE INDEX would block writes
// on a table that sits on the gateway response path.
func (UsageLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		index.Fields("user_id", "created_at"),
		index.Fields("api_key_id", "created_at"),
		index.Fields("group_id", "created_at"),
		index.Fields("organization_id", "created_at").
			Annotations(entsql.IndexWhere("organization_id IS NOT NULL")),
		// (authorization_id, created_at) was dropped on 2026-07-27 for
		// idx_scan≈0 and is back because /v1/open/logs?scope=self now
		// serves it: a third-party OAuth app needs its
		// own consumption separated from the user's CLI/API-key traffic,
		// and nothing else can answer
		// `WHERE user_id = $1 AND authorization_id = $2 ORDER BY created_at`.
		// Partial like the organization one — only OAuth calls set the
		// column, so the api-key majority of inserts pays nothing.
		// Deploy note: CREATE INDEX CONCURRENTLY before jucode-migrate;
		// the migrator's plain CREATE INDEX would block writes on a table
		// that sits on the gateway response path.
		index.Fields("authorization_id", "created_at").
			Annotations(entsql.IndexWhere("authorization_id IS NOT NULL")),
		index.Fields("user_plan_id", "billing_source", "status", "created_at"),
		// Log-browser filters (admin /usage-logs, user /me/usage-logs).
		// (status) and (model) single-column indexes were dropped on
		// 2026-07-27 for idx_scan≈0; these are different: equality column
		// first, created_at second, so `WHERE status = $1 AND created_at
		// >= $2 ORDER BY created_at DESC LIMIT 50` is one backward index
		// scan and the capped count is an index-only scan. Measured on
		// 500k synthetic rows: status page 1.83ms→0.15ms, status count
		// 21ms→0.16ms, model= page 1.35ms→0.14ms. error_class is partial
		// (ok rows have '') so the 80% success path pays no write cost;
		// request_id is partial and single-column because the browser
		// matches it exactly (it is pasted whole), never by range.
		// Deploy note: CREATE INDEX CONCURRENTLY each one before running
		// jucode-migrate — the migrator's plain CREATE INDEX would block
		// writes on a table that sits on the gateway response path.
		// The partial predicates are spelled the way pg_get_expr prints
		// them ("(col)::text <> ''::text"); with the bare "col <> ''" the
		// migrator sees a different predicate and DROPs + re-CREATEs the
		// index (non-concurrently) on every run, as it already does for
		// the same-shaped indexes on availability_observations and
		// site_orders. Verify with client.Schema.WriteTo before deploy.
		// Re-check pg_stat_user_indexes.idx_scan two weeks after rollout.
		index.Fields("status", "created_at"),
		index.Fields("model", "created_at"),
		index.Fields("error_class", "created_at").
			Annotations(entsql.IndexWhere("(error_class)::text <> ''::text")),
		index.Fields("request_id").
			Annotations(entsql.IndexWhere("(request_id)::text <> ''::text")),
	}
}
