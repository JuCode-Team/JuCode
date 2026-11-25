package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// PriceTier is one context-length band of a model's tiered pricing.
// Tiers apply to per_token billing only: the band is selected by the
// request's total input-side tokens (non-cached + cache-read +
// cache-write), and any price left nil inside the band falls back to
// the model's base column of the same name. UpToTokens is the band's
// inclusive upper bound; 0 means "no upper bound" (the last band).
type PriceTier struct {
	UpToTokens             int64            `json:"up_to_tokens"`
	InputUnitPrice         *decimal.Decimal `json:"input_unit_price,omitempty"`
	OutputUnitPrice        *decimal.Decimal `json:"output_unit_price,omitempty"`
	CacheReadInputPrice    *decimal.Decimal `json:"cache_read_input_price,omitempty"`
	CacheWriteInputPrice   *decimal.Decimal `json:"cache_write_input_price,omitempty"`
	CacheWrite1hInputPrice *decimal.Decimal `json:"cache_write_1h_input_price,omitempty"`
}

// Model is the logical model the user requests (the "model" string in the
// OpenAI request body). It carries pricing; ModelGroup carries access
// policy and rate multiplier.
type Model struct{ ent.Schema }

func (Model) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (Model) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().Unique(),
		field.Enum("billing_mode").Values("per_request", "per_token"),
		// per_request: cost = price_per_request
		field.Other("price_per_request", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// per_token: cost =
		//   non_cached_input * input_unit_price/1000 +
		//   cached_input    * cache_read_input_price/1000 +     (OpenAI prompt_tokens_details.cached_tokens / Anthropic cache_read_input_tokens)
		//   cache_write_5m  * cache_write_input_price/1000 +     (Anthropic cache_creation.ephemeral_5m / aggregate cache_creation_input_tokens / GPT cache_write_tokens)
		//   cache_write_1h  * cache_write_1h_input_price/1000 +  (Anthropic cache_creation.ephemeral_1h_input_tokens)
		//   output          * output_unit_price/1000
		// If cache_*_price is nil, falls back to input_unit_price (i.e. no discount/markup for cached tokens).
		// cache_write_input_price is the 5-minute TTL write rate (~1.25× input).
		// cache_write_1h_input_price is the 1-hour TTL write rate (~2× input);
		// nil falls back to cache_write_input_price.
		field.Other("input_unit_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("output_unit_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("cache_read_input_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("cache_write_input_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("cache_write_1h_input_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// image_*_unit_price: token prices for built-in image-generation
		// tool usage reported as tool_usage.image_gen. Nil falls back to
		// the corresponding text input/output price for backward-compatible
		// billing; set these when the upstream publishes separate image
		// token rates.
		field.Other("image_input_unit_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("image_output_unit_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// audio_*_unit_price: token prices for the audio modality, reported
		// as prompt/completion_tokens_details.audio_tokens (Chat) and
		// input/output_token_details.audio_tokens (Realtime). Upstreams charge
		// audio tokens several times the text rate, so a Realtime voice
		// session priced at the text rate under-bills badly. Nil falls back
		// to the corresponding text input/output price, which keeps existing
		// rows behaving exactly as before.
		field.Other("audio_input_unit_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		field.Other("audio_output_unit_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// output_image_price: flat charge per image emitted by the
		// model on responses / chat-completions multimodal output.
		// Stacks on top of per_token billing — text + image pricing
		// are computed independently and summed. nil = "the model
		// can't return images" (default; the gateway also gates on
		// SiteSettings.AllowImageOutput before letting an image
		// reach the client). Denominated in points (1 point = 1 CNY),
		// like every other model price.
		field.Other("output_image_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// output_second_price: flat charge per second of generated
		// audio/video, for the non-chat generation endpoints (video, TTS).
		// Stacks on the base mode like output_image_price. nil = not metered
		// by duration (falls back to the base per_request/per_token cost).
		// Points (1 point = 1 CNY).
		field.Other("output_second_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// input_character_price: flat charge per input character, for TTS
		// (audio_speech). Stacks on the base mode. nil = not metered by
		// characters. Points (1 point = 1 CNY).
		field.Other("input_character_price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Optional().
			Nillable(),
		// price_tiers: optional context-length tiered pricing for
		// per_token mode. Ordered by up_to_tokens ascending with the
		// final tier's up_to_tokens = 0 (unbounded). Empty/absent =
		// flat pricing from the base columns above.
		field.JSON("price_tiers", []PriceTier{}).Optional(),
		// currency: legacy column kept for schema compatibility. Billing
		// does NOT read it — model prices are points (1 point = 1 CNY)
		// and no exchange rate is applied on user-facing paths.
		field.String("currency").Default("CNY"),
		field.Bool("enabled").Default(true),
		field.String("description").Default(""),
		// Optional vendor (brand). Drives the "filter by vendor" sidebar on
		// the public /models page; never load-bearing for routing.
		field.UUID("vendor_id", uuid.UUID{}).Optional().Nillable(),
		// Lower sort_order shows first in the admin model list and on /models.
		// Ties fall back to name, so a fresh install (all zeroes) keeps the
		// alphabetical order it had before this field existed.
		field.Int("sort_order").Default(0),
		// Capability metadata advertised on /v1/models and /api/pricing.
		// Nil means "not configured": the gateway omits the field rather than
		// guessing, so clients never budget against a made-up number.
		// context_window is the default for every account serving the model;
		// an account binding may override it (upstream_account_models).
		field.Int("context_window").Optional().Nillable(),
		field.Int("max_output_tokens").Optional().Nillable(),
		field.Strings("reasoning_efforts").Optional(),
		// What clients show for the model (e.g. "GPT-6.1 Sol"); `name` stays
		// the id requests use. Nil: clients show the id.
		field.String("display_name").Optional().Nillable(),
		// Left out of client model pickers while staying fully callable
		// (e.g. a model a client calls internally). /v1/models still lists
		// it, marked "hidden": true.
		field.Bool("picker_hidden").Default(false),
	}
}

func (Model) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("group_links", ModelGroupModel.Type),
		edge.From("vendor", ModelVendor.Type).
			Ref("models").
			Field("vendor_id").
			Unique(),
	}
}
