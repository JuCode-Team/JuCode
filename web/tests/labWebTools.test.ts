import { describe, expect, it } from "vitest";
import type { WebFetchRequest, WebSearchRequest } from "../src/api/webTools";
import { loadLabPrefs, loadLabTab, saveLabPrefs } from "../src/lib/labCache";
import {
  FETCH_TEXT_LIMIT, fetchOutput, fetchRequest, runWebTool, searchRequest, webToolErrorMessage, webToolPrompt,
  type LabWebToolsClient,
} from "../src/lib/labWebTools";

function axiosError(status: number, code: string, message: string) {
  return Object.assign(new Error(`Request failed with status code ${status}`), {
    response: { status, data: { error: { type: "web_tool_error", code, message } } },
  });
}

function memoryStore() {
  const data = new Map<string, string>();
  return {
    getItem: (key: string) => data.get(key) ?? null,
    setItem: (key: string, value: string) => { data.set(key, value); },
    removeItem: (key: string) => { data.delete(key); },
  };
}

describe("lab web tools", () => {
  it("checks and clamps model arguments", () => {
    expect(searchRequest({ query: "  油价 ", max_results: 50, freshness: "week" })).toEqual({ query: "油价", max_results: 10, freshness: "week" });
    expect(searchRequest({ query: "x", max_results: 0, freshness: "decade" })).toEqual({ query: "x" });
    expect(searchRequest({})).toBe("缺少搜索词 query");
    expect(fetchRequest({ url: "https://a.cn/p" })).toEqual({ url: "https://a.cn/p" });
    expect(typeof fetchRequest({ url: "a.cn" })).toBe("string");
  });

  it("cuts fetched pages before they reach the model", () => {
    const content = "字".repeat(FETCH_TEXT_LIMIT + 50);
    const out = JSON.parse(fetchOutput({ object: "web.fetch", engine: "jina", url: "https://a.cn", title: "A", content }));
    expect(out.content).toHaveLength(FETCH_TEXT_LIMIT);
    expect(out.truncated).toContain(String(content.length));
    const short = JSON.parse(fetchOutput({ object: "web.fetch", engine: "jina", url: "https://a.cn", title: "A", content: "短" }));
    expect(short).not.toHaveProperty("truncated");
  });

  it("builds a search step with the hit count and links", async () => {
    const seen: WebSearchRequest[] = [];
    const client: LabWebToolsClient = {
      search: async (b) => {
        seen.push(b);
        return {
          object: "web.search", engine: "brave", query: b.query,
          results: [{ title: "A", url: "https://a.cn", snippet: "a" }, { title: "", url: "https://b.cn", snippet: "b", published_at: "2026-09-01" }],
        };
      },
      fetch: async () => { throw new Error("unused"); },
    };
    const run = await runWebTool(client, { id: "c1", name: "web_search", arguments: "{\"query\":\"汇率\",\"max_results\":2}" });
    expect(seen).toEqual([{ query: "汇率", max_results: 2 }]);
    expect(run.isError).toBe(false);
    expect(run.step).toEqual({
      id: "c1", name: "web_search", detail: "汇率", status: "done", summary: "2 条结果",
      results: [{ title: "A", url: "https://a.cn" }, { title: "https://b.cn", url: "https://b.cn" }],
    });
    expect(JSON.parse(run.output).results[1]).toEqual({ n: 2, title: "", url: "https://b.cn", snippet: "b", published_at: "2026-09-01" });
  });

  it("turns failures into an error step and an error result", async () => {
    const fetched: WebFetchRequest[] = [];
    const client: LabWebToolsClient = {
      search: async () => { throw axiosError(429, "rpm_limit", "too many"); },
      fetch: async (b) => { fetched.push(b); throw axiosError(422, "fetch_failed", "upstream 404"); },
    };
    const search = await runWebTool(client, { id: "s", name: "web_search", arguments: "{\"query\":\"q\"}" });
    expect(search.step).toMatchObject({ status: "error", error: "调用太频繁，请稍后再试", detail: "q" });
    expect(search.isError).toBe(true);
    expect(search.output).toContain("调用太频繁");
    const page = await runWebTool(client, { id: "f", name: "web_fetch", arguments: "{\"url\":\"https://x.cn\"}" });
    expect(fetched).toEqual([{ url: "https://x.cn" }]);
    expect(page.step).toMatchObject({ name: "web_fetch", status: "error", error: "网页读取失败：upstream 404", detail: "https://x.cn" });
    const broken = await runWebTool(client, { id: "b", name: "web_fetch", arguments: "{\"url\":" });
    expect(broken.step.status).toBe("error");
    expect(fetched).toHaveLength(1);
  });

  it("explains each error code", () => {
    expect(webToolErrorMessage(axiosError(400, "invalid_web_request", "query too long"))).toBe("请求参数不对：query too long");
    expect(webToolErrorMessage(axiosError(400, "engine_disabled", ""))).toBe("这个引擎已停用，换一个试试");
    expect(webToolErrorMessage(axiosError(402, "billing_rejected", "insufficient"))).toBe("余额不足或扣费被拒绝，请先充值");
    expect(webToolErrorMessage(axiosError(503, "no_available_provider", ""))).toBe("暂时没有可用的服务，请稍后再试");
    expect(webToolErrorMessage(axiosError(500, "other", "boom"))).toBe("boom");
    expect(webToolErrorMessage(new Error("Network Error"))).toBe("Network Error");
  });

  it("asks research mode for sub-questions, reading and cited sources", () => {
    const plain = webToolPrompt({ research: false, today: "2026/9/29" });
    expect(plain).toContain("2026/9/29");
    expect(plain).not.toContain("参考来源");
    const research = webToolPrompt({ research: true, today: "2026/9/29" });
    expect(research).toContain("web_fetch");
    expect(research).toContain("[1]");
    expect(research).toContain("参考来源");
  });

  it("remembers the billing switch and tool toggles", () => {
    const store = memoryStore();
    expect(loadLabPrefs(store, "u")).toEqual({ planOnly: false, search: true, research: false });
    saveLabPrefs(store, "u", { planOnly: true, search: false, research: true });
    expect(loadLabPrefs(store, "u")).toEqual({ planOnly: true, search: false, research: true });
    store.setItem("jucode-lab:v1:u:tab", "tools");
    expect(loadLabTab(store, "u")).toBe("tools");
  });
});
