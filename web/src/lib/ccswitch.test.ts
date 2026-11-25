import { describe, expect, it } from "vitest";
import {
  buildCcSwitchLink,
  ccSwitchEndpointFor,
  CODEX_DEFAULT_MODEL,
  GROK_DEFAULT_MODEL,
  OPENCODE_DEFAULT_MODEL,
  apiKeyEnv,
  manualImport,
  providerId,
} from "./ccswitch";

const endpoints = {
  anthropic: "https://example.com/anthropic",
  openaiResponses: "https://example.com/v1",
};

describe("ccSwitchEndpointFor", () => {
  it("keeps each client on its own protocol", () => {
    expect(ccSwitchEndpointFor("claude", endpoints)).toBe(endpoints.anthropic);
    expect(ccSwitchEndpointFor("claude-desktop", endpoints)).toBe(endpoints.anthropic);
    expect(ccSwitchEndpointFor("codex", endpoints)).toBe(endpoints.openaiResponses);
    expect(ccSwitchEndpointFor("grokbuild", endpoints)).toBe(endpoints.openaiResponses);
    expect(ccSwitchEndpointFor("opencode", endpoints)).toBe(endpoints.openaiResponses);
    expect(ccSwitchEndpointFor("pi", endpoints)).toBe(endpoints.openaiResponses);
  });
});

describe("buildCcSwitchLink", () => {
  it("imports Claude Desktop as its own app, not as Claude Code", () => {
    const desktop = buildCcSwitchLink({
      app: "claude-desktop",
      name: "JuCode",
      endpoint: endpoints.anthropic,
      apiKey: "sk-test",
      homepage: "https://example.com",
    });
    const cli = buildCcSwitchLink({
      app: "claude",
      name: "JuCode",
      endpoint: endpoints.anthropic,
      apiKey: "sk-test",
      homepage: "https://example.com",
    });

    const url = new URL(desktop);
    expect(url.protocol).toBe("ccswitch:");
    expect(url.host).toBe("v1");
    expect(url.pathname).toBe("/import");
    expect(url.searchParams.get("resource")).toBe("provider");
    expect(url.searchParams.get("app")).toBe("claude-desktop");
    expect(url.searchParams.get("endpoint")).toBe(endpoints.anthropic);
    expect(url.searchParams.get("enabled")).toBe("true");
    expect(url.searchParams.has("model")).toBe(false);
    expect(new URL(cli).searchParams.get("app")).toBe("claude");
    expect(desktop).not.toBe(cli);
  });

  it("writes gpt-5.5 as the Codex model", () => {
    const url = new URL(buildCcSwitchLink({
      app: "codex",
      name: "JuCode",
      endpoint: endpoints.openaiResponses,
      apiKey: "sk-test",
      model: CODEX_DEFAULT_MODEL,
    }));
    expect(url.searchParams.get("app")).toBe("codex");
    expect(url.searchParams.get("model")).toBe("gpt-5.5");
  });

  it("imports Grok as grokbuild on the OpenAI base", () => {
    const url = new URL(buildCcSwitchLink({
      app: "grokbuild",
      name: "JuCode",
      endpoint: endpoints.openaiResponses,
      apiKey: "sk-test",
      model: GROK_DEFAULT_MODEL,
    }));
    expect(url.searchParams.get("app")).toBe("grokbuild");
    expect(url.searchParams.get("endpoint")).toBe(endpoints.openaiResponses);
    expect(url.searchParams.get("model")).toBe("grok-4.7");
  });
});

describe("manualImport", () => {
  it("keeps Claude Desktop on the manual guide until the app id is accepted", () => {
    const manual = manualImport("claude-desktop", {
      endpoint: endpoints.anthropic,
      apiKey: "sk-test",
      siteName: "JuCode",
    });
    expect(manual.deepLink).toBe(false);
    expect(manual.body).toContain('"inferenceProvider": "gateway"');
    expect(manual.body).toContain(`"inferenceGatewayBaseUrl": "${endpoints.anthropic}"`);
    expect(manual.body).toContain('"inferenceGatewayAuthScheme": "bearer"');
    expect(manual.body).toContain('"inferenceGatewayApiKey": "sk-test"');
  });

  it("gives Pi a pasteable OpenAI config and no deep link", () => {
    const manual = manualImport("pi", { endpoint: endpoints.openaiResponses, apiKey: "sk-test", siteName: "JuCode" });
    expect(manual.deepLink).toBe(false);
    expect(manual.body).toContain("openai-responses");
    expect(manual.body).toContain(endpoints.openaiResponses);
    expect(manual.body).toContain("sk-test");
  });

  it("imports OpenCode as an OpenAI-compatible provider", () => {
    const url = new URL(buildCcSwitchLink({
      app: "opencode",
      name: "JuCode",
      endpoint: endpoints.openaiResponses,
      apiKey: "sk-test",
      model: OPENCODE_DEFAULT_MODEL,
    }));
    expect(url.searchParams.get("app")).toBe("opencode");
    expect(url.searchParams.get("endpoint")).toBe(endpoints.openaiResponses);
    expect(url.searchParams.get("model")).toBe("gpt-5.5");

    const manual = manualImport("opencode", {
      endpoint: endpoints.openaiResponses,
      apiKey: "sk-test",
      model: OPENCODE_DEFAULT_MODEL,
      siteName: "JuCode",
    });
    expect(manual.deepLink).toBe(true);
    expect(manual.body).toContain("@ai-sdk/openai-compatible");
    expect(manual.body).toContain(`"baseURL": "${endpoints.openaiResponses}"`);
    expect(manual.body).toContain('"gpt-5.5"');
    expect(manual.body).toContain("sk-test");
  });

  it("covers Grok's official model names in the manual file", () => {
    const manual = manualImport("grokbuild", {
      endpoint: endpoints.openaiResponses,
      apiKey: "sk-test",
      model: GROK_DEFAULT_MODEL,
      siteName: "My Gateway",
    });
    expect(manual.body).toContain("[model.grok-4.7]");
    expect(manual.body).toContain("[model.grok-4.6]");
    expect(manual.body).toContain('default = "mygateway"');
    expect(manual.body).toContain('env_key = "MYGATEWAY_API_KEY"');
    expect(manual.body).toContain("sk-test");
  });

  it("derives the provider id from the site name", () => {
    expect(providerId("JuCode")).toBe("jucode");
    expect(providerId("My Gateway")).toBe("mygateway");
    expect(apiKeyEnv("my-gw")).toBe("MY_GW_API_KEY");
  });
});
