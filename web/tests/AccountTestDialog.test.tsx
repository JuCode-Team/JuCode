/* @vitest-environment jsdom */

import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { http } from "../src/api/client";
import type { Account } from "../src/api/endpoints";
import { AccountTestDialog } from "../src/components/accounts/AccountTestDialog";

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

type Call = { method: string; url: string; body?: unknown };

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: "a1",
    name: "主账号",
    base_url: "https://api.deepseek.com",
    native_protocols: ["chat_completions", "messages"],
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

function renderDialog(account: Account, testReply: unknown, calls: Call[]) {
  http.defaults.adapter = async (config) => {
    const method = (config.method ?? "get").toLowerCase();
    const url = config.url ?? "";
    const body = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
    calls.push({ method, url, body });
    const reply = (data: unknown) => ({ data, status: 200, statusText: "OK", headers: {}, config });
    if (method === "get" && url === "/v1/admin/accounts/a1/models") {
      return reply({
        models: [
          { id: "m1", account_id: "a1", model_name: "deepseek-chat", upstream_model: "", enabled: true, cost_mode: "none", currency: "CNY" },
        ],
      });
    }
    if (method === "post" && url === "/v1/admin/accounts/a1/test") return reply(testReply);
    throw new Error(`unexpected request: ${method} ${url}`);
  };

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });

  return render(
    <Theme>
      <QueryClientProvider client={queryClient}>
        <AccountTestDialog account={account} open onOpenChange={() => {}} />
      </QueryClientProvider>
    </Theme>,
  );
}

describe("account test dialog", () => {
  it("sends the chosen protocol and bound model to the test endpoint", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderDialog(makeAccount(), { ok: true, status: 200, latency_ms: 312, model: "deepseek-chat", protocol: "chat_completions", url: "https://api.deepseek.com/v1/chat/completions" }, calls);

    // The model input prefills from the account's enabled bindings.
    await waitFor(() => {
      expect((screen.getByPlaceholderText(/选择已绑定模型/) as HTMLInputElement).value).toBe("deepseek-chat");
    });
    await user.click(screen.getByRole("button", { name: /发起测试/ }));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "post" && c.url === "/v1/admin/accounts/a1/test");
      expect(post?.body).toEqual({ protocol: "chat_completions", model: "deepseek-chat" });
    });
    expect(await screen.findByText("正常")).toBeTruthy();
    expect(screen.getByText("HTTP 200")).toBeTruthy();
    expect(screen.getByText(/312 ms/)).toBeTruthy();
  });

  it("lets the operator type an unbound model name", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderDialog(makeAccount(), { ok: false, status: 404, latency_ms: 88, model: "nope", protocol: "chat_completions", url: "u", detail: "model not found" }, calls);

    const input = screen.getByPlaceholderText(/选择已绑定模型/) as HTMLInputElement;
    await waitFor(() => expect(input.value).toBe("deepseek-chat"));
    await user.clear(input);
    await user.type(input, "nope");
    await user.click(screen.getByRole("button", { name: /发起测试/ }));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "post");
      expect(post?.body).toEqual({ protocol: "chat_completions", model: "nope" });
    });
    expect(await screen.findByText("异常")).toBeTruthy();
    expect(screen.getByText("model not found")).toBeTruthy();
  });

  it("shows a transport-level error the same way as an upstream rejection", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderDialog(makeAccount(), { ok: false, status: 0, latency_ms: 5001, model: "m", protocol: "messages", url: "u", error: "context deadline exceeded" }, calls);

    const input = screen.getByPlaceholderText(/选择已绑定模型/) as HTMLInputElement;
    await waitFor(() => expect(input.value).toBe("deepseek-chat"));
    await user.click(screen.getByRole("button", { name: /发起测试/ }));

    expect(await screen.findByText("异常")).toBeTruthy();
    expect(screen.getByText("context deadline exceeded")).toBeTruthy();
  });
});
