// Token unit-price conversions.
//
// The backend stores token unit prices as per-1K tokens denominated
// in platform points (1 积分 = 1 元). Humans and provider docs think
// per-1M, so these helpers only shift the K↔M scale; the unit label
// is the caller's responsibility (typically the points icon or a
// "积分 /1M" caption).

export type PriceUnit = "K" | "M";

// The currency a vendor publishes its list price in. Display-only:
// billing never converts (a point is a point), but "官方价" is a claim
// about someone else's price list, so it has to carry that list's own
// symbol or it reads as a 6.8× error to anyone who knows the real price.
export type PriceCurrency = "CNY" | "USD";

const CURRENCY_SYMBOL: Record<PriceCurrency, string> = { CNY: "¥", USD: "$" };

export function normalizeCurrency(raw: string | null | undefined): PriceCurrency {
  return String(raw ?? "").trim().toUpperCase() === "USD" ? "USD" : "CNY";
}

export function currencySymbol(raw: string | null | undefined): string {
  return CURRENCY_SYMBOL[normalizeCurrency(raw)];
}

function stripTrailingZeros(s: string): string {
  if (!s.includes(".")) return s;
  return s.replace(/\.?0+$/, "");
}

// Storage (per-1K) → display value as per chosen unit.
export function displayPrice(perKStr: string | undefined | null, unit: PriceUnit): string {
  if (perKStr == null || perKStr === "") return "";
  const n = parseFloat(perKStr);
  if (!Number.isFinite(n)) return "";
  const v = unit === "M" ? n * 1000 : n;
  return stripTrailingZeros(v.toFixed(10));
}

// User-typed value in chosen unit → string to send to backend (per-1K).
export function toStorage(displayStr: string | undefined | null, unit: PriceUnit): string {
  if (displayStr == null || displayStr === "") return "";
  const n = parseFloat(displayStr);
  if (!Number.isFinite(n)) return "";
  const v = unit === "M" ? n / 1000 : n;
  return stripTrailingZeros(v.toFixed(12));
}

export const UNIT_SUFFIX: Record<PriceUnit, string> = {
  K: "/1K",
  M: "/1M",
};
