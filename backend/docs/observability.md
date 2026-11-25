# Admin observability APIs

Read-only admin surface for traffic health, added in the observability PR:

| Endpoint | Source table | Purpose |
| --- | --- | --- |
| `GET /v1/admin/observability/series` | `usage_logs` | Time-bucketed metrics for one dimension (platform / provider / group / model) |
| `GET /v1/admin/observability/leaderboard` | `usage_logs` | The same metrics per entity, ranked by request count |
| `GET /v1/admin/observability/sla` | `availability_observations` | Business-only SLA report per status-page group and model |
| `GET /v1/admin/observability/provider-attempts` | `usage_logs.provider_chain` | Per-channel attempt counts, independent of who finally served the request |
| `GET /v1/admin/usage-logs?anomaly=…` | `usage_logs` | Existing log browser, additively extended with anomaly flags + filter |

Handlers live in `internal/api/admin/admin_observability.go`; anomaly
definitions in `internal/biz/anomaly.go`; the SLA aggregation in
`internal/biz/sla.go`.

## Series / leaderboard parameters

- `dim`: `platform|provider|group|model` for the series (default
  `platform`); `provider|group|model` for the leaderboard (required).
  `dim=provider` requires `provider_id`, `dim=group` requires `group_id`,
  `dim=model` requires `model` (exact model name).
- `window`: `1h|6h|24h`. Series default `24h`, leaderboard default `1h`.
  Bucket widths are fixed per window: 1h→120s, 6h→600s, 24h→1800s
  (48 buckets).
- `include_synthetic`: default `false` — rows whose API key is flagged
  `api_keys.internal` (the status-page prober) are excluded. Rows with
  `api_key_id IS NULL` (OAuth device sessions) are always kept: they are
  real customer traffic.

Unknown-but-valid IDs answer 200 with zero-filled buckets; malformed enums
or UUIDs answer 400 `{"error": "..."}`. Every bucket in the window is
present in `points` — buckets with no traffic carry zero counters and
`null` for every average/percentile metric.

## Metric dictionary (series point / leaderboard row)

All metrics aggregate `usage_logs` rows by `created_at` within the bucket
or window.

| Field | Definition |
| --- | --- |
| `requests` | `COUNT(*)` |
| `ok` | rows with `status = 'ok'` |
| `failures` | rows with `status <> 'ok'` — same failure definition as the existing provider `performance` view and `/stats` errors |
| `failure_rate` | `failures / requests`; `0` when the bucket is empty |
| `provider_faults` | rows with `error_class IN ('provider_failure','rate_limit','timeout','auth')` — the four retryable channel-level classes from `llm/error_class.go` |
| `provider_fault_rate` | `provider_faults / (ok + provider_faults)`; `null` when the denominator is 0. User-caused failures are in neither the numerator nor the denominator |
| `capacity_faults` | rows with `error_class = 'pool_exhausted'` — global capacity, deliberately not a provider fault |
| `avg_ttft_ms`, `p50/p95/p99_ttft_ms` | average and `percentile_cont` over `ttft_ms`, non-null rows only, rounded to integer ms; `null` with no eligible rows |
| `ttft_count` | rows with non-null `ttft_ms` (the sample size behind the percentiles) |
| `avg_tps` | decode speed, token-weighted: eligible `SUM(tokens_out)` over eligible `SUM(window/1000.0)`, where `window = latency_ms - LEAST(ttft_ms, COALESCE(latency_detail->>'first_output_ms', latency_detail->>'first_sse_frame_ms', ttft_ms))` and eligible is `status='ok' AND tokens_out>0 AND ttft_ms IS NOT NULL AND window > 0 AND window >= latency_ms*0.05`; served unrounded so the denominator stays recoverable (formatting is the client's job), `null` when nothing qualifies. See "What TPS measures" below — the eligible set is narrower than `requests` and than `ttft_count` |
| `tps_tokens` | `avg_tps`'s own numerator — output tokens from the eligible rows. This is the coverage figure for `avg_tps`; `ttft_count` is not. To recombine buckets see "What TPS measures" — the weight is seconds, not this |
| `avg_latency_ms`, `p95_latency_ms` | over rows with `status = 'ok' AND latency_ms > 0`; `null` with no eligible rows |
| `tokens_out` | `SUM(tokens_out)` (text tokens only, matching the TPS numerator) |
| `retries` | `SUM(retry_count)` |
| `anomalies`, `time_anomalies`, `error_anomalies` | rows matching the anomaly predicates below (`anomalies` = time OR error) |

Leaderboard rows carry the entity identity (`provider_id` / `group_id` /
`model`) plus a resolved `name`; entities deleted since the logs were
written fall back to an 8-character ID prefix, same as the usage-log
browser. Only entities with ≥1 request in the window appear, ordered by
`requests` descending.

### What TPS measures

`avg_tps` is **decode speed**: output tokens divided by the time spent
generating them. It is not system throughput (total tokens per wall-clock
second across concurrent traffic), and it is not end-to-end request speed.

**Where the window starts.** Not at `ttft_ms`. The window is
`latency_ms - LEAST(ttft_ms, first_output_ms)`, i.e. it starts when the
*model* started producing output, not when the first visible token reached
the client.

The two differ for reasoning models, and by a lot. A thinking phase carries
no readable text, so `ttft_ms` lands at the end of it — while `tokens_out`
is the upstream's `completion_tokens`, which counts every reasoning token.
Starting the clock at the first visible token drops the whole thinking phase
out of the denominator and leaves its tokens in the numerator: one observed
`reasoning_effort: xhigh` request counted 12,784 output tokens against a 56s
window after the model had been producing for 334s — 228 t/s for a model
that decodes at ~50.

`first_output_ms` is recorded by the pipe (`OnFirstOutput` in
`internal/llm/sse.go`) on the first frame showing the model producing
anything. That deliberately includes output nobody can read: a Responses
reasoning item whose content is encrypted, a reasoning-summary part opening,
an Anthropic `thinking` content block. When a reasoning *summary* does
stream, the first summary delta is itself a first token, so `ttft_ms` already
carries that moment.

Two fallbacks, because the field is new and not every path sets it:
`first_sse_frame_ms` (the first upstream frame — the previous anchor, weaker
because a Responses stream opens with lifecycle frames that say nothing about
the model) and then `ttft_ms`. All three live in the admin-only
`latency_detail` map; `/v1/me/usage-logs` lifts them into `first_output_ms` /
`first_frame_ms` so the per-request figure on the usage page uses one window
definition with these aggregates.

**Which rows count.** Only `status='ok' AND tokens_out>0 AND ttft_ms IS NOT
NULL AND window > 0 AND window >= latency_ms*0.05 AND window >=
tokens_out*2`. A request the gateway
never streamed from upstream (`stream:false` chat completions, `stream:false`
native Anthropic pass-through, the generation endpoints) has no `ttft_ms` and
therefore no measurable generation window; dividing by its whole latency
would bill prefill, routing, concurrency wait, the billing reserve and every
failed retry to decode speed. Those rows are skipped instead. Streamed
`/v1/messages` traffic does count, and is usually the bulk of it.

**Buffered upstreams are skipped too, and for the same reason.** The window
is only "time spent decoding" when the upstream streamed token by token. A
relay that assembles the whole answer before replaying it (or a provider
that answers `stream=true` with one pre-generated chunk) spends nearly the
entire request silent and then delivers in a burst, so the window measures
the replay: one observed 1487-token answer arrived 52ms before the end of a
34s request, 0.15% of it, which divides out to 28,596 t/s. The upstream's
real generation time is not observable from here, so the row is skipped
rather than clamped to a fabricated number.

The guard that decides "stream or replay" is a **share of the request, not a
rate ceiling**: the delivery has to occupy at least a twentieth of the
request's wall time. An absolute tokens-per-second bound would encode a claim
about how fast models decode — which the query cannot know — and would need
re-tuning the moment the fleet gained a faster provider; a fraction of the
request is dimensionless and self-scaling. The cost is that a legitimate
long-prefill, short-answer request reads as unmeasurable too: no
arrival-time signal can tell that apart from a replay.

Behind it sits one hard stop: at least 2ms of window per output token, an
implied ceiling of 500 t/s. The share bound cannot rule out a request that
was short end to end and still carried a lot of tokens — a relay replaying a
cached answer responds in tens of milliseconds, and a twentieth of that is
still tens of milliseconds. The stop claims only what is physically safe
("not 500 tokens a second") and nothing about any particular model; in this
fleet it removes the last rows above 500 t/s and moves the platform figure by
under 1%.

A channel that buffers everything reports `avg_tps = null` with `tps_tokens`
far below its `tokens_out` — read that gap as "this channel does not stream",
not as "no traffic".

The per-request figure on the usage page applies the same two bounds plus a
50ms floor (`src/lib/tps.ts`): below 50ms the tokens arrived inside a frame
or two, so the rate would describe SSE batching rather than the model. The
floor exists only there — an aggregate absorbs a handful of unresolvable
rows, a single cell would just print 40000 t/s.

The cost of that is coverage, so **read `avg_tps` next to `tps_tokens`**,
which is the eligible token count the rate was computed from. Do not reach
for `ttft_count`: it is a bare `COUNT(ttft_ms)` with no status or token
filter, so a provider whose streams mostly break mid-flight shows a healthy
`ttft_count` beside a `null` `avg_tps`.

**It is a pooled ratio, not a mean of per-request rates.** Summing both
sides before dividing is what keeps it stable: averaging rates per request
lets a two-token reply that finished one millisecond after its first token
claim thousands of tokens per second — at that scale SSE frame granularity
is the only thing being measured — and carry the same weight as a real
generation. Two consequences to know about, both of which make a channel
look slower than its model is:

- A single long slow generation moves the number more than a short one.
  That is the intended reading for "how fast will my output stream".
- A client that reads slowly is counted as slow generation. The gateway
  pipes upstream to downstream synchronously, so downstream backpressure
  stalls the read and lands inside `latency_ms - ttft_ms`. One large
  response to a weak mobile connection can pull a channel's hourly figure
  down by double digits. This is not a pure upstream signal.

**Recombining buckets.** The weight of a ratio is its denominator, so a
window average is `Σ tps_tokens / Σ (tps_tokens / avg_tps)` — sum the
tokens, and recover each bucket's seconds from its own rate. Averaging the
per-bucket rates weighted by `tps_tokens` instead computes `Σ(T²/S)/ΣT`,
which is never below the true value and flattens a single bad bucket into
noise.

**`latency_ms` stops when the client has the whole response**, before the
TPM settle and the balance transaction (`chargeAndLog` / `logFinal` in
`internal/llm/billing_log.go`). Otherwise database contention would show up
as slow token generation. The provider pin (`sticky.Remember`) still runs
inside the measurement. This definition is shared, not local to this
payload: `WriteUsageLog` derives an `availability_observations` row from
the same value (`availabilityInputFromUsage` in `internal/biz/availability.go`),
so the SLA report and the public status page moved with it. Latency numbers
from before this change read slightly higher for the same traffic.

**`ttft_ms` widened too.** The Anthropic translation bridge now records it
for `stream:false` clients, whose upstream is streamed anyway. Those rows
join `ttft_count`, the TTFT average and percentiles, the `ttft_ge_30s` time
anomaly, and `availability_observations.ttft_ms`. For a non-streaming
client nothing is delivered at the first token, so TTFT panels read
slightly better than that client's experience.

`tokens_out` excludes `audio_tokens_out` while the time spent generating
that audio stays in the denominator, so a text+audio model reads slower
than it is. Realtime and the audio generation endpoints are not affected —
they carry no `ttft_ms` and are already excluded.

Both admin surfaces share one formula by construction:
`queryProviderPerformance` in `admin_providers.go` interpolates the
`obsTPSExpr` constant, and a test pins the literal.

## Attempt-level provider stats

`GET /v1/admin/observability/provider-attempts?window=1h|6h|24h[&include_synthetic=…]`
answers a question the provider leaderboard cannot: **is this channel
healthy**, regardless of who ended up serving the request.

The leaderboard's `dim=provider` attributes each request to the provider in
`usage_logs.provider_id`, which is the one that *finally* served it. A channel
that fails every attempt and is rescued by failover therefore shows up with
almost no requests — its failures live only in `provider_chain`, the Redis
health buckets and the Prometheus counter. This endpoint parses
`provider_chain` directly and counts every attempt each channel handled.

Each row carries `provider_id`, resolved `name`, `attempts`, `ok`,
`provider_faults` (the same four retryable channel-level classes as
`provider_faults` above), `provider_fault_rate` (same denominator: eligible
outcomes only, so `client_canceled` / `user_request` attempts neither help nor
hurt) and `by_class`, the raw class breakdown for drill-down. Window and
`include_synthetic` behave as on the leaderboard.

Cost: parsing jsonb per row is much heavier than the other endpoints' column
aggregates — roughly 25ms for a 1h window and 900ms for 24h at about 61k
rows. It stays inside the 15s query budget behind the same 30s cache;
measure again before widening the window past 24h.

## Anomaly predicates

Defined once in `internal/biz/anomaly.go` and consumed as a Go classifier
(per-row flags on `/v1/admin/usage-logs`), ent predicates (the `?anomaly=`
filter) and raw-SQL fragments (the series/leaderboard counts). A
Postgres-backed test asserts all three select the same fixture rows.

```
time  : (ttft_ms IS NOT NULL AND ttft_ms >= 30000)
        OR latency_ms >= 120000 OR retry_count >= 2
error : status <> 'ok'
        AND status NOT IN ('quota_exceeded','banned','unauthorized')
        AND (error_class = '' OR error_class NOT IN
             ('ok','user_request','user_policy','cyber_block','client_canceled'))
```

Rationale: the excluded statuses are the caller's own account state; the
excluded classes are user-caused outcomes (mirroring the availability
exclusions), plus `error_class = 'ok'` for rows that failed *after* the
response was delivered (billing/audit persistence). A NULL `ttft_ms` never
trips the time predicate, in both Go and SQL.

Every usage-log row now carries the verdict:

```json
"anomaly": {"time": false, "error": true, "reasons": ["provider_failure"]}
```

Reasons are `ttft_ge_30s` / `latency_ge_120s` / `retry_ge_2` for the time
side and the `error_class` token (or `status:<status>` when the class is
empty) for the error side.

`GET /v1/admin/usage-logs?anomaly=any|time|error|none` filters on the same
predicates. With `anomaly` absent the endpoint behaves exactly as before
(no filter, no injected window). With `anomaly` present, the effective start
is the later of the requested start and `end - 24h` (`end` defaults to now).
This preserves bounded historical searches without allowing an anomaly hunt
to walk more than 24 hours of the table.

## SLA report

`GET /v1/admin/observability/sla?window=24h|7d|30d[&group_id=…]` reads
`availability_observations` only, scoped and ordered by the same
`SiteSettings.StatusPageGroupIDs` list the public status page renders.
48 buckets per window; state uses the public bands (`publicState`:
healthy ≥98, degraded ≥90, serious ≥80, plus the minimum-2-failures
damping) and the percentage uses the public two-decimal rounding.

`open_incident_id` is always `null`; the field is reserved so the payload
shape stays stable.

### Admin vs public: the probe difference

The public page pads its 24h bar and state pill with synthetic probe
traffic (`traffic_class='probe'`) so an idle model still reports
reachability, while its headline percentage counts business rows only —
and it never publishes raw counts. The admin SLA is **business-only
everywhere** (`traffic_class='business'`, query kind `"sla"` in
`biz/status.go`), because it *does* publish per-cell `good/bad/excluded`
counts and percentages, and probe-padded numbers would misstate what
customers experienced. Consequences to expect:

- A quiet group can be `unknown` on the admin report while the public bar
  shows green probe cells. Both are correct answers to different questions.
- Admin counts are an admin privilege; the public payload deliberately
  withholds them (see the `json:"-"` fields on `StatusBucket`).

The public `/v1/public/status` payload and queries are unchanged; a frozen
SQL test (`TestAvailabilityGroupQueryPublicKindsFrozen`) guards that.

### The timeout asymmetry

`error_class = 'timeout'` is **excluded** from availability/SLA (operator
decision: the dominant source is our own stream deadline cutting long
cold-cache prefills, not a channel fault — see
`classifyAvailabilityOutcome`), but it **counts** as a provider fault in
`provider_faults` / `provider_fault_rate`. This is intentional: the SLA
answers "did customers get service", the fault rate answers "should an
operator look at this channel", and a channel that accepts connections and
never responds must be visible in the second even though it is excluded
from the first. Do not "fix" one to match the other.

## Caching and query cost

- All three endpoints cache for 30s via `cache.GetOrCompute` (single-flight
  on miss, `X-Cache: HIT|MISS` header). Key prefixes are registered in
  `internal/cache/keys.go` (`KeyObservabilitySeriesPrefix` /
  `…LeaderboardPrefix` / `…SLAPrefix`); the suffix encodes dim, entity,
  window, synthetic flag and the snapped window start. TTL-only, like
  `KeyProviderPerformance` — no admin mutation can invalidate a traffic
  summary, only time can.
- Windows are snapped (series/SLA to the bucket grid, leaderboard to the
  TTL) so rolling polls resolve to the same cache key instead of minting a
  new one per request — the `SnapWindow` rationale from `biz/stats.go`.
- Aggregation queries run with a 15s timeout on `h.rawDB` (raw SQL is
  required for `percentile_cont`).
- **24h ceiling**: the widest usage_logs window any of these endpoints
  accepts is 24 hours, and the anomaly filter injects the same floor. The
  scans are served by the existing `created_at` range index.
- **No new usage_logs indexes.** The table is the largest in the DB and
  every insert sits on the LLM response path; the schema comment on
  `UsageLog.Indexes` documents why dimension indexes were dropped. These
  endpoints are designed to live off the `created_at` index + the 30s
  response cache instead. Before loosening either the window ceiling or
  the cache, EXPLAIN ANALYZE the 24h platform series on staging.
