package edge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Errors of the local Redis ledger.
var (
	ErrAccountNotInitialized = errors.New("regional credit account is not initialized")
	ErrInsufficientCredit    = errors.New("regional credit is insufficient")
	ErrReservationNotFound   = errors.New("regional reservation not found")
	ErrReservationConflict   = errors.New("regional reservation conflicts with an existing request")
)

type AccountState struct {
	Available      int64
	Reserved       int64
	OverdraftLimit int64
	LeaseID        uuid.UUID
	LeaseExpiresAt time.Time
	UpdatedAt      time.Time
}

type Reservation struct {
	RequestID string
	UserID    uuid.UUID
	Amount    int64
	Status    string
	Available int64
	LeaseID   uuid.UUID
}

type Ledger struct {
	rdb        *redis.Client
	prefix     string
	requestTTL time.Duration
}

func NewLedger(rdb *redis.Client, prefix string, requestTTL time.Duration) *Ledger {
	if prefix == "" {
		prefix = "jucode:region"
	}
	if requestTTL <= 0 {
		requestTTL = 30 * time.Minute
	}
	return &Ledger{rdb: rdb, prefix: prefix, requestTTL: requestTTL}
}

func (l *Ledger) Close() error {
	if l == nil || l.rdb == nil {
		return nil
	}
	return l.rdb.Close()
}

func (l *Ledger) accountKey(uid uuid.UUID) string { return l.prefix + ":acct:" + uid.String() }
func (l *Ledger) requestKey(id string) string     { return l.prefix + ":req:" + id }
func (l *Ledger) leaseMarkerKey(id uuid.UUID) string {
	return l.prefix + ":lease:" + id.String()
}

var seedScript = redis.NewScript(`
local account = KEYS[1]
if redis.call('EXISTS', account) == 1 then return 0 end
redis.call('HSET', account,
	'available', ARGV[1],
	'reserved', '0',
	'overdraft_limit', ARGV[2],
	'lease_id', '',
	'lease_expires_at', '0',
	'updated_at', ARGV[3])
return 1
`)

// SeedAccount initializes an edge account exactly once. It intentionally does
// not overwrite a live account; balance reconciliation must be versioned by
// the control plane, otherwise a stale snapshot could refill a user's Redis
// balance after the user has spent it.
func (l *Ledger) SeedAccount(ctx context.Context, uid uuid.UUID, available, overdraft int64) error {
	if uid == uuid.Nil {
		return errors.New("regional account user id is empty")
	}
	_, err := seedScript.Run(ctx, l.rdb, []string{l.accountKey(uid)}, available, overdraft, time.Now().UnixMilli()).Int()
	return err
}

var addLeaseScript = redis.NewScript(`
local account = KEYS[1]
local marker = KEYS[2]
if redis.call('EXISTS', marker) == 1 then return 0 end
redis.call('SET', marker, ARGV[3])
if redis.call('EXISTS', account) == 0 then
  redis.call('HSET', account,
    'available', ARGV[1],
    'reserved', '0',
    'overdraft_limit', ARGV[2],
    'lease_id', ARGV[3],
    'lease_expires_at', ARGV[4],
    'updated_at', ARGV[5])
else
  redis.call('HINCRBY', account, 'available', ARGV[1])
  redis.call('HSET', account,
    'overdraft_limit', ARGV[2],
    'lease_id', ARGV[3],
    'lease_expires_at', ARGV[4],
    'updated_at', ARGV[5])
end
return redis.call('HGET', account, 'available')
`)

// AddLease adds an authoritative credit lease to the local aggregate. The
// lease id is kept on the account so every new reservation can be attributed
// to the exact frozen amount on the control plane.
func (l *Ledger) AddLease(ctx context.Context, uid, leaseID uuid.UUID, available, overdraft int64, expiresAt time.Time) error {
	if uid == uuid.Nil || leaseID == uuid.Nil || available <= 0 || overdraft < 0 {
		return errors.New("invalid regional credit lease")
	}
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Hour)
	}
	_, err := addLeaseScript.Run(ctx, l.rdb, []string{l.accountKey(uid), l.leaseMarkerKey(leaseID)}, available, overdraft, leaseID.String(), expiresAt.UnixMilli(), time.Now().UnixMilli()).Result()
	return err
}

var dropLeaseScript = redis.NewScript(`
local account = KEYS[1]
if redis.call('EXISTS', account) == 0 then return 0 end
redis.call('HINCRBY', account, 'available', -tonumber(ARGV[1]))
if redis.call('HGET', account, 'lease_id') == ARGV[2] then
  redis.call('HSET', account, 'lease_id', '')
end
redis.call('HSET', account, 'updated_at', ARGV[3])
return 1
`)

// DropLease removes a released lease's unspent contribution from the local
// aggregate after the control plane has refunded it. It is intentionally
// called only after the release request succeeds remotely.
func (l *Ledger) DropLease(ctx context.Context, uid, leaseID uuid.UUID, remaining int64) error {
	if uid == uuid.Nil || leaseID == uuid.Nil {
		return errors.New("invalid regional lease release")
	}
	return dropLeaseScript.Run(ctx, l.rdb, []string{l.accountKey(uid)}, remaining, leaseID.String(), time.Now().UnixMilli()).Err()
}

var reserveScript = redis.NewScript(`
local account = KEYS[1]
local request = KEYS[2]
if redis.call('EXISTS', account) == 0 then return {'missing_account'} end

if redis.call('EXISTS', request) == 1 then
  local existing_user = redis.call('HGET', request, 'user_id')
  local existing_amount = redis.call('HGET', request, 'amount')
  local existing_lease = redis.call('HGET', request, 'lease_id') or ''
  if existing_user ~= ARGV[1] or existing_amount ~= ARGV[2] then
    return {'conflict'}
  end
  local available = redis.call('HGET', account, 'available') or '0'
  return {'existing', available, existing_lease}
end

local available = tonumber(redis.call('HGET', account, 'available') or '0')
local overdraft = tonumber(redis.call('HGET', account, 'overdraft_limit') or '0')
local amount = tonumber(ARGV[2])
if available - amount < -overdraft then return {'insufficient', available} end

redis.call('HINCRBY', account, 'available', -amount)
redis.call('HINCRBY', account, 'reserved', amount)
redis.call('HSET', account, 'updated_at', ARGV[4])
redis.call('HSET', request,
  'user_id', ARGV[1],
  'amount', ARGV[2],
  'lease_id', ARGV[3],
  'status', 'reserved',
  'created_at', ARGV[4])
redis.call('EXPIRE', request, ARGV[5])
local next_available = redis.call('HGET', account, 'available')
return {'reserved', next_available, ARGV[3]}
`)

func (l *Ledger) Reserve(ctx context.Context, uid uuid.UUID, requestID string, amount int64, leaseID uuid.UUID) (*Reservation, error) {
	if uid == uuid.Nil || requestID == "" || amount <= 0 {
		return nil, errors.New("invalid regional reservation")
	}
	lease := ""
	if leaseID != uuid.Nil {
		lease = leaseID.String()
	}
	result, err := reserveScript.Run(ctx, l.rdb, []string{l.accountKey(uid), l.requestKey(requestID)}, uid.String(), amount, lease, time.Now().UnixMilli(), int64(l.requestTTL/time.Second)).Result()
	if err != nil {
		return nil, err
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) == 0 {
		return nil, fmt.Errorf("invalid regional reservation response %T", result)
	}
	status := fmt.Sprint(parts[0])
	available := int64(0)
	if len(parts) > 1 {
		available, _ = strconv.ParseInt(fmt.Sprint(parts[1]), 10, 64)
	}
	resultLease := uuid.Nil
	if len(parts) > 2 && fmt.Sprint(parts[2]) != "" {
		resultLease, _ = uuid.Parse(fmt.Sprint(parts[2]))
	}
	switch status {
	case "missing_account":
		return nil, ErrAccountNotInitialized
	case "insufficient":
		return &Reservation{RequestID: requestID, UserID: uid, Amount: amount, Status: status, Available: available, LeaseID: resultLease}, ErrInsufficientCredit
	case "conflict":
		return nil, ErrReservationConflict
	default:
		return &Reservation{RequestID: requestID, UserID: uid, Amount: amount, Status: status, Available: available, LeaseID: resultLease}, nil
	}
}

var settleScript = redis.NewScript(`
local account = KEYS[1]
local request = KEYS[2]
if redis.call('EXISTS', account) == 0 then return {'missing_account'} end
if redis.call('EXISTS', request) == 0 then return {'missing_request'} end

local request_user = redis.call('HGET', request, 'user_id')
if request_user ~= ARGV[1] then return {'conflict'} end
local status = redis.call('HGET', request, 'status') or ''
if status == 'settled' or status == 'released' then
  local available = redis.call('HGET', account, 'available') or '0'
  local actual = redis.call('HGET', request, 'actual') or '0'
  local lease_id = redis.call('HGET', request, 'lease_id') or ''
  return {status, available, actual, 'false', lease_id}
end

local reserved = tonumber(redis.call('HGET', request, 'amount') or '0')
local actual = tonumber(ARGV[2])
local available = tonumber(redis.call('HGET', account, 'available') or '0')
local overdraft = tonumber(redis.call('HGET', account, 'overdraft_limit') or '0')
local next_available = available + reserved - actual

redis.call('HINCRBY', account, 'reserved', -reserved)
redis.call('HINCRBY', account, 'available', reserved - actual)
redis.call('HSET', account, 'updated_at', ARGV[3])
local next_status = 'settled'
if actual == 0 then next_status = 'released' end
redis.call('HSET', request, 'status', next_status, 'actual', ARGV[2], 'settled_at', ARGV[3], 'overdraft_exceeded', tostring(next_available < -overdraft))
local lease_id = redis.call('HGET', request, 'lease_id') or ''
return {next_status, tostring(next_available), ARGV[2], tostring(next_available < -overdraft), lease_id}
`)

type Settlement struct {
	RequestID         string
	UserID            uuid.UUID
	Reserved          int64
	Actual            int64
	Available         int64
	OverdraftExceeded bool
	Status            string
	LeaseID           uuid.UUID
}

func (l *Ledger) Settle(ctx context.Context, uid uuid.UUID, requestID string, actual int64) (*Settlement, error) {
	if uid == uuid.Nil || requestID == "" || actual < 0 {
		return nil, errors.New("invalid regional settlement")
	}
	result, err := settleScript.Run(ctx, l.rdb, []string{l.accountKey(uid), l.requestKey(requestID)}, uid.String(), actual, time.Now().UnixMilli()).Result()
	if err != nil {
		return nil, err
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) == 0 {
		return nil, fmt.Errorf("invalid regional settlement response %T", result)
	}
	status := fmt.Sprint(parts[0])
	if status == "missing_account" {
		return nil, ErrAccountNotInitialized
	}
	if status == "missing_request" {
		return nil, ErrReservationNotFound
	}
	if status == "conflict" {
		return nil, ErrReservationConflict
	}
	available, _ := strconv.ParseInt(fmt.Sprint(parts[1]), 10, 64)
	actualUnits, _ := strconv.ParseInt(fmt.Sprint(parts[2]), 10, 64)
	overdraft := len(parts) > 3 && fmt.Sprint(parts[3]) == "true"
	leaseID := uuid.Nil
	if len(parts) > 4 && fmt.Sprint(parts[4]) != "" {
		leaseID, _ = uuid.Parse(fmt.Sprint(parts[4]))
	}
	return &Settlement{RequestID: requestID, UserID: uid, Actual: actualUnits, Available: available, Status: status, OverdraftExceeded: overdraft, LeaseID: leaseID}, nil
}

func (l *Ledger) Account(ctx context.Context, uid uuid.UUID) (*AccountState, error) {
	values, err := l.rdb.HGetAll(ctx, l.accountKey(uid)).Result()
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, ErrAccountNotInitialized
	}
	parse := func(name string) int64 { n, _ := strconv.ParseInt(values[name], 10, 64); return n }
	updated, _ := strconv.ParseInt(values["updated_at"], 10, 64)
	leaseID := uuid.Nil
	if values["lease_id"] != "" {
		leaseID, _ = uuid.Parse(values["lease_id"])
	}
	expires, _ := strconv.ParseInt(values["lease_expires_at"], 10, 64)
	return &AccountState{Available: parse("available"), Reserved: parse("reserved"), OverdraftLimit: parse("overdraft_limit"), LeaseID: leaseID, LeaseExpiresAt: time.UnixMilli(expires), UpdatedAt: time.UnixMilli(updated)}, nil
}
