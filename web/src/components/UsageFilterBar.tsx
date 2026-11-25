import { useState, type CSSProperties, type ReactNode } from "react";
import { Badge, Box, Button, Popover, Select, Text } from "@radix-ui/themes";
import { SlidersHorizontal } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ClearableTextField } from "./ClearableTextField";
import { DateRangeInput, billingSourceOptions, endpointOptions, statusOptions } from "./UsageLogFilters";

// The personal usage log's filter row: time range, model and status stay
// visible; endpoint, billing source, key and IP fold into one popover that
// shows how many of them are applied. The host page owns every value.
export interface UsageFilterBarProps {
  start: string; setStart: (v: string) => void;
  end: string; setEnd: (v: string) => void;
  model: string; setModel: (v: string) => void;
  status: string; setStatus: (v: string) => void;
  endpoint: string; setEndpoint: (v: string) => void;
  billingSource: string; setBillingSource: (v: string) => void;
  apiKey: string; setApiKey: (v: string) => void;
  ip: string; setIp: (v: string) => void;
  /** Extra fields for the popover, e.g. the org member picker. */
  extra?: ReactNode;
  /** Whether `extra` currently narrows the result; counts toward the badge. */
  extraApplied?: boolean;
  /** Restores every filter, including the time range, to the page default. */
  onReset: () => void;
  /** True when anything differs from the default — controls the reset button. */
  dirty: boolean;
}

export default function UsageFilterBar(p: UsageFilterBarProps) {
  const { t } = useTranslation();
  const [moreOpen, setMoreOpen] = useState(false);
  const moreCount =
    [p.endpoint, p.billingSource, p.apiKey, p.ip].filter(Boolean).length + (p.extraApplied ? 1 : 0);

  return (
    <div className="jc-filter-bar">
      <DateRangeInput start={p.start} setStart={p.setStart} end={p.end} setEnd={p.setEnd} compact />
      <Box className="jc-filter-input" style={{ "--jc-filter-width": "168px" } as CSSProperties}>
        <ClearableTextField
          size="2"
          placeholder={t("usage.filterBar.model")}
          value={p.model}
          onChange={(e) => p.setModel(e.target.value)}
        />
      </Box>
      <FilterSelect value={p.status} onChange={p.setStatus} options={statusOptions(t)} />

      <Popover.Root open={moreOpen} onOpenChange={setMoreOpen}>
        <Popover.Trigger>
          <Button variant="soft" color="gray" size="2" className="jc-filter-more" aria-label={t("usage.filterBar.more")}>
            <SlidersHorizontal size={14} />
            {t("usage.filterBar.more")}
            {moreCount > 0 && (
              <Badge size="1" variant="solid" radius="full" className="jc-filter-count">{moreCount}</Badge>
            )}
          </Button>
        </Popover.Trigger>
        <Popover.Content align="start" sideOffset={8} className="jc-filter-popover">
          <div className="jc-filter-grid">
            <Field label={t("usage.filters.endpoint")}>
              <FilterSelect value={p.endpoint} onChange={p.setEndpoint} options={endpointOptions(t)} full />
            </Field>
            <Field label={t("usage.filters.billingSource")}>
              <FilterSelect value={p.billingSource} onChange={p.setBillingSource} options={billingSourceOptions(t)} full />
            </Field>
            <Field label={t("usage.filters.apiKeyName")}>
              <ClearableTextField size="2" value={p.apiKey} onChange={(e) => p.setApiKey(e.target.value)} placeholder={t("usage.filterBar.all")} />
            </Field>
            <Field label="IP">
              <ClearableTextField size="2" value={p.ip} onChange={(e) => p.setIp(e.target.value)} placeholder={t("usage.filterBar.all")} />
            </Field>
            {p.extra}
          </div>
        </Popover.Content>
      </Popover.Root>

      {p.dirty && (
        <Button variant="ghost" color="gray" size="2" className="jc-filter-reset" onClick={p.onReset}>
          {t("usage.filterBar.reset")}
        </Button>
      )}
    </div>
  );
}

export function Field({ label, children, full }: { label: string; children: ReactNode; full?: boolean }) {
  return (
    <label className={`jc-filter-field${full ? " is-full" : ""}`}>
      <Text as="span" size="1" color="gray">{label}</Text>
      {children}
    </label>
  );
}

function FilterSelect({
  value, onChange, options, full,
}: {
  value: string; onChange: (v: string) => void;
  options: { v: string; l: string }[];
  full?: boolean;
}) {
  return (
    <Select.Root value={value || "_all"} onValueChange={(v) => onChange(v === "_all" ? "" : v)}>
      <Select.Trigger variant="soft" color="gray" className={full ? "jc-filter-select is-full" : "jc-filter-select"} />
      <Select.Content>
        {options.map((o) => (
          <Select.Item key={o.v || "_all"} value={o.v || "_all"}>{o.l}</Select.Item>
        ))}
      </Select.Content>
    </Select.Root>
  );
}
