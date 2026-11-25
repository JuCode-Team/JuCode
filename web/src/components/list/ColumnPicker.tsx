import { useCallback, useMemo, useState } from "react";
import { Button, DropdownMenu } from "@radix-ui/themes";
import { Columns3 } from "lucide-react";
import { useTranslation } from "react-i18next";

export interface ColumnDef {
  key: string;
  label: string;
  /** Hidden until the operator turns it on. */
  defaultHidden?: boolean;
  /** Cannot be hidden (expand handle, primary id). */
  locked?: boolean;
}

// useColumnVisibility remembers which optional columns a table shows, per
// storage key, in localStorage. Storage failures fall back to defaults.
export function useColumnVisibility(storageKey: string, defs: ColumnDef[]) {
  const defaults = useMemo(() => new Set(defs.filter((d) => !d.defaultHidden).map((d) => d.key)), [defs]);
  const [visible, setVisibleState] = useState<Set<string>>(() => {
    try {
      const raw = window.localStorage.getItem(storageKey);
      if (raw) {
        const arr = JSON.parse(raw) as string[];
        if (Array.isArray(arr)) return new Set(arr);
      }
    } catch { /* storage unavailable → defaults */ }
    return defaults;
  });

  const setVisible = useCallback((next: Set<string>) => {
    setVisibleState(next);
    try { window.localStorage.setItem(storageKey, JSON.stringify([...next])); } catch { /* ignore */ }
  }, [storageKey]);

  const isVisible = useCallback((key: string) => {
    const def = defs.find((d) => d.key === key);
    return !!def?.locked || visible.has(key);
  }, [defs, visible]);

  const toggle = (key: string) => {
    const next = new Set(visible);
    if (next.has(key)) next.delete(key); else next.add(key);
    setVisible(next);
  };
  const reset = () => setVisible(defaults);

  return { visible, isVisible, toggle, reset, isDefault: sameSet(visible, defaults) };
}

function sameSet(a: Set<string>, b: Set<string>) {
  if (a.size !== b.size) return false;
  for (const k of a) if (!b.has(k)) return false;
  return true;
}

export function ColumnPicker({
  defs,
  isVisible,
  onToggle,
  onReset,
  isDefault,
}: {
  defs: ColumnDef[];
  isVisible: (key: string) => boolean;
  onToggle: (key: string) => void;
  onReset: () => void;
  isDefault: boolean;
}) {
  const { t } = useTranslation();
  const optional = defs.filter((d) => !d.locked);
  const shown = optional.filter((d) => isVisible(d.key)).length;
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger>
        <Button size="1" variant="soft" color="gray" className="jc-column-picker">
          <Columns3 size={14} aria-hidden />
          {t("keys.shared.columns")}
          <span className="jc-num" style={{ color: "var(--c-on-surface-variant)" }}>{shown}/{optional.length}</span>
        </Button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Content align="end" size="1" className="jc-column-picker-menu">
        {optional.map((d) => (
          <DropdownMenu.CheckboxItem
            key={d.key}
            checked={isVisible(d.key)}
            onCheckedChange={() => onToggle(d.key)}
            onSelect={(e) => e.preventDefault()}
          >
            {d.label}
          </DropdownMenu.CheckboxItem>
        ))}
        <DropdownMenu.Separator />
        <DropdownMenu.Item disabled={isDefault} onSelect={onReset}>{t("keys.shared.resetColumns")}</DropdownMenu.Item>
      </DropdownMenu.Content>
    </DropdownMenu.Root>
  );
}
