# Regional edge nodes

> **Experimental.** The edge feature, its HTTP protocol, its config keys and
> its tables may change in any release, without a migration path. It is off
> by default. Read the [known limitations](#known-limitations) before you run
> it with real money.

An edge node (`jucode-edge`) serves the OpenAI/Anthropic-compatible API from
another region. It forwards requests to upstream accounts close to it and
bills them locally, while the main server stays the only place that holds
balances. The main server in this role is the **control plane**.

## Architecture

```
                        clients in region B
                               |
                               v
 +-----------------------------------------------+
 | jucode-edge  (region_id: edge-1)              |
 |                                               |
 |  /v1/chat/completions, /v1/responses,         |
 |  /v1/messages, /v1/models                     |
 |                                               |
 |  memory + Redis:  account snapshots           |
 |                   upstream snapshot           |
 |                   local ledger (reserve /     |
 |                   settle against a lease)     |
 |  disk:            usage-event outbox          |
 +---------+---------------------------+---------+
           |                           |
           | upstream calls            | POST /internal/region/v1/*
           v                           | Bearer <service token>, TLS
   upstream accounts                   v
   allowed for edge-1   +------------------------------------+
                        | jucode (control plane)             |
                        |   region.enabled: true             |
                        |                                    |
                        |   snapshots  <- account pool,      |
                        |                 API keys           |
                        |   SQL ledger -> balances, leases,  |
                        |                 usage events,      |
                        |                 usage_logs         |
                        +-----------------+------------------+
                                          |
                                       Postgres
```

Code:

| Path | Role |
| --- | --- |
| `internal/region` | Wire protocol shared by both sides: types, paths, error codes, money units, the HTTP client |
| `internal/region/control` | Control plane: HTTP handler, `SQLLedger`, lease policy, table DDL |
| `internal/edge`, `cmd/jucode-edge` | Edge node: proxy, Redis ledger, outbox, sync loops |

The main charge path (`biz.BillingSvc`) does not know about regions. With
`region.enabled: false` nothing in `internal/region/control` is constructed,
`/internal/region/v1` answers 404, and no lease loop runs.

## How money moves

1. **Snapshots.** The edge pulls the upstream snapshot (accounts, models,
   prices, credentials encrypted with `sync_encrypt_key`) every
   `sync_interval`, and resolves a client API key on first use by the SHA-256
   of the key. The plaintext key never leaves the edge.
2. **Lease.** The first time an account needs credit, the edge asks for a
   lease. The control plane locks the user (or organization) row, checks the
   balance, the API key's daily/monthly/total cost limits (settled usage plus
   every unspent active lease of that key), and organization and member
   limits, then moves up to `region.lease_cny` from the balance into a lease
   row. The hold is written as an `adjust` balance transaction, so it does not
   count as spend. The edge mirrors only the frozen amount into Redis.
3. **Reserve and forward.** Every request gets a server-generated billing ID
   (the client's `X-Request-ID` is only echoed for log correlation). The edge
   reserves `reserve_cny` against the lease in Redis and forwards only if the
   reservation is fresh. A replayed billing ID can never buy a second
   upstream call.
4. **Settle.** After the response, in this order: the usage event is fsynced
   into the outbox, then the Redis reservation is settled to the real cost,
   then local spend counters move. If the outbox write fails, the reservation
   stays held and the whole step is retried; a failure is never treated as
   success.
5. **Report.** The outbox is pushed in batches. The control plane inserts each
   event into `regional_usage_events` (idempotent on `event_id`) and
   `usage_logs`, and books the cost against the lease. A lease absorbs at most
   what it granted; anything past it, or anything arriving after the lease was
   released, is charged straight to the balance.
6. **Release.** When a newer lease replaces an old one and the old one has no
   pending events or reservations, the edge releases it and the unspent part
   returns to the balance. Leases the edge never releases are returned by the
   control plane's cleanup loop after `region.lease_ttl`.

Every balance movement has a ledger row: `regional credit lease hold`,
`regional credit lease release`, `regional organization charge` (the spend of
an organization lease, recorded as a `deduct` at release),
`regional edge charge [over lease]`.

### Stored data

Edges and the control plane store request **metadata only**: ids, model,
endpoint, token counts, latency, status and cost. No request or response
content is written to the outbox, Redis, or any table. `UsageEvent` has no
field for it, and the control plane rejects events with unknown fields.
`TestEdgeControlRoundTrip` checks this.

## Protocol (v1)

All endpoints are `POST` with a JSON body under `/internal/region/v1`. The
request and response types are in `internal/region/protocol.go`.

| Path | Request | Response |
| --- | --- | --- |
| `/provider-snapshot` | `ProviderSnapshotRequest` (`provider_ids`: account ids, optional) | `ProviderSnapshotResponse` |
| `/account-snapshot` | `AccountSnapshotRequest` (`api_key_hash`) | `AccountSnapshotResponse` |
| `/credit-lease` | `CreditLeaseRequest` | `CreditLeaseResponse` |
| `/credit-lease/release` | `CreditLeaseReleaseRequest` | `CreditLeaseReleaseResponse` |
| `/usage-events` | `UsageEventsRequest` (at most 500 events) | `UsageEventsResponse` |

Several names say "provider" for historical reasons; the ids are upstream
**account** ids.

**Authentication.** `Authorization: Bearer <service token>`. Each edge has
its own token in `region.edges`; tokens are compared in constant time. The
token decides the caller's region: a lease request or a usage event that
names another `region_id` is refused with `region_mismatch`, and so is
releasing another region's lease. Upstream credentials inside snapshots are
additionally encrypted with AES-256-GCM under `sync_encrypt_key`. There is
no request signing, so run the protocol over TLS only and do not expose
`/internal/region/v1` to the public internet.

**Errors.** Every non-2xx response is

```json
{"error": {"code": "insufficient_balance", "message": "..."}}
```

| Code | Status | Meaning |
| --- | --- | --- |
| `unauthorized` | 401 | Missing or unknown service token |
| `invalid_request` | 400 | Malformed body |
| `not_found` | 404 | Unknown, revoked or expired API key |
| `region_mismatch` | 403 | The request names a region the token does not own |
| `insufficient_balance` | 402 | The balance cannot cover a lease |
| `api_key_cost_limit_exceeded` | 402 | The key's cost limit leaves no room for a lease |
| `organization_limit_exceeded` | 402 | Organization monthly limit |
| `member_limit_exceeded` | 402 | Organization member monthly or total limit |
| `organization_forbidden` | 403 | Key and organization do not match, or the user is not a member |
| `unavailable` | 503 | A snapshot could not be built |
| `internal_error` | 500 | Anything else; details stay in the control plane's log |

`region.Client` returns these as `*region.Error`, which matches the
corresponding sentinel (`region.ErrLeaseInsufficient`, ...) under
`errors.Is`. The edge answers its own clients with 402 `insufficient_balance`
or 403 `cost_limit_exceeded` / `organization_limit_exceeded` /
`organization_forbidden` for these, and 503 `billing_unavailable` when the
control plane cannot be reached.

**Versioning.** The version is the path. Fields may be added within v1;
readers ignore fields they do not know, except the usage-events endpoint,
which rejects them. So upgrade the control plane before the edges. A change
an existing edge cannot handle gets `/internal/region/v2`.

## Configuration

Money values are CNY decimal strings with at most six decimal places.

### Control plane (`configs/config.example.yaml`)

| Key | Default | Meaning |
| --- | --- | --- |
| `region.enabled` | `false` | Mount `/internal/region/v1` and run the lease cleanup loop |
| `region.sync_encrypt_key` | — | 64 hex chars, encrypts credentials in snapshots; required when enabled, never the same as `encrypt_key` |
| `region.lease_cny` | `"10"` | Largest lease frozen at a time |
| `region.overdraft_cny` | `"10"` | How far below zero a balance may go when a lease is frozen |
| `region.lease_ttl` | `24h` | Lifetime of a lease before cleanup returns it |
| `region.edges[].id` | — | An edge's `region_id`; at least one edge when enabled |
| `region.edges[].service_token` | — | That edge's token, at least 32 characters, unique |

`server.timezone` defines the day and month of API key cost limits on both
sides. Organization limits use UTC months, as on the main gateway.

### Edge (`configs/edge.example.yaml`)

Every key can also be set as `JUCODE_EDGE_<KEY>`, dots as underscores.

| Key | Default | Meaning |
| --- | --- | --- |
| `listen` | `:8091` | HTTP listen address |
| `region_id` | — | Required; must match `region.edges[].id` |
| `timezone` | `Asia/Shanghai` | Day and month of the local spend tripwire; set it to the control plane's `server.timezone` |
| `trust_forwarded_for` | `false` | Read the client IP for key IP allowlists from `X-Forwarded-For` |
| `redis.addr` / `.password` / `.db` | `127.0.0.1:6379`, —, `1` | Local ledger and caches |
| `control.url` | — | Control plane base URL; empty means standalone mode |
| `control.service_token` | — | This edge's token; required with `control.url` |
| `control.sync_encrypt_key` | — | The control plane's `region.sync_encrypt_key`; required |
| `upstreams.allowed_ids` | — | Upstream account ids this edge may use; required with `control.url` |
| `upstreams.file` | — | Static upstream snapshot for standalone mode |
| `outbox_dir` | `./data/edge-outbox` | Durable usage-event queue; keep it on persistent disk |
| `lease_cny` | `"10"` | Credit asked for per lease (the control plane caps it) |
| `reserve_cny` | `"1"` | Reserved per request before forwarding |
| `overdraft_cny` | `"10"` | Local negative balance allowed when a request costs more than it reserved |
| `request_ttl` | `30m` | Lifetime of a reservation in Redis |
| `account_ttl` | `10m` | How long an account snapshot is cached |
| `sync_interval` | `30s` | Snapshot refresh, outbox push and retry period |
| `max_body_bytes` | `104857600` | Largest request and non-streaming response body |
| `request_timeout` | `600s` | Total timeout of a non-streaming upstream call |

## Deploying two nodes

1. Generate a sync key and a token:
   `openssl rand -hex 32` twice.
2. On the main server, add to `config.yaml` and restart it:

   ```yaml
   region:
     enabled: true
     sync_encrypt_key: "<sync key>"
     edges:
       - id: "edge-1"
         service_token: "<token>"
   ```

   It logs `region control plane enabled: this feature is EXPERIMENTAL`.
   Run `jucode-migrate` if you have not since installing; it creates
   `regional_credit_leases` and `regional_usage_events`.
3. Put `/internal/region/v1` behind TLS and restrict it to the edge's
   address.
4. In the admin console, note the ids of the upstream accounts the edge
   should use. Only models in enabled `balance_only` groups are served by
   edges.
5. On the edge host, with Redis running, copy `configs/edge.example.yaml`
   to `edge.yaml` and set `region_id: edge-1`, `control.url`,
   `control.service_token`, `control.sync_encrypt_key`,
   `upstreams.allowed_ids` and `outbox_dir`. Then run
   `jucode-edge -config edge.yaml`.
6. Check `GET /readyz` on the edge (200 once the upstream snapshot is
   loaded), then send a request with a normal API key to
   `/v1/chat/completions`. Within `sync_interval` it shows up in the user's
   usage log and a row appears in `regional_credit_leases`.

Further edges are more entries in `region.edges`, each with its own id and
token.

## Extension points

These interfaces exist for the two roles as they work today; each has one
default implementation.

| Interface | Default | Replace it to |
| --- | --- | --- |
| `control.Authenticator` | `TokenAuth` (token → region id) | Identify edges by mTLS, signed requests, an identity provider |
| `control.LeasePolicy` | `FixedLeasePolicy` (config values) | Size leases per region, per account or by recent spend |
| `control.Ledger` | `SQLLedger` (Postgres) | Keep leases elsewhere |
| `control.Snapshots` | main server's account pool and API keys | Serve edges from another source |
| `edge.ControlPlane` | `region.Client` (HTTP) | Report usage and fetch snapshots over another transport |

## Known limitations

- Experimental: no compatibility promise between releases.
- Any edge with a valid token can fetch the snapshot of every upstream
  account, and all edges share one `sync_encrypt_key`. `upstreams.allowed_ids`
  is enforced by the edge itself.
- Only `balance_only` groups are served; plan quotas are not replicated.
- Standalone mode (no `control.url`) supports personal keys only and reports
  usage nowhere.
- The edge picks the first upstream account that serves the model; there is
  no failover between accounts and no retry.
- The edge's usage-log rows have no client IP, user agent, upstream model or
  cache-token breakdown, and regional usage triggers no invite rebates.
- The API key spend tripwire on an edge counts only that edge's spend. The
  authoritative check is at lease grant, so with several edges a key can
  overshoot its limit by up to the leases outstanding.
- A settlement that could not reach the outbox waits in memory for retry. If
  the edge process dies before the retry, that request is not charged; its
  reservation expires after `request_ttl`.
- Leases, reservations and amounts are in CNY only.
