/** @vitest-environment jsdom */
import { StrictMode } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, expect, it, vi } from "vitest";
import "../../i18n";
import VerifyEmail from "./VerifyEmail";
import { authApi } from "../../api/auth";

vi.mock("../../api/auth", () => ({ authApi: { verifyEmail: vi.fn() } }));
vi.mock("../../store/auth", () => ({ useAuthStore: (selector: (state: unknown) => unknown) => selector({ user: { id: "owner" }, tokens: { access_token: "session" } }) }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });

it("consumes the link once under StrictMode and returns the logged-in user to invitations", async () => {
  vi.mocked(authApi.verifyEmail).mockResolvedValue({ ok: true, kind: "register", user: { id: "owner", email: "owner@example.com", role: "user" } });
  const client = new QueryClient();
  const invalidate = vi.spyOn(client, "invalidateQueries");
  render(
    <StrictMode><QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/verify-email?code=one-time&kind=register"]}><VerifyEmail /></MemoryRouter>
    </QueryClientProvider></StrictMode>,
  );
  await screen.findByText("邮箱已验证");
  expect(authApi.verifyEmail).toHaveBeenCalledExactlyOnceWith({ code: "one-time" });
  expect(screen.getByRole("link", { name: "返回邀请页" }).getAttribute("href")).toBe("/me/invite");
  expect(invalidate).toHaveBeenCalledWith({ queryKey: ["me"] });
});

it("verifies old change-email links without guessing their type", async () => {
  vi.mocked(authApi.verifyEmail).mockResolvedValue({ ok: true, kind: "change_email", user: { id: "owner", email: "new@example.com", role: "user" } });
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={["/verify-email?code=legacy-change"]}><VerifyEmail /></MemoryRouter>
    </QueryClientProvider>,
  );
  await screen.findByText("邮箱已更新");
  expect(authApi.verifyEmail).toHaveBeenCalledExactlyOnceWith({ code: "legacy-change" });
  expect(screen.getByRole("link", { name: "去登录" }).getAttribute("href")).toBe("/login");
});
