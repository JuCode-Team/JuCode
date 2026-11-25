// Formatting for quota window reset times. The reset times themselves come
// from the backend, which counts each window from the plan's start_at.

import i18n, { localeTag } from "../i18n";

// fmtResetIn — render the time delta from now → reset in the most
// useful unit. Goal is a 2-character-ish glance value, not a precise
// duration. Falls back to "已重置" / "marche" when the bucket has
// already rolled (clock skew or stale render — the next refetch will
// fix the percentage anyway).
export function fmtResetIn(target: Date, now: Date = new Date()): string {
  const diffMs = target.getTime() - now.getTime();
  if (diffMs <= 0) return i18n.t("usage.resetIn.soon");
  const totalMin = Math.ceil(diffMs / 60_000);
  if (totalMin < 60) return i18n.t("usage.resetIn.minutes", { m: totalMin });
  const totalHrs = Math.floor(totalMin / 60);
  const remMin = totalMin % 60;
  if (totalHrs < 24) {
    return remMin > 0
      ? i18n.t("usage.resetIn.hoursMinutes", { h: totalHrs, m: remMin })
      : i18n.t("usage.resetIn.hours", { h: totalHrs });
  }
  const days = Math.floor(totalHrs / 24);
  const remHrs = totalHrs % 24;
  return remHrs > 0
    ? i18n.t("usage.resetIn.daysHours", { d: days, h: remHrs })
    : i18n.t("usage.resetIn.days", { d: days });
}

// fmtResetAt — absolute timestamp shown in a tooltip / secondary text.
// Locale-aware; falls back to the system default.
export function fmtResetAt(target: Date): string {
  return target.toLocaleString(localeTag(), {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}
