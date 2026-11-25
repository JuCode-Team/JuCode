import { useMemo, useState } from "react";
import { Button, Flex, SegmentedControl, Table, Text } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { adminApi } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { AdminMetricCard, AdminMetricGrid, AdminPage, AdminSection } from "../../components/AdminPage";
import { IncomeExpenseChart, type ChartKind } from "../../components/IncomeExpenseChart";
import { PointAmount } from "../../components/PointAmount";
import { DateRangeInput, last12MonthsRange, rangeToIso } from "../../components/UsageLogFilters";
import { formatAmount } from "../../lib/currency";
import { chartAxis, chartBarMaxSize, chartColors, chartGrid, chartLegendStyle, chartTooltipProps } from "../../lib/chartTheme";

function pct(raw?: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n) || raw === "") return "-";
  return `${(n * 100).toFixed(1)}%`;
}

function delta(raw?: string): string {
  if (!raw) return "无上期";
  const n = Number(raw);
  if (!Number.isFinite(n)) return "-";
  const sign = n > 0 ? "+" : "";
  return `${sign}${(n * 100).toFixed(1)}%`;
}

function kindLabel(kind: string): string {
  if (kind === "plan") return "套餐";
  if (kind === "balance") return "余额";
  return kind || "-";
}

async function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

export default function Ops() {
  const defaultRange = useMemo(() => last12MonthsRange(), []);
  const [start, setStart] = useState(defaultRange.start);
  const [end, setEnd] = useState(defaultRange.end);
  const [grain, setGrain] = useState<"day" | "month">("month");
  const [chartKind, setChartKind] = useState<ChartKind>("bar");
  const [exporting, setExporting] = useState(false);
  const [exportErr, setExportErr] = useState("");
  const range = rangeToIso(start, end);

  const report = useQuery({
    queryKey: ["admin", "ops", range.from, range.to, grain],
    queryFn: () => adminApi.opsReport({ from: range.from, to: range.to, grain }),
  });
  const data = report.data;
  const kpi = data?.kpi;
  const cmp = data?.compare;
  const funnel = data?.funnel;
  const orders = data?.orders;
  const plans = data?.plans;

  const moneyDays = (data?.series ?? []).map((d) => ({
    date: d.period,
    income: d.revenue,
    expense: d.refunds,
  }));
  const userRows = (data?.series ?? []).map((d) => ({
    period: d.period,
    new_users: d.new_users,
    active_users: d.active_users,
    paying_users: d.paying_users,
    requests: d.requests,
  }));

  async function exportXlsx() {
    setExporting(true);
    setExportErr("");
    try {
      const blob = await adminApi.opsExport({ from: range.from, to: range.to, grain });
      if (blob.type.includes("json")) {
        const text = await blob.text();
        throw new Error(text || "导出失败");
      }
      const stamp = `${(range.from ?? "").slice(0, 10)}_${(range.to ?? "").slice(0, 10)}`;
      await saveBlob(blob, `ops-${stamp}.xlsx`);
    } catch (e) {
      setExportErr(errMsg(e) || "导出失败");
    } finally {
      setExporting(false);
    }
  }

  return (
    <AdminPage
      title="运营"
      description="付费、增长和黏性。金额是积分，未按币种折算。ARPU = 净收入 / 区间活跃。环比是等长上一段。DAU/WAU/MAU 按区间终点往前算。"
      actions={(
        <>
          <DateRangeInput start={start} setStart={setStart} end={end} setEnd={setEnd} />
          <SegmentedControl.Root value={grain} onValueChange={(v) => setGrain(v as "day" | "month")} size="2">
            <SegmentedControl.Item value="day">按日</SegmentedControl.Item>
            <SegmentedControl.Item value="month">按月</SegmentedControl.Item>
          </SegmentedControl.Root>
          <Button variant="soft" onClick={() => report.refetch()}>刷新</Button>
          <Button variant="soft" loading={exporting} onClick={() => void exportXlsx()}>
            <Download size={14} />导出 Excel
          </Button>
        </>
      )}
    >
      {exportErr && <Text color="red" size="2" mb="3" as="div">{exportErr}</Text>}
      {report.error && <Text color="red" size="2" mb="3" as="div">{errMsg(report.error)}</Text>}

      <AdminMetricGrid>
        <AdminMetricCard
          label="净收入"
          value={<PointAmount amount={kpi?.net_revenue} iconSize={15} />}
          detail={<>环比 {delta(cmp?.net_revenue_delta)} · 上期 <PointAmount amount={cmp?.net_revenue} iconSize={11} /></>}
          tone="green"
        />
        <AdminMetricCard
          label="ARPPU"
          value={<PointAmount amount={kpi?.arppu} iconSize={15} />}
          detail={<>ARPU <PointAmount amount={kpi?.arpu} iconSize={11} /> · 区间活跃 {kpi?.period_active ?? 0}</>}
          tone="iris"
        />
        <AdminMetricCard
          label="客单价"
          value={<PointAmount amount={orders?.aov} iconSize={15} />}
          detail={`已支付订单 ${orders?.paid_orders ?? 0} · 退款率 ${pct(orders?.refund_rate)}`}
          tone="blue"
        />
        <AdminMetricCard
          label="下单支付率"
          value={pct(orders?.pay_rate)}
          detail={`下单 ${orders?.created ?? 0} · 已付 ${orders?.created_paid ?? 0} · 待付 ${orders?.created_pending ?? 0}`}
          tone="amber"
        />
        <AdminMetricCard
          label="黏性 DAU/MAU"
          value={pct(kpi?.stickiness)}
          detail={`DAU ${kpi?.dau ?? 0} · WAU ${kpi?.wau ?? 0} · MAU ${kpi?.mau ?? 0}`}
        />
        <AdminMetricCard
          label="复购率"
          value={pct(kpi?.repeat_pay_rate)}
          detail={`付费 ${kpi?.paying_users ?? 0} 人 · 环比 ${delta(cmp?.paying_users_delta)}`}
        />
        <AdminMetricCard
          label="请求数"
          value={(kpi?.requests ?? 0).toLocaleString()}
          detail={`环比 ${delta(cmp?.requests_delta)}`}
        />
        <AdminMetricCard
          label="新注册"
          value={(kpi?.new_users ?? 0).toLocaleString()}
          detail={`环比 ${delta(cmp?.new_users_delta)} · 首购 ${kpi?.new_paying_users ?? 0}`}
        />
      </AdminMetricGrid>

      <AdminSection title="新注册漏斗" description="期内注册用户，到区间终点是否已调用、已付费。">
        <AdminMetricGrid>
          <AdminMetricCard label="注册" value={(funnel?.signed_up ?? 0).toLocaleString()} tone="iris" />
          <AdminMetricCard
            label="已调用"
            value={(funnel?.called ?? 0).toLocaleString()}
            detail={pct(funnel?.call_rate)}
            tone="blue"
          />
          <AdminMetricCard
            label="已付费"
            value={(funnel?.paid ?? 0).toLocaleString()}
            detail={pct(funnel?.pay_rate)}
            tone="green"
          />
        </AdminMetricGrid>
      </AdminSection>

      <AdminSection title="套餐" description="在约按区间终点的 expire_at。过期看 expire_at，自然到期不会改 status。取消按 updated_at。">
        <AdminMetricGrid>
          <AdminMetricCard label="在约" value={(plans?.active ?? 0).toLocaleString()} tone="green" />
          <AdminMetricCard label="新开" value={(plans?.opened ?? 0).toLocaleString()} tone="iris" />
          <AdminMetricCard label="过期" value={(plans?.expired ?? 0).toLocaleString()} tone="amber" />
          <AdminMetricCard label="取消" value={(plans?.canceled ?? 0).toLocaleString()} />
        </AdminMetricGrid>
      </AdminSection>

      <AdminSection title="收入与退款" description="成交额对退款。默认柱状图。">
        <IncomeExpenseChart days={moneyDays} kind={chartKind} onKindChange={setChartKind} />
      </AdminSection>

      <AdminSection title="用户" description="新注册、活跃（有调用）、付费用户。">
        {userRows.length === 0 ? (
          <Text size="2" color="gray">该时间段暂无数据</Text>
        ) : (
          <ResponsiveContainer width="100%" height={280}>
            <ComposedChart data={userRows} barCategoryGap="24%">
              <CartesianGrid {...chartGrid} vertical={false} />
              <XAxis dataKey="period" {...chartAxis} interval="preserveStartEnd" minTickGap={24} />
              <YAxis {...chartAxis} width={48} allowDecimals={false} />
              <Tooltip {...chartTooltipProps} />
              <Legend wrapperStyle={chartLegendStyle} formatter={(v) => ({ new_users: "新注册", active_users: "活跃", paying_users: "付费" }[String(v)] ?? v)} />
              <Bar dataKey="new_users" name="new_users" fill={chartColors.indigo} maxBarSize={chartBarMaxSize} radius={[3, 3, 0, 0]} />
              <Line type="monotone" dataKey="active_users" name="active_users" stroke={chartColors.cyan} strokeWidth={2} dot={false} />
              <Line type="monotone" dataKey="paying_users" name="paying_users" stroke={chartColors.success} strokeWidth={2} dot={false} />
            </ComposedChart>
          </ResponsiveContainer>
        )}
      </AdminSection>

      <AdminSection title="请求量">
        {userRows.length === 0 ? (
          <Text size="2" color="gray">该时间段暂无数据</Text>
        ) : (
          <ResponsiveContainer width="100%" height={220}>
            <ComposedChart data={userRows}>
              <CartesianGrid {...chartGrid} vertical={false} />
              <XAxis dataKey="period" {...chartAxis} interval="preserveStartEnd" minTickGap={24} />
              <YAxis {...chartAxis} width={56} allowDecimals={false} tickFormatter={(v: number) => formatAmount(v, { decimals: 0 })} />
              <Tooltip {...chartTooltipProps} />
              <Line type="monotone" dataKey="requests" name="请求" stroke={chartColors.primary} strokeWidth={2} dot={false} />
            </ComposedChart>
          </ResponsiveContainer>
        )}
      </AdminSection>

      <Flex direction="column" gap="4">
        <AdminSection title="消耗归属" description="按 usage_logs，不是成交。站点订单都记在个人账上，组织没有独立下单。">
          <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>范围</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">请求</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">消耗</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(data?.consume_scope ?? []).map((r) => (
                  <Table.Row key={r.key}>
                    <Table.Cell>{r.label}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{r.count.toLocaleString()}</Table.Cell>
                    <Table.Cell align="right" className="jc-num"><PointAmount amount={r.amount} iconSize={12} /></Table.Cell>
                  </Table.Row>
                ))}
                {!report.isLoading && (data?.consume_scope.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={3}><Text color="gray">暂无</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </div>
        </AdminSection>

        <AdminSection title="计费来源">
          <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>来源</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">请求</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">消耗</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(data?.consume_source ?? []).map((r) => (
                  <Table.Row key={r.key}>
                    <Table.Cell>{r.label}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{r.count.toLocaleString()}</Table.Cell>
                    <Table.Cell align="right" className="jc-num"><PointAmount amount={r.amount} iconSize={12} /></Table.Cell>
                  </Table.Row>
                ))}
                {!report.isLoading && (data?.consume_source.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={3}><Text color="gray">暂无</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </div>
        </AdminSection>

        <AdminSection title="在约套餐构成" description="区间终点仍有效的 user_plans。">
          <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>套餐</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">人数</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(data?.plan_mix ?? []).map((r) => (
                  <Table.Row key={r.key}>
                    <Table.Cell>{r.label}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{r.count}</Table.Cell>
                  </Table.Row>
                ))}
                {!report.isLoading && (data?.plan_mix.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={2}><Text color="gray">暂无</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </div>
        </AdminSection>

        <AdminSection title="套餐成交">
          <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>套餐</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">订单数</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">金额</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(data?.plan_sales ?? []).map((r) => (
                  <Table.Row key={r.key || r.label}>
                    <Table.Cell>{r.label}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{r.count}</Table.Cell>
                    <Table.Cell align="right" className="jc-num"><PointAmount amount={r.amount} iconSize={12} /></Table.Cell>
                  </Table.Row>
                ))}
                {!report.isLoading && (data?.plan_sales.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={3}><Text color="gray">暂无</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </div>
        </AdminSection>

        <AdminSection title="次月留存" description="当月新用户在次月是否还有调用。">
          <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>注册月</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">新用户</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">次月仍有调用</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">留存率</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(data?.cohorts ?? []).map((c) => (
                  <Table.Row key={c.month}>
                    <Table.Cell>{c.month}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{c.new_users}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{c.retained}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{pct(c.rate)}</Table.Cell>
                  </Table.Row>
                ))}
                {!report.isLoading && (data?.cohorts.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={4}><Text color="gray">暂无</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </div>
        </AdminSection>

        <AdminSection title="订单类型">
          <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>类型</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">订单数</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">金额</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(data?.kind_split ?? []).map((k) => (
                  <Table.Row key={k.kind}>
                    <Table.Cell>{kindLabel(k.kind)}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{k.orders}</Table.Cell>
                    <Table.Cell align="right" className="jc-num"><PointAmount amount={k.amount} iconSize={12} /></Table.Cell>
                  </Table.Row>
                ))}
                {!report.isLoading && (data?.kind_split.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={3}><Text color="gray">暂无</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </div>
        </AdminSection>

        <AdminSection title="头部付费用户" description="区间内成交额前 20。">
          <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>邮箱</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>用户 ID</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">订单数</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">金额</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(data?.top_payers ?? []).map((p) => (
                  <Table.Row key={p.user_id}>
                    <Table.Cell>{p.email || "-"}</Table.Cell>
                    <Table.Cell className="jc-num">{p.user_id.slice(0, 8)}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{p.orders}</Table.Cell>
                    <Table.Cell align="right" className="jc-num"><PointAmount amount={p.amount} iconSize={12} /></Table.Cell>
                  </Table.Row>
                ))}
                {!report.isLoading && (data?.top_payers.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={4}><Text color="gray">暂无</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
          </div>
        </AdminSection>
      </Flex>
    </AdminPage>
  );
}
