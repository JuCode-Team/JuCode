import { describe, expect, it } from "vitest";
import { billingHint, hasSubscription, keptPins, matchGroups, overlappingModels, preferredBillingMode, selectionSummary, type AccessibleGroup } from "./groupAccess";

const groups: AccessibleGroup[] = [
  {
    id: "plan-1",
    name: "Claude 订阅",
    billing_source: "plan_only",
    description: "套餐内调用",
    vendors: ["Anthropic"],
    models: ["claude-sonnet-4-6", "claude-opus-4-6"],
  },
  {
    id: "bal-1",
    name: "官方按量",
    billing_source: "balance_only",
    vendors: ["OpenAI"],
    models: ["gpt-4o", "gpt-4.1"],
  },
  {
    id: "bal-2",
    name: "备用按量",
    billing_source: "balance_only",
    vendors: ["Anthropic"],
    models: ["claude-haiku-4"],
  },
];

describe("preferredBillingMode", () => {
  it("opens on 订阅 when the account has a plan and nothing is selected", () => {
    expect(preferredBillingMode(groups, [], true)).toBe("plan_only");
  });

  it("opens on 按量 when the account has no plan", () => {
    expect(preferredBillingMode(groups, [], false)).toBe("balance_only");
    expect(preferredBillingMode(groups, ["plan-1"], false)).toBe("balance_only");
  });

  it("opens on the billing kind already saved on the key", () => {
    expect(preferredBillingMode(groups, ["bal-1"], true)).toBe("balance_only");
    expect(preferredBillingMode(groups, ["plan-1"], true)).toBe("plan_only");
  });
});

describe("matchGroups", () => {
  it("matches vendor, model name, and group name", () => {
    const wallet = groups.filter((group) => group.billing_source === "balance_only");
    expect(matchGroups(wallet, "openai").map((match) => match.group.id)).toEqual(["bal-1"]);
    expect(matchGroups(wallet, "gpt-4o").map((match) => match.group.id)).toEqual(["bal-1"]);
    expect(matchGroups(wallet, "备用").map((match) => match.group.id)).toEqual(["bal-2"]);
    expect(matchGroups(wallet, "claude").map((match) => match.group.id)).toEqual(["bal-2"]);
  });

  it("reports which vendor or model matched", () => {
    const [hit] = matchGroups(groups, "opus");
    expect(hit.group.id).toBe("plan-1");
    expect(hit.models).toEqual(["claude-opus-4-6"]);
    expect(hit.vendors).toEqual([]);
  });
});

describe("hasSubscription", () => {
  it("counts only a real subscription, never a credit pack", () => {
    expect(hasSubscription({ has_active_plan: true, plan_type: "quota_subscription" })).toBe(true);
    expect(hasSubscription({ has_active_plan: true, plan_type: "pay_as_you_go" })).toBe(false);
    expect(hasSubscription({ has_active_plan: false })).toBe(false);
    expect(hasSubscription(undefined)).toBe(false);
  });
});

describe("billingHint", () => {
  it("does not describe a pay-as-you-go plan as quota", () => {
    expect(billingHint("plan_only", "personal", { has_active_plan: true, plan_type: "pay_as_you_go" })).toContain("个人钱包");
  });

  it("says an org key's subscription groups still draw the personal plan", () => {
    expect(billingHint("plan_only", "organization", { has_active_plan: true, plan_type: "quota_subscription" })).toContain("不扣组织余额");
  });

  it("does not claim there is no plan while usage is still loading", () => {
    expect(billingHint("plan_only", "personal", undefined, true)).toBe("正在读取订阅。");
  });
});

describe("selectionSummary", () => {
  it("explains an empty selection still allows every accessible group", () => {
    expect(selectionSummary(groups, [], "钱包按量")).toContain("全部分组");
  });

  it("says a subscription-only selection cannot use wallet groups", () => {
    expect(selectionSummary(groups, ["plan-1"], "钱包按量")).toContain("不能走钱包按量");
  });

  it("counts both billing kinds when the key mixes them", () => {
    expect(selectionSummary(groups, ["plan-1", "bal-2"], "钱包按量")).toContain("订阅 1 个，钱包按量 1 个");
  });

  it("talks only about 按量 groups when there is no subscription", () => {
    expect(selectionSummary(groups, [], "钱包按量", false)).toContain("按量分组");
    expect(selectionSummary(groups, [], "钱包按量", false)).not.toContain("全部分组");
  });
});

describe("overlappingModels", () => {
  const shared: AccessibleGroup[] = [
    { id: "pro", name: "Pro", billing_source: "balance_only", models: ["luna", "astra"] },
    { id: "azure", name: "Azure", billing_source: "balance_only", models: ["astra"] },
  ];

  it("lists only models that the selected groups share", () => {
    expect(overlappingModels(shared, ["pro", "azure"]).map((row) => row.model)).toEqual(["astra"]);
    expect(overlappingModels(shared, [])).toEqual([]);
  });

  it("drops a pin when its group is no longer selected", () => {
    const overlaps = overlappingModels(shared, ["pro"]);
    expect(keptPins([{ model: "astra", group_id: "azure" }], overlaps)).toEqual([]);
  });
});
