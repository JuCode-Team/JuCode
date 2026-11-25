import { useMemo, useRef, useState } from "react";
import { Badge, Popover, Switch, Text, Tooltip } from "@radix-ui/themes";
import { Check, ChevronDown, Search } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { PublicModel, PublicModelGroup } from "../../../api/public";
import { groupsForModel, LAB_AUTO_GROUP } from "../../../lib/labPick";
import ModelBrandIcon from "../../../components/ModelBrandIcon";

// Model first, then group. Picking a model opens the group list right away:
// 自动 (the gateway routes by price and availability) plus the groups in the
// current billing scope that carry that model.

function rate(value: number): string {
  return `×${Number(value.toFixed(3))}`;
}

function BillingBadge({ source }: { source: PublicModelGroup["billing_source"] }) {
  const { t } = useTranslation();
  return source === "plan_only"
    ? <Badge size="1" color="violet" variant="soft">{t("bench.labPicker.plan")}</Badge>
    : <Badge size="1" color="green" variant="soft">{t("bench.labPicker.balance")}</Badge>;
}

export function ModelPicker({
  models, groups, model, groupId, onModel, onGroup, placeholder, side = "bottom",
}: {
  models: PublicModel[];
  groups: PublicModelGroup[];
  model: string;
  groupId: string;
  onModel: (name: string) => void;
  onGroup: (id: string) => void;
  placeholder: string;
  side?: "top" | "bottom";
}) {
  const { t } = useTranslation();
  const [mode, setMode] = useState<"model" | "group" | null>(null);
  const [filter, setFilter] = useState("");
  const anchorRef = useRef<HTMLDivElement>(null);
  const current = models.find((item) => item.name === model);
  const modelGroups = useMemo(() => groupsForModel(current, groups), [current, groups]);
  const group = modelGroups.find((item) => item.id === groupId);
  const groupCount = (item: PublicModel) => groupsForModel(item, groups).length;
  const needle = filter.trim().toLowerCase();
  const shown = needle ? models.filter((item) => item.name.toLowerCase().includes(needle)) : models;

  const toggle = (next: "model" | "group") => {
    setMode((cur) => (cur === next ? null : next));
    if (next === "model") setFilter("");
  };

  return (
    <Popover.Root open={mode !== null} onOpenChange={(open) => { if (!open) setMode(null); }}>
      {/* Radix Themes' Popover.Anchor drops its children, so an empty anchor overlays the picker. */}
      <div ref={anchorRef} className="jc-lab-picker">
        <Popover.Anchor className="jc-lab-picker-anchor" />
        <button
          type="button"
          className={`jc-lab-picker-model${model ? "" : " is-empty"}`}
          aria-haspopup="dialog"
          aria-expanded={mode === "model"}
          onClick={() => toggle("model")}
        >
          {model && <ModelBrandIcon model={model} size={15} />}
          <span className="jc-mono">{model || placeholder}</span>
          <ChevronDown size={14} />
        </button>
        {model && (
          <Tooltip content={t("bench.labPicker.selectGroup")}>
            <button
              type="button"
              className="jc-lab-picker-group"
              aria-haspopup="dialog"
              aria-expanded={mode === "group"}
              aria-label={t("bench.labPicker.groupAria", { name: group ? group.name : t("bench.labPicker.auto") })}
              onClick={() => toggle("group")}
            >
              <span>{group ? group.name : t("bench.labPicker.auto")}</span>
              {group && <span className="jc-lab-picker-rate">{rate(group.rate_multiplier)}</span>}
            </button>
          </Tooltip>
        )}
      </div>
      <Popover.Content
        side={side}
        align="start"
        sideOffset={6}
        className="jc-lab-picker-pop"
        onOpenAutoFocus={(event) => { if (mode === "group") event.preventDefault(); }}
        onInteractOutside={(event) => {
          if (anchorRef.current?.contains(event.target as Node)) event.preventDefault();
        }}
      >
        {mode === "group" ? (
          <div role="listbox" aria-label={t("bench.labPicker.selectGroup")}>
            <div className="jc-lab-picker-head">
              <Text size="2" weight="medium">{t("bench.labPicker.selectGroup")}</Text>
              <Text size="1" color="gray" className="jc-mono">{model}</Text>
            </div>
            <button
              type="button"
              role="option"
              aria-selected={!group}
              className={`jc-lab-picker-option${!group ? " is-on" : ""}`}
              onClick={() => { onGroup(LAB_AUTO_GROUP); setMode(null); }}
            >
              <span className="jc-lab-picker-option-main">
                <span>{t("bench.labPicker.auto")}</span>
                <small>{t("bench.labPicker.autoHint")}</small>
              </span>
              {!group && <Check size={14} />}
            </button>
            {modelGroups.map((item) => (
              <button
                key={item.id}
                type="button"
                role="option"
                aria-selected={item.id === group?.id}
                className={`jc-lab-picker-option${item.id === group?.id ? " is-on" : ""}`}
                onClick={() => { onGroup(item.id); setMode(null); }}
              >
                <span className="jc-lab-picker-option-main">
                  <span>{item.name}</span>
                  {item.description && <small>{item.description}</small>}
                </span>
                <span className="jc-lab-picker-rate">{rate(item.rate_multiplier)}</span>
                <BillingBadge source={item.billing_source} />
                {item.id === group?.id ? <Check size={14} /> : <span className="jc-lab-picker-check" />}
              </button>
            ))}
          </div>
        ) : (
          <div>
            <label className="jc-lab-picker-filter">
              <Search size={14} />
              <input
                autoFocus
                value={filter}
                placeholder={t("bench.labPicker.searchModels")}
                aria-label={t("bench.labPicker.searchModels")}
                onChange={(event) => setFilter(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key !== "Enter" || shown.length !== 1) return;
                  event.preventDefault();
                  onModel(shown[0].name);
                  setMode("group");
                }}
              />
            </label>
            <div className="jc-lab-picker-list" role="listbox" aria-label={t("bench.labPicker.selectModel")}>
              {shown.length === 0 && <div className="jc-lab-picker-empty">{models.length ? t("bench.labPicker.noMatch") : t("bench.labPicker.noModels")}</div>}
              {shown.map((item) => (
                <button
                  key={item.name}
                  type="button"
                  role="option"
                  aria-selected={item.name === model}
                  className={`jc-lab-picker-option${item.name === model ? " is-on" : ""}`}
                  onClick={() => { onModel(item.name); setMode("group"); }}
                >
                  <ModelBrandIcon model={item.name} size={14} />
                  <span className="jc-lab-picker-option-main jc-mono">{item.name}</span>
                  <small className="jc-lab-picker-meta">{t("bench.labPicker.groupCount", { count: groupCount(item) })}</small>
                  {item.name === model ? <Check size={14} /> : <span className="jc-lab-picker-check" />}
                </button>
              ))}
            </div>
          </div>
        )}
      </Popover.Content>
    </Popover.Root>
  );
}

export function PlanOnlySwitch({ planOnly, hasPlan, walletLabel, onChange }: {
  planOnly: boolean;
  hasPlan: boolean;
  walletLabel: string;
  onChange: (value: boolean) => void;
}) {
  const { t } = useTranslation();
  const on = planOnly && hasPlan;
  const tip = !hasPlan
    ? t("bench.labPicker.noPlanTip", { wallet: walletLabel })
    : on
      ? t("bench.labPicker.planOnTip")
      : t("bench.labPicker.planOffTip", { wallet: walletLabel });
  return (
    <Tooltip content={tip}>
      <Text as="label" size="1" className={`jc-lab-planonly${hasPlan ? "" : " is-off"}`}>
        <Switch size="1" checked={on} disabled={!hasPlan} onCheckedChange={onChange} />
        {t("bench.labPicker.planOnly")}
      </Text>
    </Tooltip>
  );
}
