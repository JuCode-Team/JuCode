/** @vitest-environment jsdom */
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import KeyAccountStatus from "./KeyAccountStatus";
import type { OrgOverview, PlanUsage } from "../api/endpoints";

afterEach(() => cleanup());

const quotaPlan: PlanUsage = {
  has_active_plan: true,
  plan_name: "Claude Pro",
  plan_type: "quota_subscription",
  expire_at: "2026-10-27T00:00:00.000Z",
  quota_5h: "100",
  used_5h: "12",
  quota_weekly: "500",
  used_weekly: "40",
  quota_monthly: "2000",
  used_monthly: "80",
};

describe("KeyAccountStatus", () => {
  it("shows subscription windows and wallet balance together", () => {
    render(
      <KeyAccountStatus
        scope="personal"
        plan={quotaPlan}
        planLoading={false}
        balance="128.5"
        balanceLoading={false}
      />,
    );
    expect(screen.getByText("Claude Pro")).toBeTruthy();
    expect(screen.getByText("5 小时")).toBeTruthy();
    expect(screen.getByText("7 天")).toBeTruthy();
    expect(screen.getByText("30 天")).toBeTruthy();
    expect(screen.getByText("余额")).toBeTruthy();
    expect(screen.getByText("128.50")).toBeTruthy();
  });

  it("shows a pay-as-you-go plan as monthly wallet spend, not quota windows", () => {
    render(
      <KeyAccountStatus
        scope="personal"
        plan={{ has_active_plan: true, plan_name: "按量套餐", plan_type: "pay_as_you_go", monthly_spend_used: "3", monthly_spend_limit: "0" }}
        planLoading={false}
        balance="10"
        balanceLoading={false}
      />,
    );
    expect(screen.getByText("30 天消费")).toBeTruthy();
    expect(screen.getByText("/ 不限")).toBeTruthy();
    expect(screen.queryByText("5 小时")).toBeNull();
  });

  it("shows org balance and the member caps, and says subscription still bills the person", () => {
    const org: OrgOverview = {
      organization: { id: "o", name: "Acme", currency: "CNY", created_at: "" },
      my_role: "member",
      my_monthly_spend: "2",
      my_monthly_spend_limit: "20",
      my_total_spend: "9",
      my_total_spend_limit: "0",
    };
    render(
      <KeyAccountStatus
        scope="organization"
        plan={quotaPlan}
        planLoading={false}
        balance="1"
        balanceLoading={false}
        org={org}
        orgLoading={false}
      />,
    );
    expect(screen.getByText(/不扣组织余额/)).toBeTruthy();
    expect(screen.getByText("我的本月")).toBeTruthy();
    expect(screen.getByText("我的累计")).toBeTruthy();
    expect(screen.queryByText("组织余额")).toBeNull();
  });

  it("omits the subscription block when the account has no plan", () => {
    render(
      <KeyAccountStatus
        scope="personal"
        plan={{ has_active_plan: false }}
        planLoading={false}
        balance="128.5"
        balanceLoading={false}
      />,
    );
    expect(screen.queryByText("订阅")).toBeNull();
    expect(screen.queryByText("没有生效的订阅")).toBeNull();
    expect(screen.getByText("余额")).toBeTruthy();
    expect(screen.getByText("128.50")).toBeTruthy();
  });
});
