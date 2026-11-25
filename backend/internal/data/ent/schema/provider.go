package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Provider struct{ ent.Schema }

func (Provider) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (Provider) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.Enum("kind").Values("openai_compatible", "anthropic"),
		field.String("base_url").NotEmpty(),
		// protocols lists the client-facing wire protocols AND the optional
		// upstream chat dialect declaration at base_url:
		//   "openai"      — OpenAI-family (chat + responses, match client)
		//   "compatible"  — Chat Completions only (conversion target)
		//   "responses"   — Responses API only (conversion target)
		//   "anthropic"   — Anthropic Messages
		// Empty = derive from kind (openai_compatible → openai,
		// anthropic → anthropic).
		field.Strings("protocols").Optional(),
		// api_format selects the upstream dialect adaptor used for the
		// NON-CHAT generation endpoints (embeddings / image / audio / video /
		// rerank). It is orthogonal to `kind` (which drives the chat path):
		// most upstreams are kind=openai_compatible but speak different
		// generation dialects. Empty = infer from base_url host, falling back
		// to the OpenAI-compatible passthrough. Known values:
		//   "openai"      — OpenAI-compatible passthrough (default)
		//   "dashscope"   — Alibaba Bailian native (async wanx tasks)
		//   "volcengine"  — Volcengine Ark (async contents/generations tasks)
		//   "zhipu"       — Zhipu GLM (async cogvideox)
		//   "siliconflow" — SiliconFlow (async video submit/status)
		//   "minimax"     — MiniMax (3-stage async video)
		//   "gemini"      — Google Gemini/Veo (operations poll)
		//   "kling"       — Kling 可灵 (text2video/image2video async)
		//   "vidu"        — Vidu 生数 ("Token" auth, tasks/creations poll)
		//   "jimeng"      — Jimeng 即梦 (Volcengine Visual async task)
		// Stored as a string (not an enum) so adaptors can be added without a
		// schema enum migration.
		field.String("api_format").Default(""),
		// AES-GCM ciphertext (base64). Decrypted at use, never logged.
		// Default credential for every selected protocol that has no
		// override. Claude-only upstreams store their key here too —
		// this is not an OpenAI-specific field.
		field.String("api_key_enc").Optional().Sensitive(),
		// Optional Anthropic-protocol override. Empty = use api_key_enc.
		field.String("anthropic_api_key_enc").Optional().Sensitive(),
		// Only for kind=openai_compatible.
		field.String("organization").Optional().Nillable(),
		field.Enum("health").Values("healthy", "degraded", "unhealthy", "unknown").Default("unknown"),
		field.Time("last_health_at").Optional().Nillable(),
		field.Int("weight").Default(100),
		field.Bool("enabled").Default(true),
		// Soft-delete marker, intentionally distinct from `enabled`:
		//   enabled=false  → operationally paused, still a live provider
		//   deleted_at set → retired/archived; hidden from management and
		//                    routing, but the row (and its cost/settlement/
		//                    usage history) is kept so financial reports and
		//                    reconciliation stay intact. Never hard-deleted
		//                    once it has carried traffic.
		field.Time("deleted_at").Optional().Nillable(),
		// When enabled, strip OpenAI Responses image-generation tool
		// definitions from outbound requests before they reach this
		// provider. This blocks image tool invocation without affecting
		// ordinary image inputs or the separate image-output gate.
		field.Bool("block_image_generation_tool").Default(false),
		// estimate_cache_write: this provider does NOT report a cache
		// write count in usage (no cache_write_tokens /
		// cache_creation_input_tokens), so the gateway derives it: the
		// non-cached input remainder is RECLASSIFIED from tokens_in into
		// cache_creation_tokens_in (billed once at cache_write_input_price
		// instead of input_unit_price — never both, matching Anthropic and
		// OpenAI GPT-5.6+ official disjoint-set semantics). A provider-
		// reported value always wins; estimation only fills the gap.
		// Per-provider because reporting behavior is an upstream trait —
		// the same model served through another provider may report it.
		field.Bool("estimate_cache_write").Default(false),
		// cache_write_min_context_tokens: estimation guard. Upstreams only
		// cache prompts at or above a minimum length (OpenAI: 1024 tokens;
		// Anthropic: 1024/2048/4096 by model), so a shorter request writes
		// NOTHING to cache. When the input-side context total is below this
		// threshold, ApplyCacheWriteEstimate leaves the tokens as plain
		// input rather than over-charging them at the cache-write rate.
		// Only consulted when estimate_cache_write is on. Default 1024.
		field.Int("cache_write_min_context_tokens").Default(1024),
		field.String("note").Default(""),
	}
}

func (Provider) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("models", ProviderModel.Type),
		edge.To("manual_costs", ProviderCost.Type),
		edge.To("group_links", ModelGroupProvider.Type),
		edge.To("usage_logs", UsageLog.Type),
		edge.To("settlements", ProviderSettlement.Type),
	}
}

func (Provider) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("kind", "enabled", "health"),
	}
}
