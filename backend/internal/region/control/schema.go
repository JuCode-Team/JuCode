package control

// SchemaSQL creates the two tables the SQL ledger writes outside Ent.
// jucode-migrate applies it; every statement is idempotent.
//
// regional_usage_events.provider_id keeps its historical name and holds an
// upstream ACCOUNT id (see region.UsageEvent.ProviderID).
const SchemaSQL = `
CREATE TABLE IF NOT EXISTS regional_usage_events (
	 event_id uuid PRIMARY KEY,
	 request_id varchar NOT NULL DEFAULT '',
	 region_id varchar NOT NULL DEFAULT '',
	 lease_id uuid,
	 user_id uuid NOT NULL,
	 api_key_id uuid,
	 billing_scope varchar NOT NULL DEFAULT 'personal',
	 organization_id uuid,
	 provider_id uuid,
	 model varchar NOT NULL DEFAULT '',
	 endpoint varchar NOT NULL DEFAULT 'chat_completions',
	 stream boolean NOT NULL DEFAULT false,
	 api_key_name varchar NOT NULL DEFAULT '',
	 tokens_in bigint NOT NULL DEFAULT 0,
	 tokens_out bigint NOT NULL DEFAULT 0,
	 latency_ms bigint NOT NULL DEFAULT 0,
	 ttft_ms bigint,
	 status varchar NOT NULL DEFAULT 'ok',
	 cost_cny numeric(20,8) NOT NULL DEFAULT 0,
	 occurred_at timestamptz NOT NULL DEFAULT NOW(),
	 received_at timestamptz NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS regional_usage_events_user_occurred
	ON regional_usage_events (user_id, occurred_at);
CREATE INDEX IF NOT EXISTS regional_usage_events_organization_occurred
	ON regional_usage_events (organization_id, occurred_at)
	WHERE organization_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS regional_credit_leases (
	 id uuid PRIMARY KEY,
	 user_id uuid NOT NULL,
	 region_id varchar NOT NULL,
	 request_id varchar NOT NULL DEFAULT '',
	 api_key_id uuid,
	 billing_scope varchar NOT NULL DEFAULT 'personal',
	 organization_id uuid,
	 granted_cny numeric(20,8) NOT NULL DEFAULT 0,
	 spent_cny numeric(20,8) NOT NULL DEFAULT 0,
	 status varchar NOT NULL DEFAULT 'active',
	 expires_at timestamptz NOT NULL,
	 created_at timestamptz NOT NULL DEFAULT NOW(),
	 updated_at timestamptz NOT NULL DEFAULT NOW(),
	 closed_at timestamptz
);
CREATE INDEX IF NOT EXISTS regional_credit_leases_user_region_status
	ON regional_credit_leases (user_id, region_id, status);
CREATE INDEX IF NOT EXISTS regional_credit_leases_organization_region_status
	ON regional_credit_leases (organization_id, region_id, status)
	WHERE organization_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS regional_credit_leases_expiry
	ON regional_credit_leases (status, expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS regional_credit_leases_region_request
	ON regional_credit_leases (region_id, request_id)
	WHERE request_id <> '';
`
