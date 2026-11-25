import { useState } from "react";
import { Badge, Box, Card, Flex, Heading, Select, Table, Text } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import {
  Bar, BarChart, CartesianGrid, Legend, ResponsiveContainer, Tooltip, XAxis, YAxis,
} from "recharts";
import { adminApi } from "../../api/endpoints";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { chartAxis, chartBarMaxSize, chartColors, chartGrid, chartLegendStyle, chartTooltipProps } from "../../lib/chartTheme";
import { formatAmount } from "../../lib/currency";
import { errorClassLabel } from "../../lib/errorClass";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { DateRangeInput } from "../../components/UsageLogFilters";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";
import { usageSubjectName } from "../../components/UsageSubject";

// datetime-local controls take/produce LOCAL wall-clock strings; toISOString
// would feed in a UTC string and silently shift the range by the user's
// timezone offset (which is what caused "stats look empty" for UTC+8 users).
function toLocalInput(d: Date): string {
  const pad = (n: number) => n.toString().padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function defaultRange(): { start: string; end: string } {
  const now = new Date();
  const start = new Date(now.getTime() - 7 * 24 * 3600 * 1000);
  return { start: toLocalInput(start), end: toLocalInput(now) };
}

function toIsoParam(value: string): string | undefined {
  if (!value) return undefined;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

export default function Stats() {
  // cost_final_sum is in platform points (1 积分 = 1 元, ledger unit) —
  // rendered as-is, no FX conversion.
  const init = defaultRange();
  const [start, setStart] = useState(init.start);
  const [end, setEnd] = useState(init.end);
  const [bucket, setBucket] = useState("day");
  const [model, setModel] = useState("");
  const [status, setStatus] = useState("");
  const [errDim, setErrDim] = useState("error_class");

  const params = {
    start: toIsoParam(start),
    end: toIsoParam(end),
    bucket,
    model: model || undefined,
    status: status || undefined,
  };

  const usage = useQuery({
    queryKey: ["admin", "stats", "usage", params],
    queryFn: () => adminApi.statsUsage(params),
  });
  const byModel = useQuery({
    queryKey: ["admin", "stats", "breakdown", "model", params.start, params.end],
    queryFn: () => adminApi.statsBreakdown({ by: "model", start: params.start, end: params.end }),
  });
  const byStatus = useQuery({
    queryKey: ["admin", "stats", "breakdown", "status", params.start, params.end],
    queryFn: () => adminApi.statsBreakdown({ by: "status", start: params.start, end: params.end }),
  });
  const byUser = useQuery({
    queryKey: ["admin", "stats", "breakdown", "user", params.start, params.end],
    queryFn: () => adminApi.statsBreakdown({ by: "user", start: params.start, end: params.end }),
  });
  const byErr = useQuery({
    queryKey: ["admin", "stats", "breakdown", errDim, params.start, params.end],
    queryFn: () => adminApi.statsBreakdown({ by: errDim, start: params.start, end: params.end }),
  });

  if (usage.isLoading) return <RouteSkeleton />;

  // Format buckets for the chart — short time label.
  const chartData = (usage.data?.buckets ?? []).map((b) => ({
    label: new Date(b.time).toLocaleString([], bucket === "hour"
      ? { month: "2-digit", day: "2-digit", hour: "2-digit" }
      : { month: "2-digit", day: "2-digit" }),
    requests: b.requests,
    errors: b.errors,
    tokens_in: b.tokens_in + (b.audio_tokens_in ?? 0),
    tokens_out: b.tokens_out + (b.audio_tokens_out ?? 0),
    cost: parseFloat(b.cost_final_sum) || 0,
  }));
  const totals = chartData.reduce(
    (acc, row) => ({
      requests: acc.requests + row.requests,
      errors: acc.errors + row.errors,
      tokens: acc.tokens + row.tokens_in + row.tokens_out,
      cost: acc.cost + row.cost,
    }),
    { requests: 0, errors: 0, tokens: 0, cost: 0 },
  );

  return (
    <AdminPage
      title="全平台使用统计"
      description="按时间桶、模型、状态和用户维度聚合查询平台用量。"
    >
      <AdminMetricGrid>
        <AdminMetricCard label="请求数" value={totals.requests.toLocaleString()} detail="当前时间范围" tone="iris" />
        <AdminMetricCard label="错误数" value={totals.errors.toLocaleString()} detail="当前时间范围" tone={totals.errors > 0 ? "red" : "green"} />
        <AdminMetricCard label="Tokens" value={totals.tokens.toLocaleString()} detail="input + output" tone="purple" />
        <AdminMetricCard label="金额" value={<PointAmount amount={totals.cost} iconSize={16} />} detail="当前时间范围 · 积分" tone="amber" />
      </AdminMetricGrid>

      <Card mb="3" className="jc-admin-stats-filters">
        <Flex gap="3" wrap="wrap" align="end">
          <DateRangeInput
            start={start}
            setStart={setStart}
            end={end}
            setEnd={setEnd}
          />
          <Box style={{ minWidth: 110 }}>
            <Text size="1" color="gray" as="p" mb="1">桶</Text>
            <Select.Root value={bucket} onValueChange={setBucket}>
              <Select.Trigger />
              <Select.Content>
                <Select.Item value="hour">小时</Select.Item>
                <Select.Item value="day">天</Select.Item>
              </Select.Content>
            </Select.Root>
          </Box>
          <Box style={{ minWidth: 140, flex: "1 1 160px" }}>
            <Text size="1" color="gray" as="p" mb="1">模型</Text>
            <ClearableTextField placeholder="(全部)" value={model} onChange={(e) => setModel(e.target.value)} />
          </Box>
          <Box style={{ minWidth: 160 }}>
            <Text size="1" color="gray" as="p" mb="1">状态</Text>
            <Select.Root value={status || "any"} onValueChange={(v) => setStatus(v === "any" ? "" : v)}>
              <Select.Trigger />
              <Select.Content>
                <Select.Item value="any">全部</Select.Item>
                <Select.Item value="ok">ok</Select.Item>
                <Select.Item value="quota_exceeded">quota_exceeded</Select.Item>
                <Select.Item value="upstream_error">upstream_error</Select.Item>
                <Select.Item value="internal_error">internal_error</Select.Item>
              </Select.Content>
            </Select.Root>
          </Box>
        </Flex>
      </Card>

      <Card mb="3">
        <Heading size="3" mb="2">请求数 / 错误数</Heading>
        <Box style={{ width: "100%", height: 280 }}>
          <ResponsiveContainer>
            <BarChart data={chartData} margin={{ top: 5, right: 12, bottom: 0, left: -10 }}>
              <CartesianGrid {...chartGrid} />
              <XAxis dataKey="label" {...chartAxis} />
              <YAxis {...chartAxis} />
              <Tooltip {...chartTooltipProps} />
              <Legend wrapperStyle={chartLegendStyle} />
              <Bar dataKey="requests" name="请求" fill={chartColors.indigo} maxBarSize={chartBarMaxSize} radius={[4, 4, 0, 0]} />
              <Bar dataKey="errors" name="错误" fill={chartColors.danger} maxBarSize={chartBarMaxSize} radius={[4, 4, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        </Box>
      </Card>

      <Box className="jc-chart-grid">
        <Card>
          <Heading size="3" mb="2">Token 用量</Heading>
          <Box style={{ width: "100%", height: 240 }}>
            <ResponsiveContainer>
              <BarChart data={chartData} margin={{ top: 5, right: 12, bottom: 0, left: -10 }}>
                <CartesianGrid {...chartGrid} />
                <XAxis dataKey="label" {...chartAxis} />
                <YAxis {...chartAxis} />
                <Tooltip {...chartTooltipProps} />
                <Legend wrapperStyle={chartLegendStyle} />
                <Bar dataKey="tokens_in" name="input" stackId="t" fill={chartColors.violet} maxBarSize={chartBarMaxSize} />
                <Bar dataKey="tokens_out" name="output" stackId="t" fill={chartColors.success} maxBarSize={chartBarMaxSize} radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </Box>
        </Card>
        <Card>
          <Heading size="3" mb="2">金额</Heading>
          <Box style={{ width: "100%", height: 240 }}>
            <ResponsiveContainer>
              <BarChart data={chartData} margin={{ top: 5, right: 12, bottom: 0, left: -10 }}>
                <CartesianGrid {...chartGrid} />
                <XAxis dataKey="label" {...chartAxis} />
                <YAxis {...chartAxis} tickFormatter={(v: number) => formatAmount(v)} />
                <Tooltip {...chartTooltipProps} formatter={((v: unknown) => formatAmount(Number(v))) as never} />
                <Bar dataKey="cost" name="金额" fill={chartColors.amber} maxBarSize={chartBarMaxSize} radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </Box>
        </Card>
        <Card>
          <Heading size="3" mb="2">按模型</Heading>
          <Box style={{ width: "100%", height: 240 }}>
            <ResponsiveContainer>
              <BarChart data={byModel.data?.rows ?? []} margin={{ top: 5, right: 12, bottom: 0, left: -10 }}>
                <CartesianGrid {...chartGrid} />
                <XAxis dataKey="key" {...chartAxis} tickFormatter={(k: string) => usageSubjectName(undefined, k)} />
                <YAxis {...chartAxis} />
                <Tooltip {...chartTooltipProps} />
                <Bar dataKey="requests" name="请求数" fill={chartColors.indigo} maxBarSize={chartBarMaxSize} radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </Box>
        </Card>
        <Card>
          <Heading size="3" mb="2">按状态</Heading>
          <Box style={{ width: "100%", height: 240 }}>
            <ResponsiveContainer>
              <BarChart data={byStatus.data?.rows ?? []} margin={{ top: 5, right: 12, bottom: 0, left: -10 }}>
                <CartesianGrid {...chartGrid} />
                <XAxis dataKey="key" {...chartAxis} />
                <YAxis {...chartAxis} />
                <Tooltip {...chartTooltipProps} />
                <Bar dataKey="requests" name="请求数" fill={chartColors.cyan} maxBarSize={chartBarMaxSize} radius={[4, 4, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          </Box>
        </Card>
      </Box>

      <Card mt="3">
        <Heading size="3" mb="2">用户排名（按请求数 Top 20）</Heading>
        <Box style={{ overflowX: "auto" }}>
          <div className="jc-local-table-scroll">
          <Table.Root size="1" variant="surface">
            <Table.Header>
              <Table.Row>
                <Table.ColumnHeaderCell>#</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell>用户</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell>user_id</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell align="right">请求数</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell align="right">tokens (in / out)</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell align="right">金额</Table.ColumnHeaderCell>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {(byUser.data?.rows ?? []).map((r: any, i: number) => (
                <Table.Row key={r.key}>
                  <Table.Cell><Badge variant="soft" color={i < 3 ? "iris" : "gray"}>{i + 1}</Badge></Table.Cell>
                  <Table.Cell>{r.label || "—"}</Table.Cell>
                  <Table.Cell><span className="jc-mono">{r.key.slice(0, 8)}…</span></Table.Cell>
                  <Table.Cell align="right" className="jc-num">{r.requests}</Table.Cell>
                  <Table.Cell align="right" className="jc-num">{r.tokens_in} / {r.tokens_out}</Table.Cell>
                  <Table.Cell align="right" className="jc-num"><PointAmount amount={r.cost_final_sum} iconSize={12} /></Table.Cell>
                </Table.Row>
              ))}
              {(byUser.data?.rows.length ?? 0) === 0 && (
                <Table.Row><Table.Cell colSpan={6}><Text color="gray">该时间段内没有用户调用记录</Text></Table.Cell></Table.Row>
              )}
            </Table.Body>
          </Table.Root>
          </div>
        </Box>
      </Card>

      <Card mt="4">
        <Flex justify="between" align="center" mb="2">
          <Heading size="3">错误分解</Heading>
          <Select.Root value={errDim} onValueChange={setErrDim}>
            <Select.Trigger />
            <Select.Content>
              <Select.Item value="error_class">按错误分类</Select.Item>
              <Select.Item value="provider">按渠道</Select.Item>
              <Select.Item value="http_status">按 HTTP 状态码</Select.Item>
            </Select.Content>
          </Select.Root>
        </Flex>
        <Box style={{ overflowX: "auto" }}>
          <div className="jc-local-table-scroll">
          <Table.Root size="1" variant="surface">
            <Table.Header>
              <Table.Row>
                <Table.ColumnHeaderCell>{errDim === "provider" ? "渠道" : errDim === "http_status" ? "状态码" : "错误分类"}</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell align="right">请求数</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell align="right">错误数</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell align="right">错误率</Table.ColumnHeaderCell>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {(byErr.data?.rows ?? []).map((r: any) => {
                const errors = r.errors ?? 0;
                const rate = r.requests > 0 ? (100 * errors / r.requests) : 0;
                return (
                  <Table.Row key={r.key}>
                    <Table.Cell>{errDim === "error_class" ? errorClassLabel(r.label || r.key) : (r.label || r.key)}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{r.requests}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{errors}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">
                      <Badge size="1" variant="soft" color={rate >= 60 ? "red" : rate >= 20 ? "amber" : "gray"}>
                        {rate.toFixed(1)}%
                      </Badge>
                    </Table.Cell>
                  </Table.Row>
                );
              })}
              {(byErr.data?.rows.length ?? 0) === 0 && (
                <Table.Row><Table.Cell colSpan={4}><Text color="gray">该时间段内无数据</Text></Table.Cell></Table.Row>
              )}
            </Table.Body>
          </Table.Root>
          </div>
        </Box>
      </Card>
    </AdminPage>
  );
}
