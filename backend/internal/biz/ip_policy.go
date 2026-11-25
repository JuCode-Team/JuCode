// Package biz — IP policy service.
//
// IPPolicySvc owns the operator-managed CIDR list backing three CC-
// defense knobs: trusted-proxy whitelist (consumed by Gin's
// SetTrustedProxies at startup), per-IP allowlist (skips the global
// hard-cap), and a permanent block list (returns 429 at the outermost
// middleware).
//
// Hot path: middlewares ask MatchAllow / MatchBlock on every request.
// To keep that off the DB, we cache a parsed snapshot in memory and
// refresh it every refreshInterval. Mutations (Add / Delete) bump the
// snapshot immediately so admins see their change take effect on the
// next request.
package biz

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/ippolicy"
)

// refreshInterval bounds how stale the in-memory snapshot can be when
// no admin mutation has triggered an invalidation. 30s is short enough
// for operator updates to take effect without manual restart but long
// enough that the DB isn't queried per-request.
const refreshInterval = 30 * time.Second

// PolicyKind mirrors the schema enum as plain strings so callers can
// use these constants without depending on the generated ent package.
type PolicyKind string

const (
	PolicyKindTrustedProxy PolicyKind = "trusted_proxy"
	PolicyKindAllow        PolicyKind = "allow"
	PolicyKindBlock        PolicyKind = "block"
)

// PolicyEntry is the slimmed-down view served to admins and used inside
// the middleware. Hides the ent.IPPolicy receiver behind a stable shape
// so we can later move storage without breaking handlers.
type PolicyEntry struct {
	ID        uuid.UUID  `json:"id"`
	CIDR      string     `json:"cidr"`
	Kind      PolicyKind `json:"kind"`
	Note      string     `json:"note"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
}

// snapshot is the parsed form the middleware checks against. Each kind
// bucket holds parsed nets so MatchAllow/MatchBlock are O(N) over the
// (usually small) policy table — no per-request DB hop.
type snapshot struct {
	loadedAt   time.Time
	trustedRaw []string // emitted to gin.SetTrustedProxies verbatim
	allowNets  []*net.IPNet
	blockNets  []*net.IPNet
}

type IPPolicySvc struct {
	d *data.Data

	mu      sync.RWMutex
	snap    *snapshot
	refresh chan struct{} // signaled by mutations to force-reload
}

func NewIPPolicySvc(d *data.Data) *IPPolicySvc {
	return &IPPolicySvc{d: d, refresh: make(chan struct{}, 1)}
}

// StartRefreshLoop is optional — keeps the snapshot fresh in the
// background so the first request after the interval doesn't pay the
// reload cost. Cancelled by ctx.
func (s *IPPolicySvc) StartRefreshLoop(ctx context.Context) {
	go func() {
		t := time.NewTicker(refreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = s.reload(ctx)
			case <-s.refresh:
				_ = s.reload(ctx)
			}
		}
	}()
}

// reload pulls the current rows from DB and replaces the snapshot.
// Errors leave the previous snapshot in place — better stale than empty.
func (s *IPPolicySvc) reload(ctx context.Context) error {
	rows, err := s.d.Ent.IPPolicy.Query().All(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	snap := &snapshot{loadedAt: now}
	for _, r := range rows {
		if r.ExpiresAt != nil && r.ExpiresAt.Before(now) {
			continue
		}
		switch r.Kind {
		case ippolicy.KindTrustedProxy:
			snap.trustedRaw = append(snap.trustedRaw, r.Cidr)
		case ippolicy.KindAllow:
			if n := parseToNet(r.Cidr); n != nil {
				snap.allowNets = append(snap.allowNets, n)
			}
		case ippolicy.KindBlock:
			if n := parseToNet(r.Cidr); n != nil {
				snap.blockNets = append(snap.blockNets, n)
			}
		}
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	return nil
}

// ensure makes sure we have at least one snapshot loaded. Called by
// lookup helpers before the background loop has had a chance to run.
func (s *IPPolicySvc) ensure(ctx context.Context) {
	s.mu.RLock()
	loaded := s.snap != nil
	s.mu.RUnlock()
	if !loaded {
		_ = s.reload(ctx)
	}
}

// TrustedProxiesCIDRs returns the CIDR strings that should be passed to
// gin.SetTrustedProxies on startup. Combined with the static Cloudflare
// ranges and 127.0.0.1 in the caller.
func (s *IPPolicySvc) TrustedProxiesCIDRs(ctx context.Context) []string {
	s.ensure(ctx)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snap == nil {
		return nil
	}
	out := make([]string, len(s.snap.trustedRaw))
	copy(out, s.snap.trustedRaw)
	return out
}

// List returns the policy entries, optionally filtered by kind.
// Reads straight from DB (admin-facing — not on the hot path), so
// expires_at filtering is left to the caller / UI.
func (s *IPPolicySvc) List(ctx context.Context, kind PolicyKind) ([]PolicyEntry, error) {
	q := s.d.Ent.IPPolicy.Query()
	if kind != "" {
		q = q.Where(ippolicy.KindEQ(ippolicy.Kind(kind)))
	}
	rows, err := q.Order(ent.Desc(ippolicy.FieldCreatedAt)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PolicyEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, toEntry(r))
	}
	return out, nil
}

// AddInput is the admin-supplied data for creating a policy entry.
type AddInput struct {
	CIDR      string
	Kind      PolicyKind
	Note      string
	ExpiresAt *time.Time
	CreatedBy *uuid.UUID
}

// Add validates and inserts a new entry. Returns ErrInvalidCIDR when
// cidr can't be parsed as either a single IP or a CIDR block; other
// errors bubble up from ent (unique constraint, etc.).
func (s *IPPolicySvc) Add(ctx context.Context, in AddInput) (*PolicyEntry, error) {
	cidr := strings.TrimSpace(in.CIDR)
	if cidr == "" {
		return nil, ErrInvalidCIDR
	}
	if parseToNet(cidr) == nil {
		return nil, ErrInvalidCIDR
	}
	if in.Kind != PolicyKindTrustedProxy && in.Kind != PolicyKindAllow && in.Kind != PolicyKindBlock {
		return nil, fmt.Errorf("invalid kind %q", in.Kind)
	}
	create := s.d.Ent.IPPolicy.Create().
		SetCidr(cidr).
		SetKind(ippolicy.Kind(in.Kind)).
		SetNote(in.Note)
	if in.ExpiresAt != nil {
		create = create.SetExpiresAt(*in.ExpiresAt)
	}
	if in.CreatedBy != nil {
		create = create.SetCreatedBy(*in.CreatedBy)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	s.bumpRefresh()
	e := toEntry(row)
	return &e, nil
}

// Delete removes an entry by ID. Returns ent.IsNotFound on miss; caller
// usually maps that to 404.
func (s *IPPolicySvc) Delete(ctx context.Context, id uuid.UUID) error {
	if err := s.d.Ent.IPPolicy.DeleteOneID(id).Exec(ctx); err != nil {
		return err
	}
	s.bumpRefresh()
	return nil
}

func (s *IPPolicySvc) bumpRefresh() {
	select {
	case s.refresh <- struct{}{}:
	default:
		// channel buffer is 1; already-pending reload covers us.
	}
}

// ErrInvalidCIDR is returned when an admin submits an unparseable
// cidr value.
var ErrInvalidCIDR = fmt.Errorf("invalid cidr or ip")

// parseToNet accepts both bare IPs and CIDR blocks, returning a
// *net.IPNet that Contains() the original address.
func parseToNet(s string) *net.IPNet {
	s = strings.TrimSpace(s)
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n
	}
	if ip := net.ParseIP(s); ip != nil {
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		mask := net.CIDRMask(bits, bits)
		return &net.IPNet{IP: ip, Mask: mask}
	}
	return nil
}

func toEntry(r *ent.IPPolicy) PolicyEntry {
	e := PolicyEntry{
		ID:        r.ID,
		CIDR:      r.Cidr,
		Kind:      PolicyKind(r.Kind),
		Note:      r.Note,
		CreatedAt: r.CreatedAt,
	}
	if r.ExpiresAt != nil {
		e.ExpiresAt = r.ExpiresAt
	}
	if r.CreatedBy != nil {
		e.CreatedBy = r.CreatedBy
	}
	return e
}
