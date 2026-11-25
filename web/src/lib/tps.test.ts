import { describe, expect, it } from "vitest";
import { decodeTps, type TpsInput } from "./tps";

function row(over: Partial<TpsInput>): TpsInput {
  return { status: "ok", tokens_out: 500, latency_ms: 10_000, ttft_ms: 1_000, ...over };
}

describe("decodeTps", () => {
  it("divides tokens by the window after the first token", () => {
    // 500 tokens over 9s = 55.6 t/s
    expect(decodeTps(row({}))).toBeCloseTo(55.6, 1);
  });

  it("starts the window at the upstream's first frame, not the first visible token", () => {
    // A reasoning request: frames from 20s, first visible token at 500s, done
    // at 560s, 8000 tokens. The thinking tokens are counted in tokens_out, so
    // the window has to cover the thinking phase.
    const log = row({ tokens_out: 8000, latency_ms: 560_000, ttft_ms: 500_000, first_frame_ms: 20_000 });
    expect(decodeTps(log)).toBeCloseTo(14.8, 1);
    // Anchoring at ttft_ms would have read 8000 / 60s = 133 t/s.
  });

  it("prefers first_output_ms, then first_frame_ms, then ttft_ms", () => {
    // The model started producing at 20s (encrypted reasoning), the first
    // frame arrived at 15s, the first visible token at 500s.
    const log = row({
      tokens_out: 8000, latency_ms: 560_000,
      ttft_ms: 500_000, first_output_ms: 20_000, first_frame_ms: 15_000,
    });
    expect(decodeTps(log)).toBeCloseTo(14.8, 1);

    // Rows written before first_output_ms existed fall back to the frame.
    const older = row({
      tokens_out: 8000, latency_ms: 560_000,
      ttft_ms: 500_000, first_output_ms: null, first_frame_ms: 20_000,
    });
    expect(decodeTps(older)).toBeCloseTo(14.8, 1);

    // And with neither, to the first token.
    const noFrame = row({ tokens_out: 500, first_output_ms: null, first_frame_ms: null });
    expect(decodeTps(noFrame)).toBeCloseTo(decodeTps(row({})) ?? 0, 5);
  });

  it("ignores a first frame that arrived after the first token", () => {
    // Only the earlier of the two may move the window start.
    expect(decodeTps(row({ first_frame_ms: 2_000 }))).toBeCloseTo(55.6, 1);
  });

  it("rejects rows with no measurable window", () => {
    expect(decodeTps(row({ status: "upstream_error" }))).toBeNull();
    expect(decodeTps(row({ tokens_out: 0 }))).toBeNull();
    expect(decodeTps(row({ ttft_ms: null }))).toBeNull();
    // Window below the 50ms floor.
    expect(decodeTps(row({ latency_ms: 1_030, ttft_ms: 1_000 }))).toBeNull();
  });

  it("rejects a delivery that occupied only a sliver of the request", () => {
    // A relay assembling the answer: 1487 tokens delivered in the last 52ms
    // of a 34s request. The window measures the replay, not the model, so
    // there is no rate to report — 28,596 t/s would be meaningless.
    const replay = row({ tokens_out: 1487, latency_ms: 34_018, ttft_ms: 33_966, first_frame_ms: 33_966 });
    expect(decodeTps(replay)).toBeNull();

    // The same tokens over a delivery that is a real share of the request
    // (an answer phase that took 170s of a 448s thinking-heavy request).
    const streamed = row({ tokens_out: 9186, latency_ms: 448_450, ttft_ms: 427_709, first_frame_ms: 278_510 });
    expect(decodeTps(streamed)).toBeCloseTo(54.1, 1);
  });

  it("stops short requests that carried a lot of tokens", () => {
    // The share bound cannot catch this: the delivery *is* the whole request
    // (90ms), so it passes at 100%. 600 tokens in 90ms is 6,666 t/s, which no
    // model decodes — the per-token stop is what rules it out.
    expect(decodeTps(row({ tokens_out: 600, latency_ms: 90, ttft_ms: 0, first_frame_ms: 0 }))).toBeNull();
    // The same shape with few enough tokens to be plausible still counts.
    expect(decodeTps(row({ tokens_out: 20, latency_ms: 90, ttft_ms: 0, first_frame_ms: 0 })))
      .toBeCloseTo(222.2, 1);
  });
});
