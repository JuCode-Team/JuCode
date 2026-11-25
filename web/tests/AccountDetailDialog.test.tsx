/* @vitest-environment jsdom */

import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { http } from "../src/api/client";
import type { Account } from "../src/api/endpoints";
import { AccountDetailDialog } from "../src/components/accounts/AccountDetailDialog";

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
    name: "Anthropic",
    base_url: "https://api.anthropic.com",
    native_protocols: ["messages"],
    allow_conversion: false,
    endpoint_paths: { chat_completions: "/v2/chat/completions" },
    status: "active",
    health: "healthy",
    priority: 100,
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

function renderDialog(account: Account, calls: Call[]) {
  http.defaults.adapter = async (config) => {
    const method = (config.method ?? "get").toLowerCase();
    const url = config.url ?? "";
    const body = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
    calls.push({ method, url, body });
    const reply = (data: unknown) => ({ data, status: 200, statusText: "OK", headers: {}, config });
    if (method === "get" && url === "/v1/admin/accounts/a1/models") return reply({ models: [] });
    if (method === "patch" && url === "/v1/admin/accounts/a1") return reply(makeAccount());
    throw new Error(`unexpected request: ${method} ${url}`);
  };

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });

  return render(
    <Theme>
      <QueryClientProvider client={queryClient}>
        <AccountDetailDialog account={account} open onOpenChange={() => {}} />
      </QueryClientProvider>
    </Theme>,
  );
}

// The dialog is tabbed now: fields live under 端点与协议 / 凭据 / 调度,
// and Radix unmounts inactive tabs, so a test has to navigate before it can
// see the control it asserts on.
async function gotoTab(user: ReturnType<typeof userEvent.setup>, name: string) {
  // Radix Themes renders the trigger label twice (visible + hidden sizer), so
  // the accessible name may be the label duplicated — match by substring.
  await user.click(await screen.findByRole("tab", { name: new RegExp(name) }));
}

describe("account detail dialog", () => {
  it("declares the account's native protocols and lets edit mode change them", async () => {
    const user = userEvent.setup();
    renderDialog(makeAccount({ native_protocols: ["chat_completions", "messages"] }), []);
    await gotoTab(user, "端点与协议");
    // Both protocols render as declaration badges (and the multi-select mirrors
    // them).
    // The labels also title the path-override fields, so match any occurrence.
    expect((await screen.findAllByText("Chat 补全")).length).toBeGreaterThan(0);
    expect(screen.getAllByText("Messages").length).toBeGreaterThan(0);
    const messages = screen.getByRole("checkbox", { name: "Messages" }) as HTMLInputElement;
    const responses = screen.getByRole("checkbox", { name: "Responses" }) as HTMLInputElement;
    expect(messages.checked).toBe(true);
    expect(responses.checked).toBe(false);
    // Endpoint & dialect are patchable on an existing account: the declaration
    // is editable here.
    expect(messages.disabled).toBe(false);
    expect(responses.disabled).toBe(false);
    // The old "fixed at creation" note for the endpoint is gone.
    expect(screen.queryByText(/端点地址与协议在创建时固定/)).toBeNull();
  });

  it("unlocks the endpoint, dialect and request-shaping controls in edit mode", async () => {
    const user = userEvent.setup();
    renderDialog(makeAccount(), []);

    await gotoTab(user, "端点与协议");
    // base_url is editable, not read-only.
    const baseUrl = (await screen.findByDisplayValue("https://api.anthropic.com")) as HTMLInputElement;
    expect(baseUrl.readOnly).toBe(false);

    // The endpoint tab carries the conversion switch plus the Messages-dialect
    // shaping toggle for a messages-only account — all unlocked.
    const endpointSwitches = screen.getAllByRole("switch") as HTMLButtonElement[];
    expect(endpointSwitches.length).toBeGreaterThanOrEqual(2);
    expect(endpointSwitches.some((s) => s.disabled)).toBe(false);

    // Scheduling carries its own enable switch.
    await gotoTab(user, "调度");
    const scheduleSwitches = screen.getAllByRole("switch") as HTMLButtonElement[];
    expect(scheduleSwitches.length).toBeGreaterThanOrEqual(1);
    expect(scheduleSwitches.some((s) => s.disabled)).toBe(false);

    // The credential tab rotates the API key.
    await gotoTab(user, "凭据");
    expect(await screen.findByPlaceholderText("留空保持当前")).toBeTruthy();
  });

  it("carries the endpoint, dialect and shaping fields in the edit PATCH", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderDialog(makeAccount(), calls);

    await gotoTab(user, "端点与协议");
    const baseUrl = (await screen.findByDisplayValue("https://api.anthropic.com")) as HTMLInputElement;
    await user.clear(baseUrl);
    await user.type(baseUrl, "https://api.example.com/v1");

    // Add Responses to the native set (the PATCH should carry the new list).
    await user.click(screen.getByRole("checkbox", { name: "Responses" }));

    await user.type(screen.getByPlaceholderText("org-…"), "org-123");
    await user.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => {
      const patch = calls.find((c) => c.method === "patch" && c.url === "/v1/admin/accounts/a1");
      expect(patch).toBeTruthy();
      // Only the fields changed in the dialog go out, so a stale form cannot
      // revert what someone else saved meanwhile.
      expect(patch?.body).toEqual({
        base_url: "https://api.example.com/v1",
        organization: "org-123",
        native_protocols: ["messages", "responses"],
      });
    });
  });

  it("carries only the declared upstream path overrides in the PATCH", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderDialog(makeAccount(), calls);

    await gotoTab(user, "端点与协议");
    const placeholders = await screen.findAllByPlaceholderText(/^\/v1\//);
    expect(placeholders).toHaveLength(3);
    // The chat override exists on the account: it is prefilled, not empty.
    const chat = screen.getByDisplayValue("/v2/chat/completions") as HTMLInputElement;
    await user.clear(chat);
    await user.type(chat, "/v3/chat/completions");

    await user.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => {
      const patch = calls.find((c) => c.method === "patch" && c.url === "/v1/admin/accounts/a1");
      expect(patch).toBeTruthy();
      // Whole-map replacement, and the protocols without an override are not
      // sent at all (which is what clears them server-side).
      expect((patch?.body as Record<string, unknown>).endpoint_paths).toEqual({
        chat_completions: "/v3/chat/completions",
      });
    });
  });

  it("rotates the api key inline", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderDialog(makeAccount(), calls);

    await gotoTab(user, "凭据");
    await user.type(await screen.findByPlaceholderText("留空保持当前"), "sk-new-key");
    await user.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => {
      const patch = calls.find((c) => c.method === "patch" && c.url === "/v1/admin/accounts/a1");
      expect(patch).toBeTruthy();
      // Nothing else changed, so the key is the whole PATCH.
      expect(patch?.body).toEqual({ api_key: "sk-new-key" });
    });
  });
});
