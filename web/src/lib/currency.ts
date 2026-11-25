// Amount formatting for platform points (平台积分).
//
// After the points cutover every monetary number in the system IS the
// point amount — 1 积分 = 1 元人民币, top-ups convert 1:1 and model
// prices are configured in points directly. There is no user-facing FX
// conversion anymore: historical CNY-denominated rows are already point
// amounts, and the backend no longer produces USD-tagged rows.
//
// These helpers only shape the number (thousands separators / decimal
// places). The unit itself is conveyed by the <PointIcon /> glyph (see
// components/PointAmount.tsx), not by a currency symbol or the word
// "积分" sprinkled across the UI.

// The backend sends money as quoted decimal strings because every monetary
// column is numeric(20,8). Number()/parseFloat re-introduces exactly the
// binary error that storage was chosen to avoid — visible as soon as the UI
// adds two of them ("0.1" + "0.2" renders 0.30000000000000004).
//
// So amounts are summed as scaled BigInt at the ledger's own 8-decimal
// resolution, mirroring biz.moneyToFixed on the Go side. No dependency: the
// only operation the UI needs is addition.
const MONEY_SCALE_EXP = 8;
const MONEY_SCALE = 10n ** BigInt(MONEY_SCALE_EXP);

// Parses a decimal string to a BigInt scaled by 1e8, rounding half away from
// zero — the same rule Postgres applies when storing numeric(20,8). Returns
// null for anything unparseable so callers can skip rather than poison a sum.
export function toFixedPoint(amount: string | number | null | undefined): bigint | null {
  if (amount == null || amount === "") return null;
  const s = String(amount).trim();
  const m = /^([+-]?)(\d*)(?:\.(\d*))?$/.exec(s);
  if (!m || (m[2] === "" && (m[3] ?? "") === "")) return null;
  const [, sign, whole, frac = ""] = m;
  // Pad to scale, keeping one extra digit to decide the rounding direction.
  const padded = (frac + "0".repeat(MONEY_SCALE_EXP + 1)).slice(0, MONEY_SCALE_EXP + 1);
  let n = BigInt((whole || "0") + padded.slice(0, MONEY_SCALE_EXP));
  if (Number(padded[MONEY_SCALE_EXP]) >= 5) n += 1n;
  return sign === "-" ? -n : n;
}

// Renders a scaled BigInt back to a plain decimal string with trailing
// fractional zeros removed.
export function fromFixedPoint(n: bigint): string {
  const neg = n < 0n;
  const abs = neg ? -n : n;
  const whole = abs / MONEY_SCALE;
  const frac = (abs % MONEY_SCALE).toString().padStart(MONEY_SCALE_EXP, "0").replace(/0+$/, "");
  return `${neg ? "-" : ""}${whole}${frac ? "." + frac : ""}`;
}

// Exact sum of server-sent amounts. Use instead of reduce(+Number(x)) anywhere
// the result is displayed as money.
export function sumAmounts(values: Array<string | number | null | undefined>): string {
  let total = 0n;
  for (const v of values) {
    const n = toFixedPoint(v);
    if (n !== null) total += n;
  }
  return fromFixedPoint(total);
}

export interface FormatOpts {
  // decimals: fixed fraction digits (default 2). "preserve" strips
  // trailing zeros but keeps whatever significant fraction the value
  // actually has — used for usage rows where a 0.00 would hide a
  // sub-cent cost.
  decimals?: number | "preserve";
}

export function formatAmount(
  amount: number | string | null | undefined,
  opts: FormatOpts = {},
): string {
  if (amount == null || amount === "") return "";
  const fp = toFixedPoint(amount);
  if (fp === null) return "";
  const decimals = opts.decimals ?? 2;
  if (decimals === "preserve") {
    return groupThousands(fromFixedPoint(fp));
  }
  const places = Math.max(0, Math.min(MONEY_SCALE_EXP, decimals));
  const scale = 10n ** BigInt(MONEY_SCALE_EXP - places);
  let n = fp;
  const half = scale / 2n;
  if (n >= 0n) n = (n + half) / scale * scale;
  else n = (n - half) / scale * scale;
  const raw = fromFixedPoint(n);
  const neg = raw.startsWith("-");
  const abs = neg ? raw.slice(1) : raw;
  const [whole, frac = ""] = abs.split(".");
  const fracPadded = frac.padEnd(places, "0").slice(0, places);
  const grouped = groupInt(whole);
  const body = places > 0 ? `${grouped}.${fracPadded}` : grouped;
  return neg ? `-${body}` : body;
}

function groupThousands(raw: string): string {
  const neg = raw.startsWith("-");
  const abs = neg ? raw.slice(1) : raw;
  const [whole, frac] = abs.split(".");
  const body = frac ? `${groupInt(whole)}.${frac}` : groupInt(whole);
  return neg ? `-${body}` : body;
}

function groupInt(whole: string): string {
  return whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}
