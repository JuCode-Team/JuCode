package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type User struct{ ent.Schema }

func (User) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (User) Fields() []ent.Field {
	return []ent.Field{
		field.String("email").NotEmpty().Unique(),
		// Optional secondary login handle. Populated for users migrated
		// from newapi (which keyed by username) — they can sign in with
		// either email or username thereafter. Nillable so direct
		// JuCode signups stay simple (no username required). Unique
		// when present; Postgres UNIQUE index ignores NULLs by default
		// so this doesn't fight multi-NULL rows.
		field.String("username").Optional().Nillable().Unique(),
		field.String("password_hash").NotEmpty().Sensitive(),
		field.Enum("role").Values("user", "admin").Default("user"),
		// status state machine:
		//   - "active":           normal account, may log in and make requests
		//   - "banned":            admin-disabled; login + API blocked
		//   - "pending_approval": registered but awaiting admin review;
		//                         login blocked with a friendly message
		//   - "rejected":          admin denied approval; treated like banned
		//                         but kept on the books so the same email
		//                         cannot silently re-register
		field.Enum("status").Values("active", "banned", "pending_approval", "rejected").Default("active"),
		field.Other("balance", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.String("currency").Default("CNY"),
		field.Bool("email_verified").Default(false),
		field.Time("email_verified_at").Optional().Nillable(),
		field.String("nickname").Optional().Nillable(),
		// Optional UserGroup. Drives ModelGroup access gating; nil =
		// "unassigned" (still has access to fully-public model groups).
		field.UUID("user_group_id", uuid.UUID{}).Optional().Nillable(),
		// concurrency_limit_override: per-user concurrency cap that
		// supersedes user_group + plan limits when > 0. Default 0 =
		// "use group/plan default" (no override). Negative values
		// disallowed at the field level.
		field.Int("concurrency_limit_override").Default(0).NonNegative(),
		// Per-user rate caps that supersede the user_group / plan limits when
		// > 0, mirroring concurrency_limit_override. 0 = no override.
		field.Int("rpm_limit_override").Default(0).NonNegative(),
		field.Int("tpm_limit_override").Default(0).NonNegative(),
		// priority: 0-100, higher = better. Not surfaced in user-facing UI;
		// admin can read/edit. Auto-decremented on policy_keyword /
		// cyber_security incidents (see internal/biz/priority.go).
		field.Int("priority").Default(100).Min(0).Max(100),
		// frozen_until: a reversible, time-boxed account suspension applied by
		// the automated sanction engine (see internal/biz/sanction.go). While
		// now < frozen_until the API-key auth middleware rejects requests with
		// 403 account_frozen; the freeze lifts on its own at expiry (no
		// unfreeze job needed). nil = not frozen. Distinct from status=banned,
		// which is a permanent admin/sanction decision.
		field.Time("frozen_until").Optional().Nillable(),
		// invite_code: the user's own referral code (8 chars, uppercase
		// alphanumeric excluding 0O1I). Lazily generated — on Register for
		// new users, on first GET /v1/me/invite for pre-existing ones — so
		// nillable. Unique when present (Postgres UNIQUE ignores NULLs).
		field.String("invite_code").Optional().Nillable().Unique(),
		// invited_by_user_id: denormalized inviter pointer, set once at
		// registration when a valid invite code was supplied. The
		// authoritative bind record (IP/UA/status) lives in invite_bindings;
		// this column exists for cheap "who invited X" lookups.
		field.UUID("invited_by_user_id", uuid.UUID{}).Optional().Nillable(),
		// register_ip / last_login_ip: the client IP at signup and at the
		// latest password/TOTP login. Read by the invite risk checks to spot
		// an inviter referring their own second account. Nullable: accounts
		// created before 2026-10 have no recorded IP.
		field.String("register_ip").Optional(),
		field.String("last_login_ip").Optional(),
		field.Time("last_login_at").Optional().Nillable(),
		// --- TOTP two-factor auth ---
		// Any user may bind TOTP voluntarily; privileged roles (admin /
		// cs / agent / promoter) are steered to bind on login. totp_secret is written
		// at setup time and only becomes authoritative once totp_enabled
		// flips true on confirm — an unconfirmed secret is a pending setup
		// that the next setup call overwrites.
		field.Bool("totp_enabled").Default(false),
		field.String("totp_secret").Optional().Nillable().Sensitive(),
		field.Time("totp_confirmed_at").Optional().Nillable(),
		// totp_recovery_codes stores SHA-256 hex digests of the UNUSED
		// recovery codes. Plaintext is shown exactly once at confirm time.
		// A consumed code is removed from the slice (one-time use).
		field.Strings("totp_recovery_codes").Optional().Sensitive(),
	}
}

func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("api_keys", ApiKey.Type),
		edge.To("oauth_authorizations", OAuthAuthorization.Type),
		edge.To("user_plans", UserPlan.Type),
		edge.To("balance_txs", BalanceTx.Type),
		edge.To("owned_organizations", Organization.Type),
		edge.To("organization_memberships", OrganizationMember.Type),
		edge.To("organization_balance_txs", OrganizationBalanceTx.Type),
		edge.To("site_orders", SiteOrder.Type),
		edge.To("usage_logs", UsageLog.Type),
		edge.To("roles", UserRole.Type),
		edge.To("model_group_grants", ModelGroupUserGrant.Type),
		edge.From("user_group", UserGroup.Type).
			Ref("users").
			Field("user_group_id").
			Unique(),
	}
}

func (User) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "role"),
		index.Fields("invited_by_user_id"),
	}
}

// Annotations: keep email lookups fast & case-insensitive at the app layer
// (we lowercase before storing). No DB-level citext needed for MVP.
var _ = schema.Annotation(nil)
