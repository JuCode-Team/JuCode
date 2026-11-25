import { describe, expect, it } from "vitest";
import { applyMaskStamp, blankMask, maskHasEdits } from "./imageMask";

describe("image mask", () => {
  it("paints transparent edit pixels and restores them with the eraser", () => {
    const alpha = blankMask(20, 20);
    expect(maskHasEdits(alpha)).toBe(false);
    applyMaskStamp(alpha, 20, 20, { x0: 4, y0: 4, x1: 4, y1: 4, radius: 2, erase: false });
    expect(alpha[4 * 20 + 4]).toBe(0);
    expect(alpha[0]).toBe(255);
    expect(maskHasEdits(alpha)).toBe(true);
    applyMaskStamp(alpha, 20, 20, { x0: 4, y0: 4, x1: 4, y1: 4, radius: 3, erase: true });
    expect(alpha[4 * 20 + 4]).toBe(255);
    expect(maskHasEdits(alpha)).toBe(false);
  });

  it("fills the gap between two brush points", () => {
    const alpha = blankMask(40, 10);
    applyMaskStamp(alpha, 40, 10, { x0: 2, y0: 5, x1: 36, y1: 5, radius: 1, erase: false });
    expect(alpha[5 * 40 + 20]).toBe(0);
  });
});
