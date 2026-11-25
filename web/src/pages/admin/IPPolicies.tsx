import { useState } from "react";
import { Badge, Box, Button, Code, Dialog, Flex, Select, Text, TextField } from "@radix-ui/themes";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { adminApi, type IPPolicy, type IPPolicyKind } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

const KIND_LABEL: Record<IPPolicyKind, string> = {
  trusted_proxy: "可信代理",
  allow: "白名单",
  block: "黑名单",
};

const KIND_COLOR: Record<IPPolicyKind, "blue" | "green" | "red"> = {
  trusted_proxy: "blue",
  allow: "green",
  block: "red",
};

const KINDS: IPPolicyKind[] = ["trusted_proxy", "allow", "block"];

const blankForm = () => ({ cidr: "", kind: "block" as IPPolicyKind, note: "", expires_at: "" });

function isExpired(p: IPPolicy): boolean {
  return !!p.expires_at && new Date(p.expires_at).getTime() <= Date.now();
}

export default function IPPolicies() {
  const qc = useQueryClient();
  const [kindFilter, setKindFilter] = useState<IPPolicyKind | "">("");
  const [dialogOpen, setDialogOpen] = useState(false);
  const [form, setForm] = useState(blankForm());
  const [selected, setSelected] = useState<Set<string | number>>(new Set());

  // The endpoint returns the whole list in one shot (it is a hand-curated
  // set, not user-generated volume), so search and paging happen here.
  const paged = usePagedQuery<IPPolicy>({
    queryKey: ["admin", "ip-policies"],
    queryFn: async ({ q, limit, offset }) => {
      const rows = await adminApi.ipPolicies(kindFilter ? { kind: kindFilter } : {});
      const needle = q.toLowerCase();
      const matched = needle
        ? rows.filter((r) => r.cidr.toLowerCase().includes(needle) || (r.note ?? "").toLowerCase().includes(needle))
        : rows;
      return { items: matched.slice(offset, offset + limit), total: matched.length, limit, offset };
    },
    extras: [kindFilter],
    pageSize: 50,
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin", "ip-policies"] });

  const create = useMutation({
    mutationFn: () =>
      adminApi.createIPPolicy({
        cidr: form.cidr.trim(),
        kind: form.kind,
        note: form.note.trim(),
        expires_at: form.expires_at ? new Date(form.expires_at).toISOString() : undefined,
      }),
    onSuccess: () => { setDialogOpen(false); invalidate(); },
  });
  const del = useMutation({
    mutationFn: (id: string) => adminApi.deleteIPPolicy(id),
    onSuccess: invalidate,
  });
  const batchDelete = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.deleteIPPolicy(id))),
    onSuccess: () => { setSelected(new Set()); invalidate(); },
  });

  const openCreate = () => { setForm(blankForm()); create.reset(); setDialogOpen(true); };

  const columns: Column<IPPolicy>[] = [
    {
      header: "CIDR / IP",
      minWidth: "180px",
      cell: (p) => <Code>{p.cidr}</Code>,
    },
    {
      header: "类型",
      width: "110px",
      cell: (p) => <Badge color={KIND_COLOR[p.kind] ?? "gray"}>{KIND_LABEL[p.kind] ?? p.kind}</Badge>,
    },
    {
      header: "备注",
      minWidth: "180px",
      wrap: true,
      cell: (p) => <Text size="2" color="gray">{p.note || "—"}</Text>,
    },
    {
      header: "过期",
      minWidth: "200px",
      wrap: true,
      cell: (p) =>
        p.expires_at ? (
          <Flex gap="2" align="center" wrap="wrap">
            <Text size="2">{new Date(p.expires_at).toLocaleString()}</Text>
            {isExpired(p) && <Badge color="gray" variant="soft">已过期</Badge>}
          </Flex>
        ) : (
          <Text size="2" color="gray">永久</Text>
        ),
    },
    {
      header: "创建时间",
      minWidth: "170px",
      cell: (p) => <Text size="2" color="gray">{new Date(p.created_at).toLocaleString()}</Text>,
    },
    {
      header: "操作",
      width: "90px",
      align: "right",
      cell: (p) => (
        <Button size="1" color="red" variant="soft" onClick={() => {
          if (confirm(`删除策略 ${p.cidr}（${KIND_LABEL[p.kind] ?? p.kind}）？`)) del.mutate(p.id);
        }}>
          删除
        </Button>
      ),
    },
  ];

  const rows = paged.items;
  const selectedIds = rows.filter((p) => selected.has(p.id)).map((p) => p.id);
  const countOf = (kind: IPPolicyKind) => rows.filter((p) => p.kind === kind).length;

  return (
    <AdminPage
      title="IP 策略"
      description="可信代理决定取哪一段 XFF 作为真实客户端 IP；白名单放行，黑名单直接拒绝。改动约 30 秒后在网关生效。"
      actions={<Button onClick={openCreate}>新建策略</Button>}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="匹配条目" value={paged.total.toLocaleString()} detail="当前筛选" tone="iris" />
        <AdminMetricCard label="可信代理" value={countOf("trusted_proxy")} detail="当前页" tone="blue" />
        <AdminMetricCard label="白名单" value={countOf("allow")} detail="当前页" tone="green" />
        <AdminMetricCard label="黑名单" value={countOf("block")} detail="当前页" tone="red" />
      </AdminMetricGrid>

      {paged.query.isError && (
        <Text as="div" color="red" size="2" mb="3">
          加载 IP 策略失败：{errMsg(paged.query.error)}。请点击刷新重试。
        </Text>
      )}
      {del.isError && <Text as="div" color="red" size="2" mb="3">删除失败：{errMsg(del.error)}</Text>}
      {batchDelete.isError && <Text as="div" color="red" size="2" mb="3">批量删除失败：{errMsg(batchDelete.error)}</Text>}

      <DataTable<IPPolicy>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(p) => p.id}
        searchPlaceholder="搜索 CIDR / 备注..."
        emptyText="暂无 IP 策略 — 点击右上「新建策略」"
        selectedRowKeys={selected}
        onSelectedRowKeysChange={setSelected}
        filters={
          <Select.Root value={kindFilter || "_any"} onValueChange={(v) => setKindFilter(v === "_any" ? "" : (v as IPPolicyKind))}>
            <Select.Trigger placeholder="类型" />
            <Select.Content>
              <Select.Item value="_any">全部类型</Select.Item>
              {KINDS.map((k) => <Select.Item key={k} value={k}>{KIND_LABEL[k]}</Select.Item>)}
            </Select.Content>
          </Select.Root>
        }
        bulkActions={
          <Button size="1" color="red" variant="soft" loading={batchDelete.isPending}
            onClick={() => { if (confirm(`删除选中的 ${selectedIds.length} 条策略？`)) batchDelete.mutate(selectedIds); }}>
            批量删除
          </Button>
        }
      />

      <Dialog.Root open={dialogOpen} onOpenChange={setDialogOpen}>
        <Dialog.Content style={{ maxWidth: 460 }}>
          <Dialog.Title>新建 IP 策略</Dialog.Title>
          <Flex direction="column" gap="3" mt="3">
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">CIDR 或单个 IP</Text>
              <ClearableTextField placeholder="203.0.113.7 或 10.0.0.0/8" value={form.cidr}
                onChange={(e) => setForm({ ...form, cidr: e.target.value })}
                style={{ fontFamily: "ui-monospace, monospace" }} />
              <Text size="1" color="gray" as="div" mt="1">IPv4 / IPv6 都支持；不带掩码按单个地址处理</Text>
            </Box>
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">类型</Text>
              <Select.Root value={form.kind} onValueChange={(v) => setForm({ ...form, kind: v as IPPolicyKind })}>
                <Select.Trigger style={{ width: "100%" }} />
                <Select.Content>
                  {KINDS.map((k) => <Select.Item key={k} value={k}>{KIND_LABEL[k]}（{k}）</Select.Item>)}
                </Select.Content>
              </Select.Root>
            </Box>
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">备注（可选）</Text>
              <ClearableTextField placeholder="为什么加这条" value={form.note}
                onChange={(e) => setForm({ ...form, note: e.target.value })} />
            </Box>
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">过期时间（可选）</Text>
              <TextField.Root type="datetime-local" value={form.expires_at}
                onChange={(e) => setForm({ ...form, expires_at: e.target.value })} />
              <Text size="1" color="gray" as="div" mt="1">留空表示永久生效</Text>
            </Box>
            {create.isError && <Text size="2" color="red">{errMsg(create.error)}</Text>}
            <Flex gap="2" justify="end" mt="2">
              <Button variant="soft" color="gray" onClick={() => setDialogOpen(false)}>取消</Button>
              <Button disabled={!form.cidr.trim()} loading={create.isPending} onClick={() => create.mutate()}>创建</Button>
            </Flex>
          </Flex>
        </Dialog.Content>
      </Dialog.Root>
    </AdminPage>
  );
}
