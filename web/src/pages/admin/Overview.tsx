import { useEffect, useRef, useState } from "react";
import { Box, Tooltip } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { Gauge } from "lucide-react";
import { adminApi } from "../../api/endpoints";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import TokenUsageSection from "../../components/TokenUsageSection";
import { formatAmount } from "../../lib/currency";
import { AdminMetricCard, AdminMetricGrid, AdminPage, AdminSection } from "../../components/AdminPage";

type Tone = "iris" | "green" | "red" | "blue" | "amber" | "gray" | "purple";
function RatioLine({
  label,
  value,
  tone = "green",
}: {
  label: string;
  value: number;
  tone?: Tone;
}) {
  const pct = clampPercent(value);

  return (
    <div className="jc-overview-ratio" data-tone={tone}>
      <div className="jc-overview-ratio-meta">
        <span>{label}</span>
        <span className="jc-num">{formatPercent(pct)}</span>
      </div>
      <div className="jc-overview-ratio-track" aria-hidden="true">
        <span style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

export default function Overview() {
  const { data } = useQuery({
    queryKey: ["admin", "overview"],
    queryFn: adminApi.overview,
    refetchInterval: 2_000,
  });

  if (!data) return <RouteSkeleton />;

  const bannedRate = percentOf(data.users.banned, data.users.total);
  // Usage-based activity: DAU/MAU count distinct users who made an LLM API
  // call in the window (from usage_logs), not the status flag.
  const dauRate = percentOf(data.users.dau, data.users.total);
  const stickiness = percentOf(data.users.dau, data.users.mau); // DAU / MAU
  const errorRate = percentOf(data.today.errors, data.today.requests);

  return (
    <AdminPage
      title="看板"
      description="核心运营指标实时刷新，每 2 秒更新一次。"
    >
      <Box className="jc-overview-page">
      <AdminMetricGrid>
        <AdminMetricCard
          label="今日请求"
          value={<AnimatedMetricValue value={data.today.requests} formatter={formatCount} />}
          detail="2 秒刷新"
          tone="iris"
        />
        <AdminMetricCard
          label="今日金额"
          value={
            <AnimatedMetricValue
              value={Number(data.today.cost_usd) || 0}
              formatter={(v) => formatAmount(v)}
            />
          }
          detail="平台积分"
          tone="amber"
        />
        <AdminMetricCard
          label="今日 Tokens"
          value={<AnimatedMetricValue value={data.today.tokens} formatter={formatCount} />}
          detail="input + output"
          tone="purple"
        />
        <AdminMetricCard
          label="今日错误"
          value={<AnimatedMetricValue value={data.today.errors} formatter={formatCount} />}
          detail={formatPercent(errorRate)}
          tone={errorRate > 1 ? "red" : "green"}
        />
      </AdminMetricGrid>

      <AdminSection title="今日健康度" description="请求延迟与错误率用于判断网关实时状态。">
        <AdminMetricGrid>
          <AdminMetricCard label="平均延迟" value={`${formatCount(data.today.avg_latency_ms)} ms`} detail="今日请求" tone="blue" />
          <AdminMetricCard label="错误率" value={formatPercent(errorRate)} detail="errors / requests" tone={errorRate > 5 ? "red" : errorRate > 1 ? "amber" : "green"} />
          <AdminMetricCard label="今日活跃占比" value={formatPercent(dauRate)} detail="DAU / total" tone="green" />
        </AdminMetricGrid>
          <RatioLine
            label="错误率"
            value={errorRate}
            tone={errorRate > 5 ? "red" : errorRate > 1 ? "amber" : "green"}
          />
      </AdminSection>

      <TokenUsageSection
        queryKey={["admin", "overview"]}
        fetcher={(p) => adminApi.statsUsage(p)}
        fullWidthCards
      />

      <AdminSection title="用户" description="活跃度按 LLM API 调用统计，去重用户数。">
        <AdminMetricGrid>
          <AdminMetricCard label="总数" value={formatCount(data.users.total)} tone="iris" />
          <AdminMetricCard label="日活 DAU" value={formatCount(data.users.dau)} detail={`${formatPercent(dauRate)} · 今日调用`} tone="green" />
          <AdminMetricCard label="月活 MAU" value={formatCount(data.users.mau)} detail="近 30 天调用" tone="blue" />
          <AdminMetricCard label="封禁" value={formatCount(data.users.banned)} detail={formatPercent(bannedRate)} tone="red" />
        </AdminMetricGrid>
        <RatioLine label="用户粘性 DAU / MAU" value={stickiness} tone="green" />
      </AdminSection>

      <WebVitalsSection />
      </Box>
    </AdminPage>
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

// Compact Core Web Vitals — p75 + good-rate per metric, sampled from
// real users over the last 7 days. Web-vitals library posts to
// /v1/public/web-vitals on pagehide, aggregated server-side with
// percentile_cont.
function WebVitalsSection() {
  const { data } = useQuery({
    queryKey: ["admin", "web-vitals", 7],
    queryFn: () => adminApi.webVitals(7),
    refetchInterval: 60_000,
  });

  // P75 thresholds per Core Web Vitals spec.
  const thresholds: Record<string, { good: number; needs: number; unit: string; label: string }> = {
    LCP:  { good: 2500, needs: 4000, unit: "ms", label: "LCP" },
    FCP:  { good: 1800, needs: 3000, unit: "ms", label: "FCP" },
    INP:  { good: 200,  needs: 500,  unit: "ms", label: "INP" },
    TTFB: { good: 800,  needs: 1800, unit: "ms", label: "TTFB" },
    CLS:  { good: 0.1,  needs: 0.25, unit: "",   label: "CLS" },
  };
  const order = ["LCP", "INP", "CLS", "FCP", "TTFB"];

  type Row = { name: string; count: number; p50: number; p75: number; p95: number; good_pct: number };
  const rowByName: Record<string, Row> = {};
  for (const r of data?.rows ?? []) rowByName[r.name] = r;
  const sampleCount = data?.rows.reduce((s, r) => s + r.count, 0) ?? 0;

  return (
    <AdminSection
      title="Core Web Vitals · 近 7 天"
      description={sampleCount > 0 ? `共 ${formatCount(sampleCount)} 个真实用户样本` : "等待真实用户访问后采集"}
      actions={<Gauge size={16} strokeWidth={1.8} />}
    >
      <AdminMetricGrid>
        {order.map((name) => {
          const r = rowByName[name];
          const t = thresholds[name];
          if (!t) return null;
          const p75 = r?.p75 ?? 0;
          // Color by p75 vs thresholds.
          const color: "green" | "amber" | "red" | "gray" =
            !r || r.count === 0
              ? "gray"
              : p75 <= t.good
                ? "green"
                : p75 <= t.needs
                  ? "amber"
                  : "red";
          const display =
            !r || r.count === 0
              ? "—"
              : t.unit === "ms"
                ? `${Math.round(p75)} ${t.unit}`
                : p75.toFixed(2);
          return (
            <Tooltip
              key={name}
              content={
                r && r.count > 0
                  ? `n=${r.count} · p50 ${formatVal(r.p50, t.unit)} · p75 ${formatVal(r.p75, t.unit)} · p95 ${formatVal(r.p95, t.unit)} · ${r.good_pct.toFixed(0)}% good`
                  : "等待真实用户访问后采集"
              }
            >
              <Box>
                <AdminMetricCard
                  label={`${t.label} p75`}
                  value={display}
                  detail={r && r.count > 0 ? `${r.good_pct.toFixed(0)}% good` : "无样本"}
                  tone={color}
                />
              </Box>
            </Tooltip>
          );
        })}
      </AdminMetricGrid>
    </AdminSection>
  );
}

function formatVal(v: number, unit: string): string {
  if (unit === "ms") return `${Math.round(v)}ms`;
  return v.toFixed(3);
}

function formatCount(n: number): string {
  return Math.round(n).toLocaleString();
}

function percentOf(part: number, total: number): number {
  if (!Number.isFinite(part) || !Number.isFinite(total) || total <= 0) return 0;
  return (part / total) * 100;
}

function clampPercent(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.min(100, Math.max(0, value));
}

function formatPercent(value: number): string {
  const pct = clampPercent(value);
  const decimals = pct > 0 && pct < 100 && !Number.isInteger(pct) ? 1 : 0;
  return `${pct.toFixed(decimals)}%`;
}
