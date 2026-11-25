import type { SiteSettings } from "../api/endpoints";
import i18n from "../i18n";

// resolveEndpoints fills in defaults from site_url when an admin hasn't
// overridden a specific protocol's base URL. Keeps the endpoints page (and
// the CC Switch deep links built below) useful even before settings are
// fully configured.
export function resolveEndpoints(s: SiteSettings | undefined): {
  openaiResponses: string;
  openaiCompat: string;
  anthropic: string;
  /** Site origin. Gemini CLI appends /v1beta itself. */
  gemini: string;
} {
  const base = (s?.site_url ?? "").replace(/\/+$/, "");
  return {
    openaiResponses: s?.endpoint_openai_responses || (base ? `${base}/v1` : ""),
    openaiCompat: s?.endpoint_openai_compat || (base ? `${base}/v1` : ""),
    // Anthropic SDKs (and Claude Code) append "/v1/messages" themselves.
    // The base URL surfaced to users here must therefore stop at
    // /anthropic — appending /v1 ourselves would double it to
    // /anthropic/v1/v1/messages at request time.
    anthropic: s?.endpoint_anthropic || (base ? `${base}/anthropic` : ""),
    gemini: base,
  };
}

// The CC Switch desktop app (github.com/farion1231/cc-switch) registers a
// ccswitch:// URL scheme that imports a provider in one click. Only the
// subset of its v1 protocol we actually rely on is modelled here.
//
// "claude" is Claude Code. "claude-desktop" is the desktop app. They share
// the Anthropic base URL and live in separate CC Switch stores.
// Released builds still reject app=claude-desktop in parse_provider_deeplink.
// Until that import lands, Claude Desktop is a guided mock of the desktop
// panel (tab, add, API key, base URL) rather than a ccswitch:// link.
// "grokbuild" is Grok (CC Switch's app id). "opencode" is accepted by the
// provider deep link and written as @ai-sdk/openai-compatible, which calls
// {endpoint}/chat/completions. "pi" is the Pi coding agent; current CC
// Switch builds reject that id on the deep link and expect the provider
// to be pasted into the Pi page.
export type CcSwitchApp = "claude" | "claude-desktop" | "codex" | "opencode" | "grokbuild" | "pi";

export const CC_SWITCH_SITE = "https://ccswitch.io/zh";

export interface CcSwitchProviderLink {
  app: CcSwitchApp;
  /** Display name of the provider inside CC Switch. */
  name: string;
  /** Base URL for this app. See ccSwitchEndpointFor. */
  endpoint: string;
  apiKey: string;
  /** Our site URL. See the homepage note below. */
  homepage?: string;
  /** Codex, OpenCode, and Grok write this into their config. Other apps omit it. */
  model?: string;
}

/**
 * Build a `ccswitch://` deep link that imports one provider.
 *
 * Protocol shape is stricter than it looks: "v1" is the URL *host*, not a
 * path segment, and the path must be exactly "/import". CC Switch rejects
 * anything else outright.
 */
export function buildCcSwitchLink(p: CcSwitchProviderLink): string {
  const params = new URLSearchParams({
    resource: "provider",
    app: p.app,
    name: p.name,
    endpoint: p.endpoint,
    apiKey: p.apiKey,
    // enabled=true does more than its name suggests: CC Switch switches
    // over to the freshly imported provider immediately. That is what
    // someone clicking "导入" expects, so we always send it.
    enabled: "true",
  });
  // Without an explicit homepage CC Switch infers one by stripping the
  // "api." prefix off the endpoint host (api.example.com → example.com),
  // which lands on a domain we may not even serve. Send ours when known.
  if (p.homepage) params.set("homepage", p.homepage);
  // CC Switch hardcodes model = "gpt-5-codex" when the link omits it, so
  // for codex we always want to say something explicit.
  if (p.model) params.set("model", p.model);
  return `ccswitch://v1/import?${params.toString()}`;
}

export interface CcSwitchEndpoints {
  anthropic: string;
  openaiResponses: string;
}

/**
 * Which public endpoint a CC Switch app should be pointed at.
 *
 * Claude Code and Claude Desktop share the Anthropic base and stay apart
 * by app id. Codex, OpenCode, Grok, and Pi speak OpenAI on /v1.
 */
export function ccSwitchEndpointFor(app: CcSwitchApp, endpoints: CcSwitchEndpoints): string {
  switch (app) {
    case "claude":
    case "claude-desktop":
      return endpoints.anthropic;
    default:
      return endpoints.openaiResponses;
  }
}

/**
 * Model written into Codex's CC Switch import when the user has not picked
 * another one. Matches the config.toml snippet on the endpoints page.
 * CC Switch otherwise hardcodes "gpt-5-codex".
 */
export const CODEX_DEFAULT_MODEL = "gpt-5.5";

/** Grok model matching the endpoints page. CC Switch otherwise uses grok-4.5. */
export const GROK_DEFAULT_MODEL = "grok-4.7";

/** Model written into an OpenCode import when the user has not picked one. */
export const OPENCODE_DEFAULT_MODEL = "gpt-5.5";

/**
 * Provider id written into client configs: the site name in lower case with
 * whitespace dropped, then reduced to the characters a TOML bare key allows.
 */
export function providerId(siteName: string): string {
  return siteName.toLowerCase().replace(/\s+/g, "").replace(/[^a-z0-9_-]/g, "") || "provider";
}

/** Environment variable holding the API key, e.g. JUCODE_API_KEY. */
export function apiKeyEnv(siteName: string): string {
  return `${providerId(siteName).toUpperCase().replace(/-/g, "_")}_API_KEY`;
}

export interface ManualImport {
  filename: string;
  body: string;
  note: string;
  /** Current CC Switch builds can import this app from a ccswitch:// link. */
  deepLink: boolean;
}

/**
 * Config the user can paste when they would rather not use the deep link.
 * Codex and Grok snippets follow the endpoints page, which is the shape
 * these CLIs accept against this gateway.
 */
export function manualImport(
  app: CcSwitchApp,
  input: { endpoint: string; apiKey: string; model?: string; siteName: string },
): ManualImport {
  const id = providerId(input.siteName);
  const name = input.siteName;
  const endpoint = input.endpoint || i18n.t("keys.ccSwitch.manual.notConfigured");
  const apiKey = input.apiKey || "sk-juc-...";
  const model = input.model || "";
  switch (app) {
    case "claude":
      return {
        filename: "shell",
        deepLink: true,
        note: i18n.t("keys.ccSwitch.manual.claude"),
        body: `export ANTHROPIC_BASE_URL=${endpoint}\nexport ANTHROPIC_API_KEY=${apiKey}`,
      };
    case "claude-desktop":
      return {
        filename: "Claude Desktop",
        deepLink: false,
        note: i18n.t("keys.ccSwitch.manual.claudeDesktop"),
        body: claudeDesktopProfile(endpoint, apiKey),
      };
    case "codex":
      return {
        filename: "~/.codex/config.toml",
        deepLink: true,
        note: i18n.t("keys.ccSwitch.manual.codex"),
        body: `model_provider = "${id}"
model = "${model || CODEX_DEFAULT_MODEL}"

[model_providers.${id}]
name = "${id}"
base_url = "${endpoint}"
wire_api = "responses"
experimental_bearer_token = "${apiKey}"`,
      };
    case "opencode":
      return {
        filename: "~/.config/opencode/opencode.json",
        deepLink: true,
        note: i18n.t("keys.ccSwitch.manual.opencode"),
        body: opencodeManual(endpoint, apiKey, model || OPENCODE_DEFAULT_MODEL, id, name),
      };
    case "grokbuild":
      return {
        filename: "~/.grok/config.toml",
        deepLink: true,
        note: i18n.t("keys.ccSwitch.manual.grok"),
        body: grokManual(endpoint, apiKey, model || GROK_DEFAULT_MODEL, input.siteName),
      };
    case "pi":
      return {
        filename: "~/.pi/agent/models.json",
        deepLink: false,
        note: i18n.t("keys.ccSwitch.manual.pi"),
        body: piManual(endpoint, apiKey, id, name),
      };
  }
}

function grokManual(endpoint: string, apiKey: string, model: string, siteName: string): string {
  const id = providerId(siteName);
  const env = apiKeyEnv(siteName);
  const block = (name: string, extra = "") => `[model.${name}]
${extra}base_url = "${endpoint}"
api_backend = "responses"
env_key = "${env}"`;
  return `[models]
default = "${id}"

${block(id, `model = "${model}"\nname = "${siteName}"\n`)}

${block("grok-4.7")}

${block("grok-4.6")}

# export ${env}=${apiKey}`;
}

function claudeDesktopProfile(endpoint: string, apiKey: string): string {
  return `{
  "inferenceProvider": "gateway",
  "inferenceGatewayBaseUrl": "${endpoint}",
  "inferenceGatewayAuthScheme": "bearer",
  "inferenceGatewayApiKey": "${apiKey}"
}`;
}

function opencodeManual(endpoint: string, apiKey: string, model: string, id: string, name: string): string {
  return `{
  "provider": {
    "${id}": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "${name}",
      "options": {
        "baseURL": "${endpoint}",
        "apiKey": "${apiKey}"
      },
      "models": {
        "${model}": { "name": "${model}" }
      }
    }
  }
}`;
}

function piManual(endpoint: string, apiKey: string, id: string, name: string): string {
  return `{
  "providers": {
    "${id}": {
      "name": "${name}",
      "baseUrl": "${endpoint}",
      "api": "openai-responses",
      "apiKey": "${apiKey}"
    }
  }
}`;
}
