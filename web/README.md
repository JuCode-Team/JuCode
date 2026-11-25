# JuCode Console

The web frontend of JuCode, a unified LLM gateway that gives coding clients
(Codex, Claude Code, Cursor and others) one base URL for the OpenAI Responses,
OpenAI Chat Completions and Anthropic Messages protocols.

This directory contains:

- the public site: home, model catalog and prices, status page and
  endpoint guide;
- the user console: API keys, usage, billing and top-ups, plans, tickets,
  organizations and the playground;
- the admin console: upstream accounts, models and groups, users, finance,
  site settings, logs and observability;
- the support (`/cs`) and reseller (`/agent`) consoles.

It is a single-page app built with React, TypeScript, Vite, Radix Themes and
TanStack Query. It talks to the JuCode backend over its HTTP API (`/v1/*`)
and needs a running backend for everything except static pages.

## Requirements

- Node.js 20.19+ or 22.12+ and npm
- A JuCode backend. In development the console expects it at
  `http://localhost:8090`.

## Development

```sh
npm install
npm run dev        # Vite dev server on http://localhost:5173
```

Other scripts:

```sh
npm run typecheck  # type-check the app, the tests and the Vite config
npm run lint       # eslint
npm test           # vitest
npm run build      # type-check, then build into dist/
npm run preview    # serve dist/ locally
```

## Configuration

Build-time configuration uses `VITE_*` environment variables. Copy
`.env.example` to `.env.local` and uncomment what you need; every variable is
optional; links whose target is not configured (docs, web app) are hidden.

| Variable | Default | Purpose |
| --- | --- | --- |
| `VITE_API_BASE` | same origin (dev: `http://localhost:8090`) | Backend origin |
| `VITE_SITE_NAME` | `JuCode` | Site name before the backend's site settings load; also used in `index.html` and `llms.txt` |
| `VITE_SITE_DOMAINS` | empty | Registered domains the site is served from, used to resolve sibling sites on the visitor's domain |
| `VITE_SITE_DEFAULT_DOMAIN` | empty | Domain used when the current host matches none of the above |
| `VITE_DOCS_URL` | `https://docs.<current domain>` | Documentation site; hidden when neither this nor a domain is set |
| `VITE_WEB_APP_URL` | empty | Hosted web chat app; empty hides the link |
| `VITE_PUBLIC_SITE_URL` | empty | Public origin written into `llms.txt`; empty writes relative links |

The defaults live in `src/config/deployment.ts`. Everything else (site name,
logo, contact details, announcements, payment channels, refund options and
so on) is configured at runtime in the admin console under 系统设置 and
served by the backend.

## Deployment

`npm run build` writes a static bundle to `dist/`. Serve it with any web
server and:

- forward `/v1/*` and `/healthz/*` to the backend, or build with
  `VITE_API_BASE` pointing at the backend origin;
- fall back to `index.html` for unknown paths, since routing happens in the
  browser.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
