import { forwardRef } from "react";
import type { ReactNode, CSSProperties, MouseEvent } from "react";

// A quiet surface primitive for marketing pages — the same panel grammar the
// console gets from Radix <Card> (surface-container-low, hairline, 12px, no
// shadow). It deliberately contains no pointer-tracked lighting: hierarchy
// comes from its content, hairline and spacing.

export interface PremiumCardProps {
  children: ReactNode;
  /** Extra classes appended after "premium-card". */
  className?: string;
  /** Highlight variant — brand-colored 1px edge (.premium-pro-highlight). */
  highlight?: boolean;
  /** Kept for backwards compatibility; surfaces are intentionally static. */
  interactive?: boolean;
  /** Optional inline style merged onto the wrapper. */
  style?: CSSProperties;
  /** Click handler forwarded to the wrapper. */
  onClick?: (e: MouseEvent<HTMLDivElement>) => void;
}

const PremiumCard = forwardRef<HTMLDivElement, PremiumCardProps>(function PremiumCard(
  { children, className = "", highlight = false, style, onClick },
  forwardedRef,
) {
  const composed = [
    "premium-card",
    highlight ? "premium-pro-highlight" : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <div
      ref={forwardedRef}
      className={composed}
      style={style}
      onClick={onClick}
    >
      {children}
    </div>
  );
});

export default PremiumCard;
