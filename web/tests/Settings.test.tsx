/* @vitest-environment jsdom */

import { Theme } from "@radix-ui/themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { http } from "../src/api/client";
import type { MarketingBannerConfig, SiteSettings } from "../src/api/endpoints";
import Settings from "../src/pages/admin/Settings";

const { refreshSite } = vi.hoisted(() => ({ refreshSite: vi.fn() }));

vi.mock("../src/store/site", () => ({
  useSiteStore: (
    selector: (state: { refresh: typeof refreshSite }) => unknown,
  ) => selector({ refresh: refreshSite }),
}));

const originalAdapter = http.defaults.adapter;

afterEach(() => {
  cleanup();
  http.defaults.adapter = originalAdapter;
  refreshSite.mockReset();
});

describe("marketing banner settings", () => {
  it("sends edited banner values through the nested PUT contract", async () => {
    let persisted = {
      site_name: "JuCode",
      site_description: "AI 编程平台",
      site_url: "https://example.com",
      site_logo: "",
      icp_number: "",
      contact_email: "support@example.com",
      contact_group_label: "QQ 群",
      contact_group: "",
      contact_group_qr_url: "",
      contact_dock_enabled: false,
      marketing_banner: {
        enabled: false,
        text: "",
        link_url: "",
      },
    } as SiteSettings;
    let putBody: Record<string, unknown> | undefined;

    http.defaults.adapter = async (config) => {
      if (config.method === "get" && config.url === "/v1/admin/settings") {
        return {
          data: persisted,
          status: 200,
          statusText: "OK",
          headers: {},
          config,
        };
      }

      if (config.method === "put" && config.url === "/v1/admin/settings") {
        putBody = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
        const marketingBanner = putBody?.marketing_banner as MarketingBannerConfig;
        persisted = { ...persisted, marketing_banner: marketingBanner };
        return {
          data: persisted,
          status: 200,
          statusText: "OK",
          headers: {},
          config,
        };
      }

      throw new Error(`Unexpected request: ${config.method} ${config.url}`);
    };

    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const user = userEvent.setup();

    render(
      <Theme>
        <MemoryRouter initialEntries={["/admin/settings"]}>
          <QueryClientProvider client={queryClient}>
            <Settings />
          </QueryClientProvider>
        </MemoryRouter>
      </Theme>,
    );

    await screen.findByDisplayValue("JuCode");
    await user.click(screen.getByRole("switch", { name: "启用营销横幅" }));
    await user.type(screen.getByPlaceholderText("请输入需要展示的内容"), "后台配置的横幅文案");
    await user.type(screen.getByPlaceholderText("https://example.com"), "https://example.com/from-admin");
    await user.click(screen.getByRole("button", { name: "保存全部" }));

    await waitFor(() => {
      expect(putBody).toBeDefined();
    });
    expect(putBody?.marketing_banner).toEqual({
      enabled: true,
      text: "后台配置的横幅文案",
      link_url: "https://example.com/from-admin",
    });
    expect(putBody).not.toHaveProperty("marketing_banner_enabled");
    expect(putBody).not.toHaveProperty("marketing_banner_text");
    expect(putBody).not.toHaveProperty("marketing_banner_link_url");

    await screen.findByText("已保存。");
    expect(screen.getByRole("switch", { name: "启用营销横幅" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByPlaceholderText("请输入需要展示的内容")).toHaveProperty(
      "value",
      "后台配置的横幅文案",
    );
    expect(screen.getByPlaceholderText("https://example.com")).toHaveProperty(
      "value",
      "https://example.com/from-admin",
    );
  });
});

describe("contact dock settings", () => {
  it("uploads the QR image and saves the group label, number, QR URL and the dock switch", async () => {
    const persisted = {
      site_name: "JuCode",
      site_description: "",
      site_url: "",
      site_logo: "",
      icp_number: "",
      contact_email: "",
      contact_group_label: "QQ 群",
      contact_group: "",
      contact_group_qr_url: "",
      contact_dock_enabled: false,
      marketing_banner: { enabled: false, text: "", link_url: "" },
    } as SiteSettings;
    let putBody: Record<string, unknown> | undefined;
    let uploaded: unknown;

    http.defaults.adapter = async (config) => {
      const ok = (data: unknown) => ({ data, status: 200, statusText: "OK", headers: {}, config });
      if (config.method === "get" && config.url === "/v1/admin/settings") return ok(persisted);
      if (config.method === "post" && config.url === "/v1/admin/site-assets") {
        uploaded = (config.data as FormData).get("file");
        return ok({ url: "/v1/public/site-assets/0123456789abcdef.png" });
      }
      if (config.method === "put" && config.url === "/v1/admin/settings") {
        putBody = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
        return ok({ ...persisted, ...putBody });
      }
      throw new Error(`Unexpected request: ${config.method} ${config.url}`);
    };

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    const { container } = render(
      <Theme>
        <MemoryRouter initialEntries={["/admin/settings"]}>
          <QueryClientProvider client={queryClient}>
            <Settings />
          </QueryClientProvider>
        </MemoryRouter>
      </Theme>,
    );

    await screen.findByDisplayValue("JuCode");
    await user.clear(screen.getByPlaceholderText("QQ 群"));
    await user.type(screen.getByPlaceholderText("QQ 群"), "微信群");
    await user.type(screen.getByPlaceholderText("例如 123456789"), "123456789");

    const file = new File(["png"], "qr.png", { type: "image/png" });
    const input = container.querySelector<HTMLInputElement>('input[type="file"][accept="image/png,image/jpeg,image/webp"]');
    expect(input).not.toBeNull();
    await user.upload(input!, file);
    await screen.findByAltText("群二维码预览");
    expect(uploaded).toBeInstanceOf(File);

    await user.click(screen.getByText("显示右下角联系按钮"));
    await user.click(screen.getByRole("button", { name: "保存全部" }));

    await waitFor(() => expect(putBody).toBeDefined());
    expect(putBody).toMatchObject({
      contact_group_label: "微信群",
      contact_group: "123456789",
      contact_group_qr_url: "/v1/public/site-assets/0123456789abcdef.png",
      contact_dock_enabled: true,
    });
  });
});

describe("top-up presets", () => {
  it("saves the parsed list and ignores text that does not parse", async () => {
    const persisted = {
      site_name: "JuCode",
      contact_group_label: "QQ 群",
      contact_group: "",
      contact_group_qr_url: "",
      marketing_banner: { enabled: false, text: "", link_url: "" },
      topup_presets: [50, 100],
    } as unknown as SiteSettings;
    let putBody: Record<string, unknown> | undefined;
    http.defaults.adapter = async (config) => {
      const ok = (data: unknown) => ({ data, status: 200, statusText: "OK", headers: {}, config });
      if (config.method === "get" && config.url === "/v1/admin/settings") return ok(persisted);
      if (config.method === "put" && config.url === "/v1/admin/settings") {
        putBody = typeof config.data === "string" ? JSON.parse(config.data) : config.data;
        return ok({ ...persisted, ...putBody });
      }
      throw new Error(`Unexpected request: ${config.method} ${config.url}`);
    };

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const user = userEvent.setup();
    render(
      <Theme>
        <MemoryRouter initialEntries={["/admin/settings"]}>
          <QueryClientProvider client={queryClient}>
            <Settings />
          </QueryClientProvider>
        </MemoryRouter>
      </Theme>,
    );

    await screen.findByDisplayValue("JuCode");
    await user.click(screen.getByRole("button", { name: "支付通道" }));
    const input = await screen.findByDisplayValue("50, 100");
    await user.clear(input);
    await user.type(input, "30, 60x");
    expect(await screen.findByText("只能填写正整数，用逗号分隔。")).toBeTruthy();
    await user.type(input, "{backspace}0, 120");
    await user.click(screen.getByRole("button", { name: "保存全部" }));

    await waitFor(() => expect(putBody).toBeDefined());
    expect(putBody?.topup_presets).toEqual([30, 600, 120]);
  });
});
