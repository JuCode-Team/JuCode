package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// IPPolicy holds operator-managed IP / CIDR entries that customize the
// CC-defense layer:
//
//   - kind="trusted_proxy": treated as a Gin trusted proxy on startup
//     (in addition to the auto-fetched Cloudflare ranges). Used for
//     internal VPC test boxes, additional load balancers, etc.
//   - kind="allow": exempt from the global per-IP hard-cap and from the
//     auto-block list. Used for in-house load testing, monitoring probes,
//     or partner integrations that legitimately exceed limits.
//   - kind="block": permanent block (no TTL). Returns 429 at the
//     outermost middleware. Different from the Redis-backed automatic
//     block list (which expires after 1h).
//
// `cidr` is a string column rather than a postgres `cidr` type so we can
// store either single IPs ("1.2.3.4") or CIDR ranges ("10.0.0.0/8")
// without two columns. Validation happens at the biz layer.
//
// `expires_at` is optional: trusted_proxy / allow entries normally live
// forever; manual block entries can carry an expiry so operators don't
// have to remember to clean up.
type IPPolicy struct{ ent.Schema }

func (IPPolicy) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (IPPolicy) Fields() []ent.Field {
	return []ent.Field{
		field.String("cidr").NotEmpty().
			Comment("Single IP (1.2.3.4) or CIDR (10.0.0.0/8)"),
		field.Enum("kind").Values(
			"trusted_proxy", // forwarded-by; honored at SetTrustedProxies
			"allow",         // skip rate limits + block list
			"block",         // permanent block
		),
		field.String("note").Default(""),
		field.Time("expires_at").Optional().Nillable(),
		field.UUID("created_by", uuid.UUID{}).Optional().Nillable().
			Comment("Admin user that added this entry"),
	}
}

func (IPPolicy) Indexes() []ent.Index {
	return []ent.Index{
		// Lookup by kind is the hot path (filter the snapshot per
		// middleware concern); compound with created_at for stable
		// admin listing.
		index.Fields("kind", "created_at"),
		// Prevent dup entries: same cidr × same kind is meaningless.
		index.Fields("cidr", "kind").Unique(),
	}
}

// Ensure the time package import isn't dropped if expires_at is removed
// in a refactor.
var _ = time.Time{}
