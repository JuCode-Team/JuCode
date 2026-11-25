import type { ReactNode } from "react";

// PageHeader is the single page-title block for every console route.
// Title on the left, optional one-line description under it, optional
// actions aligned to the title baseline on the right. Styles live in
// styles/console-shell.css (.jc-page-*).
export function PageHeader({
  title,
  description,
  actions,
  className = "",
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <header className={`jc-page-header ${className}`.trim()}>
      <div className="jc-page-heading">
        <h1 className="jc-page-title">{title}</h1>
        {description && <p className="jc-page-sub">{description}</p>}
      </div>
      {actions && <div className="jc-page-actions">{actions}</div>}
    </header>
  );
}
