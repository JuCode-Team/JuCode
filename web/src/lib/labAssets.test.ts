import { describe, expect, it } from "vitest";
import { trimAssetIds } from "./labAssets";

describe("lab asset budget", () => {
  it("drops the oldest pictures once the byte budget or the count cap is hit", () => {
    const items = [
      { id: "new", createdAt: 3, bytes: 50 },
      { id: "mid", createdAt: 2, bytes: 40 },
      { id: "old", createdAt: 1, bytes: 30 },
    ];
    expect(trimAssetIds(items, 100, 10)).toEqual(["old"]);
    expect(trimAssetIds(items, 1000, 2).sort()).toEqual(["old"]);
    expect(trimAssetIds(items, 70, 10, new Set(["old"]))).toEqual(["mid"]);
  });
});
