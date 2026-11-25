// Presentation helpers for the per-row anomaly verdict on admin usage
// logs. The thresholds and reason tokens are defined once backend-side in
// internal/biz/anomaly.go; this file only translates them, so a token
// added there still renders (as itself) instead of vanishing.
//
// Time reasons (ttft/latency/retry) are a slow-request signal, error
// reasons mean the request failed for a cause we own — an operator reads
// the badge color first, so error wins whenever both are set.

import type { AnomalyFilter, UsageLogAnomaly } from "../api/endpoints";
import i18n from "../i18n";
import { errorClassLabel } from "./errorClass";

// Reason tokens and statuses with a label in the usage.anomaly namespace.
// Statuses only reach the reason list when the row has no error_class.
const REASON_KEYS = new Set(["ttft_ge_30s", "latency_ge_120s", "retry_ge_2"] as const);
const STATUS_KEYS = new Set(["upstream_error", "internal_error"] as const);
type AnomalyKey = "ttft_ge_30s" | "latency_ge_120s" | "retry_ge_2" | "upstream_error" | "internal_error";

export function anomalyReasonLabel(reason: string): string {
  if ((REASON_KEYS as Set<string>).has(reason)) return i18n.t(`usage.anomaly.${reason as AnomalyKey}`);
  if (reason.startsWith("status:")) {
    const status = reason.slice("status:".length);
    return (STATUS_KEYS as Set<string>).has(status) ? i18n.t(`usage.anomaly.${status as AnomalyKey}`) : status;
  }
  return errorClassLabel(reason);
}

// anomalyBadge returns null for a normal row (and for a backend that
// doesn't send the field at all), so callers can render nothing.
export function anomalyBadge(
  anomaly: UsageLogAnomaly | undefined | null,
): { color: "red" | "amber"; label: string; reasons: string[] } | null {
  if (!anomaly || (!anomaly.time && !anomaly.error)) return null;
  return {
    color: anomaly.error ? "red" : "amber",
    label: i18n.t("usage.anomaly.label"),
    reasons: (anomaly.reasons ?? []).map(anomalyReasonLabel),
  };
}

export const ANOMALY_FILTER_VALUES: AnomalyFilter[] = ["any", "time", "error", "none"];

// parseAnomalyFilter guards the value coming from the URL: the backend
// 400s on an unknown ?anomaly=, so a typo in a hand-edited link should
// fall back to "no filter" rather than break the page.
export function parseAnomalyFilter(value: string | null | undefined): AnomalyFilter | "" {
  return ANOMALY_FILTER_VALUES.includes(value as AnomalyFilter) ? (value as AnomalyFilter) : "";
}
