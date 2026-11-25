/* @vitest-environment jsdom */
import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it } from "vitest";
import { http } from "../src/api/client";
import AdminUserPlans from "../src/pages/admin/AdminUserPlans";

globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};
Element.prototype.scrollIntoView ??= () => {};
const originalAdapter = http.defaults.adapter;
afterEach(() => { cleanup(); http.defaults.adapter = originalAdapter; });

const row = (id: string, email: string) => ({ user_plan_id: id, user_id: "u-" + id, email, username: "", plan_id: "p1", plan_name: "订阅套餐",
  start_at: "2026-09-01T00:00:00Z", expire_at: "2026-10-01T00:00:00Z", quota_5h: "10", quota_weekly: "50", quota_monthly: "100",
  used_5h: "1", used_weekly: "2", used_monthly: "3", banked_resets: [] });

function setup() {
  const posts: unknown[] = [];
  http.defaults.adapter = async (config) => {
    let data: unknown;
    if (config.method === "post") {
      posts.push(JSON.parse(config.data));
      data = { job: { id: "j1", action: "clear", status: "running", total: 3, done: 0, failed: 0, errors: [], created_at: "2026-09-28T00:00:00Z", updated_at: "2026-09-28T00:00:00Z" } };
    } else if (config.url === "/v1/admin/user-plans") {
      data = { user_plans: [row("up1", "a@x.test"), row("up2", "b@x.test")], total: 3, limit: 50, offset: 0 };
    } else if (config.url === "/v1/admin/user-plans/reset-jobs") {
      data = { jobs: [] };
    } else {
      data = { plans: [{ id: "p1", name: "订阅套餐", type: "quota_subscription", status: "active" }] };
    }
    return { data, status: 200, statusText: "OK", headers: {}, config };
  };
  render(<Theme><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><AdminUserPlans /></QueryClientProvider></Theme>);
  return posts;
}

it("resets the ticked plans by id", async () => {
  const posts = setup();
  await screen.findByText("a@x.test");
  await userEvent.click(screen.getAllByRole("checkbox")[1]);
  await userEvent.click(screen.getByRole("button", { name: /批量重置（1）/ }));
  await userEvent.click(screen.getByRole("button", { name: "确认重置" }));
  await waitFor(() => expect(posts).toEqual([{ action: "clear", window: "all", ids: ["up1"] }]));
});

it("restarts every plan matching the filter, not just the page", async () => {
  const posts = setup();
  await screen.findByText("a@x.test");
  await userEvent.click(screen.getByRole("button", { name: "选择筛选结果全部 3 条" }));
  await userEvent.click(screen.getByRole("button", { name: /批量重置（3）/ }));
  await userEvent.click(screen.getByRole("radio", { name: /提前重置/ }));
  await userEvent.click(screen.getByRole("button", { name: "确认重置" }));
  await waitFor(() => expect(posts).toEqual([{ action: "restart", window: "all", filter: {} }]));
});
