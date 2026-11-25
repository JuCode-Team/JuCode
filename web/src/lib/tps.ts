// Per-request decode speed for the usage table. Kept out of the page so the
// rule is testable — it is the same rule the admin aggregates apply
// (obsTPSRowFilter in JuCode-backend/internal/api/admin/admin_observability.go),
// and it has been wrong once already.

// Under this much generation time the tokens arrived inside a frame or two,
// so the rate would describe SSE batching rather than the model: a stream the
// client cut short, or a relay that buffered the whole body and released it
// at once, otherwise reads as tens of thousands of tokens per second.
export const MIN_TPS_WINDOW_MS = 50;

// The delivery must also occupy a meaningful share of the request. A relay
// that assembles the whole answer before replaying it spends nearly the whole
// request silent and then delivers in a burst, so the window measures the
// replay rather than the model — one observed 1487-token answer arrived 52ms
// before the end of a 34s request. A share of the request is dimensionless
// and self-scaling, unlike a maximum-tokens-per-second constant, which would
// encode a claim about model physics and need re-tuning for a faster fleet.
// Same bound as the backend aggregates (obsTPSMinDeliveryShare).
export const MIN_TPS_DELIVERY_SHARE = 0.05;

// The hard stop behind that share bound: at least 2ms of window per output
// token, an implied ceiling of 500 t/s. What the share bound cannot rule out
// is a request that was short end to end and still carried a lot of tokens —
// a relay replaying a cached answer responds in tens of milliseconds, and a
// twentieth of that is still tens of milliseconds. No arrival-time signal
// tells that apart from a real decode, so this ceiling stays as the final
// stop. It claims only what is physically safe and nothing more.
// Same bound as the backend aggregates (obsTPSRowFilter).
export const MIN_TPS_WINDOW_MS_PER_TOKEN = 2;

/** The fields decodeTps needs; structurally compatible with the usage row. */
export interface TpsInput {
  status: string;
  tokens_out: number;
  latency_ms: number;
  ttft_ms?: number | null;
  /** Model started producing output (an encrypted reasoning item counts). */
  first_output_ms?: number | null;
  /** First upstream frame; weaker proxy, used for rows predating the above. */
  first_frame_ms?: number | null;
}

/**
 * Decode speed: output tokens over the generation window, rather than over
 * the whole request — that would charge prefill, queueing and retries to the
 * model and make a fast model on a long prompt read as slow.
 *
 * The window starts when the model started producing output, NOT at the first
 * visible token, and the difference is the whole point for reasoning models: a
 * thinking phase carries no readable text, so ttft_ms lands at the end of it
 * while tokens_out (the upstream's completion_tokens) counts every reasoning
 * token. Anchoring at ttft_ms would divide the thinking tokens by the answer's
 * time alone — one observed xhigh request read as 228 t/s for a model that
 * decodes at ~50.
 *
 * first_output_ms is the preferred start: the pipe records it on the first
 * frame showing the model producing anything, including a reasoning item
 * whose content is encrypted. first_frame_ms is the weaker fallback for rows
 * written before that field existed, and ttft_ms the last resort, so the
 * window never starts after the first token it is dividing.
 *
 * Null whenever the window is not measurable; the column header explains the
 * dash. The admin avg_tps uses the same window and bounds EXCEPT the 50ms
 * floor, which only exists here: an aggregate absorbs a handful of
 * unresolvable rows, a single cell would just print 40000 t/s.
 */
export function decodeTps(log: TpsInput): number | null {
  if (log.status !== "ok" || log.tokens_out <= 0 || log.ttft_ms == null) return null;
  const started = log.first_output_ms ?? log.first_frame_ms ?? log.ttft_ms;
  const windowMs = log.latency_ms - Math.min(log.ttft_ms, started);
  if (windowMs < MIN_TPS_WINDOW_MS) return null;
  if (windowMs < log.latency_ms * MIN_TPS_DELIVERY_SHARE) return null;
  if (windowMs < log.tokens_out * MIN_TPS_WINDOW_MS_PER_TOKEN) return null;
  return (log.tokens_out * 1000) / windowMs;
}
