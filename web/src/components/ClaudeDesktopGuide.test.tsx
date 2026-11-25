/** @vitest-environment jsdom */
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { ClaudeDesktopGuide } from "./ClaudeDesktopGuide";

afterEach(() => cleanup());

describe("ClaudeDesktopGuide", () => {
  it("walks from the Desktop tab through add and the filled form", async () => {
    const user = userEvent.setup();
    render(
      <ClaudeDesktopGuide
        providerName="JuCode"
        endpoint="https://api.example.com/anthropic"
        apiKey="sk-test"
      />,
    );

    expect(screen.getByText(/带显示器角标的是 Claude Desktop/)).toBeTruthy();
    await user.click(screen.getByRole("tab", { name: "Claude Desktop" }));

    expect(screen.getByText(/橙色加号/)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "添加新供应商" }));

    expect(screen.getByText("sk-test")).toBeTruthy();
    expect(screen.getByText("https://api.example.com/anthropic")).toBeTruthy();
    expect(screen.getByRole("button", { name: "直连" }).getAttribute("aria-pressed")).toBe("true");

    await user.click(screen.getByRole("button", { name: "模型映射" }));
    expect(screen.getByRole("button", { name: "添加" })).toHaveProperty("disabled", true);

    await user.click(screen.getByRole("button", { name: "直连" }));
    await user.click(screen.getByRole("button", { name: /^添加$/ }));

    expect(screen.getByText("JuCode")).toBeTruthy();
    expect(screen.getByText(/重启 Claude Desktop/)).toBeTruthy();
  });
});
