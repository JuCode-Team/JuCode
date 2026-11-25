import { useEffect, useMemo, useRef, useState } from "react";
import { Box, Card, Flex, Heading, SegmentedControl, Text, Tooltip } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { ArrowDownToLine, ArrowUpFromLine, Database, Hash } from "lucide-react";
import type { StatsBucket } from "../api/endpoints";

// TokenUsageSection — switchable 24h / 本周 / 本月 token rollup.
//
// Shared between the admin Overview (drives /v1/admin/stats/usage) and the
// user Me page (drives /v1/me/stats/usage). The two endpoints return the
// same { buckets: StatsBucket[] } shape, so the only thing the caller has
// to supply is the fetcher + a unique queryKey prefix so react-query caches
// stay isolated between scopes.
//
// tokens_in from the backend is plain, non-cache text input; audio lives
// in audio_tokens_* and is folded back into the displayed 输入/输出 so a
// voice-heavy window does not look empty. cached_tokens_in is the
// cache-hit portion; cache_creation_tokens_in is cache-write input. The
// caller can fold cache writes into its displayed "输入" value.

type Range = "24h" | "week" | "month";
type TokenColor = "iris" | "green" | "purple" | "amber";

export interface TokenUsageQuery {
  start: string;
  // `end` is intentionally absent — see rangeToParams. The backend defaults
  // it to now so the window rolls with each poll.
  bucket: string;
}
export interface TokenUsageResult {
  buckets: StatsBucket[];
}

export interface TokenTotals {
  in: number;
  out: number;
  cached: number;
  cacheWrite: number;
}

// useTokenTotals fetches one bucketed usage window and sums it. Shared by
// this section and the personal overview's Token 构成 panel so both roll up
// audio / cache dimensions the same way.
export function useTokenTotals(
  queryKey: readonly unknown[],
  fetcher: (params: TokenUsageQuery) => Promise<TokenUsageResult>,
  params: TokenUsageQuery,
  pollMs: number,
) {
  const { data, isLoading } = useQuery({
    queryKey: [...queryKey, "token-usage", params.start, params.bucket],
    queryFn: () => fetcher(params),
    refetchInterval: pollMs,
  });
  const totals = useMemo<TokenTotals>(() => {
    let tIn = 0, tOut = 0, tCached = 0, tCacheWrite = 0;
    for (const b of data?.buckets ?? []) {
      tIn += b.tokens_in + (b.audio_tokens_in ?? 0);
      tOut += b.tokens_out + (b.audio_tokens_out ?? 0);
      tCached += b.cached_tokens_in;
      tCacheWrite += b.cache_creation_tokens_in;
    }
    return { in: tIn, out: tOut, cached: tCached, cacheWrite: tCacheWrite };
  }, [data]);
  return { totals, isLoading };
}

// Cache hit rate = cache-read ÷ all prompt input (uncached + cache-read +
// cache-write). Cache-write tokens are misses — the prompt wasn't in cache,
// so it had to be written. Omitting them from the denominator inflates the
// rate to ~100% on cached workloads where uncached input is near-zero.
export function cacheHitRate(t: TokenTotals): string {
  const denom = t.in + t.cached + t.cacheWrite;
  return denom > 0 ? ((t.cached / denom) * 100).toFixed(1) + "%" : "—";
}

export interface TokenUsageSectionProps {
  // Stable cache key prefix — e.g. ["admin", "overview"] or ["me"]. Each
  // scope must have its own so admin & user views don't share cache rows.
  queryKey: readonly unknown[];
  // Returns the bucketed usage. Identical shape across admin & user APIs.
  fetcher: (params: TokenUsageQuery) => Promise<TokenUsageResult>;
  // Optional override; defaults to "Token 用量".
  title?: string;
  // Personal usage treats cache writes as input while keeping cache hits
  // separate. Admin views can retain the raw, split accounting dimensions.
  includeCacheWriteInInput?: boolean;
  // Desktop layout with exactly four equal cards spanning the full row.
  fullWidthCards?: boolean;
  // Poll interval in ms. Defaults to DEFAULT_POLL_MS; pass a larger value for
  // views where staleness matters even less.
  pollMs?: number;
}

// The server caches this response for 5s when the Redis hour ring can serve it
// and 30s otherwise, so anything under 5s is guaranteed to re-render an
// identical payload. This panel was polling every 2s, which made
// /v1/me/stats/usage roughly 46% of all site traffic for no visible benefit.
export const DEFAULT_POLL_MS = 15_000;

// Note: `end` is deliberately NOT sent. The backend defaults it to now on
// every request, so the window rolls forward instead of freezing at whatever
// moment this ran. Sending a fixed `end` meant the chart stopped picking up
// new traffic the instant it mounted, which made the 2s polling pointless.
//
// For the 24h range `start` is snapped to the hour: it keeps the query string
// (and therefore the response-cache key) stable for an hour at a time, and it
// matches how the backend groups rows — a mid-hour start would make the first
// bucket a partial one and rule out the Redis hour-counter fast path.
export function rangeToParams(range: Range): TokenUsageQuery {
  const now = new Date();
  let from: Date;
  let bucket: string;
  if (range === "24h") {
    from = new Date(now.getTime() - 24 * 3600 * 1000);
    from.setMinutes(0, 0, 0);
    bucket = "hour";
  } else if (range === "week") {
    // 本周 = 周一 00:00 → 现在 (Mon-start, matches CN/EU convention)
    from = new Date(now);
    const dow = (from.getDay() + 6) % 7; // Mon=0 ... Sun=6
    from.setDate(from.getDate() - dow);
    from.setHours(0, 0, 0, 0);
    bucket = "day";
  } else {
    // 本月 = 月 1 号 00:00 → 现在
    from = new Date(now.getFullYear(), now.getMonth(), 1, 0, 0, 0, 0);
    bucket = "day";
  }
  return {
    start: from.toISOString(),
    bucket,
  };
}

function SectionHeading({
  title,
  Icon,
}: {
  title: string;
  Icon: React.ComponentType<{ size?: number; strokeWidth?: number }>;
}) {
  return (
    <Flex align="center" gap="2">
      <Icon size={16} strokeWidth={1.8} />
      <Heading size="3" color="gray">{title}</Heading>
    </Flex>
  );
}

function Stat({
  label,
  value,
  Icon,
  color,
}: {
  label: string;
  value: React.ReactNode;
  Icon: React.ComponentType<{ size?: number; strokeWidth?: number }>;
  color: TokenColor;
}) {
  return (
    <Card>
      <Flex align="start" justify="between" gap="2">
        <Box style={{ minWidth: 0 }}>
          <Box style={{ marginBottom: 14, lineHeight: 1 }}>
            <Text size="2" color="gray">{label}</Text>
          </Box>
          <Heading  size="8" className="jc-num">{value}</Heading>
        </Box>
        <Box
          style={{
            width: 32, height: 32, borderRadius: 81,
            background: `var(--${color}-3)`, color: `var(--${color}-11)`,
            display: "flex", alignItems: "center", justifyContent: "center",
            flexShrink: 0,
          }}
        >
          <Icon size={16} strokeWidth={1.8} />
        </Box>
      </Flex>
    </Card>
  );
}

function AnimatedMetricValue({
  value,
  formatter,
}: {
  value: number;
  formatter: (value: number) => string;
}) {
  const [displayValue, setDisplayValue] = useState(value);
  const [delta, setDelta] = useState(0);
  const targetRef = useRef(Number.isFinite(value) ? value : 0);
  const previousTargetRef = useRef(Number.isFinite(value) ? value : 0);
  const displayRef = useRef(Number.isFinite(value) ? value : 0);
  const frameRef = useRef<number | null>(null);

  useEffect(() => {
    const to = Number.isFinite(value) ? value : 0;
    const diff = to - previousTargetRef.current;
    previousTargetRef.current = to;
    targetRef.current = to;
    setDelta(diff > 0 ? diff : 0);
    const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (reduceMotion) {
      setDisplayValue(to);
      displayRef.current = to;
      return;
    }
  }, [value]);

  useEffect(() => {
    const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (reduceMotion) return;

    let last = performance.now();
    const tick = (now: number) => {
      const elapsed = Math.min(64, now - last);
      last = now;
      const target = targetRef.current;
      const current = displayRef.current;
      const gap = target - current;
      if (Math.abs(gap) < 0.001) {
        if (current !== target) {
          displayRef.current = target;
          setDisplayValue(target);
        }
      } else {
        const next = current + gap * Math.min(1, elapsed / 2_000);
        displayRef.current = next;
        setDisplayValue(next);
      }
      frameRef.current = window.requestAnimationFrame(tick);
    };
    frameRef.current = window.requestAnimationFrame(tick);
    return () => {
      if (frameRef.current != null) window.cancelAnimationFrame(frameRef.current);
    };
  }, []);

  return (
    <span className="jc-live-metric">
      {delta > 0 && <span className="jc-live-delta">+{formatter(delta)}</span>}
      <span className="jc-overview-live-value">{formatter(displayValue)}</span>
    </span>
  );
}

export default function TokenUsageSection({
  queryKey,
  fetcher,
  title,
  includeCacheWriteInInput = false,
  fullWidthCards = false,
  pollMs = DEFAULT_POLL_MS,
}: TokenUsageSectionProps) {
  const { t } = useTranslation();
  const [range, setRange] = useState<Range>("24h");

  // Params are built per render, not memoized on [range]: a memoized value
  // would pin the window to mount time and every poll would re-request the
  // same stale slice. The query key is derived from the hour-snapped start,
  // so this is still one cache entry that refreshes in place.
  const { totals, isLoading } = useTokenTotals(queryKey, fetcher, rangeToParams(range), pollMs);

  const displayedInput = totals.in + (includeCacheWriteInInput ? totals.cacheWrite : 0);
  const displayedTotal = displayedInput + totals.cached + totals.out;

  const fmt = (n: number) => n.toLocaleString();
  const animatedValue = (value: number) => (
    <AnimatedMetricValue value={value} formatter={(v) => fmt(Math.round(v))} />
  );
  const hitRate = cacheHitRate(totals);

  return (
    <>
      <Flex align="center" justify="between" mb="2" wrap="wrap" gap="2">
        <SectionHeading title={title ?? t("usage.tokenSection.title")} Icon={Hash} />
        <Flex align="center" gap="2" wrap="wrap" justify="end">
          <SegmentedControl.Root
            value={range}
            onValueChange={(v) => setRange(v as Range)}
            size="1"
          >
            <SegmentedControl.Item value="24h">24h</SegmentedControl.Item>
            <SegmentedControl.Item value="week">{t("usage.tokenSection.thisWeek")}</SegmentedControl.Item>
            <SegmentedControl.Item value="month">{t("usage.tokenSection.thisMonth")}</SegmentedControl.Item>
          </SegmentedControl.Root>
        </Flex>
      </Flex>
      <Box className={fullWidthCards ? "jc-token-usage-grid" : "jc-stat-grid"} mb="4">
        <Stat
          label={t("usage.tokenSection.inputUncached")}
          value={isLoading ? "—" : animatedValue(totals.in)}
          Icon={ArrowDownToLine}
          color="iris"
        />
        <Stat
          label={t("usage.tokenSection.cached")}
          value={isLoading ? "—" : animatedValue(totals.cached)}
          Icon={Database}
          color="green"
        />
        <Stat
          label={t("usage.tokenSection.output")}
          value={isLoading ? "—" : animatedValue(totals.out)}
          Icon={ArrowUpFromLine}
          color="purple"
        />
        <Tooltip content={t("usage.tokenSection.hitTip")}>
          <Card>
            <Flex align="start" justify="between" gap="2">
              <Box style={{ minWidth: 0 }}>
                <Box style={{ marginBottom: 12, lineHeight: 1 }}>
                  <Text size="2" color="gray">{t("usage.tokenSection.totalHit")}</Text>
                </Box>
                <Box style={{ marginBottom: 8, lineHeight: 1 }}>
                    <Heading size="8" className="jc-num">
                      {isLoading ? "—" : animatedValue(displayedTotal)}
                    </Heading>
                </Box>

                <Text  size="1" color="gray">{hitRate}</Text>
              </Box>
              <Box
                style={{
                  width: 32, height: 32, borderRadius: 8,
                  background: "var(--amber-3)", color: "var(--amber-11)",
                  display: "flex", alignItems: "center", justifyContent: "center",
                  flexShrink: 0,
                }}
              >
                <Hash size={16} strokeWidth={1.8} />
              </Box>
            </Flex>
          </Card>
        </Tooltip>
      </Box>
    </>
  );
}
