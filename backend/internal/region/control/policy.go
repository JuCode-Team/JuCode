package control

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/region"
)

// LeaseTerms bound one credit lease.
type LeaseTerms struct {
	// MaxCNY caps the amount frozen by one lease; an edge asking for more
	// gets this much.
	MaxCNY decimal.Decimal
	// OverdraftCNY is how far below zero the balance may go when the lease
	// is frozen.
	OverdraftCNY decimal.Decimal
	// TTL is how long the lease lives before the cleanup loop releases it.
	TTL time.Duration
}

// LeasePolicy decides the terms of a lease request. The SQL ledger still
// enforces balance, organization limits and API key cost limits on top of
// whatever the policy returns.
type LeasePolicy interface {
	Terms(req region.CreditLeaseRequest) LeaseTerms
}

// FixedLeasePolicy gives every region and account the same terms; it is what
// the region.lease_cny / overdraft_cny / lease_ttl config builds.
type FixedLeasePolicy LeaseTerms

func (p FixedLeasePolicy) Terms(region.CreditLeaseRequest) LeaseTerms { return LeaseTerms(p) }
