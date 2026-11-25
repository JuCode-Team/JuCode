// FilterBar is the shared list filter shell for console list pages.
//
//   ┌──────────────────────────────────────────────────────────────────┐
//   │ [search] [主筛选 ≤4] [更多筛选 ▾]                   [trailing]    │  primary
//   │ [高级筛选 …]                                                     │  advanced (collapsible)
//   │ 状态：成功 ×  模型：gpt ×   清空筛选               共 361 条    │  chips
//   └──────────────────────────────────────────────────────────────────┘
//
// The bar is stateless about filter values: the page owns them and passes
// controls in. FilterSelect / FilterInput are the labelled controls that go
// inside — every control shows its field name even once it holds a value.

import { useId, useState, type CSSProperties, type ReactNode } from "react";
import { Button, Select, Text, TextField } from "@radix-ui/themes";
import { ChevronDown, Search, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ClearableTextField } from "../ClearableTextField";

export interface FilterChip {
  key: string;
  label: string;
  value: string;
  onRemove: () => void;
}

export function FilterBar({
  primary,
  advanced,
  advancedActive = 0,
  trailing,
  chips = [],
  onClearAll,
  summary,
  defaultAdvancedOpen = false,
  className,
}: {
  primary: ReactNode;
  advanced?: ReactNode;
  /** Number of active filters inside the collapsed area, shown on the toggle. */
  advancedActive?: number;
  trailing?: ReactNode;
  chips?: FilterChip[];
  onClearAll?: () => void;
  /** Right side of the chips row, e.g. 「共 361 条」 */
  summary?: ReactNode;
  defaultAdvancedOpen?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(defaultAdvancedOpen || advancedActive > 0);
  const panelId = useId();

  return (
    <div className={["jc-list-filters", className].filter(Boolean).join(" ")}>
      <div className="jc-filter-row jc-filter-row-primary">
        <div className="jc-filter-controls">
          {primary}
          {advanced && (
            <Button
              type="button"
              size="2"
              variant="ghost"
              color="gray"
              className={`jc-filter-more ${open ? "is-open" : ""}`}
              aria-expanded={open}
              aria-controls={panelId}
              onClick={() => setOpen((v) => !v)}
            >
              {t("keys.shared.moreFilters")}
              {advancedActive > 0 && <span className="jc-filter-more-count">{advancedActive}</span>}
              <ChevronDown size={14} className="jc-filter-more-icon" />
            </Button>
          )}
        </div>
        {trailing && <div className="jc-filter-trailing">{trailing}</div>}
      </div>

      {advanced && (
        <div id={panelId} className={`jc-filter-advanced ${open ? "is-open" : ""}`} aria-hidden={!open}>
          <div className="jc-filter-advanced-inner">
            <div className="jc-filter-row">
              <div className="jc-filter-controls">{advanced}</div>
            </div>
          </div>
        </div>
      )}

      <FilterChips chips={chips} onClearAll={onClearAll} summary={summary} />
    </div>
  );
}

/** The applied-filters row on its own, for shells that own their toolbar. */
export function FilterChips({
  chips,
  onClearAll,
  summary,
  className,
}: {
  chips: FilterChip[];
  onClearAll?: () => void;
  summary?: ReactNode;
  className?: string;
}) {
  // The row exists to explain an active filter set; with nothing applied the
  // pager already states the total, so the band stays out of the way.
  const { t } = useTranslation();
  const hasChips = chips.length > 0;
  if (!hasChips) return null;
  return (
    <div className={["jc-filter-applied", className].filter(Boolean).join(" ")}>
      <div className="jc-filter-chips">
        {chips.map((c) => (
          <button
            key={c.key}
            type="button"
            className="jc-filter-chip"
            onClick={c.onRemove}
            aria-label={t("keys.shared.removeFilter", { label: c.label, value: c.value })}
          >
            <span className="jc-filter-chip-label">{c.label}</span>
            <span className="jc-filter-chip-value">{c.value}</span>
            <X size={12} aria-hidden />
          </button>
        ))}
        {hasChips && onClearAll && (
          <Button type="button" size="1" variant="ghost" color="gray" className="jc-filter-clear" onClick={onClearAll}>
            {t("keys.shared.clearFilters")}
          </Button>
        )}
      </div>
      {summary && <div className="jc-filter-summary">{summary}</div>}
    </div>
  );
}

export interface FilterOption {
  v: string;
  l: string;
}

/** Select whose trigger always reads 「字段：值」; empty value reads 「字段：全部」. */
export function FilterSelect({
  label,
  value,
  onChange,
  options,
  allLabel,
  width,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: FilterOption[];
  allLabel?: string;
  width?: number;
}) {
  const { t } = useTranslation();
  const current = options.find((o) => o.v === value);
  const active = value !== "";
  const allText = allLabel ?? t("keys.shared.all");
  return (
    <Select.Root value={value || "_all"} onValueChange={(v) => onChange(v === "_all" ? "" : v)}>
      <Select.Trigger
        variant="soft"
        color="gray"
        className={`jc-filter-select ${active ? "is-active" : ""}`}
        style={width ? ({ "--jc-filter-width": `${width}px` } as CSSProperties) : undefined}
        aria-label={label}
      >
        <span className="jc-filter-field-label">{label}</span>
        <span className="jc-filter-field-value">{active ? current?.l ?? value : allText}</span>
      </Select.Trigger>
      <Select.Content position="popper">
        <Select.Item value="_all">{allText}</Select.Item>
        {options.filter((o) => o.v !== "").map((o) => (
          <Select.Item key={o.v} value={o.v}>{o.l}</Select.Item>
        ))}
      </Select.Content>
    </Select.Root>
  );
}

/** Text filter with the field name pinned in the leading slot. */
export function FilterInput({
  label,
  value,
  onChange,
  placeholder,
  width = 160,
  search = false,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  width?: number;
  /** Render a search glyph instead of the text label (for the primary search box). */
  search?: boolean;
}) {
  return (
    <ClearableTextField
      size="2"
      className={`jc-filter-input ${value ? "is-active" : ""}`}
      style={{ "--jc-filter-width": `${width}px` } as CSSProperties}
      value={value}
      placeholder={placeholder}
      aria-label={label}
      onChange={(e) => onChange(e.target.value)}
    >
      <TextField.Slot side="left" className="jc-filter-input-label">
        {search ? <Search size={14} aria-hidden /> : <Text size="1">{label}</Text>}
      </TextField.Slot>
    </ClearableTextField>
  );
}

/** Wraps a control that has no label of its own (e.g. a date range). */
export function FilterField({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <div className={["jc-filter-field", className].filter(Boolean).join(" ")} aria-label={label}>
      {children}
    </div>
  );
}
