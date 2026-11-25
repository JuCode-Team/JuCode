// Shared per-model color assignment for charts.
//
// colorsForModels is deterministic and collision-free within one chart:
// the hue comes from the vendor (so a Claude series is always violet and a
// GPT series always blue, on every page), and the lightness step comes from
// the model's rank among its vendor siblings in the chart. Two models in
// one chart never share a color; the same model keeps its hue across pages.
// Vendor → oklch hue. Brand-derived: violet is the request color, blue the
// response color, mint the "saved" color; the rest are spaced around the
// wheel so neighbours stay distinguishable at equal lightness.
const VENDOR_HUES: Array<[RegExp, number]> = [
  [/claude|anthropic/, 300],
  [/gpt|codex|chatgpt|openai|\bo[134]\b/, 245],
  [/gemini|google/, 165],
  [/deepseek/, 70],
  [/kimi|moonshot/, 350],
  [/qwen|qwq/, 200],
  [/grok|xai/, 275],
  [/doubao|seed/, 45],
  [/glm|zhipu|z\.ai/, 120],
  [/llama|meta|muse/, 20],
  [/mimo|xiaomi/, 95],
];
const FALLBACK_HUES = [230, 30, 140, 330, 80, 260];

function vendorHue(model: string): number {
  const name = model.toLowerCase();
  for (const [re, hue] of VENDOR_HUES) if (re.test(name)) return hue;
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return FALLBACK_HUES[h % FALLBACK_HUES.length];
}

// Lightness steps for the 1st…nth model of one vendor. Around 0.7 the same
// swatch reads on both the dark and the light surface.
const LIGHTNESS_STEPS = [0.7, 0.56, 0.82, 0.46];

export function colorsForModels(models: readonly string[]): Record<string, string> {
  const byHue = new Map<number, string[]>();
  for (const m of [...models].sort()) {
    const hue = vendorHue(m);
    const list = byHue.get(hue) ?? [];
    list.push(m);
    byHue.set(hue, list);
  }
  const out: Record<string, string> = {};
  for (const [hue, list] of byHue) {
    list.forEach((m, i) => {
      const l = LIGHTNESS_STEPS[i % LIGHTNESS_STEPS.length];
      const c = i < LIGHTNESS_STEPS.length ? 0.14 : 0.08;
      out[m] = `oklch(${l} ${c} ${hue})`;
    });
  }
  return out;
}
