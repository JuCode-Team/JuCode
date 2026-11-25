/* @vitest-environment jsdom */

import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it } from "vitest";
import { http } from "../src/api/client";
import type { Account, AccountModel, GroupAccount, Model, ModelGroup } from "../src/api/endpoints";
import Groups from "../src/pages/admin/Groups";

const originalAdapter = http.defaults.adapter;

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
(globalThis as unknown as { ResizeObserver: unknown }).ResizeObserver =
  (globalThis as unknown as { ResizeObserver?: unknown }).ResizeObserver ?? ResizeObserverStub;

// Radix Select needs pointer-capture + scroll APIs jsdom does not implement.
const proto = Element.prototype as Element & {
  hasPointerCapture?: (id: number) => boolean;
  setPointerCapture?: (id: number) => void;
  releasePointerCapture?: (id: number) => void;
  scrollIntoView?: (arg?: unknown) => void;
};
proto.hasPointerCapture ??= () => false;
proto.setPointerCapture ??= () => {};
proto.releasePointerCapture ??= () => {};
proto.scrollIntoView ??= () => {};

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

function makeGroup(overrides: Partial<ModelGroup> = {}): ModelGroup {
  return {
    id: "g1",
    name: "生产分组",
    billing_source: "balance_only",
    rate_multiplier: 1,
    allowed_user_group_ids: [],
    enabled: true,
    model_ids: [],
    model_links: [],
    sort_order: 0,
    ...overrides,
  };
}

function makeLink(overrides: Partial<GroupAccount> = {}): GroupAccount {
  return {
    binding_id: "b1",
    group_id: "g1",
    account_id: "a1",
    account_name: "主账号",
    priority: 100,
    weight: 100,
    enabled: true,
    status: "active",
    health: "healthy",
    schedulable: true,
    has_credential: true,
    cooldown_active: false,
    schedulable_now: true,
    ...overrides,
  };
}

function makeModel(overrides: Partial<AccountModel> = {}): AccountModel {
  return {
    id: "am1",
    account_id: "a1",
    model_name: "deepseek-v4",
    enabled: true,
    cost_mode: "none",
    currency: "USD",
    ...overrides,
  };
}

function renderGroups(opts: {
  groups?: ModelGroup[];
  accounts?: Account[];
  bound?: GroupAccount[];
  accountModels?: Record<string, AccountModel[]>;
  calls: Call[];
}) {
  const { groups = [makeGroup()], accounts = [], bound = [], accountModels = {}, calls } = opts;
  http.defaults.adapter = async (config) => {
    const method = (config.method ?? "get").toLowerCase();
    const url = config.url ?? "";
    const body = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
    calls.push({ method, url, body, params: config.params });
    const reply = (data: unknown) => ({ data, status: 200, statusText: "OK", headers: {}, config });
    if (method === "get" && url === "/v1/admin/model-groups") return reply({ groups });
    if (method === "get" && url === "/v1/admin/models") return reply({ models: [] as Model[] });
    if (method === "get" && url === "/v1/admin/accounts") return reply({ accounts, limit: 50, offset: 0 });
    if (method === "get" && url === "/v1/admin/account-pools") return reply({ pools: [] });
    if (method === "get" && url === "/v1/admin/user-groups") return reply({ user_groups: [] });
    if (method === "get" && url === "/v1/admin/model-groups/g1/accounts") return reply({ accounts: bound });
    const am = url.match(/^\/v1\/admin\/accounts\/([^/]+)\/models$/);
    if (method === "get" && am) return reply({ models: accountModels[am[1]] ?? [] });
    if (method === "patch" && url === "/v1/admin/model-groups/g1") return reply({ group: makeGroup() });
    if (method === "post" && url === "/v1/admin/model-groups/g1/accounts") return reply({ account: makeLink() });
    if (method === "delete" && url.startsWith("/v1/admin/model-groups/g1/accounts/")) return reply({ ok: true });
    throw new Error(`unexpected request: ${method} ${url}`);
  };

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });

  return render(
    <Theme>
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <Groups />
        </MemoryRouter>
      </QueryClientProvider>
    </Theme>,
  );
}

async function openManage(user: ReturnType<typeof userEvent.setup>) {
  await screen.findByText("生产分组");
  await user.click(screen.getByRole("button", { name: "管理分组" }));
  await screen.findByText(/还没有成员账号|设置白名单/);
}

describe("group member management", () => {
  it("shows an empty member list instead of every account in the pool", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderGroups({
      accounts: [makeAccount(), makeAccount({ id: "a2", name: "备用账号" })],
      calls,
    });
    await openManage(user);

    expect(await screen.findByText(/还没有成员账号/)).toBeTruthy();
    // No member rows: 移除 only exists on a bound member.
    expect(screen.queryByRole("button", { name: "移除" })).toBeNull();
    // The pool of addable accounts is a searchable checkbox pick-list.
    expect(screen.getByPlaceholderText(/搜索未绑定账号/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /添加选中.*账号/ })).toBeTruthy();
  });

  it("binds an explicitly chosen account", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderGroups({ accounts: [makeAccount({ id: "a2", name: "备用账号" })], calls });
    await openManage(user);

    await user.click(await screen.findByRole("checkbox", { name: /备用账号/ }));
    await user.click(screen.getByRole("button", { name: /添加选中.*账号/ }));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "post" && c.url === "/v1/admin/model-groups/g1/accounts");
      expect(post).toBeTruthy();
      expect(post?.body).toEqual({ account_id: "a2" });
    });
  });

  it("defaults a member to all configured models and can narrow to a whitelist", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderGroups({
      accounts: [makeAccount()],
      bound: [makeLink()],
      accountModels: {
        a1: [makeModel({ model_name: "deepseek-v4" }), makeModel({ id: "am2", model_name: "glm-5.2" })],
      },
      calls,
    });
    await screen.findByText("生产分组");
    await user.click(screen.getByRole("button", { name: "管理分组" }));

    // The member row renders with the all-models default badge.
    expect(await screen.findByText("全部 2 个模型")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "设置白名单" }));
    await user.click(await screen.findByRole("checkbox", { name: /deepseek-v4/ }));
    await user.click(screen.getByRole("button", { name: /保存白名单/ }));

    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === "post" && c.url === "/v1/admin/model-groups/g1/accounts"
          && (c.body as { model_names?: string[] })?.model_names !== undefined,
      );
      expect(post).toBeTruthy();
      expect((post?.body as { model_names: string[] }).model_names).toEqual(["glm-5.2"]);
    });
  });

  it("clears a whitelist back to all models", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderGroups({
      accounts: [makeAccount()],
      bound: [makeLink({ model_names: ["deepseek-v4"] })],
      accountModels: {
        a1: [makeModel({ model_name: "deepseek-v4" }), makeModel({ id: "am2", model_name: "glm-5.2" })],
      },
      calls,
    });
    await screen.findByText("生产分组");
    await user.click(screen.getByRole("button", { name: "管理分组" }));

    expect(await screen.findByText("模型白名单 1 个")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "编辑白名单" }));
    await user.click(await screen.findByRole("button", { name: "恢复全部" }));

    await waitFor(() => {
      const post = calls.find(
        (c) => c.method === "post" && c.url === "/v1/admin/model-groups/g1/accounts"
          && (c.body as { model_names?: string[] })?.model_names !== undefined,
      );
      expect(post).toBeTruthy();
      expect((post?.body as { model_names: string[] }).model_names).toEqual([]);
    });
  });
});

describe("group intro profile", () => {
  it("saves only filled dims in the basic-info patch", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderGroups({
      groups: [makeGroup({ profile: [{ name: "速度", value: 80 }] })],
      accounts: [makeAccount()],
      calls,
    });
    await openManage(user);

    // Existing dim is prefilled; add a second one, leave the rest blank.
    expect((await screen.findByPlaceholderText("维度 1 名称") as HTMLInputElement).value).toBe("速度");
    await user.type(screen.getByPlaceholderText("维度 2 名称"), "稳定性");
    await user.type(screen.getAllByPlaceholderText("0–100")[1], "90");
    await user.click(screen.getByRole("button", { name: "保存基础信息" }));

    await waitFor(() => {
      const patch = calls.find((c) => c.method === "patch" && c.url === "/v1/admin/model-groups/g1");
      expect(patch).toBeTruthy();
      expect((patch?.body as { profile: { name: string; value: number }[] }).profile)
        .toEqual([{ name: "速度", value: 80 }, { name: "稳定性", value: 90 }]);
    });
  });

  it("blocks save when a named dim has no score", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderGroups({ accounts: [makeAccount()], calls });
    await openManage(user);

    await user.type(screen.getByPlaceholderText("维度 1 名称"), "速度");
    expect(await screen.findByText("填了名称的维度需要补数值。")).toBeTruthy();
    expect(screen.getByRole("button", { name: "保存基础信息" })).toHaveProperty("disabled", true);
    expect(calls.find((c) => c.method === "patch")).toBeUndefined();
  });
});
