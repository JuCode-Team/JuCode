// PointIcon / PointAmount: the platform-points unit marker.
//
// All user-facing amounts are platform points (1 积分 = 1 元, top-up
// 1:1). Instead of writing the word "积分" next to every number we mark
// amounts with a single coin glyph; reserve the plain-text wording for
// the few places that must explain the unit in prose (e.g. the top-up
// page's "1 元 = 1 积分" note).
import { Coins } from "lucide-react";
import { formatAmount } from "../lib/currency";

export function PointIcon({
  size = 14,
  className,
}: {
  size?: number;
  className?: string;
}) {
  return (
    <Coins
      size={size}
      strokeWidth={1.8}
      aria-hidden="true"
      className={className}
      style={{ display: "inline-block", verticalAlign: "-0.15em", flexShrink: 0 }}
    />
  );
}

export function PointAmount({
  amount,
  decimals = 2,
  iconSize = 14,
  icon = true,
  className,
}: {
  amount: number | string | null | undefined;
  decimals?: number | "preserve";
  iconSize?: number;
  // Dense tables name the unit once in the column header; pass false there
  // so the cell is just the number.
  icon?: boolean;
  className?: string;
}) {
  const text = formatAmount(amount, { decimals });
  if (!icon) return <span className={className}>{text}</span>;
  return (
    <span className={className} style={{ display: "inline-flex", alignItems: "baseline", gap: 3 }}>
      <PointIcon size={iconSize} />
      <span>{text}</span>
    </span>
  );
}
