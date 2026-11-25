import { useEffect, useState } from "react";
import { Badge, Box, Button, Code, Dialog, Flex, Select, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { adminApi, type Plan } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

type Form = {
  name: string;
  price: string;
  currency: string;
  type: Plan["type"];
  period_days: string;
  quota_5h: string;
  quota_weekly: string;
  quota_monthly: string;
  allowed_group_ids: string[];
  description: string;
  status: "active" | "archived";
  concurrency_limit: string;
  monthly_spend_limit: string;
  initial_balance: string;
  auto_reload_enabled: boolean;
  auto_reload_threshold: string;
  auto_reload_amount: string;
};

const blankForm = (): Form => ({
  name: "", price: "0", currency: "CNY", type: "quota_subscription", period_days: "30",
  quota_5h: "0", quota_weekly: "0", quota_monthly: "0",
  allowed_group_ids: [], description: "", status: "active",
  concurrency_limit: "0",
  monthly_spend_limit: "0", initial_balance: "0", auto_reload_enabled: false,
  auto_reload_threshold: "0", auto_reload_amount: "0",
});

const fromPlan = (p: Plan): Form => ({
  name: p.name, price: p.price, currency: p.currency, type: p.type ?? "quota_subscription",
  period_days: String(p.period_days),
  quota_5h: p.quota_5h, quota_weekly: p.quota_weekly, quota_monthly: p.quota_monthly,
  allowed_group_ids: p.allowed_group_ids ?? [],
  description: p.description ?? "",
  status: p.status,
  concurrency_limit: String(p.concurrency_limit ?? 0),
  monthly_spend_limit: p.monthly_spend_limit ?? "0",
  initial_balance: p.initial_balance ?? "0",
  auto_reload_enabled: Boolean(p.auto_reload_enabled),
  auto_reload_threshold: p.auto_reload_threshold ?? "0",
  auto_reload_amount: p.auto_reload_amount ?? "0",
});

export default function AdminPlans() {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState("");
  const [selected, setSelected] = useState<Set<string | number>>(new Set());
  const paged = usePagedQuery<Plan>({
    queryKey: ["admin", "plans"],
    queryFn: (params) => adminApi.plansPaged({
      ...params,
      status: statusFilter || undefined,
    }),
    pageSize: 50,
    extras: [statusFilter],
  });
  const { data: groups } = useQuery({ queryKey: ["admin", "groups"], queryFn: adminApi.groups });
  const [dialog, setDialog] = useState<{ mode: "create" } | { mode: "edit"; id: string } | null>(null);
  const [form, setForm] = useState<Form>(blankForm());

  const openCreate = () => { setForm(blankForm()); setDialog({ mode: "create" }); };
  const openEdit = (p: Plan) => { setForm(fromPlan(p)); setDialog({ mode: "edit", id: p.id }); };
  const close = () => setDialog(null);

  const buildBody = (includeStatus: boolean) => {
    const b: any = {
      name: form.name, price: form.price, currency: "CNY",
      type: form.type,
      period_days: parseInt(form.period_days, 10) || 30,
      quota_5h: form.type === "pay_as_you_go" ? "0" : form.quota_5h,
      quota_weekly: form.type === "pay_as_you_go" ? "0" : form.quota_weekly,
      quota_monthly: form.type === "pay_as_you_go" ? "0" : form.quota_monthly,
      allowed_group_ids: form.allowed_group_ids,
      description: form.description,
      concurrency_limit: Math.max(0, parseInt(form.concurrency_limit, 10) || 0),
      monthly_spend_limit: form.type === "pay_as_you_go" ? form.monthly_spend_limit : "0",
      initial_balance: form.type === "pay_as_you_go" ? form.initial_balance : "0",
      auto_reload_enabled: form.type === "pay_as_you_go" && form.auto_reload_enabled,
      auto_reload_threshold: form.type === "pay_as_you_go" ? form.auto_reload_threshold : "0",
      auto_reload_amount: form.type === "pay_as_you_go" ? form.auto_reload_amount : "0",
    };
    if (includeStatus) b.status = form.status;
    return b;
  };

  const create = useMutation({
    mutationFn: () => adminApi.createPlan(buildBody(false)),
    onSuccess: () => { close(); qc.invalidateQueries({ queryKey: ["admin", "plans"] }); },
  });
  const update = useMutation({
    mutationFn: (id: string) => adminApi.updatePlan(id, buildBody(true)),
    onSuccess: () => { close(); qc.invalidateQueries({ queryKey: ["admin", "plans"] }); },
  });
  // archive = soft delete: plan.status -> archived. Already-subscribed
  // users continue to bill against this plan until their UserPlan
  // expires naturally.
  const archive = useMutation({
    mutationFn: (id: string) => adminApi.deletePlan(id),
    onSuccess: () => { setDeleteTarget(null); qc.invalidateQueries({ queryKey: ["admin", "plans"] }); },
  });
  // hardDelete = nuke: drops the plan row and every UserPlan/snapshot
  // referencing it (server-side, in one transaction). The dialog
  // requires the operator to type the plan name as second confirmation.
  const hardDelete = useMutation({
    mutationFn: (id: string) => adminApi.deletePlan(id, { hard: true }),
    onSuccess: () => { setDeleteTarget(null); qc.invalidateQueries({ queryKey: ["admin", "plans"] }); },
  });
  const reactivate = useMutation({
    mutationFn: (id: string) => adminApi.updatePlan(id, { status: "active" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "plans"] }),
  });
  const batchArchive = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.deletePlan(id))),
    onSuccess: () => { setSelected(new Set()); qc.invalidateQueries({ queryKey: ["admin", "plans"] }); },
  });
  const batchReactivate = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.updatePlan(id, { status: "active" }))),
    onSuccess: () => { setSelected(new Set()); qc.invalidateQueries({ queryKey: ["admin", "plans"] }); },
  });

  // null when the delete dialog is closed; holds the targeted plan
  // when open so the dialog can show name + accept type-to-confirm
  // before allowing hard delete.
  const [deleteTarget, setDeleteTarget] = useState<Plan | null>(null);
  const [confirmText, setConfirmText] = useState("");
  useEffect(() => { if (!deleteTarget) setConfirmText(""); }, [deleteTarget]);
  useEffect(() => setSelected(new Set()), [statusFilter, paged.q, paged.page]);

  const toggleGroup = (id: string) => {
    const has = form.allowed_group_ids.includes(id);
    setForm({ ...form, allowed_group_ids: has ? form.allowed_group_ids.filter((x) => x !== id) : [...form.allowed_group_ids, id] });
  };

  const submit = () => {
    if (dialog?.mode === "create") create.mutate();
    else if (dialog?.mode === "edit") update.mutate(dialog.id);
  };

  const columns: Column<Plan>[] = [
    { header: "名称", cell: (p) => p.name },
    {
      header: "类型",
      width: "110px",
      cell: (p) => p.type === "pay_as_you_go"
        ? <Badge variant="soft" color="blue">按量</Badge>
        : <Badge variant="soft" color="iris">额度</Badge>,
    },
    {
      header: "价格",
      width: "120px",
      cell: (p) => <PointAmount amount={p.price} iconSize={12} />,
    },
    {
      header: "周期",
      width: "80px",
      cell: (p) => `${p.period_days}d`,
    },
    {
      header: "5h/周/月",
      cell: (p) => p.type === "pay_as_you_go"
        ? `发放 ${p.initial_balance} / 月上限 ${p.monthly_spend_limit}`
        : `${p.quota_5h} / ${p.quota_weekly} / ${p.quota_monthly}`,
    },
    {
      header: "允许分组",
      width: "100px",
      align: "right",
      cell: (p) => <span className="jc-num">{p.allowed_group_ids?.length || 0}</span>,
    },
    {
      header: "并发",
      width: "90px",
      cell: (p) =>
        p.concurrency_limit > 0
          ? <Badge variant="soft" color="iris" className="jc-num">{p.concurrency_limit}</Badge>
          : <Text size="2" color="gray">不限</Text>,
    },
    {
      header: "状态",
      width: "100px",
      cell: (p) => <Badge color={p.status === "active" ? "green" : "gray"}>{p.status}</Badge>,
    },
    {
      header: "操作",
      width: "150px",
      align: "right",
      cell: (p) => (
        <Flex gap="2" justify="end" wrap="wrap">
          <Button size="1" variant="soft" onClick={() => openEdit(p)}>编辑</Button>
          {p.status === "active" && (
            <Button size="1" color="orange" variant="soft" onClick={() => setDeleteTarget(p)}>
              删除…
            </Button>
          )}
          {p.status === "archived" && (
            <>
              <Button size="1" color="green" variant="soft" onClick={() => reactivate.mutate(p.id)}>
                恢复
              </Button>
              <Button size="1" color="red" variant="soft" onClick={() => setDeleteTarget(p)}>
                彻底删除…
              </Button>
            </>
          )}
        </Flex>
      ),
    },
  ];

  // Reactivate and the two batch actions fire straight off the table with no
  // dialog of their own, so their failures need a line on the page itself.
  const listError = reactivate.error ?? batchArchive.error ?? batchReactivate.error;

  const selectedPlans = paged.items.filter((p) => selected.has(p.id));
  const activeSelected = selectedPlans.filter((p) => p.status === "active").map((p) => p.id);
  const archivedSelected = selectedPlans.filter((p) => p.status === "archived").map((p) => p.id);

  const filters = (
    <Select.Root value={statusFilter || "_any"} onValueChange={(v) => setStatusFilter(v === "_any" ? "" : v)}>
      <Select.Trigger placeholder="状态" />
      <Select.Content>
        <Select.Item value="_any">全部状态</Select.Item>
        <Select.Item value="active">active</Select.Item>
        <Select.Item value="archived">archived</Select.Item>
      </Select.Content>
    </Select.Root>
  );

  const bulkActions = (
    <>
      {activeSelected.length > 0 && (
        <Button size="1" color="orange" variant="soft" loading={batchArchive.isPending}
          onClick={() => { if (confirm(`归档选中的 ${activeSelected.length} 个 active 套餐？`)) batchArchive.mutate(activeSelected); }}>
          批量归档
        </Button>
      )}
      {archivedSelected.length > 0 && (
        <Button size="1" color="green" variant="soft" loading={batchReactivate.isPending}
          onClick={() => batchReactivate.mutate(archivedSelected)}>
          批量恢复
        </Button>
      )}
    </>
  );

  return (
    <AdminPage
      title="套餐管理"
      description="维护面向用户的订阅方案、额度窗口和可用模型分组。"
      actions={<Button onClick={openCreate}>新建套餐</Button>}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="匹配套餐" value={paged.total.toLocaleString()} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="当前页可售" value={paged.items.filter((p) => p.status === "active").length} detail="active" tone="green" />
        <AdminMetricCard label="当前页归档" value={paged.items.filter((p) => p.status === "archived").length} detail="archived" tone="gray" />
        <AdminMetricCard label="已选择" value={selected.size} detail="支持批量归档/恢复" tone={selected.size ? "blue" : "gray"} />
      </AdminMetricGrid>

      {listError != null && (
        <Text size="2" color="red" as="div" mb="2">操作失败: {errMsg(listError)}</Text>
      )}

      <DataTable<Plan>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(p) => p.id}
        searchPlaceholder="搜索套餐名..."
        filters={filters}
        selectedRowKeys={selected}
        onSelectedRowKeysChange={setSelected}
        bulkActions={bulkActions}
      />

      <Dialog.Root open={dialog !== null} onOpenChange={(o) => { if (!o) close(); }}>
        <Dialog.Content style={{ maxWidth: 520 }}>
          <Dialog.Title>{dialog?.mode === "edit" ? "编辑套餐" : "新建套餐"}</Dialog.Title>
          <Flex direction="column" gap="3" mt="3">
            <PlanField label="名称">
              <ClearableTextField placeholder="如 Coding Lite" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </PlanField>
            <PlanField label="套餐类型">
              <Select.Root value={form.type} onValueChange={(v) => setForm({ ...form, type: v as Plan["type"] })}>
                <Select.Trigger />
                <Select.Content>
                  <Select.Item value="quota_subscription">额度套餐</Select.Item>
                  <Select.Item value="pay_as_you_go">按量套餐</Select.Item>
                </Select.Content>
              </Select.Root>
            </PlanField>
            <PlanField label="价格" hint="按平台积分填写（1 积分 = 1 元），例如 99">
              <ClearableTextField placeholder="99" value={form.price}
                onChange={(e) => setForm({ ...form, price: e.target.value })} />
            </PlanField>
            <PlanField label="周期天数" hint="一个计费周期长度,通常 30">
              <ClearableTextField placeholder="30" value={form.period_days}
                onChange={(e) => setForm({ ...form, period_days: e.target.value })} />
            </PlanField>
            {form.type === "quota_subscription" ? (
              <>
                <PlanField label="5h 上限" hint="5 小时滑动窗口的额度上限,0 = 不限">
                  <ClearableTextField placeholder="0" value={form.quota_5h}
                    onChange={(e) => setForm({ ...form, quota_5h: e.target.value })} />
                </PlanField>
                <PlanField label="周上限" hint="7 天累计额度上限,0 = 不限">
                  <ClearableTextField placeholder="0" value={form.quota_weekly}
                    onChange={(e) => setForm({ ...form, quota_weekly: e.target.value })} />
                </PlanField>
                <PlanField label="月上限" hint="30 天累计额度上限,0 = 不限">
                  <ClearableTextField placeholder="0" value={form.quota_monthly}
                    onChange={(e) => setForm({ ...form, quota_monthly: e.target.value })} />
                </PlanField>
              </>
            ) : (
              <>
                <PlanField label="月消费上限" hint="本月按量扣余额的最高消费;0 = 不限">
                  <ClearableTextField placeholder="0" value={form.monthly_spend_limit}
                    onChange={(e) => setForm({ ...form, monthly_spend_limit: e.target.value })} />
                </PlanField>
                <PlanField label="购买后发放额度" hint="购买后一次性充入用户余额，不随套餐到期清零；0 = 不发放">
                  <ClearableTextField placeholder="0" value={form.initial_balance}
                    onChange={(e) => setForm({ ...form, initial_balance: e.target.value })} />
                </PlanField>
                {/* Schema-only. The backend stores these three columns and
                    returns them, but nothing reads them at runtime: there is
                    no job that watches balances or creates the order. Kept so
                    saving a plan doesn't wipe the columns. */}
                <PlanField
                  label="自动充值（未实现）"
                  hint="后端只保存这三个字段，没有扣款逻辑：开启也不会自动下单或扣款。请让用户手动充值。"
                >
                  <Select.Root value={form.auto_reload_enabled ? "on" : "off"} onValueChange={(v) => setForm({ ...form, auto_reload_enabled: v === "on" })}>
                    <Select.Trigger />
                    <Select.Content>
                      <Select.Item value="off">关闭</Select.Item>
                      <Select.Item value="on">开启（当前无效果）</Select.Item>
                    </Select.Content>
                  </Select.Root>
                </PlanField>
                <Flex gap="3" align="end">
                  <Box style={{ flex: 1 }}>
                    <PlanField label="触发阈值" hint="仅保存，暂不生效">
                      <ClearableTextField placeholder="0" value={form.auto_reload_threshold}
                        onChange={(e) => setForm({ ...form, auto_reload_threshold: e.target.value })} />
                    </PlanField>
                  </Box>
                  <Box style={{ flex: 1 }}>
                    <PlanField label="充值金额" hint="仅保存，暂不生效">
                      <ClearableTextField placeholder="0" value={form.auto_reload_amount}
                        onChange={(e) => setForm({ ...form, auto_reload_amount: e.target.value })} />
                    </PlanField>
                  </Box>
                </Flex>
              </>
            )}
            <PlanField label="并发上限" hint="套餐计费分组生效;0 = 不限">
              <ClearableTextField type="number" placeholder="0" value={form.concurrency_limit}
                onChange={(e) => setForm({ ...form, concurrency_limit: e.target.value })} />
            </PlanField>
            <PlanField label="描述" hint="可选,展示给用户">
              <ClearableTextField placeholder="灵活套餐，按需选择适合的开发节奏" value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })} />
            </PlanField>
            {dialog?.mode === "edit" && (
              <PlanField label="状态">
                <Select.Root value={form.status} onValueChange={(v) => setForm({ ...form, status: v as "active" | "archived" })}>
                  <Select.Trigger />
                  <Select.Content>
                    <Select.Item value="active">active</Select.Item>
                    <Select.Item value="archived">archived</Select.Item>
                  </Select.Content>
                </Select.Root>
              </PlanField>
            )}
            <Text size="2" color="gray" mt="2">允许分组（多选；空 = 全部）：</Text>
            <Flex wrap="wrap" gap="2">
              {(groups ?? []).map((g) => (
                <Button key={g.id} size="1"
                  variant={form.allowed_group_ids.includes(g.id) ? "solid" : "outline"}
                  onClick={() => toggleGroup(g.id)}>
                  {g.name}
                </Button>
              ))}
            </Flex>
            {(create.isError || update.isError) && (
              <Text size="2" color="red">保存失败: {errMsg(create.error ?? update.error)}</Text>
            )}
            <Flex gap="2" justify="end" mt="3">
              <Button variant="soft" color="gray" onClick={close}>取消</Button>
              <Button loading={create.isPending || update.isPending} onClick={submit}>
                {dialog?.mode === "edit" ? "保存" : "创建"}
              </Button>
            </Flex>
          </Flex>
        </Dialog.Content>
      </Dialog.Root>

      {/* Delete confirmation dialog: two paths.
            - Archive (orange):  reversible, keeps existing UserPlans alive.
            - Hard delete (red): drops the plan AND every UserPlan / snapshot
              referencing it. Gated by typing the plan name verbatim. */}
      <Dialog.Root open={deleteTarget !== null} onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}>
        <Dialog.Content style={{ maxWidth: 480 }}>
          <Dialog.Title>删除套餐「{deleteTarget?.name}」</Dialog.Title>
          <Flex direction="column" gap="3" mt="2">
            <Box style={{ background: "var(--amber-3)", border: "1px solid var(--amber-6)", padding: 10, borderRadius: 8 }}>
              <Text size="2" weight="medium" as="div" mb="1">归档（推荐）</Text>
              <Text size="1" color="gray" as="div" mb="2">
                状态置为 archived。已订阅用户的套餐继续生效到自然过期；新用户无法订阅。可随时「恢复」。
              </Text>
              <Button
                size="2"
                color="orange"
                variant="soft"
                loading={archive.isPending}
                disabled={deleteTarget?.status !== "active"}
                onClick={() => deleteTarget && archive.mutate(deleteTarget.id)}
              >
                {deleteTarget?.status === "active" ? "归档" : "已是归档状态"}
              </Button>
            </Box>

            <Box style={{ background: "var(--red-3)", border: "1px solid var(--red-6)", padding: 10, borderRadius: 8 }}>
              <Text size="2" weight="medium" as="div" mb="1" color="red">彻底删除（不可恢复）</Text>
              <Text size="1" color="gray" as="div" mb="2">
                直接抹除套餐行 + 所有引用此套餐的 UserPlan 与配额快照。<b>正在订阅这个套餐的用户会立即失去套餐</b>，
                但他们的余额扣费 / 历史账单 / 使用日志保留。Redis 配额计数器靠 TTL 自然过期。
              </Text>
              <Text size="1" as="div" mb="1">
                输入套餐名 <Code>{deleteTarget?.name}</Code> 二次确认：
              </Text>
              <ClearableTextField
                value={confirmText}
                onChange={(e) => setConfirmText(e.target.value)}
                placeholder={deleteTarget?.name}
                style={{ fontFamily: "ui-monospace, monospace" }}
              />
              <Box mt="2">
                <Button
                  size="2"
                  color="red"
                  loading={hardDelete.isPending}
                  disabled={!deleteTarget || confirmText !== deleteTarget.name}
                  onClick={() => deleteTarget && hardDelete.mutate(deleteTarget.id)}
                >
                  彻底删除
                </Button>
              </Box>
            </Box>

            {(archive.isError || hardDelete.isError) && (
              <Text color="red" size="2">{errMsg(archive.error ?? hardDelete.error)}</Text>
            )}

            <Flex justify="end" mt="1">
              <Button variant="soft" color="gray" onClick={() => setDeleteTarget(null)}>关闭</Button>
            </Flex>
          </Flex>
        </Dialog.Content>
      </Dialog.Root>
    </AdminPage>
  );
}

function PlanField({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <Box>
      <Text size="1" weight="medium" as="div" mb="1">{label}</Text>
      {children}
      {hint && <Text size="1" color="gray" as="div" mt="1">{hint}</Text>}
    </Box>
  );
}
