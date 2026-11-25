import { useState } from "react";
import { Badge, Box, Card, Code, Flex, Grid, Heading, Select, Text, Tooltip } from "@radix-ui/themes";
import { Link } from "react-router-dom";
import { ExternalLink } from "lucide-react";
import {
  Area, AreaChart, Bar, BarChart, CartesianGrid, Cell,
  ResponsiveContainer, Tooltip as RTooltip, XAxis, YAxis,
} from "recharts";
import { useQuery } from "@tanstack/react-query";
import { adminApi, type UserIncident } from "../../api/endpoints";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import CopyableId from "../../components/CopyableId";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";
import { chartAxis, chartBarMaxSize, chartColors, chartGrid, chartTooltipProps } from "../../lib/chartTheme";

const KINDS = ["policy_keyword", "cyber_security", "rate_limit", "quota_exceeded", "auth_failure", "forbidden", "llm_moderation", "sanction", "other"];
const SEVERITIES = ["info", "low", "medium", "high", "critical"];

// Severity is a risk gradient — color it consistently everywhere (chart +
// badge) so a glance reads the same across the page.
const SEVERITY_HEX: Record<string, string> = {
  info: chartColors.indigo,
  low: chartColors.cyan,
  medium: chartColors.amber,
  high: chartColors.orange,
  critical: chartColors.danger,
};
// Kind has no natural order — cycle a fixed palette so colors stay stable.
const KIND_PALETTE = [
  chartColors.primary, chartColors.cyan, chartColors.amber, chartColors.danger,
  chartColors.success, chartColors.indigo, chartColors.violet, chartColors.orange, chartColors.slate,
];

export default function IncidentsPage() {
  const [kind, setKind] = useState<string>("");
  const [sev, setSev] = useState<string>("");
  const [uid, setUid] = useState<string>("");

  const paged = usePagedQuery<UserIncident>({
    queryKey: ["admin", "incidents"],
    queryFn: (p) =>
      adminApi
        .incidents({
          q: p.q || undefined,
          kind: kind || undefined,
          severity: sev || undefined,
          user_id: uid.trim() || undefined,
          limit: p.limit,
          offset: p.offset,
        })
        .then((r) => ({ items: r.incidents, total: r.total, limit: r.limit, offset: r.offset })),
    extras: [kind, sev, uid.trim()],
    pageSize: 50,
  });

  // Stats scope to the user filter when one is set, otherwise all users.
  // Window defaults server-side to the trailing 30 days.
  const statsQ = useQuery({
    queryKey: ["admin", "incidents", "stats", uid.trim()],
    queryFn: () => adminApi.incidentStats({ user_id: uid.trim() || undefined }),
    staleTime: 60_000,
  });
  const stats = statsQ.data;

  const blockedPct = stats && stats.total > 0 ? Math.round((stats.blocked / stats.total) * 100) : 0;
  const fpPct = stats && stats.reviewed > 0 ? Math.round((stats.false_positive / stats.reviewed) * 100) : 0;
  const reviewPct = stats && stats.total > 0 ? Math.round((stats.reviewed / stats.total) * 100) : 0;

  const cols: Column<UserIncident>[] = [
    {
      header: "时间",
      width: "150px",
      cell: (r) => <span className="jc-mono" style={{ fontSize: 12 }}>{new Date(r.created_at).toLocaleString()}</span>,
    },
    { header: "类型", width: "130px", cell: (r) => <Badge size="1" variant="soft">{r.kind}</Badge> },
    { header: "严重度", width: "90px", cell: (r) => <Badge size="1" variant="soft" color={severityColor(r.severity)}>{r.severity}</Badge> },
    {
      header: "用户",
      width: "110px",
      cell: (r) => r.user_id ? (
        <Tooltip content="打开用户详情">
          <Link to={`/admin/users?edit=${encodeURIComponent(r.user_id)}`} className="jc-mono" style={{ fontSize: 11, textDecoration: "none" }}>
            <Flex align="center" gap="1">
              <Code style={{ fontSize: 11 }}>{r.user_id.slice(0, 8)}</Code>
              <ExternalLink size={11} style={{ opacity: 0.6 }} />
            </Flex>
          </Link>
        </Tooltip>
      ) : <Text size="1" color="gray">—</Text>,
    },
    {
      header: "模型 / 端点",
      width: "160px",
      cell: (r) => (
        <Flex direction="column" gap="0">
          <Text size="1">{r.model || "—"}</Text>
          <Text size="1" color="gray">{r.endpoint || ""}</Text>
        </Flex>
      ),
    },
    {
      header: "动作",
      width: "80px",
      cell: (r) => r.action ? <Badge size="1" color={r.action === "blocked" ? "red" : "gray"} variant="soft">{r.action}</Badge> : <Text size="1" color="gray">—</Text>,
    },
    {
      header: "摘要",
      cell: (r) => <Text size="1" style={{ display: "block" }} truncate>{r.message}</Text>,
    },
    {
      header: "请求 ID",
      width: "120px",
      cell: (r) => r.request_id
        ? <CopyableId value={r.request_id} display={r.request_id.slice(0, 12)} label="点击复制请求 ID" />
        : <Text size="1" color="gray">—</Text>,
    },
  ];

  const filters = (
    <>
      <Select.Root value={kind || "all"} onValueChange={(v) => setKind(v === "all" ? "" : v)}>
        <Select.Trigger placeholder="类型" />
        <Select.Content>
          <Select.Item value="all">类型: 全部</Select.Item>
          {KINDS.map((k) => <Select.Item key={k} value={k}>{k}</Select.Item>)}
        </Select.Content>
      </Select.Root>
      <Select.Root value={sev || "all"} onValueChange={(v) => setSev(v === "all" ? "" : v)}>
        <Select.Trigger placeholder="严重度" />
        <Select.Content>
          <Select.Item value="all">严重度: 全部</Select.Item>
          {SEVERITIES.map((s) => <Select.Item key={s} value={s}>{s}</Select.Item>)}
        </Select.Content>
      </Select.Root>
      <Box style={{ minWidth: 220 }}>
        <ClearableTextField placeholder="user_id (UUID)" value={uid}
          onChange={(e) => setUid(e.target.value)}
          style={{ fontFamily: "ui-monospace, monospace" }} />
      </Box>
    </>
  );

  return (
    <AdminPage
      title="用户异常"
      description="关键词拦截、限速、配额耗尽、鉴权失败等用户流量异常事件。统计图表基于近 30 天。"
    >
      <AdminMetricGrid>
        <AdminMetricCard label="事件总数" value={(stats?.total ?? 0).toLocaleString()} detail={uid.trim() ? "该用户 · 近 30 天" : "全部 · 近 30 天"} tone="iris" />
        <AdminMetricCard label="阻断占比" value={`${blockedPct}%`} detail={`${(stats?.blocked ?? 0).toLocaleString()} 次 blocked`} tone="red" />
        <AdminMetricCard label="误报率" value={`${fpPct}%`} detail={`${(stats?.false_positive ?? 0).toLocaleString()} / ${(stats?.reviewed ?? 0).toLocaleString()} 已复核`} tone="amber" />
        <AdminMetricCard label="复核进度" value={`${reviewPct}%`} detail={`${(stats?.reviewed ?? 0).toLocaleString()} / ${(stats?.total ?? 0).toLocaleString()}`} tone="blue" />
      </AdminMetricGrid>

      <Grid columns={{ initial: "1", md: "2", lg: "3" }} gap="3" mb="4">
        <ChartCard title="严重度分布">
          <CategoryBar data={stats?.by_severity ?? []} colorFor={(k) => SEVERITY_HEX[k] ?? chartColors.primary} order={SEVERITIES} />
        </ChartCard>
        <ChartCard title="类型分布">
          <CategoryBar data={stats?.by_kind ?? []} colorFor={(_, i) => KIND_PALETTE[i % KIND_PALETTE.length]} />
        </ChartCard>
        <ChartCard title="每日趋势 (总量 / 阻断)">
          <TrendArea data={stats?.daily ?? []} />
        </ChartCard>
      </Grid>

      <DataTable
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={cols}
        rowKey={(r) => r.id}
        searchPlaceholder="搜索 message / kind…"
        filters={filters}
      />
    </AdminPage>
  );
}

function ChartCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Card>
      <Heading size="2" mb="2">{title}</Heading>
      <Box style={{ height: 200 }}>{children}</Box>
    </Card>
  );
}

// CategoryBar renders a horizontal bar per category, sorted by count
// (backend already sorts desc). Optional `order` pins a fixed sort so
// severity reads info→critical rather than by frequency.
function CategoryBar({
  data,
  colorFor,
  order,
}: {
  data: { key: string; count: number }[];
  colorFor: (key: string, index: number) => string;
  order?: string[];
}) {
  const rows = order
    ? [...data].sort((a, b) => order.indexOf(a.key) - order.indexOf(b.key))
    : data;
  if (rows.length === 0) return <EmptyChart />;
  return (
    <ResponsiveContainer>
      <BarChart data={rows} layout="vertical" margin={{ top: 4, right: 12, bottom: 0, left: 8 }}>
        <CartesianGrid {...chartGrid} horizontal={false} />
        <XAxis type="number" {...chartAxis} allowDecimals={false} />
        <YAxis type="category" dataKey="key" width={92} {...chartAxis} />
        <RTooltip {...chartTooltipProps} />
        <Bar dataKey="count" maxBarSize={chartBarMaxSize} radius={[0, 4, 4, 0]}>
          {rows.map((r, i) => <Cell key={r.key} fill={colorFor(r.key, i)} />)}
        </Bar>
      </BarChart>
    </ResponsiveContainer>
  );
}

function TrendArea({ data }: { data: { date: string; total: number; blocked: number }[] }) {
  if (data.length === 0) return <EmptyChart />;
  return (
    <ResponsiveContainer>
      <AreaChart data={data} margin={{ top: 4, right: 12, bottom: 0, left: -18 }}>
        <defs>
          <linearGradient id="incTotal" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={chartColors.indigo} stopOpacity={0.35} />
            <stop offset="100%" stopColor={chartColors.indigo} stopOpacity={0.02} />
          </linearGradient>
          <linearGradient id="incBlocked" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={chartColors.danger} stopOpacity={0.4} />
            <stop offset="100%" stopColor={chartColors.danger} stopOpacity={0.02} />
          </linearGradient>
        </defs>
        <CartesianGrid {...chartGrid} vertical={false} />
        <XAxis dataKey="date" {...chartAxis} tickFormatter={(d: string) => d.slice(5)} minTickGap={24} />
        <YAxis {...chartAxis} allowDecimals={false} width={34} />
        <RTooltip {...chartTooltipProps} />
        <Area type="monotone" dataKey="total" name="总量" stroke={chartColors.indigo} fill="url(#incTotal)" strokeWidth={1.5} />
        <Area type="monotone" dataKey="blocked" name="阻断" stroke={chartColors.danger} fill="url(#incBlocked)" strokeWidth={1.5} />
      </AreaChart>
    </ResponsiveContainer>
  );
}

function EmptyChart() {
  return (
    <Flex align="center" justify="center" style={{ height: "100%" }}>
      <Text size="1" color="gray">暂无数据</Text>
    </Flex>
  );
}

function severityColor(s: string): "gray" | "blue" | "amber" | "orange" | "red" {
  switch (s) {
    case "info": return "blue";
    case "low": return "gray";
    case "medium": return "amber";
    case "high": return "orange";
    case "critical": return "red";
    default: return "gray";
  }
}
