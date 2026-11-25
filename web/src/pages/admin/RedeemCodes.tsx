import { useState } from "react";
import { Badge, Box, Button, Callout, Code, Flex, Select, Tooltip } from "@radix-ui/themes";
import { Check } from "lucide-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { adminApi, type Plan, type RedeemCode } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";
import { CancelButton, Field, FieldGrid, FormDialog } from "../../components/form/FormLayout";

export default function RedeemCodes() {
  const qc = useQueryClient();
  const { data: plans } = useQuery({ queryKey: ["admin", "plans"], queryFn: adminApi.plans });

  const [status, setStatus] = useState("");
  const [kind, setKind] = useState("");
  const [batchId, setBatchId] = useState("");
  const [selected, setSelected] = useState<Set<string | number>>(new Set());

  const paged = usePagedQuery<RedeemCode>({
    queryKey: ["admin", "redeem-codes"],
    queryFn: (p) => adminApi.redeemCodesPaged({
      ...p,
      status: status || undefined,
      kind: kind || undefined,
      batch_id: batchId || undefined,
    }),
    pageSize: 50,
    extras: [status, kind, batchId],
  });

  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({
    kind: "balance" as "balance" | "plan",
    amount: "1.00",
    plan_id: "",
    count: 5,
    max_uses: 1,
    note: "",
    expires_at: "",
  });
  const [genResult, setGenResult] = useState<{ codes: string[]; batch_id: string } | null>(null);

  const gen = useMutation({
    mutationFn: () => {
      const body: any = {
        kind: form.kind,
        count: form.count,
        max_uses: form.max_uses,
        note: form.note,
      };
      if (form.kind === "balance") {
        body.amount = form.amount;
        body.currency = "CNY"; // 面额即积分（1 积分 = 1 元）
      } else {
        body.plan_id = form.plan_id;
      }
      if (form.expires_at) body.expires_at = new Date(form.expires_at).toISOString();
      return adminApi.generateRedeemCodes(body);
    },
    onSuccess: (r) => {
      setGenResult({ codes: r.codes.map((c) => c.code), batch_id: r.batch_id });
      setOpen(false);
      qc.invalidateQueries({ queryKey: ["admin", "redeem-codes"] });
    },
  });

  const revoke = useMutation({
    mutationFn: (id: string) => adminApi.revokeRedeemCode(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "redeem-codes"] }),
  });
  const revokeBatch = useMutation({
    mutationFn: (b: string) => adminApi.revokeRedeemBatch(b),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "redeem-codes"] }),
  });
  const batchRevoke = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.revokeRedeemCode(id))),
    onSuccess: () => { setSelected(new Set()); qc.invalidateQueries({ queryKey: ["admin", "redeem-codes"] }); },
  });

  const planMap = new Map<string, Plan>();
  (plans ?? []).forEach((p) => planMap.set(p.id, p));

  const statusColor = (s: string) =>
    s === "active" ? "green" : s === "exhausted" ? "gray" : s === "revoked" ? "red" : "orange";

  const columns: Column<RedeemCode>[] = [
    {
      header: "code",
      cell: (c) => <Code>{c.code}</Code>,
    },
    {
      header: "类型",
      width: "90px",
      cell: (c) => <Badge variant="soft">{c.kind}</Badge>,
    },
    {
      header: "面额",
      cell: (c) =>
        c.kind === "balance"
          ? <span className="jc-mono"><PointAmount amount={c.amount} iconSize={12} /></span>
          : <span>{planMap.get(c.plan_id ?? "")?.name ?? c.plan_id ?? "—"}</span>,
    },
    {
      header: "状态",
      width: "100px",
      cell: (c) => <Badge color={statusColor(c.status)}>{c.status}</Badge>,
    },
    {
      header: "used / max",
      width: "100px",
      cell: (c) => <span className="jc-num">{c.used_count} / {c.max_uses}</span>,
    },
    {
      header: "批次",
      width: "140px",
      cell: (c) => (
        <Tooltip content="点击只看这个批次">
          <Code
            style={{ cursor: "pointer" }}
            onClick={() => setBatchId(c.batch_id)}
          >{c.batch_id.slice(0, 8)}</Code>
        </Tooltip>
      ),
    },
    {
      header: "过期",
      width: "120px",
      cell: (c) => (c.expires_at ? new Date(c.expires_at).toLocaleDateString() : "—"),
    },
    {
      header: "操作",
      width: "120px",
      align: "right",
      cell: (c) => (
        c.status === "active" && (
          <Button size="1" color="red" variant="soft" onClick={() => revoke.mutate(c.id)}>撤销</Button>
        )
      ),
    },
  ];

  const filters = (
    <>
      <Select.Root value={status || "_any"} onValueChange={(v) => setStatus(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="状态" />
        <Select.Content>
          <Select.Item value="_any">全部状态</Select.Item>
          <Select.Item value="active">active</Select.Item>
          <Select.Item value="exhausted">exhausted</Select.Item>
          <Select.Item value="revoked">revoked</Select.Item>
          <Select.Item value="expired">expired</Select.Item>
        </Select.Content>
      </Select.Root>
      <Select.Root value={kind || "_any"} onValueChange={(v) => setKind(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="类型" />
        <Select.Content>
          <Select.Item value="_any">全部类型</Select.Item>
          <Select.Item value="balance">balance</Select.Item>
          <Select.Item value="plan">plan</Select.Item>
        </Select.Content>
      </Select.Root>
      {batchId && (
        <Flex align="center" gap="1">
          <Badge color="iris" variant="soft">
            批次 {batchId.slice(0, 8)}
          </Badge>
          <Button size="1" variant="soft" color="gray" onClick={() => setBatchId("")}>清除</Button>
          <Button
            size="1"
            color="red"
            variant="soft"
            onClick={() => {
              if (confirm(`撤销整批 ${batchId.slice(0, 8)}？`)) revokeBatch.mutate(batchId);
            }}
          >撤销整批</Button>
        </Flex>
      )}
    </>
  );

  const selectedCodes = paged.items.filter((c) => selected.has(c.id));
  const activeSelectedIds = selectedCodes.filter((c) => c.status === "active").map((c) => c.id);

  return (
    <AdminPage
      title="兑换码"
      description="批量生成、筛选、按批次追踪和撤销兑换码。"
      actions={<Button onClick={() => setOpen(true)}>批量生成</Button>}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="匹配兑换码" value={paged.total.toLocaleString()} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="当前页可用" value={paged.items.filter((c) => c.status === "active").length} detail="active" tone="green" />
        <AdminMetricCard label="当前页失效" value={paged.items.filter((c) => c.status !== "active").length} detail="revoked / expired / exhausted" tone="amber" />
        <AdminMetricCard label="已选择" value={selected.size} detail="支持批量撤销" tone={selected.size ? "blue" : "gray"} />
      </AdminMetricGrid>

      {genResult && (
        <Callout.Root color="green" variant="surface" mb="4">
          <Callout.Icon><Check size={16} /></Callout.Icon>
          <Box style={{ flex: 1, minWidth: 0 }}>
            <Callout.Text>
              已生成 {genResult.codes.length} 个兑换码，批次 <Code>{genResult.batch_id}</Code>。请妥善保存，下方列表也可查看。
            </Callout.Text>
            <Box mt="2" className="jc-mono" style={{ maxHeight: 120, overflow: "auto", fontSize: "var(--text-body-sm)", background: "var(--c-surface-container-lowest)", border: "1px solid var(--c-outline-variant)", padding: 8, borderRadius: "var(--radius-control)" }}>
              {genResult.codes.map((c) => <div key={c}>{c}</div>)}
            </Box>
            <Flex gap="2" mt="2">
              <Button size="1" variant="soft" color="gray" onClick={() => setBatchId(genResult.batch_id)}>
                只看本批次
              </Button>
              <Button size="1" variant="soft" color="gray" onClick={() => setGenResult(null)}>关闭</Button>
            </Flex>
          </Box>
        </Callout.Root>
      )}

      <DataTable<RedeemCode>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(c) => c.id}
        searchPlaceholder="搜索 code / batch..."
        filters={filters}
        selectedRowKeys={selected}
        onSelectedRowKeysChange={setSelected}
        bulkActions={activeSelectedIds.length > 0 && (
          <Button size="1" color="red" variant="soft" loading={batchRevoke.isPending}
            onClick={() => { if (confirm(`撤销选中的 ${activeSelectedIds.length} 个 active 兑换码？`)) batchRevoke.mutate(activeSelectedIds); }}>
            批量撤销
          </Button>
        )}
      />

      <FormDialog
        open={open}
        onOpenChange={setOpen}
        size="sm"
        title="批量生成兑换码"
        description="同一批生成的码共用一个批次号，可按批次筛选与整批撤销。"
        error={gen.isError ? errMsg(gen.error) : undefined}
        footer={
          <>
            <CancelButton />
            <Button loading={gen.isPending} onClick={() => gen.mutate()}>生成</Button>
          </>
        }
      >
        <FieldGrid>
          <Field label="类型" span="full">
            <Select.Root value={form.kind} onValueChange={(v) => setForm({ ...form, kind: v as any })}>
              <Select.Trigger />
              <Select.Content>
                <Select.Item value="balance">余额</Select.Item>
                <Select.Item value="plan">套餐</Select.Item>
              </Select.Content>
            </Select.Root>
          </Field>
          {form.kind === "balance" ? (
            <Field label="每码金额" required hint="单位积分，1 积分 = 1 元。" span="full">
              <ClearableTextField className="jc-num" placeholder="5.00" value={form.amount} onChange={(e) => setForm({ ...form, amount: e.target.value })} />
            </Field>
          ) : (
            <Field label="套餐" required span="full">
              <Select.Root value={form.plan_id} onValueChange={(v) => setForm({ ...form, plan_id: v })}>
                <Select.Trigger placeholder="选择套餐" />
                <Select.Content>
                  {(plans ?? []).map((p) => <Select.Item key={p.id} value={p.id}>{p.name}</Select.Item>)}
                </Select.Content>
              </Select.Root>
            </Field>
          )}
          <Field label="生成数量" required>
            <ClearableTextField className="jc-num" type="number" min="1" value={String(form.count)} onChange={(e) => setForm({ ...form, count: parseInt(e.target.value) || 1 })} />
          </Field>
          <Field label="每码兑换次数" hint="默认 1 次。">
            <ClearableTextField className="jc-num" type="number" min="1" value={String(form.max_uses)} onChange={(e) => setForm({ ...form, max_uses: parseInt(e.target.value) || 1 })} />
          </Field>
          <Field label="过期时间" hint="留空永久有效。" span="full">
            <ClearableTextField type="datetime-local" value={form.expires_at} onChange={(e) => setForm({ ...form, expires_at: e.target.value })} />
          </Field>
          <Field label="备注" hint="仅管理端可见。" span="full">
            <ClearableTextField placeholder="如：活动赠送" value={form.note} onChange={(e) => setForm({ ...form, note: e.target.value })} />
          </Field>
        </FieldGrid>
      </FormDialog>
    </AdminPage>
  );
}
