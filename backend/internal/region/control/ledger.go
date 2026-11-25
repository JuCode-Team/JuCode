package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/region"
)

// SQLLedger is the default Ledger: it keeps leases and edge usage in the
// regional_credit_leases / regional_usage_events tables (see SchemaSQL) and
// moves money on users.balance / organizations.balance with matching
// balance_txes / organization_balance_txes rows, on the main Postgres.
type SQLLedger struct {
	db     *sql.DB
	policy LeasePolicy
}

func NewSQLLedger(db *sql.DB, policy LeasePolicy) *SQLLedger {
	return &SQLLedger{db: db, policy: policy}
}

// ApplyUsage is the authoritative-side consumer for charges
// produced by jucode-edge. The event table makes retries idempotent; the user
// row lock serializes edge settlement with top-ups and main-site traffic.
// Personal events settle against users; organization events settle against
// organizations. The lease keeps the authoritative balance held until the
// edge has flushed the final usage event, so retries remain idempotent.
func (l *SQLLedger) ApplyUsage(ctx context.Context, events []region.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	for _, event := range events {
		if event.EventID == uuid.Nil || event.UserID == uuid.Nil || event.CostCNY == "" {
			return rollback(errors.New("invalid regional usage event"))
		}
		cost, err := decimal.NewFromString(event.CostCNY)
		if err != nil || cost.Sign() < 0 {
			return rollback(fmt.Errorf("invalid regional event cost %q", event.CostCNY))
		}
		scope := strings.TrimSpace(event.BillingScope)
		if scope == "" {
			scope = region.BillingScopePersonal
		}
		organizationID := uuid.Nil
		if event.OrganizationID != nil {
			organizationID = *event.OrganizationID
		}
		var inserted bool
		// regional_usage_events.provider_id keeps its historical column name but
		// holds an ACCOUNT id — the same id the edge scheduled by (see
		// insertUsageLog below). The column is not renamed because the
		// region wire/table contract is frozen; readers resolve it as an account.
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO regional_usage_events
			  (event_id, request_id, region_id, lease_id, user_id, api_key_id, billing_scope, organization_id,
			   provider_id, model, endpoint, stream, api_key_name, tokens_in, tokens_out, latency_ms, ttft_ms,
			   status, cost_cny, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
			ON CONFLICT (event_id) DO NOTHING
			RETURNING true`, event.EventID, event.RequestID, event.RegionID, event.LeaseID, event.UserID,
			event.APIKeyID, scope, nullableUUID(organizationID), event.ProviderID, event.Model, usageEndpoint(event.Endpoint), event.Stream, event.APIKeyName, event.TokensIn, event.TokensOut, event.LatencyMs, event.TtftMs, event.Status, cost, event.OccurredAt).Scan(&inserted); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// An event recorded by an earlier delivery may predate the
				// regional usage-log bridge. Repair the missing row idempotently.
				if err := insertUsageLog(ctx, tx, event, cost, scope, organizationID); err != nil {
					return rollback(err)
				}
				continue
			}
			return rollback(err)
		}
		if err := insertUsageLog(ctx, tx, event, cost, scope, organizationID); err != nil {
			return rollback(err)
		}
		if !inserted || cost.IsZero() {
			continue
		}
		if event.LeaseID != uuid.Nil {
			var leaseUser uuid.UUID
			var leaseScope, leaseStatus string
			var leaseOrganization sql.NullString
			var granted, spent decimal.Decimal
			if err := tx.QueryRowContext(ctx, `
				SELECT user_id, billing_scope, organization_id, granted_cny, spent_cny, status
				FROM regional_credit_leases WHERE id=$1 FOR UPDATE`, event.LeaseID).
				Scan(&leaseUser, &leaseScope, &leaseOrganization, &granted, &spent, &leaseStatus); err != nil {
				return rollback(err)
			}
			if leaseUser != event.UserID {
				return rollback(errors.New("regional usage lease user mismatch"))
			}
			if strings.TrimSpace(leaseScope) == "" {
				leaseScope = region.BillingScopePersonal
			}
			if scope == region.BillingScopePersonal && leaseScope == region.BillingScopeOrganization {
				scope = leaseScope
			}
			if organizationID == uuid.Nil && leaseOrganization.Valid {
				parsed, parseErr := uuid.Parse(leaseOrganization.String)
				if parseErr != nil {
					return rollback(parseErr)
				}
				organizationID = parsed
			}
			if scope == region.BillingScopeOrganization && organizationID == uuid.Nil {
				return rollback(region.ErrOrganizationForbidden)
			}
			// A lease absorbs at most its granted amount: spent may never
			// exceed granted (there is no implicit overdraft on the hold;
			// personal overdraft at settle time is 0). Anything past the hold
			// — an over-reporting edge, or a lease already expired/released
			// before an outbox retry arrived — is charged directly against
			// the live balance so the final accounting result is preserved
			// without letting an edge mint credit against a small hold.
			leaseShare := decimal.Zero
			if leaseStatus == "active" {
				if room := granted.Sub(spent); room.Sign() > 0 {
					leaseShare = decimal.Min(cost, room)
				}
				if leaseShare.Sign() > 0 {
					if _, err := tx.ExecContext(ctx, `
						UPDATE regional_credit_leases
						SET spent_cny = spent_cny + $1, updated_at = NOW()
						WHERE id=$2`, leaseShare, event.LeaseID); err != nil {
						return rollback(err)
					}
				}
			}
			if overflow := cost.Sub(leaseShare); overflow.Sign() > 0 {
				if scope == region.BillingScopeOrganization {
					var balance decimal.Decimal
					if err := tx.QueryRowContext(ctx, `SELECT balance FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&balance); err != nil {
						return rollback(err)
					}
					newBalance := balance.Sub(overflow)
					if _, err := tx.ExecContext(ctx, `UPDATE organizations SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`, newBalance, organizationID); err != nil {
						return rollback(err)
					}
					if _, err := tx.ExecContext(ctx, `
						INSERT INTO organization_balance_txes
						  (id, created_at, updated_at, organization_id, actor_user_id, kind, amount, balance_after, currency, note)
						VALUES ($1,NOW(),NOW(),$2,$3,'deduct',$4,$5,'CNY',$6)`, uuid.New(), organizationID, event.UserID, overflow.Neg(), newBalance,
						"regional organization charge over lease "+event.RegionID+"/"+event.RequestID); err != nil {
						return rollback(err)
					}
				} else {
					var balance decimal.Decimal
					if err := tx.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1 FOR UPDATE`, event.UserID).Scan(&balance); err != nil {
						return rollback(err)
					}
					newBalance := balance.Sub(overflow)
					if _, err := tx.ExecContext(ctx, `UPDATE users SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`, newBalance, event.UserID); err != nil {
						return rollback(err)
					}
					if _, err := tx.ExecContext(ctx, `
						INSERT INTO balance_txes
						  (id, created_at, updated_at, user_id, kind, amount, balance_after, currency, note)
						VALUES ($1,NOW(),NOW(),$2,'deduct',$3,$4,'CNY',$5)`, uuid.New(), event.UserID, overflow.Neg(), newBalance,
						"regional edge charge over lease "+event.RegionID+"/"+event.RequestID); err != nil {
						return rollback(err)
					}
				}
			}
			continue
		}
		var balance decimal.Decimal
		if err := tx.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1 FOR UPDATE`, event.UserID).Scan(&balance); err != nil {
			return rollback(err)
		}
		newBalance := balance.Sub(cost)
		if _, err := tx.ExecContext(ctx, `UPDATE users SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`, newBalance, event.UserID); err != nil {
			return rollback(err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO balance_txes
			  (id, created_at, updated_at, user_id, kind, amount, balance_after, currency, note)
			VALUES ($1,NOW(),NOW(),$2,'deduct',$3,$4,'CNY',$5)`, uuid.New(), event.UserID, cost.Neg(), newBalance,
			"regional edge charge "+event.RegionID+"/"+event.RequestID); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func usageEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "chat_completions"
	}
	return endpoint
}

// insertUsageLog mirrors the fields that are available at the edge
// boundary. It runs on the same SQL transaction as regional_usage_events, so
// a retry cannot charge twice or leave a charged event invisible in Usage.
// request_id is also checked because the first implementation of regional
// events did not use the event UUID as the usage-log UUID.
//
// Region.UsageEvent.ProviderID keeps its historical wire name but carries an
// ACCOUNT id: the edge schedules by the id in the synchronized snapshot
// (biz.AccountPoolSvc.RegionalSnapshot), which is an upstream account id. This
// is therefore the one place the authoritative side resolves an edge-reported
// id, and it resolves it as an account — provider_account_id is written from it
// and provider_id only from that account's legacy_provider_id, so the account
// stays the primary attribution and pre-migration grouping is preserved.
//
// usageInsertSQL writes one edge-reported request into usage_logs.
//
// The column list and the SELECT list must line up one-for-one: Postgres only
// reports "INSERT has more expressions than target columns" at run time, and
// this statement failed that way in production (a stray literal), so
// TestUsageInsertArity keeps the two lists — and the parameter count —
// honest without a database.
const usageInsertSQL = `
		INSERT INTO usage_logs
		  (id, created_at, updated_at, request_id, endpoint, model, upstream_model,
		   tokens_in, tokens_out, cached_tokens_in, cache_creation_tokens_in,
		   image_tokens_in, image_tokens_out, cache_write_estimated, output_images,
		   cost_base, cost_final, rate_multiplier_applied, currency, billing_source,
		   status, http_status, latency_ms, ttft_ms, retry_count, provider_chain,
		   error_message, api_key_id, provider_id, provider_account_id, user_id, organization_id,
		   ip, user_agent, reasoning_effort, stream, api_key_name)
		SELECT $1::uuid,$2::timestamptz,$2::timestamptz,$3::varchar,$4::varchar,$5::varchar,'',$15::bigint,$16::bigint,0,0,0,0,false,0,
		       $6::numeric,$6::numeric,1.0,'CNY',$7::varchar,$8::varchar,200,$17::bigint,$18::bigint,0,'[]'::jsonb,'',
		       $9::uuid,
		       (SELECT a.legacy_provider_id FROM upstream_accounts a WHERE a.id = $10::uuid),
		       $10::uuid,
		       $11::uuid,$12::uuid,'','','',$13::boolean,$14::varchar
		WHERE NOT EXISTS (
			SELECT 1 FROM usage_logs WHERE request_id=$3::varchar AND request_id <> ''
		)
		ON CONFLICT (id) DO NOTHING`

func insertUsageLog(ctx context.Context, tx *sql.Tx, event region.UsageEvent, cost decimal.Decimal, scope string, organizationID uuid.UUID) error {
	if event.RequestID != "" {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM usage_logs WHERE request_id=$1 AND request_id <> '')`, event.RequestID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
	}
	billingSource := "balance"
	if scope == region.BillingScopeOrganization || organizationID != uuid.Nil {
		billingSource = "organization_balance"
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	status := strings.TrimSpace(event.Status)
	if status == "" {
		status = "ok"
	}
	_, err := tx.ExecContext(ctx, usageInsertSQL,
		usageInsertArgs(event, occurredAt, cost, billingSource, status, organizationID)...)
	return err
}

// usageInsertArgs is the positional bind list for
// usageInsertSQL. It lives in its own function so the arity test can
// compare it against the statement's placeholders without a database.
func usageInsertArgs(event region.UsageEvent, occurredAt time.Time, cost decimal.Decimal,
	billingSource, status string, organizationID uuid.UUID) []any {
	return []any{
		event.EventID, occurredAt, event.RequestID, usageEndpoint(event.Endpoint), event.Model,
		cost, billingSource, status, nullableUUID(event.APIKeyID), nullableUUID(event.ProviderID), event.UserID,
		nullableUUID(organizationID), event.Stream, event.APIKeyName, event.TokensIn, event.TokensOut, event.LatencyMs, event.TtftMs,
	}
}

// ReleaseExpiredRegionalLeases is a safety net for an edge that has been
// offline long enough to miss the normal release path. Late usage events are
// charged directly by ApplyUsage, so expiring a lease cannot
// silently turn a completed request into free usage.
func (l *SQLLedger) ReleaseExpired(ctx context.Context, limit int) error {
	if limit <= 0 {
		limit = 100
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT id FROM regional_credit_leases
		WHERE status='active' AND expires_at <= NOW()
		ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := l.ReleaseLease(ctx, "", id); err != nil {
			return err
		}
	}
	return nil
}

func (l *SQLLedger) StartCleanup(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		cleanup := func() {
			if err := l.ReleaseExpired(ctx, 100); err != nil {
				// The leader loop is best-effort; normal usage sync remains
				// available if this maintenance query is temporarily down.
				slog.Warn("regional lease cleanup failed", "err", err)
			}
		}
		cleanup()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cleanup()
			}
		}
	}()
}

// GrantLease freezes a bounded slice of the authoritative balance.
// Personal keys lock users.balance; organization keys lock organizations.balance
// and carry the organization scope through the lease for later settlement.
func (l *SQLLedger) GrantLease(ctx context.Context, req region.CreditLeaseRequest) (*region.CreditLease, error) {
	req.RequestID = strings.TrimSpace(req.RequestID)
	req.RegionID = strings.TrimSpace(req.RegionID)
	scope := strings.TrimSpace(req.BillingScope)
	if scope == "" {
		scope = region.BillingScopePersonal
	}
	if req.UserID == uuid.Nil || req.RegionID == "" || req.RequestID == "" {
		return nil, errors.New("invalid regional lease identity")
	}
	if scope != region.BillingScopePersonal && scope != region.BillingScopeOrganization {
		return nil, errors.New("invalid regional billing scope")
	}
	organizationID := uuid.Nil
	if req.OrganizationID != nil {
		organizationID = *req.OrganizationID
	}
	if scope == region.BillingScopeOrganization && organizationID == uuid.Nil {
		return nil, region.ErrOrganizationForbidden
	}
	if scope == region.BillingScopePersonal && organizationID != uuid.Nil {
		return nil, errors.New("personal regional lease cannot have an organization")
	}

	requested, err := decimal.NewFromString(req.RequestedCNY)
	if err != nil || !requested.GreaterThan(decimal.Zero) {
		return nil, errors.New("invalid regional lease amount")
	}
	terms := l.policy.Terms(req)
	if !terms.MaxCNY.GreaterThan(decimal.Zero) {
		return nil, errors.New("invalid regional lease cap")
	}
	if requested.GreaterThan(terms.MaxCNY) {
		requested = terms.MaxCNY
	}
	overdraft := terms.OverdraftCNY
	if overdraft.Sign() < 0 {
		return nil, errors.New("invalid regional overdraft")
	}
	ttl := terms.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*region.CreditLease, error) { _ = tx.Rollback(); return nil, e }
	headroom, err := validateLeaseKey(ctx, tx, req, scope, organizationID)
	if err != nil {
		return rollback(err)
	}
	// The key's daily/monthly/total cost limits bound how much credit may be
	// frozen for it: without this, an edge lease would be a way around the
	// limits the main gateway enforces per request.
	if headroom != nil {
		if headroom.Sign() <= 0 {
			return rollback(region.ErrApiKeyCostLimit)
		}
		if requested.GreaterThan(*headroom) {
			requested = *headroom
		}
	}
	loadExisting := func() (*region.CreditLease, error) {
		var existing region.CreditLease
		var granted, spent decimal.Decimal
		var apiKeyText, organizationText sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT id, user_id, api_key_id, billing_scope, organization_id, region_id, request_id,
			       granted_cny, spent_cny, status, expires_at, created_at
			FROM regional_credit_leases WHERE region_id=$1 AND request_id=$2 FOR UPDATE`, req.RegionID, req.RequestID).
			Scan(&existing.ID, &existing.UserID, &apiKeyText, &existing.BillingScope, &organizationText,
				&existing.RegionID, &existing.RequestID, &granted, &spent, &existing.Status, &existing.ExpiresAt, &existing.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if apiKeyText.Valid {
			existing.APIKeyID, _ = uuid.Parse(apiKeyText.String)
		}
		if organizationText.Valid {
			parsed, parseErr := uuid.Parse(organizationText.String)
			if parseErr != nil {
				return nil, parseErr
			}
			existing.OrganizationID = &parsed
		}
		if strings.TrimSpace(existing.BillingScope) == "" {
			existing.BillingScope = region.BillingScopePersonal
		}
		existing.GrantedCNY = granted.String()
		existing.SpentCNY = spent.String()
		existing.RemainingCNY = granted.Sub(spent).String()
		return &existing, nil
	}
	validateExisting := func(existing *region.CreditLease) error {
		if existing.UserID != req.UserID || existing.BillingScope != scope {
			return errors.New("regional lease request identity conflict")
		}
		if scope == region.BillingScopeOrganization && (existing.OrganizationID == nil || *existing.OrganizationID != organizationID) {
			return errors.New("regional lease organization conflict")
		}
		if existing.Status != "active" {
			return region.ErrLeaseInsufficient
		}
		return nil
	}
	if existing, err := loadExisting(); err != nil {
		return rollback(err)
	} else if existing != nil {
		if err := validateExisting(existing); err != nil {
			return rollback(err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}

	var balance decimal.Decimal
	if scope == region.BillingScopeOrganization {
		var monthlyLimit decimal.Decimal
		if err := tx.QueryRowContext(ctx, `SELECT balance, monthly_spend_limit FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&balance, &monthlyLimit); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rollback(region.ErrOrganizationForbidden)
			}
			return rollback(err)
		}
		var memberLimit, memberTotalLimit decimal.Decimal
		if err := tx.QueryRowContext(ctx, `SELECT monthly_spend_limit, total_spend_limit FROM organization_members WHERE organization_id=$1 AND user_id=$2`, organizationID, req.UserID).Scan(&memberLimit, &memberTotalLimit); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rollback(region.ErrOrganizationForbidden)
			}
			return rollback(err)
		}
		orgUsed, err := organizationUsage(ctx, tx, organizationID, nil)
		if err != nil {
			return rollback(err)
		}
		if monthlyLimit.Sign() > 0 && orgUsed.Add(requested).GreaterThan(monthlyLimit) {
			return rollback(region.ErrOrganizationLimit)
		}
		memberUsed, err := organizationUsage(ctx, tx, organizationID, &req.UserID)
		if err != nil {
			return rollback(err)
		}
		if memberLimit.Sign() > 0 && memberUsed.Add(requested).GreaterThan(memberLimit) {
			return rollback(region.ErrMemberLimit)
		}
		// The cumulative cap has to be enforced here too, or the edge is a
		// way around it: a member at their lifetime limit could keep leasing
		// regional credit indefinitely.
		if memberTotalLimit.Sign() > 0 {
			memberLifetime, err := organizationLifetimeUsage(ctx, tx, organizationID, req.UserID)
			if err != nil {
				return rollback(err)
			}
			if memberLifetime.Add(requested).GreaterThan(memberTotalLimit) {
				return rollback(region.ErrMemberLimit)
			}
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1 FOR UPDATE`, req.UserID).Scan(&balance); err != nil {
		return rollback(err)
	}

	// A duplicate request may have committed while this transaction waited
	// for the account row. Re-check before creating another hold.
	if existing, err := loadExisting(); err != nil {
		return rollback(err)
	} else if existing != nil {
		if err := validateExisting(existing); err != nil {
			return rollback(err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	newBalance := balance.Sub(requested)
	if newBalance.LessThan(overdraft.Neg()) {
		return rollback(region.ErrLeaseInsufficient)
	}

	leaseID := uuid.New()
	expiresAt := time.Now().Add(ttl)
	var insertedID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO regional_credit_leases
		  (id, user_id, api_key_id, billing_scope, organization_id, region_id, request_id,
		   granted_cny, spent_cny, status, expires_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,0,'active',$9,NOW(),NOW())
		ON CONFLICT (region_id, request_id) WHERE request_id <> '' DO NOTHING
		RETURNING id`, leaseID, req.UserID, nullableUUID(req.APIKeyID), scope, nullableUUID(organizationID), req.RegionID,
		req.RequestID, requested, expiresAt).Scan(&insertedID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rollback(err)
	}
	if insertedID == uuid.Nil {
		existing, loadErr := loadExisting()
		if loadErr != nil || existing == nil {
			if loadErr == nil {
				loadErr = errors.New("regional lease request conflict")
			}
			return rollback(loadErr)
		}
		if err := validateExisting(existing); err != nil {
			return rollback(err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if scope == region.BillingScopeOrganization {
		if _, err := tx.ExecContext(ctx, `UPDATE organizations SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`, newBalance, organizationID); err != nil {
			return rollback(err)
		}
		// Every organizations.balance movement must have a matching ledger
		// row, or the billing summary cannot be reconciled against the
		// balance. The hold is an 'adjust' (not 'deduct') so the monthly
		// spend aggregation, which sums deducts, does not count frozen
		// credit as consumed.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO organization_balance_txes
			  (id, created_at, updated_at, organization_id, actor_user_id, kind, amount, balance_after, currency, note)
			VALUES ($1,NOW(),NOW(),$2,$3,'adjust',$4,$5,'CNY',$6)`, uuid.New(), organizationID, req.UserID, requested.Neg(), newBalance,
			"regional credit lease hold "+req.RegionID+"/"+leaseID.String()); err != nil {
			return rollback(err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`, newBalance, req.UserID); err != nil {
			return rollback(err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO balance_txes
			  (id, created_at, updated_at, user_id, kind, amount, balance_after, currency, note)
			VALUES ($1,NOW(),NOW(),$2,'adjust',$3,$4,'CNY',$5)`, uuid.New(), req.UserID, requested.Neg(), newBalance,
			"regional credit lease hold "+req.RegionID+"/"+leaseID.String()); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &region.CreditLease{
		ID: leaseID, UserID: req.UserID, APIKeyID: req.APIKeyID, BillingScope: scope,
		OrganizationID: nullableUUIDPtr(organizationID), RegionID: req.RegionID, RequestID: req.RequestID,
		GrantedCNY: requested.String(), SpentCNY: "0", RemainingCNY: requested.String(),
		Status: "active", ExpiresAt: expiresAt, CreatedAt: time.Now(),
	}, nil
}

// validateLeaseKey checks that the API key exists, is live, and
// matches the requested billing scope. The key row is locked so concurrent
// grants for the same key serialize their cost-limit headroom computation.
// It returns the smallest remaining cost-limit headroom across the key's
// daily/monthly/total limits, counting both settled usage and the unspent
// part of the key's active leases; nil means the key has no cost limits.
func validateLeaseKey(ctx context.Context, tx *sql.Tx, req region.CreditLeaseRequest, scope string, organizationID uuid.UUID) (*decimal.Decimal, error) {
	if scope == region.BillingScopeOrganization && req.APIKeyID == uuid.Nil {
		return nil, region.ErrOrganizationForbidden
	}
	if req.APIKeyID == uuid.Nil {
		return nil, nil
	}
	var actualScope string
	var actualOrganization sql.NullString
	var dailyLimit, monthlyLimit, totalLimit decimal.NullDecimal
	if err := tx.QueryRowContext(ctx, `
		SELECT billing_scope, organization_id, daily_cost_limit, monthly_cost_limit, total_cost_limit
		FROM api_keys
		WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())
		FOR UPDATE`, req.APIKeyID, req.UserID).
		Scan(&actualScope, &actualOrganization, &dailyLimit, &monthlyLimit, &totalLimit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, region.ErrOrganizationForbidden
		}
		return nil, err
	}
	if strings.TrimSpace(actualScope) == "" {
		actualScope = region.BillingScopePersonal
	}
	if actualScope != scope {
		return nil, region.ErrOrganizationForbidden
	}
	if scope == region.BillingScopeOrganization {
		if !actualOrganization.Valid {
			return nil, region.ErrOrganizationForbidden
		}
		actualID, err := uuid.Parse(actualOrganization.String)
		if err != nil || actualID != organizationID {
			return nil, region.ErrOrganizationForbidden
		}
	}
	limited := func(l decimal.NullDecimal) bool { return l.Valid && l.Decimal.Sign() > 0 }
	if !limited(dailyLimit) && !limited(monthlyLimit) && !limited(totalLimit) {
		return nil, nil
	}
	dayStart, monthStart := biz.CalendarPeriods(time.Now())
	var dailyUsed, monthlyUsed, totalUsed decimal.Decimal
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_final) FILTER (WHERE created_at >= $2),0),
		       COALESCE(SUM(cost_final) FILTER (WHERE created_at >= $3),0),
		       COALESCE(SUM(cost_final),0)
		FROM usage_logs WHERE api_key_id=$1 AND status='ok'`, req.APIKeyID, dayStart, monthStart).
		Scan(&dailyUsed, &monthlyUsed, &totalUsed); err != nil {
		return nil, err
	}
	// Unspent active holds count against the limits too, otherwise a key
	// could keep leasing new slices before any settle lands in usage_logs.
	var pendingHold decimal.Decimal
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(GREATEST(granted_cny - spent_cny, 0)), 0)
		FROM regional_credit_leases
		WHERE api_key_id=$1 AND status='active' AND expires_at > NOW()`, req.APIKeyID).
		Scan(&pendingHold); err != nil {
		return nil, err
	}
	var headroom *decimal.Decimal
	consider := func(limit decimal.NullDecimal, used decimal.Decimal) {
		if !limited(limit) {
			return
		}
		room := limit.Decimal.Sub(used).Sub(pendingHold)
		if headroom == nil || room.LessThan(*headroom) {
			headroom = &room
		}
	}
	consider(dailyLimit, dailyUsed)
	consider(monthlyLimit, monthlyUsed)
	consider(totalLimit, totalUsed)
	return headroom, nil
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullableUUIDPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func organizationUsage(ctx context.Context, tx *sql.Tx, organizationID uuid.UUID, userID *uuid.UUID) (decimal.Decimal, error) {
	// The month starts at a UTC boundary computed here, like the main
	// gateway's organizationMonthSpend: date_trunc('month', NOW()) would
	// follow the connection's TimeZone setting.
	usageQuery := `
		SELECT COALESCE(SUM(ABS(amount)), 0)
		FROM organization_balance_txes
		WHERE organization_id=$1 AND kind='deduct'
		  AND created_at >= $2`
	usageArgs := []any{organizationID, monthStartUTC(time.Now())}
	if userID != nil {
		usageQuery += ` AND actor_user_id=$3`
		usageArgs = append(usageArgs, *userID)
	}
	var used decimal.Decimal
	if err := tx.QueryRowContext(ctx, usageQuery, usageArgs...).Scan(&used); err != nil {
		return decimal.Zero, err
	}
	leaseQuery := `
		SELECT COALESCE(SUM(GREATEST(granted_cny - spent_cny, 0)), 0)
		FROM regional_credit_leases
		WHERE organization_id=$1 AND billing_scope='organization'
		  AND status='active' AND expires_at > NOW()`
	leaseArgs := []any{organizationID}
	if userID != nil {
		leaseQuery += ` AND user_id=$2`
		leaseArgs = append(leaseArgs, *userID)
	}
	var pending decimal.Decimal
	if err := tx.QueryRowContext(ctx, leaseQuery, leaseArgs...).Scan(&pending); err != nil {
		return decimal.Zero, err
	}
	return used.Add(pending), nil
}

func monthStartUTC(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// organizationLifetimeUsage is organizationUsage without the
// month window: all-time member spend plus the unspent part of their active
// leases, for the cumulative member cap.
func organizationLifetimeUsage(ctx context.Context, tx *sql.Tx, organizationID, userID uuid.UUID) (decimal.Decimal, error) {
	var used decimal.Decimal
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(ABS(amount)), 0)
		FROM organization_balance_txes
		WHERE organization_id=$1 AND kind='deduct' AND actor_user_id=$2`,
		organizationID, userID).Scan(&used); err != nil {
		return decimal.Zero, err
	}
	var pending decimal.Decimal
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(GREATEST(granted_cny - spent_cny, 0)), 0)
		FROM regional_credit_leases
		WHERE organization_id=$1 AND user_id=$2 AND billing_scope='organization'
		  AND status='active' AND expires_at > NOW()`,
		organizationID, userID).Scan(&pending); err != nil {
		return decimal.Zero, err
	}
	return used.Add(pending), nil
}

// ReleaseLease returns the unused part of a frozen lease. Organization
// leases refund organizations.balance and record only the actual spend as an
// organization deduction; the temporary hold itself is not a billable tx.
func (l *SQLLedger) ReleaseLease(ctx context.Context, regionID string, leaseID uuid.UUID) error {
	if leaseID == uuid.Nil {
		return errors.New("regional lease id is empty")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	var userID uuid.UUID
	var scope string
	var organizationText sql.NullString
	var granted, spent decimal.Decimal
	var status, leaseRegion string
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id, billing_scope, organization_id, granted_cny, spent_cny, status, region_id
		FROM regional_credit_leases WHERE id=$1 FOR UPDATE`, leaseID).
		Scan(&userID, &scope, &organizationText, &granted, &spent, &status, &leaseRegion); err != nil {
		return rollback(err)
	}
	if regionID != "" && leaseRegion != regionID {
		return rollback(region.ErrRegionMismatch)
	}
	if strings.TrimSpace(scope) == "" {
		scope = region.BillingScopePersonal
	}
	if status != "active" {
		return tx.Commit()
	}
	refund := granted.Sub(spent)
	if scope == region.BillingScopeOrganization {
		if !organizationText.Valid {
			return rollback(region.ErrOrganizationForbidden)
		}
		organizationID, parseErr := uuid.Parse(organizationText.String)
		if parseErr != nil {
			return rollback(parseErr)
		}
		var balance decimal.Decimal
		if err := tx.QueryRowContext(ctx, `SELECT balance FROM organizations WHERE id=$1 FOR UPDATE`, organizationID).Scan(&balance); err != nil {
			return rollback(err)
		}
		newBalance := balance.Add(refund)
		if _, err := tx.ExecContext(ctx, `UPDATE organizations SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`, newBalance, organizationID); err != nil {
			return rollback(err)
		}
		// Mirror the hold written at grant time: release the full hold back
		// on the ledger, then record the actual spend as the settle
		// deduction. The two rows sum to the real balance movement (+refund)
		// and keep the running balance_after auditable.
		if !granted.IsZero() {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO organization_balance_txes
				  (id, created_at, updated_at, organization_id, actor_user_id, kind, amount, balance_after, currency, note)
				VALUES ($1,NOW(),NOW(),$2,$3,'adjust',$4,$5,'CNY',$6)`, uuid.New(), organizationID, userID, granted, balance.Add(granted),
				"regional credit lease release "+leaseID.String()); err != nil {
				return rollback(err)
			}
		}
		if !spent.IsZero() {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO organization_balance_txes
				  (id, created_at, updated_at, organization_id, actor_user_id, kind, amount, balance_after, currency, note)
				VALUES ($1,NOW(),NOW(),$2,$3,'deduct',$4,$5,'CNY',$6)`, uuid.New(), organizationID, userID, spent.Neg(), newBalance,
				"regional organization charge "+leaseID.String()); err != nil {
				return rollback(err)
			}
		}
	} else {
		var balance decimal.Decimal
		if err := tx.QueryRowContext(ctx, `SELECT balance FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&balance); err != nil {
			return rollback(err)
		}
		newBalance := balance.Add(refund)
		if _, err := tx.ExecContext(ctx, `UPDATE users SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`, newBalance, userID); err != nil {
			return rollback(err)
		}
		if !refund.IsZero() {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO balance_txes
				  (id, created_at, updated_at, user_id, kind, amount, balance_after, currency, note)
				VALUES ($1,NOW(),NOW(),$2,'adjust',$3,$4,'CNY',$5)`, uuid.New(), userID, refund, newBalance,
				"regional credit lease release "+leaseID.String()); err != nil {
				return rollback(err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE regional_credit_leases
		SET status='closed', updated_at=NOW(), closed_at=NOW()
		WHERE id=$1`, leaseID); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}
