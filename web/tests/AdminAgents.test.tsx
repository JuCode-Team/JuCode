/* @vitest-environment jsdom */
import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it } from "vitest";
import { http } from "../src/api/client";
import AdminAgents from "../src/pages/admin/AdminAgents";

globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};
Element.prototype.scrollIntoView ??= () => {};
const originalAdapter = http.defaults.adapter;
afterEach(() => { cleanup(); http.defaults.adapter = originalAdapter; });

// Stored share 70 (budget 30) under a cap of 20: the row shows the capped
// budget 20 = discount 20 + rebate 0.
function setup() {
  const patches: unknown[] = [];
  http.defaults.adapter = async (config) => {
    let data: unknown = {};
    if (config.method === "patch") {
      patches.push(JSON.parse(config.data));
    } else if (config.url === "/v1/admin/agents") {
      data = { agents: [{ agent_id: "a1", email: "agent@x.test", platform_share_pct: "70.00", status: "active",
        coupon_code: "C-AAAA", coupon_discount_pct: "20", coupon_rebate_rate_pct: "0" }] };
    } else if (config.url === "/v1/admin/settings") {
      data = { max_incentive_pct: 20 };
    }
    return { data, status: 200, statusText: "OK", headers: {}, config };
  };
  render(<Theme><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><AdminAgents /></QueryClientProvider></Theme>);
  return patches;
}

it("sends only the fields the admin changed, never the capped budget", async () => {
  const patches = setup();
  await screen.findByText("agent@x.test");
  await userEvent.click(screen.getByRole("button", { name: "配置" }));
  const dialog = within(await screen.findByRole("dialog"));
  const discount = dialog.getAllByRole("spinbutton")[1]; // [让利额度, 客户折扣]
  await userEvent.clear(discount);
  await userEvent.type(discount, "10");
  await userEvent.click(dialog.getByRole("button", { name: "保存" }));
  await waitFor(() => expect(patches).toEqual([{ coupon_discount_pct: "10" }]));
});

it("blocks a discount larger than the budget", async () => {
  setup();
  await screen.findByText("agent@x.test");
  await userEvent.click(screen.getByRole("button", { name: "配置" }));
  const dialog = within(await screen.findByRole("dialog"));
  const discount = dialog.getAllByRole("spinbutton")[1]; // [让利额度, 客户折扣]
  await userEvent.clear(discount);
  await userEvent.type(discount, "25");
  expect((dialog.getByRole("button", { name: "保存" }) as HTMLButtonElement).disabled).toBe(true);
});
