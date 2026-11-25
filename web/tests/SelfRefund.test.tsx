/* @vitest-environment jsdom */
import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, expect, it } from "vitest";
import { http } from "../src/api/client";
import { SelfRefundDialog } from "../src/components/SelfRefund";

globalThis.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
Element.prototype.hasPointerCapture ??= () => false;
Element.prototype.setPointerCapture ??= () => {};
Element.prototype.releasePointerCapture ??= () => {};
Element.prototype.scrollIntoView ??= () => {};
const originalAdapter = http.defaults.adapter;
afterEach(() => { cleanup(); http.defaults.adapter = originalAdapter; });

const plan = {
  enabled: true,
  refundable: "30",
  orders: null,
  cash: "0",
  unsupported: "30",
  plans: [
    { order_id: "p1", order_no: "SO1", plan_name: "Lite", channel: "alipay", cash: "20", remaining_days: 20, total_days: 30, auto: true },
    { order_id: "p2", order_no: "SO2", plan_name: "Pro", channel: "yisufu", cash: "40", remaining_days: 20, total_days: 30, auto: false },
  ],
};

function Harness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button onClick={() => setOpen(true)}>打开退款</button>
      <SelfRefundDialog open={open} onClose={() => setOpen(false)} />
    </>
  );
}

it("refunds what the gateway can return and starts over when reopened", async () => {
  const calls: string[] = [];
  http.defaults.adapter = async (config) => {
    calls.push(`${config.method} ${config.url}`);
    const data = config.method === "post"
      ? { refunded: null, refunded_plans: [plan.plans[0]], unsupported: "30" }
      : plan;
    return { data, status: 200, statusText: "OK", headers: {}, config };
  };
  render(<Theme><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><Harness /></QueryClientProvider></Theme>);

  await userEvent.click(screen.getByRole("button", { name: "打开退款" }));
  // Only the plan paid through a gateway goes back automatically; the rest
  // (unsupported balance + the yisufu plan) is left for support.
  expect(await screen.findByText("原路退回")).toBeTruthy();
  expect(screen.getByText(/20\.00/, { selector: "dd" })).toBeTruthy();
  expect(screen.getByText(/70\.00/, { selector: "dd" })).toBeTruthy();

  await userEvent.click(screen.getByRole("button", { name: "申请退款" }));
  await userEvent.click(await screen.findByRole("button", { name: "确认退款" }));
  expect(await screen.findByText(/Lite.*已按剩余 20 天退款/)).toBeTruthy();
  expect(screen.getByText(/未能自动退回/)).toBeTruthy();
  expect(calls).toContain("post /v1/me/self-refund");

  await userEvent.keyboard("{Escape}");
  await waitFor(() => expect(screen.queryByText(/已按剩余 20 天退款/)).toBeNull());
  await userEvent.click(screen.getByRole("button", { name: "打开退款" }));
  expect(await screen.findByRole("button", { name: "申请退款" })).toBeTruthy();
  expect(screen.queryByText(/已按剩余 20 天退款/)).toBeNull();
});
