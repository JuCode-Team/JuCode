/* @vitest-environment jsdom */
import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it } from "vitest";
import { http } from "../src/api/client";
import AccessTokens from "../src/pages/user/AccessTokens";

globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};
Element.prototype.scrollIntoView ??= () => {};
const originalAdapter = http.defaults.adapter;
afterEach(() => { cleanup(); http.defaults.adapter = originalAdapter; });

it("creates a token, shows it once, and revokes by id", async () => {
  const calls: { method?: string; url?: string; body?: unknown }[] = [];
  http.defaults.adapter = async (config) => {
    calls.push({ method: config.method, url: config.url, body: config.data ? JSON.parse(config.data) : undefined });
    let data: unknown = { tokens: [{ id: "t1", name: "旧令牌", prefix: "jcp-abcd1234", created_at: "2026-09-01T00:00:00Z", expires_at: null, last_used_at: null }] };
    if (config.method === "post") data = { id: "t2", name: "工单智能体", prefix: "jcp-zzzz0000", created_at: "2026-09-28T00:00:00Z", token: "jcp-zzzz0000SECRET" };
    if (config.method === "delete") data = { revoked: true };
    return { data, status: 200, statusText: "OK", headers: {}, config };
  };
  render(<Theme><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><AccessTokens /></QueryClientProvider></Theme>);

  await screen.findByText("旧令牌");
  await userEvent.click(screen.getByRole("button", { name: "创建令牌" }));
  await userEvent.type(screen.getByPlaceholderText(/名称/), "工单智能体");
  await userEvent.click(screen.getByRole("button", { name: "创建" }));
  expect(await screen.findByText("jcp-zzzz0000SECRET")).toBeTruthy();
  expect(calls.find((c) => c.method === "post")).toEqual({ method: "post", url: "/v1/me/access-tokens", body: { name: "工单智能体", expires_in_days: 90 } });
  await userEvent.click(screen.getByRole("button", { name: "完成" }));
  await waitFor(() => expect(screen.queryByText("jcp-zzzz0000SECRET")).toBeNull());

  await userEvent.click(screen.getByRole("button", { name: "撤销" }));
  await userEvent.click(await screen.findByRole("button", { name: "撤销" }));
  await waitFor(() => expect(calls.some((c) => c.method === "delete" && c.url === "/v1/me/access-tokens/t1")).toBe(true));
});
