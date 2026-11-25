import "../../personal-responsive.css";
import { useMemo, useState, type ReactNode } from "react";
import {
  AlertDialog,
  Badge,
  Box,
  Button,
  Card,
  Flex,
  Heading,
  IconButton,
  Progress,
  SegmentedControl,
  Select,
  Text,
  Tooltip as RTooltip,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { localeTag } from "../../i18n";
import {
  Area,
  Bar,
  CartesianGrid,
  ComposedChart,
  ResponsiveContainer,
  Tooltip as ChartTooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  Calendar,
  CalendarRange,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Clock,
  HelpCircle,
  KeyRound,
  MonitorSmartphone,
} from "lucide-react";
import LiveRateBar from "../../components/LiveRateBar";
import { UsageSubjectIcon, usageSubjectName } from "../../components/UsageSubject";
import { isWebToolEndpoint } from "../../api/webTools";
import { meApi, type BankedReset, type PlanUsage } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import CopyableId from "../../components/CopyableId";
import UsageFilterBar, { Field } from "../../components/UsageFilterBar";
import { last7DaysRange } from "../../components/UsageLogFilters";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { usePagedQuery } from "../../lib/paged";
import { decodeTps } from "../../lib/tps";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { PointAmount } from "../../components/PointAmount";
import { chartAxis, chartBarMaxSize, chartColors, chartGrid, chartTooltipProps } from "../../lib/chartTheme";
import { formatAmount } from "../../lib/currency";
import { formatDateTime } from "../../lib/formatTime";
import { useMediaQuery } from "../../hooks/useMediaQuery";
import {
  fmtResetAt,
  fmtResetIn,
} from "../../lib/quotaWindow";
import { useActiveOrg } from "../../hooks/useOrgContext";
import { useOrgScope } from "../../store/orgContext";
import { usageReasonColor, usageReasonLabel } from "../../lib/usageReason";
import { PageHeader } from "../../components/PageHeader";

function formatLatency(ms: number | null | undefined): string {
  if (ms == null || ms <= 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}
// Decode speed lives in src/lib/tps.ts so the window rule has a test; see
// that file for why the window starts at the upstream's first frame.
const outputTps = decodeTps;

function formatTPS(log: UsageLog): string {
  const tps = outputTps(log);
  if (tps == null) return "—";
  return `${Math.round(tps)} t/s`;
}

function formatCount(n: number | null | undefined): string {
  if (n == null) return "—";
  return n.toLocaleString();
}

// Long ids read as noise in a detail grid; keep the ends, copy the whole.
function shortId(id: string): string {
  return id.length > 22 ? `${id.slice(0, 14)}…${id.slice(-6)}` : id;
}

// Points in a dense table: four fixed decimals, so the column lines up; a
// non-zero cost that rounds away is still visible as a floor.
function formatPoints(v: string | number | null | undefined): string {
  if (v == null || v === "") return "—";
  const fixed = formatAmount(v, { decimals: 4 });
  if (fixed === "0.0000" && Number(v) > 0) return "<0.0001";
  return fixed;
}

function pct(used: string, quota: string): number {
  const u = parseFloat(used || "0");
  const q = parseFloat(quota || "0");
  if (q <= 0) return 0;
  return Math.min(100, (u / q) * 100);
}

function QuotaCard({
  label, used, quota, Icon, color, resetAt,
}: {
  label: string;
  used?: string;
  quota?: string;
  Icon: React.ComponentType<{ size?: number; strokeWidth?: number }>;
  color: "iris" | "purple" | "amber";
  // Drives the "reset in NhMm" / absolute timestamp tooltip line. The
  // backend counts each window from the plan's start_at.
  resetAt?: string;
}) {
  const { t } = useTranslation();
  const usedValue = used ?? "0";
  const quotaValue = quota ?? "0";
  const p = pct(usedValue, quotaValue);
  const tone = p >= 90 ? "red" : p >= 70 ? "amber" : color;
  return (
    <Card>
      <Flex align="center" justify="between" mb="1">
        <Text size="1" color="gray">
          {label}
        </Text>
        <Box style={{ color: `var(--${color}-11)`, display: "inline-flex" }}>
          <Icon size={16} strokeWidth={1.8} />
        </Box>
      </Flex>
      <Heading size="5" className="jc-num" style={{ lineHeight: 1.2 }}>
        <PointAmount amount={usedValue} iconSize={14} /> <Text size="2" color="gray">/ {parseFloat(quotaValue) > 0 ? <PointAmount amount={quotaValue} iconSize={12} /> : "∞"}</Text>
      </Heading>
      <Box mt="2">
        <Progress value={p} color={tone} />
      </Box>
      <Flex  justify="between" align="center" mt="1">
        <Text size="1" color="gray">
          {p.toFixed(1)}%
        </Text>
        {resetAt && (
          <RTooltip content={t("usage.page.quota.resetAt", { time: fmtResetAt(new Date(resetAt)) })}>
            <Text size="1" color="gray" style={{ cursor: "help" }}>
              {t("usage.page.quota.resetIn", { when: fmtResetIn(new Date(resetAt)) })}
            </Text>
          </RTooltip>
        )}
      </Flex>
    </Card>
  );
}

function SpendLimitCard({
  used, quota,
}: {
  used?: string;
  quota?: string;
}) {
  const { t } = useTranslation();
  const usedValue = used ?? "0";
  const quotaValue = quota ?? "0";
  const p = pct(usedValue, quotaValue);
  return (
    <Card>
      <Flex align="center" justify="between" mb="1">
        <Text size="1" color="gray">{t("usage.page.quota.spend30d")}</Text>
        <Box style={{ color: "var(--blue-11)", display: "inline-flex" }}>
          <CalendarRange size={16} strokeWidth={1.8} />
        </Box>
      </Flex>
      <Heading size="5" className="jc-num" style={{ lineHeight: 1.2 }}>
        <PointAmount amount={usedValue} iconSize={14} /> <Text size="2" color="gray">/ {parseFloat(quotaValue) > 0 ? <PointAmount amount={quotaValue} iconSize={12} /> : "∞"}</Text>
      </Heading>
      <Box mt="2">
        <Progress value={p} color={p >= 90 ? "red" : p >= 70 ? "amber" : "blue"} />
      </Box>
      <Text size="1" color="gray" mt="1" as="div">
        {p.toFixed(1)}%
      </Text>
    </Card>
  );
}

type UsageLog = {
  id: string;
  request_id?: string;
  created_at: string;
  endpoint: string;
  model: string;
  upstream_model?: string;
  tokens_in: number;
  tokens_out: number;
  cached_tokens_in?: number;
  cache_creation_tokens_in?: number;
  cache_creation_5m_tokens_in?: number;
  cache_creation_1h_tokens_in?: number;
  image_tokens_in?: number;
  image_tokens_out?: number;
  audio_tokens_in?: number;
  audio_tokens_out?: number;
  cache_write_estimated?: boolean;
  output_images?: number;
  cost_base?: string;
  cost_final: string;
  currency: string;
  billing_source: string;
  // 实际服务本次请求的模型分组；group_switches 是自动路由为了拿到可用渠道
  // 换组的次数（绝大多数请求为 0，字段缺省）。
  group_id?: string;
  group?: string;
  group_switches?: number;
  status: string;
  http_status?: number;
  reason_code?: string;
  reason?: string;
  latency_ms: number;
  ttft_ms?: number | null;
  ttfb_ms?: number | null;
  // When the model started producing output, and when the upstream's first
  // SSE frame arrived. Both are earlier than ttft_ms when a thinking phase
  // carried no visible text; see decodeTps in lib/tps.ts.
  first_output_ms?: number | null;
  first_frame_ms?: number | null;
  thinking_ms?: number | null;
  reasoning_ms?: number | null;
  ip?: string;
  user_agent?: string;
  reasoning_effort?: string;
  stream?: boolean;
  api_key_name?: string;
  api_key_id?: string;
  retry_count?: number;
  // Org-manager view only: which member produced the row.
  user_id?: string;
  user_email?: string;
};

function HeaderWithHelp({
  label,
  tip,
  align = "left",
}: {
  label: string;
  tip: string;
  align?: "left" | "center" | "right";
}) {
  const justify =
    align === "right" ? "end" : align === "center" ? "center" : "start";

  return (
    <RTooltip content={tip}>
      <Flex
        align="center"
        gap="1"
        justify={justify}
        style={{ width: "100%", minWidth: 0, cursor: "help" }}
      >
        <span>{label}</span>
        <HelpCircle size={12} style={{ opacity: 0.7 }} />
      </Flex>
    </RTooltip>
  );
}

function billingSourceLabel(t: TFunction, source: string | null | undefined): string {
  if (source === "plan" || source === "plan_balance" || source === "balance" || source === "none") {
    return t(`usage.page.billingSource.${source}`);
  }
  return source || "—";
}

// Status is one mark and one word; the long reason lives in the tooltip and
// the expanded row. Tone follows usageReasonColor: quota is a warning, user-
// caused outcomes are neutral, everything else that failed is red.
function StatusMark({ log }: { log: UsageLog }) {
  const color = usageReasonColor(log.reason_code, log.status);
  const tone = color === "green" ? "ok" : color === "orange" ? "warn" : color === "red" ? "err" : "muted";
  const label = usageReasonLabel(log.reason_code, log.status);
  const mark = (
    <span className="jc-status" data-tone={tone}>
      <span>{label}</span>
    </span>
  );
  return log.reason ? <RTooltip content={log.reason}>{mark}</RTooltip> : mark;
}

function formatCacheWriteTokens(log: Pick<UsageLog, "cache_creation_tokens_in" | "cache_creation_5m_tokens_in" | "cache_creation_1h_tokens_in" | "cache_write_estimated">) {
  const total = log.cache_creation_tokens_in ?? 0;
  if (!total) return "—";
  const prefix = log.cache_write_estimated ? "~" : "";
  const oneH = log.cache_creation_1h_tokens_in ?? 0;
  if (oneH > 0) {
    return `${prefix}${formatCount(log.cache_creation_5m_tokens_in ?? 0)}+${formatCount(oneH)}`;
  }
  return `${prefix}${formatCount(total)}`;
}

// 输入 / 输出 up front; cache traffic as a second muted line only when present.
function TokensCell({ log }: { log: UsageLog }) {
  const { t } = useTranslation();
  const cached = log.cached_tokens_in ?? 0;
  const written = log.cache_creation_tokens_in ?? 0;
  const media = (log.image_tokens_in || log.image_tokens_out || log.audio_tokens_in || log.audio_tokens_out) ? t("usage.page.tokensCell.media") : "";
  const sub = [
    cached > 0 ? t("usage.page.tokensCell.cached", { value: formatCount(cached) }) : "",
    written > 0 ? t("usage.page.tokensCell.written", { value: formatCacheWriteTokens(log) }) : "",
    media,
  ].filter(Boolean).join(" · ");
  return (
    <span className="jc-cell-num">
      {formatCount(log.tokens_in)} / {formatCount(log.tokens_out)}
      {sub && <span className="jc-cell-sub">{sub}</span>}
    </span>
  );
}

/* ---------- expanded row ---------- */

type DetailRow = { label: string; value: ReactNode; mono?: boolean; muted?: boolean };

function DetailGroup({ title, rows }: { title: string; rows: (DetailRow | null | false)[] }) {
  const items = rows.filter((r): r is DetailRow => !!r);
  if (items.length === 0) return null;
  return (
    <section className="jc-log-detail-group">
      <h4>{title}</h4>
      <dl>
        {items.map((r) => (
          <div className="jc-log-detail-row" key={r.label}>
            <dt>{r.label}</dt>
            <dd className={`${r.mono ? "is-mono" : ""} ${r.muted ? "is-muted" : ""}`.trim()}>{r.value}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

function UsageLogDetails({ log }: { log: UsageLog }) {
  const { t } = useTranslation();
  const tps = outputTps(log);
  const mediaIn = (log.image_tokens_in ?? 0) + (log.audio_tokens_in ?? 0);
  const mediaOut = (log.image_tokens_out ?? 0) + (log.audio_tokens_out ?? 0);
  return (
    <div className="jc-log-detail" onClick={(e) => e.stopPropagation()}>
      <DetailGroup
        title={t("usage.page.detail.request")}
        rows={[
          { label: t("usage.page.detail.requestId"), value: <CopyableId value={log.request_id || log.id} display={shortId(log.request_id || log.id)} size={12} color="gray" label={t("usage.page.detail.copyRequestId")} /> },
          { label: t("usage.page.detail.time"), value: formatDateTime(log.created_at, "full"), mono: true },
          { label: t("usage.page.detail.endpoint"), value: log.endpoint || "—", mono: true },
          { label: t("usage.page.detail.httpStatus"), value: log.http_status ?? "—", mono: true },
          { label: t("usage.page.detail.stream"), value: log.stream ? "SSE" : t("usage.page.detail.no") },
          log.reasoning_effort ? { label: t("usage.page.detail.effort"), value: log.reasoning_effort, mono: true } : null,
          (log.retry_count ?? 0) > 0 ? { label: t("usage.page.detail.retries"), value: log.retry_count, mono: true } : null,
          log.ip ? { label: "IP", value: log.ip, mono: true } : null,
        ]}
      />
      <DetailGroup
        title={isWebToolEndpoint(log.endpoint) ? t("usage.page.detail.toolBilling") : t("usage.page.detail.modelBilling")}
        rows={[
          isWebToolEndpoint(log.endpoint)
            ? { label: t("usage.page.detail.tool"), value: usageSubjectName(log.endpoint, log.model) }
            : { label: t("usage.page.detail.model"), value: log.model, mono: true },
          log.upstream_model && log.upstream_model !== log.model
            ? { label: isWebToolEndpoint(log.endpoint) ? t("usage.page.detail.vendor") : t("usage.page.detail.upstreamModel"), value: log.upstream_model, mono: true }
            : null,
          log.group ? { label: t("usage.page.detail.group"), value: log.group_switches ? t("usage.page.detail.groupSwitched", { group: log.group, count: log.group_switches }) : log.group } : null,
          { label: t("usage.page.detail.key"), value: log.api_key_name || t("usage.common.deviceLogin"), muted: !log.api_key_name },
          { label: t("usage.page.detail.billingSource"), value: billingSourceLabel(t, log.billing_source) },
          log.cost_base != null && log.cost_base !== log.cost_final ? { label: t("usage.page.detail.costBase"), value: t("usage.common.pointsValue", { value: formatPoints(log.cost_base) }), mono: true } : null,
          { label: t("usage.page.detail.costFinal"), value: t("usage.common.pointsValue", { value: formatPoints(log.cost_final) }), mono: true },
        ]}
      />
      <DetailGroup
        title="tokens"
        rows={[
          { label: t("usage.page.detail.input"), value: formatCount(log.tokens_in), mono: true },
          { label: t("usage.page.detail.output"), value: formatCount(log.tokens_out), mono: true },
          { label: t("usage.page.detail.cacheHit"), value: formatCount(log.cached_tokens_in ?? 0), mono: true },
          { label: log.cache_write_estimated ? t("usage.page.detail.cacheWriteEstimated") : t("usage.page.detail.cacheWrite"), value: formatCacheWriteTokens(log), mono: true },
          (log.cache_creation_1h_tokens_in ?? 0) > 0 ? { label: t("usage.page.detail.cacheWrite5m1h"), value: `${formatCount(log.cache_creation_5m_tokens_in ?? 0)} / ${formatCount(log.cache_creation_1h_tokens_in ?? 0)}`, mono: true } : null,
          mediaIn > 0 || mediaOut > 0 ? { label: t("usage.page.detail.imageIO"), value: `${formatCount(log.image_tokens_in ?? 0)} / ${formatCount(log.image_tokens_out ?? 0)}`, mono: true } : null,
          (log.audio_tokens_in ?? 0) > 0 || (log.audio_tokens_out ?? 0) > 0 ? { label: t("usage.page.detail.audioIO"), value: `${formatCount(log.audio_tokens_in ?? 0)} / ${formatCount(log.audio_tokens_out ?? 0)}`, mono: true } : null,
          (log.output_images ?? 0) > 0 ? { label: t("usage.page.detail.outputImages"), value: log.output_images, mono: true } : null,
        ]}
      />
      <DetailGroup
        title={t("usage.page.detail.latency")}
        rows={[
          { label: t("usage.page.detail.totalTime"), value: formatLatency(log.latency_ms), mono: true },
          { label: t("usage.page.detail.ttfb"), value: formatLatency(log.ttfb_ms ?? null), mono: true },
          { label: t("usage.page.detail.ttft"), value: formatLatency(log.ttft_ms ?? null), mono: true },
          (log.thinking_ms ?? log.reasoning_ms) ? { label: t("usage.page.detail.thinking"), value: formatLatency(log.thinking_ms ?? log.reasoning_ms ?? null), mono: true } : null,
          { label: t("usage.page.detail.speed"), value: tps == null ? "—" : `${Math.round(tps)} t/s`, mono: true },
        ]}
      />
      {log.status !== "ok" && (
        <p className="jc-log-detail-note">
          {usageReasonLabel(log.reason_code, log.status)}
          {log.reason ? t("usage.page.detail.reasonSuffix", { reason: log.reason }) : ""}
        </p>
      )}
    </div>
  );
}

/* ---------- mobile list ---------- */

function UsageLogCard({ log, expanded, onToggle }: { log: UsageLog; expanded: boolean; onToggle: () => void }) {
  return (
    <article className="jc-log-card">
      <button type="button" className="jc-log-card-head" aria-expanded={expanded} onClick={onToggle}>
        <UsageSubjectIcon endpoint={log.endpoint} model={log.model} size={16} />
        <span className="jc-log-card-model">{usageSubjectName(log.endpoint, log.model)}</span>
        <span className="jc-log-card-cost">{formatPoints(log.cost_final)}</span>
        <span className="jc-log-card-meta">
          <span className="jc-log-card-meta-row">
            <StatusMark log={log} />
            <span>{formatDateTime(log.created_at, "list-seconds")}</span>
          </span>
          <span className="jc-log-card-meta-row">
            <span>tokens {formatCount(log.tokens_in)} / {formatCount(log.tokens_out)}</span>
            <span>{formatLatency(log.latency_ms)}</span>
          </span>
        </span>
      </button>
      {expanded && <UsageLogDetails log={log} />}
    </article>
  );
}

function MobilePager({
  page, total, pageSize, hasPrev, hasNext, setPage, busy,
}: {
  page: number; total: number; pageSize: number; hasPrev: boolean; hasNext: boolean; setPage: (n: number) => void; busy?: boolean;
}) {
  const { t } = useTranslation();
  const start1 = total === 0 ? 0 : page * pageSize + 1;
  const end1 = Math.min((page + 1) * pageSize, total);
  return (
    <div className="jc-log-pager">
      <span className="jc-num">{total === 0 ? t("usage.page.pager.zero") : t("usage.page.pager.range", { start: start1, end: end1, total })}</span>
      <div>
        <IconButton size="1" variant="soft" color="gray" disabled={!hasPrev || busy} onClick={() => setPage(Math.max(0, page - 1))} aria-label={t("usage.page.pager.prev")}>
          <ChevronLeft size={14} />
        </IconButton>
        <span className="jc-num" style={{ padding: "0 6px" }}>{page + 1} / {Math.max(1, Math.ceil(total / pageSize))}</span>
        <IconButton size="1" variant="soft" color="gray" disabled={!hasNext || busy} onClick={() => setPage(page + 1)} aria-label={t("usage.page.pager.next")}>
          <ChevronRight size={14} />
        </IconButton>
      </div>
    </div>
  );
}

/* ============ 统计 tab ============ */

type StatsRange = "24h" | "7d" | "30d";

function rangeWindow(range: StatsRange): { start: string; end: string; bucket: "hour" | "day" } {
  const end = new Date();
  const hours = range === "24h" ? 24 : range === "7d" ? 7 * 24 : 30 * 24;
  const start = new Date(end.getTime() - hours * 3600 * 1000);
  return { start: start.toISOString(), end: end.toISOString(), bucket: range === "24h" ? "hour" : "day" };
}

function fmtBucketLabel(iso: string, bucket: "hour" | "day"): string {
  const d = new Date(iso);
  return bucket === "hour"
    ? d.toLocaleTimeString(localeTag(), { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString(localeTag(), { month: "2-digit", day: "2-digit" });
}

// SpendChart: per-bucket spend as bars, running total as a soft area on
// the secondary axis — 柱状 + 面积 in one panel without doubling chrome.
function SpendChart({ range, scope }: { range: StatsRange; scope: string }) {
  const { t } = useTranslation();
  const { data } = useQuery({
    queryKey: ["me-scoped", scope, "spend-series", range],
    queryFn: () => meApi.myStats(rangeWindow(range)),
    // A 24h/7d/30d spend curve does not visibly change inside a minute.
    refetchInterval: 60_000,
  });

  const { chart, total } = useMemo(() => {
    const bucket = range === "24h" ? "hour" as const : "day" as const;
    const chart = (data?.buckets ?? []).reduce<
      { label: string; spend: number; cumulative: number }[]
    >((rows, b) => {
      const spend = parseFloat(b.cost_final_sum) || 0;
      const cumulative = (rows.at(-1)?.cumulative ?? 0) + spend;
      rows.push({ label: fmtBucketLabel(b.time, bucket), spend, cumulative });
      return rows;
    }, []);
    return { chart, total: chart.at(-1)?.cumulative ?? 0 };
  }, [data, range]);

  return (
    <Card className="pn-card-flat jc-usage-panel">
      <Flex justify="between" align="start" mb="3" gap="2" wrap="wrap">
        <Box>
          <Heading size="4">{t("usage.page.spend.title")}</Heading>
          <Text size="1" color="gray" as="div">{range === "24h" ? t("usage.page.spend.subtitleHour") : t("usage.page.spend.subtitleDay")}</Text>
        </Box>
        <Text size="2" color="gray" className="jc-num">
          {t("usage.page.spend.total", { value: formatAmount(total) })}
        </Text>
      </Flex>
      {chart.length === 0 ? (
        <Box py="6" style={{ textAlign: "center" }}>
          <Text size="2" color="gray">{t("usage.page.spend.empty")}</Text>
        </Box>
      ) : (
        <Box style={{ width: "100%", height: 280 }}>
          <ResponsiveContainer>
            <ComposedChart data={chart} margin={{ top: 5, right: 0, bottom: 0, left: -10 }}>
              <defs>
                <linearGradient id="jcSpendCumFill" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor={chartColors.cyan} stopOpacity={0.28} />
                  <stop offset="100%" stopColor={chartColors.cyan} stopOpacity={0.02} />
                </linearGradient>
              </defs>
              <CartesianGrid {...chartGrid} />
              <XAxis dataKey="label" {...chartAxis} />
              <YAxis yAxisId="spend" {...chartAxis} tickFormatter={(v: number) => formatAmount(v)} />
              <YAxis yAxisId="cum" orientation="right" hide />
              <ChartTooltip
                {...chartTooltipProps}
                formatter={(v: unknown, name: unknown) => [
                  formatAmount(Number(v)),
                  name === "spend" ? t("usage.page.spend.spend") : t("usage.page.spend.cumulative"),
                ]}
              />
              <Area
                yAxisId="cum"
                type="monotone"
                dataKey="cumulative"
                stroke={chartColors.cyan}
                strokeWidth={1.5}
                fill="url(#jcSpendCumFill)"
                dot={false}
              />
              <Bar
                yAxisId="spend"
                dataKey="spend"
                fill={chartColors.indigo}
                maxBarSize={chartBarMaxSize}
                radius={[3, 3, 0, 0]}
              />
            </ComposedChart>
          </ResponsiveContainer>
        </Box>
      )}
    </Card>
  );
}

// ModelDistribution: per-model totals over the selected window as a
// ranked horizontal-bar list, switchable 按金额 / 按次数.
// A row of the distribution panel, already reduced to what it draws. `key` is
// the React/color identity, `label` the display text — they differ for API
// keys, where the identity is a UUID nobody wants to read.
type DistRow = { key: string; label: string; requests: number; cost: number; muted?: boolean; icon?: ReactNode; note?: string };

/** Which usage rows the caller made — grouped by model, or by the API key they authenticated with. */
type DistDimension = "model" | "api_key";

function ModelDistribution({ range, scope }: { range: StatsRange; scope: string }) {
  const { t } = useTranslation();
  const [dimension, setDimension] = useState<DistDimension>("model");
  const [metric, setMetric] = useState<"cost" | "requests">("cost");

  const byModel = useQuery({
    queryKey: ["me-scoped", scope, "model-dist", range],
    queryFn: () => meApi.myStatsByModel(rangeWindow(range)),
    // AggregateByModel has no hour-ring fast path — every miss is real SQL.
    refetchInterval: 60_000,
    enabled: dimension === "model",
  });
  const byKey = useQuery({
    queryKey: ["me-scoped", scope, "key-dist", range],
    queryFn: () => meApi.myStatsByKey(rangeWindow(range)),
    // Same story as by-model: no counter fast path, so poll no harder.
    refetchInterval: 60_000,
    enabled: dimension === "api_key",
  });

  const rows = useMemo<{ arr: DistRow[]; total: number }>(() => {
    let arr: DistRow[];
    if (dimension === "model") {
      // The by-model endpoint answers in (time, model) slices; the panel has
      // no time axis, so fold the buckets away.
      const map = new Map<string, { requests: number; cost: number }>();
      for (const r of byModel.data?.rows ?? []) {
        const cur = map.get(r.model) ?? { requests: 0, cost: 0 };
        cur.requests += r.requests;
        cur.cost += parseFloat(r.cost_final_sum) || 0;
        map.set(r.model, cur);
      }
      arr = Array.from(map, ([model, v]) => ({
        key: model,
        label: usageSubjectName(undefined, model),
        icon: <UsageSubjectIcon model={model} size={15} />,
        ...v,
      }));
    } else {
      // by-key arrives pre-grouped, one row per key. Empty api_key_id is the
      // bucket for calls that carried no key at all — CLI/Desktop sign in with
      // an OAuth device session, so those rows have no key to attribute to.
      arr = (byKey.data?.rows ?? []).map((r) => ({
        key: r.api_key_id || "(none)",
        label: r.api_key_id ? r.api_key_name || t("usage.page.dist.unnamed") : t("usage.common.deviceLogin"),
        muted: !r.api_key_id,
        // A revoked key keeps its history, so it can still top the list long
        // after it stopped working — say so rather than let it read as live.
        note: r.revoked ? t("usage.page.dist.revoked") : undefined,
        icon: r.api_key_id ? <KeyRound size={14} opacity={0.7} /> : <MonitorSmartphone size={14} opacity={0.7} />,
        requests: r.requests,
        cost: parseFloat(r.cost_final_sum) || 0,
      }));
    }
    arr.sort((a, b) => (metric === "cost" ? b.cost - a.cost : b.requests - a.requests));
    const total = arr.reduce((s, x) => s + (metric === "cost" ? x.cost : x.requests), 0);
    return { arr, total };
  }, [dimension, byModel.data, byKey.data, metric, t]);

  const loading = dimension === "model" ? byModel.isPending : byKey.isPending;

  return (
    <Card className="pn-card-flat jc-usage-panel">
      <Flex justify="between" align="start" mb="3" gap="2" wrap="wrap">
        <Box>
          <Heading size="4">{t("usage.page.dist.title")}</Heading>
          <Text size="1" color="gray" as="div">{metric === "cost" ? t("usage.page.dist.byCost") : t("usage.page.dist.byRequests")}</Text>
        </Box>
        <Flex gap="2" wrap="wrap">
          <SegmentedControl.Root value={dimension} onValueChange={(v) => setDimension(v as DistDimension)} size="1">
            <SegmentedControl.Item value="model">{t("usage.page.dist.model")}</SegmentedControl.Item>
            <SegmentedControl.Item value="api_key">API Key</SegmentedControl.Item>
          </SegmentedControl.Root>
          <SegmentedControl.Root value={metric} onValueChange={(v) => setMetric(v as "cost" | "requests")} size="1">
            <SegmentedControl.Item value="cost">{t("usage.page.dist.amount")}</SegmentedControl.Item>
            <SegmentedControl.Item value="requests">{t("usage.page.dist.count")}</SegmentedControl.Item>
          </SegmentedControl.Root>
        </Flex>
      </Flex>
      {rows.arr.length === 0 ? (
        <Box py="6" style={{ textAlign: "center" }}>
          <Text size="2" color="gray">{loading ? t("usage.page.dist.loading") : t("usage.page.dist.empty")}</Text>
        </Box>
      ) : (
        <Flex direction="column" gap="3" className="jc-model-dist-list">
          {rows.arr.map((r) => {
            const value = metric === "cost" ? r.cost : r.requests;
            const share = rows.total > 0 ? (value / rows.total) * 100 : 0;
            // A ranked list needs no color coding: the bar length is the
            // value. The no-key residual bucket is drawn in the neutral outline.
            const color = r.muted ? "var(--c-outline)" : "var(--c-primary)";
            return (
              <Box key={r.key} className="jc-model-dist-row">
                <Flex align="center" justify="between" gap="2" mb="1">
                  <Flex align="center" gap="2" style={{ minWidth: 0 }}>
                    {r.icon}
                    <Text size="2" className="jc-mono" color={r.muted ? "gray" : undefined} truncate>{r.label}</Text>
                    {r.note && <Badge size="1" variant="soft" color="gray" style={{ flexShrink: 0 }}>{r.note}</Badge>}
                  </Flex>
                  <Flex align="baseline" gap="2" style={{ flexShrink: 0 }}>
                    <Text size="2" className="jc-num">
                      {metric === "cost"
                        ? t("usage.common.pointsValue", { value: formatAmount(r.cost) })
                        : t("usage.page.dist.requests", { value: r.requests.toLocaleString() })}
                    </Text>
                    <Text size="1" color="gray" className="jc-num">{share.toFixed(1)}%</Text>
                  </Flex>
                </Flex>
                <div className="jc-model-dist-track">
                  <span style={{ width: `${Math.max(share, 1.5)}%`, background: color }} />
                </div>
              </Box>
            );
          })}
        </Flex>
      )}
    </Card>
  );
}

const bankedWindowKeys = {
  all: "usage.page.banked.windowAll",
  "5h": "usage.page.banked.window5h",
  weekly: "usage.page.banked.windowWeekly",
  monthly: "usage.page.banked.windowMonthly",
} as const satisfies Record<BankedReset["window"], string>;

// Resets an admin granted for the user to spend when they choose. Each window
// is one row; using one spends the oldest reset of that window.
function BankedResets({ resets }: { resets: BankedReset[] }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const use = useMutation({
    mutationFn: meApi.useBankedReset,
    onSettled: () => qc.invalidateQueries({ queryKey: ["me", "usage"] }),
  });
  const byWindow = resets.reduce<Partial<Record<BankedReset["window"], BankedReset[]>>>(
    (m, r) => ({ ...m, [r.window]: [...(m[r.window] ?? []), r] }), {});
  return (
    <Card>
      <Flex direction="column" gap="2">
        <Text size="2" weight="medium">{t("usage.page.banked.title")}</Text>
        <Text size="1" color="gray">{t("usage.page.banked.description")}</Text>
        {(Object.entries(byWindow) as [BankedReset["window"], BankedReset[]][]).map(([w, list]) => (
          <Flex key={w} align="center" justify="between" gap="2">
            <Text size="2">{t(bankedWindowKeys[w])} <Text color="gray">× {list.length}</Text></Text>
            <AlertDialog.Root>
              <AlertDialog.Trigger>
                <Button size="1" variant="soft" loading={use.isPending && use.variables === list[0].id} disabled={use.isPending}>{t("usage.page.banked.use")}</Button>
              </AlertDialog.Trigger>
              <AlertDialog.Content maxWidth="440px">
                <AlertDialog.Title>{t("usage.page.banked.confirmTitle")}</AlertDialog.Title>
                <AlertDialog.Description size="2" color="gray">
                  {w === "all" ? t("usage.page.banked.confirmAll") : t("usage.page.banked.confirmOne", { window: t(bankedWindowKeys[w]) })}
                </AlertDialog.Description>
                <Flex gap="2" justify="end" mt="4">
                  <AlertDialog.Cancel><Button variant="soft" color="gray">{t("usage.common.cancel")}</Button></AlertDialog.Cancel>
                  <AlertDialog.Action><Button disabled={use.isPending} onClick={() => use.mutate(list[0].id)}>{t("usage.page.banked.use")}</Button></AlertDialog.Action>
                </Flex>
              </AlertDialog.Content>
            </AlertDialog.Root>
          </Flex>
        ))}
        {use.isError && <Text size="1" color="red" role="alert">{errMsg(use.error)}</Text>}
      </Flex>
    </Card>
  );
}

function UsageStats({ plan, scope, range }: { plan?: PlanUsage; scope: string; range: StatsRange }) {
  const { t } = useTranslation();
  return (
    <Flex direction="column" gap="3">
      {plan?.has_active_plan && (
        <>
          <Flex align="center" gap="2" wrap="wrap">
            <Heading size="3" color="gray">{t("usage.page.stats.plan")}</Heading>
            <Badge variant="soft" color="iris">{plan.plan_name}</Badge>
            {plan.plan_type === "pay_as_you_go" && <Badge variant="soft" color="blue">{t("usage.page.stats.payg")}</Badge>}
            <Text size="1" color="gray">{t("usage.page.stats.until", { date: plan.expire_at ? formatDateTime(plan.expire_at, "full") : "—" })}</Text>
          </Flex>
          {plan.plan_type === "pay_as_you_go" ? (
            <Box className="jc-usage-quota-grid">
              <SpendLimitCard used={plan.monthly_spend_used} quota={plan.monthly_spend_limit} />
            </Box>
          ) : (
            <Box className="jc-usage-quota-grid">
              <QuotaCard label={t("usage.common.range5h")} used={plan.used_5h} quota={plan.quota_5h} Icon={Clock} color="iris" resetAt={plan.reset_5h_at} />
              <QuotaCard label={t("usage.common.range7d")} used={plan.used_weekly} quota={plan.quota_weekly} Icon={Calendar} color="purple" resetAt={plan.reset_weekly_at} />
              <QuotaCard label={t("usage.common.range30d")} used={plan.used_monthly} quota={plan.quota_monthly} Icon={CalendarRange} color="amber" resetAt={plan.reset_monthly_at} />
            </Box>
          )}
          {!!plan.banked_resets?.length && <BankedResets resets={plan.banked_resets} />}
        </>
      )}
      <div className="jc-usage-stats-grid" style={{ marginTop: 0 }}>
        <SpendChart range={range} scope={scope} />
        <ModelDistribution range={range} scope={scope} />
      </div>
    </Flex>
  );
}

export default function Usage() {
  const { t } = useTranslation();
  const scope = useOrgScope();
  const activeOrg = useActiveOrg();
  const managerView = !!activeOrg?.isManager;
  const isMobile = useMediaQuery("(max-width: 767px)");
  // Plan quota is a personal concept — hidden in org context (org spend
  // is bounded by org/member monthly limits instead).
  const { data, isLoading } = useQuery({
    queryKey: ["me", "usage"],
    queryFn: meApi.usage,
    // Live quota readout, deliberately uncached server-side (see handleUsage):
    // every poll is a real query. Quota moves far slower than a human reads it.
    refetchInterval: 20_000,
    enabled: !activeOrg,
  });
  // Member drill-down filter for the org owner/admin view.
  const [memberUserId, setMemberUserId] = useState("");
  const { data: orgDetail } = useQuery({
    queryKey: ["me", "organizations", activeOrg?.id ?? ""],
    queryFn: () => meApi.organizationDetail(activeOrg!.id),
    enabled: managerView,
  });
  // Both tabs open on the same window: the log lists the last 7 days, the
  // statistics aggregate them.
  const defaultRange = useMemo(() => last7DaysRange(), []);
  const [tab, setTab] = useState<"stats" | "logs">("logs");
  const [statsRange, setStatsRange] = useState<StatsRange>("7d");

  // Filter state. The range picker stores local datetimes; they become
  // RFC3339 only when shipped to the API. Clearing a filter means "any".
  const [endpoint, setEndpoint] = useState("");
  const [billingSource, setBillingSource] = useState("");
  const [status, setStatus] = useState("");
  const [model, setModel] = useState("");
  const [ip, setIp] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [start, setStart] = useState(defaultRange.start);
  const [end, setEnd] = useState(defaultRange.end);
  const [expandedRows, setExpandedRows] = useState<Set<string>>(
    () => new Set(),
  );

  const filtersDirty =
    !!(endpoint || billingSource || status || model || ip || apiKey || memberUserId) ||
    start !== defaultRange.start || end !== defaultRange.end;

  const resetFilters = () => {
    setEndpoint(""); setBillingSource(""); setStatus(""); setModel(""); setIp(""); setApiKey("");
    setMemberUserId("");
    const fresh = last7DaysRange();
    setStart(fresh.start); setEnd(fresh.end);
  };

  const toggleExpanded = (id: string) => {
    setExpandedRows((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const paged = usePagedQuery<UsageLog>({
    queryKey: ["me-scoped", scope, "usage-logs"],
    queryFn: (params) =>
      meApi.myUsageLogsPaged({
        ...params,
        endpoint: endpoint || undefined,
        billing_source: billingSource || undefined,
        status: status || undefined,
        model: model || undefined,
        ip: ip || undefined,
        api_key: apiKey || undefined,
        member_user_id: managerView && memberUserId ? memberUserId : undefined,
        start: start ? new Date(start).toISOString() : undefined,
        end: end ? new Date(end).toISOString() : undefined,
      }),
    extras: [endpoint, billingSource, status, model, ip, apiKey, start, end, memberUserId],
    pageSize: 20,
    refetchInterval: tab === "logs" ? 20_000 : false,
    enabled: tab === "logs",
  });

  const columns: Column<UsageLog>[] = [
    {
      header: t("usage.page.columns.time"),
      minWidth: "156px",
      cell: (l) => {
        const expanded = expandedRows.has(l.id);
        return (
          <span className="jc-cell-time jc-cell-mono">
            <IconButton
              size="1"
              variant="ghost"
              color="gray"
              aria-label={expanded ? t("usage.page.columns.collapse") : t("usage.page.columns.expand")}
              aria-expanded={expanded}
              onClick={(event) => {
                event.stopPropagation();
                toggleExpanded(l.id);
              }}
            >
              {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
            </IconButton>
            {formatDateTime(l.created_at, "list-seconds")}
          </span>
        );
      },
    },
    {
      header: t("usage.page.columns.status"),
      minWidth: "96px",
      cell: (l) => <StatusMark log={l} />,
    },
    ...(managerView ? [{
      header: t("usage.page.columns.member"),
      minWidth: "140px",
      cell: (l: UsageLog) =>
        l.user_email || l.user_id ? (
          <RTooltip content={l.user_id ?? ""}>
            <Text size="2" truncate style={{ cursor: "help", display: "block", maxWidth: 200 }}>
              {l.user_email || l.user_id}
            </Text>
          </RTooltip>
        ) : (
          <Text size="2" color="gray">—</Text>
        ),
    } satisfies Column<UsageLog>] : []),
    {
      header: t("usage.page.columns.model"),
      minWidth: "190px",
      cell: (l) => (
        <span className="jc-cell-model">
          <UsageSubjectIcon endpoint={l.endpoint} model={l.model} size={15} />
          <span className="jc-cell-mono">{usageSubjectName(l.endpoint, l.model)}</span>
        </span>
      ),
    },
    {
      header: t("usage.page.columns.key"),
      minWidth: "120px",
      cell: (l) =>
        l.api_key_name ? (
          <Text size="2" truncate style={{ display: "block", maxWidth: 180 }} title={l.api_key_name}>
            {l.api_key_name}
          </Text>
        ) : (
          <Text size="2" color="gray">{t("usage.common.deviceLogin")}</Text>
        ),
    },
    {
      header: <HeaderWithHelp label={t("usage.page.columns.tokensIO")} tip={t("usage.page.columns.tokensTip")} align="right" />,
      minWidth: "150px",
      align: "right",
      cell: (l) => <TokensCell log={l} />,
    },
    {
      header: t("usage.page.columns.cost"),
      minWidth: "108px",
      align: "right",
      cell: (l) => <span className="jc-cell-num">{formatPoints(l.cost_final)}</span>,
    },
    {
      header: t("usage.page.columns.duration"),
      minWidth: "84px",
      align: "right",
      cell: (l) => <span className="jc-cell-num">{formatLatency(l.latency_ms)}</span>,
    },
    {
      header: <HeaderWithHelp label={t("usage.page.columns.ttft")} tip={t("usage.page.columns.ttftTip")} align="right" />,
      minWidth: "84px",
      align: "right",
      cell: (l) => <span className="jc-cell-num">{formatLatency(l.ttft_ms ?? null)}</span>,
    },
    {
      header: <HeaderWithHelp label="TPS" tip={t("usage.page.columns.tpsTip")} align="right" />,
      minWidth: "84px",
      align: "right",
      cell: (l) => <span className="jc-cell-num">{formatTPS(l)}</span>,
    },
  ];

  if (isLoading) return <RouteSkeleton />;

  const memberFilter = managerView ? (
    <Field label={t("usage.page.columns.member")} full>
      <Select.Root
        value={memberUserId || "all"}
        onValueChange={(v) => setMemberUserId(v === "all" ? "" : v)}
      >
        <Select.Trigger variant="soft" color="gray" className="jc-filter-select is-full" />
        <Select.Content>
          <Select.Item value="all">{t("usage.page.allMembers")}</Select.Item>
          {(orgDetail?.members ?? []).map((m) => (
            <Select.Item key={m.user_id} value={m.user_id}>
              {m.email || m.nickname || m.user_id}
            </Select.Item>
          ))}
        </Select.Content>
      </Select.Root>
    </Field>
  ) : null;

  const filterBar = (
    <UsageFilterBar
      start={start} setStart={setStart}
      end={end} setEnd={setEnd}
      model={model} setModel={setModel}
      status={status} setStatus={setStatus}
      endpoint={endpoint} setEndpoint={setEndpoint}
      billingSource={billingSource} setBillingSource={setBillingSource}
      apiKey={apiKey} setApiKey={setApiKey}
      ip={ip} setIp={setIp}
      extra={memberFilter}
      extraApplied={!!memberUserId}
      dirty={filtersDirty}
      onReset={resetFilters}
    />
  );

  return (
    <Box className="jc-ledger jc-usage-ledger">
      <PageHeader
        title={t("usage.page.title")}
        description={activeOrg
          ? managerView
            ? t("usage.page.descOrgManager", { org: activeOrg.org?.name ?? t("usage.page.orgFallback") })
            : t("usage.page.descOrgMember", { org: activeOrg.org?.name ?? t("usage.page.orgFallback") })
          : t("usage.page.descPersonal")}
      />

      <div className="jc-ledger-tabs">
        <SegmentedControl.Root value={tab} onValueChange={(v) => setTab(v as "stats" | "logs")} size="2">
          <SegmentedControl.Item value="logs">{t("usage.page.tabLogs")}</SegmentedControl.Item>
          <SegmentedControl.Item value="stats">{t("usage.page.tabStats")}</SegmentedControl.Item>
        </SegmentedControl.Root>
        {tab === "stats" && (
          <SegmentedControl.Root value={statsRange} onValueChange={(v) => setStatsRange(v as StatsRange)} size="1">
            <SegmentedControl.Item value="24h">{t("usage.common.range24h")}</SegmentedControl.Item>
            <SegmentedControl.Item value="7d">{t("usage.common.range7d")}</SegmentedControl.Item>
            <SegmentedControl.Item value="30d">{t("usage.common.range30d")}</SegmentedControl.Item>
          </SegmentedControl.Root>
        )}
      </div>

      {tab === "stats" && <UsageStats plan={activeOrg ? undefined : data} scope={scope} range={statsRange} />}

      {tab === "logs" && !isMobile && (
        <DataTable<UsageLog>
          {...paged}
          isLoading={paged.query.isLoading}
          isFetching={paged.query.isFetching}
          refetch={() => paged.query.refetch()}
          columns={columns}
          rowKey={(l) => l.id}
          emptyText={t("usage.page.empty")}
          renderExpandedRow={(l) => <UsageLogDetails log={l} />}
          isRowExpanded={(l) => expandedRows.has(l.id)}
          onRowClick={(l) => toggleExpanded(l.id)}
          filters={filterBar}
          actions={<LiveRateBar variant="inline" queryKey={["me-scoped", scope]} fetcher={meApi.myRate} />}
        />
      )}

      {tab === "logs" && isMobile && (
        <Flex direction="column" gap="3">
          {filterBar}
          {paged.items.length === 0 && !paged.query.isLoading ? (
            <div className="jc-log-empty">{t("usage.page.empty")}</div>
          ) : (
            <div className="jc-log-cards">
              {paged.items.map((log) => (
                <UsageLogCard
                  key={log.id}
                  log={log}
                  expanded={expandedRows.has(log.id)}
                  onToggle={() => toggleExpanded(log.id)}
                />
              ))}
            </div>
          )}
          <MobilePager
            page={paged.page}
            total={paged.total}
            pageSize={paged.pageSize}
            hasPrev={paged.hasPrev}
            hasNext={paged.hasNext}
            setPage={paged.setPage}
            busy={paged.query.isFetching}
          />
        </Flex>
      )}
    </Box>
  );
}
