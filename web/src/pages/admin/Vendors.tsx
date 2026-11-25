import { useState } from "react";
import { Badge, Box, Button, Code, Dialog, Flex, Text } from "@radix-ui/themes";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { adminApi, type Vendor } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

type DialogState = { mode: "create" } | { mode: "edit"; vendor: Vendor } | null;

const blankForm = () => ({ name: "", slug: "", description: "", sort_order: "0" });

export default function Vendors() {
  const qc = useQueryClient();
  const paged = usePagedQuery<Vendor>({
    queryKey: ["admin", "vendors"],
    queryFn: (params) => adminApi.vendorsPaged(params),
    pageSize: 50,
  });
  const [dialog, setDialog] = useState<DialogState>(null);
  const [form, setForm] = useState(blankForm());
  const [selected, setSelected] = useState<Set<string | number>>(new Set());

  const openCreate = () => { setForm(blankForm()); setDialog({ mode: "create" }); };
  const openEdit = (v: Vendor) => {
    setForm({ name: v.name, slug: v.slug, description: v.description ?? "", sort_order: String(v.sort_order) });
    setDialog({ mode: "edit", vendor: v });
  };
  const close = () => setDialog(null);

  const buildBody = () => ({
    name: form.name.trim(),
    slug: form.slug.trim() || undefined,
    description: form.description.trim(),
    sort_order: parseInt(form.sort_order, 10) || 0,
  });

  const create = useMutation({
    mutationFn: () => adminApi.createVendor(buildBody()),
    onSuccess: () => { close(); qc.invalidateQueries({ queryKey: ["admin", "vendors"] }); },
  });
  const update = useMutation({
    mutationFn: (id: string) => adminApi.updateVendor(id, buildBody()),
    onSuccess: () => { close(); qc.invalidateQueries({ queryKey: ["admin", "vendors"] }); },
  });
  const del = useMutation({
    mutationFn: (id: string) => adminApi.deleteVendor(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "vendors"] }),
  });
  const batchDelete = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.deleteVendor(id))),
    onSuccess: () => { setSelected(new Set()); qc.invalidateQueries({ queryKey: ["admin", "vendors"] }); },
  });

  const submit = () => {
    if (!form.name.trim()) return;
    if (dialog?.mode === "create") create.mutate();
    else if (dialog?.mode === "edit") update.mutate(dialog.vendor.id);
  };

  const columns: Column<Vendor>[] = [
    {
      header: "名称",
      cell: (v) => <Text weight="medium">{v.name}</Text>,
    },
    {
      header: "slug",
      width: "160px",
      cell: (v) => <Code>{v.slug}</Code>,
    },
    {
      header: "描述",
      cell: (v) => <Text size="2" color="gray">{v.description || "—"}</Text>,
    },
    {
      header: "排序",
      width: "80px",
      cell: (v) => <Badge variant="soft">{v.sort_order}</Badge>,
    },
    {
      header: "操作",
      width: "140px",
      align: "right",
      cell: (v) => (
        <Flex gap="2" justify="end">
          <Button size="1" variant="soft" onClick={() => openEdit(v)}>编辑</Button>
          <Button size="1" color="red" variant="soft" onClick={() => {
            if (confirm(`删除厂商 "${v.name}"？相关模型将被自动取消关联。`)) del.mutate(v.id);
          }}>删除</Button>
        </Flex>
      ),
    },
  ];

  const selectedIds = paged.items.filter((v) => selected.has(v.id)).map((v) => v.id);

  return (
    <AdminPage
      title="模型厂商"
      description="管理模型背后的品牌，供公开模型列表和后台模型表按厂商筛选。"
      actions={<Button onClick={openCreate}>新建厂商</Button>}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="匹配厂商" value={paged.total.toLocaleString()} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="当前页" value={paged.items.length} detail="本页展示" tone="blue" />
        <AdminMetricCard label="已选择" value={selected.size} detail="支持批量删除" tone={selected.size ? "amber" : "gray"} />
        <AdminMetricCard label="排序最小值" value={paged.items.length ? Math.min(...paged.items.map((v) => v.sort_order)) : "—"} detail="sort_order" />
      </AdminMetricGrid>

      <DataTable<Vendor>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(v) => v.id}
        searchPlaceholder="搜索名称 / slug..."
        emptyText="尚未创建厂商 — 点击右上「新建厂商」"
        selectedRowKeys={selected}
        onSelectedRowKeysChange={setSelected}
        bulkActions={
          <Button size="1" color="red" variant="soft" loading={batchDelete.isPending}
            onClick={() => { if (confirm(`删除选中的 ${selectedIds.length} 个厂商？相关模型将被自动取消关联。`)) batchDelete.mutate(selectedIds); }}>
            批量删除
          </Button>
        }
      />

      <Dialog.Root open={dialog !== null} onOpenChange={(o) => { if (!o) close(); }}>
        <Dialog.Content style={{ maxWidth: 460 }}>
          <Dialog.Title>{dialog?.mode === "edit" ? "编辑厂商" : "新建厂商"}</Dialog.Title>
          <Flex direction="column" gap="3" mt="3">
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">名称</Text>
              <ClearableTextField placeholder="OpenAI" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </Box>
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">slug</Text>
              <ClearableTextField placeholder="留空则由名称自动生成（openai）" value={form.slug}
                onChange={(e) => setForm({ ...form, slug: e.target.value })}
                style={{ fontFamily: "ui-monospace, monospace" }} />
              <Text size="1" color="gray" as="div" mt="1">URL 安全标识，公开 API 用 ?vendor=slug 筛选</Text>
            </Box>
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">描述（可选）</Text>
              <ClearableTextField value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })} />
            </Box>
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">排序权重</Text>
              <ClearableTextField type="number" placeholder="0" value={form.sort_order}
                onChange={(e) => setForm({ ...form, sort_order: e.target.value })} />
              <Text size="1" color="gray" as="div" mt="1">数值越小越靠前；并列按名称字母序</Text>
            </Box>
            {(create.isError || update.isError) && (
              <Text size="2" color="red">{errMsg(create.error || update.error)}</Text>
            )}
            <Flex gap="2" justify="end" mt="2">
              <Button variant="soft" color="gray" onClick={close}>取消</Button>
              <Button disabled={!form.name.trim()} loading={create.isPending || update.isPending} onClick={submit}>
                {dialog?.mode === "edit" ? "保存" : "创建"}
              </Button>
            </Flex>
          </Flex>
        </Dialog.Content>
      </Dialog.Root>
    </AdminPage>
  );
}
