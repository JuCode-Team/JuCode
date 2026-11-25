// Package region is the wire protocol between a control plane (the main
// server, see package control) and its edge nodes (jucode-edge, see package
// edge): request and response types, error codes, money units and the HTTP
// client an edge uses. The regional edge feature is experimental.
// None of the types in this package contain plaintext provider credentials.
//
// Naming note: several types and JSON keys here still say "provider". The wire
// contract is frozen — a non-upgraded edge must keep working byte-for-byte —
// but the entity behind those names is the upstream ACCOUNT
// (ent.UpstreamAccount). In particular ProviderSnapshot.ID and
// UsageEvent.ProviderID both carry an account id.
package region

import (
	"time"

	"github.com/google/uuid"
)

// The control-plane API. Every endpoint is a POST with a JSON body and a
// JSON response, authenticated with "Authorization: Bearer <service token>".
// The version is the path: fields may be added within v1 (old readers ignore
// them), anything an existing edge could not parse needs /v2.
const (
	APIPrefix = "/internal/region/v1"

	PathProviderSnapshot   = "/provider-snapshot"    // ProviderSnapshotRequest → ProviderSnapshotResponse
	PathAccountSnapshot    = "/account-snapshot"     // AccountSnapshotRequest → AccountSnapshotResponse
	PathCreditLease        = "/credit-lease"         // CreditLeaseRequest → CreditLeaseResponse
	PathCreditLeaseRelease = "/credit-lease/release" // CreditLeaseReleaseRequest → CreditLeaseReleaseResponse
	PathUsageEvents        = "/usage-events"         // UsageEventsRequest → UsageEventsResponse

	// MaxUsageEventsPerBatch bounds one usage-events request.
	MaxUsageEventsPerBatch = 500
)

const (
	BillingScopePersonal     = "personal"
	BillingScopeOrganization = "organization"
)

// ProviderSnapshotRequest asks for upstream accounts. ProviderIDs (account
// ids, historical key) narrows the answer; empty means all of them.
type ProviderSnapshotRequest struct {
	ProviderIDs []string `json:"provider_ids,omitempty"`
}

// AccountSnapshotRequest resolves an API key by the lowercase hex SHA-256 of
// the key the client sent to the edge.
type AccountSnapshotRequest struct {
	APIKeyHash string `json:"api_key_hash"`
}

// ProviderSnapshot is one upstream ACCOUNT as an edge node needs it: endpoint,
// dialect, credential envelope and the model/price bindings the edge schedules
// by. The name and the JSON keys are historical (the edge protocol must not
// change); ID is the account id — see biz.AccountPoolSvc.RegionalSnapshot.
// APIKeyEnvelope is encrypted with the separately managed regional sync key.
type ProviderSnapshot struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Kind    string    `json:"kind"`
	BaseURL string    `json:"base_url"`
	// Protocols mirrors the account's native protocols: client-facing wire
	// protocols the upstream accepts. Empty = derive from Kind, matching the
	// main gateway.
	Protocols      []string `json:"protocols,omitempty"`
	APIFormat      string   `json:"api_format,omitempty"`
	Organization   string   `json:"organization,omitempty"`
	APIKeyEnvelope string   `json:"api_key_envelope"`
	// AnthropicAPIKeyEnvelope is the optional Anthropic-protocol override.
	// Empty = the edge uses APIKeyEnvelope (the default key).
	AnthropicAPIKeyEnvelope string                `json:"anthropic_api_key_envelope,omitempty"`
	Enabled                 bool                  `json:"enabled"`
	Weight                  int                   `json:"weight"`
	Models                  []string              `json:"models,omitempty"`
	UpstreamModels          map[string]string     `json:"upstream_models,omitempty"`
	Prices                  map[string]ModelPrice `json:"prices,omitempty"`
	Version                 int64                 `json:"version"`
	UpdatedAt               time.Time             `json:"updated_at"`
}

type ModelPrice struct {
	// Values are CNY per 1K tokens. Account snapshots contain final user
	// prices after the selected model-group multiplier is applied.
	BillingMode       string `json:"billing_mode,omitempty"`
	InputPer1K        string `json:"input_per_1k,omitempty"`
	OutputPer1K       string `json:"output_per_1k,omitempty"`
	CacheReadPer1K    string `json:"cache_read_per_1k,omitempty"`
	CacheWritePer1K   string `json:"cache_write_per_1k,omitempty"`
	CacheWrite1hPer1K string `json:"cache_write_1h_per_1k,omitempty"`
	// Audio*Per1K price the audio modality. Empty falls back to the text
	// price of the same side, matching the control plane. An older edge
	// build ignores these and bills audio at the text rate, which is what
	// it did before the modality was split out.
	AudioInputPer1K  string           `json:"audio_input_per_1k,omitempty"`
	AudioOutputPer1K string           `json:"audio_output_per_1k,omitempty"`
	PerRequestCNY    string           `json:"per_request_cny,omitempty"`
	PriceTiers       []ModelPriceTier `json:"price_tiers,omitempty"`
}

type ModelPriceTier struct {
	UpToTokens        int64  `json:"up_to_tokens"`
	InputPer1K        string `json:"input_per_1k,omitempty"`
	OutputPer1K       string `json:"output_per_1k,omitempty"`
	CacheReadPer1K    string `json:"cache_read_per_1k,omitempty"`
	CacheWritePer1K   string `json:"cache_write_per_1k,omitempty"`
	CacheWrite1hPer1K string `json:"cache_write_1h_per_1k,omitempty"`
}

// AccountSnapshot contains only the data needed to authenticate and apply
// account policy at the edge. The API key plaintext is never synchronized.
type AccountSnapshot struct {
	Version             int64                 `json:"version"`
	UserID              uuid.UUID             `json:"user_id"`
	APIKeyID            uuid.UUID             `json:"api_key_id"`
	APIKeyHash          string                `json:"api_key_hash"`
	APIKeyName          string                `json:"api_key_name,omitempty"`
	BillingScope        string                `json:"billing_scope,omitempty"`
	OrganizationID      *uuid.UUID            `json:"organization_id,omitempty"`
	Status              string                `json:"status"`
	Revoked             bool                  `json:"revoked"`
	ExpiresAt           *time.Time            `json:"expires_at,omitempty"`
	AllowedGroupIDs     []uuid.UUID           `json:"allowed_group_ids,omitempty"`
	AllowedModels       []string              `json:"allowed_models,omitempty"`
	ModelPrices         map[string]ModelPrice `json:"model_prices,omitempty"`
	IPWhitelist         []string              `json:"ip_whitelist,omitempty"`
	BalanceCNY          string                `json:"balance_cny"`
	OverdraftCNY        string                `json:"overdraft_cny,omitempty"`
	DailyCostLimitCNY   string                `json:"daily_cost_limit_cny,omitempty"`
	MonthlyCostLimitCNY string                `json:"monthly_cost_limit_cny,omitempty"`
	TotalCostLimitCNY   string                `json:"total_cost_limit_cny,omitempty"`
	UpdatedAt           time.Time             `json:"updated_at"`
}

// ProviderSnapshotResponse is the snapshot payload. `Providers` keeps its
// historical name/JSON key; each element's ID is an account id.
type ProviderSnapshotResponse struct {
	Version     int64              `json:"version"`
	Providers   []ProviderSnapshot `json:"providers"`
	GeneratedAt time.Time          `json:"generated_at"`
}

type AccountSnapshotResponse struct {
	Account AccountSnapshot `json:"account"`
}

type CreditLease struct {
	ID             uuid.UUID  `json:"id"`
	UserID         uuid.UUID  `json:"user_id"`
	APIKeyID       uuid.UUID  `json:"api_key_id,omitempty"`
	BillingScope   string     `json:"billing_scope,omitempty"`
	OrganizationID *uuid.UUID `json:"organization_id,omitempty"`
	RegionID       string     `json:"region_id"`
	RequestID      string     `json:"request_id,omitempty"`
	GrantedCNY     string     `json:"granted_cny"`
	SpentCNY       string     `json:"spent_cny"`
	RemainingCNY   string     `json:"remaining_cny"`
	Status         string     `json:"status"`
	ExpiresAt      time.Time  `json:"expires_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

type CreditLeaseRequest struct {
	UserID         uuid.UUID  `json:"user_id"`
	APIKeyID       uuid.UUID  `json:"api_key_id,omitempty"`
	BillingScope   string     `json:"billing_scope,omitempty"`
	OrganizationID *uuid.UUID `json:"organization_id,omitempty"`
	RegionID       string     `json:"region_id"`
	RequestID      string     `json:"request_id"`
	RequestedCNY   string     `json:"requested_cny"`
}

type CreditLeaseResponse struct {
	Lease CreditLease `json:"lease"`
}

type CreditLeaseReleaseRequest struct {
	LeaseID uuid.UUID `json:"lease_id"`
}

type CreditLeaseReleaseResponse struct {
	Released bool `json:"released"`
}

// UsageEvent is the durable accounting event emitted by an edge. CostCNY is
// a decimal string to avoid JSON floating point loss.
//
// It is request metadata only. It must never carry request or response
// content (prompts, messages, completions, tool arguments): the control plane
// rejects unknown fields, and nothing here is a place to put a body.
type UsageEvent struct {
	EventID        uuid.UUID  `json:"event_id"`
	RequestID      string     `json:"request_id"`
	RegionID       string     `json:"region_id"`
	LeaseID        uuid.UUID  `json:"lease_id"`
	UserID         uuid.UUID  `json:"user_id"`
	APIKeyID       uuid.UUID  `json:"api_key_id"`
	BillingScope   string     `json:"billing_scope,omitempty"`
	OrganizationID *uuid.UUID `json:"organization_id,omitempty"`
	// ProviderID keeps its historical wire name and carries an ACCOUNT id: it is
	// the id the edge scheduled by, out of the snapshot it synchronized. The main
	// site resolves it as an account (see control.insertUsageLog)
	// and writes provider_account_id from it.
	ProviderID uuid.UUID `json:"provider_id"`
	Model      string    `json:"model"`
	// Endpoint and Stream preserve the gateway surface in the authoritative
	// usage log. Older edge binaries may omit them; the control plane falls
	// back to chat_completions for those events.
	Endpoint    string    `json:"endpoint,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	APIKeyName  string    `json:"api_key_name,omitempty"`
	TokensIn    int       `json:"tokens_in,omitempty"`
	TokensOut   int       `json:"tokens_out,omitempty"`
	LatencyMs   int       `json:"latency_ms,omitempty"`
	TtftMs      *int      `json:"ttft_ms,omitempty"`
	Status      string    `json:"status"`
	ReservedCNY string    `json:"reserved_cny"`
	CostCNY     string    `json:"cost_cny"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type UsageEventsRequest struct {
	Events []UsageEvent `json:"events"`
}

type UsageEventsResponse struct {
	Accepted int `json:"accepted"`
}
