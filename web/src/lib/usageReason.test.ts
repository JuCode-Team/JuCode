import { describe, expect, it, vi } from "vitest";

// Labels come from i18n, whose init touches document.documentElement; the
// test runs in node, so give it a stand-in before the import below runs.
vi.hoisted(() => {
  (globalThis as { document?: unknown }).document ??= { documentElement: {} };
});

import { usageReasonColor, usageReasonLabel } from "./usageReason";

describe("usageReasonLabel", () => {
  it("names client cancel instead of upstream error", () => {
    expect(usageReasonLabel("client_canceled", "upstream_error")).toBe("客户端取消");
  });

  it("keeps mid-generation failure distinct from unavailable", () => {
    expect(usageReasonLabel("generation_failed", "upstream_error")).toBe("生成失败");
    expect(usageReasonLabel("stream_interrupted", "upstream_error")).toBe("响应中断");
    expect(usageReasonLabel("upstream_unavailable", "upstream_error")).toBe("上游不可用");
  });

  it("falls back to the coarse status when no reason code", () => {
    expect(usageReasonLabel(undefined, "upstream_error")).toBe("上游错误");
    expect(usageReasonLabel(undefined, "ok")).toBe("成功");
  });

  it("does not name channel quota on the user badge", () => {
    expect(usageReasonLabel("service_busy", "upstream_error")).toBe("服务繁忙");
    expect(usageReasonLabel("upstream_quota", "upstream_error")).toBe("服务繁忙");
    expect(usageReasonLabel("channel_auth", "upstream_error")).toBe("上游不可用");
  });
});

describe("usageReasonColor", () => {
  it("does not paint client cancel as a platform fault", () => {
    expect(usageReasonColor("client_canceled", "upstream_error")).toBe("gray");
  });

  it("does not paint masked channel quota as a billing shortfall", () => {
    expect(usageReasonColor("service_busy", "upstream_error")).toBe("red");
    expect(usageReasonColor("upstream_quota", "upstream_error")).toBe("red");
  });
});
