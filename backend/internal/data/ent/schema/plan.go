package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type Plan struct{ ent.Schema }

func (Plan) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (Plan) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.Other("price", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.String("currency").Default("CNY"),
		field.Enum("type").Values("quota_subscription", "pay_as_you_go").Default("quota_subscription"),
		// 30 = monthly recurring window measured from start_at;
		// 0  = one-shot, expire_at is hard-set on purchase.
		field.Int("period_days").Default(30),
		field.Other("quota_5h", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Other("quota_weekly", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Other("quota_monthly", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		// model groups this plan can access; admin-curated, no FK enforcement.
		field.JSON("allowed_group_ids", []uuid.UUID{}).Default([]uuid.UUID{}),
		field.Enum("status").Values("active", "archived").Default("active"),
		field.String("description").Default(""),
		// Max simultaneous in-flight LLM requests when this user-plan is the
		// active billing source. 0 = unlimited.
		field.Int("concurrency_limit").Default(0).NonNegative(),
		// Rate caps over a rolling 60s window, applied when this user-plan is
		// the active billing source. 0 = unlimited (same convention as
		// concurrency_limit). RPM counts requests that an upstream actually
		// accepted; TPM accumulates output tokens after the response.
		field.Int("rpm_limit").Default(0).NonNegative(),
		field.Int("tpm_limit").Default(0).NonNegative(),
		field.Other("monthly_spend_limit", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Other("initial_balance", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Bool("auto_reload_enabled").Default(false),
		field.Other("auto_reload_threshold", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
		field.Other("auto_reload_amount", decimal.Decimal{}).
			SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).
			Default(decimal.Zero),
	}
}
