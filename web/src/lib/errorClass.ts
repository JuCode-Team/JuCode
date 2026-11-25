// Chinese labels for the gateway's error_class values. Mirrors the enum in
// JuCode-backend/internal/llm/error_class.go — the admin console used to show
// the raw token ("cyber_block"), which reads as a leak of internal naming and
// doesn't match how the rest of the console words the same thing.
//
// cyber_block is the 风控 class: an upstream content-safety / moderation /
// content-filter rejection. It is labelled "内容触发风控" to line up with the
// wording already used by the keyword & moderation pipeline (留证 "命中风控",
// 用户异常 kind=cyber_security).

import i18n from "../i18n";
import { usage as zh } from "../i18n/zh/usage";

type ErrorClassKey = keyof typeof zh.errorClass;
const CLASSES = new Set<string>(Object.keys(zh.errorClass));

// errorClassLabel returns the Chinese label for a class, falling back to the
// raw value so a class added backend-side still renders (as its token) instead
// of vanishing.
export function errorClassLabel(cls: string): string {
  // "none" is only the namespace key for the literal "(none)" bucket.
  const key = cls === "(none)" ? "none" : cls === "none" ? "" : cls;
  return CLASSES.has(key) ? i18n.t(`usage.errorClass.${key as ErrorClassKey}`) : cls;
}

// errorClassColor picks the badge tone: 风控/审核 blocks are caused by the
// request's content (amber — operator action is "look at the prompt"), genuine
// upstream faults are red, everything else stays gray.
export function errorClassColor(cls: string): "red" | "amber" | "gray" {
  if (cls === "ok") return "gray";
  if (cls === "cyber_block" || cls === "user_policy" || cls === "user_request") return "amber";
  return "red";
}
