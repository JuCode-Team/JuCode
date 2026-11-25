// 控制台统一的时间展示。列表用短格式（同年省略年份），详情用完整格式，
// 「最近使用」这类只关心新旧的位置用相对时间，并把完整时间放在 title 里。
import i18n from "../i18n";

const pad = (n: number) => String(n).padStart(2, "0");

function parse(value?: string | number | Date | null): Date | null {
  if (value == null || value === "") return null;
  const d = value instanceof Date ? value : new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** 列表格式：今年 → `09-27 14:56`，跨年 → `2025-12-01 14:56`；list-seconds 在列表格式后补秒（调用日志）。 */
export function formatDateTime(value?: string | number | Date | null, style: "list" | "list-seconds" | "full" | "date" = "list"): string {
  const d = parse(value);
  if (!d) return "—";
  const ymd = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const md = `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (style === "full") return `${ymd} ${hm}:${pad(d.getSeconds())}`;
  if (style === "date") return d.getFullYear() === new Date().getFullYear() ? md : ymd;
  const list = d.getFullYear() === new Date().getFullYear() ? `${md} ${hm}` : `${ymd} ${hm}`;
  return style === "list-seconds" ? `${list}:${pad(d.getSeconds())}` : list;
}

/** 相对时间：刚刚 / 12 分钟前 / 3 小时前 / 昨天 / 5 天前；超过 30 天退回日期。 */
export function formatRelative(value?: string | number | Date | null, now = Date.now()): string {
  const d = parse(value);
  if (!d) return "—";
  const diff = now - d.getTime();
  if (diff < 60_000) return i18n.t("usage.relative.justNow");
  const min = Math.floor(diff / 60_000);
  if (min < 60) return i18n.t("usage.relative.minutesAgo", { count: min });
  const hour = Math.floor(min / 60);
  if (hour < 24) return i18n.t("usage.relative.hoursAgo", { count: hour });
  const day = Math.floor(hour / 24);
  if (day === 1) return i18n.t("usage.relative.yesterday");
  if (day <= 30) return i18n.t("usage.relative.daysAgo", { count: day });
  return formatDateTime(d, "date");
}
