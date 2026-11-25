import i18n from "../i18n";

export type BillingSource = "plan_only" | "balance_only";
export type GroupRunState = "healthy" | "degraded" | "serious" | "down" | "unknown";

// One group the signed-in account may grant to an API key.
// vendors are model vendors (厂商): OpenAI, Anthropic, and so on.
export interface AccessibleGroup {
  id: string;
  name: string;
  billing_source: BillingSource;
  rate_multiplier?: number;
  description?: string;
  vendors?: string[];
  models?: string[];
  supports_image?: boolean;
  exclusive?: boolean;
  status?: GroupRunState;
}

export interface GroupMatch {
  group: AccessibleGroup;
  vendors: string[];
  models: string[];
}

export function preferredBillingMode(groups: AccessibleGroup[], selectedIds: string[], hasPlan: boolean): BillingSource {
  if (!hasPlan) return "balance_only";
  const selected = groups.filter((group) => selectedIds.includes(group.id));
  const planSelected = selected.filter((group) => group.billing_source === "plan_only").length;
  const balanceSelected = selected.length - planSelected;
  if (planSelected > 0 && balanceSelected === 0) return "plan_only";
  if (balanceSelected > 0 && planSelected === 0) return "balance_only";
  if (planSelected !== balanceSelected) return planSelected > balanceSelected ? "plan_only" : "balance_only";
  if (groups.some((group) => group.billing_source === "plan_only")) return "plan_only";
  return "balance_only";
}

export function matchGroups(groups: AccessibleGroup[], query: string): GroupMatch[] {
  const needle = query.trim().toLowerCase();
  const out: GroupMatch[] = [];
  for (const group of groups) {
    if (!needle) {
      out.push({ group, vendors: [], models: [] });
      continue;
    }
    const nameHit = includes(group.name, needle) || includes(group.description, needle);
    const vendors = (group.vendors ?? []).filter((name) => includes(name, needle));
    const models = (group.models ?? []).filter((name) => includes(name, needle));
    if (nameHit || vendors.length > 0 || models.length > 0) {
      out.push({ group, vendors, models });
    }
  }
  return out;
}

export function selectionSummary(groups: AccessibleGroup[], selectedIds: string[], balanceLabel: string, hasPlan = true): string {
  const finish = (parts: string[]) => `${parts.join(i18n.t("keys.groups.summary.sep"))}${i18n.t("keys.groups.summary.end")}`;
  if (!hasPlan) {
    if (selectedIds.length === 0) {
      return i18n.t("keys.groups.summary.noneBalance");
    }
    const known = new Set(groups.map((group) => group.id));
    const missing = selectedIds.filter((id) => !known.has(id)).length;
    const parts = [i18n.t("keys.groups.summary.limited", { n: selectedIds.length })];
    if (missing > 0) parts.push(i18n.t("keys.groups.summary.missing", { n: missing }));
    parts.push(i18n.t("keys.groups.summary.noAutoAdd"));
    return finish(parts);
  }
  if (selectedIds.length === 0) {
    return i18n.t("keys.groups.summary.noneAll");
  }
  const byId = new Map(groups.map((group) => [group.id, group]));
  let plan = 0;
  let balance = 0;
  let missing = 0;
  for (const id of selectedIds) {
    const group = byId.get(id);
    if (!group) missing += 1;
    else if (group.billing_source === "plan_only") plan += 1;
    else balance += 1;
  }
  const parts = [i18n.t("keys.groups.summary.limited", { n: selectedIds.length })];
  if (plan > 0 && balance > 0) parts.push(i18n.t("keys.groups.summary.mixed", { plan, label: balanceLabel, balance }));
  else if (plan > 0) parts.push(i18n.t("keys.groups.summary.allPlan", { label: balanceLabel }));
  else if (balance > 0) parts.push(i18n.t("keys.groups.summary.allBalance", { label: balanceLabel }));
  if (missing > 0) parts.push(i18n.t("keys.groups.summary.missing", { n: missing }));
  parts.push(i18n.t("keys.groups.summary.noAutoAdd"));
  return finish(parts);
}

// hasSubscription: only a real subscription unlocks plan-only (订阅) groups and
// 套餐专属 keys. A pay_as_you_go "额度套餐" just grants balance — the same
// thing as the wallet — so it never shows the 订阅 tab or the plan-key guide.
export function hasSubscription<T extends { has_active_plan: boolean; plan_type?: string }>(plan?: T | null): plan is T {
  return !!plan?.has_active_plan && plan.plan_type !== "pay_as_you_go";
}

export function billingHint(
  mode: BillingSource,
  scope: "personal" | "organization",
  plan?: { has_active_plan: boolean; plan_type?: string } | null,
  planLoading = false,
): string {
  if (mode === "plan_only") {
    if (planLoading) return i18n.t("keys.groups.hint.readingPlan");
    if (!plan?.has_active_plan) return i18n.t("keys.groups.hint.noPlan");
    if (plan.plan_type === "pay_as_you_go") return i18n.t("keys.groups.hint.paygo");
    if (scope === "organization") return i18n.t("keys.groups.hint.planOrg");
    return i18n.t("keys.groups.hint.plan");
  }
  if (scope === "organization") return i18n.t("keys.groups.hint.balanceOrg");
  return i18n.t("keys.groups.hint.balance");
}

export interface ModelOverlap {
  model: string;
  groups: { id: string; name: string }[];
}

// overlappingModels lists models that the selected groups all can serve in
// more than one group. An empty selection has nothing to configure yet.
export function overlappingModels(groups: AccessibleGroup[], selectedIds: string[]): ModelOverlap[] {
  if (selectedIds.length === 0) return [];
  const chosen = new Set(selectedIds);
  const byModel = new Map<string, { id: string; name: string }[]>();
  for (const group of groups) {
    if (!chosen.has(group.id)) continue;
    for (const model of group.models ?? []) {
      const list = byModel.get(model) ?? [];
      if (!list.some((item) => item.id === group.id)) list.push({ id: group.id, name: group.name });
      byModel.set(model, list);
    }
  }
  return [...byModel.entries()]
    .filter(([, list]) => list.length >= 2)
    .map(([model, list]) => ({
      model,
      groups: [...list].sort((a, b) => a.name.localeCompare(b.name, "zh")),
    }))
    .sort((a, b) => a.model.localeCompare(b.model));
}

export function keptPins(pins: { model: string; group_id: string }[], overlaps: ModelOverlap[]) {
  return pins.filter((pin) => overlaps.some((row) => row.model === pin.model && row.groups.some((group) => group.id === pin.group_id)));
}

function includes(value: string | undefined, needle: string): boolean {
  return (value ?? "").toLowerCase().includes(needle);
}
