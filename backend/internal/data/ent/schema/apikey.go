package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ModelPin names the group a single model should try first. The model string
// is the public model name from the request, not the model row id.
type ModelPin struct {
	Model   string    `json:"model"`
	GroupID uuid.UUID `json:"group_id"`
}

type ApiKey struct{ ent.Schema }

func (ApiKey) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (ApiKey) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}),
		// "sk-juc-XXXXXXXX" — first 8 chars of the random secret, for UI display.
		field.String("prefix").NotEmpty(),
		// SHA-256 hash of the secret; gateway resolves an inbound API key
		// by hashing the bearer token and looking it up here. Kept Unique
		// + Sensitive so the column is excluded from default ent reads.
		field.String("hash").NotEmpty().Unique().Sensitive(),
		// AES-GCM-encrypted full secret. Empty for keys created before
		// this column existed (legacy keys can't be revealed; the
		// frontend renders a "legacy" badge instead of a show button).
		// Decrypted only by ApiKeySvc.Reveal — never logged.
		field.String("secret_enc").Default("").Sensitive(),
		field.String("name").Default(""),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("revoked_at").Optional().Nillable(),
		field.Time("expires_at").Optional().Nillable(),
		// Empty = key may call any model the owning user can access.
		// Non-empty = restricted to models in these ModelGroups (intersection
		// with the user's own access). Admin-curated; no FK enforcement.
		field.JSON("allowed_group_ids", []uuid.UUID{}).Default([]uuid.UUID{}),
		// Empty/NULL = key may be used from any address. Non-empty = the
		// gateway rejects requests whose client IP matches none of the
		// entries. Entries are single IPs ("1.2.3.4", "::1") or CIDRs
		// ("10.0.0.0/8"), normalized by biz.NormalizeIPWhitelist before
		// storage. Optional (nullable) — ent JSON defaults are ORM-level
		// only, so a NOT NULL column can't be added to existing rows;
		// NULL reads back as a nil slice which every consumer treats as
		// "unrestricted".
		field.JSON("ip_whitelist", []string{}).Optional().Default([]string{}),
		// auto_route: when a model lives in multiple ModelGroups the caller
		// can access, the gateway orders those groups by auto_route_mode
		// and tries the first one that can serve. When false the gateway
		// keeps the first name-ordered match.
		field.Bool("auto_route").Default(false),
		// auto_route_mode chooses the order used while auto_route is on.
		// price: multiplier times a cache-hit-adjusted unit price.
		// stable: recent success rate. fast: recent latency.
		// Groups with no samples stay in the middle of the order.
		field.Enum("auto_route_mode").Values("price", "stable", "fast").Default("price"),
		// auto_route_max_rate: ceiling for price mode. 0 = no ceiling.
		// Stable and fast modes ignore it. A model pin may still name a
		// group above the ceiling, because the user picked that group.
		field.Float("auto_route_max_rate").Default(0),
		// model_pins: for one model, try this group before the mode order.
		// Empty means every model follows auto_route_mode. A pin whose
		// group is disabled, deleted, or outside allowed_group_ids is ignored.
		field.JSON("model_pins", []ModelPin{}).Optional().Default([]ModelPin{}),
		// internal: this key belongs to the platform, not to a customer. Its
		// traffic is billed exactly like anyone else's — the flag only tags the
		// resulting availability observations as synthetic so they stay out of
		// the public availability percentage. Set on the status-page prober's
		// key; there is no admin UI for it, because a customer key that picked
		// this up would silently vanish from the metric it is supposed to move.
		field.Bool("internal").Default(false),
		// system: a console-owned key, currently the lab key. It is hidden
		// from the API key list, rejected by the public gateway, and cannot
		// be edited, revealed, or revoked. Allowed groups stay empty so the
		// lab can call every group the account can access.
		field.Bool("system").Default(false),
		field.Enum("billing_scope").Values("personal", "organization").Default("personal"),
		field.UUID("organization_id", uuid.UUID{}).Optional().Nillable(),
		field.Other("daily_cost_limit", decimal.Decimal{}).SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).Optional().Nillable(),
		field.Other("monthly_cost_limit", decimal.Decimal{}).SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).Optional().Nillable(),
		field.Other("total_cost_limit", decimal.Decimal{}).SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).Optional().Nillable(),
	}
}

func (ApiKey) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("api_keys").
			Field("user_id").
			Required().
			Unique(),
		edge.From("organization", Organization.Type).
			Ref("api_keys").
			Field("organization_id").
			Unique(),
		edge.To("usage_logs", UsageLog.Type),
	}
}

func (ApiKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "revoked_at"),
		index.Fields("organization_id"),
	}
}
