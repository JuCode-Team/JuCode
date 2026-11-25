/* @vitest-environment jsdom */
import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { AxiosError } from "axios";
import { http } from "../src/api/client";
import { UserPlansTab } from "../src/pages/admin/UserPlansTab";

globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};
Element.prototype.scrollIntoView ??= () => {};
const originalAdapter = http.defaults.adapter;
afterEach(() => { cleanup(); http.defaults.adapter = originalAdapter; });

const plan = { user_plan_id: "up1", plan_id: "p1", name: "测试套餐", type: "quota_subscription", status: "active", current: true,
  start_at: "2026-01-01T00:00:00Z", expire_at: "2099-01-01T00:00:00Z", quota_5h: "100", quota_weekly: "300", quota_monthly: "1000",
  used_5h: "25", used_weekly: "75", used_monthly: "100", requests: 12, total_cost: "123.45",
  reset_5h_at: "2026-01-01T05:00:00Z", reset_weekly_at: "2026-01-08T00:00:00Z", reset_monthly_at: "2026-01-31T00:00:00Z" };
function setup(fail = false) {
  const calls: { url: string; body: unknown; code: unknown }[] = [];
  http.defaults.adapter = async (config) => {
    if (config.method !== "get") {
      calls.push({ url: config.url!, body: config.data ? JSON.parse(config.data) : null, code: config.headers["X-TOTP-Code"] });
      if (fail) throw new AxiosError("重置失败", "ERR_BAD_RESPONSE", config, undefined, { status: 500, data: { error: "重置失败" }, statusText: "Error", headers: {}, config });
    }
    const data = { plans: config.url === "/v1/admin/plans" ? [{ id: "p1", name: "测试套餐", type: "quota_subscription", status: "active" }] : [plan] };
    return { data, status: 200, statusText: "OK", headers: {}, config };
  };
  const changed = vi.fn();
  render(<Theme><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })}><UserPlansTab userId="u1" onChanged={changed} /></QueryClientProvider></Theme>);
  return { calls, changed };
}
it("shows quota and statistics and confirms reset with the selected window", async () => {
  const { calls, changed } = setup();
  expect(await screen.findByText(/剩余 75 积分/)).toBeTruthy();
  expect(screen.getByText(/累计成功调用 12 次/)).toBeTruthy();
  expect(screen.getByText(`下次重置：${new Date("2026-01-31T00:00:00Z").toLocaleString()}`)).toBeTruthy();
  await userEvent.click(screen.getByRole("button", { name: "重置额度" }));
  expect(calls).toHaveLength(0);
  await userEvent.click(screen.getByRole("combobox", { name: "重置窗口" }));
  await userEvent.click(screen.getByRole("option", { name: "5 小时" }));
  await userEvent.click(screen.getByRole("button", { name: "确认重置额度" }));
  await waitFor(() => expect(changed).toHaveBeenCalledOnce());
  // A TOTP code is asked for by the global step-up prompt when the server
  // wants one, not inline here.
  expect(calls).toEqual([{ url: "/v1/admin/users/u1/plans/up1/reset-quota", body: { window: "5h" }, code: undefined }]);
});
it("creates, edits and deletes through scoped admin APIs", async () => {
  const { calls, changed } = setup();
  await screen.findByText(/剩余 75 积分/);
  await userEvent.click(screen.getByRole("button", { name: "新增套餐" }));
  await userEvent.click(screen.getByRole("combobox", { name: "套餐" }));
  await userEvent.click(screen.getByRole("option", { name: "测试套餐" }));
  await userEvent.click(screen.getByRole("button", { name: "确认新增套餐" }));
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(1));
  expect(calls[0].body).toEqual({ plan_id: "p1", request_id: expect.any(String) });
  await userEvent.click(screen.getByRole("button", { name: "修改套餐" }));
  await userEvent.click(screen.getByRole("button", { name: "确认修改套餐" }));
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(2));
  expect(calls[1].body).toEqual({ plan_id: "p1", expire_at: "2099-01-01T00:00:00.000Z" });
  await userEvent.click(screen.getByRole("button", { name: "删除套餐" }));
  expect(calls).toHaveLength(2);
  await userEvent.click(screen.getByRole("button", { name: "确认删除套餐" }));
  await waitFor(() => expect(changed).toHaveBeenCalledTimes(3));
  expect(calls[2].url).toBe("/v1/admin/users/u1/plans/up1");
});
it("keeps the confirmation open and reports failed mutations", async () => {
  const { changed } = setup(true);
  await userEvent.click(await screen.findByRole("button", { name: "重置额度" }));
  await userEvent.click(screen.getByRole("button", { name: "确认重置额度" }));
  expect(await screen.findByRole("alert")).toHaveProperty("textContent", "重置失败");
  expect(changed).not.toHaveBeenCalled();
});
