import { useState } from "react";
import { Badge, Box, Button, Card, Dialog, Flex, Progress, Select, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { adminApi, type AdminUserPlan } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../../components/ClearableTextField";

const windows = { "5h": "5 小时", weekly: "7 天", monthly: "30 天", all: "全部窗口" };
const amount = (value: string | number) => Number(value).toLocaleString(undefined, { maximumFractionDigits: 8 });
function localDate(value: string) {
  const d = new Date(value);
  return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}

type Action = { kind: "create" } | { kind: "edit" | "delete" | "reset"; plan: AdminUserPlan };

export function UserPlansTab({ userId, onChanged }: { userId: string; onChanged: () => void }) {
  const qc = useQueryClient();
  const queryKey = ["admin", "user-plans", userId];
  const query = useQuery({ queryKey, queryFn: () => adminApi.userPlans(userId), refetchInterval: 30_000 });
  const [action, setAction] = useState<Action | null>(null);
  return <Flex direction="column" gap="3">
    <Flex justify="between" align="center" gap="2">
      <Text weight="medium">套餐与用量</Text>
      <Flex gap="2"><Button size="1" variant="soft" loading={query.isFetching} onClick={() => query.refetch()}>刷新</Button><Button size="1" onClick={() => setAction({ kind: "create" })}>新增套餐</Button></Flex>
    </Flex>
    <Text size="1" color="gray">多份有效套餐中，系统使用开始时间最新的一份。消费统计保留历史记录，不随额度重置清零。</Text>
    {query.isPending && <Text color="gray">加载套餐用量…</Text>}
    {query.isError && <Text role="alert" color="red">{errMsg(query.error)}</Text>}
    {query.data?.length === 0 && <Text color="gray">该用户暂无套餐</Text>}
    {query.data?.map((p) => {
      const active = p.status === "active" && new Date(p.expire_at) > new Date();
      const state = p.status === "canceled" ? "已删除" : !active ? "已到期" : p.current ? "当前使用" : "有效";
      return <Card key={p.user_plan_id}>
        <Flex direction="column" gap="3">
          <Flex justify="between" align="center" gap="2" wrap="wrap"><Text weight="medium">{p.name}</Text><Badge color={active ? "green" : "gray"}>{state}</Badge></Flex>
          <Text size="1" color="gray">{new Date(p.start_at).toLocaleString()} — {new Date(p.expire_at).toLocaleString()}</Text>
          {p.type === "quota_subscription" ? (["5h", "weekly", "monthly"] as const).map((w) => <Quota key={w} window={w} used={p[`used_${w}`] ?? "0"} limit={p[`quota_${w}`]} resetAt={p[`reset_${w}_at`]} />)
            : <><Quota window="monthly" used={p.used_monthly} limit={p.monthly_spend_limit} resetAt={p.reset_monthly_at} /><Text size="1" color="gray">按量套餐从账户余额扣费，30 天消费不支持清零；余额在“基础”页调整。</Text></>}
          <Text size="2">累计成功调用 {p.requests.toLocaleString()} 次 · 消费 {amount(p.total_cost)} 积分</Text>
          <Text size="1" color="gray">累计统计以保留的调用日志为准。</Text>
          <Flex gap="2" wrap="wrap" justify="end">
            <Button size="1" variant="soft" disabled={p.status === "canceled"} onClick={() => setAction({ kind: "edit", plan: p })}>修改套餐</Button>
            {p.type === "quota_subscription" && <Button size="1" variant="soft" disabled={!active} onClick={() => setAction({ kind: "reset", plan: p })}>重置额度</Button>}
            <Button size="1" variant="soft" color="red" disabled={p.status === "canceled"} onClick={() => setAction({ kind: "delete", plan: p })}>删除套餐</Button>
          </Flex>
        </Flex>
      </Card>;
    })}
    {action && <PlanAction userId={userId} action={action} onClose={() => setAction(null)} onSaved={() => {
      setAction(null); qc.invalidateQueries({ queryKey }); onChanged();
    }} />}
  </Flex>;
}

function Quota({ window, used, limit, resetAt }: { window: "5h" | "weekly" | "monthly"; used: string; limit: string; resetAt?: string }) {
  const limited = Number(limit) > 0;
  const percent = limited ? Math.min(100, Math.max(0, Number(used) / Number(limit) * 100)) : 0;
  return <Box>
    <Flex justify="between" gap="2" wrap="wrap"><Text size="2">{windows[window]}：已用 {amount(used)} / {limited ? amount(limit) : "不限额"} 积分</Text>{limited && <Text size="1" color={percent >= 100 ? "red" : "gray"}>剩余 {amount(Math.max(0, Number(limit) - Number(used)))} 积分</Text>}</Flex>
    {limited && <Progress mt="1" value={percent} color={percent >= 100 ? "red" : "blue"} aria-label={`${windows[window]}额度使用率`} />}
    {resetAt && <Text size="1" color="gray">下次重置：{new Date(resetAt).toLocaleString()}</Text>}
  </Box>;
}

function PlanAction({ userId, action, onClose, onSaved }: { userId: string; action: Action; onClose: () => void; onSaved: () => void }) {
  const editing = action.kind === "edit";
  const form = editing || action.kind === "create";
  const [planId, setPlanId] = useState(editing ? action.plan.plan_id : "");
  const [expiry, setExpiry] = useState(editing ? localDate(action.plan.expire_at) : "");
  const [window, setWindow] = useState("all");
  const [requestId] = useState(() => crypto.randomUUID());
  const plans = useQuery({ queryKey: ["admin", "plans"], queryFn: adminApi.plans, enabled: form });
  const selected = plans.data?.find((p) => p.id === planId);
  const title = { create: "新增套餐", edit: "修改套餐", delete: "删除套餐", reset: "重置额度" }[action.kind];
  const save = useMutation({ mutationFn: async () => {
    if (action.kind === "create") return adminApi.grantUserPlan(userId, { request_id: requestId, plan_id: planId, ...(expiry ? { expire_at: new Date(expiry).toISOString() } : {}) });
    if (action.kind === "edit") return adminApi.updateUserPlan(userId, action.plan.user_plan_id, { plan_id: planId, expire_at: new Date(expiry).toISOString() });
    if (action.kind === "delete") return adminApi.deleteUserPlan(userId, action.plan.user_plan_id);
    return adminApi.resetUserPlanQuota(userId, action.plan.user_plan_id, window);
  }, onSuccess: onSaved });
  const validTime = expiry ? Number.isFinite(new Date(expiry).getTime()) && new Date(expiry) > new Date() : !editing;
  return <Dialog.Root open onOpenChange={(open) => { if (!open && !save.isPending) onClose(); }}>
    <Dialog.Content maxWidth="480px">
      <Dialog.Title>{title}</Dialog.Title>
      <Dialog.Description size="2" mb="3">
        {action.kind === "create" ? "直接发放套餐，不扣用户余额，也不生成支付订单。新增后将优先使用这份套餐。"
          : action.kind === "edit" ? "可更换同类套餐或调整到期时间，保留已消耗额度，不重复赠送初始余额。"
          : action.kind === "delete" ? `删除「${action.plan.name}」后立即停止该套餐权益，保留用量和订单记录，不自动退款或扣回赠送余额。其他有效套餐可能接替生效。`
          : `重置「${action.plan.name}」选定窗口的已用额度，立即恢复可用额度。历史统计不变，正在处理的请求仍会在结算时计入。`}
      </Dialog.Description>
      <Flex direction="column" gap="3">
        {form && <>
          <Text as="label" size="2">套餐
            <Select.Root value={planId} onValueChange={setPlanId}><Select.Trigger aria-label="套餐" placeholder="选择套餐" style={{ width: "100%" }} /><Select.Content>
              {(plans.data ?? []).filter((p) => (p.status === "active" || p.id === planId) && (!editing || p.type === action.plan.type)).map((p) => <Select.Item key={p.id} value={p.id}>{p.name}</Select.Item>)}
            </Select.Content></Select.Root>
          </Text>
          {plans.isError && <Text color="red">{errMsg(plans.error)}</Text>}
          <Text as="label" size="2">到期时间（本地时间）<ClearableTextField aria-label="到期时间" type="datetime-local" value={expiry} onChange={(e) => setExpiry(e.target.value)} /></Text>
          {!editing && <Text size="1" color="gray">留空按套餐有效天数发放。{selected?.type === "pay_as_you_go" && `同时赠送 ${amount(selected.initial_balance)} 积分初始余额。`}</Text>}
          {expiry && !validTime && <Text size="1" color="red">到期时间须晚于当前时间</Text>}
        </>}
        {action.kind === "reset" && <Select.Root value={window} onValueChange={setWindow}><Select.Trigger aria-label="重置窗口" /><Select.Content>{Object.entries(windows).map(([id, label]) => <Select.Item key={id} value={id}>{label}</Select.Item>)}</Select.Content></Select.Root>}
        {save.isError && <Text role="alert" color="red">{errMsg(save.error)}</Text>}
        <Flex justify="end" gap="2"><Button variant="soft" color="gray" disabled={save.isPending} onClick={onClose}>取消</Button><Button color={action.kind === "delete" ? "red" : undefined} loading={save.isPending} disabled={form && (!selected || !validTime || plans.isError)} onClick={() => save.mutate()}>确认{title}</Button></Flex>
      </Flex>
    </Dialog.Content>
  </Dialog.Root>;
}
