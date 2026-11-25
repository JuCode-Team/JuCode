import { describe, expect, it } from "vitest";
import { vendorChoices, vendorKey } from "./modelBrand";

describe("vendorChoices", () => {
  it("names Anthropic as Claude and xAI as xAI, with 全部 implied by an empty filter", () => {
    expect(vendorKey("Anthropic")).toBe("claude");
    expect(vendorKey("xAI")).toBe("grok");
    expect(vendorKey("OpenAI")).toBe("openai");
    expect(vendorChoices([
      { vendors: ["OpenAI"] },
      { vendors: ["Anthropic"] },
      { vendors: ["xAI"] },
      { vendors: ["Anthropic", "Claude"] },
    ]).map((choice) => `${choice.label}:${choice.count}`)).toEqual(["OpenAI:1", "Claude:2", "xAI:1"]);
  });
});
