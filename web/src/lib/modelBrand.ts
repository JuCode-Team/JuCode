import i18n from "../i18n";

export type ModelBrand =
  | "claude"
  | "anthropic"
  | "gemini"
  | "deepseek"
  | "qwen"
  | "grok"
  | "kimi"
  | "moonshot"
  | "chatglm"
  | "zhipu"
  | "meta"
  | "mimo"
  | "doubao"
  | "minimax"
  | "openai"
  | "other";

export function modelBrand(model: string): ModelBrand {
  const name = model.toLowerCase();
  if (name.includes("claude")) return "claude";
  if (name.includes("anthropic")) return "anthropic";
  if (name.includes("gemini") || name.includes("google")) return "gemini";
  if (name.includes("deepseek")) return "deepseek";
  if (name.includes("qwen") || name.includes("qwq") || name.includes("alibaba") || name.includes("通义")) return "qwen";
  if (name.includes("grok") || name.includes("x.ai") || name.includes("xai")) return "grok";
  if (name.includes("kimi")) return "kimi";
  if (name.includes("moonshot")) return "moonshot";
  if (name.includes("glm") || name.includes("chatglm") || name.includes("智谱")) return "chatglm";
  if (name.includes("zhipu") || name.includes("z.ai")) return "zhipu";
  if (name.includes("muse") || name.includes("llama") || /\bmeta\b/.test(name)) return "meta";
  if (name.includes("mimo") || name.includes("xiaomi")) return "mimo";
  if (name.includes("doubao") || name.includes("bytedance") || name.includes("字节")) return "doubao";
  if (name.includes("minimax")) return "minimax";
  if (/\b(gpt|o[134]|codex|chatgpt|openai)/.test(name)) return "openai";
  return "other";
}

export interface VendorChoice {
  key: string;
  label: string;
  count: number;
}

// Fixed brand names; zhipu/doubao are localized at call time in vendorName.
const VENDOR_LABEL: Record<string, string> = {
  openai: "OpenAI",
  claude: "Claude",
  grok: "xAI",
  gemini: "Gemini",
  deepseek: "DeepSeek",
  qwen: "Qwen",
  kimi: "Kimi",
  meta: "Meta",
  mimo: "MiMo",
  minimax: "MiniMax",
};

const VENDOR_ORDER = ["openai", "claude", "grok", "gemini", "deepseek", "qwen", "kimi", "zhipu", "doubao", "meta", "mimo", "minimax"];

export function vendorKey(name: string): string {
  const brand = modelBrand(name);
  if (brand === "anthropic" || brand === "claude") return "claude";
  if (brand === "kimi" || brand === "moonshot") return "kimi";
  if (brand === "chatglm" || brand === "zhipu") return "zhipu";
  if (brand === "grok") return "grok";
  if (brand === "other") return name.trim().toLowerCase() || "other";
  return brand;
}

function vendorName(key: string): string | undefined {
  if (key === "zhipu") return i18n.t("keys.vendors.zhipu");
  if (key === "doubao") return i18n.t("keys.vendors.doubao");
  return VENDOR_LABEL[key];
}

export function vendorLabel(name: string): string {
  const key = vendorKey(name);
  return vendorName(key) ?? name.trim();
}

export function groupVendorKeys(group: { vendors?: string[]; models?: string[] }): string[] {
  const source = group.vendors?.length ? group.vendors : (group.models ?? []);
  return [...new Set(source.map(vendorKey))];
}

export function vendorChoices(groups: { vendors?: string[]; models?: string[] }[]): VendorChoice[] {
  const counts = new Map<string, { label: string; count: number }>();
  for (const group of groups) {
    const seen = new Set<string>();
    const source = group.vendors?.length ? group.vendors : (group.models ?? []);
    for (const name of source) {
      const key = vendorKey(name);
      if (seen.has(key)) continue;
      seen.add(key);
      const current = counts.get(key) ?? { label: vendorLabel(name), count: 0 };
      current.count += 1;
      counts.set(key, current);
    }
  }
  return [...counts.entries()]
    .map(([key, value]) => ({ key, label: vendorName(key) ?? value.label, count: value.count }))
    .sort((a, b) => {
      const ai = VENDOR_ORDER.indexOf(a.key);
      const bi = VENDOR_ORDER.indexOf(b.key);
      if (ai === -1 && bi === -1) return a.label.localeCompare(b.label, "zh");
      if (ai === -1) return 1;
      if (bi === -1) return -1;
      return ai - bi;
    });
}

export function matchesVendor(group: { vendors?: string[]; models?: string[] }, key: string | null): boolean {
  if (!key) return true;
  return groupVendorKeys(group).includes(key);
}
