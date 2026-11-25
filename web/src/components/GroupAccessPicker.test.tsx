/** @vitest-environment jsdom */
import { cleanup, render, screen } from "@testing-library/react";
import userEvent, { PointerEventsCheckLevel } from "@testing-library/user-event";
import { Theme } from "@radix-ui/themes";
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";
import GroupAccessPicker from "./GroupAccessPicker";
import type { AccessibleGroup } from "../lib/groupAccess";

afterEach(() => cleanup());

// 超过 6 个分组才出现厂商筛选与搜索；用填充分组把两侧都撑过阈值。
const filler = (source: AccessibleGroup["billing_source"], prefix: string): AccessibleGroup[] =>
  Array.from({ length: 6 }, (_, i) => ({
    id: `${prefix}-fill-${i}`,
    name: `${prefix}备用 ${i + 1}`,
    billing_source: source,
    rate_multiplier: 1,
    vendors: ["DeepSeek"],
    models: ["deepseek-v3"],
  }));

const base: AccessibleGroup[] = [
  {
    id: "plan-1",
    name: "Claude 订阅",
    billing_source: "plan_only",
    rate_multiplier: 1,
    vendors: ["Anthropic"],
    models: ["claude-sonnet-4-6"],
    supports_image: true,
    exclusive: true,
    status: "healthy",
  },
  {
    id: "bal-1",
    name: "官方按量",
    billing_source: "balance_only",
    rate_multiplier: 1.2,
    vendors: ["OpenAI"],
    models: ["gpt-4o"],
    description: "官方直连",
  },
];
const groups: AccessibleGroup[] = [...base, ...filler("plan_only", "订阅"), ...filler("balance_only", "按量")];

function Harness() {
  const [value, setValue] = useState<string[]>([]);
  return (
    <Theme>
      <GroupAccessPicker
        groups={groups}
        value={value}
        onChange={setValue}
        balanceLabel="钱包按量"
        plan={{ has_active_plan: true, plan_type: "quota_subscription" }}
      />
    </Theme>
  );
}

describe("GroupAccessPicker", () => {
  it("starts on subscription groups and searches wallet groups by model", async () => {
    const user = userEvent.setup({ pointerEventsCheck: PointerEventsCheckLevel.Never });
    render(<Harness />);

    expect(screen.getByText("Claude 订阅")).toBeTruthy();
    expect(screen.getByRole("button", { name: "全部", pressed: true })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Claude/ })).toBeTruthy();
    expect(screen.getByText("生图")).toBeTruthy();
    expect(screen.getByText("代理专属")).toBeTruthy();
    expect(screen.getByText("正常")).toBeTruthy();
    expect(screen.queryByText("官方按量")).toBeNull();
    expect(screen.getByText(/从当前订阅额度扣除/)).toBeTruthy();

    await user.click(screen.getByRole("radio", { name: "钱包按量" }));
    expect(screen.getByText("官方按量")).toBeTruthy();
    expect(screen.queryByText("Claude 订阅")).toBeNull();

    await user.type(screen.getByPlaceholderText("搜索厂商、模型或分组"), "gpt-4o");
    expect(screen.getByText("官方按量")).toBeTruthy();
    expect(screen.getByText("匹配 gpt-4o")).toBeTruthy();

    await user.clear(screen.getByPlaceholderText("搜索厂商、模型或分组"));
    await user.click(screen.getByRole("checkbox", { name: "官方按量" }));
    expect(screen.getByText(/不能走订阅/)).toBeTruthy();
    expect(screen.getByRole("radio", { name: "钱包按量 · 1" })).toBeTruthy();
  });

  it("skips the subscription choice and lists wallet groups when there is no plan", () => {
    render(
      <Theme>
        <GroupAccessPicker
          groups={base}
          value={[]}
          onChange={() => {}}
          balanceLabel="钱包按量"
          plan={{ has_active_plan: false }}
        />
      </Theme>,
    );

    expect(screen.queryByRole("radio", { name: "订阅" })).toBeNull();
    expect(screen.getByText("官方按量")).toBeTruthy();
    expect(screen.queryByText("Claude 订阅")).toBeNull();
    expect(screen.getByText(/从钱包余额扣除/)).toBeTruthy();
  });

  it("treats a credit pack (pay_as_you_go) like the wallet: no subscription choice", () => {
    render(
      <Theme>
        <GroupAccessPicker
          groups={base}
          value={[]}
          onChange={() => {}}
          balanceLabel="钱包按量"
          plan={{ has_active_plan: true, plan_type: "pay_as_you_go" }}
        />
      </Theme>,
    );

    expect(screen.queryByRole("radio", { name: "订阅" })).toBeNull();
    expect(screen.queryByText("Claude 订阅")).toBeNull();
    expect(screen.getByText("官方按量")).toBeTruthy();
  });

  it("keeps a short list bare and appends the account context to the billing line", () => {
    render(
      <Theme>
        <GroupAccessPicker
          groups={base}
          value={[]}
          onChange={() => {}}
          balanceLabel="钱包按量"
          plan={{ has_active_plan: false }}
          context={{ balance_only: "钱包余额 183.38" }}
        />
      </Theme>,
    );

    expect(screen.getByText("这些分组按倍率从钱包余额扣除 · 钱包余额 183.38")).toBeTruthy();
    expect(screen.queryByPlaceholderText("搜索厂商、模型或分组")).toBeNull();
    expect(screen.queryByRole("button", { name: "全部" })).toBeNull();
    expect(screen.queryByRole("button", { name: "全选" })).toBeNull();
    expect(screen.queryByText("gpt-4o")).toBeNull();
    expect(screen.queryByText("官方直连")).toBeNull();
  });

  it("shows every group until a vendor is chosen", async () => {
    const user = userEvent.setup({ pointerEventsCheck: PointerEventsCheckLevel.Never });
    const listed: AccessibleGroup[] = [
      ...groups,
      {
        id: "plan-2",
        name: "GPT 订阅",
        billing_source: "plan_only",
        vendors: ["OpenAI"],
        models: ["gpt-4o"],
        status: "healthy",
      },
      {
        id: "plan-3",
        name: "Grok 订阅",
        billing_source: "plan_only",
        vendors: ["xAI"],
        models: ["grok-4"],
        status: "healthy",
      },
    ];
    render(
      <Theme>
        <GroupAccessPicker
          groups={listed}
          value={[]}
          onChange={() => {}}
          balanceLabel="钱包按量"
          plan={{ has_active_plan: true, plan_type: "quota_subscription" }}
        />
      </Theme>,
    );

    expect(screen.getByText("Claude 订阅")).toBeTruthy();
    expect(screen.getByText("GPT 订阅")).toBeTruthy();
    expect(screen.getByText("Grok 订阅")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: /OpenAI/ }));
    expect(screen.getByText("GPT 订阅")).toBeTruthy();
    expect(screen.queryByText("Claude 订阅")).toBeNull();
    expect(screen.queryByText("Grok 订阅")).toBeNull();

    await user.click(screen.getByRole("button", { name: "全部" }));
    expect(screen.getByText("Claude 订阅")).toBeTruthy();
    expect(screen.getByText("Grok 订阅")).toBeTruthy();
  });

  it("does not empty the other billing side when its groups lack the chosen vendor", async () => {
    const user = userEvent.setup({ pointerEventsCheck: PointerEventsCheckLevel.Never });
    render(<Harness />);

    await user.click(screen.getByRole("button", { name: /Claude/ }));
    expect(screen.queryByText("官方按量")).toBeNull();

    await user.click(screen.getByRole("radio", { name: "钱包按量" }));
    expect(screen.getByText("官方按量")).toBeTruthy();
    expect(screen.getByRole("button", { name: "全部", pressed: true })).toBeTruthy();

    await user.click(screen.getByRole("radio", { name: "订阅" }));
    expect(screen.getByText("Claude 订阅")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Claude/, pressed: true })).toBeTruthy();
  });
});
