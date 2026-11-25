// 性能观测 — per-channel / group / account / model performance over a rolling
// window, plus each row's live RPM / TPM.
//
// Reads the three /v1/admin/observability endpoints (series, leaderboard,
// sla). Data notes for whoever changes this page:
//
// - The series is a fixed 48-point grid, zero-filled. Latency/rate fields
//   are null (not 0) in buckets with no sample, and the charts render those
//   as gaps (connectNulls={false}) rather than a dip to zero.
// - 业务可用率 (SLA section) and 原始失败率 (charts) answer different
//   questions: the raw failure rate counts every failed request, while the
//   SLA excludes synthetic probe traffic and caller-side errors before
//   grading against the same 98/90/80 thresholds as the public /status page.
// - 包含探测流量 is off by default so the numbers reflect real user traffic.
//
// dim / entity id / window are synced to the URL so rows here and the
// provider table can deep-link into a specific view.
import { useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  Badge, Box, Callout, Card, Flex, Heading, SegmentedControl, Select, Switch, Table, Text, Tooltip,
} from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle } from "lucide-react";
import {
  Bar, CartesianGrid, ComposedChart, Line, ResponsiveContainer, Tooltip as RTooltip, XAxis, YAxis,
} from "recharts";
import {
  adminApi,
  type ObservabilityDim,
  type ObservabilityErrorWindow,
  type ObservabilityPoint,
  type ObservabilityLeaderboardRow,
  type ObservabilitySlaGroup,
  type ObservabilitySlaModel,
  type ObservabilitySlaWindow,
  type ObservabilityWindow,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { AdminPage } from "../../components/AdminPage";
import LiveRateBar from "../../components/LiveRateBar";
import AvailabilityDots, { stateLabel, stateTone, type AvailabilityTone } from "../../components/AvailabilityDots";
import { chartAxis, chartBarMaxSize, chartColors, chartGrid, chartTooltipProps } from "../../lib/chartTheme";
import { errorClassColor, errorClassLabel } from "../../lib/errorClass";

const DIMS: { value: ObservabilityDim; label: string }[] = [
  { value: "platform", label: "全平台" },
  { value: "provider", label: "渠道" },
  { value: "account", label: "账号" },
  { value: "model", label: "模型" },
  { value: "group", label: "分组" },
];
const WINDOWS: { value: ObservabilityWindow; label: string; ms: number }[] = [
  { value: "1h", label: "1小时", ms: 3600_000 },
  { value: "6h", label: "6小时", ms: 6 * 3600_000 },
  { value: "24h", label: "24小时", ms: 24 * 3600_000 },
];
const SLA_WINDOWS: { value: ObservabilitySlaWindow; label: string }[] = [
  { value: "24h", label: "24小时" },
  { value: "7d", label: "7天" },
  { value: "30d", label: "30天" },
];

// Below this many requests in the whole window, rates are noise: show a
// 样本少 hint and keep the readout gray instead of alarming red.
const LOW_SAMPLE_REQUESTS = 50;

function isDim(v: string | null): v is ObservabilityDim {
  return v === "platform" || v === "provider" || v === "group" || v === "account" || v === "model";
}
function isWindow(v: string | null): v is ObservabilityWindow {
  return v === "1h" || v === "6h" || v === "24h";
}

// The URL is the single source of truth for dim / entity / window, so a
// pasted link reproduces the exact view. Entity ids live under the param
// name the backend expects (provider_id / group_id / account_id / model).
function entityParamName(dim: ObservabilityDim): "provider_id" | "group_id" | "account_id" | "model" | null {
  if (dim === "provider") return "provider_id";
  if (dim === "group") return "group_id";
  if (dim === "account") return "account_id";
  if (dim === "model") return "model";
  return null;
}

// A leaderboard row's identity lives under the dim-specific field
// (provider_id / group_id / account_id / model) — there is no generic `id`.
function rowEntityId(dim: Exclude<ObservabilityDim, "platform">, row: ObservabilityLeaderboardRow): string {
  if (dim === "provider") return row.provider_id ?? "";
  if (dim === "group") return row.group_id ?? "";
  if (dim === "account") return row.account_id ?? "";
  return row.model ?? "";
}

export default function Observability() {
  const [searchParams, setSearchParams] = useSearchParams();

  const dim: ObservabilityDim = isDim(searchParams.get("dim")) ? (searchParams.get("dim") as ObservabilityDim) : "platform";
  const window_: ObservabilityWindow = isWindow(searchParams.get("window")) ? (searchParams.get("window") as ObservabilityWindow) : "6h";
  const entityParam = entityParamName(dim);
  const entityId = entityParam ? (searchParams.get(entityParam) ?? "") : "";
  const includeSynthetic = searchParams.get("synthetic") === "1";

  function updateParams(next: { dim?: ObservabilityDim; entityId?: string; window?: ObservabilityWindow; synthetic?: boolean }) {
    const p = new URLSearchParams(searchParams);
    const nextDim = next.dim ?? dim;
    p.set("dim", nextDim);
    p.set("window", next.window ?? window_);
    // Only one entity key survives a dim switch; stale ids from the old
    // dimension would silently scope the next query.
    p.delete("provider_id"); p.delete("group_id"); p.delete("account_id"); p.delete("model");
    const key = entityParamName(nextDim);
    const id = next.entityId !== undefined ? next.entityId : (next.dim !== undefined && next.dim !== dim ? "" : entityId);
    if (key && id) p.set(key, id);
    const syn = next.synthetic ?? includeSynthetic;
    if (syn) p.set("synthetic", "1"); else p.delete("synthetic");
    setSearchParams(p, { replace: true });
  }

  // Entity option lists — only fetched for the dimension in use.
  const providersQ = useQuery({
    queryKey: ["admin", "providers", "all"],
    queryFn: adminApi.providers,
    enabled: dim === "provider",
    staleTime: 60_000,
  });
  const groupsQ = useQuery({
    queryKey: ["admin", "groups", "all"],
    queryFn: adminApi.groups,
    enabled: dim === "group",
    staleTime: 60_000,
  });
  const accountsQ = useQuery({
    queryKey: ["admin", "accounts", "topology-all"],
    queryFn: () => adminApi.accounts({ limit: 1000 }),
    enabled: dim === "account",
    staleTime: 60_000,
  });
  const modelsQ = useQuery({
    queryKey: ["admin", "models", "all"],
    queryFn: adminApi.models,
    enabled: dim === "model",
    staleTime: 60_000,
  });

  const entityOptions: { id: string; name: string }[] = useMemo(() => {
    if (dim === "provider") return (providersQ.data ?? []).map((p) => ({ id: p.id, name: p.name }));
    if (dim === "group") return (groupsQ.data ?? []).map((g) => ({ id: g.id, name: g.name }));
    if (dim === "account") return (accountsQ.data ?? []).map((a) => ({ id: a.id, name: a.name }));
    // The series endpoint keys models by name, not by id.
    if (dim === "model") return (modelsQ.data ?? []).map((m) => ({ id: m.name, name: m.name }));
    return [];
  }, [dim, providersQ.data, groupsQ.data, accountsQ.data, modelsQ.data]);

  const needsEntity = dim !== "platform";
  const seriesEnabled = !needsEntity || !!entityId;

  const seriesQ = useQuery({
    queryKey: ["admin", "observability", "series", dim, entityId, window_, includeSynthetic],
    queryFn: () =>
      adminApi.observabilitySeries({
        dim,
        window: window_,
        include_synthetic: includeSynthetic,
        provider_id: dim === "provider" ? entityId : undefined,
        group_id: dim === "group" ? entityId : undefined,
        account_id: dim === "account" ? entityId : undefined,
        model: dim === "model" ? entityId : undefined,
      }),
    enabled: seriesEnabled,
    refetchInterval: 60_000,
  });

  const lbDim: Exclude<ObservabilityDim, "platform"> = dim === "platform" ? "provider" : dim;
  const [lbDimOverride, setLbDimOverride] = useState<Exclude<ObservabilityDim, "platform"> | null>(null);
  const leaderboardDim = lbDimOverride ?? lbDim;
  const leaderboardQ = useQuery({
    queryKey: ["admin", "observability", "leaderboard", leaderboardDim, window_, includeSynthetic],
    queryFn: () =>
      adminApi.observabilityLeaderboard({ dim: leaderboardDim, window: window_, include_synthetic: includeSynthetic }),
    refetchInterval: 60_000,
  });

  const [slaWindow, setSlaWindow] = useState<ObservabilitySlaWindow>("24h");
  const slaQ = useQuery({
    queryKey: ["admin", "observability", "sla", slaWindow],
    queryFn: () => adminApi.observabilitySla({ window: slaWindow }),
    refetchInterval: 60_000,
  });

  const points = useMemo(() => seriesQ.data?.points ?? [], [seriesQ.data]);
  const agg = useMemo(() => aggregate(points), [points]);
  const lowSamples = agg.requests > 0 && agg.requests < LOW_SAMPLE_REQUESTS;
  // Anchor the logs deep-link window to when the leaderboard was fetched
  // (render must stay pure, so no Date.now() here). 0 = no data yet.
  const windowMs = WINDOWS.find((w) => w.value === window_)?.ms ?? 6 * 3600_000;
  const windowStartIso = leaderboardQ.dataUpdatedAt > 0
    ? new Date(leaderboardQ.dataUpdatedAt - windowMs).toISOString()
    : "";

  const chartData = useMemo(
    () =>
      points.map((p) => ({
        label: shortTime(p.time),
        p50: p.p50_ttft_ms,
        p95: p.p95_ttft_ms,
        avg: p.avg_ttft_ms,
        tps: p.avg_tps,
        // A bucket with no requests has no failure rate; null breaks the
        // line instead of drawing a reassuring 0%.
        failRate: p.requests > 0 ? p.failure_rate : null,
        pfRate: p.provider_fault_rate,
        anomalies: p.anomalies,
        requests: p.requests,
      })),
    [points],
  );

  return (
    <AdminPage
      title="性能观测"
      description="按渠道、账号、模型、分组看实时 RPM/TPM、请求数、失败率、首字速度、生成速度和业务可用率。可用率不含探测请求。"
    >
      <LiveRateBar queryKey={["admin"]} fetcher={adminApi.statsRate} title="平台当前速率" />

      {/* Control bar */}
      <Card mb="3">
        <Flex gap="4" align="end" wrap="wrap">
          <Box>
            <Text size="1" color="gray" as="p" mb="1">维度</Text>
            <SegmentedControl.Root size="1" value={dim} onValueChange={(v) => updateParams({ dim: v as ObservabilityDim, entityId: "" })}>
              {DIMS.map((d) => (
                <SegmentedControl.Item key={d.value} value={d.value}>{d.label}</SegmentedControl.Item>
              ))}
            </SegmentedControl.Root>
          </Box>
          {needsEntity && (
            <Box style={{ minWidth: 220 }}>
              <Text size="1" color="gray" as="p" mb="1">{dimLabel(dim)}</Text>
              <Select.Root value={entityId || "__none"} onValueChange={(v) => updateParams({ entityId: v === "__none" ? "" : v })}>
                <Select.Trigger placeholder={`选择${dimLabel(dim)}`} style={{ width: "100%" }} />
                <Select.Content>
                  <Select.Item value="__none">（未选择）</Select.Item>
                  {entityOptions.map((o) => (
                    <Select.Item key={o.id} value={o.id}>{o.name}</Select.Item>
                  ))}
                </Select.Content>
              </Select.Root>
            </Box>
          )}
          <Box>
            <Text size="1" color="gray" as="p" mb="1">窗口</Text>
            <SegmentedControl.Root size="1" value={window_} onValueChange={(v) => updateParams({ window: v as ObservabilityWindow })}>
              {WINDOWS.map((w) => (
                <SegmentedControl.Item key={w.value} value={w.value}>{w.label}</SegmentedControl.Item>
              ))}
            </SegmentedControl.Root>
          </Box>
          <Flex align="center" gap="2" pb="1">
            <Switch
              id="obs-synthetic"
              size="1"
              checked={includeSynthetic}
              onCheckedChange={(v) => updateParams({ synthetic: v })}
            />
            <Text as="label" htmlFor="obs-synthetic" size="1" color="gray" style={{ cursor: "pointer" }}>
              包含探测流量
            </Text>
          </Flex>
          {lowSamples && (
            <Badge size="1" variant="soft" color="gray">样本少 · 窗口内仅 {agg.requests} 次请求</Badge>
          )}
        </Flex>
      </Card>

      {seriesQ.isError && (
        <Callout.Root color="red" mb="3">
          <Callout.Icon><AlertCircle size={15} /></Callout.Icon>
          <Callout.Text>
            性能数据加载失败：{errMsg(seriesQ.error)}。请确认后端已部署性能观测接口。
          </Callout.Text>
        </Callout.Root>
      )}

      {needsEntity && !entityId && !seriesQ.isError && (
        <Card mb="3">
          <Flex align="center" justify="center" py="6">
            <Text size="2" color="gray">请先选择要查看的{dimLabel(dim)}。</Text>
          </Flex>
        </Card>
      )}

      {seriesEnabled && !seriesQ.isError && (
        <>
          {/* Numeric readout strip */}
          <div className="jc-obs-readout">
            <ReadoutItem label="平均 TTFT" value={fmtMs(agg.avgTtft)} sub={`${agg.ttftCount.toLocaleString()} 个样本`} />
            <ReadoutItem label="P95 TTFT" value={fmtMs(agg.p95Ttft)} sub="各桶 p95 加权近似" />
            <ReadoutItem label="平均 TPS" value={fmtTps(agg.avgTps)} sub={`${agg.tpsTokens.toLocaleString()} 个输出 token`} />
            <ReadoutItem
              label="原始失败率"
              value={fmtPct(agg.failureRate)}
              tone={rateTone(agg.failureRate, lowSamples)}
              sub={`${agg.failures.toLocaleString()} / ${agg.requests.toLocaleString()} 次`}
            />
            <ReadoutItem
              label="渠道故障率"
              value={fmtPct(agg.providerFaultRate)}
              tone={rateTone(agg.providerFaultRate, lowSamples)}
              sub={`${agg.providerFaults.toLocaleString()} 次`}
            />
            <ReadoutItem
              label="异常请求"
              value={agg.anomalies.toLocaleString()}
              sub={`耗时 ${agg.timeAnomalies.toLocaleString()} · 错误 ${agg.errAnomalies.toLocaleString()}`}
            />
          </div>

          {/* Chart stack — shared X, one quantity per axis */}
          {seriesQ.isSuccess && agg.requests === 0 ? (
            <Card mb="4">
              <Flex align="center" justify="center" py="7">
                <Text size="2" color="gray">最近这段时间没有请求。</Text>
              </Flex>
            </Card>
          ) : (
            <Card mb="4" className="jc-obs-charts">
              <ChartRow title="首 token（TTFT）" legend={[["p50", TTFT_P50], ["p95", TTFT_P95], ["avg", chartColors.slate]]}>
                <ComposedChart data={chartData} margin={CHART_MARGIN} syncId="obs">
                  <CartesianGrid {...chartGrid} vertical={false} />
                  <XAxis dataKey="label" hide />
                  <YAxis {...chartAxis} width={CHART_Y_WIDTH} tickFormatter={(v: number) => fmtMsTick(v)} />
                  <RTooltip {...chartTooltipProps} formatter={msFormatter} />
                  <Line type="stepAfter" dataKey="p50" name="p50" stroke={TTFT_P50} strokeWidth={1.5} dot={false} activeDot={DOT} connectNulls={false} isAnimationActive={false} />
                  <Line type="stepAfter" dataKey="p95" name="p95" stroke={TTFT_P95} strokeWidth={1.5} dot={false} activeDot={DOT} connectNulls={false} isAnimationActive={false} />
                  <Line type="stepAfter" dataKey="avg" name="avg" stroke={chartColors.slate} strokeWidth={1.5} strokeDasharray="4 3" dot={false} activeDot={DOT} connectNulls={false} isAnimationActive={false} />
                </ComposedChart>
              </ChartRow>
              <ChartRow title="生成速度（TPS）" legend={[["avg", chartColors.success]]}>
                <ComposedChart data={chartData} margin={CHART_MARGIN} syncId="obs">
                  <CartesianGrid {...chartGrid} vertical={false} />
                  <XAxis dataKey="label" hide />
                  <YAxis {...chartAxis} width={CHART_Y_WIDTH} />
                  <RTooltip {...chartTooltipProps} formatter={tpsFormatter} />
                  <Line type="stepAfter" dataKey="tps" name="TPS" stroke={chartColors.success} strokeWidth={1.5} dot={false} activeDot={DOT} connectNulls={false} isAnimationActive={false} />
                </ComposedChart>
              </ChartRow>
              <ChartRow title="失败率" legend={[["原始失败率", chartColors.amber], ["渠道故障率", chartColors.danger], ["异常数", chartColors.slate]]}>
                <ComposedChart data={chartData} margin={CHART_MARGIN} syncId="obs">
                  <CartesianGrid {...chartGrid} vertical={false} />
                  <XAxis dataKey="label" hide />
                  <YAxis {...chartAxis} width={CHART_Y_WIDTH} tickFormatter={fmtPctTick} />
                  {/* Anomaly counts share the time axis but not the % axis. */}
                  <YAxis yAxisId="count" orientation="right" hide />
                  <RTooltip {...chartTooltipProps} formatter={failFormatter} />
                  <Bar yAxisId="count" dataKey="anomalies" name="异常数" fill={chartColors.slate} fillOpacity={0.45} barSize={3} isAnimationActive={false} />
                  <Line type="stepAfter" dataKey="failRate" name="原始失败率" stroke={chartColors.amber} strokeWidth={1.5} dot={false} activeDot={DOT} connectNulls={false} isAnimationActive={false} />
                  <Line type="stepAfter" dataKey="pfRate" name="渠道故障率" stroke={chartColors.danger} strokeWidth={1.5} dot={false} activeDot={DOT} connectNulls={false} isAnimationActive={false} />
                </ComposedChart>
              </ChartRow>
              <ChartRow title="请求量" legend={[["请求", chartColors.indigo]]} last>
                <ComposedChart data={chartData} margin={CHART_MARGIN} syncId="obs">
                  <CartesianGrid {...chartGrid} vertical={false} />
                  <XAxis dataKey="label" {...chartAxis} minTickGap={28} />
                  <YAxis {...chartAxis} width={CHART_Y_WIDTH} allowDecimals={false} />
                  <RTooltip {...chartTooltipProps} />
                  <Bar dataKey="requests" name="请求" fill={chartColors.indigo} maxBarSize={chartBarMaxSize} isAnimationActive={false} />
                </ComposedChart>
              </ChartRow>
            </Card>
          )}
        </>
      )}

      {/* Leaderboard */}
      <Card mb="4">
        <Flex justify="between" align="center" mb="2" gap="3" wrap="wrap">
          <Heading size="3">
            排行
            <Text size="1" color="gray" weight="regular" ml="2">RPM / TPM 为最近 1 分钟（TPM 计输出 token），其余为所选窗口。点击行在上方图表中查看</Text>
          </Heading>
          <SegmentedControl.Root size="1" value={leaderboardDim} onValueChange={(v) => setLbDimOverride(v as Exclude<ObservabilityDim, "platform">)}>
            <SegmentedControl.Item value="provider">渠道</SegmentedControl.Item>
            <SegmentedControl.Item value="account">账号</SegmentedControl.Item>
            <SegmentedControl.Item value="group">分组</SegmentedControl.Item>
            <SegmentedControl.Item value="model">模型</SegmentedControl.Item>
          </SegmentedControl.Root>
        </Flex>
        {leaderboardQ.isError ? (
          <Text size="1" color="gray">排行数据加载失败：{errMsg(leaderboardQ.error)}</Text>
        ) : (
          <Box style={{ overflowX: "auto" }}>
            <Table.Root size="1" variant="surface">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>{dimLabel(leaderboardDim)}</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">RPM</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">TPM</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">请求</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">失败率</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">渠道故障率</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">TTFT p50 / p95</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">TPS</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">异常</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">操作</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(leaderboardQ.data?.rows ?? []).map((row) => (
                  <LeaderboardRow
                    key={rowEntityId(leaderboardDim, row)}
                    row={row}
                    dim={leaderboardDim}
                    windowStartIso={windowStartIso}
                    onSelect={() => {
                      setLbDimOverride(null);
                      updateParams({ dim: leaderboardDim, entityId: rowEntityId(leaderboardDim, row) });
                    }}
                  />
                ))}
                {leaderboardQ.isSuccess && (leaderboardQ.data?.rows.length ?? 0) === 0 && (
                  <Table.Row>
                    <Table.Cell colSpan={10}><Text size="1" color="gray">最近这段时间没有请求。</Text></Table.Cell>
                  </Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </Box>
        )}
      </Card>

      {/* SLA strip */}
      <Card>
        <Flex justify="between" align="center" mb="1" gap="3" wrap="wrap">
          <Box>
            <Heading size="3">业务可用率</Heading>
            <Text size="1" color="gray" as="div" mt="1">
              每组按真实业务请求判定，不含探测请求与调用方错误。阈值与公开状态页一致：98% / 90% / 80%。
            </Text>
          </Box>
          <SegmentedControl.Root size="1" value={slaWindow} onValueChange={(v) => setSlaWindow(v as ObservabilitySlaWindow)}>
            {SLA_WINDOWS.map((w) => (
              <SegmentedControl.Item key={w.value} value={w.value}>{w.label}</SegmentedControl.Item>
            ))}
          </SegmentedControl.Root>
        </Flex>
        {slaQ.isError ? (
          <Text size="1" color="gray" as="div" mt="2">可用率数据加载失败：{errMsg(slaQ.error)}</Text>
        ) : slaQ.isSuccess && (slaQ.data?.groups.length ?? 0) === 0 ? (
          <Text size="1" color="gray" as="div" mt="2">没有可判定的分组。</Text>
        ) : (
          <Box mt="2">
            {(slaQ.data?.groups ?? []).map((g) => <SlaGroupRow key={g.group_id} group={g} />)}
          </Box>
        )}
      </Card>

      <ErrorsSection />
    </AdminPage>
  );
}

// ---- readout strip ----

function ReadoutItem({ label, value, sub, tone }: { label: string; value: string; sub?: string; tone?: "amber" | "red" }) {
  return (
    <div className="jc-obs-readout-item" data-tone={tone}>
      <span className="jc-obs-readout-label">{label}</span>
      <strong className="jc-num jc-obs-readout-value">{value}</strong>
      {sub && <span className="jc-obs-readout-sub">{sub}</span>}
    </div>
  );
}

// Rates only turn amber/red once the sample is big enough to mean it.
function rateTone(rate: number | null, lowSamples: boolean): "amber" | "red" | undefined {
  if (rate == null || lowSamples) return undefined;
  if (rate >= 0.1) return "red";
  if (rate >= 0.02) return "amber";
  return undefined;
}

// ---- charts ----

const CHART_MARGIN = { top: 6, right: 12, bottom: 0, left: 0 } as const;
const CHART_Y_WIDTH = 52;
const DOT = { r: 3, strokeWidth: 0 } as const;
const TTFT_P50 = chartColors.cyan;
const TTFT_P95 = chartColors.primary;

function ChartRow({ title, legend, last = false, children }: {
  title: string;
  legend: [string, string][];
  last?: boolean;
  children: React.ReactElement;
}) {
  return (
    <div className="jc-obs-chart-row" data-last={last || undefined}>
      <Flex align="center" gap="3" mb="1" wrap="wrap">
        <Text size="1" weight="medium">{title}</Text>
        <Flex gap="3" align="center">
          {legend.map(([name, color]) => (
            <Flex key={name} align="center" gap="1">
              <span aria-hidden style={{ width: 10, height: 2, background: color, display: "inline-block" }} />
              <Text size="1" color="gray">{name}</Text>
            </Flex>
          ))}
        </Flex>
      </Flex>
      <Box style={{ width: "100%", height: last ? 200 : 180 }}>
        <ResponsiveContainer>{children}</ResponsiveContainer>
      </Box>
    </div>
  );
}

// ---- leaderboard ----

function LeaderboardRow({ row, dim, windowStartIso, onSelect }: {
  row: ObservabilityLeaderboardRow;
  dim: Exclude<ObservabilityDim, "platform">;
  windowStartIso: string;
  onSelect: () => void;
}) {
  const low = row.requests > 0 && row.requests < LOW_SAMPLE_REQUESTS;
  const anomalyUrl = anomalyLogsUrl(dim, row, windowStartIso);
  return (
    <Table.Row
      className="jc-obs-lb-row"
      onClick={onSelect}
      title="点击在上方图表中查看"
    >
      <Table.Cell>
        <Flex align="center" gap="2">
          {/* The whole row is a pointer target, but a <tr> can't take focus
              sanely — the name is the real (keyboard-reachable) control. */}
          <button
            type="button"
            className="jc-obs-lb-name"
            onClick={(e) => { e.stopPropagation(); onSelect(); }}
          >
            {row.name}
          </button>
          {low && <Badge size="1" variant="soft" color="gray">样本少</Badge>}
        </Flex>
      </Table.Cell>
      <Table.Cell align="right" className="jc-num">{(row.rpm ?? 0).toLocaleString()}</Table.Cell>
      <Table.Cell align="right" className="jc-num">{(row.tpm ?? 0).toLocaleString()}</Table.Cell>
      <Table.Cell align="right" className="jc-num">{row.requests.toLocaleString()}</Table.Cell>
      <Table.Cell align="right" className="jc-num">
        <RateBadge rate={row.failure_rate} low={low} />
      </Table.Cell>
      <Table.Cell align="right" className="jc-num">
        <RateBadge rate={row.provider_fault_rate} low={low} />
      </Table.Cell>
      <Table.Cell align="right" className="jc-num">{fmtMs(row.p50_ttft_ms)} / {fmtMs(row.p95_ttft_ms)}</Table.Cell>
      <Table.Cell align="right" className="jc-num">{fmtTps(row.avg_tps)}</Table.Cell>
      <Table.Cell align="right" className="jc-num">{row.anomalies.toLocaleString()}</Table.Cell>
      <Table.Cell align="right">
        <Link
          to={anomalyUrl}
          onClick={(e) => e.stopPropagation()}
          style={{ fontSize: 12, whiteSpace: "nowrap" }}
        >
          查看异常请求
        </Link>
      </Table.Cell>
    </Table.Row>
  );
}

function RateBadge({ rate, low }: { rate: number | null; low: boolean }) {
  if (rate == null) return <Text size="1" color="gray">—</Text>;
  const tone = rateTone(rate, low);
  if (!tone) return <span>{fmtPct(rate)}</span>;
  return <Badge size="1" variant="soft" color={tone}>{fmtPct(rate)}</Badge>;
}

// The logs page prefills the filters it supports today (model / start);
// anomaly and provider/group scoping land with the anomaly-filter API in a
// later PR. Unknown params are ignored there, so the URL is written in full
// now and tightens the moment those filters ship.
function anomalyLogsUrl(dim: Exclude<ObservabilityDim, "platform">, row: ObservabilityLeaderboardRow, startIso: string): string {
  const q = new URLSearchParams({ anomaly: "any" });
  if (startIso) q.set("start", startIso);
  const id = rowEntityId(dim, row);
  if (id) q.set(entityParamName(dim)!, id);
  return `/admin/logs?${q.toString()}`;
}

// ---- SLA ----

const SLA_TONE_TO_BADGE: Record<AvailabilityTone, "green" | "amber" | "orange" | "red" | "gray"> = {
  green: "green", amber: "amber", orange: "orange", red: "red", gray: "gray",
};

function SlaGroupRow({ group }: { group: ObservabilitySlaGroup }) {
  const [open, setOpen] = useState(false);
  const models = group.models ?? [];
  return (
    <div className="jc-obs-sla-row">
      <Flex align="center" gap="3" wrap="wrap">
        <Box style={{ minWidth: 160, flex: "0 1 auto" }}>
          <Text size="2" weight="medium">{group.name}</Text>
        </Box>
        <Badge size="1" variant="soft" color={SLA_TONE_TO_BADGE[stateTone(group.state)]}>
          {stateLabel(group.state)}
        </Badge>
        {group.open_incident_id && (
          <Tooltip content="该组当前有进行中的系统异常">
            <Badge size="1" variant="soft" color="red">进行中异常</Badge>
          </Tooltip>
        )}
        <Box style={{ flex: "1 1 240px", minWidth: 200 }}>
          <AvailabilityDots buckets={group.buckets ?? []} columns={48} size={5} />
        </Box>
        <Text size="2" weight="medium" className="jc-num" style={{ minWidth: 68, textAlign: "right" }}>
          {group.availability_pct == null ? "—" : `${group.availability_pct.toFixed(2)}%`}
        </Text>
      </Flex>
      <Flex gap="3" mt="1" align="center" wrap="wrap">
        <Text size="1" color="gray" className="jc-num">
          正常 {group.good.toLocaleString()} · 失败 {group.bad.toLocaleString()} · 不计入 {group.excluded.toLocaleString()}
        </Text>
        <Text size="1" color="gray" className="jc-num">平均延迟 {fmtMs(group.avg_latency_ms)}</Text>
        {models.length > 0 && (
          <button type="button" className="jc-obs-sla-toggle" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
            {open ? "收起模型" : `查看 ${models.length} 个模型`}
          </button>
        )}
      </Flex>
      {open && models.length > 0 && (
        <Box mt="2" className="jc-obs-sla-models">
          {models.map((m) => <SlaModelRow key={m.model} model={m} />)}
        </Box>
      )}
    </div>
  );
}

function SlaModelRow({ model }: { model: ObservabilitySlaModel }) {
  return (
    <Flex align="center" gap="3" className="jc-obs-sla-model" wrap="wrap">
      <Badge size="1" variant="soft" color={SLA_TONE_TO_BADGE[stateTone(model.state)]}>
        {stateLabel(model.state)}
      </Badge>
      <Text size="1" style={{ minWidth: 140 }}>{model.model}</Text>
      <Box style={{ flex: "1 1 200px", minWidth: 160 }}>
        <AvailabilityDots buckets={model.buckets ?? []} columns={48} size={4} />
      </Box>
      <Text size="1" className="jc-num" style={{ minWidth: 60, textAlign: "right" }}>
        {model.availability_pct == null ? "—" : `${model.availability_pct.toFixed(2)}%`}
      </Text>
    </Flex>
  );
}

// ---- aggregation & formatting ----

interface Aggregate {
  requests: number;
  failures: number;
  providerFaults: number;
  anomalies: number;
  timeAnomalies: number;
  errAnomalies: number;
  ttftCount: number;
  failureRate: number | null;
  providerFaultRate: number | null;
  avgTtft: number | null;
  // Weighted mean of per-bucket p95s — an approximation (exact windowed
  // percentiles would need the raw samples), labelled P95 all the same
  // because it is the best available figure and consistently biased.
  p95Ttft: number | null;
  avgTps: number | null;
  // Output tokens behind avgTps. Unlike requests, this is the metric's real
  // coverage: a channel can serve thousands of non-streamed requests and
  // contribute nothing to it.
  tpsTokens: number;
}

function aggregate(points: ObservabilityPoint[]): Aggregate {
  let requests = 0, failures = 0, providerFaults = 0, anomalies = 0, timeAnomalies = 0, errAnomalies = 0;
  let ttftCount = 0, ttftSum = 0, p95Sum = 0, p95Weight = 0, tpsTokens = 0, tpsSeconds = 0;
  for (const p of points) {
    requests += p.requests;
    failures += p.failures;
    providerFaults += p.provider_faults;
    anomalies += p.anomalies;
    timeAnomalies += p.time_anomalies;
    errAnomalies += p.error_anomalies;
    if (p.ttft_count > 0) {
      ttftCount += p.ttft_count;
      if (p.avg_ttft_ms != null) ttftSum += p.avg_ttft_ms * p.ttft_count;
      if (p.p95_ttft_ms != null) { p95Sum += p.p95_ttft_ms * p.ttft_count; p95Weight += p.ttft_count; }
    }
    // Each bucket's avg_tps is tokens over seconds, so recombining buckets
    // means summing both sides rather than averaging the ratios: sum the
    // tokens, and recover each bucket's seconds as tokens/rate. Averaging the
    // ratios weighted by tokens computes Σ(T²/S)/ΣT, which by Cauchy-Schwarz
    // never falls below the true ΣT/ΣS — one degraded bucket among healthy
    // ones reads as a rounding error instead of an outage.
    if (p.avg_tps != null && p.avg_tps > 0 && p.tps_tokens > 0) {
      tpsTokens += p.tps_tokens;
      tpsSeconds += p.tps_tokens / p.avg_tps;
    }
  }
  return {
    requests, failures, providerFaults, anomalies, timeAnomalies, errAnomalies, ttftCount,
    failureRate: requests > 0 ? failures / requests : null,
    providerFaultRate: requests > 0 ? providerFaults / requests : null,
    avgTtft: ttftCount > 0 ? ttftSum / ttftCount : null,
    p95Ttft: p95Weight > 0 ? p95Sum / p95Weight : null,
    avgTps: tpsSeconds > 0 ? tpsTokens / tpsSeconds : null,
    tpsTokens,
  };
}

function dimLabel(dim: ObservabilityDim): string {
  switch (dim) {
    case "provider": return "渠道";
    case "group": return "分组";
    case "account": return "账号";
    case "model": return "模型";
    default: return "全平台";
  }
}

function shortTime(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function fmtMs(ms?: number | null): string {
  if (ms == null) return "—";
  if (ms >= 10_000) return `${(ms / 1000).toFixed(1)}s`;
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
  return `${Math.round(ms)}ms`;
}

function fmtMsTick(ms: number): string {
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.round(ms)}`;
}

function fmtPct(v?: number | null): string {
  if (v == null) return "—";
  return `${(v * 100).toFixed(v >= 0.1 ? 1 : 2)}%`;
}

// Axis ticks for 0–1 rates. Low rates keep one decimal: on a quiet channel
// rounding 0.4% down would print a column of identical "0%" ticks.
function fmtPctTick(v: number): string {
  const pct = v * 100;
  if (pct === 0) return "0%";
  return pct >= 10 ? `${Math.round(pct)}%` : `${parseFloat(pct.toFixed(1))}%`;
}

function fmtTps(v?: number | null): string {
  if (v == null) return "—";
  return v.toFixed(v >= 10 ? 1 : 2);
}

// Recharts tooltip formatters. The value arrives untyped from recharts.
function msFormatter(value: unknown): string {
  return typeof value === "number" ? fmtMs(value) : "—";
}
function tpsFormatter(value: unknown): string {
  return typeof value === "number" ? fmtTps(value) : "—";
}
function failFormatter(value: unknown, name: unknown): string {
  if (typeof value !== "number") return "—";
  if (name === "异常数") return String(value);
  return fmtPct(value);
}

// ---- live error monitor ----

const ERROR_WINDOWS: { value: ObservabilityErrorWindow; label: string }[] = [
  { value: "15m", label: "15分钟" },
  { value: "1h", label: "1小时" },
  { value: "6h", label: "6小时" },
  { value: "24h", label: "24小时" },
];

// ErrorsSection answers "what is failing right now": per-class breakdown with
// shares and first/last sighting, a per-bucket error timeline, and the newest
// raw error rows. Polls every 15s — the backend answer is TTL-cached anyway.
function ErrorsSection() {
  const [window_, setWindow] = useState<ObservabilityErrorWindow>("15m");
  const q = useQuery({
    queryKey: ["admin", "observability", "errors", window_],
    queryFn: () => adminApi.observabilityErrors({ window: window_, limit: 60 }),
    refetchInterval: 15_000,
    placeholderData: (prev) => prev,
  });
  const d = q.data;

  const bucketData = useMemo(() => (d?.buckets ?? []).map((b) => ({
    label: shortTime(b.t),
    errors: b.errors,
    requests: b.requests,
  })), [d]);

  return (
    <Card mt="3">
      <Flex justify="between" align="center" mb="2" gap="3" wrap="wrap">
        <Box>
          <Heading size="3">实时错误</Heading>
          <Text size="1" color="gray" as="div" mt="1">
            窗口内 {d ? `${d.total_errors} 个系统错误 / ${d.total_requests} 个请求` : "…"}，按错误分类聚合；每 15 秒刷新。
            {d && d.excluded_errors > 0 ? ` 已排除 ${d.excluded_errors} 个用户侧失败（请求参数错误、客户端取消、内容风控、额度或密钥问题）。` : ""}
          </Text>
        </Box>
        <Flex align="center" gap="3">
          {q.isFetching && <Text size="1" color="gray">刷新中…</Text>}
          <SegmentedControl.Root size="1" value={window_} onValueChange={(v) => setWindow(v as ObservabilityErrorWindow)}>
            {ERROR_WINDOWS.map((w) => (
              <SegmentedControl.Item key={w.value} value={w.value}>{w.label}</SegmentedControl.Item>
            ))}
          </SegmentedControl.Root>
        </Flex>
      </Flex>

      {q.isError ? (
        <Text size="1" color="gray" as="div">错误数据加载失败：{errMsg(q.error)}</Text>
      ) : d && d.total_errors === 0 ? (
        <Flex align="center" justify="center" py="5">
          <Text size="2" color="gray">窗口内没有系统错误。</Text>
        </Flex>
      ) : (
        <>
          {/* per-class breakdown */}
          <Box mb="4">
            <Table.Root size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>分类</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">次数</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell style={{ width: 160 }}>占比</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>首见 / 末见</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>高频原因</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>主要账号</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>主要模型</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(d?.breakdown ?? []).map((r) => (
                  <Table.Row key={r.error_class}>
                    <Table.Cell>
                      <Badge size="1" variant="soft" color={errorClassColor(r.error_class)}>{errorClassLabel(r.error_class)}</Badge>
                    </Table.Cell>
                    <Table.Cell align="right" className="jc-num">{r.count}</Table.Cell>
                    <Table.Cell>
                      <Flex align="center" gap="2">
                        <Box style={{ width: 90, height: 6, background: "var(--gray-4)", borderRadius: 3, overflow: "hidden", flexShrink: 0 }}>
                          <Box style={{ width: `${Math.round(r.share * 100)}%`, height: "100%", background: "var(--red-9)" }} />
                        </Box>
                        <Text size="1" color="gray" className="jc-num">{Math.round(r.share * 100)}%</Text>
                      </Flex>
                    </Table.Cell>
                    <Table.Cell>
                      <Text size="1" color="gray" className="jc-num">{shortTime(r.first_seen)} / {shortTime(r.last_seen)}</Text>
                    </Table.Cell>
                    <Table.Cell>
                      <Tooltip content={r.sample}>
                        <Text size="1" color="gray" style={{ display: "inline-block", maxWidth: 300, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", verticalAlign: "bottom" }}>
                          {r.sample || "—"}
                        </Text>
                      </Tooltip>
                    </Table.Cell>
                    <Table.Cell><Text size="1">{r.top_account || "—"}</Text></Table.Cell>
                    <Table.Cell><Text size="1">{r.top_model || "—"}</Text></Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Root>
          </Box>

          {/* error timeline */}
          <Box mb="4" style={{ height: 130 }}>
            <ResponsiveContainer width="100%" height="100%">
              <ComposedChart data={bucketData} margin={{ top: 4, right: 8, bottom: 0, left: 0 }}>
                <CartesianGrid {...chartGrid} vertical={false} />
                <XAxis dataKey="label" {...chartAxis} tickLine={false} minTickGap={40} />
                <YAxis {...chartAxis} width={36} allowDecimals={false} />
                <RTooltip {...chartTooltipProps} formatter={(v, name) => [v, name === "errors" ? "错误" : "请求"]} />
                <Bar dataKey="errors" name="errors" fill={chartColors.danger} radius={[2, 2, 0, 0]} isAnimationActive={false} />
              </ComposedChart>
            </ResponsiveContainer>
          </Box>

          {/* newest rows */}
          <Text size="1" color="gray" as="div" mb="1">最新错误（{d?.recent.length ?? 0} 条）</Text>
          <Box style={{ maxHeight: 320, overflowY: "auto" }}>
            <Table.Root size="1">
              <Table.Body>
                {(d?.recent ?? []).map((r, i) => (
                  <Table.Row key={`${r.at}-${i}`}>
                    <Table.Cell style={{ width: 80 }}>
                      <Text size="1" color="gray" className="jc-num">{shortTimeSec(r.at)}</Text>
                    </Table.Cell>
                    <Table.Cell style={{ width: 110 }}>
                      <Badge size="1" variant="soft" color={errorClassColor(r.error_class)}>{errorClassLabel(r.error_class)}</Badge>
                    </Table.Cell>
                    <Table.Cell style={{ width: 50 }}>
                      <Text size="1" color={r.http_status >= 500 ? "red" : r.http_status >= 400 ? "amber" : "gray"} className="jc-num">{r.http_status || "—"}</Text>
                    </Table.Cell>
                    <Table.Cell style={{ width: 150 }}>
                      <Text size="1" style={{ display: "block", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{r.model || "—"}</Text>
                    </Table.Cell>
                    <Table.Cell style={{ width: 130 }}>
                      <Text size="1" style={{ display: "block", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{r.account || "—"}</Text>
                    </Table.Cell>
                    <Table.Cell style={{ width: 110 }}>
                      <Text size="1" color="gray" style={{ display: "block", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{r.group || "—"}</Text>
                    </Table.Cell>
                    <Table.Cell>
                      <Tooltip content={r.message}>
                        <Text size="1" color="gray" style={{ display: "block", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", maxWidth: 420 }}>{r.message}</Text>
                      </Tooltip>
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Root>
          </Box>
        </>
      )}
    </Card>
  );
}

function shortTimeSec(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}
