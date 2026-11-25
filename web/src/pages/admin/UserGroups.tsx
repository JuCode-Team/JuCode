import { useEffect, useState } from "react";
import { Badge, Box, Button, Checkbox, Dialog, Flex, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { adminApi, type ModelGroup, type UserGroup } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

type DialogState = { mode: "create" } | { mode: "edit"; group: UserGroup } | { mode: "bind"; group: UserGroup } | null;
const blank = () => ({ name: "", description: "", sort_order: "0", concurrency_limit: "0" });

export default function UserGroups() {
  const qc = useQueryClient();
  const paged = usePagedQuery<UserGroup>({
    queryKey: ["admin", "user-groups"],
    queryFn: (params) => adminApi.userGroupsPaged(params),
    pageSize: 50,
  });
  const [dialog, setDialog] = useState<DialogState>(null);
  const [form, setForm] = useState(blank());

  const openCreate = () => { setForm(blank()); setDialog({ mode: "create" }); };
  const openEdit = (g: UserGroup) => {
    setForm({
      name: g.name,
      description: g.description ?? "",
      sort_order: String(g.sort_order),
      concurrency_limit: String(g.concurrency_limit),
    });
    setDialog({ mode: "edit", group: g });
  };
  const close = () => setDialog(null);

  const body = () => ({
    name: form.name.trim(),
    description: form.description.trim(),
    sort_order: parseInt(form.sort_order, 10) || 0,
    concurrency_limit: Math.max(0, parseInt(form.concurrency_limit, 10) || 0),
  });

  const inv = () => qc.invalidateQueries({ queryKey: ["admin", "user-groups"] });
  const create = useMutation({
    mutationFn: () => adminApi.createUserGroup(body()),
    onSuccess: () => { close(); inv(); },
  });
  const update = useMutation({
    mutationFn: (id: string) => adminApi.updateUserGroup(id, body()),
    onSuccess: () => { close(); inv(); },
  });
  const del = useMutation({
    mutationFn: (id: string) => adminApi.deleteUserGroup(id),
    onSuccess: () => inv(),
  });
  const setDefault = useMutation({
    mutationFn: (id: string) => adminApi.setDefaultUserGroup(id),
    onSuccess: () => inv(),
  });

  const submit = () => {
    if (!form.name.trim()) return;
    if (dialog?.mode === "create") create.mutate();
    else if (dialog?.mode === "edit") update.mutate(dialog.group.id);
  };

  const columns: Column<UserGroup>[] = [
    {
      header: "名称",
      cell: (g) => (
        <Flex align="center" gap="2">
          <Text weight="medium">{g.name}</Text>
          {g.is_default && <Badge color="green" variant="soft">默认</Badge>}
        </Flex>
      ),
    },
    {
      header: "描述",
      cell: (g) => <Text size="2" color="gray">{g.description || "—"}</Text>,
    },
    {
      header: "并发上限",
      width: "110px",
      cell: (g) =>
        g.concurrency_limit > 0
          ? <Badge variant="soft" color="iris" className="jc-num">{g.concurrency_limit}</Badge>
          : <Text size="2" color="gray">不限</Text>,
    },
    {
      header: "默认",
      width: "70px",
      cell: (g) => (g.is_default ? "✓" : "—"),
    },
    {
      header: "操作",
      width: "360px",
      align: "right",
      cell: (g) => (
        <Flex gap="2" justify="end" wrap="wrap">
          {!g.is_default && (
            <Button size="1" variant="soft" color="green"
              loading={setDefault.isPending && setDefault.variables === g.id}
              onClick={() => setDefault.mutate(g.id)}>
              设为默认
            </Button>
          )}
          <Button size="1" variant="soft" onClick={() => openEdit(g)}>编辑</Button>
          <Button size="1" variant="soft" onClick={() => setDialog({ mode: "bind", group: g })}>绑定专属分组</Button>
          <Button size="1" color="red" variant="soft"
            disabled={g.is_default}
            title={g.is_default ? "默认组不可删除；请先把其他组设为默认" : undefined}
            onClick={() => {
              if (confirm(`删除用户组 "${g.name}"？组内成员将被解除归属，受限的模型分组需要手动重新审视。`)) del.mutate(g.id);
            }}>
            删除
          </Button>
        </Flex>
      ),
    },
  ];

  return (
    <AdminPage
      title="用户组"
      description="将用户分桶并配置并发上限。未分组用户使用默认组，并发上限 0 表示不限。"
      actions={<Button onClick={openCreate}>新建用户组</Button>}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="匹配用户组" value={paged.total.toLocaleString()} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="当前页默认组" value={paged.items.filter((g) => g.is_default).length} detail="只能有一个" tone="green" />
        <AdminMetricCard label="限流组" value={paged.items.filter((g) => g.concurrency_limit > 0).length} detail="当前页" tone="amber" />
        <AdminMetricCard label="不限并发" value={paged.items.filter((g) => g.concurrency_limit <= 0).length} detail="当前页" />
      </AdminMetricGrid>

      <DataTable<UserGroup>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(g) => g.id}
        searchPlaceholder="搜索用户组..."
        emptyText="尚未创建用户组 — 点击右上「新建用户组」"
      />

      <Dialog.Root open={dialog !== null} onOpenChange={(o) => { if (!o) close(); }}>
        {dialog?.mode === "bind" ? (
          <BindExclusiveGroupsDialog userGroup={dialog.group} onClose={close} />
        ) : (
        <Dialog.Content style={{ maxWidth: 460 }}>
          <Dialog.Title>{dialog?.mode === "edit" ? "编辑用户组" : "新建用户组"}</Dialog.Title>
          <Flex direction="column" gap="3" mt="3">
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">名称</Text>
              <ClearableTextField placeholder="VIP / 企业 / 团队 A …" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} />
            </Box>
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">描述（可选）</Text>
              <ClearableTextField value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })} />
            </Box>
            <Flex gap="3">
              <Box style={{ flex: 1 }}>
                <Text size="2" weight="medium" as="div" mb="1">并发上限</Text>
                <ClearableTextField type="number" placeholder="0 = 不限" value={form.concurrency_limit}
                  onChange={(e) => setForm({ ...form, concurrency_limit: e.target.value })} />
                <Text size="1" color="gray" as="div" mt="1">
                  本组用户在按余额计费时的最大同时在飞请求数
                </Text>
              </Box>
              <Box style={{ flex: 1 }}>
                <Text size="2" weight="medium" as="div" mb="1">排序权重</Text>
                <ClearableTextField type="number" placeholder="0" value={form.sort_order}
                  onChange={(e) => setForm({ ...form, sort_order: e.target.value })} />
                <Text size="1" color="gray" as="div" mt="1">小的靠前</Text>
              </Box>
            </Flex>
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
        )}
      </Dialog.Root>
    </AdminPage>
  );
}

function BindExclusiveGroupsDialog({ userGroup, onClose }: { userGroup: UserGroup; onClose: () => void }) {
  const qc = useQueryClient();
  const { data: groups } = useQuery({ queryKey: ["admin", "groups"], queryFn: adminApi.groups });
  const exclusive = (groups ?? []).filter((g: ModelGroup) => g.exclusive);
  const { data: current } = useQuery({
    queryKey: ["admin", "user-groups", userGroup.id, "model-group-grants"],
    queryFn: () => adminApi.userGroupModelGroupGrants(userGroup.id),
  });
  const [picked, setPicked] = useState<string[]>([]);
  useEffect(() => { if (current) setPicked(current); }, [current]);
  const save = useMutation({
    mutationFn: () => adminApi.replaceUserGroupModelGroupGrants(userGroup.id, picked),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "user-groups"] });
      qc.invalidateQueries({ queryKey: ["admin", "groups"] });
      onClose();
    },
  });
  return (
    <Dialog.Content style={{ maxWidth: 480 }}>
      <Dialog.Title>绑定专属模型分组</Dialog.Title>
      <Text size="2" color="gray" as="p" mt="2">
        勾选后，{userGroup.name} 的成员可以走这些专属分组。公开分组不用绑。
      </Text>
      <Flex direction="column" gap="2" mt="3">
        {exclusive.length === 0 ? (
          <Text size="2" color="gray">还没有专属模型分组。去「分组」页复制一个并打开专属。</Text>
        ) : exclusive.map((g) => (
          <label key={g.id} style={{ display: "inline-flex", alignItems: "center", gap: 8, cursor: "pointer" }}>
            <Checkbox
              checked={picked.includes(g.id)}
              onCheckedChange={() => setPicked((s) => s.includes(g.id) ? s.filter((x) => x !== g.id) : [...s, g.id])}
            />
            <Text size="2">{g.name}</Text>
            <Badge size="1" variant="soft">×{g.rate_multiplier}</Badge>
          </label>
        ))}
        {save.isError && <Text size="2" color="red">{errMsg(save.error)}</Text>}
        <Flex gap="2" justify="end" mt="2">
          <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
          <Button loading={save.isPending} onClick={() => save.mutate()}>保存</Button>
        </Flex>
      </Flex>
    </Dialog.Content>
  );
}
