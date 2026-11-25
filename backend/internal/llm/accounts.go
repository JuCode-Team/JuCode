package llm

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

// orderRouteAccounts is the request-side account ordering. The account is now
// the routing entity (route.Account is set by the biz resolver), so there is no
// per-channel expansion any more. This only:
//
//   - drops accounts that cannot serve right now (biz.AccountSchedulable) —
//     belt-and-braces, the biz router already filters them;
//   - promotes a bound / sticky account to the front of its priority tier so a
//     conversation keeps landing on the same account for cache affinity.
//
// Nothing here is client-visible: the client never learns which account served
// it, and no request input selects one. The only client-influenced input is the
// session anchor, which pins a conversation for cache-friendly affinity — the
// same anchor the provider-level sticky used to use.
func (g *Gateway) orderRouteAccounts(ctx context.Context, user *ent.User, apiKey *ent.ApiKey, groupID uuid.UUID, convID, model string, providers []biz.RouteAccount) []biz.RouteAccount {
	if len(providers) == 0 {
		return providers
	}
	now := time.Now()
	active := make([]biz.RouteAccount, 0, len(providers))
	parked := make([]biz.RouteAccount, 0, len(providers))
	for _, rp := range providers {
		if rp.Account != nil && biz.AccountSchedulable(rp.Account, model, now) {
			active = append(active, rp)
		} else {
			// A candidate with no account is an upstream bug; a non-schedulable
			// one is ordinary (parked by health). Keep both as a last resort so
			// a request still makes an attempt rather than failing untried.
			parked = append(parked, rp)
		}
	}
	if len(active) == 0 {
		active = parked
	}
	if len(active) == 0 {
		return providers
	}
	if g.accounts != nil && user != nil {
		pinned := uuid.Nil
		if bound, err := g.accounts.ResolveBinding(ctx, user.ID, apiKeyID(apiKey), organizationID(apiKey), convID); err != nil {
			slog.Debug("account.binding_lookup_failed", "err", err.Error())
		} else if bound != uuid.Nil {
			pinned = bound
		}
		if pinned == uuid.Nil {
			if sticky, err := g.accounts.LookupSticky(ctx, user.ID, groupID, convID); err != nil {
				slog.Debug("sticky.account_lookup_failed", "err", err.Error())
			} else if sticky != uuid.Nil {
				pinned = sticky
			}
		}
		if pinned != uuid.Nil {
			if assigned := assignedFirst(providers, pinned, model, now); len(assigned) > 0 {
				return assigned
			}
			active = promoteSticky(active, pinned)
		}
	}
	return active
}

// assignedFirst keeps the session's account at the front even while it is
// cooling. Hard-disabled accounts are left to the normal ordering. Other
// accounts that can serve right now follow, so this request can fall
// through inside the group without reassigning the session.
func assignedFirst(providers []biz.RouteAccount, pinned uuid.UUID, model string, now time.Time) []biz.RouteAccount {
	idx := -1
	for i := range providers {
		account := providers[i].Account
		if account != nil && account.ID == pinned && biz.AccountAssignable(account, now) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	out := []biz.RouteAccount{providers[idx]}
	for i := range providers {
		if i == idx {
			continue
		}
		account := providers[i].Account
		if account != nil && biz.AccountSchedulable(account, model, now) {
			out = append(out, providers[i])
		}
	}
	return out
}

// buildProviderForAttempt constructs the upstream client for one attempt. The
// credential always comes from the account: g.accounts.RequestToken resolves
// the stored API key.
func (g *Gateway) buildProviderForAttempt(ctx context.Context, rp biz.RouteAccount, protocol biz.RequestProtocol) (Provider, error) {
	if rp.Account == nil {
		return nil, errors.New("route candidate has no account")
	}
	if g.accounts == nil {
		return nil, errors.New("account pool is not wired")
	}
	token, err := g.accounts.RequestToken(ctx, rp.Account)
	if err != nil {
		return nil, err
	}
	client := g.accountClient(rp.Account)
	switch protocol {
	case biz.RequestProtocolAnthropic:
		return NewAnthropicProviderWithClient(rp.Account, token, client), nil
	default:
		return NewOpenAIProviderWithClient(rp.Account, token, client), nil
	}
}

// acquireAccountSlot takes the account's in-flight slot when it caps
// concurrency. The slot is released when the request handler returns, which
// covers the streaming copy as well.
func (g *Gateway) acquireAccountSlot(ctx context.Context, rp biz.RouteAccount) bool {
	if rp.Account == nil || g.accounts == nil {
		return true
	}
	return g.accounts.Acquire(ctx, rp.Account.ID, rp.Account.MaxConcurrency)
}

func (g *Gateway) releaseAccountSlot(ctx context.Context, rp biz.RouteAccount) {
	if rp.Account == nil || g.accounts == nil {
		return
	}
	// The client may have canceled the request; the slot must still be
	// returned, so the decrement runs detached and bounded. A leaked slot
	// would hide the account until its TTL.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	g.accounts.Release(rctx, rp.Account.ID, rp.Account.MaxConcurrency)
}

// rememberAccountOutcome pins the winning account for the sticky window and
// records a successful attempt, so follow-up requests land on the same account
// and its health/last-used stay current.
func (g *Gateway) rememberAccountOutcome(ctx context.Context, userID, groupID uuid.UUID, convID string, rp biz.RouteAccount) {
	if rp.Account == nil || g.accounts == nil {
		return
	}
	existing, err := g.accounts.LookupSticky(ctx, userID, groupID, convID)
	if err == nil && (existing == uuid.Nil || existing == rp.Account.ID) {
		if err := g.accounts.RememberSticky(ctx, userID, groupID, convID, rp.Account.ID); err != nil {
			slog.Debug("sticky.account_remember_failed", "err", err.Error())
		}
	}
	g.accounts.TouchLastUsed(ctx, rp.Account.ID)
	g.accounts.RecordOutcome(ctx, rp.Account.ID, true)
	// A served request is proof the account works: it resets the failure
	// counters and lifts optimistic cooldowns the state machine set earlier.
	g.accounts.RecordAccountSuccess(ctx, rp.Account)
}

func (g *Gateway) withStickyGroup(ctx context.Context, userID uuid.UUID, model, convID string) context.Context {
	if g.sticky == nil || convID == "" {
		return ctx
	}
	session, modelGroup, err := g.sticky.LookupConversation(ctx, userID, model, convID)
	if err != nil || (session == uuid.Nil && modelGroup == uuid.Nil) {
		return ctx
	}
	return biz.WithStickyGroups(ctx, session, modelGroup)
}

func (g *Gateway) rememberRouteGroup(ctx context.Context, userID, groupID uuid.UUID, model, convID string, moved bool) {
	if g.sticky == nil {
		return
	}
	if err := g.sticky.RememberConversation(ctx, userID, groupID, model, convID, moved); err != nil && !biz.IsStickyDisabled(err) {
		slog.Debug("sticky.group_remember_failed", "err", err.Error())
	}
}

// sampleAccountQuota feeds the vendor's rate-limit headers into the account's
// quota snapshot. It runs detached from the request (the caller may be gone by
// the time the write lands, and no answer depends on it) and is throttled
// inside the service, so ordinary traffic costs nothing.
func (g *Gateway) sampleAccountQuota(reqCtx context.Context, rp biz.RouteAccount, header http.Header) {
	if rp.Account == nil || g.accounts == nil || !hasQuotaHeaders(header) {
		return
	}
	acctID := rp.Account.ID
	snapshot := header.Clone()
	base := context.WithoutCancel(reqCtx)
	go func() {
		ctx, cancel := context.WithTimeout(base, 5*time.Second)
		defer cancel()
		g.accounts.SampleQuotaHeaders(ctx, acctID, snapshot)
	}()
}

// hasQuotaHeaders keeps the sampling goroutine off the hot path for the vast
// majority of responses, which carry no rate-limit headers at all.
func hasQuotaHeaders(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(k, "Anthropic-Ratelimit") || strings.HasPrefix(k, "X-Codex") || k == "X-Ratelimit-Policy" {
			return true
		}
	}
	return false
}

// accountIDRef is the usage-row attribution pointer, nil for a candidate with
// no account.
func accountIDRef(acct *ent.UpstreamAccount) *uuid.UUID {
	if acct == nil || acct.ID == uuid.Nil {
		return nil
	}
	id := acct.ID
	return &id
}

// usageAttribution splits a route candidate's usage-log attribution:
// provider_id is the legacy channel a migrated account came from (nil for
// accounts created after the migration / with no legacy link), and
// provider_account_id is the account that served the attempt. Both are
// internal and never echoed to a client.
func usageAttribution(rp biz.RouteAccount) (providerID, accountID *uuid.UUID) {
	return rp.LegacyProviderID(), accountIDRef(rp.Account)
}

// metricOwner is the label value for the per-upstream metrics. It is the
// serving account's id, or the nil uuid when no account served the attempt.
func metricOwner(rp biz.RouteAccount) string {
	if rp.Account != nil && rp.Account.ID != uuid.Nil {
		return rp.Account.ID.String()
	}
	return uuid.Nil.String()
}

// routeAccountName is the admin-facing account name for the attempt chain.
func routeAccountName(rp biz.RouteAccount) string {
	if rp.Account == nil {
		return ""
	}
	return rp.Account.Name
}

// routeAccountKind labels the attempt's wire family for the admin drill-down:
// the account's declared api_format when set, else inferred, else "openai".
func routeAccountKind(rp biz.RouteAccount) string {
	return ProviderFormat(rp.Account)
}
