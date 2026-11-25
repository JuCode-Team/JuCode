import OpenAI from "@lobehub/icons/es/OpenAI/components/Mono";
import Anthropic from "@lobehub/icons/es/Anthropic/components/Mono";
import Claude from "@lobehub/icons/es/Claude/components/Color";
import Gemini from "@lobehub/icons/es/Gemini/components/Color";
import DeepSeek from "@lobehub/icons/es/DeepSeek/components/Color";
import Qwen from "@lobehub/icons/es/Qwen/components/Color";
import Grok from "@lobehub/icons/es/Grok/components/Mono";
import Moonshot from "@lobehub/icons/es/Moonshot/components/Mono";
import Doubao from "@lobehub/icons/es/Doubao/components/Color";
import ChatGLM from "@lobehub/icons/es/ChatGLM/components/Color";
import Zhipu from "@lobehub/icons/es/Zhipu/components/Color";
import Meta from "@lobehub/icons/es/Meta/components/Color";
import XiaomiMiMo from "@lobehub/icons/es/XiaomiMiMo/components/Mono";
import Kimi from "@lobehub/icons/es/Kimi/components/Color";
import Minimax from "@lobehub/icons/es/Minimax/components/Color";
import ClaudeMono from "@lobehub/icons/es/Claude/components/Mono";
import GeminiMono from "@lobehub/icons/es/Gemini/components/Mono";
import DeepSeekMono from "@lobehub/icons/es/DeepSeek/components/Mono";
import QwenMono from "@lobehub/icons/es/Qwen/components/Mono";
import DoubaoMono from "@lobehub/icons/es/Doubao/components/Mono";
import ChatGLMMono from "@lobehub/icons/es/ChatGLM/components/Mono";
import ZhipuMono from "@lobehub/icons/es/Zhipu/components/Mono";
import MetaMono from "@lobehub/icons/es/Meta/components/Mono";
import KimiMono from "@lobehub/icons/es/Kimi/components/Mono";
import MinimaxMono from "@lobehub/icons/es/Minimax/components/Mono";
import { Box } from "lucide-react";
import type { ComponentType } from "react";
import { modelBrand, type ModelBrand } from "../lib/modelBrand";

export type { ModelBrand };
export { modelBrand };

type Icon = ComponentType<{ size?: number | string }>;

// Every vendor in @lobehub/icons ships a Mono glyph (fill = currentColor), so
// the default follows the text colour and stays visible in both themes.
// Vendors without a Color variant (OpenAI, Anthropic, Grok, Moonshot, MiMo)
// use Mono for both.
const MONO: Record<Exclude<ModelBrand, "other">, Icon> = {
  claude: ClaudeMono,
  anthropic: Anthropic,
  gemini: GeminiMono,
  deepseek: DeepSeekMono,
  qwen: QwenMono,
  grok: Grok,
  kimi: KimiMono,
  moonshot: Moonshot,
  chatglm: ChatGLMMono,
  zhipu: ZhipuMono,
  meta: MetaMono,
  mimo: XiaomiMiMo,
  doubao: DoubaoMono,
  minimax: MinimaxMono,
  openai: OpenAI,
};

const COLOR: Record<Exclude<ModelBrand, "other">, Icon> = {
  ...MONO,
  claude: Claude,
  gemini: Gemini,
  deepseek: DeepSeek,
  qwen: Qwen,
  kimi: Kimi,
  chatglm: ChatGLM,
  zhipu: Zhipu,
  meta: Meta,
  doubao: Doubao,
  minimax: Minimax,
};

interface ModelBrandIconProps {
  model: string;
  size?: number;
  // Single-colour glyph inheriting currentColor (default, site-wide rule).
  // Pass mono={false} only where the vendor's own colours are wanted.
  mono?: boolean;
}

export default function ModelBrandIcon({ model, size = 18, mono = true }: ModelBrandIconProps) {
  const brand = modelBrand(model);
  if (brand === "other") return <Box size={size} strokeWidth={1.7} />;
  const Icon = (mono ? MONO : COLOR)[brand];
  return <Icon size={size} />;
}
