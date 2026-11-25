// How the playground speaks to the gateway. Claude uses the Messages API.
// GPT uses Responses while tools are on, other models use Chat Completions.
// Web search, page reading and drawing are functions the model chooses to
// call; the playground runs them and sends the results back.

import i18n from "../i18n";
import { LAB_AUTO_GROUP, type LabBillingSource } from "./labPick";

export interface LabTools {
  search: boolean;
  image: boolean;
}

export type LabSurface = "chat" | "responses" | "anthropic";

export interface EffortOption {
  value: string;
  label: string;
}

const CLAUDE = /^(claude|anthropic)([-.]|$)/i;
const OPENAI = /^(gpt|o[134]|chatgpt|codex)([-.]|$)/i;
const GROK = /^grok([-.]|$)/i;
const GEMINI = /^gemini([-.]|$)/i;
const IMAGE_TOOL = /^gpt-image-2(\.5)?(?:$|[-.])/i;
const IMAGE_MODEL = /^(gpt-image|dall-e)([-.]|$)/i;

export function isClaudeModel(name: string): boolean {
  return CLAUDE.test(name.trim());
}

export function isOpenAIModel(name: string): boolean {
  return OPENAI.test(name.trim());
}

export function effortOptions(model: string): EffortOption[] {
  const name = model.trim();
  if (!name) return [];
  const base = (values: string[]): EffortOption[] => [
    { value: "default", label: i18n.t("bench.protocol.effortDefault") },
    ...values.map((value) => ({ value, label: value })),
  ];
  if (/^gpt-image([-.]|$)/i.test(name)) return [];
  if (isClaudeModel(name)) return base(["low", "medium", "high", "xhigh"]);
  if (isOpenAIModel(name)) return base(["none", "low", "medium", "high", "xhigh"]);
  if (GROK.test(name) || GEMINI.test(name)) return base(["low", "medium", "high"]);
  return [];
}

export function labSurface(model: string, tools: LabTools): LabSurface {
  if (isClaudeModel(model)) return "anthropic";
  if (isOpenAIModel(model) && tools.search) return "responses";
  return "chat";
}

export function labPath(surface: LabSurface): string {
  if (surface === "anthropic") return "/anthropic/v1/messages";
  if (surface === "responses") return "/v1/responses";
  return "/v1/chat/completions";
}

// labConsolePath is the session-authenticated laboratory route. The public
// gateway paths stay on labPath; the lab key secret is not a client credential.
export function labConsolePath(surface: LabSurface): string {
  if (surface === "anthropic") return "/v1/me/lab/anthropic/messages";
  if (surface === "responses") return "/v1/me/lab/responses";
  return "/v1/me/lab/chat/completions";
}

/** Every text model gets the platform search tools. Image models do not chat. */
export function supportsWebSearch(model: string): boolean {
  const name = model.trim();
  return !!name && !IMAGE_MODEL.test(name);
}

export function supportsImageTool(model: string): boolean {
  return isOpenAIModel(model) && !/^gpt-image([-.]|$)/i.test(model.trim());
}

/** gpt-image-2 / gpt-image-2.5, including published suffixes, on the selected group. */
export function imageToolModel(names: string[], groupNames: string[]): string {
  const allowed = new Set(groupNames);
  const hits = names.filter((name) => IMAGE_TOOL.test(name.trim()) && allowed.has(name));
  hits.sort((a, b) => imageToolRank(a) - imageToolRank(b) || a.localeCompare(b));
  return hits[0] ?? "";
}

function imageToolRank(name: string): number {
  const n = name.trim().toLowerCase();
  if (n === "gpt-image-2") return 0;
  if (n === "gpt-image-2.5") return 1;
  if (n.startsWith("gpt-image-2.5")) return 2;
  return 3;
}

export interface LabHeaderInput {
  token?: string;
  orgId?: string;
  /** A group id pins that group. Empty or LAB_AUTO_GROUP leaves routing to the gateway. */
  group?: string;
  /** Narrows routing, pinned or automatic, to groups of this billing source. */
  billingSource?: LabBillingSource;
  json?: boolean;
}

export function labHeaders({ token, orgId, group, billingSource, json = true }: LabHeaderInput): Record<string, string> {
  const headers: Record<string, string> = {};
  if (json) headers["Content-Type"] = "application/json";
  if (token) headers.Authorization = `Bearer ${token}`;
  if (orgId) headers["X-Organization-Id"] = orgId;
  if (group && group !== LAB_AUTO_GROUP) headers["X-JuCode-Group"] = group;
  if (billingSource) headers["X-JuCode-Billing-Source"] = billingSource;
  return headers;
}

export interface ImageSizeChoice {
  label: string;
  size: string;
}

export interface ImageSizePolicy {
  flexible: boolean;
  presets: ImageSizeChoice[];
  maxEdge: number;
  multiple: number;
  maxRatio: number;
  minPixels: number;
  maxPixels: number;
  experimentalPixels: number;
}

// Built per call so the "auto" label follows the UI language.
const GPT2_PRESETS = (): ImageSizeChoice[] => [
  { label: i18n.t("bench.protocol.sizeAuto"), size: "auto" },
  { label: "1:1", size: "1024x1024" },
  { label: "3:2", size: "1536x1024" },
  { label: "2:3", size: "1024x1536" },
  { label: "16:9", size: "1536x864" },
  { label: "2K", size: "2560x1440" },
  { label: "4K", size: "3840x2160" },
];

const GPT1_PRESETS = (): ImageSizeChoice[] => [
  { label: i18n.t("bench.protocol.sizeAuto"), size: "auto" },
  { label: "1:1", size: "1024x1024" },
  { label: "3:2", size: "1536x1024" },
  { label: "2:3", size: "1024x1536" },
];

const FLEX_LIMITS = {
  flexible: true,
  maxEdge: 3840,
  multiple: 16,
  maxRatio: 3,
  minPixels: 655_360,
  maxPixels: 8_294_400,
  experimentalPixels: 3_686_400,
};

/** Official sizes. gpt-image-2 / 2.5 accept custom WxH inside the documented limits. */
export function imageSizePolicy(model: string): ImageSizePolicy {
  const name = model.trim().toLowerCase();
  if (/^gpt-image-2(\.5)?(?:$|[-.])/.test(name)) return { ...FLEX_LIMITS, presets: GPT2_PRESETS() };
  if (/^gpt-image-1/.test(name)) return { ...FLEX_LIMITS, flexible: false, presets: GPT1_PRESETS() };
  if (name.startsWith("dall-e-3")) {
    return { ...FLEX_LIMITS, flexible: false, presets: [
      { label: "1:1", size: "1024x1024" },
      { label: i18n.t("bench.protocol.sizeLandscape"), size: "1792x1024" },
      { label: i18n.t("bench.protocol.sizePortrait"), size: "1024x1792" },
    ] };
  }
  if (name.startsWith("dall-e-2")) {
    return { ...FLEX_LIMITS, flexible: false, presets: [
      { label: "256", size: "256x256" },
      { label: "512", size: "512x512" },
      { label: "1024", size: "1024x1024" },
    ] };
  }
  return { ...FLEX_LIMITS, flexible: false, presets: GPT1_PRESETS().filter((item) => item.size !== "auto") };
}

/** How many images a generation may send. GPT Image edits accept 16 inputs. dall-e-2 accepts one. dall-e-3 cannot take an image. */
export function imageReferenceLimit(model: string): number {
  const name = model.trim().toLowerCase();
  if (name.startsWith("dall-e-3")) return 0;
  if (name.startsWith("dall-e-2")) return 1;
  return 16;
}

/** An edit request may include extra inputs, but the mask applies only to the first image and the call returns new pictures, not one edit per input. The lab edits exactly one image. */
export function imageEditLimit(model: string): number {
  return imageReferenceLimit(model) > 0 ? 1 : 0;
}

export function imageQualityOptions(model: string): string[] {
  const name = model.trim().toLowerCase();
  if (/^gpt-image-2\.5(?:$|[-.])/.test(name)) return ["auto", "low", "medium", "high", "xhigh", "max"];
  if (/^gpt-image-/.test(name)) return ["auto", "low", "medium", "high"];
  return ["auto"];
}

/** gpt-image-2 and 2.5 always read inputs at high fidelity, so the field is omitted. */
export function imageSupportsFidelity(model: string): boolean {
  return /^gpt-image-1/.test(model.trim().toLowerCase());
}

export function imageSizeError(model: string, size: string): string {
  const policy = imageSizePolicy(model);
  const value = size.trim().toLowerCase();
  if (value === "auto") return policy.presets.some((item) => item.size === "auto") ? "" : i18n.t("bench.protocol.sizeNoAuto");
  const match = /^(\d+)x(\d+)$/.exec(value);
  if (!match) return i18n.t("bench.protocol.sizeFormat");
  const width = Number(match[1]);
  const height = Number(match[2]);
  if (!policy.flexible) {
    return policy.presets.some((item) => item.size === value) ? "" : i18n.t("bench.protocol.sizeListedOnly");
  }
  if (width % policy.multiple !== 0 || height % policy.multiple !== 0) return i18n.t("bench.protocol.sizeMultiple", { n: policy.multiple });
  if (width > policy.maxEdge || height > policy.maxEdge) return i18n.t("bench.protocol.sizeMaxEdge", { n: policy.maxEdge });
  const longEdge = Math.max(width, height);
  const shortEdge = Math.min(width, height);
  if (shortEdge === 0 || longEdge / shortEdge > policy.maxRatio) return i18n.t("bench.protocol.sizeRatio", { n: policy.maxRatio });
  const pixels = width * height;
  if (pixels < policy.minPixels) return i18n.t("bench.protocol.sizeMinPixels", { n: policy.minPixels });
  if (pixels > policy.maxPixels) return i18n.t("bench.protocol.sizeMaxPixels", { n: policy.maxPixels });
  return "";
}

export const GENERATE_IMAGE_TOOL = "generate_image";
export const WEB_SEARCH_TOOL = "web_search";
export const WEB_FETCH_TOOL = "web_fetch";

export type LabToolName = "web_search" | "web_fetch" | "generate_image";

interface ToolSpec {
  name: LabToolName;
  description: string;
  parameters: Record<string, unknown>;
}

// Built per call so tool descriptions follow the UI language.
const TOOL_SPECS = (): Record<LabToolName, ToolSpec> => ({
  web_search: {
    name: WEB_SEARCH_TOOL,
    description: i18n.t("bench.protocol.searchDesc"),
    parameters: {
      type: "object",
      properties: {
        query: { type: "string", description: i18n.t("bench.protocol.searchQuery") },
        max_results: { type: "integer", minimum: 1, maximum: 10, description: i18n.t("bench.protocol.searchMax") },
        freshness: { type: "string", enum: ["day", "week", "month", "year"], description: i18n.t("bench.protocol.searchFreshness") },
      },
      required: ["query"],
    },
  },
  web_fetch: {
    name: WEB_FETCH_TOOL,
    description: i18n.t("bench.protocol.fetchDesc"),
    parameters: {
      type: "object",
      properties: {
        url: { type: "string", description: i18n.t("bench.protocol.fetchUrl") },
      },
      required: ["url"],
    },
  },
  generate_image: {
    name: GENERATE_IMAGE_TOOL,
    description: i18n.t("bench.protocol.imageDesc"),
    parameters: {
      type: "object",
      properties: {
        prompt: { type: "string", description: i18n.t("bench.protocol.imagePrompt") },
      },
      required: ["prompt"],
    },
  },
});

/** Function tools in the shape each surface expects. */
export function labToolDefs(surface: LabSurface, tools: LabTools): Array<Record<string, unknown>> {
  const names: LabToolName[] = [
    ...(tools.search ? [WEB_SEARCH_TOOL, WEB_FETCH_TOOL] as const : []),
    ...(tools.image ? [GENERATE_IMAGE_TOOL] as const : []),
  ];
  const specs = TOOL_SPECS();
  return names.map((name) => {
    const spec = specs[name];
    if (surface === "anthropic") return { name: spec.name, description: spec.description, input_schema: spec.parameters };
    if (surface === "responses") return { type: "function", name: spec.name, description: spec.description, parameters: spec.parameters };
    return { type: "function", function: { name: spec.name, description: spec.description, parameters: spec.parameters } };
  });
}

export interface LabRequestOptions {
  model: string;
  input: unknown;
  tools: Array<Record<string, unknown>>;
  effort?: string;
  system?: string;
  /** Tool rounds are used up: the model must answer in text. */
  forceAnswer?: boolean;
}

export function labRequestBody(surface: LabSurface, o: LabRequestOptions): Record<string, unknown> {
  const tools = o.tools.length ? { tools: o.tools } : {};
  const none = o.forceAnswer && o.tools.length > 0;
  if (surface === "anthropic") {
    return {
      model: o.model,
      max_tokens: 8192,
      stream: true,
      ...(o.system ? { system: o.system } : {}),
      messages: o.input,
      ...tools,
      ...(none ? { tool_choice: { type: "none" } } : {}),
      ...(o.effort ? { output_config: { effort: o.effort } } : {}),
    };
  }
  if (surface === "responses") {
    return {
      model: o.model,
      stream: true,
      ...(o.system ? { instructions: o.system } : {}),
      input: o.input,
      ...tools,
      ...(none ? { tool_choice: "none" } : {}),
      ...(o.effort ? { reasoning: { effort: o.effort } } : {}),
    };
  }
  const messages = Array.isArray(o.input) ? o.input : [];
  return {
    model: o.model,
    stream: true,
    messages: o.system ? [{ role: "system", content: o.system }, ...messages] : messages,
    ...tools,
    ...(none ? { tool_choice: "none" } : {}),
    ...(o.effort ? { reasoning_effort: o.effort } : {}),
  };
}

export interface LabDelta {
  text: string;
  reasoning: string;
  images: string[];
}

function imageResult(raw: unknown): string {
  if (typeof raw !== "string" || !raw) return "";
  if (raw.startsWith("data:") || raw.startsWith("http")) return raw;
  return `data:image/png;base64,${raw}`;
}

/** One SSE JSON payload from chat, Responses, or Anthropic Messages. */
export function readLabDelta(payload: Record<string, unknown>): LabDelta {
  const empty: LabDelta = { text: "", reasoning: "", images: [] };
  const choice = (payload.choices as Array<Record<string, unknown>> | undefined)?.[0];
  const chatDelta = choice?.delta as Record<string, unknown> | undefined;
  if (chatDelta) {
    return {
      text: typeof chatDelta.content === "string" ? chatDelta.content : "",
      reasoning: typeof chatDelta.reasoning_content === "string" ? chatDelta.reasoning_content : "",
      images: [],
    };
  }
  if (payload.type === "response.output_text.delta" && typeof payload.delta === "string") {
    return { ...empty, text: payload.delta };
  }
  if (payload.type === "content_block_delta") {
    const delta = payload.delta as Record<string, unknown> | undefined;
    if (delta?.type === "text_delta" && typeof delta.text === "string") return { ...empty, text: delta.text };
    if (delta?.type === "thinking_delta" && typeof delta.thinking === "string") return { ...empty, reasoning: delta.thinking };
  }
  if (payload.type === "response.output_item.done") {
    const item = payload.item as Record<string, unknown> | undefined;
    const src = imageResult(item?.result);
    if (item?.type === "image_generation_call" && src) return { ...empty, images: [src] };
  }
  return empty;
}

export interface LabToolPatch {
  index?: number;
  id?: string;
  callId?: string;
  name?: LabToolName;
  inputDelta?: string;
  input?: string;
  done?: boolean;
}

export interface LabToolAcc {
  id: string;
  callId?: string;
  index?: number;
  name: LabToolName;
  input: string;
  done: boolean;
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : undefined;
}

function asText(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function knownTool(value: unknown): LabToolName | undefined {
  if (value === GENERATE_IMAGE_TOOL || value === WEB_SEARCH_TOOL || value === WEB_FETCH_TOOL) return value;
  return undefined;
}

function objectInput(value: unknown): string | undefined {
  const record = asRecord(value);
  return record && Object.keys(record).length > 0 ? JSON.stringify(record) : undefined;
}

/** Function calls inside one SSE payload. Index-only fragments update an existing call; they never invent one. */
export function readLabTools(payload: Record<string, unknown>): { patches: LabToolPatch[]; stop: boolean } {
  const patches: LabToolPatch[] = [];
  let stop = false;
  const choice = (payload.choices as Array<Record<string, unknown>> | undefined)?.[0];
  if (choice) {
    if (choice.finish_reason === "tool_calls") stop = true;
    const calls = asRecord(choice.delta)?.tool_calls;
    if (Array.isArray(calls)) {
      for (const raw of calls) {
        const call = asRecord(raw);
        if (!call) continue;
        const fn = asRecord(call.function);
        const name = knownTool(fn?.name);
        if (fn?.name && !name) continue;
        const args = asText(fn?.arguments);
        const index = typeof call.index === "number" ? call.index : undefined;
        const id = asText(call.id) || undefined;
        if (!name && !id && !args && index === undefined) continue;
        patches.push({
          index,
          id,
          ...(name ? { name } : {}),
          ...(args ? { inputDelta: args } : {}),
        });
      }
    }
    return { patches, stop };
  }

  const type = asText(payload.type);
  if (type === "content_block_start") {
    const block = asRecord(payload.content_block);
    const index = typeof payload.index === "number" ? payload.index : undefined;
    const name = knownTool(block?.name);
    if (asText(block?.type) === "tool_use" && name) {
      const input = objectInput(block?.input);
      patches.push({ index, id: asText(block?.id) || undefined, name, ...(input ? { input } : {}) });
    }
  }
  if (type === "content_block_delta") {
    const delta = asRecord(payload.delta);
    if (delta?.type === "input_json_delta" && typeof delta.partial_json === "string" && typeof payload.index === "number") {
      patches.push({ index: payload.index, inputDelta: delta.partial_json });
    }
  }
  if (type === "content_block_stop" && typeof payload.index === "number") {
    patches.push({ index: payload.index, done: true });
  }
  if (type === "message_delta" && asText(asRecord(payload.delta)?.stop_reason) === "tool_use") stop = true;

  if (type === "response.output_item.added" || type === "response.output_item.done") {
    const item = asRecord(payload.item);
    const name = knownTool(item?.name);
    if (item?.type === "function_call" && name) {
      const done = type === "response.output_item.done";
      const args = asText(item.arguments);
      patches.push({
        id: asText(item.id) || undefined,
        callId: asText(item.call_id) || undefined,
        name,
        ...(args ? { input: args } : {}),
        done,
      });
      if (done) stop = true;
    }
  }
  if (type === "response.function_call_arguments.delta" && typeof payload.delta === "string") {
    patches.push({
      id: asText(payload.item_id) || asText(payload.call_id) || undefined,
      index: typeof payload.output_index === "number" ? payload.output_index : undefined,
      inputDelta: payload.delta,
    });
  }
  if (type === "response.function_call_arguments.done") {
    const args = asText(payload.arguments);
    patches.push({
      id: asText(payload.item_id) || undefined,
      index: typeof payload.output_index === "number" ? payload.output_index : undefined,
      ...(args ? { input: args } : {}),
      done: true,
    });
  }
  if (type === "response.completed") {
    const output = asRecord(payload.response)?.output;
    if (Array.isArray(output)) {
      for (const raw of output) {
        const item = asRecord(raw);
        const name = knownTool(item?.name);
        if (item?.type !== "function_call" || !name) continue;
        stop = true;
        const args = asText(item.arguments);
        patches.push({
          id: asText(item.id) || undefined,
          callId: asText(item.call_id) || undefined,
          name,
          ...(args ? { input: args } : {}),
          done: true,
        });
      }
    }
  }
  return { patches, stop };
}

function findTool(tools: LabToolAcc[], patch: LabToolPatch): number {
  if (patch.id) {
    return tools.findIndex((item) => item.id === patch.id || item.callId === patch.id);
  }
  if (patch.callId) {
    const byCall = tools.findIndex((item) => item.callId === patch.callId || item.id === patch.callId);
    if (byCall >= 0) return byCall;
  }
  if (patch.index !== undefined) return tools.findIndex((item) => item.index === patch.index);
  return -1;
}

export function foldLabTool(tools: LabToolAcc[], patch: LabToolPatch): LabToolAcc[] {
  const next = tools.map((item) => ({ ...item }));
  const at = findTool(next, patch);
  if (at < 0) {
    if (!patch.name) return next;
    const created: LabToolAcc = {
      id: patch.id || `tool-${patch.index ?? next.length}`,
      callId: patch.callId,
      index: patch.index,
      name: patch.name,
      input: patch.input || "",
      done: patch.done === true,
    };
    if (patch.inputDelta) created.input += patch.inputDelta;
    next.push(created);
    return next;
  }
  const item = next[at];
  if (patch.id && (item.id === patch.id || item.id.startsWith("tool-"))) item.id = patch.id;
  if (patch.callId) item.callId = patch.callId;
  if (patch.index !== undefined) item.index = patch.index;
  if (patch.name) item.name = patch.name;
  if (patch.input) item.input = patch.input;
  if (patch.inputDelta) item.input += patch.inputDelta;
  if (patch.done) item.done = true;
  return next;
}

/** Arguments of a finished call. Broken JSON gives an empty object. */
export function parseToolArgs(input: string): Record<string, unknown> {
  try {
    return asRecord(JSON.parse(input.trim() || "{}")) ?? {};
  } catch {
    return {};
  }
}

/** The one field worth showing while a call streams: prompt, query or url. */
export function labToolDetail(input: string): string {
  const trimmed = input.trim();
  if (!trimmed) return "";
  const record = parseToolArgs(trimmed);
  const value = record.prompt ?? record.query ?? record.url;
  if (typeof value === "string" && value.trim()) return value.trim();
  const match = /"(?:prompt|query|url)"\s*:\s*"((?:\\.|[^"\\])*)/.exec(trimmed);
  if (!match) return trimmed.startsWith("{") ? "" : trimmed;
  return match[1].replace(/\\n/g, " ").replace(/\\"/g, "\"").replace(/\\\\/g, "\\");
}

export type AnthropicBlock = Record<string, unknown>;

/** Rebuilds the assistant content blocks of one Messages stream, so thinking signatures go back with tool results. */
export function foldAnthropicBlocks(blocks: AnthropicBlock[], payload: Record<string, unknown>): AnthropicBlock[] {
  const index = payload.index;
  if (typeof index !== "number") return blocks;
  if (payload.type === "content_block_start") {
    const block = asRecord(payload.content_block);
    if (!block) return blocks;
    const next = [...blocks];
    const kind = asText(block.type);
    if (kind === "tool_use") next[index] = { type: "tool_use", id: asText(block.id), name: asText(block.name), partial: objectInput(block.input) ?? "" };
    else if (kind === "text") next[index] = { type: "text", text: asText(block.text) };
    else if (kind === "thinking") next[index] = { type: "thinking", thinking: asText(block.thinking), signature: asText(block.signature) };
    else next[index] = { ...block };
    return next;
  }
  if (payload.type !== "content_block_delta") return blocks;
  const current = blocks[index];
  const delta = asRecord(payload.delta);
  if (!current || !delta) return blocks;
  const next = [...blocks];
  const block = { ...current };
  if (delta.type === "text_delta") block.text = asText(block.text) + asText(delta.text);
  else if (delta.type === "thinking_delta") block.thinking = asText(block.thinking) + asText(delta.thinking);
  else if (delta.type === "signature_delta") block.signature = asText(block.signature) + asText(delta.signature);
  else if (delta.type === "input_json_delta") block.partial = asText(block.partial) + asText(delta.partial_json);
  else return blocks;
  next[index] = block;
  return next;
}

export interface ToolRunResult {
  id: string;
  callId?: string;
  name: LabToolName;
  arguments: string;
  /** Text handed back to the model. */
  output: string;
  isError?: boolean;
}

export interface AssistantToolTurn {
  text: string;
  /** Chat Completions reasoning_content of this round. Some reasoning models need it back. */
  reasoning?: string;
  /** Anthropic content blocks of this round, from foldAnthropicBlocks. */
  blocks?: AnthropicBlock[];
  calls: ToolRunResult[];
}

function anthropicContent(turn: AssistantToolTurn): AnthropicBlock[] {
  const wanted = new Set(turn.calls.map((call) => call.id));
  const out: AnthropicBlock[] = [];
  const seen = new Set<string>();
  for (const block of turn.blocks ?? []) {
    if (!block) continue;
    const kind = asText(block.type);
    if (kind === "text" && asText(block.text).trim()) out.push({ type: "text", text: block.text });
    else if (kind === "thinking" && asText(block.signature)) out.push({ type: "thinking", thinking: asText(block.thinking), signature: block.signature });
    else if (kind === "redacted_thinking") out.push(block);
    else if (kind === "tool_use" && wanted.has(asText(block.id))) {
      seen.add(asText(block.id));
      out.push({ type: "tool_use", id: block.id, name: block.name, input: parseToolArgs(asText(block.partial)) });
    }
  }
  if (!turn.blocks && turn.text.trim()) out.push({ type: "text", text: turn.text });
  for (const call of turn.calls) {
    if (seen.has(call.id)) continue;
    out.push({ type: "tool_use", id: call.id, name: call.name, input: parseToolArgs(call.arguments) });
  }
  return out;
}

/** The conversation after one tool round: the assistant's calls, then their results. */
export function appendToolTurn(
  surface: LabSurface,
  input: Array<Record<string, unknown>>,
  turn: AssistantToolTurn,
): Array<Record<string, unknown>> {
  if (surface === "anthropic") {
    return [
      ...input,
      { role: "assistant", content: anthropicContent(turn) },
      {
        role: "user",
        content: turn.calls.map((call) => ({
          type: "tool_result",
          tool_use_id: call.id,
          content: call.output,
          ...(call.isError ? { is_error: true } : {}),
        })),
      },
    ];
  }
  if (surface === "responses") {
    return [
      ...input,
      ...(turn.text ? [{ role: "assistant", content: turn.text }] : []),
      ...turn.calls.map((call) => ({
        type: "function_call",
        call_id: call.callId || call.id,
        name: call.name,
        arguments: call.arguments || "{}",
      })),
      ...turn.calls.map((call) => ({
        type: "function_call_output",
        call_id: call.callId || call.id,
        output: call.output,
      })),
    ];
  }
  return [
    ...input,
    {
      role: "assistant",
      content: turn.text || null,
      ...(turn.reasoning ? { reasoning_content: turn.reasoning } : {}),
      tool_calls: turn.calls.map((call) => ({
        id: call.callId || call.id,
        type: "function",
        function: { name: call.name, arguments: call.arguments || "{}" },
      })),
    },
    ...turn.calls.map((call) => ({
      role: "tool",
      tool_call_id: call.callId || call.id,
      content: call.output,
    })),
  ];
}
