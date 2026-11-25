// Short user-facing labels for usage_logs.reason_code. The long sentence
// lives in `reason` (from backend ResolveUsageReason); the badge should
// name the outcome, not collapse every failure into "上游错误".

import i18n from "../i18n";
import { usage as zh } from "../i18n/zh/usage";

const REASON_CODES = new Set<string>(Object.keys(zh.reason));
const STATUS_CODES = new Set<string>(Object.keys(zh.status));

const USER_CAUSED = new Set([
  "client_canceled",
  "content_safety",
  "output_policy",
  "invalid_request",
  "request_body_too_large",
  "context_length_exceeded",
  "model_not_found",
]);

export function usageReasonLabel(reasonCode?: string, status?: string): string {
  if (status === "ok") return i18n.t("usage.status.ok");
  if (reasonCode && REASON_CODES.has(reasonCode)) {
    return i18n.t(`usage.reason.${reasonCode as keyof typeof zh.reason}`);
  }
  if (status && STATUS_CODES.has(status)) {
    return i18n.t(`usage.status.${status as keyof typeof zh.status}`);
  }
  return status || "—";
}

export function usageReasonColor(
  reasonCode?: string,
  status?: string,
): "green" | "orange" | "red" | "gray" {
  if (status === "ok") return "green";
  if (status === "quota_exceeded" || reasonCode === "account_quota_exceeded") {
    return "orange";
  }
  if (reasonCode && USER_CAUSED.has(reasonCode)) return "gray";
  if (status === "internal_error" || status === "upstream_error") return "red";
  return "gray";
}
