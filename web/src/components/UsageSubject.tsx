// UsageSubject labels what a usage row called: the model, or — for the web
// tools (endpoint web_search / web_fetch) — the tool engine, which is not a
// model and gets its own icon and name.
import { Globe } from "lucide-react";
import { isWebEngineId, isWebToolEndpoint, webEngineName } from "../api/webTools";
import ModelBrandIcon from "./ModelBrandIcon";

function isWebTool(endpoint: string | undefined, model: string): boolean {
  return endpoint ? isWebToolEndpoint(endpoint) : isWebEngineId(model);
}

export function usageSubjectName(endpoint: string | undefined, model: string): string {
  return isWebTool(endpoint, model) ? webEngineName(model) : model;
}

export function UsageSubjectIcon({ endpoint, model, size }: { endpoint?: string; model: string; size: number }) {
  if (isWebTool(endpoint, model)) return <Globe size={size} aria-hidden style={{ flexShrink: 0, color: "var(--gray-11)" }} />;
  return <ModelBrandIcon model={model} size={size} />;
}
