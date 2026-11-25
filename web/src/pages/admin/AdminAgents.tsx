import "../../admin-responsive.css";
import { useEffect, useState } from "react";
import {
  Badge,
  Box,
  Button,
  Card,
  Dialog,
  Flex,
  Select,
  Table,
  Text,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { adminApi, type AdminAgentRow, type AgentEntryKind, type SiteSettings } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { PointAmount } from "../../components/PointAmount";
import { sumAmounts } from "../../lib/currency";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

// Admin-facing agent management. Lists every AgentProfile with a
// pending-settlement snapshot, lets admin set the site-wide max incentive
// and redeem-code refund rules,
// each agent's budget / coupon discount / status, and triggers "Settle" to
// close out the pending entries into a new AgentSettlement.
export default function AdminAgents() {
  const qc = useQueryClient();
  const agents = useQuery({ queryKey: ["admin", "agents"], queryFn: adminApi.agents });
  const settings = useQuery({ queryKey: ["admin", "settings"], queryFn: adminApi.getSettings });
  const maxIncentive = settings.data?.max_incentive_pct;
  const [editing, setEditing] = useState<AdminAgentRow | null>(null);
  const [settling, setSettling] = useState<AdminAgentRow | null>(null);
  const [historyOf, setHistoryOf] = useState<AdminAgentRow | null>(null);

  const rows = agents.data ?? [];
  const pendingEntries = rows.reduce((sum, a) => sum + (a.pending_entries ?? 0), 0);
  const pendingTotal = sumAmounts(rows.map((a) => a.pending_total_payable));

  return (
    <AdminPage
      title="代理结算"
      description="设置代理的让利额度和专属优惠码折扣，结算卡密销售与优惠码返利。"
    >
      <MaxIncentiveCard value={maxIncentive} />
      {settings.data && <RefundRulesCard settings={settings.data} />}
      <AdminMetricGrid>
        <AdminMetricCard label="代理数" value={rows.length} detail="已授予代理角色" tone="iris" />
        <AdminMetricCard label="活跃代理" value={rows.filter((a) => a.status === "active").length} detail="active" tone="green" />
        <AdminMetricCard label="待结算流水" value={pendingEntries.toLocaleString()} detail="pending entries" tone="amber" />
        <AdminMetricCard label="待结算净额" value={<PointAmount amount={pendingTotal} iconSize={16} />} detail="平台应收，负数为平台应付" tone="blue" />
      </AdminMetricGrid>
      <Card className="pn-card-flat jc-table-panel">
        <div className="jc-local-table-scroll">
        <Table.Root>
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>代理</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>让利额度</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>优惠码</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>客户折扣 / 代理返利</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>状态</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">待结算流水</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">卡密销售</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">卡密退款</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">优惠码返利</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">待结算净额</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">操作</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {(agents.data ?? []).map((a) => (
              <Table.Row key={a.agent_id}>
                <Table.Cell>
                  <Text size="2" weight="medium">{a.email || a.agent_id.slice(0, 8)}</Text>
                  <Text size="1" color="gray" as="div">{a.nickname || a.agent_id}</Text>
                </Table.Cell>
                <Table.Cell>{budgetOf(a)}%</Table.Cell>
                <Table.Cell><Text className="jc-mono">{a.coupon_code || "未创建"}</Text></Table.Cell>
                <Table.Cell>{a.coupon_discount_pct ?? "0"}% / {a.coupon_rebate_rate_pct ?? "0"}%</Table.Cell>
                <Table.Cell>
                  <Badge size="1" variant="soft" color={a.status === "active" ? "green" : "amber"}>{a.status}</Badge>
                </Table.Cell>
                <Table.Cell align="right">{a.pending_entries ?? 0}</Table.Cell>
                <Table.Cell align="right"><PointAmount amount={a.pending_sales ?? "0"} iconSize={12} /></Table.Cell>
                <Table.Cell align="right"><PointAmount amount={a.pending_refunds ?? "0"} iconSize={12} /></Table.Cell>
                <Table.Cell align="right"><PointAmount amount={a.pending_coupon_rebates ?? "0"} iconSize={12} /></Table.Cell>
                <Table.Cell align="right">
                  <Text weight="bold"><PointAmount amount={a.pending_total_payable ?? "0"} iconSize={12} /></Text>
                </Table.Cell>
                <Table.Cell align="right">
                  <Flex gap="1" justify="end">
                    <Button size="1" variant="soft" onClick={() => setHistoryOf(a)}>明细</Button>
                    <Button size="1" variant="soft" onClick={() => setEditing(a)}>配置</Button>
                    <Button size="1" color="iris" onClick={() => setSettling(a)} disabled={(a.pending_entries ?? 0) === 0}>结算</Button>
                  </Flex>
                </Table.Cell>
              </Table.Row>
            ))}
            {(agents.data ?? []).length === 0 && (
              <Table.Row><Table.Cell colSpan={11}><Text color="gray">尚未授予代理角色</Text></Table.Cell></Table.Row>
            )}
          </Table.Body>
        </Table.Root>
        </div>
      </Card>

      <EditAgentDialog
        agent={editing}
        maxIncentive={maxIncentive}
        onClose={() => setEditing(null)}
        onDone={() => qc.invalidateQueries({ queryKey: ["admin", "agents"] })}
      />
      <SettleDialog
        agent={settling}
        onClose={() => setSettling(null)}
        onDone={() => qc.invalidateQueries({ queryKey: ["admin", "agents"] })}
      />
      <HistoryDialog agent={historyOf} onClose={() => setHistoryOf(null)} />
    </AdminPage>
  );
}

// budgetOf is the agent's effective budget as the backend applies it: the
// coupon discount plus rebate, both already capped by max_incentive_pct.
function budgetOf(a: AdminAgentRow): string {
  return trimPct(Number(a.coupon_discount_pct ?? 0) + Number(a.coupon_rebate_rate_pct ?? 0));
}

const entryKindText: Record<AgentEntryKind, string> = {
  sale: "卡密销售",
  refund: "卡密退款",
  coupon_rebate: "优惠码返利",
  coupon_refund: "返利冲减",
  balance_withdraw: "提现到余额",
};

function trimPct(n: number): string {
  return n.toFixed(2).replace(/\.?0+$/, "");
}

function MaxIncentiveCard({ value }: { value?: number }) {
  const qc = useQueryClient();
  const [draft, setDraft] = useState("");
  useEffect(() => {
    if (value !== undefined) setDraft(String(value));
  }, [value]);
  // PUT expects the full SiteSettings blob: re-GET right before saving so
  // other settings are never wiped.
  const save = useMutation({
    mutationFn: async () => {
      const current = await adminApi.getSettings();
      return adminApi.updateSettings({ ...current, max_incentive_pct: Number(draft) });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "settings"] });
      qc.invalidateQueries({ queryKey: ["admin", "agents"] });
    },
  });
  const n = Number(draft);
  const valid = draft.trim() !== "" && n > 0 && n <= 100;
  return (
    <Card className="pn-card-flat" mb="4">
      <Flex align="end" gap="3" wrap="wrap">
        <Box style={{ flex: 1, minWidth: 260 }}>
          <Text size="2" weight="bold" as="div">最大让利比例</Text>
          <Text size="1" color="gray">
            每笔订单最多让出面额的这个比例，即代理的拿货折扣。代理让利额度、邀请充值返利比例都不能超过它；调低后，超出的代理额度立即按上限计算。
          </Text>
        </Box>
        <Box style={{ width: 120 }}>
          <ClearableTextField type="number" value={draft} onChange={(e) => setDraft(e.target.value)} />
        </Box>
        <Button loading={save.isPending} disabled={!valid || n === value} onClick={() => save.mutate()}>保存</Button>
      </Flex>
      {save.isError && <Text size="2" color="red" as="div" mt="2">保存失败：{errMsg(save.error)}</Text>}
    </Card>
  );
}

type RefundRuleKey = "agent_plan_refund_day_divisor" | "agent_plan_refund_rate_pct" | "agent_balance_refund_rate_pct";

const REFUND_RULES: { key: RefundRuleKey; label: string; hint: string; min: number; max: number }[] = [
  { key: "agent_plan_refund_day_divisor", label: "套餐折算天数", hint: "套餐退款 = 原价 × 剩余天数 ÷ 此天数 × 套餐退款比例", min: 1, max: 3650 },
  { key: "agent_plan_refund_rate_pct", label: "套餐退款比例（%）", hint: "按天折算后的金额再乘以此比例", min: 0, max: 100 },
  { key: "agent_balance_refund_rate_pct", label: "余额退款比例（%）", hint: "余额码退款 = 面额 × 此比例", min: 0, max: 100 },
];

// RefundRulesCard edits how much a reseller refunds when a customer returns a
// redeem code. Like MaxIncentiveCard it re-reads the settings before saving.
function RefundRulesCard({ settings }: { settings: SiteSettings }) {
  const qc = useQueryClient();
  const stored = (key: RefundRuleKey) => String(settings[key]);
  const [draft, setDraft] = useState<Record<RefundRuleKey, string>>(() => ({
    agent_plan_refund_day_divisor: stored("agent_plan_refund_day_divisor"),
    agent_plan_refund_rate_pct: stored("agent_plan_refund_rate_pct"),
    agent_balance_refund_rate_pct: stored("agent_balance_refund_rate_pct"),
  }));
  const save = useMutation({
    mutationFn: async () => {
      const current = await adminApi.getSettings();
      return adminApi.updateSettings({
        ...current,
        agent_plan_refund_day_divisor: Math.round(Number(draft.agent_plan_refund_day_divisor)),
        agent_plan_refund_rate_pct: Number(draft.agent_plan_refund_rate_pct),
        agent_balance_refund_rate_pct: Number(draft.agent_balance_refund_rate_pct),
      });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "settings"] }),
  });
  const valid = REFUND_RULES.every(({ key, min, max }) => {
    const n = Number(draft[key]);
    return draft[key].trim() !== "" && Number.isFinite(n) && n >= min && n <= max;
  });
  const dirty = REFUND_RULES.some(({ key }) => Number(draft[key]) !== settings[key]);
  return (
    <Card className="pn-card-flat" mb="4">
      <Text size="2" weight="bold" as="div">卡密退款规则</Text>
      <Text size="1" color="gray" as="div" mb="3">代理为客户登记卡密退款时，按以下规则计算退款金额。修改只影响之后登记的退款。</Text>
      <Flex align="end" gap="3" wrap="wrap">
        {REFUND_RULES.map(({ key, label, hint, min, max }) => (
          <Box key={key} style={{ flex: "1 1 200px" }}>
            <Text as="label" size="2" weight="medium">{label}</Text>
            <ClearableTextField type="number" min={min} max={max} value={draft[key]} onChange={(e) => setDraft({ ...draft, [key]: e.target.value })} />
            <Text size="1" color="gray" as="div" mt="1">{hint}</Text>
          </Box>
        ))}
        <Button loading={save.isPending} disabled={!valid || !dirty} onClick={() => save.mutate()}>保存</Button>
      </Flex>
      {save.isError && <Text size="2" color="red" as="div" mt="2">保存失败：{errMsg(save.error)}</Text>}
    </Card>
  );
}

function EditAgentDialog({ agent, maxIncentive, onClose, onDone }: { agent: AdminAgentRow | null; maxIncentive?: number; onClose: () => void; onDone: () => void }) {
  const [budget, setBudget] = useState("");
  const [discount, setDiscount] = useState("");
  const [status, setStatus] = useState<"active" | "suspended">("active");
  const m = useMutation({
    // Only changed fields are sent: the backend applies them one by one, and
    // resending a capped budget would store the cap as the agent's own.
    // Budget is edited as 让利额度 but stored as the platform share.
    mutationFn: () => adminApi.updateAgent(agent!.agent_id, {
      platform_share_pct: budget !== budgetOf(agent!) ? trimPct(100 - Number(budget)) : undefined,
      coupon_discount_pct: discount !== (agent!.coupon_discount_pct ?? "0") ? discount.trim() : undefined,
      status: status !== agent!.status ? status : undefined,
    }),
    onSuccess: () => { onDone(); onClose(); },
  });
  const { reset } = m;
  useEffect(() => {
    if (!agent) return;
    setBudget(budgetOf(agent));
    setDiscount(agent.coupon_discount_pct ?? "0");
    setStatus(agent.status);
    reset();
  }, [agent, reset]);
  const b = Number(budget);
  const d = Number(discount);
  const rebate = b - d;
  const valid = budget.trim() !== "" && discount.trim() !== "" &&
    b >= 0 && b <= (maxIncentive ?? 100) && d >= 0 && d <= b;
  return (
    <Dialog.Root open={!!agent} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="440px">
        {agent && (
          <>
            <Dialog.Title>配置代理 — {agent.email || agent.agent_id.slice(0, 8)}</Dialog.Title>
            <Flex direction="column" gap="3" mt="3">
              <Box>
                <Text as="label" size="2" weight="medium">让利额度（%）</Text>
                <ClearableTextField type="number" value={budget} onChange={(e) => setBudget(e.target.value)} />
                <Text size="1" color="gray">
                  卡密按面额的 {trimPct(100 - Number(budget || 0))}% 向平台结算；优惠码的客户折扣与代理返利合计不超过此额度。上限 {maxIncentive ?? "—"}%。
                </Text>
              </Box>
              <Box>
                <Text as="label" size="2" weight="medium">优惠码客户折扣（%）</Text>
                <ClearableTextField type="number" value={discount} onChange={(e) => setDiscount(e.target.value)} />
                <Text size="1" color="gray">
                  代理返利 {Number.isFinite(rebate) && rebate >= 0 ? trimPct(rebate) : "—"}%，按订单实付金额计算。代理也可以自己调整折扣。
                </Text>
              </Box>
              <Box>
                <Text as="label" size="2" weight="medium">状态</Text>
                <Select.Root value={status} onValueChange={(v) => setStatus(v as "active" | "suspended")}>
                  <Select.Trigger />
                  <Select.Content>
                    <Select.Item value="active">活跃</Select.Item>
                    <Select.Item value="suspended">停用（无法生成兑换码、登记退款，优惠码失效）</Select.Item>
                  </Select.Content>
                </Select.Root>
              </Box>
              {m.isError && <Text size="2" color="red">保存失败: {errMsg(m.error)}</Text>}
              <Flex gap="2" justify="end">
                <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
                <Button disabled={m.isPending || !valid} onClick={() => m.mutate()}>保存</Button>
              </Flex>
            </Flex>
          </>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}

function SettleDialog({ agent, onClose, onDone }: { agent: AdminAgentRow | null; onClose: () => void; onDone: () => void }) {
  const [note, setNote] = useState("");
  const m = useMutation({
    mutationFn: () => adminApi.settleAgent(agent!.agent_id, note || undefined),
    onSuccess: () => { setNote(""); onDone(); onClose(); },
  });
  return (
    <Dialog.Root open={!!agent} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="460px">
        {agent && (
          <>
            <Dialog.Title>结算 — {agent.email || agent.agent_id.slice(0, 8)}</Dialog.Title>
            <Dialog.Description size="2" color="gray">
              将 {agent.pending_entries ?? 0} 笔未结算流水打包为一笔结算单，净额（正数为代理应付平台，负数为平台应付代理）:
              <Text weight="bold" mx="1"><PointAmount amount={agent.pending_total_payable ?? "0"} iconSize={13} /></Text>
              新的销售/退款会进入下一个周期。
            </Dialog.Description>
            <Box mt="3">
              <Text as="label" size="2" weight="medium">备注 (可选)</Text>
              <ClearableTextField value={note} onChange={(e) => setNote(e.target.value)} placeholder="例：2026-05 月结算" />
            </Box>
            {m.isError && <Text size="2" color="red" as="div" mt="3">结算失败: {errMsg(m.error)}</Text>}
            <Flex gap="2" justify="end" mt="3">
              <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
              <Button color="iris" disabled={m.isPending} onClick={() => m.mutate()}>确认结算</Button>
            </Flex>
          </>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}

function HistoryDialog({ agent, onClose }: { agent: AdminAgentRow | null; onClose: () => void }) {
  const settlements = useQuery({
    queryKey: ["admin", "agent-settlements", agent?.agent_id],
    queryFn: () => adminApi.agentSettlements(agent!.agent_id),
    enabled: !!agent,
  });
  const refunds = useQuery({
    queryKey: ["admin", "agent-refunds", agent?.agent_id],
    queryFn: () => adminApi.agentRefunds(agent!.agent_id),
    enabled: !!agent,
  });
  const pending = useQuery({
    queryKey: ["admin", "agent-entries", agent?.agent_id, "pending"],
    queryFn: () => adminApi.agentEntries(agent!.agent_id, { status: "pending", limit: 500 }),
    enabled: !!agent,
  });
  return (
    <Dialog.Root open={!!agent} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="720px">
        {agent && (
          <>
            <Dialog.Title>明细 — {agent.email || agent.agent_id.slice(0, 8)}</Dialog.Title>
            <Box mt="3">
              <Text size="2" weight="bold" as="div" mb="2">待结算流水</Text>
              <div className="jc-local-table-scroll">
              <Table.Root size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell>时间</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>类型</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>用户</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">金额</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">应付平台</Table.ColumnHeaderCell>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {(pending.data?.entries ?? []).map((e) => (
                    <Table.Row key={e.id}>
                      <Table.Cell>{new Date(e.created_at).toLocaleString()}</Table.Cell>
                      <Table.Cell>{entryKindText[e.kind] ?? e.kind}</Table.Cell>
                      <Table.Cell><Text size="1" className="jc-mono">{e.user_id?.slice(0, 8) ?? "—"}</Text></Table.Cell>
                      <Table.Cell align="right"><PointAmount amount={e.sale_amount} iconSize={12} /></Table.Cell>
                      <Table.Cell align="right"><PointAmount amount={e.platform_payable} iconSize={12} /></Table.Cell>
                    </Table.Row>
                  ))}
                  {(pending.data?.entries ?? []).length === 0 && (
                    <Table.Row><Table.Cell colSpan={5}><Text color="gray">无</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
              </div>
            </Box>
            <Box mt="3">
              <Text size="2" weight="bold" as="div" mb="2">结算历史</Text>
              <div className="jc-local-table-scroll">
              <Table.Root size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell>时间</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>流水数</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">应付平台</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>备注</Table.ColumnHeaderCell>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {(settlements.data?.settlements ?? []).map((s) => (
                    <Table.Row key={s.id}>
                      <Table.Cell>{new Date(s.created_at).toLocaleString()}</Table.Cell>
                      <Table.Cell>{s.entries_count}</Table.Cell>
                      <Table.Cell align="right"><PointAmount amount={s.total_payable} iconSize={12} /></Table.Cell>
                      <Table.Cell>{s.note || "—"}</Table.Cell>
                    </Table.Row>
                  ))}
                  {(settlements.data?.settlements ?? []).length === 0 && (
                    <Table.Row><Table.Cell colSpan={4}><Text color="gray">无</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
              </div>
            </Box>
            <Box mt="3">
              <Text size="2" weight="bold" as="div" mb="2">退款记录</Text>
              <div className="jc-local-table-scroll">
              <Table.Root size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell>时间</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>类型</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">退款金额</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">平台扣减</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>原因</Table.ColumnHeaderCell>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {(refunds.data?.refunds ?? []).map((r) => {
                    return (
                      <Table.Row key={r.id}>
                        <Table.Cell>{new Date(r.created_at).toLocaleString()}</Table.Cell>
                        <Table.Cell>{r.kind}</Table.Cell>
                        <Table.Cell align="right"><PointAmount amount={r.refund_amount} iconSize={12} /></Table.Cell>
                        <Table.Cell align="right"><PointAmount amount={r.platform_deduct} iconSize={12} /></Table.Cell>
                        <Table.Cell>{r.reason || "—"}</Table.Cell>
                      </Table.Row>
                    );
                  })}
                  {(refunds.data?.refunds ?? []).length === 0 && (
                    <Table.Row><Table.Cell colSpan={5}><Text color="gray">无</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
              </div>
            </Box>
            <Flex justify="end" mt="3">
              <Button variant="soft" onClick={onClose}>关闭</Button>
            </Flex>
          </>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}
