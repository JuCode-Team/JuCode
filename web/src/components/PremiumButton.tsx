import { type ReactNode, type CSSProperties, type MouseEvent } from "react";
import { Link } from "react-router-dom";

// PremiumButton wraps the marketing-page CTA buttons that ship as
// `<Link className="btn-premium-primary px-8 py-3 font-medium text-[14px]">`
// or `<a className="btn-premium-secondary ...">` scattered around Home /
// Navbar / UserPlans. The visual is pure CSS (`.btn-premium-primary` /
// `.btn-premium-secondary` in index.css); this component enforces a
// consistent size + padding scale and picks the right underlying element based on
// which prop the caller supplies.
//
// Polymorphic via prop priority — `to` (router Link) → `href` (anchor) →
// `onClick`/native button. Mirrors how the call-sites used them before
// extraction; keeps callers from having to think about element choice
// twice.

// Four tiers, mirrored 1:1 by the Radix Button skin in console-radix.css
// (solid / soft gray / ghost / solid red). Marketing, auth and pricing render
// through this component; console pages use Radix <Button> directly. Both
// read the same tokens, so a primary button looks identical on either side.
type Variant = "primary" | "secondary" | "ghost" | "danger";
// Heights: sm 32 (= Radix size 2) · md 36 (nav / card footer) · lg 44 (hero
// CTA, mobile primary target) · block 40, full width (auth forms).
type Size = "sm" | "md" | "lg" | "block";

const VARIANT_CLS: Record<Variant, string> = {
  primary: "btn-premium-primary",
  secondary: "btn-premium-secondary",
  ghost: "btn-premium-ghost",
  danger: "btn-premium-danger",
};

const SIZE_CLS: Record<Size, string> = {
  sm: "h-8 px-3 text-body-sm",
  md: "h-9 px-4 text-ui",
  lg: "h-11 px-6 text-ui",
  block: "h-10 w-full px-4 text-ui text-center",
};

export interface PremiumButtonProps {
  children: ReactNode;
  variant?: Variant;
  size?: Size;
  /** React Router target — picks the Link element. */
  to?: string;
  /** External link href — picks an <a>. Ignored if `to` is set. */
  href?: string;
  /** External link target/rel forwarded only when `href` is used. */
  external?: boolean;
  /** Native click handler — only meaningful for the button form. */
  onClick?: (e: MouseEvent<HTMLElement>) => void;
  disabled?: boolean;
  type?: "button" | "submit";
  className?: string;
  style?: CSSProperties;
  title?: string;
}

export default function PremiumButton({
  children,
  variant = "primary",
  size = "md",
  to,
  href,
  external,
  onClick,
  disabled,
  type = "button",
  className = "",
  style,
  title,
}: PremiumButtonProps) {
  const cls = [
    VARIANT_CLS[variant],
    SIZE_CLS[size],
    disabled ? "is-disabled" : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");

  if (to !== undefined) {
    // disabled router-Link: render an inert span so we don't ship a
    // clickable nav when caller intends "disabled state."
    if (disabled) {
      return <span className={cls} style={style} title={title}>{children}</span>;
    }
    return (
      <Link
        to={to}
        className={cls}
        style={style}
        title={title}
        onClick={onClick}
      >
        {children}
      </Link>
    );
  }

  if (href !== undefined) {
    const extraProps = external ? { target: "_blank", rel: "noopener noreferrer" } : {};
    return (
      <a
        href={href}
        className={cls}
        style={style}
        title={title}
        onClick={onClick}
        {...extraProps}
      >
        {children}
      </a>
    );
  }

  return (
    <button
      type={type}
      className={cls}
      style={style}
      title={title}
      onClick={onClick}
      disabled={disabled}
    >
      {children}
    </button>
  );
}
