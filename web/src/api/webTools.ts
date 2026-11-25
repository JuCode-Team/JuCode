// Web tools: the platform's own search and page-fetch API (/tools/v1/*).
// They are not models — each engine has its own per-call price and switch,
// set on the admin "网页工具" page, and calls are billed to the balance.
import i18n from "../i18n";
import { http } from "./client";

export type WebToolKind = "search" | "fetch";

export interface WebEngine {
  // "<kind>.<engine>", e.g. "search.auto". Usage rows carry it in `model`.
  id: string;
  kind: WebToolKind;
  engine: string;
  name: string;
  vendors: string[];
}

export interface WebEngineAdmin extends WebEngine {
  enabled: boolean;
  // Per successful call, CNY, decimal string.
  price: string;
}

export interface WebVendorAccounts {
  total: number;
  schedulable: number;
}

export interface WebToolsAdmin {
  engines: WebEngineAdmin[];
  user_rpm: number;
  // Keyed by vendor (account api_format).
  accounts: Record<string, WebVendorAccounts>;
}

export interface WebToolsUpdate {
  engines: Record<string, { enabled: boolean; price: string }>;
  user_rpm: number;
}

export interface WebToolPrice {
  id: string;
  kind: WebToolKind;
  engine: string;
  name: string;
  price: string;
}

export interface WebSearchRequest {
  query: string;
  engine?: string;
  max_results?: number;
  freshness?: "day" | "week" | "month" | "year";
}

export interface WebSearchResult {
  title: string;
  url: string;
  snippet: string;
  published_at?: string;
}

export interface WebSearchResponse {
  object: "web.search";
  engine: string;
  query: string;
  results: WebSearchResult[];
}

export interface WebFetchRequest {
  url: string;
  engine?: string;
}

export interface WebFetchResponse {
  object: "web.fetch";
  engine: string;
  url: string;
  title: string;
  content: string;
  status?: number;
  published_at?: string;
}

export const webToolsAdminApi = {
  get: () => http.get<WebToolsAdmin>("/v1/admin/web-tools").then((r) => r.data),
  update: (b: WebToolsUpdate) => http.put<WebToolsAdmin>("/v1/admin/web-tools", b).then((r) => r.data),
};

export const webToolsPublicApi = {
  prices: () => http.get<{ currency: string; tools: WebToolPrice[] }>("/api/tools/pricing").then((r) => r.data.tools),
};

// labWebToolsApi calls the tools through the console lab (session login, the
// lab key pays), the same surface /tools/v1 serves to API keys.
export const labWebToolsApi = {
  search: (b: WebSearchRequest, signal?: AbortSignal) =>
    http.post<WebSearchResponse>("/v1/me/lab/tools/search", b, { signal }).then((r) => r.data),
  fetch: (b: WebFetchRequest, signal?: AbortSignal) =>
    http.post<WebFetchResponse>("/v1/me/lab/tools/fetch", b, { signal }).then((r) => r.data),
};

// Engine ids with display names, for usage rows whose endpoint is web_search / web_fetch.
const ENGINE_IDS = new Set([
  "search.auto",
  "search.parallel",
  "search.brave",
  "fetch.jina",
  "fetch.firecrawl",
  "fetch.parallel",
] as const);
type EngineId = typeof ENGINE_IDS extends Set<infer T> ? T : never;

export function isWebToolEndpoint(endpoint?: string): boolean {
  return endpoint === "web_search" || endpoint === "web_fetch";
}

// isWebEngineId recognizes an engine id where no endpoint is at hand (usage
// aggregated by model).
export function isWebEngineId(id: string): boolean {
  return ENGINE_IDS.has(id as EngineId);
}

export function webEngineName(id: string): string {
  return ENGINE_IDS.has(id as EngineId) ? i18n.t(`bench.webTools.engines.${id as EngineId}`) : id;
}
