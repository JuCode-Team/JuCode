package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/region"
)

type cachedAccount struct {
	account *region.AccountSnapshot
	expires time.Time
}

// cachedUpstream is one upstream ACCOUNT this node may forward to. The wire
// snapshot keeps its historical name (region.ProviderSnapshot) and its ID is the
// account id; nothing here may treat that id as a separate channel entity —
// there is exactly one upstream layer now, and the edge's job is unchanged:
// schedule by the id it was given, forward to the endpoint it was told about.
type cachedUpstream struct {
	snapshot        region.ProviderSnapshot
	apiKey          string
	anthropicAPIKey string
}

// upstreamKey returns the credential for the wire protocol being spoken.
// Anthropic uses its override when set; every other protocol uses the default.
func (p cachedUpstream) upstreamKey(protocol string) string {
	if protocol == "anthropic" && p.anthropicAPIKey != "" {
		return p.anthropicAPIKey
	}
	return p.apiKey
}

type localLease struct {
	accountID  uuid.UUID
	remaining  int64
	releasable bool
}

// pendingSettlement is a completed request whose accounting could not be made
// fully durable in-line (outbox enqueue or local settle failed). It is held
// in memory and retried by the settlement retry loop; while it is pending the
// reservation stays held in Redis, so the money is frozen, never refunded.
type pendingSettlement struct {
	event     region.UsageEvent
	accountID uuid.UUID
	actual    int64
	// enqueued marks that the usage event is already durable on disk and only
	// the local settle still needs to happen.
	enqueued bool
}

// ControlPlane is how an edge talks to its control plane. *region.Client is
// the HTTP implementation; another transport (a queue, a different RPC) can
// replace it as long as it keeps the region package's semantics, including
// returning the region sentinel errors for refused leases.
type ControlPlane interface {
	FetchAccount(ctx context.Context, apiKeyHash string) (*region.AccountSnapshot, error)
	// FetchProviders returns the upstream account snapshot, narrowed to
	// accountIDs.
	FetchProviders(ctx context.Context, accountIDs []string) (*region.ProviderSnapshotResponse, error)
	GrantLease(ctx context.Context, req region.CreditLeaseRequest) (*region.CreditLease, error)
	ReleaseLease(ctx context.Context, leaseID uuid.UUID) error
	PushUsage(ctx context.Context, events []region.UsageEvent) error
}

type Service struct {
	cfg       Config
	ledger    *Ledger
	rdb       *redis.Client
	outbox    *FileOutbox
	control   ControlPlane // nil: standalone, no control plane
	cipher    *cryptopkg.Cipher
	hc        *http.Client
	streamHC  *http.Client
	lease     int64
	reserve   int64
	overdraft int64

	mu        sync.RWMutex
	accounts  map[string]cachedAccount
	upstreams []cachedUpstream
	leaseMu   sync.Mutex
	leases    map[uuid.UUID]localLease

	retryMu       sync.Mutex
	settleRetries []pendingSettlement
}

// NewService builds an edge. control is nil in standalone mode
// (upstreams.file, no leases); NewControlPlane builds the HTTP one.
func NewService(cfg Config, control ControlPlane) (*Service, error) {
	reserve, err := region.ParseUnits(cfg.ReserveCNY)
	if err != nil {
		return nil, err
	}
	overdraft, err := region.ParseUnits(cfg.OverdraftCNY)
	if err != nil {
		return nil, err
	}
	lease, err := region.ParseUnits(cfg.LeaseCNY)
	if err != nil || lease <= 0 {
		if err == nil {
			err = errors.New("lease_cny must be positive")
		}
		return nil, err
	}
	outbox, err := NewFileOutbox(cfg.OutboxDir)
	if err != nil {
		return nil, err
	}
	cipher, err := cryptopkg.New(cfg.SyncKey)
	if err != nil {
		return nil, fmt.Errorf("regional sync cipher: %w", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	transport := edgeTransport()
	s := &Service{
		cfg: cfg, ledger: NewLedger(rdb, "jucode:edge:"+cfg.RegionID, cfg.RequestTTL),
		rdb:    rdb,
		outbox: outbox, control: control, cipher: cipher,
		// Non-streaming requests keep a bounded total duration. A normal JSON
		// response must not be allowed to hang forever while waiting for the
		// upstream body.
		hc: &http.Client{Transport: transport, Timeout: cfg.RequestTimeout},
		// Streaming responses are governed by the caller context and the
		// stream-specific idle/total policies. http.Client.Timeout is a hard
		// wall-clock deadline that would terminate an otherwise healthy SSE
		// stream after the non-stream request budget.
		streamHC: &http.Client{Transport: transport},
		lease:    lease, reserve: reserve, overdraft: overdraft,
		accounts: make(map[string]cachedAccount),
		leases:   make(map[uuid.UUID]localLease),
	}
	// providers_file holds the upstream ACCOUNT snapshot (the JSON keeps its
	// historical "providers" key); allowed_provider_ids lists account ids.
	if cfg.ProvidersFile != "" {
		if err := s.loadUpstreamsFile(cfg.ProvidersFile); err != nil {
			return nil, err
		}
	} else {
		_ = s.loadUpstreamCache(ctx)
	}
	return s, nil
}

// NewControlPlane returns the HTTP control plane cfg points at, or nil when
// no control_url is configured.
func NewControlPlane(cfg Config) ControlPlane {
	if cfg.ControlURL == "" {
		return nil
	}
	return region.NewClient(cfg.ControlURL, cfg.ServiceToken, nil)
}

func edgeTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        1000, MaxIdleConnsPerHost: 500,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
	}
}

func (s *Service) Close() { _ = s.ledger.Close() }

func (s *Service) Start(ctx context.Context) {
	// Local accounting durability must not depend on the control plane being
	// configured: parked settlements are retried even in providers-file mode.
	go s.settlementRetryLoop(ctx)
	if s.control == nil {
		return
	}
	go s.upstreamSyncLoop(ctx)
	go s.accountSyncLoop(ctx)
	go s.outboxLoop(ctx)
}

func (s *Service) upstreamSyncLoop(ctx context.Context) {
	s.syncUpstreams(ctx)
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncUpstreams(ctx)
		}
	}
}

// syncUpstreams pulls the upstream ACCOUNT snapshot. The control-plane method
// and its endpoint still say "provider"; the ids it returns are account ids.
func (s *Service) syncUpstreams(ctx context.Context) {
	resp, err := s.control.FetchProviders(ctx, s.cfg.AllowedProviderIDs)
	if err != nil {
		slog.Warn("regional upstream sync failed", "err", err)
		return
	}
	if err := s.applyUpstreams(resp.Providers); err != nil {
		slog.Warn("regional upstream snapshot rejected", "err", err)
	}
}

func (s *Service) accountSyncLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshCachedAccounts(ctx)
		}
	}
}

func (s *Service) refreshCachedAccounts(ctx context.Context) {
	s.mu.RLock()
	hashes := make([]string, 0, len(s.accounts))
	for hash := range s.accounts {
		hashes = append(hashes, hash)
	}
	s.mu.RUnlock()
	for _, hash := range hashes {
		account, err := s.control.FetchAccount(ctx, hash)
		if err != nil {
			slog.Warn("regional account sync failed", "err", err)
			continue
		}
		s.mu.Lock()
		s.accounts[hash] = cachedAccount{account: account, expires: time.Now().Add(s.cfg.AccountTTL)}
		s.mu.Unlock()
		if data, marshalErr := json.Marshal(account); marshalErr == nil {
			if cacheErr := s.rdb.Set(ctx, s.accountCacheKey(hash), data, s.cfg.AccountTTL).Err(); cacheErr != nil {
				slog.Warn("regional account cache refresh failed", "err", cacheErr)
			}
		}
	}
}

func (s *Service) outboxLoop(ctx context.Context) {
	s.flushOutbox(ctx)
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushOutbox(ctx)
		}
	}
}

// flushOutbox pushes one batch of pending usage events to the control plane
// and acknowledges them locally once it accepted them.
func (s *Service) flushOutbox(ctx context.Context) {
	events, err := s.outbox.Pending(ctx, 100)
	if err != nil || len(events) == 0 {
		return
	}
	if err := s.control.PushUsage(ctx, events); err != nil {
		slog.Warn("regional usage sync failed", "count", len(events), "err", err)
		return
	}
	if err := s.outbox.Ack(ctx, events); err != nil {
		slog.Warn("regional outbox ack failed", "err", err)
		return
	}
	s.releaseReleasableLeases(ctx)
}

func (s *Service) loadUpstreamsFile(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read upstreams file: %w", err)
	}
	var resp region.ProviderSnapshotResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("decode upstreams file: %w", err)
	}
	return s.applyUpstreams(resp.Providers)
}

// upstreamCacheKey keeps its historical "provider-snapshot" spelling: a cached
// snapshot written by an older edge (or read by a newer one) must be the same
// key, and its IDs are account ids either way.
func (s *Service) upstreamCacheKey() string {
	return "jucode:edge:" + s.cfg.RegionID + ":provider-snapshot"
}

func (s *Service) loadUpstreamCache(ctx context.Context) error {
	data, err := s.rdb.Get(ctx, s.upstreamCacheKey()).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return err
	}
	var resp region.ProviderSnapshotResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return err
	}
	return s.applyUpstreams(resp.Providers)
}

// applyUpstreams replaces the schedulable upstream set with a fresh snapshot.
// Each snapshot is an upstream account (snap.ID is an account id).
func (s *Service) applyUpstreams(snapshots []region.ProviderSnapshot) error {
	logger := slog.Default()
	upstreams := make([]cachedUpstream, 0, len(snapshots))
	for _, snap := range snapshots {
		if len(s.cfg.AllowedProviderIDs) > 0 && !containsUpstreamID(s.cfg.AllowedProviderIDs, snap.ID.String()) {
			continue
		}
		if !snap.Enabled || strings.TrimSpace(snap.BaseURL) == "" {
			continue
		}
		if snap.APIKeyEnvelope == "" && snap.AnthropicAPIKeyEnvelope == "" {
			continue
		}
		key := ""
		if snap.APIKeyEnvelope != "" {
			var err error
			key, err = s.cipher.Decrypt(snap.APIKeyEnvelope)
			if err != nil {
				return fmt.Errorf("decrypt upstream account %s: %w", snap.ID, err)
			}
		}
		anthropicKey := ""
		if snap.AnthropicAPIKeyEnvelope != "" {
			var err error
			anthropicKey, err = s.cipher.Decrypt(snap.AnthropicAPIKeyEnvelope)
			if err != nil {
				return fmt.Errorf("decrypt upstream account %s anthropic key: %w", snap.ID, err)
			}
		}
		if key == "" && anthropicKey == "" {
			continue
		}
		upstreams = append(upstreams, cachedUpstream{snapshot: snap, apiKey: key, anthropicAPIKey: anthropicKey})
	}
	s.mu.Lock()
	s.upstreams = upstreams
	s.mu.Unlock()
	if data, err := json.Marshal(region.ProviderSnapshotResponse{Providers: snapshots, GeneratedAt: time.Now()}); err == nil {
		if err := s.rdb.Set(context.Background(), s.upstreamCacheKey(), data, s.cfg.SyncInterval*4).Err(); err != nil {
			logger.Warn("regional upstream cache write failed", "err", err)
		}
	}
	logger.Info("regional upstream accounts loaded", "count", len(upstreams), "region", s.cfg.RegionID)
	return nil
}

// containsUpstreamID compares one configured allowed account id. The config key
// (allowed_provider_ids) is operator-facing and stays as it is; its values are
// account ids.
func containsUpstreamID(ids []string, want string) bool {
	for _, id := range ids {
		if strings.EqualFold(strings.TrimSpace(id), want) {
			return true
		}
	}
	return false
}

func (s *Service) account(ctx context.Context, bearer string) (*region.AccountSnapshot, error) {
	secret := strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer "))
	if secret == "" {
		return nil, errors.New("missing bearer token")
	}
	hash := hashSecret(secret)
	now := time.Now()
	s.mu.RLock()
	cached, ok := s.accounts[hash]
	s.mu.RUnlock()
	if ok && now.Before(cached.expires) {
		return cached.account, nil
	}
	if data, redisErr := s.rdb.Get(ctx, s.accountCacheKey(hash)).Bytes(); redisErr == nil {
		var snapshot region.AccountSnapshot
		if json.Unmarshal(data, &snapshot) == nil && snapshot.APIKeyHash == hash {
			s.mu.Lock()
			s.accounts[hash] = cachedAccount{account: &snapshot, expires: now.Add(s.cfg.AccountTTL)}
			s.mu.Unlock()
			return &snapshot, nil
		}
	} else if !errors.Is(redisErr, redis.Nil) {
		slog.Warn("regional account cache read failed", "err", redisErr)
	}
	if s.control == nil {
		return nil, errors.New("account is not cached and control plane is not configured")
	}
	account, err := s.control.FetchAccount(ctx, hash)
	if err != nil {
		return nil, err
	}
	if account.UserID == uuid.Nil || account.APIKeyID == uuid.Nil || account.APIKeyHash != hash {
		return nil, errors.New("invalid account snapshot")
	}
	s.mu.Lock()
	s.accounts[hash] = cachedAccount{account: account, expires: now.Add(s.cfg.AccountTTL)}
	s.mu.Unlock()
	if data, marshalErr := json.Marshal(account); marshalErr == nil {
		if cacheErr := s.rdb.Set(ctx, s.accountCacheKey(hash), data, s.cfg.AccountTTL).Err(); cacheErr != nil {
			slog.Warn("regional account cache write failed", "err", cacheErr)
		}
	}
	return account, nil
}

func (s *Service) accountCacheKey(hash string) string {
	return "jucode:edge:" + s.cfg.RegionID + ":account:" + hash
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *Service) seedAccount(ctx context.Context, a *region.AccountSnapshot) error {
	accountID, err := ledgerAccountID(a)
	if err != nil {
		return err
	}
	available, err := region.ParseUnits(a.BalanceCNY)
	if err != nil {
		return err
	}
	overdraft := s.overdraft
	if a.OverdraftCNY != "" {
		if n, e := region.ParseUnits(a.OverdraftCNY); e == nil {
			overdraft = n
		}
	}
	return s.ledger.SeedAccount(ctx, accountID, available, overdraft)
}

func ledgerAccountID(a *region.AccountSnapshot) (uuid.UUID, error) {
	if a == nil || a.UserID == uuid.Nil {
		return uuid.Nil, errors.New("invalid regional account")
	}
	switch strings.TrimSpace(a.BillingScope) {
	case "", region.BillingScopePersonal:
		return a.UserID, nil
	case region.BillingScopeOrganization:
		if a.OrganizationID == nil || *a.OrganizationID == uuid.Nil {
			return uuid.Nil, errors.New("organization account id is empty")
		}
		return *a.OrganizationID, nil
	default:
		return uuid.Nil, fmt.Errorf("unsupported billing scope %q", a.BillingScope)
	}
}

func (s *Service) keySpendKey(apiKeyID uuid.UUID, window string) string {
	return "jucode:edge:" + s.cfg.RegionID + ":keyspend:" + apiKeyID.String() + ":" + window
}

// keySpendWindows names the day and month counters. They use the business
// timezone (config timezone, set with biz.SetTimezone at startup), which
// must be the control plane's server.timezone for the windows to line up
// with the authoritative limit check.
func keySpendWindows(now time.Time) (day, month string) {
	n := now.In(biz.Location())
	return "d:" + n.Format("20060102"), "m:" + n.Format("200601")
}

// recordKeySpend accumulates the settled cost per API key in Redis. The
// counters back the edge-side cost-limit tripwire; the authoritative check
// runs on the control plane when a lease is granted.
func (s *Service) recordKeySpend(ctx context.Context, apiKeyID uuid.UUID, amount int64) {
	if apiKeyID == uuid.Nil || amount <= 0 {
		return
	}
	day, month := keySpendWindows(time.Now())
	pipe := s.rdb.Pipeline()
	dayKey := s.keySpendKey(apiKeyID, day)
	monthKey := s.keySpendKey(apiKeyID, month)
	pipe.IncrBy(ctx, dayKey, amount)
	pipe.Expire(ctx, dayKey, 48*time.Hour)
	pipe.IncrBy(ctx, monthKey, amount)
	pipe.Expire(ctx, monthKey, 45*24*time.Hour)
	pipe.IncrBy(ctx, s.keySpendKey(apiKeyID, "total"), amount)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("regional key spend counter update failed", "api_key_id", apiKeyID, "err", err)
	}
}

// keyCostLimitExceeded enforces the API key daily/monthly/total cost limits
// from the account snapshot against the spend this edge has settled. It is a
// fast local tripwire: the control plane re-checks the same limits against
// the full authoritative usage history before freezing any new credit, so
// traffic split across edges is still bounded centrally.
func (s *Service) keyCostLimitExceeded(ctx context.Context, a *region.AccountSnapshot) bool {
	if a == nil || a.APIKeyID == uuid.Nil {
		return false
	}
	day, month := keySpendWindows(time.Now())
	checks := []struct{ limit, window string }{
		{a.DailyCostLimitCNY, day},
		{a.MonthlyCostLimitCNY, month},
		{a.TotalCostLimitCNY, "total"},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.limit) == "" {
			continue
		}
		limit, err := region.ParseUnits(check.limit)
		if err != nil || limit <= 0 {
			continue
		}
		used, err := s.rdb.Get(ctx, s.keySpendKey(a.APIKeyID, check.window)).Int64()
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				// Fail open: the reserve path needs the same Redis anyway, and
				// the authoritative grant-time check still applies.
				slog.Warn("regional key spend counter read failed", "api_key_id", a.APIKeyID, "err", err)
			}
			continue
		}
		if used >= limit {
			return true
		}
	}
	return false
}

func (s *Service) accountOverdraft(a *region.AccountSnapshot) int64 {
	overdraft := s.overdraft
	if a != nil && a.OverdraftCNY != "" {
		if n, err := region.ParseUnits(a.OverdraftCNY); err == nil && n >= 0 {
			overdraft = n
		}
	}
	return overdraft
}

// grantLease freezes real balance on the control plane and
// mirrors only that frozen slice into the local Redis ledger. The account
// snapshot balance is deliberately not used here: it is informational and
// may already be stale when several edge nodes are active.
func (s *Service) grantLease(ctx context.Context, a *region.AccountSnapshot, requestID string) error {
	if a == nil || a.UserID == uuid.Nil {
		return errors.New("invalid account for regional lease")
	}
	accountID, err := ledgerAccountID(a)
	if err != nil {
		return err
	}
	if s.control == nil {
		if strings.TrimSpace(a.BillingScope) == region.BillingScopeOrganization {
			return errors.New("organization billing requires the regional control plane")
		}
		return s.seedAccount(ctx, a)
	}
	oldLeaseID := uuid.Nil
	if state, stateErr := s.ledger.Account(ctx, accountID); stateErr == nil {
		oldLeaseID = state.LeaseID
	}
	lease, err := s.control.GrantLease(ctx, region.CreditLeaseRequest{
		UserID: a.UserID, APIKeyID: a.APIKeyID, BillingScope: a.BillingScope,
		OrganizationID: a.OrganizationID, RegionID: s.cfg.RegionID, RequestID: requestID,
		RequestedCNY: region.FormatUnits(s.lease),
	})
	if err != nil {
		return err
	}
	if lease.ID == uuid.Nil || lease.UserID != a.UserID || lease.RegionID != s.cfg.RegionID {
		return errors.New("invalid regional lease response")
	}
	if strings.TrimSpace(a.BillingScope) == region.BillingScopeOrganization {
		if lease.BillingScope != region.BillingScopeOrganization || a.OrganizationID == nil || lease.OrganizationID == nil || *lease.OrganizationID != *a.OrganizationID {
			return errors.New("regional organization lease mismatch")
		}
	}
	available, err := region.ParseUnits(lease.RemainingCNY)
	if err != nil {
		available, err = region.ParseUnits(lease.GrantedCNY)
	}
	if err != nil || available <= 0 {
		return errors.New("regional lease has no available credit")
	}
	if err := s.ledger.AddLease(ctx, accountID, lease.ID, available, s.accountOverdraft(a), lease.ExpiresAt); err != nil {
		return err
	}
	s.leaseMu.Lock()
	if old, ok := s.leases[oldLeaseID]; ok && oldLeaseID != uuid.Nil && oldLeaseID != lease.ID {
		old.releasable = true
		s.leases[oldLeaseID] = old
	}
	s.leases[lease.ID] = localLease{accountID: accountID, remaining: available}
	s.leaseMu.Unlock()
	return nil
}

// finalizeUsage makes the accounting of a completed request durable, in a
// strict order: (1) the usage event is fsynced into the file outbox, then
// (2) the local Redis reservation is settled, then (3) local spend counters
// are updated. If step 1 fails (disk full, outbox directory gone) the
// reservation is deliberately NOT settled: the money stays frozen and the
// whole unit is parked for retry, so a failure can never be treated as
// success and never turns into a fully refundable lease upstream.
func (s *Service) finalizeUsage(accountID uuid.UUID, event region.UsageEvent, actual int64, logCorrelation string) {
	ctx := context.Background()
	p := pendingSettlement{event: event, accountID: accountID, actual: actual}
	if err := s.outbox.Enqueue(ctx, event); err != nil {
		slog.Error("regional usage outbox enqueue failed; reservation stays held for retry",
			"request_id", event.RequestID, "correlation_id", logCorrelation, "err", err)
		s.parkSettlement(p)
		return
	}
	p.enqueued = true
	s.settleDurable(ctx, p, logCorrelation)
}

// settleDurable settles a reservation whose usage event is already durable on
// disk. Transient errors are parked for retry; a reservation that no longer
// exists locally is dropped (the durable event still charges centrally, the
// local ledger reconciles on the next lease sync).
func (s *Service) settleDurable(ctx context.Context, p pendingSettlement, logCorrelation string) {
	settlement, err := s.ledger.Settle(ctx, p.accountID, p.event.RequestID, p.actual)
	if err != nil {
		if errors.Is(err, ErrReservationNotFound) || errors.Is(err, ErrReservationConflict) || errors.Is(err, ErrAccountNotInitialized) {
			slog.Error("regional settlement unrecoverable locally; central event remains durable",
				"request_id", p.event.RequestID, "correlation_id", logCorrelation, "err", err)
			return
		}
		slog.Error("regional billing settlement failed; queued for retry",
			"request_id", p.event.RequestID, "correlation_id", logCorrelation, "err", err)
		s.parkSettlement(p)
		return
	}
	if settlement.OverdraftExceeded {
		slog.Warn("regional account exceeded overdraft limit", "user_id", p.event.UserID, "request_id", p.event.RequestID)
	}
	s.recordLeaseSpend(settlement.LeaseID, p.accountID, p.actual)
	s.recordKeySpend(ctx, p.event.APIKeyID, p.actual)
}

// releaseReservation settles a reservation with zero cost (upstream failure
// paths). No central event is needed, but a failed release is still retried
// so the reserved amount does not stay frozen until the request TTL.
func (s *Service) releaseReservation(accountID uuid.UUID, requestID string) {
	ctx := context.Background()
	if _, err := s.ledger.Settle(ctx, accountID, requestID, 0); err != nil {
		slog.Warn("regional reservation release failed; queued for retry", "request_id", requestID, "err", err)
		s.parkSettlement(pendingSettlement{
			event: region.UsageEvent{RequestID: requestID}, accountID: accountID, enqueued: true,
		})
	}
}

func (s *Service) parkSettlement(p pendingSettlement) {
	s.retryMu.Lock()
	s.settleRetries = append(s.settleRetries, p)
	s.retryMu.Unlock()
}

func (s *Service) settlementRetryLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.drainSettlementRetries(ctx)
		}
	}
}

// drainSettlementRetries replays parked settlements in order. If the outbox
// is still failing the remainder is put back untouched so nothing is dropped.
func (s *Service) drainSettlementRetries(ctx context.Context) {
	s.retryMu.Lock()
	pending := s.settleRetries
	s.settleRetries = nil
	s.retryMu.Unlock()
	for i := range pending {
		p := pending[i]
		if !p.enqueued {
			if err := s.outbox.Enqueue(ctx, p.event); err != nil {
				slog.Warn("regional usage outbox retry failed", "request_id", p.event.RequestID, "queued", len(pending)-i, "err", err)
				s.retryMu.Lock()
				s.settleRetries = append(pending[i:], s.settleRetries...)
				s.retryMu.Unlock()
				return
			}
			p.enqueued = true
		}
		s.settleDurable(ctx, p, "")
	}
}

func (s *Service) recordLeaseSpend(leaseID, accountID uuid.UUID, amount int64) {
	if leaseID == uuid.Nil || amount < 0 {
		return
	}
	s.leaseMu.Lock()
	if lease, ok := s.leases[leaseID]; ok && lease.accountID == accountID {
		lease.remaining -= amount
		s.leases[leaseID] = lease
	}
	s.leaseMu.Unlock()
}

func (s *Service) releaseReleasableLeases(ctx context.Context) {
	if s.control == nil {
		return
	}
	pending, err := s.outbox.Pending(ctx, 10000)
	if err != nil {
		return
	}
	pendingLease := make(map[uuid.UUID]struct{}, len(pending))
	for _, event := range pending {
		if event.LeaseID != uuid.Nil {
			pendingLease[event.LeaseID] = struct{}{}
		}
	}
	s.leaseMu.Lock()
	candidates := make(map[uuid.UUID]localLease)
	for id, lease := range s.leases {
		if lease.releasable {
			candidates[id] = lease
		}
	}
	s.leaseMu.Unlock()
	for leaseID, lease := range candidates {
		if _, ok := pendingLease[leaseID]; ok {
			continue
		}
		state, err := s.ledger.Account(ctx, lease.accountID)
		if err != nil || state.Reserved != 0 || state.LeaseID == leaseID {
			continue
		}
		if err := s.control.ReleaseLease(ctx, leaseID); err != nil {
			slog.Warn("regional lease release failed", "lease_id", leaseID, "err", err)
			continue
		}
		if err := s.ledger.DropLease(ctx, lease.accountID, leaseID, lease.remaining); err != nil {
			slog.Warn("regional local lease release failed", "lease_id", leaseID, "err", err)
			continue
		}
		s.leaseMu.Lock()
		delete(s.leases, leaseID)
		s.leaseMu.Unlock()
	}
}

func (s *Service) reserveRequest(ctx context.Context, a *region.AccountSnapshot, requestID string, amount int64) (*Reservation, error) {
	accountID, err := ledgerAccountID(a)
	if err != nil {
		return nil, err
	}
	if s.control == nil {
		if strings.TrimSpace(a.BillingScope) == region.BillingScopeOrganization {
			return nil, errors.New("organization billing requires the regional control plane")
		}
		if err := s.seedAccount(ctx, a); err != nil {
			return nil, err
		}
		return s.ledger.Reserve(ctx, accountID, requestID, amount, uuid.Nil)
	}

	state, err := s.ledger.Account(ctx, accountID)
	leaseExpiring := err == nil && state.LeaseID != uuid.Nil && !state.LeaseExpiresAt.IsZero() && state.LeaseExpiresAt.Before(time.Now().Add(time.Minute))
	if errors.Is(err, ErrAccountNotInitialized) || (err == nil && (state.LeaseID == uuid.Nil || leaseExpiring)) {
		if err := s.grantLease(ctx, a, requestID); err != nil {
			return nil, err
		}
		state, err = s.ledger.Account(ctx, accountID)
	}
	if err != nil {
		return nil, err
	}
	reservation, err := s.ledger.Reserve(ctx, accountID, requestID, amount, state.LeaseID)
	if !errors.Is(err, ErrInsufficientCredit) {
		return reservation, err
	}
	// The current local lease is exhausted. Grant a new slice atomically on
	// the control plane, then retry the same idempotent request once.
	if grantErr := s.grantLease(ctx, a, requestID); grantErr != nil {
		return reservation, grantErr
	}
	state, err = s.ledger.Account(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.ledger.Reserve(ctx, accountID, requestID, amount, state.LeaseID)
}

// snapshotProtocols mirrors biz.AccountSupportsRequestProtocol on the edge's
// snapshot type: the declared protocol list mapped onto the client-facing
// protocols this node serves ("openai" / "anthropic"), else the kind's native
// one.
//
// "compatible" and "responses" declare which upstream dialect an account
// speaks inside the OpenAI family, so both answer to the openai surface.
// Treating them as unknown would drop an account declared
// ["anthropic","compatible"] out of OpenAI routing entirely, which is not what
// the control plane does.
func snapshotProtocols(snap region.ProviderSnapshot) []string {
	out := make([]string, 0, len(snap.Protocols))
	for _, p := range snap.Protocols {
		var canonical string
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "openai", "compatible", "responses":
			canonical = "openai"
		case "anthropic":
			canonical = "anthropic"
		default:
			continue
		}
		if !slices.Contains(out, canonical) {
			out = append(out, canonical)
		}
	}
	if len(out) > 0 {
		return out
	}
	if snap.Kind == "anthropic" {
		return []string{"anthropic"}
	}
	return []string{"openai"}
}

func snapshotSupportsProtocol(snap region.ProviderSnapshot, protocol string) bool {
	return slices.Contains(snapshotProtocols(snap), protocol)
}

// chooseUpstream picks the upstream account to forward this model to. Selection
// is by the snapshot the control plane handed over: protocol support first, then
// exact model membership, with the single-unrestricted-account fallback.
func (s *Service) chooseUpstream(model, protocol string) (cachedUpstream, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.upstreams {
		if !snapshotSupportsProtocol(p.snapshot, protocol) {
			continue
		}
		for _, supported := range p.snapshot.Models {
			if supported == model {
				return p, p.snapshot.UpstreamModels[model], nil
			}
		}
	}
	if len(s.upstreams) == 1 && len(s.upstreams[0].snapshot.Models) == 0 &&
		snapshotSupportsProtocol(s.upstreams[0].snapshot, protocol) {
		return s.upstreams[0], s.upstreams[0].snapshot.UpstreamModels[model], nil
	}
	return cachedUpstream{}, "", fmt.Errorf("no upstream account configured for model %q", model)
}

func joinURL(base, requestPath string) string {
	base = strings.TrimRight(base, "/")
	requestPath = "/" + strings.TrimLeft(requestPath, "/")
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(requestPath, "/v1/") {
		requestPath = strings.TrimPrefix(requestPath, "/v1")
	}
	return base + path.Clean(requestPath)
}
