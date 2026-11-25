import { useState } from "react";
import { Badge, Box, Button, Callout, Dialog, Flex, Progress, SegmentedControl, Select, Text, TextField } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { adminApi, type AdminResettableUserPlan, type PlanResetFilter, type PlanResetJob } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { AdminPage, AdminSection } from "../../components/AdminPage";

const windows = { all: "全部窗口", "5h": "5 小时", weekly: "7 天", monthly: "30 天" };
const actions: Record<PlanResetJob["action"], { label: string; hint: string }> = {
  clear: { label: "清零额度", hint: "清零选定窗口的已用额度，重置时间不变。" },
  restart: { label: "提前重置", hint: "选定窗口立即重置，之后的重置时间从现在起算。开始时间和到期时间不变。" },
  grant: { label: "备用重置", hint: "给每个套餐发放一次备用重置。用户在用量页的统计中自行使用，使用时清零选定窗口的已用额度；套餐失效后作废。" },
};
// A running job saves progress every few seconds; one silent for this long
// lost its server (deploy or crash) and will not finish.
const interrupted = (j: PlanResetJob) => j.status === "running" && Date.now() - new Date(j.updated_at).getTime() > 60_000;
const countBy = (keys: string[]) => keys.reduce<Record<string, number>>((m, k) => ({ ...m, [k]: (m[k] ?? 0) + 1 }), {});
const amount = (value: string) => Number(value).toLocaleString(undefined, { maximumFractionDigits: 2 });
// Date inputs give a local calendar day; the range covers whole days.
const dayStart = (day: string, offsetDays = 0) => {
  const d = new Date(`${day}T00:00`);
  d.setDate(d.getDate() + offsetDays);
  return d.toISOString();
};

export default function AdminUserPlans() {
  const [planId, setPlanId] = useState("");
  const [expireFrom, setExpireFrom] = useState("");
  const [expireTo, setExpireTo] = useState("");
  const [selected, setSelected] = useState<Set<string | number>>(new Set());
  const [allMatching, setAllMatching] = useState(false);
  const [resetOpen, setResetOpen] = useState(false);
  const plans = useQuery({ queryKey: ["admin", "plans"], queryFn: adminApi.plans });
  const subscriptionPlans = plans.data?.filter((p) => p.type === "quota_subscription") ?? [];

  const filterParams = {
    plan_id: planId || undefined,
    expire_after: expireFrom ? dayStart(expireFrom) : undefined,
    expire_before: expireTo ? dayStart(expireTo, 1) : undefined,
  };
  const paged = usePagedQuery<AdminResettableUserPlan>({
    queryKey: ["admin", "user-plans"],
    queryFn: (params) => adminApi.resettableUserPlansPaged({ ...params, ...filterParams }),
    extras: [planId, expireFrom, expireTo],
  });
  // A changed filter changes what "all matching" means; drop it rather than
  // silently widen or narrow a selection the operator already looked at.
  const setFilter = (fn: () => void) => { fn(); setAllMatching(false); };
  const filter: PlanResetFilter = { q: paged.q || undefined, ...filterParams };
  const count = allMatching ? paged.total : selected.size;
  // Until the list catches up with the filter, paged.total still counts the
  // previous one while `filter` already carries the new values.
  const settling = paged.query.isFetching || paged.rawQ.trim() !== paged.q;

  const columns: Column<AdminResettableUserPlan>[] = [
    {
      header: "用户",
      cell: (p) => <Flex direction="column"><Text size="2">{p.email || p.username}</Text>{p.email && p.username && <Text size="1" color="gray">{p.username}</Text>}</Flex>,
    },
    { header: "套餐", cell: (p) => <Text size="2">{p.plan_name}</Text> },
    {
      header: "已用（5 小时 / 7 天 / 30 天）",
      cell: (p) => <Text size="2" className="jc-num">{amount(p.used_5h)} / {amount(p.used_weekly)} / {amount(p.used_monthly)}</Text>,
    },
    {
      header: "备用重置",
      cell: (p) => p.banked_resets.length
        ? <Text size="2">{Object.entries(countBy(p.banked_resets.map((r) => r.window))).map(([w, n]) => `${windows[w as keyof typeof windows]} ×${n}`).join("，")}</Text>
        : <Text size="2" color="gray">—</Text>,
    },
    {
      header: "有效期",
      cell: (p) => <Text size="1" color="gray">{new Date(p.start_at).toLocaleDateString()} — {new Date(p.expire_at).toLocaleDateString()}</Text>,
    },
  ];

  const filters = (
    <Flex gap="2" wrap="wrap" align="center">
      <Select.Root value={planId || "_any"} onValueChange={(v) => setFilter(() => setPlanId(v === "_any" ? "" : v))}>
        <Select.Trigger placeholder="套餐" />
        <Select.Content>
          <Select.Item value="_any">全部套餐</Select.Item>
          {subscriptionPlans.map((p) => <Select.Item key={p.id} value={p.id}>{p.name}</Select.Item>)}
        </Select.Content>
      </Select.Root>
      <Text size="1" color="gray">到期</Text>
      <TextField.Root type="date" aria-label="到期起始日" value={expireFrom} onChange={(e) => setFilter(() => setExpireFrom(e.target.value))} />
      <Text size="1" color="gray">至</Text>
      <TextField.Root type="date" aria-label="到期截止日" value={expireTo} onChange={(e) => setFilter(() => setExpireTo(e.target.value))} />
    </Flex>
  );

  return (
    <AdminPage
      title="用户套餐"
      description="有效的订阅套餐。筛选并选择后，可批量清零已用额度、提前自然重置或发放备用重置，任务在服务端后台执行。"
      actions={<Button disabled={count === 0 || (allMatching && settling)} onClick={() => setResetOpen(true)}>批量重置{count > 0 ? `（${count}）` : ""}</Button>}
    >
      <Flex gap="2" align="center" mb="2" wrap="wrap">
        {allMatching
          ? <><Badge color="iris" variant="soft">已选择筛选结果全部 {paged.total.toLocaleString()} 条</Badge><Button size="1" variant="ghost" onClick={() => setAllMatching(false)}>取消</Button></>
          : <Button size="1" variant="soft" disabled={paged.total === 0 || settling} onClick={() => { setSelected(new Set()); setAllMatching(true); }}>选择筛选结果全部 {paged.total.toLocaleString()} 条</Button>}
      </Flex>
      <DataTable<AdminResettableUserPlan>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(p) => p.user_plan_id}
        searchPlaceholder="搜索用户邮箱、用户名、昵称..."
        setRawQ={(v) => setFilter(() => paged.setRawQ(v))}
        filters={filters}
        selectedRowKeys={allMatching ? undefined : selected}
        onSelectedRowKeysChange={allMatching ? undefined : setSelected}
      />
      <ResetJobs />
      {resetOpen && <ResetDialog count={count} target={allMatching ? { filter } : { ids: [...selected].map(String) }}
        onClose={() => setResetOpen(false)}
        onStarted={() => { setResetOpen(false); setSelected(new Set()); setAllMatching(false); }} />}
    </AdminPage>
  );
}

function ResetDialog({ count, target, onClose, onStarted }: {
  count: number;
  target: { ids: string[] } | { filter: PlanResetFilter };
  onClose: () => void;
  onStarted: () => void;
}) {
  const qc = useQueryClient();
  const [action, setAction] = useState<PlanResetJob["action"]>("clear");
  const [window, setWindow] = useState("all");
  const start = useMutation({
    mutationFn: () => adminApi.startPlanReset({ action, window, ...target }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["admin", "plan-reset-jobs"] }); onStarted(); },
  });
  return <Dialog.Root open onOpenChange={(open) => { if (!open) onClose(); }}>
    <Dialog.Content style={{ maxWidth: 480 }}>
      <Dialog.Title>批量重置 {count.toLocaleString()} 个套餐</Dialog.Title>
      <Flex direction="column" gap="3">
        <SegmentedControl.Root value={action} onValueChange={(v) => setAction(v as PlanResetJob["action"])}>
          {Object.entries(actions).map(([id, a]) => <SegmentedControl.Item key={id} value={id}>{a.label}</SegmentedControl.Item>)}
        </SegmentedControl.Root>
        <Text size="2" color="gray">{actions[action].hint}</Text>
        <Select.Root value={window} onValueChange={setWindow}>
          <Select.Trigger aria-label="重置窗口" />
          <Select.Content>{Object.entries(windows).map(([id, label]) => <Select.Item key={id} value={id}>{label}</Select.Item>)}</Select.Content>
        </Select.Root>
        <Text size="1" color="gray">执行时只处理仍然有效的订阅套餐；提交后在后台执行，进度见下方任务列表。</Text>
        {start.isError && <Text size="2" color="red" role="alert">{errMsg(start.error)}</Text>}
        <Flex gap="2" justify="end">
          <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
          <Button color="red" loading={start.isPending} onClick={() => start.mutate()}>确认重置</Button>
        </Flex>
      </Flex>
    </Dialog.Content>
  </Dialog.Root>;
}

function ResetJobs() {
  const jobs = useQuery({
    queryKey: ["admin", "plan-reset-jobs"],
    queryFn: adminApi.planResetJobs,
    refetchInterval: (q) => q.state.data?.some((j) => j.status === "running" && !interrupted(j)) ? 2000 : false,
  });
  if (!jobs.data?.length) return null;
  return <AdminSection title="重置任务" description="保留最近的任务 7 天。">
    <Flex direction="column" gap="3">
      {jobs.data.map((j) => <Box key={j.id}>
        <Flex justify="between" gap="2" wrap="wrap">
          {/* Restart jobs from before windows were selectable reset every window. */}
          <Text size="2">{actions[j.action]?.label ?? j.action} · {windows[(j.window || "all") as keyof typeof windows] ?? j.window} · {new Date(j.created_at).toLocaleString()}</Text>
          <Text size="1" color={j.failed > 0 ? "red" : "gray"}>
            {interrupted(j) ? (j.action === "grant" ? "已中断，重新提交会给已处理的套餐重复发放" : "已中断，可重新提交") : j.status === "running" ? "执行中" : "已完成"} · {j.done}/{j.total}{j.failed > 0 ? ` · 失败 ${j.failed}` : ""}
          </Text>
        </Flex>
        <Progress mt="1" value={j.total ? (j.done / j.total) * 100 : 100} color={j.failed > 0 ? "red" : "blue"} />
        {j.errors.length > 0 && <Callout.Root color="red" size="1" mt="2"><Callout.Text>{j.errors.join("；")}</Callout.Text></Callout.Root>}
      </Box>)}
    </Flex>
  </AdminSection>;
}
