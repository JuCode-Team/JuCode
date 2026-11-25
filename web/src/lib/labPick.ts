import i18n from "../i18n";

export const LAB_AUTO_GROUP = "_auto";

export type LabBillingSource = "balance_only" | "plan_only";

export interface LabPickGroup {
  id: string;
  name?: string;
  billing_source?: string;
  rate_multiplier?: number;
}

export interface LabPickModel<G extends LabPickGroup = LabPickGroup> {
  name: string;
  groups?: G[];
}

export function labBillingSource(planOnly: boolean): LabBillingSource {
  return planOnly ? "plan_only" : "balance_only";
}

/** Groups of one billing source that the user can reach. `accessible` null means every group. */
export function candidateGroups<G extends LabPickGroup>(
  models: LabPickModel<G>[],
  source: LabBillingSource,
  accessible: Set<string> | null,
): G[] {
  const seen = new Map<string, G>();
  for (const model of models) {
    for (const group of model.groups ?? []) {
      if (seen.has(group.id) || group.billing_source !== source) continue;
      if (accessible && !accessible.has(group.id)) continue;
      seen.set(group.id, group);
    }
  }
  return [...seen.values()];
}

/** Models that at least one candidate group serves. */
export function modelsForGroups<T extends LabPickModel>(models: T[], groups: LabPickGroup[]): T[] {
  const allowed = new Set(groups.map((group) => group.id));
  return models.filter((model) => model.groups?.some((group) => allowed.has(group.id)));
}

/** Candidate groups that carry this model, cheapest first. */
export function groupsForModel<G extends LabPickGroup>(model: LabPickModel | undefined, groups: G[]): G[] {
  if (!model) return [];
  const own = new Set((model.groups ?? []).map((group) => group.id));
  return groups
    .filter((group) => own.has(group.id))
    .sort((a, b) => (a.rate_multiplier ?? 1) - (b.rate_multiplier ?? 1) || (a.name ?? a.id).localeCompare(b.name ?? b.id));
}

// Billing scope decides the model list; the chosen model decides the group
// list. An impossible pair clears only the visible choice; the caller keeps
// the stored one so a later compatible scope can show it again.
export function reconcileLabPick<T extends LabPickModel>(
  models: T[],
  groups: LabPickGroup[],
  groupId: string,
  model: string,
): { groupId: string; model: string; note: string } {
  const offered = modelsForGroups(models, groups);
  const picked = model ? offered.find((item) => item.name === model) : undefined;
  if (model && !picked) {
    return { groupId: LAB_AUTO_GROUP, model: "", note: i18n.t("bench.pick.modelGone", { model }) };
  }
  if (groupId === LAB_AUTO_GROUP || !picked) return { groupId: LAB_AUTO_GROUP, model, note: "" };
  if (groupsForModel(picked, groups).some((group) => group.id === groupId)) return { groupId, model, note: "" };
  return { groupId: LAB_AUTO_GROUP, model, note: i18n.t("bench.pick.groupReset") };
}
