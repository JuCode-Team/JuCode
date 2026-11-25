/* @vitest-environment jsdom */

import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it } from "vitest";
import { http } from "../src/api/client";
import PublicModels from "../src/pages/PublicModels";

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

const GROUP_FAST = {
  id: "g-fast",
  name: "极速通道",
  billing_source: "balance_only",
  rate_multiplier: 1,
  description: "低延迟专线，面向实时交互场景。",
};

const GROUP_ECON = {
  id: "g-econ",
  name: "经济分组",
  billing_source: "balance_only",
  rate_multiplier: 0.8,
};

const GROUP_PLAN = {
  id: "g-plan",
  name: "套餐专属",
  billing_source: "plan_only",
  rate_multiplier: 0.1,
};

const MODELS = [
  {
    name: "model-x",
    billing_mode: "per_token",
    currency: "CNY",
    description: "测试模型",
    input_unit_price: "1",
    output_unit_price: "2",
    vendor: { id: "v1", name: "测试厂商", slug: "acme" },
    groups: [GROUP_FAST, GROUP_ECON, GROUP_PLAN],
  },
];

function renderPage(models = MODELS) {
  http.defaults.adapter = async (config) => {
    const url = config.url ?? "";
    const reply = (data: unknown) => ({ data, status: 200, statusText: "OK", headers: {}, config });
    if (url === "/v1/public/models") return reply({ models });
    if (url === "/v1/public/vendors") return reply({ vendors: [] });
    if (url === "/v1/public/status") return reply({ groups: [], window_hours: 24 });
    throw new Error(`unexpected request: ${url}`);
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <Theme>
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <PublicModels />
        </MemoryRouter>
      </QueryClientProvider>
    </Theme>,
  );
}

function card() {
  const el = document.querySelector("article.cs-card");
  if (!el) throw new Error("no card rendered");
  return el as HTMLElement;
}

function outputRow() {
  const row = [...card().querySelectorAll(".cs-price-row")].find(
    (r) => r.querySelector(".cs-price-label")?.textContent === "输出",
  );
  if (!row) throw new Error("no output row");
  return row;
}

describe("model showcase", () => {
  it("quotes each card from its cheapest balance group and ignores plan-only groups", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "model-x" });

    // 0.8× beats 1×; the 0.1× plan-only group is not purchasable with balance.
    expect(card().querySelector(".cs-group-link")?.textContent).toBe("经济分组0.8×");
    expect(screen.queryByText("套餐专属")).toBeNull();

    // Output: 2/1K → 2000/1M official, ×0.8 on site.
    expect(outputRow().querySelector(".cs-price-official")?.textContent).toBe("¥2000");
    expect(outputRow().querySelector(".cs-price-site")?.textContent).toBe("1600");
  });

  it("keeps Han text of the saving badge out of the mono figure", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "model-x" });

    const badge = card().querySelector(".cs-badge");
    expect(badge?.textContent).toBe("省 20%");
    expect(badge?.querySelector("b")?.textContent).toBe("20%");
  });

  it("switches the whole section to the picked group and back", async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole("heading", { name: "model-x" });

    const groups = screen.getByRole("group", { name: "测试厂商 分组" });
    await user.click(within(groups).getByRole("button", { name: /极速通道/ }));

    expect(card().querySelector(".cs-group-link")?.textContent).toBe("极速通道1×");
    expect(card().querySelector(".cs-badge")?.textContent).toBe("官方同价");
    expect(outputRow().querySelector(".cs-price-site")?.textContent).toBe("2000");

    // Clicking the active group again returns to the per-model lowest price.
    await user.click(within(groups).getByRole("button", { name: /极速通道/ }));
    expect(card().querySelector(".cs-group-link")?.textContent).toBe("经济分组0.8×");
  });

  it("rescales prices when the unit toggles to per 1K", async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByRole("heading", { name: "model-x" });

    await user.click(screen.getByRole("button", { name: "每 1K" }));
    expect(outputRow().querySelector(".cs-price-official")?.textContent).toBe("¥2");
    expect(outputRow().querySelector(".cs-price-site")?.textContent).toBe("1.6");
  });
});
