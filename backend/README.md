# JuCode backend

The JuCode backend is an LLM API gateway and the console backend behind it,
written in Go. It routes client requests to upstream model providers with the
operator's own API keys, meters and bills every call against user balances or
plans, and serves the user, customer-service and admin consoles.

The web console ([`../web`](../web)) and the CLI ([`../cli`](../cli)) live in
the same repository and talk to this server.

## Features

- **OpenAI- and Anthropic-compatible gateway**: `/v1/chat/completions`,
  `/v1/responses` (HTTP and WebSocket), `/anthropic/v1/messages` (also at
  `/v1/messages`), `/v1/models`, embeddings, images, audio, rerank and async
  video. Requests are forwarded natively when the upstream speaks the client's
  protocol and can be converted between Chat Completions, Responses and
  Messages when a site switch allows it.
- **Account pool**: each upstream account is an endpoint plus an official API
  key (OpenAI, Anthropic, Azure OpenAI, Azure AI Foundry, any
  OpenAI-compatible vendor; native adaptors such as Gemini, DashScope and
  Volcengine Ark cover the non-chat generation endpoints). Model groups bind accounts directly; routing
  honours priority, weight, health, per-account concurrency, cooldowns and
  conversation stickiness, and fails over across accounts and groups.
  Provider definitions and a vendored provider catalog prefill new accounts.
- **Billing**: per-token and per-request prices, cache read/write pricing,
  reserve-then-settle holds, balances, quota plans ("Coding Plan") with
  rolling windows, credit packs, redeem codes, organizations with shared
  balances, invite rebates and agent commissions.
- **Payments**: Alipay website and mobile checkout with refunds back through
  Alipay, off by default; orders can also be marked paid by an admin.
- **Web tools**: `/tools/v1/search` and `/tools/v1/fetch` backed by Parallel,
  Brave Search, Jina Reader or Firecrawl accounts.
- **Accounts and access**: email/password sign-in, TOTP, personal access
  tokens, API keys with group and IP restrictions, and a first-party OAuth
  flow for the CLI, Desktop and web app (the client list is configurable).
- **Operations**: public status page with synthetic probes, SLA monitoring and
  incidents, per-user rate limits, content moderation and sanctions, ops alert
  webhooks (generic, DingTalk, Feishu), Prometheus metrics, tickets and a
  customer-service console.
- **Remote access relay**: lets a signed-in client reach the user's own
  computers through `/v1/relay`.
- **Regional edge** (experimental, off by default): `jucode-edge` serves a
  region from local upstream accounts and leases credit from the main
  server. See [docs/edge.md](docs/edge.md).

## Requirements

- Go 1.26 or later
- PostgreSQL 16 (14 or later should work)
- Redis 7

## Quick start with Docker Compose

The repository root has a `docker-compose.yml` that starts Postgres, Redis,
this server and the web console; see the [root README](../README.md#quick-start).

## Running from source

1. Start Postgres and Redis, and create a database:

   ```bash
   createdb jucode
   ```

2. Create a config file and set at least the database DSN, `jwt.secret` (32
   bytes or more) and `encrypt_key`:

   ```bash
   cp configs/config.example.yaml configs/config.yaml
   openssl rand -hex 32   # use the output as encrypt_key
   ```

   Every setting can also come from the environment with the `JUCODE_`
   prefix and underscores for nesting, for example `JUCODE_DATABASE_DSN`,
   `JUCODE_REDIS_ADDR`, `JUCODE_JWT_SECRET` and `JUCODE_ENCRYPT_KEY`.
   `INTERNAL_API_TOKEN` enables the `/internal/*` drain and metrics
   endpoints.

3. Apply the schema and start the server:

   ```bash
   go run ./cmd/jucode-migrate --config configs/config.yaml
   go run ./cmd/jucode --config configs/config.yaml
   ```

4. Register a user through the web console or `POST /v1/auth/register`, then
   make it an administrator:

   ```sql
   UPDATE users SET role = 'admin' WHERE email = 'you@example.com';
   ```

5. In the admin console, add an upstream account (base URL, protocols and API
   key), create a model group, bind the account and the models to it, and
   create an API key for a user. Clients then call the gateway with
   `Authorization: Bearer <api key>`.

Most runtime behaviour (protocol conversion, stickiness, rate limits,
moderation, email, alerts, pricing, agent refund terms, top-up presets) is
configured from the admin console and stored in the database, not in the
config file. Calendar days and months for reports and spend limits follow
`server.timezone` (default `Asia/Shanghai`).

## Commands

| Command | Purpose |
| --- | --- |
| `cmd/jucode` | The API server |
| `cmd/jucode-migrate` | Applies the Ent schema plus the raw-SQL tables Ent cannot express |
| `cmd/jucode-edge` | Experimental regional edge node (`configs/edge.example.yaml`, [docs/edge.md](docs/edge.md)) |
| `cmd/mock-openai` | A fake OpenAI-compatible upstream for local testing |

## Build and test

```bash
go build ./...
go vet ./...
go test ./...
```

Tests that need Postgres or Redis are skipped unless these are set; run them
with `-p 1` because they share one database:

```bash
export JUCODE_TEST_PG_DSN='postgres://postgres@127.0.0.1:5432/jucode_test?sslmode=disable'
export JUCODE_TEST_REDIS_ADDR='127.0.0.1:6379'
go test -p 1 ./...
```

`make check` runs gofumpt, `go vet`, golangci-lint and the tests.

After editing a schema under `internal/data/ent/schema`, regenerate the Ent
code with `go generate ./internal/data/ent`.

## Layout

| Path | Contents |
| --- | --- |
| `internal/api` | HTTP routing and handlers (auth, user, admin, cs, oauth, llm, openapi) |
| `internal/llm` | The gateway: routing, protocol conversion, streaming, metering |
| `internal/biz` | Business services: accounts, billing, plans, finance, status |
| `internal/data` | Postgres (Ent) and Redis clients; schemas in `internal/data/ent/schema` |
| `internal/providerkit` | Upstream credential format and the vendored provider catalog |
| `internal/payments` | Payment channel clients |
| `internal/edge`, `internal/region`, `internal/region/control` | Experimental regional edge: the edge node, the shared protocol, the control plane |
| `internal/relay` | Remote-access relay |
| `configs` | Example configuration |
| `docs` | Design notes for billing, retries, observability and more |

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
