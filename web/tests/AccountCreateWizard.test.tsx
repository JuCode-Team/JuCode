/* @vitest-environment jsdom */

import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it } from "vitest";
import { http } from "../src/api/client";
import type { Account, ProviderCatalogDetail } from "../src/api/endpoints";
import { AccountCreateWizard } from "../src/components/accounts/AccountCreateWizard";

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

const deepseekEntry = {
  id: "deepseek",
  name: "DeepSeek",
  login_kind: "api-key",
  api_key_format: "bearer",
  base_url: "https://api.deepseek.com/anthropic",
  protocol: "chat",
  default_model: "deepseek-v4-pro",
  model_count: 2,
  usable: true,
};

const anthropicEntry = {
  id: "anthropic",
  name: "Anthropic (Claude Pro/Max)",
  login_kind: "oauth-code",
  api_key_format: "bearer",
  base_url: "https://api.anthropic.com",
  protocol: "anthropic",
  default_model: "claude-sonnet-4-5",
  model_count: 1,
  usable: true,
};

function deepseekDetail(): ProviderCatalogDetail {
  return {
    catalog_version: "18.1.21",
    provider: deepseekEntry,
    models: [
      { id: "deepseek-v4-pro", name: "DeepSeek V4 Pro", api: "openai-completions", protocol: "chat", supported: true, context_window: 1_000_000, max_tokens: 384_000, reasoning: false },
      { id: "deepseek-v4-chat", name: "DeepSeek V4 Chat", api: "openai-completions", protocol: "chat", supported: true, context_window: 128_000, max_tokens: 32_000, reasoning: false },
      { id: "deepseek-embed", name: "DeepSeek Embed", api: "google-generative-ai", supported: false, context_window: 8_000, max_tokens: 0, reasoning: false },
    ],
  };
}

function createdAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: "acc1",
    name: "DeepSeek",
    base_url: "https://api.deepseek.com/anthropic",
    native_protocols: ["chat_completions"],
    allow_conversion: false,
    status: "active",
    health: "unknown",
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

function renderWizard(calls: Call[]) {
  http.defaults.adapter = async (config) => {
    const method = (config.method ?? "get").toLowerCase();
    const url = config.url ?? "";
    const body = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
    calls.push({ method, url, body });
    const reply = (data: unknown, status = 200) => ({ data, status, statusText: "OK", headers: {}, config });

    if (method === "get" && url === "/v1/admin/provider-definitions") {
      // Picking a definition skips the endpoint step and preselects the
      // models it ships.
      return reply({
        definitions: [
          {
            definition_id: "moonshot",
            name: "Moonshot（仅凭据）",
            vendor: "moonshot",
            base_url: "https://api.moonshot.cn/v1",
            api_format: "openai",
            native_protocols: ["chat_completions"],
            default_protocol: "chat_completions",
            enabled: true,
            builtin: true,
            source: "builtin",
            version: 1,
            credential_label: "API Key",
            credential_hint: "一个账号就是一份凭据。",
            models: [{ name: "kimi-k2", protocol: "chat_completions" }],
          },
        ],
      });
    }
    if (method === "get" && url === "/v1/admin/provider-catalog") {
      return reply({ catalog_version: "18.1.21", providers: [anthropicEntry, deepseekEntry] });
    }
    if (method === "get" && url === "/v1/admin/provider-catalog/deepseek") return reply(deepseekDetail());
    if (method === "post" && url === "/v1/admin/accounts") return reply(createdAccount(), 201);
    if (method === "post" && url === "/v1/admin/accounts/acc1/models") {
      return reply({ model: { id: "m1", account_id: "acc1", model_name: body?.model_name, enabled: true, cost_mode: "none", currency: "CNY" } }, 201);
    }
    throw new Error(`unexpected request: ${method} ${url}`);
  };

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });

  return render(
    <Theme>
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <AccountCreateWizard open onOpenChange={() => {}} />
        </MemoryRouter>
      </QueryClientProvider>
    </Theme>,
  );
}

describe("account create wizard", () => {
  it("walks template → endpoint → api key → models → create", async () => {
    const calls: Call[] = [];
    const user = userEvent.setup();
    renderWizard(calls);

    // Step 0: pick a template.
    // Definitions exist, so step 0 opens on that tab; templates sit under「目录模板」.
    await user.click(await screen.findByRole("tab", { name: /目录模板/ }));
    await user.click(await screen.findByText("DeepSeek"));
    await user.click(screen.getByRole("button", { name: "下一步" }));

    // Step 1: the template prefilled the endpoint.
    expect(await screen.findByDisplayValue("https://api.deepseek.com/anthropic")).toBeTruthy();
    expect(screen.getByDisplayValue("DeepSeek")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "下一步" }));

    // Step 2: API-key login.
    await user.type(await screen.findByPlaceholderText("sk-..."), "sk-deepseek");
    await user.click(screen.getByRole("button", { name: "下一步" }));

    // Step 3: create + bind the default model.
    await user.click(await screen.findByRole("button", { name: "创建账号并完成" }));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "post" && c.url === "/v1/admin/accounts");
      expect(post).toBeTruthy();
      expect(post?.body).toMatchObject({
        name: "DeepSeek",
        base_url: "https://api.deepseek.com/anthropic",
        native_protocols: ["chat_completions"],
        api_key: "sk-deepseek",
      });
    });
    await waitFor(() => {
      const modelPost = calls.find((c) => c.method === "post" && c.url === "/v1/admin/accounts/acc1/models");
      expect(modelPost?.body).toMatchObject({ model_name: "deepseek-v4-pro", cost_mode: "none" });
    });
    expect(await screen.findByText(/已就绪/)).toBeTruthy();
  });

  it("offers only API-key catalog templates", async () => {
    const user = userEvent.setup();
    renderWizard([]);

    await user.click(await screen.findByRole("tab", { name: /目录模板/ }));
    expect(await screen.findByText("DeepSeek")).toBeTruthy();
    // Subscription logins (OAuth) are not an account source.
    expect(screen.queryByText("Anthropic (Claude Pro/Max)")).toBeNull();
    expect(screen.queryByText(/OAuth/)).toBeNull();
  });

  it("creates from a provider definition with no endpoint questions asked", async () => {
    const user = userEvent.setup();
    const calls: Call[] = [];
    renderWizard(calls);

    // Step 0 offers the definition first; picking it jumps straight to the
    // credential step because the endpoint, protocol and models are fixed.
    await user.click(await screen.findByText("Moonshot（仅凭据）"));

    // The credential step names what the vendor expects; there is no
    // login-mode picker.
    const tokenField = await screen.findByPlaceholderText("粘贴令牌");
    expect(screen.getByText("API Key")).toBeTruthy();
    expect(screen.getByText("一个账号就是一份凭据。")).toBeTruthy();
    expect(screen.queryByText(/OAuth/)).toBeNull();
    await user.type(tokenField, "wb-token-123");
    await user.click(screen.getByRole("button", { name: "下一步" }));

    // Models come preselected from the definition, and the create body carries
    // the definition id instead of a hand-written endpoint.
    expect(await screen.findByLabelText("kimi-k2")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: /创建账号并完成/ }));

    await waitFor(() => {
      const create = calls.find((c) => c.method === "post" && c.url === "/v1/admin/accounts");
      expect(create?.body).toMatchObject({
        definition_id: "moonshot",
        api_key: "wb-token-123",
        base_url: "https://api.moonshot.cn/v1",
        native_protocols: ["chat_completions"],
      });
    });
  });
});