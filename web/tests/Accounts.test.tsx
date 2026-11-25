/* @vitest-environment jsdom */

import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it } from "vitest";
import { http } from "../src/api/client";
import type { Account } from "../src/api/endpoints";
import Accounts from "../src/pages/admin/Accounts";

const originalAdapter = http.defaults.adapter;

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
(globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver =
  (globalThis as unknown as { ResizeObserver?: unknown }).ResizeObserver ?? ResizeObserverStub;

afterEach(() => {
  cleanup();
  http.defaults.adapter = originalAdapter;
});

type Call = { method: string; url: string; body?: unknown; params?: unknown };

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: "a1",
    name: "主账号",
    base_url: "https://api.deepseek.com",
    native_protocols: ["chat_completions"],
    allow_conversion: false,
    status: "active",
    health: "healthy",
    priority: 10,
    weight: 100,
    max_concurrency: 0,
    has_credential: true,
    cooldown_active: false,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    schedulable: true,
    effective_pool_mode: "balanced",
    consecutive_failures: 0,
    schedulable_now: true,
    ...overrides,
  };
}

function renderAccounts(accounts: Account[], calls: Call[]) {
  http.defaults.adapter = async (config) => {
    const method = (config.method ?? "get").toLowerCase();
    const url = config.url ?? "";
    const body = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
    calls.push({ method, url, body, params: config.params });
    const reply = (data: unknown) => ({ data, status: 200, statusText: "OK", headers: {}, config });
    if (method === "get" && url === "/v1/admin/accounts") return reply({ accounts, limit: 50, offset: 0 });
    if (method === "get" && url === "/v1/admin/account-pools") return reply({ pools: [] });
    if (method === "put" && url.startsWith("/v1/admin/accounts/") && url.endsWith("/schedulable")) return reply(makeAccount());
    if (method === "post" && url === "/v1/admin/accounts/a1/recover") return reply(makeAccount());
    if (method === "delete" && url.startsWith("/v1/admin/accounts/")) return reply({ ok: true });
    throw new Error(`unexpected request: ${method} ${url}`);
  };

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });

  return render(
    <Theme>
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Accounts />
        </MemoryRouter>
      </QueryClientProvider>
    </Theme>,
  );
}

describe("accounts list", () => {
  it("renders the account with its cooldown and quota-window badges", async () => {
    const resetAt = new Date(Date.now() + 30 * 60 * 1000).toISOString();
    const account = makeAccount({
      rate_limit_reset_at: resetAt,
      consecutive_failures: 4,
      last_error_class: "rate_limit",
      quota_snapshot: { source: "passive", windows: [{ name: "5h", used_percent: 97, reset_at: Math.floor(Date.now() / 1000) + 1800 }] },
    });
    const calls: Call[] = [];
    renderAccounts([account], calls);

    expect(await screen.findByText("主账号")).toBeTruthy();
    expect(screen.getByText(/429 限流/)).toBeTruthy();
    expect(screen.getByText("5h")).toBeTruthy();
    expect(screen.getByText("97%")).toBeTruthy();
    // The endpoint & protocol declaration is part of the row.
    expect(screen.getByText("Chat 补全")).toBeTruthy();
  });

  it("passes the search term through to the API", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderAccounts([makeAccount()], calls);
    await screen.findByText("主账号");

    await user.type(screen.getByPlaceholderText("搜索名称 / URL / 外部身份..."), "team-a");

    await waitFor(() => {
      const listCalls = calls.filter((c) => c.method === "get" && c.url === "/v1/admin/accounts");
      expect(listCalls.some((c) => (c.params as { q?: string })?.q === "team-a")).toBe(true);
    }, { timeout: 2500 });
  });

  it("bulk-pauses scheduling for the selected accounts", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderAccounts([makeAccount()], calls);
    await screen.findByText("主账号");

    const checkboxes = screen.getAllByRole("checkbox");
    await user.click(checkboxes[checkboxes.length - 1]);
    await user.click(await screen.findByRole("button", { name: "停用调度" }));

    await waitFor(() => {
      const put = calls.find((c) => c.method === "put" && c.url === "/v1/admin/accounts/a1/schedulable");
      expect(put).toBeTruthy();
      expect(put?.body).toEqual({ schedulable: false });
    });
  });

  it("recovers an account from the row action", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderAccounts([makeAccount()], calls);
    await screen.findByText("主账号");

    await user.click(screen.getByRole("button", { name: "恢复账号" }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === "post" && c.url === "/v1/admin/accounts/a1/recover")).toBe(true);
    });
  });

  it("offers quota refresh only for accounts whose catalog has a usage endpoint", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderAccounts([
      makeAccount(),
      makeAccount({ id: "a2", name: "Anthropic", has_usage_endpoint: true }),
    ], calls);
    await screen.findByText("主账号");

    const menus = screen.getAllByRole("button", { name: "更多操作" });
    // Plain api-key account: no usage probe exists, so the
    // action is not offered at all. Wait for the menu to open first (删除 is
    // always there), so the absence is a real assertion.
    await user.click(menus[0]);
    await screen.findByRole("menuitem", { name: /删除/ });
    expect(screen.queryByRole("menuitem", { name: /查询额度/ })).toBeNull();
    await user.keyboard("{Escape}");

    await user.click(menus[1]);
    expect(await screen.findByRole("menuitem", { name: /查询额度/ })).toBeTruthy();
  });
});
