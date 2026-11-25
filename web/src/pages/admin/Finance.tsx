import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Badge, Box, Button, Dialog, Flex, Grid, Select, Table, Tabs, Text, TextArea } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, RotateCcw, Trash2 } from "lucide-react";
import { OrderRefundableHint } from "../../components/OrderRefundableHint";
import {
  adminApi,
  type Plan,
  type Provider,
  type ProviderSettlement,
  type RefundPolicy,
  type SiteOrder,
  type SiteRefund,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { AdminMetricCard, AdminPage, AdminSection } from "../../components/AdminPage";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { usePagedQuery } from "../../lib/paged";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { DateRangeInput, last30DaysRange, rangeToIso } from "../../components/UsageLogFilters";
import { IncomeExpenseChart, type ChartKind } from "../../components/IncomeExpenseChart";

type OrderStatus = SiteOrder["status"] | "";
type OrderKind = SiteOrder["kind"] | "";

// money() tags channel-cost rows with their stored currency — vendors
// may settle in USD (converted via the channel settlement rate), so
// those rows keep a real currency label. Platform-side amounts
// (revenue, orders, refunds) are points and use <PointAmount />.
const CURRENCY_OPTIONS = ["CNY", "USD"] as const;

function money(amount?: string, currency = "CNY"): string {
  if (!amount) return `${currency} 0.00`;
  const n = Number(amount);
  const value = Number.isFinite(n)
    ? n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
    : amount;
  return `${currency} ${value}`;
}

function dt(value?: string): string {
  return value ? new Date(value).toLocaleString() : "-";
}

function shortID(value?: string): string {
  return value ? value.slice(0, 8) : "-";
}

// Only the listing endpoints eager-load the buyer. The same rows can also
// arrive from a mutation response (create / pay / approve), so fall back to
// the id prefix instead of rendering an empty cell.
function buyer(row: { user_email?: string; user_nickname?: string; user_id?: string }): ReactNode {
  const primary = row.user_email || row.user_nickname;
  if (!primary) return shortID(row.user_id);
  return (
    <Flex direction="column">
      <Text size="1">{primary}</Text>
      {row.user_email && row.user_nickname && (
        <Text size="1" color="gray">{row.user_nickname}</Text>
      )}
    </Flex>
  );
}

function orderStatusColor(status: SiteOrder["status"]): "gray" | "green" | "amber" | "red" | "blue" {
  return ({
    pending: "amber",
    paid: "green",
    canceled: "gray",
    partially_refunded: "blue",
    refunded: "red",
  } as const)[status];
}

function orderStatusLabel(status: SiteOrder["status"]): string {
  return ({
    pending: "待支付",
    paid: "已支付",
    canceled: "已取消",
    partially_refunded: "部分退款",
    refunded: "已退款",
  } as const)[status];
}

function refundStatusColor(status: SiteRefund["status"]): "gray" | "green" | "amber" | "red" {
  return ({
    pending: "amber",
    approved: "green",
    rejected: "red",
    canceled: "gray",
  } as const)[status];
}

function refundStatusLabel(status: SiteRefund["status"]): string {
  return ({
    pending: "待审批",
    approved: "已通过",
    rejected: "已拒绝",
    canceled: "已取消",
  } as const)[status];
}

export default function Finance() {
  const qc = useQueryClient();
  const defaultRange = useMemo(() => last30DaysRange(), []);
  const [start, setStart] = useState(defaultRange.start);
  const [end, setEnd] = useState(defaultRange.end);
  const [chartKind, setChartKind] = useState<ChartKind>("bar");
  const [costOpen, setCostOpen] = useState(false);
  const [settlementMonth, setSettlementMonth] = useState(() => new Date().toISOString().slice(0, 7));
  const [settlementDialog, setSettlementDialog] = useState<ProviderSettlement | null>(null);
  const [orderOpen, setOrderOpen] = useState(false);
  const [refundOrder, setRefundOrder] = useState<SiteOrder | null>(null);
  const [orderStatus, setOrderStatus] = useState<OrderStatus>("");
  const [orderKind, setOrderKind] = useState<OrderKind>("");
  const range = rangeToIso(start, end);

  const providers = useQuery({ queryKey: ["admin", "providers", "all"], queryFn: adminApi.providers });
  const plans = useQuery({ queryKey: ["admin", "plans", "all"], queryFn: adminApi.plans });
  const summary = useQuery({
    queryKey: ["admin", "finance", "summary", range.from, range.to],
    queryFn: () => adminApi.financeSummary({ from: range.from, to: range.to }),
  });
  const costs = useQuery({
    queryKey: ["admin", "finance", "provider-costs", range.from, range.to],
    queryFn: () => adminApi.providerCosts({ from: range.from, to: range.to, limit: 50 }),
  });
  const settlements = useQuery({
    queryKey: ["admin", "finance", "provider-settlements", settlementMonth],
    queryFn: () => adminApi.providerSettlements({ month: settlementMonth || undefined }),
  });
  const orders = usePagedQuery<SiteOrder>({
    queryKey: ["admin", "finance", "orders"],
    queryFn: (p) => adminApi.financeOrders({
      q: p.q || undefined,
      status: orderStatus || undefined,
      kind: orderKind || undefined,
      limit: p.limit,
      offset: p.offset,
    }).then((r) => ({ items: r.orders, total: r.total, limit: r.limit, offset: r.offset })),
    extras: [orderStatus, orderKind],
    pageSize: 50,
  });
  const policy = useQuery({ queryKey: ["admin", "finance", "refund-policy"], queryFn: adminApi.refundPolicy });

  const invalidateFinance = () => {
    qc.invalidateQueries({ queryKey: ["admin", "finance"] });
    qc.invalidateQueries({ queryKey: ["admin", "providers"] });
  };

  const pay = useMutation({
    mutationFn: (id: string) => adminApi.markFinanceOrderPaid(id),
    onSuccess: invalidateFinance,
  });
  const cancel = useMutation({
    mutationFn: (id: string) => adminApi.cancelFinanceOrder(id),
    onSuccess: invalidateFinance,
  });
  const delCost = useMutation({
    mutationFn: (id: string) => adminApi.deleteProviderCost(id),
    onSuccess: invalidateFinance,
  });
  const approve = useMutation({
    mutationFn: (id: string) => adminApi.approveFinanceRefund(id),
    onSuccess: invalidateFinance,
  });
  const reject = useMutation({
    mutationFn: (id: string) => adminApi.rejectFinanceRefund(id),
    onSuccess: invalidateFinance,
  });

  const providerMap = useMemo(() => new Map((providers.data ?? []).map((p) => [p.id, p.name])), [providers.data]);
  const data = summary.data;

  const orderColumns: Column<SiteOrder>[] = [
    { header: "订单号", minWidth: "170px", cell: (o) => o.order_no },
    { header: "用户", minWidth: "170px", cell: (o) => buyer(o) },
    { header: "类型", width: "70px", cell: (o) => (o.kind === "plan" ? "套餐" : "余额") },
    {
      header: "金额", width: "120px", align: "right",
      cell: (o) => <PointAmount amount={o.amount} iconSize={12} />,
    },
    {
      header: "状态", width: "110px",
      cell: (o) => <Badge color={orderStatusColor(o.status)} variant="soft">{orderStatusLabel(o.status)}</Badge>,
    },
    {
      header: "退款", minWidth: "180px",
      cell: (o) => <OrderRefundCell order={o} onApprove={(id) => approve.mutate(id)} onReject={(id) => reject.mutate(id)} approving={approve.isPending} rejecting={reject.isPending} />,
    },
    { header: "创建时间", width: "160px", cell: (o) => dt(o.created_at) },
    {
      header: "操作", width: "180px", align: "right",
      cell: (o) => (
        <Flex gap="1" justify="end">
          {o.status === "pending" && (
            <>
              <Button size="1" variant="soft" color="green" loading={pay.isPending} onClick={() => pay.mutate(o.id)}>确认支付</Button>
              <Button size="1" variant="soft" color="gray" loading={cancel.isPending} onClick={() => cancel.mutate(o.id)}>取消</Button>
            </>
          )}
          {(o.status === "paid" || o.status === "partially_refunded") && (
            <Button size="1" variant="soft" color="amber" onClick={() => setRefundOrder(o)}>
              <RotateCcw size={13} />退款
            </Button>
          )}
        </Flex>
      ),
    },
  ];

  return (
    <AdminPage
      title="财务"
      description="按时间段查看收入、支出和净利润。退款在订单里处理。"
      actions={(
        <>
          <DateRangeInput start={start} setStart={setStart} end={end} setEnd={setEnd} />
          <Button variant="soft" onClick={() => summary.refetch()}>刷新</Button>
        </>
      )}
    >
      <Grid columns={{ initial: "1", sm: "3" }} gap="3" mb="4">
        <AdminMetricCard
          label="收入"
          value={<PointAmount amount={data?.total_income} iconSize={15} />}
          detail={<>站内 <PointAmount amount={data?.site_income} iconSize={11} /> · 代理 <PointAmount amount={data?.agent_income} iconSize={11} /></>}
          tone="green"
        />
        <AdminMetricCard
          label="支出"
          value={<PointAmount amount={data?.total_cost} iconSize={15} />}
          detail={<>用量 <PointAmount amount={data?.fixed_cost} iconSize={11} /> · 手动 <PointAmount amount={data?.manual_cost} iconSize={11} /></>}
          tone="amber"
        />
        <AdminMetricCard
          label="净利润"
          value={<PointAmount amount={data?.net_profit} iconSize={15} />}
          detail="收入 − 支出"
          tone={Number(data?.net_profit ?? 0) >= 0 ? "green" : "red"}
        />
      </Grid>

      <AdminSection title="每日收支" description="按自然日聚合，默认柱状图，可切换折线。">
        <IncomeExpenseChart days={data?.days ?? []} kind={chartKind} onKindChange={setChartKind} />
      </AdminSection>

      <Tabs.Root defaultValue="orders">
        <Tabs.List mb="4">
          <Tabs.Trigger value="orders">订单</Tabs.Trigger>
          <Tabs.Trigger value="costs">成本</Tabs.Trigger>
          <Tabs.Trigger value="settlements">对账</Tabs.Trigger>
          <Tabs.Trigger value="policy">退款策略</Tabs.Trigger>
        </Tabs.List>

        <Tabs.Content value="settlements">
          <AdminSection
            title="提供商月度对账"
            description="按自然月汇总每个提供商的模型用量成本和手动成本，确认时可录入实际结算金额。"
            actions={(
              <>
                <ClearableTextField type="month" value={settlementMonth} onChange={(e) => setSettlementMonth(e.target.value)} />
                <Button variant="soft" onClick={() => settlements.refetch()}>刷新</Button>
              </>
            )}
          >
            <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>提供商</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">系统成本</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">实际成本</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">差额</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>状态</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>确认时间</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">操作</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(settlements.data?.settlements ?? []).map((s) => (
                  <Table.Row key={s.provider_id}>
                    <Table.Cell>{s.provider_name}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{money(s.calculated_cost, s.currency)}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{money(s.actual_cost, s.currency)}</Table.Cell>
                    <Table.Cell align="right" className="jc-num">{money(s.correction_delta, s.currency)}</Table.Cell>
                    <Table.Cell>
                      <Badge color={s.status === "confirmed" ? "green" : "amber"} variant="soft">
                        {s.status === "confirmed" ? "已确认" : "待确认"}
                      </Badge>
                    </Table.Cell>
                    <Table.Cell>{dt(s.confirmed_at)}</Table.Cell>
                    <Table.Cell align="right">
                      <Button size="1" variant="soft" onClick={() => setSettlementDialog(s)}>
                        {s.status === "confirmed" ? "查看 / 纠正" : "确认"}
                      </Button>
                    </Table.Cell>
                  </Table.Row>
                ))}
                {!settlements.isLoading && (settlements.data?.settlements.length ?? 0) === 0 && (
                  <Table.Row><Table.Cell colSpan={7}><Text color="gray">暂无提供商</Text></Table.Cell></Table.Row>
                )}
              </Table.Body>
            </Table.Root>
            </div>
          </AdminSection>
        </Tabs.Content>

        <Tabs.Content value="costs">
          <Grid columns={{ initial: "1", lg: "2" }} gap="4">
            <AdminSection title="提供商成本拆分" description="固定成本来自模型真实成本单价，单次成本来自手动登记。">
              <div className="jc-local-table-scroll">
              <Table.Root variant="surface" size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell>提供商</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">固定成本</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">单次成本</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">合计</Table.ColumnHeaderCell>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {(data?.providers ?? []).map((p) => (
                    <Table.Row key={p.provider_id}>
                      <Table.Cell>{p.provider_name}</Table.Cell>
                      <Table.Cell align="right" className="jc-num"><PointAmount amount={p.fixed_cost} iconSize={12} /></Table.Cell>
                      <Table.Cell align="right" className="jc-num"><PointAmount amount={p.manual_cost} iconSize={12} /></Table.Cell>
                      <Table.Cell align="right" className="jc-num"><PointAmount amount={p.total_cost} iconSize={12} /></Table.Cell>
                    </Table.Row>
                  ))}
                  {!summary.isLoading && (data?.providers.length ?? 0) === 0 && (
                    <Table.Row><Table.Cell colSpan={4}><Text color="gray">暂无提供商成本数据</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
              </div>
            </AdminSection>

            <AdminSection
              title="手动单次成本"
              description="记录订阅费、临时补量、人工采购等无法从 token 用量推导的提供商成本。"
              actions={<Button size="2" onClick={() => setCostOpen(true)}><Plus size={14} />登记成本</Button>}
            >
              <div className="jc-local-table-scroll">
              <Table.Root variant="surface" size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell>提供商</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">金额</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>发生时间</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>备注</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell />
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {(costs.data?.costs ?? []).map((c) => (
                    <Table.Row key={c.id}>
                      <Table.Cell>{providerMap.get(c.provider_id) ?? shortID(c.provider_id)}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">{money(c.amount, c.currency)}</Table.Cell>
                      <Table.Cell>{dt(c.occurred_at)}</Table.Cell>
                      <Table.Cell><Text size="1" color="gray">{c.note || "-"}</Text></Table.Cell>
                      <Table.Cell align="right">
                        <Button size="1" variant="soft" color="red" onClick={() => delCost.mutate(c.id)}>
                          <Trash2 size={13} />
                        </Button>
                      </Table.Cell>
                    </Table.Row>
                  ))}
                  {!costs.isLoading && (costs.data?.costs.length ?? 0) === 0 && (
                    <Table.Row><Table.Cell colSpan={5}><Text color="gray">暂无手动成本</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
              </div>
            </AdminSection>
          </Grid>
        </Tabs.Content>

        <Tabs.Content value="orders">
          <AdminSection
            title="站内订单"
            description="只管理站内支付订单状态，不接入支付网关；确认支付后自动发放余额或套餐权益。"
            actions={<Button size="2" onClick={() => setOrderOpen(true)}><Plus size={14} />创建订单</Button>}
          >
            <DataTable<SiteOrder>
              {...orders}
              card={false}
              isLoading={orders.query.isLoading}
              isFetching={orders.query.isFetching}
              refetch={() => orders.query.refetch()}
              columns={orderColumns}
              rowKey={(o) => o.id}
              searchPlaceholder="搜索订单号 / 用户邮箱、昵称…"
              emptyText="暂无订单"
              filters={(
                <>
                  <Select.Root value={orderKind || "_all"} onValueChange={(v) => setOrderKind(v === "_all" ? "" : v as OrderKind)}>
                    <Select.Trigger placeholder="类型" />
                    <Select.Content>
                      <Select.Item value="_all">全部类型</Select.Item>
                      <Select.Item value="balance">余额</Select.Item>
                      <Select.Item value="plan">套餐</Select.Item>
                    </Select.Content>
                  </Select.Root>
                  <Select.Root value={orderStatus || "_all"} onValueChange={(v) => setOrderStatus(v === "_all" ? "" : v as OrderStatus)}>
                    <Select.Trigger placeholder="状态" />
                    <Select.Content>
                      <Select.Item value="_all">全部状态</Select.Item>
                      <Select.Item value="pending">待支付</Select.Item>
                      <Select.Item value="paid">已支付</Select.Item>
                      <Select.Item value="partially_refunded">部分退款</Select.Item>
                      <Select.Item value="refunded">已退款</Select.Item>
                      <Select.Item value="canceled">已取消</Select.Item>
                    </Select.Content>
                  </Select.Root>
                </>
              )}
            />
          </AdminSection>
        </Tabs.Content>

        <Tabs.Content value="policy">
          <RefundPolicyForm policy={policy.data} isLoading={policy.isLoading} />
        </Tabs.Content>
      </Tabs.Root>

      <ProviderCostDialog
        open={costOpen}
        providers={providers.data ?? []}
        onClose={() => setCostOpen(false)}
        onSaved={invalidateFinance}
      />
      <ProviderSettlementDialog
        settlement={settlementDialog}
        month={settlementMonth}
        onClose={() => setSettlementDialog(null)}
        onSaved={invalidateFinance}
      />
      <OrderDialog
        open={orderOpen}
        plans={plans.data ?? []}
        onClose={() => setOrderOpen(false)}
        onSaved={invalidateFinance}
      />
      <RefundDialog
        order={refundOrder}
        onClose={() => setRefundOrder(null)}
        onSaved={invalidateFinance}
      />
      {summary.isError && <Text color="red" mt="3" as="div">{errMsg(summary.error)}</Text>}
    </AdminPage>
  );
}

function ProviderCostDialog({
  open,
  providers,
  onClose,
  onSaved,
}: {
  open: boolean;
  providers: Provider[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [providerID, setProviderID] = useState("");
  const [amount, setAmount] = useState("");
  const [currency, setCurrency] = useState("CNY");
  const [occurredAt, setOccurredAt] = useState("");
  const [note, setNote] = useState("");
  const create = useMutation({
    mutationFn: () => adminApi.createProviderCost({
      provider_id: providerID,
      amount,
      currency,
      occurred_at: occurredAt ? new Date(occurredAt).toISOString() : undefined,
      note,
    }),
    onSuccess: () => {
      setAmount("");
      setNote("");
      setOccurredAt("");
      onSaved();
      onClose();
    },
  });

  useEffect(() => {
    if (!providerID && providers[0]) setProviderID(providers[0].id);
  }, [providerID, providers]);

  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="520px">
        <Dialog.Title>登记单次成本</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Field label="提供商">
            <Select.Root value={providerID || "_none"} onValueChange={(v) => setProviderID(v === "_none" ? "" : v)}>
              <Select.Trigger />
              <Select.Content>
                {providers.length === 0 && <Select.Item value="_none">暂无提供商</Select.Item>}
                {providers.map((p) => <Select.Item key={p.id} value={p.id}>{p.name}</Select.Item>)}
              </Select.Content>
            </Select.Root>
          </Field>
          <Field label="金额">
            <ClearableTextField type="number" value={amount} onChange={(e) => setAmount(e.target.value)} />
          </Field>
          <Field label="币种">
            <Select.Root value={currency} onValueChange={setCurrency}>
              <Select.Trigger />
              <Select.Content>
                {CURRENCY_OPTIONS.map((c) => <Select.Item key={c} value={c}>{c}</Select.Item>)}
              </Select.Content>
            </Select.Root>
          </Field>
          <Field label="发生时间">
            <ClearableTextField type="datetime-local" value={occurredAt} onChange={(e) => setOccurredAt(e.target.value)} />
          </Field>
          <Field label="备注">
            <ClearableTextField value={note} onChange={(e) => setNote(e.target.value)} />
          </Field>
          {create.isError && <Text color="red" size="2">{errMsg(create.error)}</Text>}
          <Flex justify="end" gap="2">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button disabled={!providerID || !amount.trim()} loading={create.isPending} onClick={() => create.mutate()}>保存</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function ProviderSettlementDialog({
  settlement,
  month,
  onClose,
  onSaved,
}: {
  settlement: ProviderSettlement | null;
  month: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [actualCost, setActualCost] = useState("");
  const [note, setNote] = useState("");
  const confirm = useMutation({
    mutationFn: () => adminApi.confirmProviderSettlement(settlement!.provider_id, {
      month,
      actual_cost: actualCost.trim(),
      note,
    }),
    onSuccess: () => {
      onSaved();
      onClose();
    },
  });

  useEffect(() => {
    if (!settlement) return;
    setActualCost(settlement.actual_cost || settlement.calculated_cost);
    setNote(settlement.note ?? "");
  }, [settlement]);

  return (
    <Dialog.Root open={!!settlement} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="980px">
        <Dialog.Title>{settlement?.provider_name} 月度对账</Dialog.Title>
        {settlement && (
          <Flex direction="column" gap="4" mt="3">
            <Grid columns={{ initial: "1", sm: "4" }} gap="3">
              <MiniMetric label="系统成本" value={money(settlement.calculated_cost, settlement.currency)} />
              <MiniMetric label="用量成本" value={money(settlement.usage_cost, settlement.currency)} />
              <MiniMetric label="手动成本" value={money(settlement.manual_cost, settlement.currency)} />
              <MiniMetric label="差额" value={money(settlement.correction_delta, settlement.currency)} />
            </Grid>

            <Box>
              <Text size="2" weight="bold" as="div" mb="2">用量明细</Text>
              <div className="jc-local-table-scroll">
              <Table.Root variant="surface" size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell>分组</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>模型</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">请求</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">输入</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">输出</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">缓存读</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">缓存写</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">成本</Table.ColumnHeaderCell>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {settlement.usage_details.map((d) => (
                    <Table.Row key={`${d.group_id}:${d.model}`}>
                      <Table.Cell>{d.group_name || "-"}</Table.Cell>
                      <Table.Cell><Text className="jc-mono" size="1">{d.model}</Text></Table.Cell>
                      <Table.Cell align="right" className="jc-num">{d.requests.toLocaleString()}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">{d.tokens_in.toLocaleString()}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">{d.tokens_out.toLocaleString()}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">{d.cached_tokens_in.toLocaleString()}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">{d.cache_creation_tokens_in.toLocaleString()}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">{money(d.cost, settlement.currency)}</Table.Cell>
                    </Table.Row>
                  ))}
                  {settlement.usage_details.length === 0 && (
                    <Table.Row><Table.Cell colSpan={8}><Text color="gray">暂无用量成本</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
              </div>
            </Box>

            <Box>
              <Text size="2" weight="bold" as="div" mb="2">手动成本明细</Text>
              <div className="jc-local-table-scroll">
              <Table.Root variant="surface" size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell>发生时间</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">原金额</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">折算（积分）</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>备注</Table.ColumnHeaderCell>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {settlement.manual_cost_details.map((d) => (
                    <Table.Row key={d.id}>
                      <Table.Cell>{dt(d.occurred_at)}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">{money(d.amount, d.currency)}</Table.Cell>
                      <Table.Cell align="right" className="jc-num"><PointAmount amount={d.amount_cny} iconSize={12} /></Table.Cell>
                      <Table.Cell><Text size="1" color="gray">{d.note || "-"}</Text></Table.Cell>
                    </Table.Row>
                  ))}
                  {settlement.manual_cost_details.length === 0 && (
                    <Table.Row><Table.Cell colSpan={4}><Text color="gray">暂无手动成本</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
              </div>
            </Box>

            <Grid columns={{ initial: "1", sm: "2" }} gap="3">
              <Field label="实际结算成本">
                <ClearableTextField type="number" value={actualCost} onChange={(e) => setActualCost(e.target.value)} />
              </Field>
              <Field label="备注">
                <ClearableTextField value={note} onChange={(e) => setNote(e.target.value)} />
              </Field>
            </Grid>
            {confirm.isError && <Text color="red" size="2">{errMsg(confirm.error)}</Text>}
            <Flex justify="end" gap="2">
              <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
              <Button loading={confirm.isPending} disabled={!actualCost.trim()} onClick={() => confirm.mutate()}>
                确认结算
              </Button>
            </Flex>
          </Flex>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}

function MiniMetric({ label, value }: { label: string; value: string }) {
  return (
    <Box p="3" style={{ border: "1px solid var(--gray-a6)", borderRadius: 8 }}>
      <Text size="1" color="gray" as="div">{label}</Text>
      <Text size="3" weight="bold" className="jc-num">{value}</Text>
    </Box>
  );
}

function OrderDialog({
  open,
  plans,
  onClose,
  onSaved,
}: {
  open: boolean;
  plans: Plan[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [userID, setUserID] = useState("");
  const [kind, setKind] = useState<SiteOrder["kind"]>("balance");
  const [planID, setPlanID] = useState("");
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const selectedPlan = plans.find((p) => p.id === planID);
  const create = useMutation({
    mutationFn: () => adminApi.createFinanceOrder({
      user_id: userID.trim(),
      kind,
      plan_id: kind === "plan" ? planID : undefined,
      amount,
      currency: "CNY", // 订单金额即积分（1 积分 = 1 元）
      note,
    }),
    onSuccess: () => {
      setUserID("");
      setAmount("");
      setNote("");
      onSaved();
      onClose();
    },
  });

  useEffect(() => {
    if (kind !== "plan" || !selectedPlan) return;
    setAmount(selectedPlan.price);
  }, [kind, selectedPlan]);

  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="560px">
        <Dialog.Title>创建站内订单</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Field label="用户 ID">
            <ClearableTextField value={userID} onChange={(e) => setUserID(e.target.value)} placeholder="UUID" />
          </Field>
          <Field label="订单类型">
            <Select.Root value={kind} onValueChange={(v) => setKind(v as SiteOrder["kind"])}>
              <Select.Trigger />
              <Select.Content>
                <Select.Item value="balance">余额充值</Select.Item>
                <Select.Item value="plan">套餐购买</Select.Item>
              </Select.Content>
            </Select.Root>
          </Field>
          {kind === "plan" && (
            <Field label="套餐">
              <Select.Root value={planID || "_none"} onValueChange={(v) => setPlanID(v === "_none" ? "" : v)}>
                <Select.Trigger />
                <Select.Content>
                  <Select.Item value="_none">选择套餐</Select.Item>
                  {plans.map((p) => <Select.Item key={p.id} value={p.id}>{p.name} · <PointAmount amount={p.price} iconSize={11} /></Select.Item>)}
                </Select.Content>
              </Select.Root>
            </Field>
          )}
          <Flex gap="3" wrap="wrap">
            <Box style={{ flex: "1 1 180px" }}>
              <Field label="金额（积分）">
                <ClearableTextField type="number" value={amount} onChange={(e) => setAmount(e.target.value)} />
              </Field>
            </Box>
          </Flex>
          <Field label="备注">
            <TextArea value={note} onChange={(e) => setNote(e.target.value)} rows={3} />
          </Field>
          {create.isError && <Text color="red" size="2">{errMsg(create.error)}</Text>}
          <Flex justify="end" gap="2">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button disabled={!userID.trim() || !amount.trim() || (kind === "plan" && !planID)} loading={create.isPending} onClick={() => create.mutate()}>
              创建
            </Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function RefundDialog({
  order,
  onClose,
  onSaved,
}: {
  order: SiteOrder | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [amount, setAmount] = useState("");
  const [reason, setReason] = useState("");
  const [note, setNote] = useState("");
  const refundable = useQuery({
    queryKey: ["admin", "order-refundable", order?.id],
    queryFn: () => adminApi.orderRefundable(order!.id),
    enabled: !!order,
  });
  // 打开时把可退金额填进去，可以改。
  useEffect(() => {
    if (order && refundable.data?.balance_bearing) setAmount(refundable.data.cash);
  }, [order, refundable.data]);
  const create = useMutation({
    mutationFn: () => adminApi.createFinanceRefund(order!.id, {
      amount: amount.trim() || undefined,
      reason,
      note,
    }),
    onSuccess: () => {
      setAmount("");
      setReason("");
      setNote("");
      onSaved();
      onClose();
    },
  });

  return (
    <Dialog.Root open={!!order} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="520px">
        <Dialog.Title>创建退款申请</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Text size="2" color="gray">订单 {order?.order_no}，金额 <PointAmount amount={order?.amount} iconSize={12} />。支付渠道支持自动退款的订单审批后原路退回。</Text>
          <OrderRefundableHint data={refundable.data} loading={refundable.isLoading} />
          <Field label="退款金额">
            <ClearableTextField value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="留空按可退金额" />
          </Field>
          <Field label="退款原因">
            <ClearableTextField value={reason} onChange={(e) => setReason(e.target.value)} />
          </Field>
          <Field label="审批备注">
            <TextArea value={note} onChange={(e) => setNote(e.target.value)} rows={3} />
          </Field>
          {create.isError && <Text color="red" size="2">{errMsg(create.error)}</Text>}
          <Flex justify="end" gap="2">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button loading={create.isPending} onClick={() => create.mutate()}>提交退款</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function RefundPolicyForm({ policy, isLoading }: { policy?: RefundPolicy; isLoading: boolean }) {
  const qc = useQueryClient();
  const [form, setForm] = useState<RefundPolicy>({
    plan_no_reason_days: 7,
    balance_no_reason_days: 1,
    plan_refund_ratio: 0.7,
    balance_refund_ratio: 1,
    min_refund_amount: "0",
  });
  const save = useMutation({
    mutationFn: () => adminApi.updateRefundPolicy({
      ...form,
      plan_no_reason_days: Number(form.plan_no_reason_days) || 0,
      balance_no_reason_days: Number(form.balance_no_reason_days) || 0,
      plan_refund_ratio: Number(form.plan_refund_ratio) || 0,
      balance_refund_ratio: Number(form.balance_refund_ratio) || 0,
    }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "finance", "refund-policy"] }),
  });

  useEffect(() => {
    if (policy) setForm(policy);
  }, [policy]);

  return (
    <AdminSection title="退款策略" description="策略用于生成默认退款金额；实际退款仍需客服审批。">
      <Grid columns={{ initial: "1", sm: "2" }} gap="3">
        <Field label="套餐无理由退款天数">
          <ClearableTextField type="number" value={String(form.plan_no_reason_days)} onChange={(e) => setForm({ ...form, plan_no_reason_days: Number(e.target.value) })} />
        </Field>
        <Field label="余额无理由退款天数">
          <ClearableTextField type="number" value={String(form.balance_no_reason_days)} onChange={(e) => setForm({ ...form, balance_no_reason_days: Number(e.target.value) })} />
        </Field>
        <Field label="套餐退款比例">
          <ClearableTextField type="number" step="0.01" value={String(form.plan_refund_ratio)} onChange={(e) => setForm({ ...form, plan_refund_ratio: Number(e.target.value) })} />
        </Field>
        <Field label="余额退款比例">
          <ClearableTextField type="number" step="0.01" value={String(form.balance_refund_ratio)} onChange={(e) => setForm({ ...form, balance_refund_ratio: Number(e.target.value) })} />
        </Field>
        <Field label="最低退款金额">
          <ClearableTextField value={form.min_refund_amount} onChange={(e) => setForm({ ...form, min_refund_amount: e.target.value })} />
        </Field>
      </Grid>
      {save.isError && <Text color="red" mt="3" as="div">{errMsg(save.error)}</Text>}
      {save.isSuccess && <Text color="green" mt="3" as="div">策略已保存</Text>}
      <Flex justify="end" mt="4">
        <Button loading={save.isPending || isLoading} onClick={() => save.mutate()}>保存策略</Button>
      </Flex>
    </AdminSection>
  );
}

function OrderRefundCell({
  order,
  onApprove,
  onReject,
  approving,
  rejecting,
}: {
  order: SiteOrder;
  onApprove: (id: string) => void;
  onReject: (id: string) => void;
  approving: boolean;
  rejecting: boolean;
}) {
  const refunds = order.refunds ?? [];
  if (refunds.length === 0) {
    return <Text size="1" color="gray">—</Text>;
  }
  return (
    <Flex direction="column" gap="1">
      {refunds.map((r) => (
        <Flex key={r.id} align="center" gap="2" wrap="wrap">
          <Badge size="1" color={refundStatusColor(r.status)} variant="soft">{refundStatusLabel(r.status)}</Badge>
          <PointAmount amount={r.amount} iconSize={11} />
          {r.status === "pending" && (
            <>
              <Button size="1" variant="ghost" color="green" loading={approving} onClick={() => onApprove(r.id)}>通过</Button>
              <Button size="1" variant="ghost" color="red" loading={rejecting} onClick={() => onReject(r.id)}>拒绝</Button>
            </>
          )}
        </Flex>
      ))}
    </Flex>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Box>
      <Text as="div" size="2" weight="medium" mb="1">{label}</Text>
      {children}
    </Box>
  );
}
