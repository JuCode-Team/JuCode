import { describe, expect, it } from "vitest";

import {
  appendToolTurn,
  effortOptions,
  foldAnthropicBlocks,
  foldLabTool,
  imageToolModel,
  labConsolePath,
  labHeaders,
  labPath,
  labRequestBody,
  labSurface,
  labToolDefs,
  labToolDetail,
  readLabDelta,
  readLabTools,
  imageQualityOptions,
  imageEditLimit,
  imageReferenceLimit,
  imageSizeError,
  imageSizePolicy,
  imageSupportsFidelity,
  supportsWebSearch,
  type AnthropicBlock,
  type LabToolAcc,
} from "./labProtocol";

function fold(events: Array<Record<string, unknown>>): LabToolAcc[] {
  return events.reduce((tools, event) => readLabTools(event).patches.reduce(foldLabTool, tools), [] as LabToolAcc[]);
}

describe("lab protocol", () => {
  it("keeps a plain gpt chat on chat completions", () => {
    expect(labSurface("gpt-56-sol", { search: false, image: false })).toBe("chat");
    expect(labSurface("gpt-56-sol", { search: true, image: false })).toBe("responses");
    expect(labSurface("claude-opus-5", { search: true, image: false })).toBe("anthropic");
    expect(labSurface("deepseek-v4-flash", { search: true, image: false })).toBe("chat");
    expect(labPath("chat")).toBe("/v1/chat/completions");
    expect(labConsolePath("chat")).toBe("/v1/me/lab/chat/completions");
    expect(labConsolePath("anthropic")).toBe("/v1/me/lab/anthropic/messages");
    expect(labConsolePath("responses")).toBe("/v1/me/lab/responses");
  });

  it("offers the platform search tools as plain functions on every surface", () => {
    const claude = labToolDefs("anthropic", { search: true, image: false });
    expect(claude.map((tool) => tool.name)).toEqual(["web_search", "web_fetch"]);
    expect(claude[0]).toHaveProperty("input_schema");
    expect(claude[0]).not.toHaveProperty("type");
    const responses = labToolDefs("responses", { search: true, image: true });
    expect(responses.map((tool) => [tool.type, tool.name])).toEqual([
      ["function", "web_search"], ["function", "web_fetch"], ["function", "generate_image"],
    ]);
    const chat = labToolDefs("chat", { search: true, image: false });
    expect(chat[1]).toMatchObject({ type: "function", function: { name: "web_fetch" } });
    expect(labToolDefs("chat", { search: false, image: false })).toEqual([]);
    const all = JSON.stringify([claude, responses, chat]);
    expect(all).not.toContain("web_search_20250305");
    expect(all).not.toContain("\"type\":\"web_search\"");
  });

  it("gives every text model web search, but not image models", () => {
    expect(supportsWebSearch("deepseek-v4-flash")).toBe(true);
    expect(supportsWebSearch("claude-opus-5")).toBe(true);
    expect(supportsWebSearch("gpt-image-2")).toBe(false);
    expect(supportsWebSearch("dall-e-3")).toBe(false);
    expect(supportsWebSearch("")).toBe(false);
  });

  it("sends the billing source and pins a group only when one is chosen", () => {
    expect(labHeaders({ token: "t", orgId: "o", group: "_auto", billingSource: "balance_only" })).toEqual({
      "Content-Type": "application/json",
      Authorization: "Bearer t",
      "X-Organization-Id": "o",
      "X-JuCode-Billing-Source": "balance_only",
    });
    expect(labHeaders({ group: "g1", billingSource: "plan_only", json: false })).toEqual({
      "X-JuCode-Group": "g1",
      "X-JuCode-Billing-Source": "plan_only",
    });
    expect(labHeaders({ group: "" })).toEqual({ "Content-Type": "application/json" });
  });

  it("puts the system prompt and forced answer where each surface expects them", () => {
    const tools = labToolDefs("chat", { search: true, image: false });
    const chat = labRequestBody("chat", { model: "m", input: [{ role: "user", content: "hi" }], tools, system: "S", forceAnswer: true, effort: "high" });
    expect(chat.messages).toEqual([{ role: "system", content: "S" }, { role: "user", content: "hi" }]);
    expect(chat.tool_choice).toBe("none");
    expect(chat.reasoning_effort).toBe("high");
    const responses = labRequestBody("responses", { model: "m", input: [], tools, system: "S" });
    expect(responses.instructions).toBe("S");
    expect(responses).not.toHaveProperty("tool_choice");
    const claude = labRequestBody("anthropic", { model: "m", input: [], tools, system: "S", forceAnswer: true });
    expect(claude.system).toBe("S");
    expect(claude.tool_choice).toEqual({ type: "none" });
    expect(labRequestBody("chat", { model: "m", input: [], tools: [], forceAnswer: true })).not.toHaveProperty("tool_choice");
  });

  it("offers image generation for gpt-image-2 and 2.5, including suffixes", () => {
    const names = ["gpt-image-1", "gpt-image-2-mini", "gpt-image-2.5-flare", "gpt-image-2", "claude-opus-5"];
    expect(imageToolModel(names, ["gpt-image-2.5-flare", "claude-opus-5"])).toBe("gpt-image-2.5-flare");
    expect(imageToolModel(names, names)).toBe("gpt-image-2");
    expect(imageToolModel(names, ["gpt-image-1"])).toBe("");
  });

  it("hides thinking levels on models that do not take them", () => {
    expect(effortOptions("claude-opus-5").map((o) => o.value)).toContain("xhigh");
    expect(effortOptions("gpt-image-2")).toEqual([]);
  });

  it("reads chat completions tool calls and builds the next turn", () => {
    const calls = fold([
      { choices: [{ delta: { tool_calls: [{ index: 0, id: "call_1", type: "function", function: { name: "web_search", arguments: "" } }] }, finish_reason: null }] },
      { choices: [{ delta: { tool_calls: [{ index: 0, function: { arguments: "{\"query\":\"今日汇率\"}" } }] }, finish_reason: null }] },
      { choices: [{ delta: { tool_calls: [{ index: 1, id: "call_2", function: { name: "web_fetch", arguments: "{\"url\":\"https://a.cn\"}" } }] }, finish_reason: null }] },
      { choices: [{ delta: { tool_calls: [{ index: 2, id: "call_3", function: { name: "unknown_tool", arguments: "{}" } }] }, finish_reason: "tool_calls" }] },
    ]);
    expect(calls.map((call) => [call.id, call.name, labToolDetail(call.input)])).toEqual([
      ["call_1", "web_search", "今日汇率"],
      ["call_2", "web_fetch", "https://a.cn"],
    ]);
    const next = appendToolTurn("chat", [{ role: "user", content: "汇率" }], {
      text: "",
      reasoning: "想一下",
      calls: calls.map((call) => ({ id: call.id, name: call.name, arguments: call.input, output: `out-${call.id}` })),
    });
    expect(next[1]).toEqual({
      role: "assistant",
      content: null,
      reasoning_content: "想一下",
      tool_calls: [
        { id: "call_1", type: "function", function: { name: "web_search", arguments: "{\"query\":\"今日汇率\"}" } },
        { id: "call_2", type: "function", function: { name: "web_fetch", arguments: "{\"url\":\"https://a.cn\"}" } },
      ],
    });
    expect(next.slice(2)).toEqual([
      { role: "tool", tool_call_id: "call_1", content: "out-call_1" },
      { role: "tool", tool_call_id: "call_2", content: "out-call_2" },
    ]);
  });

  it("reads responses function_call items and answers them by call_id", () => {
    const events = [
      { type: "response.output_item.added", item: { type: "function_call", id: "fc_1", call_id: "call_1", name: "web_search", arguments: "" } },
      { type: "response.function_call_arguments.delta", item_id: "fc_1", output_index: 0, delta: "{\"query\":" },
      { type: "response.function_call_arguments.delta", item_id: "fc_1", output_index: 0, delta: "\"雨夜\"}" },
      { type: "response.function_call_arguments.delta", item_id: "fc_other", output_index: 3, delta: "{\"query\":\"不该出现\"}" },
      { type: "response.output_item.done", item: { type: "function_call", id: "fc_1", call_id: "call_1", name: "web_search", arguments: "{\"query\":\"雨夜\"}" } },
    ];
    const calls = fold(events);
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({ id: "fc_1", callId: "call_1", name: "web_search", done: true });
    expect(labToolDetail(calls[0].input)).toBe("雨夜");
    expect(readLabTools({ type: "response.output_item.done", item: { type: "web_search_call", id: "ws_1" } }).patches).toEqual([]);
    const next = appendToolTurn("responses", [{ role: "user", content: "q" }], {
      text: "我查一下",
      calls: [{ id: "fc_1", callId: "call_1", name: "web_search", arguments: calls[0].input, output: "R" }],
    });
    expect(next.slice(1)).toEqual([
      { role: "assistant", content: "我查一下" },
      { type: "function_call", call_id: "call_1", name: "web_search", arguments: "{\"query\":\"雨夜\"}" },
      { type: "function_call_output", call_id: "call_1", output: "R" },
    ]);
  });

  it("reads anthropic tool_use blocks and sends thinking back with tool_result", () => {
    const events: Array<Record<string, unknown>> = [
      { type: "content_block_start", index: 0, content_block: { type: "thinking", thinking: "", signature: "" } },
      { type: "content_block_delta", index: 0, delta: { type: "thinking_delta", thinking: "要搜" } },
      { type: "content_block_delta", index: 0, delta: { type: "signature_delta", signature: "sig" } },
      { type: "content_block_stop", index: 0 },
      { type: "content_block_start", index: 1, content_block: { type: "text", text: "" } },
      { type: "content_block_delta", index: 1, delta: { type: "text_delta", text: "先搜索。" } },
      { type: "content_block_stop", index: 1 },
      { type: "content_block_start", index: 2, content_block: { type: "tool_use", id: "toolu_1", name: "web_fetch", input: {} } },
      { type: "content_block_delta", index: 2, delta: { type: "input_json_delta", partial_json: "{\"url\":\"https://b.cn\"}" } },
      { type: "content_block_stop", index: 2 },
      { type: "message_delta", delta: { stop_reason: "tool_use" } },
    ];
    const calls = fold(events);
    expect(calls).toEqual([
      { id: "toolu_1", callId: undefined, index: 2, name: "web_fetch", input: "{\"url\":\"https://b.cn\"}", done: true },
    ]);
    expect(readLabTools(events[events.length - 1]).stop).toBe(true);
    const blocks = events.reduce<AnthropicBlock[]>((acc, event) => foldAnthropicBlocks(acc, event), []);
    const next = appendToolTurn("anthropic", [{ role: "user", content: "q" }], {
      text: "先搜索。",
      blocks,
      calls: [{ id: "toolu_1", name: "web_fetch", arguments: calls[0].input, output: "页面", isError: false }],
    });
    expect(next[1]).toEqual({
      role: "assistant",
      content: [
        { type: "thinking", thinking: "要搜", signature: "sig" },
        { type: "text", text: "先搜索。" },
        { type: "tool_use", id: "toolu_1", name: "web_fetch", input: { url: "https://b.cn" } },
      ],
    });
    expect(next[2]).toEqual({ role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_1", content: "页面" }] });
    const failed = appendToolTurn("anthropic", [], {
      text: "",
      calls: [{ id: "toolu_2", name: "web_search", arguments: "{\"query\":\"x\"}", output: "失败", isError: true }],
    });
    expect(failed[0]).toEqual({ role: "assistant", content: [{ type: "tool_use", id: "toolu_2", name: "web_search", input: { query: "x" } }] });
    expect(failed[1]).toEqual({ role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_2", content: "失败", is_error: true }] });
  });

  it("ignores anthropic server tools and keeps generate_image", () => {
    expect(fold([
      { type: "content_block_start", index: 0, content_block: { type: "server_tool_use", id: "srv_1", name: "web_search" } },
    ])).toEqual([]);
    const drawn = fold([
      { type: "content_block_start", index: 1, content_block: { type: "tool_use", id: "toolu_1", name: "generate_image" } },
      { type: "content_block_delta", index: 1, delta: { type: "input_json_delta", partial_json: "{\"prompt\":\"一只猫\"}" } },
    ]);
    expect(labToolDetail(drawn[0].input)).toBe("一只猫");
    expect(labToolDetail("{\"prompt\":\"还在流")).toBe("还在流");
    expect(labToolDetail("{\"url\":\"https://c.cn/pa")).toBe("https://c.cn/pa");
  });

  it("reads text and images from each official stream", () => {
    expect(readLabDelta({ choices: [{ delta: { content: "hi", reasoning_content: "think" } }] })).toEqual({
      text: "hi", reasoning: "think", images: [],
    });
    expect(readLabDelta({ type: "response.output_text.delta", delta: "a" }).text).toBe("a");
    expect(readLabDelta({ type: "content_block_delta", delta: { type: "text_delta", text: "b" } }).text).toBe("b");
    expect(readLabDelta({
      type: "response.output_item.done",
      item: { type: "image_generation_call", result: "aaaa" },
    }).images).toEqual(["data:image/png;base64,aaaa"]);
  });

  it("uses documented sizes for each gpt image family", () => {
    expect(imageSizePolicy("gpt-image-2").flexible).toBe(true);
    expect(imageSizePolicy("gpt-image-2.5-flare").presets.map((item) => item.size)).toContain("3840x2160");
    expect(imageSizePolicy("gpt-image-1.5").flexible).toBe(false);
    expect(imageSizePolicy("gpt-image-1-mini").presets.map((item) => item.size)).toEqual([
      "auto", "1024x1024", "1536x1024", "1024x1536",
    ]);
    expect(imageSizeError("gpt-image-2", "512x512")).toMatch(/655360/);
    expect(imageSizeError("gpt-image-2", "1024x1025")).toMatch(/16/);
    expect(imageSizeError("gpt-image-2", "3840x1024")).toMatch(/3:1/);
    expect(imageSizeError("gpt-image-2", "2560x1440")).toBe("");
    expect(imageSizeError("gpt-image-1", "2560x1440")).toMatch(/列出的尺寸/);
    expect(imageSizeError("gpt-image-1", "auto")).toBe("");
  });

  it("caps references at the documented edit limit and keeps fidelity off gpt-image-2", () => {
    expect(imageReferenceLimit("gpt-image-2")).toBe(16);
    expect(imageReferenceLimit("gpt-image-2.5-flare")).toBe(16);
    expect(imageReferenceLimit("dall-e-2")).toBe(1);
    expect(imageReferenceLimit("dall-e-3")).toBe(0);
    expect(imageEditLimit("gpt-image-2")).toBe(1);
    expect(imageEditLimit("dall-e-2")).toBe(1);
    expect(imageEditLimit("dall-e-3")).toBe(0);
    expect(imageQualityOptions("gpt-image-2.5-sunburst")).toContain("max");
    expect(imageQualityOptions("gpt-image-2")).not.toContain("max");
    expect(imageSupportsFidelity("gpt-image-1.5")).toBe(true);
    expect(imageSupportsFidelity("gpt-image-2")).toBe(false);
  });
});
