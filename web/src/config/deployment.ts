// Deployment-specific values (domains, sibling sites, the web app link) are
// read from VITE_* variables here. Unset values are empty, which hides the
// links that need them. Kept free of import.meta so vite.config.ts can use
// the same values for index.html and llms.txt; the app reads the resolved
// values from config/env.ts.

export interface Deployment {
  // Shown while the backend's site settings are loading or unset.
  siteName: string;
  // Registered domains this frontend is served from. Sibling sites (docs.…)
  // are resolved under whichever one the visitor is on, so a link never moves
  // them to another domain.
  siteDomains: string[];
  // Used when the current host is none of siteDomains (local dev, previews).
  // "" = none.
  defaultDomain: string;
  // Documentation site. "" = https://docs.<current domain>, or no docs link
  // when no domain is configured.
  docsUrl: string;
  // Hosted web chat app. "" hides the link.
  webAppUrl: string;
  // Public origin written into llms.txt. "" = relative links.
  publicSiteUrl: string;
}

type Env = Record<string, string | boolean | undefined>;

function value(env: Env, key: string, fallback: string): string {
  const v = env[key];
  return typeof v === "string" ? v.trim() : fallback;
}

function nonEmpty(env: Env, key: string, fallback: string): string {
  return value(env, key, fallback) || fallback;
}

export function resolveDeployment(env: Env): Deployment {
  const domains = value(env, "VITE_SITE_DOMAINS", "")
    .split(",")
    .map((d) => d.trim().toLowerCase())
    .filter(Boolean);
  return {
    siteName: nonEmpty(env, "VITE_SITE_NAME", "JuCode"),
    siteDomains: domains,
    defaultDomain: value(env, "VITE_SITE_DEFAULT_DOMAIN", "").toLowerCase(),
    docsUrl: value(env, "VITE_DOCS_URL", ""),
    webAppUrl: value(env, "VITE_WEB_APP_URL", ""),
    publicSiteUrl: value(env, "VITE_PUBLIC_SITE_URL", "").replace(/\/+$/, ""),
  };
}
