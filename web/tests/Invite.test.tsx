/* @vitest-environment jsdom */
import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { http } from "../src/api/client";
import Invite from "../src/pages/user/Invite";
import AdminInvite from "../src/pages/admin/Invite";

globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};
Element.prototype.scrollIntoView ??= () => {};
const originalAdapter = http.defaults.adapter;
afterEach(() => { cleanup(); http.defaults.adapter = originalAdapter; });

function show(page: React.ReactNode) {
  render(<Theme><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter>{page}</MemoryRouter>
  </QueryClientProvider></Theme>);
}

it("shows paid-order progress and the inviter email hold for each invitee", async () => {
  http.defaults.adapter = async (config) => ({
    data: {
      invite_code: "9SEZN7VT", invite_url: "https://test.invalid/register?code=9SEZN7VT",
      program: { enabled: true, signup_reward_enabled: true, signup_reward_amount: "5",
        signup_condition: "min_topup", signup_min_topup_amount: "50", topup_rebate_enabled: true, topup_rebate_rate_pct: "10",
        topup_rebate_window_days: 14, risk_review: true, email_verification_hold: true },
      stats: { invited_count: 1, pending_count: 0, qualified_count: 0, held_count: 1, signup_reward_paid: "0", topup_rebate_paid: "0" },
      inviter_email_verification_required: true,
      invitees: [{ user_id: "invitee", email_masked: "s***@example.com", created_at: "2026-10-07T06:12:24Z",
        signup_reward_status: "held", signup_reward_amount: "5", rebate_paid: "0", rebate_held: "0.1",
        signup_condition: "min_topup", signup_paid_amount: "50", signup_required_amount: "50",
        signup_email_verification_required: true, rebate_email_verification_required: true,
        signup_reward_held_reasons: ["inviter_email_unverified"], rebate_held_reasons: ["inviter_email_unverified"] }],
      rewards: [], rebates: [], total: 1,
    }, status: 200, statusText: "OK", headers: {}, config,
  });
  show(<Invite />);
  await screen.findByText("你的邮箱尚未验证");
  const row = within(screen.getByText("s***@example.com").closest("tr")!);
  expect(row.getByText("已达标")).toBeTruthy();
  expect(row.getByText("已实付", { exact: false }).textContent).toContain("50");
  expect(row.getByText("已暂扣")).toBeTruthy();
  expect(row.getByText("你的邮箱未验证，验证后自动发放。")).toBeTruthy();
  expect(row.getByText("验证邮箱后自动发放", { exact: false }).textContent).toContain("你的邮箱未验证");
});

it("never sends incomplete user IDs while filtering rebates, including refresh", async () => {
  const requests: Array<Record<string, unknown>> = [];
  http.defaults.adapter = async (config) => {
    if (config.url === "/v1/admin/invite/rebates") requests.push({ ...config.params });
    return { data: config.url === "/v1/admin/settings" ? {} : { bindings: [], rewards: [], rebates: [], total: 0, limit: 50, offset: 0 },
      status: 200, statusText: "OK", headers: {}, config };
  };
  show(<AdminInvite />);
  await userEvent.click(await screen.findByRole("tab", { name: /充值返利/ }));
  const panel = within(screen.getByRole("tabpanel"));
  const input = panel.getByRole("textbox", { name: "被邀请人 ID" });
  await userEvent.type(input, "f4eaa013");
  expect(panel.getByText("请输入完整用户 ID（36 位，含连字符）")).toBeTruthy();
  const beforeRefresh = requests.length;
  await userEvent.click(panel.getByRole("button", { name: "刷新" }));
  expect(requests.length).toBe(beforeRefresh);
  await userEvent.type(input, "-fbd7-4732-9eab-a8733fed4154");
  await waitFor(() => expect(requests.some((p) => p.invitee_id === "f4eaa013-fbd7-4732-9eab-a8733fed4154")).toBe(true));
  expect(requests.every((p) => !p.invitee_id || /^[0-9a-f-]{36}$/i.test(String(p.invitee_id)))).toBe(true);
});
