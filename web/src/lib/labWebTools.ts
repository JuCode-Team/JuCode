// The lab's web tools: argument checks, the text handed back to the model,
// the compact step shown in the message, and readable error messages.
// Calls go through the platform tools (/v1/me/lab/tools/*), billed per call.

import i18n from "../i18n";
import type { WebFetchRequest, WebFetchResponse, WebSearchRequest, WebSearchResponse } from "../api/webTools";
import type { StoredToolCall } from "./labCache";
import { parseToolArgs, WEB_FETCH_TOOL, WEB_SEARCH_TOOL, type LabToolName } from "./labProtocol";

export const TOOL_ROUNDS = 8;
export const RESEARCH_TOOL_ROUNDS = 12;
/** Page text handed back to the model is cut here. */
export const FETCH_TEXT_LIMIT = 12_000;
const STEP_RESULTS = 8;

export interface LabWebToolsClient {
  search: (b: WebSearchRequest, signal?: AbortSignal) => Promise<WebSearchResponse>;
  fetch: (b: WebFetchRequest, signal?: AbortSignal) => Promise<WebFetchResponse>;
}

export interface WebToolRun {
  /** Text for the model. */
  output: string;
  isError: boolean;
  step: StoredToolCall;
}

const FRESHNESS = new Set(["day", "week", "month", "year"]);

export function searchRequest(args: Record<string, unknown>): WebSearchRequest | string {
  const query = typeof args.query === "string" ? args.query.trim() : "";
  if (!query) return i18n.t("bench.tools.missingQuery");
  const out: WebSearchRequest = { query };
  const max = Number(args.max_results);
  if (Number.isFinite(max) && max > 0) out.max_results = Math.min(10, Math.max(1, Math.round(max)));
  if (typeof args.freshness === "string" && FRESHNESS.has(args.freshness)) {
    out.freshness = args.freshness as WebSearchRequest["freshness"];
  }
  return out;
}

export function fetchRequest(args: Record<string, unknown>): WebFetchRequest | string {
  const url = typeof args.url === "string" ? args.url.trim() : "";
  if (!/^https?:\/\/\S+$/i.test(url)) return i18n.t("bench.tools.missingUrl");
  return { url };
}

export function searchOutput(res: WebSearchResponse): string {
  if (res.results.length === 0) return JSON.stringify({ query: res.query, results: [], note: i18n.t("bench.tools.noResultsNote") });
  return JSON.stringify({
    query: res.query,
    results: res.results.map((item, i) => ({
      n: i + 1,
      title: item.title,
      url: item.url,
      snippet: item.snippet,
      ...(item.published_at ? { published_at: item.published_at } : {}),
    })),
  });
}

export function fetchOutput(res: WebFetchResponse, limit = FETCH_TEXT_LIMIT): string {
  const content = res.content ?? "";
  const cut = content.length > limit;
  return JSON.stringify({
    url: res.url,
    title: res.title,
    ...(res.published_at ? { published_at: res.published_at } : {}),
    content: cut ? content.slice(0, limit) : content,
    ...(cut ? { truncated: i18n.t("bench.tools.truncated", { total: content.length, limit }) } : {}),
  });
}

const ERROR_CODES = new Set([
  "invalid_web_request",
  "engine_disabled",
  "rpm_limit",
  "billing_rejected",
  "no_available_provider",
  "fetch_failed",
] as const);
type ErrorCode = typeof ERROR_CODES extends Set<infer T> ? T : never;

function errorText(code: ErrorCode): string {
  return i18n.t(`bench.tools.errors.${code}`);
}

/** A readable message for a failed web tool call (axios error with body {error:{type,code,message}}). */
export function webToolErrorMessage(err: unknown): string {
  const response = (err as { response?: { status?: number; data?: unknown } } | null)?.response;
  const body = response?.data as { error?: unknown } | undefined;
  const detail = body?.error;
  const code = typeof detail === "object" && detail !== null ? (detail as { code?: unknown }).code : undefined;
  const message = typeof detail === "object" && detail !== null
    ? (detail as { message?: unknown }).message
    : detail;
  const text = typeof message === "string" ? message.trim() : "";
  const known = typeof code === "string" && ERROR_CODES.has(code as ErrorCode) ? errorText(code as ErrorCode) : undefined;
  if (known) return code === "invalid_web_request" || code === "fetch_failed" ? (text ? i18n.t("bench.tools.errorDetail", { known, text }) : known) : known;
  if (response?.status === 429) return errorText("rpm_limit");
  if (response?.status === 402) return errorText("billing_rejected");
  if (text) return text;
  if (err instanceof Error && err.message) return err.message;
  return response?.status ? `HTTP ${response.status}` : i18n.t("bench.tools.requestFailed");
}

/** Runs one web_search or web_fetch call. Never throws; failures become an error step and an error result. */
export async function runWebTool(
  client: LabWebToolsClient,
  call: { id: string; name: LabToolName; arguments: string },
  signal?: AbortSignal,
): Promise<WebToolRun> {
  const args = parseToolArgs(call.arguments);
  const fail = (detail: string, error: string): WebToolRun => ({
    output: i18n.t("bench.tools.callFailed", { error }),
    isError: true,
    step: { id: call.id, name: call.name, detail, status: "error", error },
  });
  if (call.name === WEB_SEARCH_TOOL) {
    const req = searchRequest(args);
    if (typeof req === "string") return fail("", req);
    try {
      const res = await client.search(req, signal);
      return {
        output: searchOutput(res),
        isError: false,
        step: {
          id: call.id,
          name: call.name,
          detail: req.query,
          status: "done",
          summary: i18n.t("bench.tools.resultCount", { count: res.results.length }),
          results: res.results.slice(0, STEP_RESULTS).map((item) => ({ title: item.title || item.url, url: item.url })),
        },
      };
    } catch (err) {
      return fail(req.query, webToolErrorMessage(err));
    }
  }
  if (call.name === WEB_FETCH_TOOL) {
    const req = fetchRequest(args);
    if (typeof req === "string") return fail(typeof args.url === "string" ? args.url : "", req);
    try {
      const res = await client.fetch(req, signal);
      return {
        output: fetchOutput(res),
        isError: false,
        step: {
          id: call.id,
          name: call.name,
          detail: req.url,
          status: "done",
          summary: res.title || undefined,
        },
      };
    } catch (err) {
      return fail(req.url, webToolErrorMessage(err));
    }
  }
  return fail("", i18n.t("bench.tools.unsupported", { name: call.name }));
}

export function webToolPrompt(opts: { research: boolean; today: string }): string {
  const base = [
    i18n.t("bench.tools.prompt.today", { today: opts.today }),
    i18n.t("bench.tools.prompt.tools"),
    i18n.t("bench.tools.prompt.when"),
    i18n.t("bench.tools.prompt.cite"),
    i18n.t("bench.tools.prompt.untrusted"),
  ];
  if (!opts.research) return base.join("\n");
  return [
    ...base,
    "",
    i18n.t("bench.tools.prompt.research"),
    i18n.t("bench.tools.prompt.step1"),
    i18n.t("bench.tools.prompt.step2"),
    i18n.t("bench.tools.prompt.step3"),
    i18n.t("bench.tools.prompt.step4"),
    i18n.t("bench.tools.prompt.step5"),
    i18n.t("bench.tools.prompt.conflicts"),
  ].join("\n");
}
