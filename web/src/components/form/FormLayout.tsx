// FormLayout: the shared vocabulary for admin forms.
//
// Every admin dialog and settings page composes the same five primitives so
// labels, hints, section rhythm and the action bar read identically:
//
//   FormSection  — 区块标题 + 一句说明 + 分隔线；块内内容自带 16px 顶部间距
//   FieldGrid    — 块内字段的 2 列（或 3 列）栅格；<640px 自动折成单列
//   Field        — 标签(必填标记 / 技术名) + 控件 + 说明 + 就地错误
//   FormDialog   — 固定头 / 滚动体 / 吸底操作栏的对话框壳，宽度三档 sm/md/lg
//   DangerZone   — 独立红色描边区块，只放不可逆操作，永远排在最底
//
// Styling lives in styles/forms.css; components only emit class names.
import type { CSSProperties, ReactNode } from "react";
import { Button, Callout, Dialog, Switch } from "@radix-ui/themes";
import { AlertCircle, AlertTriangle } from "lucide-react";
import { useTranslation } from "react-i18next";

// ---------------------------------------------------------------- Field

export function Field({
  label, code, required, hint, error, htmlFor, span, children, className,
}: {
  label: ReactNode;
  // Technical identifier shown after the label in mono (base_url, api_format).
  code?: string;
  required?: boolean;
  hint?: ReactNode;
  error?: ReactNode;
  htmlFor?: string;
  // Occupy the whole row of the enclosing FieldGrid.
  span?: "full";
  children: ReactNode;
  className?: string;
}) {
  const { t } = useTranslation();
  const classes = ["jc-field", span === "full" ? "jc-field-span-full" : "", className ?? ""].filter(Boolean).join(" ");
  return (
    <div className={classes}>
      <label className="jc-field-label" htmlFor={htmlFor}>
        <span>{label}</span>
        {code && <code className="jc-field-code">{code}</code>}
        {required && <span className="jc-field-req" title={t("keys.shared.required")}>*</span>}
      </label>
      <div className="jc-field-control">{children}</div>
      {error ? (
        <div className="jc-field-error" role="alert">{error}</div>
      ) : hint ? (
        <div className="jc-field-hint">{hint}</div>
      ) : null}
    </div>
  );
}

// ------------------------------------------------------------ FieldGrid

export function FieldGrid({
  columns = 2, children, className, style,
}: {
  columns?: 1 | 2 | 3;
  children: ReactNode;
  className?: string;
  style?: CSSProperties;
}) {
  return (
    <div className={["jc-field-grid", className ?? ""].filter(Boolean).join(" ")} data-columns={columns} style={style}>
      {children}
    </div>
  );
}

// ---------------------------------------------------------- FormSection

export function FormSection({
  title, description, actions, children, className,
}: {
  title: ReactNode;
  description?: ReactNode;
  // Right-aligned control in the section head (a master Switch, an add button).
  actions?: ReactNode;
  children?: ReactNode;
  className?: string;
}) {
  return (
    <section className={["jc-form-section", className ?? ""].filter(Boolean).join(" ")}>
      <header className="jc-form-section-head">
        <div className="jc-form-section-heading">
          <h3 className="jc-form-section-title">{title}</h3>
          {description && <p className="jc-form-section-desc">{description}</p>}
        </div>
        {actions && <div className="jc-form-section-actions">{actions}</div>}
      </header>
      {children && <div className="jc-form-section-body">{children}</div>}
    </section>
  );
}

// ----------------------------------------------------------- DangerZone

export function DangerZone({
  title, description, action,
}: {
  title: ReactNode;
  description?: ReactNode;
  action: ReactNode;
}) {
  const { t } = useTranslation();
  return (
    <div className="jc-danger-zone" role="group" aria-label={t("keys.shared.dangerZone")}>
      <div className="jc-danger-zone-text">
        <div className="jc-danger-zone-title">{title}</div>
        {description && <div className="jc-danger-zone-desc">{description}</div>}
      </div>
      <div className="jc-danger-zone-action">{action}</div>
    </div>
  );
}

// ------------------------------------------------------------ FormError

// FormError is the server-side / whole-form error. It sits directly above
// the action bar so it is visible without scrolling.
export function FormError({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <Callout.Root color="red" size="1" className="jc-form-error" role="alert">
      <Callout.Icon><AlertCircle size={14} /></Callout.Icon>
      <Callout.Text>{children}</Callout.Text>
    </Callout.Root>
  );
}

// ----------------------------------------------------------- FormDialog

export type FormDialogSize = "sm" | "md" | "lg";
const DIALOG_WIDTH: Record<FormDialogSize, number> = { sm: 420, md: 560, lg: 760 };

// FormDialog: fixed header, scrolling body, sticky footer. `footer` holds the
// right-aligned primary actions; `footerLeft` the secondary / destructive
// ones. `error` renders above the actions so it never scrolls out of view.
export function FormDialog({
  open, onOpenChange, size = "md", title, description, header, children,
  footer, footerLeft, error, notice, contentProps,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  size?: FormDialogSize;
  title: ReactNode;
  description?: ReactNode;
  // Extra header content below the description (step indicator, badges, tabs).
  header?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  footerLeft?: ReactNode;
  error?: ReactNode;
  // Non-blocking warnings (amber) shown above the actions, next to `error`.
  notice?: ReactNode;
  contentProps?: Omit<React.ComponentProps<typeof Dialog.Content>, "children" | "className">;
}) {
  const { t } = useTranslation();
  const width = DIALOG_WIDTH[size];
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Content
        className="jc-form-dialog"
        data-size={size}
        maxWidth={`${width}px`}
        width={`min(${width}px, 96vw)`}
        {...contentProps}
      >
        <div className="jc-form-dialog-head">
          <Dialog.Title className="jc-form-dialog-title">{title}</Dialog.Title>
          {description ? (
            <Dialog.Description size="2" className="jc-form-dialog-desc">{description}</Dialog.Description>
          ) : (
            <Dialog.Description style={{ display: "none" }}>{typeof title === "string" ? title : t("keys.shared.form")}</Dialog.Description>
          )}
          {header}
        </div>
        <div className="jc-form-dialog-body">{children}</div>
        {(footer || footerLeft || error || notice) && (
          <div className="jc-form-dialog-foot">
            {notice && (
              <Callout.Root color="amber" size="1" className="jc-form-error" variant="surface">
                <Callout.Icon><AlertTriangle size={14} /></Callout.Icon>
                <Callout.Text>{notice}</Callout.Text>
              </Callout.Root>
            )}
            {error && <FormError>{error}</FormError>}
            <div className="jc-form-dialog-actions">
              <div className="jc-form-dialog-actions-left">{footerLeft}</div>
              <div className="jc-form-dialog-actions-right">{footer}</div>
            </div>
          </div>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}

// CancelButton closes the enclosing dialog; kept here so every footer spells
// "取消" the same way.
export function CancelButton({ children, ...props }: React.ComponentProps<typeof Button>) {
  const { t } = useTranslation();
  return (
    <Dialog.Close>
      <Button variant="soft" color="gray" {...props}>{children ?? t("keys.shared.cancel")}</Button>
    </Dialog.Close>
  );
}

// -------------------------------------------------------------- SaveBar

// SaveBar is the page-level counterpart of the dialog footer: sticks to the
// bottom of the viewport while the form above it scrolls.
export function SaveBar({
  status, actions, error,
}: {
  status?: ReactNode;
  actions: ReactNode;
  error?: ReactNode;
}) {
  return (
    <div className="jc-savebar">
      {error && <FormError>{error}</FormError>}
      <div className="jc-savebar-row">
        <div className="jc-savebar-status">{status}</div>
        <div className="jc-savebar-actions">{actions}</div>
      </div>
    </div>
  );
}

// ------------------------------------------------------------ ToggleRow

// ToggleRow: a Switch with its label and hint in one bordered row. The whole
// row is the label, so clicking the text toggles the switch.
export function ToggleRow({
  checked, onCheckedChange, label, hint, disabled,
}: {
  checked: boolean;
  onCheckedChange: (v: boolean) => void;
  label: ReactNode;
  hint?: ReactNode;
  disabled?: boolean;
}) {
  return (
    <label className="jc-toggle-row" data-disabled={disabled ? "true" : undefined}>
      <div>
        <div className="jc-toggle-row-title">{label}</div>
        {hint && <div className="jc-toggle-row-hint">{hint}</div>}
      </div>
      <Switch checked={checked} onCheckedChange={onCheckedChange} disabled={disabled} />
    </label>
  );
}
