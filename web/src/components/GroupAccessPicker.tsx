import { useState, type ReactNode } from "react";
import { Button, Checkbox, Flex, SegmentedControl, Text, TextField } from "@radix-ui/themes";
import { Search } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ClearableTextField } from "./ClearableTextField";
import ModelBrandIcon from "./ModelBrandIcon";
import { stateHint, stateLabel, stateTone } from "./AvailabilityDots";
import { formatMultiplier } from "../lib/groupUsage";
import { matchesVendor, vendorChoices, vendorKey, vendorLabel } from "../lib/modelBrand";
import {
  billingHint,
  hasSubscription,
  matchGroups,
  preferredBillingMode,
  selectionSummary,
  type AccessibleGroup,
  type BillingSource,
  type GroupRunState,
} from "../lib/groupAccess";

export default function GroupAccessPicker({
  groups,
  value,
  onChange,
  balanceLabel,
  loading = false,
  scope = "personal",
  plan = null,
  planLoading = false,
  emptySummary,
  context,
}: {
  groups: AccessibleGroup[];
  value: string[];
  onChange: (ids: string[]) => void;
  balanceLabel: string;
  // 未勾选任何分组时替代默认的“可访问全部分组”摘要；传空串则不显示摘要。
  // 用于分组已全部停用的 Key：列表为空但并非不限分组。
  emptySummary?: string;
  loading?: boolean;
  scope?: "personal" | "organization";
  plan?: { has_active_plan: boolean; plan_type?: string } | null;
  planLoading?: boolean;
  // 当前 tab 的账户上下文（套餐名与到期、余额），接在扣费说明后面，同一行显示。
  context?: Partial<Record<BillingSource, ReactNode>>;
}) {
  const { t } = useTranslation();
  const [mode, setMode] = useState<BillingSource | null>(null);
  const [query, setQuery] = useState("");
  const [vendor, setVendor] = useState<string | null>(null);
  const hasPlan = !planLoading && hasSubscription(plan);
  const active = hasPlan ? (mode ?? preferredBillingMode(groups, value, true)) : "balance_only";
  const inMode = groups.filter((group) => group.billing_source === active);
  // 分组不多时一眼能看完，厂商筛选、搜索和批量操作都收起；收起时也不让残留的筛选条件生效。
  const filterable = inMode.length > FILTER_THRESHOLD;
  const choices = filterable ? vendorChoices(inMode) : [];
  // A vendor picked on 订阅 may not exist on 按量. Treat that as 全部 for
  // this side, but keep the choice so switching back restores it.
  const vendorActive = choices.some((choice) => choice.key === vendor) ? vendor : null;
  const vendorFiltered = inMode.filter((group) => matchesVendor(group, vendorActive));
  const matches = matchGroups(vendorFiltered, filterable ? query : "");
  const visibleIds = matches.map((match) => match.group.id);
  const selected = new Set(value);
  const planSelected = groups.filter((group) => group.billing_source === "plan_only" && selected.has(group.id)).length;
  const balanceSelected = groups.filter((group) => group.billing_source === "balance_only" && selected.has(group.id)).length;
  const modeName = active === "plan_only" ? t("keys.groups.subscription") : balanceLabel;
  const vendorName = choices.find((choice) => choice.key === vendorActive)?.label;
  const countLabel = vendorName
    ? t("keys.groups.countVendor", { n: matches.length, vendor: vendorName, mode: modeName })
    : filterable && query.trim()
      ? t("keys.groups.countMatch", { n: matches.length, mode: modeName })
      : t("keys.groups.count", { n: inMode.length, mode: modeName });
  const allVisibleSelected = visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));
  const anyVisibleSelected = visibleIds.some((id) => selected.has(id));

  const toggle = (id: string) => {
    onChange(selected.has(id) ? value.filter((item) => item !== id) : [...value, id]);
  };
  const selectVisible = () => {
    const next = visibleIds.filter((id) => !selected.has(id));
    if (next.length > 0) onChange([...value, ...next]);
  };
  const clearVisible = () => {
    const drop = new Set(visibleIds);
    onChange(value.filter((id) => !drop.has(id)));
  };

  return (
    <div className="jc-group-pick">
      <span className="jc-field-label">{t("keys.groups.label")}</span>
      {hasPlan && (
        <SegmentedControl.Root
          className="jc-group-pick-mode"
          value={active}
          onValueChange={(next) => setMode(next as BillingSource)}
          size="2"
        >
          <SegmentedControl.Item value="plan_only">{tabLabel(t("keys.groups.subscription"), planSelected)}</SegmentedControl.Item>
          <SegmentedControl.Item value="balance_only">{tabLabel(balanceLabel, balanceSelected)}</SegmentedControl.Item>
        </SegmentedControl.Root>
      )}
      {!planLoading && (
        <Text size="1" color="gray" as="p">
          {billingHint(active, scope, plan, false).replace(/[。.]$/, "")}
          {context?.[active] && <> · {context[active]}</>}
        </Text>
      )}
      {!planLoading && !loading && choices.length > 0 && (
        <div className="jc-group-pick-filter" role="group" aria-label={t("keys.groups.vendor")}>
          <button type="button" aria-pressed={vendorActive === null} onClick={() => setVendor(null)}>{t("keys.shared.all")}</button>
          {choices.map((choice) => (
            <button
              key={choice.key}
              type="button"
              aria-pressed={vendorActive === choice.key}
              onClick={() => setVendor(vendorActive === choice.key ? null : choice.key)}
            >
              <ModelBrandIcon model={choice.label} size={14} />
              {choice.label}
              <span className="jc-group-pick-filter-count">{choice.count}</span>
            </button>
          ))}
        </div>
      )}
      {filterable && (
        <ClearableTextField
          placeholder={t("keys.groups.search")}
          value={query}
          onChange={(event) => setQuery(event.target.value)}
        >
          <TextField.Slot><Search size={14} /></TextField.Slot>
        </ClearableTextField>
      )}
      {filterable && <Flex className="jc-group-pick-tools" align="center" justify="between" gap="2">
        <Text size="1" color="gray">
          {planLoading
            ? t("keys.groups.readingPlan")
            : loading
              ? t("keys.groups.loadingGroups")
              : countLabel}
        </Text>
        <Flex gap="1">
          <Button size="1" variant="ghost" color="gray" disabled={allVisibleSelected || visibleIds.length === 0} onClick={selectVisible}>{t("keys.groups.selectAll")}</Button>
          <Button size="1" variant="ghost" color="gray" disabled={!anyVisibleSelected} onClick={clearVisible}>
            {query.trim() ? t("keys.groups.clearResults") : t("keys.groups.clearKind")}
          </Button>
        </Flex>
      </Flex>}
      <div className="jc-group-pick-list">
        {loading || planLoading ? (
          <div className="jc-group-pick-empty">{planLoading ? t("keys.groups.readingPlanFull") : t("keys.groups.loadingGroupsFull")}</div>
        ) : groups.length === 0 ? (
          <div className="jc-group-pick-empty">{t("keys.groups.noneAtAll")}</div>
        ) : inMode.length === 0 ? (
          <div className="jc-group-pick-empty">{t("keys.groups.noneInMode", { mode: modeName })}</div>
        ) : vendorFiltered.length === 0 ? (
          <div className="jc-group-pick-empty">{t("keys.groups.noneVendor", { vendor: vendorName, mode: modeName })}</div>
        ) : matches.length === 0 ? (
          <div className="jc-group-pick-empty">{t("keys.groups.noneQuery", { query: query.trim(), scope: vendorName ? `${vendorName}` : modeName })}</div>
        ) : matches.map((match) => {
          const checked = selected.has(match.group.id);
          const hits = [...match.vendors, ...match.models];
          return (
            <div
              key={match.group.id}
              title={match.group.description || undefined}
              className={`jc-group-pick-row${checked ? " is-checked" : ""}`}
              onClick={(event) => {
                if ((event.target as HTMLElement).closest("button")) return;
                toggle(match.group.id);
              }}
            >
              <Checkbox
                checked={checked}
                aria-label={match.group.name}
                onCheckedChange={() => toggle(match.group.id)}
              />
              <div className="jc-group-pick-body">
                <div className="jc-group-pick-name">
                  <span>{match.group.name}</span>
                  <GroupMarks group={match.group} />
                  <GroupStatus state={match.group.status} />
                </div>
                {hits.length > 0 && <div className="jc-group-pick-hit">{t("keys.groups.hits", { list: hits.join(t("keys.shared.listSep")) })}</div>}
              </div>
              <div className="jc-group-pick-rate">
                <strong>{formatMultiplier(match.group.rate_multiplier)}</strong>
              </div>
            </div>
          );
        })}
      </div>
      {!loading && !planLoading && (value.length === 0 && emptySummary !== undefined
        ? (emptySummary ? <Text size="1" color="gray" as="p">{emptySummary}</Text> : null)
        : <Text size="1" color="gray" as="p">{selectionSummary(groups, value, balanceLabel, hasPlan)}</Text>)}
    </div>
  );
}

const FILTER_THRESHOLD = 6;

function tabLabel(name: string, selected: number): string {
  return selected > 0 ? `${name} · ${selected}` : name;
}

function GroupMarks({ group }: { group: AccessibleGroup }) {
  const { t } = useTranslation();
  const brands = brandMarks(group);
  if (brands.length === 0 && !group.supports_image && !group.exclusive) return null;
  return (
    <span className="jc-group-pick-marks">
      {brands.length > 0 && (
        <span className="jc-group-pick-vendors">
          {brands.map((brand) => (
            <span key={brand.key} className="jc-group-pick-vendor" title={brand.label} aria-label={brand.label}>
              <ModelBrandIcon model={brand.label} size={14} />
            </span>
          ))}
        </span>
      )}
      {group.supports_image && <span className="jc-group-pick-tag">{t("keys.groups.image")}</span>}
      {group.exclusive && <span className="jc-group-pick-tag">{t("keys.groups.exclusive")}</span>}
    </span>
  );
}

function brandMarks(group: AccessibleGroup): { key: string; label: string }[] {
  const source = group.vendors?.length ? group.vendors : (group.models ?? []);
  const seen = new Set<string>();
  const out: { key: string; label: string }[] = [];
  for (const name of source) {
    const key = vendorKey(name);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ key, label: vendorLabel(name) });
    if (out.length >= 6) break;
  }
  return out;
}

function GroupStatus({ state }: { state?: GroupRunState }) {
  const status = state ?? "unknown";
  return (
    <div className="jc-group-pick-status" title={stateHint(status)}>
      <span className="avail-dot" data-tone={stateTone(status)} />
      <span>{stateLabel(status)}</span>
    </div>
  );
}
