// AccountFormFields: the shared form vocabulary for the account surfaces —
// the AccountForm shape, the dialect/protocol constants, and the small layout
// primitives (Field, Section, ToggleRow) every account form reuses.
import { Field, FormSection, ToggleRow } from "../form/FormLayout";
import type { Account, AccountProtocol, HeaderRules, PathProtocol, PoolMode } from "../../api/endpoints";

// Known generation-endpoint dialect adaptors (mirrors the backend's base_url
// inference and the video/embedding adaptor dispatch).
// The web tool vendors (parallel / brave / jina / firecrawl) serve /tools/v1/*.
export const API_FORMATS = ["openai", "dashscope", "volcengine", "zhipu", "siliconflow", "minimax", "gemini", "kling", "vidu", "jimeng", "parallel", "brave", "jina", "firecrawl"] as const;

export const PROTOCOL_OPTIONS: { value: AccountProtocol; label: string }[] = [
  { value: "chat_completions", label: "Chat 补全（/v1/chat/completions）" },
  { value: "responses", label: "Responses（/v1/responses）" },
  { value: "messages", label: "Messages（/v1/messages）" },
  { value: "web", label: "网页工具（/tools/v1/search、/tools/v1/fetch）" },
];

export const PATH_PROTOCOL_OPTIONS = PROTOCOL_OPTIONS.filter(
  (o): o is { value: PathProtocol; label: string } => o.value !== "web",
);

export const PROTOCOL_SHORT_LABEL: Record<AccountProtocol, string> = {
  chat_completions: "chat_completions",
  responses: "responses",
  messages: "messages",
  web: "web",
};

// The path the gateway uses when the account declares no override.
export const DEFAULT_ENDPOINT_PATH: Record<PathProtocol, string> = {
  chat_completions: "/v1/chat/completions",
  responses: "/v1/responses",
  messages: "/v1/messages",
};

// HEADER_RULES_PLACEHOLDER shows every rule kind at once: move the key to the
// gateway's own header, and strip one beta flag the vendor rejects.
export const HEADER_RULES_PLACEHOLDER = `{
  "set": {"api-key": "{api_key}"},
  "remove": ["x-api-key"],
  "drop_values": {"anthropic-beta": ["advisor-tool-*"]}
}`;

// parseHeaderRules turns the form's JSON text into the update body. Blank text
// means "no rules" and is sent as {} so saving clears them.
export function parseHeaderRules(text: string): HeaderRules {
  const trimmed = text.trim();
  if (!trimmed) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    throw new Error("请求头规则不是合法的 JSON");
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("请求头规则必须是 JSON 对象");
  }
  return parsed as HeaderRules;
}

export interface AccountForm {
  name: string;
  base_url: string;
  api_format: string;
  organization: string;
  native_protocols: AccountProtocol[];
  default_protocol: AccountProtocol | "";
  allow_conversion: boolean;
  // 上游路径覆盖：方言 -> 路径。留空即用网关默认（/v1/...）。
  endpoint_paths: Partial<Record<AccountProtocol, string>>;
  // 上游请求头规则（JSON 文本），留空即不改写。
  header_rules: string;
  block_image_generation_tool: boolean;
  estimate_cache_write: boolean;
  cache_write_min_context_tokens: string;
  api_key: string;
  priority: string;
  weight: string;
  max_concurrency: string;
  pool_mode: PoolMode;
  note: string;
  enabled: boolean;
}

export function formFromAccount(a: Account): AccountForm {
  return {
    name: a.name ?? "",
    base_url: a.base_url ?? "",
    api_format: a.api_format ?? "",
    organization: a.organization ?? "",
    native_protocols: a.native_protocols?.length ? a.native_protocols : ["chat_completions"],
    default_protocol: (a.default_protocol as AccountProtocol) || "",
    allow_conversion: a.allow_conversion ?? false,
    endpoint_paths: { ...(a.endpoint_paths ?? {}) },
    header_rules: a.header_rules ? JSON.stringify(a.header_rules, null, 2) : "",
    block_image_generation_tool: a.block_image_generation_tool ?? false,
    estimate_cache_write: a.estimate_cache_write ?? false,
    cache_write_min_context_tokens: String(a.cache_write_min_context_tokens ?? 0),
    api_key: "",
    priority: String(a.priority ?? 100),
    weight: String(a.weight ?? 100),
    max_concurrency: String(a.max_concurrency ?? 0),
    pool_mode: a.pool_mode ?? "",
    note: a.note ?? "",
    // A health-disabled account (status error) is not taking traffic either;
    // switching it on here re-enables it.
    enabled: a.status === "active",
  };
}

export type SetFormField = <K extends keyof AccountForm>(key: K, value: AccountForm[K]) => void;

// Section / Field / ToggleRow are the shared form primitives (components/form).
// Section keeps the older two-argument shape the account tabs use and stacks
// its children with the standard 16px rhythm.
export function Section({ title, description, children }: { title: string; description?: React.ReactNode; children: React.ReactNode }) {
  return (
    <FormSection title={title} description={description}>
      <div className="jc-form-stack">{children}</div>
    </FormSection>
  );
}

export { Field, ToggleRow };
