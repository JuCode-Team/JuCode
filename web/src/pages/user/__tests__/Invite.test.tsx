/** @vitest-environment jsdom */
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Theme } from "@radix-ui/themes";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

// Node 25 ships a localStorage global that is undefined without
// --localstorage-file and shadows jsdom's; i18n reads it at import time.
vi.hoisted(() => {
  if (!globalThis.localStorage) {
    const store = new Map<string, string>();
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      value: {
        getItem: (k: string) => store.get(k) ?? null,
        setItem: (k: string, v: string) => void store.set(k, v),
        removeItem: (k: string) => void store.delete(k),
        clear: () => store.clear(),
      },
    });
  }
});
import Invite from "../Invite";
import { meApi, type InviteResponse } from "../../../api/endpoints";

vi.mock("../../../api/endpoints", () => ({ meApi: { invite: vi.fn(), sendEmailVerification: vi.fn() } }));
vi.mock("../../../components/PointAmount", () => ({ PointAmount: ({ amount }: { amount: string }) => <span>{amount}</span> }));
vi.mock("../../../components/ConsoleSkeleton", () => ({ RouteSkeleton: () => <div>加载中</div> }));

const data = {
  invite_code: "ABCDEFGH", invite_url: "https://example.com/register?code=ABCDEFGH",
  inviter_email_verification_required: true,
  program: { enabled: true, signup_reward_enabled: false, topup_rebate_enabled: false, topup_rebate_window_days: 0, risk_review: true, email_verification_hold: true },
  stats: { invited_count: 1, pending_count: 0, signup_reward_paid: "0", topup_rebate_paid: "0" },
  invitees: [{
    user_id: "friend", email_masked: "friend@***", created_at: "2026-10-08T00:00:00Z",
    signup_reward_status: "held", signup_reward_amount: "5", signup_condition: "none",
    signup_email_verification_required: true, rebate_paid: "0", rebate_held: "10",
    rebate_email_verification_required: true,
    signup_reward_held_reasons: ["inviter_email_unverified"], rebate_held_reasons: ["inviter_email_unverified"],
  }],
} as InviteResponse;

function renderInvite() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 30_000, refetchOnWindowFocus: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/me/invite"]}><Theme><Invite /></Theme></MemoryRouter></QueryClientProvider>);
  return client;
}

beforeEach(() => {
  vi.mocked(meApi.invite).mockResolvedValue(data);
  vi.mocked(meApi.sendEmailVerification).mockResolvedValue({ ok: true, email: "owner@example.com", cooldown_seconds: 60 });
});
afterEach(() => { cleanup(); focusManager.setFocused(undefined); vi.clearAllMocks(); });

it("sends current-email verification from each unverified warning and shares the cooldown", async () => {
  renderInvite();
  const buttons = await screen.findAllByRole("button", { name: "验证邮箱" });
  expect(buttons).toHaveLength(3);
  fireEvent.click(buttons[1]);
  await screen.findByText(/验证邮件已发送至 owner@example.com/);
  expect(meApi.sendEmailVerification).toHaveBeenCalledTimes(1);
  for (const button of screen.getAllByRole("button", { name: "60 秒后重发" })) {
    expect((button as HTMLButtonElement).disabled).toBe(true);
  }
  expect(screen.getByText("请先验证邮箱。验证后，因邮箱未验证而暂扣的奖励和返利会自动发放。")).toBeTruthy();
});

it("shows delivery errors and allows retry", async () => {
  vi.mocked(meApi.sendEmailVerification).mockRejectedValueOnce(new Error("邮件服务未配置，请联系管理员。"));
  renderInvite();
  fireEvent.click((await screen.findAllByRole("button", { name: "验证邮箱" }))[0]);
  await screen.findByRole("alert");
  expect(screen.getByText("邮件服务未配置，请联系管理员。")).toBeTruthy();
  fireEvent.click(screen.getAllByRole("button", { name: "验证邮箱" })[0]);
  await screen.findByText(/验证邮件已发送至/);
  expect(meApi.sendEmailVerification).toHaveBeenCalledTimes(2);
});

it("refreshes the warning after the email was verified in another tab", async () => {
  renderInvite();
  await screen.findAllByRole("button", { name: "验证邮箱" });
  vi.mocked(meApi.invite).mockResolvedValue({ ...data, inviter_email_verification_required: false, invitees: [] });
  await act(async () => { focusManager.setFocused(false); focusManager.setFocused(true); });
  await waitFor(() => expect(screen.queryByRole("button", { name: "验证邮箱" })).toBeNull());
  expect(screen.queryByText("你的邮箱尚未验证")).toBeNull();
});

it("explains held payouts and the rebate window", async () => {
  vi.mocked(meApi.invite).mockResolvedValue({
    ...data,
    inviter_email_verification_required: false,
    program: { ...data.program, topup_rebate_enabled: true, topup_rebate_rate_pct: "10", topup_rebate_window_days: 14 },
    invitees: [{
      ...data.invitees[0],
      signup_email_verification_required: false,
      rebate_email_verification_required: false,
      signup_reward_held_reasons: ["self_referral_suspected"],
      rebate_held_reasons: ["inviter_email_unverified", "amount_review"],
    }],
  });
  renderInvite();
  await screen.findByText(/朋友注册后 14 天内的每笔余额充值/);
  expect(screen.getByText(/超过 14 天的充值不再返利/)).toBeTruthy();
  expect(screen.getByText("奖励暂扣：疑似与你是同一人，需管理员复核后发放。")).toBeTruthy();
  expect(screen.getByText(/暂扣：产生时你的邮箱尚未验证；金额较大，需人工核对，需管理员复核/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "验证邮箱" })).toBeNull();
});
