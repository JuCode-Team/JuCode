import { describe, expect, it } from "vitest";

import {
  candidateGroups, groupsForModel, LAB_AUTO_GROUP, labBillingSource, modelsForGroups, reconcileLabPick,
  type LabPickGroup, type LabPickModel,
} from "./labPick";

const balance: LabPickGroup = { id: "balance", name: "余额", billing_source: "balance_only", rate_multiplier: 1.2 };
const cheap: LabPickGroup = { id: "cheap", name: "特价", billing_source: "balance_only", rate_multiplier: 0.8 };
const plan: LabPickGroup = { id: "plan", name: "套餐", billing_source: "plan_only", rate_multiplier: 1 };
const hidden: LabPickGroup = { id: "hidden", name: "未开放", billing_source: "balance_only", rate_multiplier: 1 };

const models: LabPickModel[] = [
  { name: "gpt-wallet", groups: [balance] },
  { name: "claude-both", groups: [plan, balance, cheap] },
  { name: "opus-plan", groups: [plan] },
  { name: "secret", groups: [hidden] },
];

const wallet = candidateGroups(models, "balance_only", new Set(["balance", "cheap", "plan"]));
const planScope = candidateGroups(models, "plan_only", new Set(["balance", "cheap", "plan"]));

describe("lab billing scope", () => {
  it("maps the 仅套餐 switch to a billing source", () => {
    expect(labBillingSource(false)).toBe("balance_only");
    expect(labBillingSource(true)).toBe("plan_only");
  });

  it("keeps only reachable groups of the chosen billing source", () => {
    expect(wallet.map((group) => group.id)).toEqual(["balance", "cheap"]);
    expect(planScope.map((group) => group.id)).toEqual(["plan"]);
    expect(candidateGroups(models, "balance_only", null).map((group) => group.id)).toEqual(["balance", "cheap", "hidden"]);
  });

  it("lists models the scope serves", () => {
    expect(modelsForGroups(models, wallet).map((model) => model.name)).toEqual(["gpt-wallet", "claude-both"]);
    expect(modelsForGroups(models, planScope).map((model) => model.name)).toEqual(["claude-both", "opus-plan"]);
  });

  it("offers only the scope's groups that carry the model, cheapest first", () => {
    expect(groupsForModel(models[1], wallet).map((group) => group.id)).toEqual(["cheap", "balance"]);
    expect(groupsForModel(models[1], planScope).map((group) => group.id)).toEqual(["plan"]);
    expect(groupsForModel(models[0], planScope)).toEqual([]);
    expect(groupsForModel(undefined, wallet)).toEqual([]);
  });
});

describe("reconcileLabPick", () => {
  it("keeps a model and group the current scope offers", () => {
    expect(reconcileLabPick(models, wallet, "cheap", "claude-both")).toEqual({ groupId: "cheap", model: "claude-both", note: "" });
    expect(reconcileLabPick(models, wallet, LAB_AUTO_GROUP, "gpt-wallet")).toEqual({ groupId: LAB_AUTO_GROUP, model: "gpt-wallet", note: "" });
  });

  it("hides a model the current billing scope does not offer", () => {
    expect(reconcileLabPick(models, planScope, LAB_AUTO_GROUP, "gpt-wallet")).toEqual({
      groupId: LAB_AUTO_GROUP,
      model: "",
      note: "当前计费方式下没有「gpt-wallet」，请重新选择模型",
    });
    expect(reconcileLabPick(models, wallet, LAB_AUTO_GROUP, "gpt-wallet").model).toBe("gpt-wallet");
  });

  it("falls back to automatic when the saved group is outside this billing scope", () => {
    expect(reconcileLabPick(models, wallet, "plan", "claude-both")).toEqual({
      groupId: LAB_AUTO_GROUP,
      model: "claude-both",
      note: "刚才的分组不在这个计费方式里，已改回自动",
    });
    expect(reconcileLabPick(models, planScope, "plan", "claude-both")).toEqual({ groupId: "plan", model: "claude-both", note: "" });
  });

  it("falls back to automatic when the group does not carry the model", () => {
    expect(reconcileLabPick(models, wallet, "cheap", "gpt-wallet").groupId).toBe(LAB_AUTO_GROUP);
  });
});
