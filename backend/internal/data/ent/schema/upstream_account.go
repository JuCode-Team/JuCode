package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UpstreamAccount is one entry of the account pool: an upstream endpoint plus
// the credential used to call it. It carries everything the gateway needs —
// endpoint, dialect, API key, request shaping, scheduling inputs, health and
// quota state — so business model groups bind accounts directly, with no
// channel layer in between.
//
// Named "upstream account" to keep it apart from the user account service
// (biz.Account); in the admin UI these are simply 账号.
type UpstreamAccount struct{ ent.Schema }

func (UpstreamAccount) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (UpstreamAccount) Fields() []ent.Field {
	return []ent.Field{
		// --- endpoint & dialect ---
		// Admin-facing label (never client-visible).
		field.String("name").Default(""),
		field.String("base_url").NotEmpty(),
		// Generation-endpoint adaptor dialect (openai / dashscope / …).
		field.String("api_format").Default(""),
		// Wire protocols the upstream endpoint itself speaks natively:
		// "chat_completions" | "responses" | "messages". A client request whose
		// protocol is in this set is forwarded unchanged.
		field.Strings("native_protocols"),
		// Conversion target when the client protocol is not native ("" = derive
		// from native_protocols).
		field.String("default_protocol").Default(""),
		// Whether this account may serve a client format it does not speak
		// natively, via protocol conversion (still gated by the site switch
		// format_conversion_enabled).
		field.Bool("allow_conversion").Default(false),
		// Only for OpenAI-compatible dialects.
		field.String("organization").Optional().Nillable(),
		// Per-protocol upstream path overrides, e.g.
		// {"chat_completions": "/v2/chat/completions"}. Vendors that do not live
		// under the /v1/* conventions (custom providers) declare their own path
		// here; empty means "use the gateway's dialect default", so rows that
		// never set it behave exactly as before.
		field.JSON("endpoint_paths", map[string]any{}).Optional(),
		// Upstream request header rewrites (see HeaderRules); empty means the
		// gateway's headers go out unchanged.
		field.JSON("header_rules", HeaderRules{}).Optional(),
		// --- credential ---
		// AES-GCM ciphertext (base64) of the providerkit credential JSON (the
		// upstream API key).
		field.String("credential_enc").Optional().Sensitive(),
		// --- request shaping ---
		field.Bool("block_image_generation_tool").Default(false),
		field.Bool("estimate_cache_write").Default(false),
		field.Int("cache_write_min_context_tokens").Default(1024),
		// --- scheduling inputs ---
		field.Enum("status").Values("active", "disabled", "expired", "error").Default("active"),
		// schedulable is the runtime pause switch, independent of status: an
		// operator or the quota auto-pause can park an account without touching
		// its lifecycle state. Time-based cooldowns live in the *_until fields.
		field.Bool("schedulable").Default(true),
		field.Enum("health").Values("healthy", "degraded", "unhealthy", "unknown").Default("unknown"),
		field.Int("priority").Default(100),
		field.Int("weight").Default(100),
		// Max in-flight requests for this account; 0 = unlimited.
		field.Int("max_concurrency").Default(0),
		// pool_mode overrides the pool's cooldown/disable policy for this
		// account: "" = inherit (pool, else site default), otherwise one of
		// balanced | conservative | lenient | observe. Stored as a string so
		// new policies can be added without an enum migration; validated at
		// the admin boundary.
		field.String("pool_mode").Default(""),
		// --- health state (cooldown / disable) ---
		field.Time("rate_limit_reset_at").Optional().Nillable(),
		field.Time("overload_until").Optional().Nillable(),
		field.Time("temp_unschedulable_until").Optional().Nillable(),
		field.String("temp_unschedulable_reason").Default(""),
		field.String("last_error_class").Default(""),
		field.Time("last_error_at").Optional().Nillable(),
		field.Int("consecutive_failures").Default(0),
		// model_cooldowns: {"<model>": {"until_ms": <int>, "reason": "<class>"}} —
		// a model-scoped cooldown that must not park the whole account.
		field.JSON("model_cooldowns", map[string]any{}).Optional(),
		// --- quota ---
		// quota_snapshot: last observed windows/limits, e.g.
		// {"windows":[{"name":"5h","used_percent":82,"reset_at":...}],"source":"passive"}.
		field.JSON("quota_snapshot", map[string]any{}).Optional(),
		field.Time("quota_checked_at").Optional().Nillable(),
		// quota_policy: per-account overrides for auto-pause thresholds.
		field.JSON("quota_policy", map[string]any{}).Optional(),
		// --- lifecycle / identity ---
		field.Time("last_used_at").Optional().Nillable(),
		field.String("last_error").Default(""),
		// legacy_provider_id links a migrated account to the channel row it came
		// from, so historic usage and finance stay traceable.
		field.UUID("legacy_provider_id", uuid.UUID{}).Optional().Nillable(),
		// definition_id names the provider definition this account was created
		// from ("azure-openai", …). Empty means a hand-configured account. The
		// definition supplies prefill and the quota probe.
		field.String("definition_id").Default(""),
		field.String("note").Default(""),
		field.Time("deleted_at").Optional().Nillable(),
	}
}

func (UpstreamAccount) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("models", UpstreamAccountModel.Type),
		edge.To("group_links", GroupAccount.Type),
		edge.To("manual_costs", UpstreamAccountCost.Type),
		edge.To("settlements", UpstreamAccountSettlement.Type),
		edge.To("pool_members", AccountPoolMember.Type),
		edge.To("bindings", UpstreamAccountBinding.Type),
	}
}

func (UpstreamAccount) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "schedulable", "health"),
		index.Fields("priority"),
		index.Fields("rate_limit_reset_at"),
		index.Fields("temp_unschedulable_until"),
		index.Fields("definition_id"),
	}
}
