// Package control is the control-plane side of the experimental regional
// edge feature: the private /internal/region/v1 endpoints that jucode-edge
// nodes call, and the SQL ledger behind them. It is separate from the public
// /v1 API and protected by a dedicated service token, not user JWT/API-key
// middleware. Nothing here runs unless config region.enabled is true.
package control

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	regionpkg "github.com/jucode/jucode/internal/region"
)

// Snapshots is what the control plane tells edges about the main site's
// state. The main server implements it on top of the account pool and API
// key services.
type Snapshots interface {
	// UpstreamSnapshot lists every upstream account an edge may forward to,
	// with credentials encrypted for transport.
	UpstreamSnapshot(ctx context.Context) ([]regionpkg.ProviderSnapshot, error)
	// AccountSnapshot resolves an API key by its SHA-256 hex digest.
	AccountSnapshot(ctx context.Context, apiKeyHash string) (*regionpkg.AccountSnapshot, error)
}

// Ledger is the money side of the control plane: credit leases frozen on the
// authoritative balance, and the usage events that settle against them.
// SQLLedger is the default implementation.
type Ledger interface {
	GrantLease(ctx context.Context, req regionpkg.CreditLeaseRequest) (*regionpkg.CreditLease, error)
	// ReleaseLease returns the unspent part of a lease. regionID is the
	// caller's region; a lease of another region is refused with
	// region.ErrRegionMismatch. An empty regionID skips the check (cleanup).
	ReleaseLease(ctx context.Context, regionID string, leaseID uuid.UUID) error
	ApplyUsage(ctx context.Context, events []regionpkg.UsageEvent) error
}

// Authenticator tells which region a request comes from. A request that
// names a different region_id than the authenticated one is refused, so one
// edge cannot spend or release another edge's leases.
type Authenticator interface {
	Authenticate(r *http.Request) (regionID string, ok bool)
}

// TokenAuth is the default Authenticator: a bearer service token per region,
// mapping token to region id (config region.edges).
type TokenAuth map[string]string

func (t TokenAuth) Authenticate(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	got := []byte(strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
	if len(got) == 0 {
		return "", false
	}
	// Compare against every token so the time taken does not tell which
	// prefix matched.
	regionID, ok := "", false
	for token, id := range t {
		if subtle.ConstantTimeCompare(got, []byte(token)) == 1 {
			regionID, ok = id, true
		}
	}
	return regionID, ok
}

type Handler struct {
	auth      Authenticator
	snapshots Snapshots
	ledger    Ledger
}

func NewHandler(auth Authenticator, snapshots Snapshots, ledger Ledger) *Handler {
	return &Handler{auth: auth, snapshots: snapshots, ledger: ledger}
}

const regionIDKey = "region.id"

// Mount registers the endpoints on r, which the caller mounts at
// region.APIPrefix.
func (h *Handler) Mount(r gin.IRouter) {
	r.POST(regionpkg.PathProviderSnapshot, h.authorized, h.providerSnapshot)
	r.POST(regionpkg.PathAccountSnapshot, h.authorized, h.accountSnapshot)
	r.POST(regionpkg.PathCreditLease, h.authorized, h.creditLease)
	r.POST(regionpkg.PathCreditLeaseRelease, h.authorized, h.releaseCreditLease)
	r.POST(regionpkg.PathUsageEvents, h.authorized, h.usageEvents)
}

func (h *Handler) authorized(c *gin.Context) {
	regionID, ok := h.auth.Authenticate(c.Request)
	if !ok || regionID == "" {
		abort(c, http.StatusUnauthorized, regionpkg.CodeUnauthorized, "missing or unknown service token")
		return
	}
	c.Set(regionIDKey, regionID)
}

func callerRegion(c *gin.Context) string { return c.GetString(regionIDKey) }

func abort(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, regionpkg.ErrorResponse{Error: regionpkg.ErrorBody{Code: code, Message: message}})
}

// abortErr answers with the code of a known protocol error, or a generic
// internal error that does not leak err's text.
func abortErr(c *gin.Context, err error) {
	status, code := regionpkg.StatusAndCode(err)
	message := "internal error"
	if code != regionpkg.CodeInternal {
		message = err.Error()
	}
	abort(c, status, code, message)
}

func (h *Handler) providerSnapshot(c *gin.Context) {
	var req regionpkg.ProviderSnapshotRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		abort(c, http.StatusBadRequest, regionpkg.CodeInvalidRequest, "invalid provider snapshot request")
		return
	}
	providers, err := h.snapshots.UpstreamSnapshot(c.Request.Context())
	if err != nil {
		slog.Warn("region upstream snapshot failed", "region_id", callerRegion(c), "err", err)
		abort(c, http.StatusServiceUnavailable, regionpkg.CodeUnavailable, "provider snapshot unavailable")
		return
	}
	if len(req.ProviderIDs) > 0 {
		allowed := make(map[string]struct{}, len(req.ProviderIDs))
		for _, id := range req.ProviderIDs {
			if parsed, parseErr := uuid.Parse(strings.TrimSpace(id)); parseErr == nil {
				allowed[parsed.String()] = struct{}{}
			}
		}
		filtered := providers[:0]
		for _, provider := range providers {
			if _, ok := allowed[provider.ID.String()]; ok {
				filtered = append(filtered, provider)
			}
		}
		providers = filtered
	}
	c.JSON(http.StatusOK, regionpkg.ProviderSnapshotResponse{Version: snapshotVersion(providers), Providers: providers, GeneratedAt: time.Now()})
}

func (h *Handler) accountSnapshot(c *gin.Context) {
	var req regionpkg.AccountSnapshotRequest
	if err := c.ShouldBindJSON(&req); err != nil || !validHash(req.APIKeyHash) {
		abort(c, http.StatusBadRequest, regionpkg.CodeInvalidRequest, "api_key_hash must be a SHA-256 hex digest")
		return
	}
	account, err := h.snapshots.AccountSnapshot(c.Request.Context(), strings.ToLower(req.APIKeyHash))
	if err != nil {
		if !errors.Is(err, regionpkg.ErrNotFound) {
			slog.Warn("region account snapshot failed", "region_id", callerRegion(c), "err", err)
		}
		// Every failure reads as not found: the edge treats them the same
		// (the client's key is rejected) and the reason stays here.
		abort(c, http.StatusNotFound, regionpkg.CodeNotFound, "account not found")
		return
	}
	c.JSON(http.StatusOK, regionpkg.AccountSnapshotResponse{Account: *account})
}

func (h *Handler) usageEvents(c *gin.Context) {
	var req regionpkg.UsageEventsRequest
	decoder := json.NewDecoder(c.Request.Body)
	// Unknown fields are refused so a newer edge cannot slip data (least of
	// all request content) past a control plane that does not know it.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || len(req.Events) > regionpkg.MaxUsageEventsPerBatch {
		abort(c, http.StatusBadRequest, regionpkg.CodeInvalidRequest, "invalid usage event batch")
		return
	}
	for _, event := range req.Events {
		if event.RegionID != callerRegion(c) {
			abortErr(c, regionpkg.ErrRegionMismatch)
			return
		}
	}
	if err := h.ledger.ApplyUsage(c.Request.Context(), req.Events); err != nil {
		slog.Warn("region usage event apply failed", "region_id", callerRegion(c), "count", len(req.Events), "err", err)
		abortErr(c, err)
		return
	}
	c.JSON(http.StatusOK, regionpkg.UsageEventsResponse{Accepted: len(req.Events)})
}

func (h *Handler) creditLease(c *gin.Context) {
	var req regionpkg.CreditLeaseRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == uuid.Nil || strings.TrimSpace(req.RegionID) == "" || strings.TrimSpace(req.RequestID) == "" || strings.TrimSpace(req.RequestedCNY) == "" {
		abort(c, http.StatusBadRequest, regionpkg.CodeInvalidRequest, "invalid credit lease request")
		return
	}
	if strings.TrimSpace(req.RegionID) != callerRegion(c) {
		abortErr(c, regionpkg.ErrRegionMismatch)
		return
	}
	lease, err := h.ledger.GrantLease(c.Request.Context(), req)
	if err != nil {
		slog.Warn("region credit lease grant failed", "user_id", req.UserID, "api_key_id", req.APIKeyID, "billing_scope", req.BillingScope, "organization_id", req.OrganizationID, "region_id", req.RegionID, "request_id", req.RequestID, "err", err)
		abortErr(c, err)
		return
	}
	c.JSON(http.StatusOK, regionpkg.CreditLeaseResponse{Lease: *lease})
}

func (h *Handler) releaseCreditLease(c *gin.Context) {
	var req regionpkg.CreditLeaseReleaseRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.LeaseID == uuid.Nil {
		abort(c, http.StatusBadRequest, regionpkg.CodeInvalidRequest, "invalid lease id")
		return
	}
	if err := h.ledger.ReleaseLease(c.Request.Context(), callerRegion(c), req.LeaseID); err != nil {
		if !errors.Is(err, regionpkg.ErrRegionMismatch) {
			slog.Warn("region credit lease release failed", "region_id", callerRegion(c), "lease_id", req.LeaseID, "err", err)
		}
		abortErr(c, err)
		return
	}
	c.JSON(http.StatusOK, regionpkg.CreditLeaseReleaseResponse{Released: true})
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func snapshotVersion(providers []regionpkg.ProviderSnapshot) int64 {
	var version int64
	for _, p := range providers {
		if p.Version > version {
			version = p.Version
		}
	}
	return version
}
