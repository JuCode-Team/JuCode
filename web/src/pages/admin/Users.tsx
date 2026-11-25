import { useEffect, useMemo, useState } from "react";
import {
  AlertDialog, Badge, Box, Button, Card, Checkbox, Code, Dialog, DropdownMenu, Flex, Heading,
  IconButton, Select, Table, Tabs, Text, Tooltip,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import { Check, Copy, Eye, EyeOff, MoreHorizontal, UserCheck, UserX } from "lucide-react";
import { adminApi, type ApiKeyView, type ModelGroup, type UserGroup } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { copyText } from "../../lib/copy";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { AdminPage } from "../../components/AdminPage";
import { FilterChips, FilterSelect, type FilterChip, type FilterOption } from "../../components/list/FilterBar";
import { ConfirmDialog } from "../../components/ConfirmDialog";

import { UserPlansTab } from "./UserPlansTab";

const PAGE_SIZE = 20;

type UserStatus = "active" | "banned" | "pending_approval" | "rejected";
const STATUS_LABEL: Record<string, string> = {
  active: "正常",
  pending_approval: "待审核",
  banned: "已封禁",
  rejected: "已拒绝",
};
const userStatusLabel = (s: string) => STATUS_LABEL[s] ?? s;
const roleLabel = (r: string) => (r === "admin" ? "管理员" : r === "user" ? "用户" : r);

// One pending confirmation at a time; every destructive row / bulk action
// routes through it instead of window.confirm.
type PendingAction =
  | { kind: "ban"; user: any }
  | { kind: "reject"; user: any }
  | { kind: "restore"; user: any }
  | { kind: "delete"; user: any }
  | { kind: "bulk-ban"; ids: string[] }
  | { kind: "bulk-delete"; ids: string[] };

export default function Users() {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState<"" | UserStatus>("");
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [params] = useSearchParams();
  const editingParam = params.get("edit");
  const [editingId, setEditingId] = useState<string | null>(editingParam);
  const [selected, setSelected] = useState<Set<string | number>>(new Set());

  useEffect(() => {
    if (editingParam) setEditingId(editingParam);
  }, [editingParam]);

  const paged = usePagedQuery<any>({
    queryKey: ["admin", "users"],
    queryFn: (params) =>
      adminApi.users({
        q: params.q || undefined,
        status: statusFilter || undefined,
        limit: params.limit,
        offset: params.offset,
      }).then((r) => ({
        items: r.users,
        total: r.total,
        limit: r.limit,
        offset: r.offset,
      })),
    pageSize: PAGE_SIZE,
    extras: [statusFilter],
  });
  // Lightweight unread badge: pending count for the "需要审核" filter pill.
  const { data: pendingData } = useQuery({
    queryKey: ["admin", "users", "pending-count"],
    queryFn: () => adminApi.users({ status: "pending_approval", limit: 1, offset: 0 }),
    refetchInterval: 30_000,
  });
  const pendingTotal = pendingData?.total ?? 0;
  const { data: userGroups } = useQuery({ queryKey: ["admin", "user-groups"], queryFn: adminApi.userGroups });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin", "users"] });
  const banToggle = useMutation({
    mutationFn: ({ id, status }: { id: string; status: "active" | "banned" }) =>
      adminApi.updateUser(id, { status }),
    onSuccess: () => invalidate(),
  });
  const approve = useMutation({
    mutationFn: (id: string) => adminApi.approveUser(id),
    onSuccess: () => invalidate(),
  });
  const reject = useMutation({
    mutationFn: (id: string) => adminApi.rejectUser(id),
    onSuccess: () => invalidate(),
  });
  const del = useMutation({
    mutationFn: (id: string) => adminApi.deleteUser(id),
    onSuccess: () => invalidate(),
  });

  const userGroupName = useMemo(() => {
    const m = new Map<string, string>();
    (userGroups ?? []).forEach((g) => m.set(g.id, g.name));
    return m;
  }, [userGroups]);

  const selectedUsers = paged.items.filter((u) => selected.has(u.id));
  const selectedIds = selectedUsers.map((u) => u.id);

  const batchApprove = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.approveUser(id))),
    onSuccess: () => { setSelected(new Set()); invalidate(); },
  });
  const batchBan = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.updateUser(id, { status: "banned" }))),
    onSuccess: () => { setSelected(new Set()); invalidate(); },
  });
  const batchRestore = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.updateUser(id, { status: "active" }))),
    onSuccess: () => { setSelected(new Set()); invalidate(); },
  });
  const batchDelete = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.deleteUser(id))),
    onSuccess: () => { setSelected(new Set()); invalidate(); },
  });

  const columns: Column<any>[] = [
    {
      header: "用户",
      width: "340px",
      minWidth: "240px",
      cell: (u) => (
        <Box style={{ minWidth: 0 }}>
          <Text size="2" weight="medium" as="div" truncate title={u.email}>{u.email}</Text>
          <Text size="1" color="gray" as="div" truncate className={u.nickname || u.username ? undefined : "jc-cell-mono"}>{u.nickname || u.username || u.id}</Text>
        </Box>
      ),
    },
    {
      header: "角色",
      width: "90px",
      cell: (u) => <Badge variant="soft" color={u.role === "admin" ? "iris" : "gray"}>{roleLabel(u.role)}</Badge>,
    },
    {
      header: "状态",
      width: "100px",
      cell: (u) => <Badge variant="soft" color={statusBadgeColor(u.status)}>{userStatusLabel(u.status)}</Badge>,
    },
    {
      header: "用户组",
      width: "140px",
      cell: (u) => u.user_group_id
        ? <Badge variant="soft" color="iris">{userGroupName.get(u.user_group_id) ?? u.user_group_id.slice(0, 6)}</Badge>
        : <Text size="2" color="gray">未分组</Text>,
    },
    {
      header: "余额（积分）",
      width: "120px",
      type: "number",
      cell: (u) => <PointAmount amount={u.balance} iconSize={12} />,
    },
    {
      header: "注册",
      width: "110px",
      cell: (u) => <span className="jc-num jc-cell-muted">{new Date(u.created_at).toLocaleDateString()}</span>,
    },
    {
      header: "操作",
      width: "120px",
      type: "actions",
      cell: (u) => (
        <Flex gap="1" justify="end" align="center">
          <Button size="1" variant="soft" color="gray" onClick={() => setEditingId(u.id)}>编辑</Button>
          {u.status === "pending_approval" && (
            <Button size="1" variant="soft" color="green" loading={approve.isPending} onClick={() => approve.mutate(u.id)}>通过</Button>
          )}
          <DropdownMenu.Root>
            <DropdownMenu.Trigger>
              <IconButton size="1" variant="ghost" color="gray" className="jc-row-menu" aria-label="更多操作">
                <MoreHorizontal size={15} />
              </IconButton>
            </DropdownMenu.Trigger>
            <DropdownMenu.Content align="end" size="1">
              {u.status === "pending_approval" && (
                <DropdownMenu.Item color="orange" onSelect={() => setPending({ kind: "reject", user: u })}>拒绝注册</DropdownMenu.Item>
              )}
              {u.status === "active" && (
                <DropdownMenu.Item color="orange" onSelect={() => setPending({ kind: "ban", user: u })}>封禁</DropdownMenu.Item>
              )}
              {u.status === "banned" && (
                <DropdownMenu.Item onSelect={() => banToggle.mutate({ id: u.id, status: "active" })}>解除封禁</DropdownMenu.Item>
              )}
              {u.status === "rejected" && (
                <DropdownMenu.Item onSelect={() => setPending({ kind: "restore", user: u })}>恢复为正常</DropdownMenu.Item>
              )}
              <DropdownMenu.Separator />
              <DropdownMenu.Item color="red" onSelect={() => setPending({ kind: "delete", user: u })}>删除用户</DropdownMenu.Item>
            </DropdownMenu.Content>
          </DropdownMenu.Root>
        </Flex>
      ),
    },
  ];

  const statusOptions: FilterOption[] = [
    { v: "active", l: "正常" },
    { v: "pending_approval", l: pendingTotal > 0 ? `待审核（${pendingTotal}）` : "待审核" },
    { v: "banned", l: "已封禁" },
    { v: "rejected", l: "已拒绝" },
  ];

  const chips: FilterChip[] = [];
  if (paged.q) chips.push({ key: "q", label: "搜索", value: paged.q, onRemove: () => paged.setRawQ("") });
  if (statusFilter) chips.push({ key: "status", label: "状态", value: userStatusLabel(statusFilter), onRemove: () => setStatusFilter("") });
  const clearAll = () => { paged.setRawQ(""); setStatusFilter(""); };

  const filters = (
    <>
      <FilterSelect
        label="状态"
        value={statusFilter}
        onChange={(v) => setStatusFilter(v as "" | UserStatus)}
        options={statusOptions}
      />
      {pendingTotal > 0 && statusFilter !== "pending_approval" && (
        <Button size="2" variant="soft" color="amber" onClick={() => setStatusFilter("pending_approval")}>
          {pendingTotal} 条注册待审核
        </Button>
      )}
    </>
  );

  const bulkActions = (
    <>
      {selectedUsers.some((u) => u.status === "pending_approval") && (
        <Button size="1" variant="soft" color="green" loading={batchApprove.isPending}
          onClick={() => batchApprove.mutate(selectedUsers.filter((u) => u.status === "pending_approval").map((u) => u.id))}>
          <UserCheck size={13} /> 通过待审核
        </Button>
      )}
      {selectedUsers.some((u) => u.status === "active") && (
        <Button size="1" variant="soft" color="orange" loading={batchBan.isPending}
          onClick={() => setPending({ kind: "bulk-ban", ids: selectedUsers.filter((u) => u.status === "active").map((u) => u.id) })}>
          <UserX size={13} /> 批量封禁
        </Button>
      )}
      {selectedUsers.some((u) => u.status === "banned" || u.status === "rejected") && (
        <Button size="1" variant="soft" color="green" loading={batchRestore.isPending}
          onClick={() => batchRestore.mutate(selectedUsers.filter((u) => u.status === "banned" || u.status === "rejected").map((u) => u.id))}>
          批量恢复
        </Button>
      )}
      <Button size="1" variant="soft" color="red" loading={batchDelete.isPending}
        onClick={() => setPending({ kind: "bulk-delete", ids: selectedIds })}>
        批量删除
      </Button>
    </>
  );

  return (
    <AdminPage
      title="用户"
      description="搜索、审核、封禁和用户资产管理集中在同一张列表里。"
    >
      <DataTable<any>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(u) => u.id}
        searchPlaceholder="搜索邮箱 / 用户名 / 昵称"
        filters={filters}
        selectedRowKeys={selected}
        onSelectedRowKeysChange={setSelected}
        bulkActions={bulkActions}
        toolbarFooter={
          <FilterChips
            chips={chips}
            onClearAll={clearAll}
            summary={<>共 <strong>{paged.total.toLocaleString()}</strong> 位用户</>}
          />
        }
        emptyText="没有匹配的用户"
        emptyHint={chips.length > 0 ? "调整或清空筛选条件后再试" : undefined}
        emptyAction={chips.length > 0 ? <Button size="1" variant="soft" color="gray" onClick={clearAll}>清空筛选</Button> : undefined}
      />

      <ConfirmDialog
        open={pending !== null}
        title={pendingTitle(pending)}
        description={pendingDescription(pending)}
        confirmLabel={pendingConfirmLabel(pending)}
        color={pending?.kind === "delete" || pending?.kind === "bulk-delete" ? "red" : pending?.kind === "restore" ? "green" : "orange"}
        loading={banToggle.isPending || reject.isPending || del.isPending || batchBan.isPending || batchDelete.isPending}
        onCancel={() => setPending(null)}
        onConfirm={async () => {
          if (!pending) return;
          switch (pending.kind) {
            case "ban": await banToggle.mutateAsync({ id: pending.user.id, status: "banned" }); break;
            case "restore": await banToggle.mutateAsync({ id: pending.user.id, status: "active" }); break;
            case "reject": await reject.mutateAsync(pending.user.id); break;
            case "delete": await del.mutateAsync(pending.user.id); break;
            case "bulk-ban": await batchBan.mutateAsync(pending.ids); break;
            case "bulk-delete": await batchDelete.mutateAsync(pending.ids); break;
          }
          setPending(null);
        }}
      />

      {editingId && (
        <UserEditDialog
          userId={editingId}
          userGroups={userGroups ?? []}
          onClose={() => setEditingId(null)}
        />
      )}
    </AdminPage>
  );
}

/* ============ Edit dialog (tabbed) ============ */

function UserEditDialog({ userId, userGroups, onClose }: { userId: string; userGroups: UserGroup[]; onClose: () => void }) {
  const qc = useQueryClient();
  const { data: user, isLoading } = useQuery({
    queryKey: ["admin", "user", userId],
    queryFn: () => adminApi.userDetail(userId),
  });
  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["admin", "users"] });
    qc.invalidateQueries({ queryKey: ["admin", "user", userId] });
  };

  return (
    <Dialog.Root open onOpenChange={(o) => { if (!o) onClose(); }}>
      <Dialog.Content style={{ maxWidth: 640 }}>
        <Dialog.Title>{user?.email ?? "加载中…"}</Dialog.Title>
        <Dialog.Description size="2" color="gray" mb="3">
          <Code>{userId}</Code>
        </Dialog.Description>

        {isLoading || !user ? (
          <Text color="gray">加载中…</Text>
        ) : (
          <Tabs.Root defaultValue="info">
            <Tabs.List>
              <Tabs.Trigger value="info">基础</Tabs.Trigger>
              <Tabs.Trigger value="roles">角色</Tabs.Trigger>
              <Tabs.Trigger value="plan">套餐</Tabs.Trigger>
              <Tabs.Trigger value="keys">API Keys</Tabs.Trigger>
            </Tabs.List>
            <Box pt="4">
              <Tabs.Content value="info">
                <InfoTab user={user} userGroups={userGroups} onChanged={invalidate} />
              </Tabs.Content>
              <Tabs.Content value="roles">
                <RolesTab user={user} onChanged={invalidate} />
              </Tabs.Content>
              <Tabs.Content value="plan">
                <UserPlansTab userId={user.id} onChanged={invalidate} />
              </Tabs.Content>
              <Tabs.Content value="keys">
                <KeysTab userId={user.id} />
              </Tabs.Content>
            </Box>
          </Tabs.Root>
        )}

        <Flex justify="end" mt="4">
          <Button variant="soft" color="gray" onClick={onClose}>关闭</Button>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

/* ----- functional roles ----- */

function RolesTab({ user, onChanged }: { user: any; onChanged: () => void }) {
  const existing: string[] = Array.isArray(user.roles) ? user.roles : [];
  const [cs, setCs] = useState(existing.includes("cs"));
  const [agent, setAgent] = useState(existing.includes("agent"));
  const m = useMutation({
    mutationFn: () => {
      const roles: string[] = [];
      if (cs) roles.push("cs");
      if (agent) roles.push("agent");
      return adminApi.setUserRoles(user.id, roles);
    },
    onSuccess: () => onChanged(),
  });
  return (
    <Flex direction="column" gap="3">
      <Text size="2" color="gray">
        附加职能角色。主身份 (user / admin) 在“基础”选项卡设置；admin 默认拥有全部职能权限。
      </Text>
      <Box>
        <Flex gap="2" align="center" mb="2">
          <Checkbox checked={cs} onCheckedChange={(v) => setCs(!!v)} />
          <Text size="2">客服 (cs) — 处理工单、调整用户余额 / 套餐</Text>
        </Flex>
        <Flex gap="2" align="center">
          <Checkbox checked={agent} onCheckedChange={(v) => setAgent(!!v)} />
          <Text size="2">代理 (agent) — 生成兑换码、专属优惠码、查看流水与结算</Text>
        </Flex>
      </Box>
      <Flex justify="end">
        <Button disabled={m.isPending} onClick={() => m.mutate()}>保存角色</Button>
      </Flex>
    </Flex>
  );
}

/* ----- basic info + user_group + balance + topup ----- */

function InfoTab({ user, userGroups, onChanged }: { user: any; userGroups: UserGroup[]; onChanged: () => void }) {
  const [form, setForm] = useState({
    email: user.email,
    nickname: user.nickname ?? "",
    role: user.role,
    status: user.status,
    user_group_id: user.user_group_id ?? "",
    concurrency_limit_override: String(user.concurrency_limit_override ?? 0),
    priority: String(user.priority ?? 100),
  });
  const [topupAmount, setTopupAmount] = useState("");
  const [topupNote, setTopupNote] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");

  const update = useMutation({
    mutationFn: () => adminApi.updateUser(user.id, {
      email: form.email,
      nickname: form.nickname,
      role: form.role,
      status: form.status,
      user_group_id: form.user_group_id,
      concurrency_limit_override: Number(form.concurrency_limit_override) || 0,
      priority: Math.max(0, Math.min(100, Number(form.priority) || 100)),
    }),
    onSuccess: () => onChanged(),
  });
  const topup = useMutation({
    mutationFn: () => adminApi.topupUser(user.id, topupAmount, topupNote),
    onSuccess: () => { setTopupAmount(""); setTopupNote(""); onChanged(); },
  });
  const resetPassword = useMutation({
    mutationFn: () => adminApi.resetUserPassword(user.id, newPassword),
    onSuccess: () => { setNewPassword(""); setConfirmPassword(""); },
  });
  const canResetPassword =
    newPassword.length >= 8 &&
    newPassword === confirmPassword &&
    !resetPassword.isPending;

  return (
    <Flex direction="column" gap="3">
      <Flex gap="3" wrap="wrap">
        <Box style={{ flex: 1, minWidth: 220 }}>
          <Text size="1" color="gray" as="p" mb="1">Email</Text>
          <ClearableTextField value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} />
        </Box>
        <Box style={{ flex: 1, minWidth: 220 }}>
          <Text size="1" color="gray" as="p" mb="1">昵称</Text>
          <ClearableTextField value={form.nickname} onChange={(e) => setForm({ ...form, nickname: e.target.value })} />
        </Box>
      </Flex>
      <Flex gap="3" wrap="wrap">
        <Box style={{ flex: 1, minWidth: 160 }}>
          <Text size="1" color="gray" as="p" mb="1">角色</Text>
          <Select.Root value={form.role} onValueChange={(v) => setForm({ ...form, role: v })}>
            <Select.Trigger />
            <Select.Content>
              <Select.Item value="user">user</Select.Item>
              <Select.Item value="admin">admin</Select.Item>
            </Select.Content>
          </Select.Root>
        </Box>
        <Box style={{ flex: 1, minWidth: 160 }}>
          <Text size="1" color="gray" as="p" mb="1">状态</Text>
          <Select.Root value={form.status} onValueChange={(v) => setForm({ ...form, status: v })}>
            <Select.Trigger />
            <Select.Content>
              <Select.Item value="active">active</Select.Item>
              <Select.Item value="banned">banned</Select.Item>
            </Select.Content>
          </Select.Root>
        </Box>
        <Box style={{ flex: 1, minWidth: 200 }}>
          <Text size="1" color="gray" as="p" mb="1">用户组</Text>
          <Select.Root value={form.user_group_id || "_none"} onValueChange={(v) => setForm({ ...form, user_group_id: v === "_none" ? "" : v })}>
            <Select.Trigger placeholder="未分组" />
            <Select.Content>
              <Select.Item value="_none">未分组</Select.Item>
              {userGroups.map((g) => <Select.Item key={g.id} value={g.id}>{g.name}</Select.Item>)}
            </Select.Content>
          </Select.Root>
        </Box>
      </Flex>
      <Box>
        <Text size="1" color="gray" as="p" mb="1">余额（积分 · 调整用下方"充值"，负数为扣减）</Text>
        <PointAmount amount={user.balance} iconSize={12} />
      </Box>
      <Flex gap="3" wrap="wrap">
        <Box style={{ flex: 1, minWidth: 180 }}>
          <Text size="1" color="gray" as="p" mb="1">并发覆盖（0 = 用组/套餐默认）</Text>
          <ClearableTextField type="number" min="0" value={form.concurrency_limit_override}
            onChange={(e) => setForm({ ...form, concurrency_limit_override: e.target.value })} />
        </Box>
        <Box style={{ flex: 1, minWidth: 180 }}>
          <Text size="1" color="gray" as="p" mb="1">优先级（0-100，不向用户显示）</Text>
          <ClearableTextField type="number" min="0" max="100" value={form.priority}
            onChange={(e) => setForm({ ...form, priority: e.target.value })} />
        </Box>
      </Flex>

      <UserExclusiveGroups userId={user.id} />

      {update.isError && <Text size="2" color="red">{errMsg(update.error)}</Text>}
      <Flex justify="end">
        <Button loading={update.isPending} onClick={() => update.mutate()}>保存基础信息</Button>
      </Flex>

      <Box mt="3" pt="3" style={{ borderTop: "1px solid var(--c-outline-variant, var(--gray-4))" }}>
        <Heading size="3" mb="2">充值</Heading>
        <Flex gap="2" align="end" wrap="wrap">
          <Box style={{ minWidth: 140, flex: 1 }}>
            <Text size="1" color="gray" as="p" mb="1">金额</Text>
            <ClearableTextField placeholder="例如 10，扣减填 -10" value={topupAmount} onChange={(e) => setTopupAmount(e.target.value)} />
          </Box>
          <Box style={{ minWidth: 200, flex: 2 }}>
            <Text size="1" color="gray" as="p" mb="1">备注（可选）</Text>
            <ClearableTextField value={topupNote} onChange={(e) => setTopupNote(e.target.value)} />
          </Box>
          <Button loading={topup.isPending} disabled={!topupAmount.trim()} onClick={() => topup.mutate()}>充值</Button>
        </Flex>
        {topup.isError && <Text size="2" color="red" mt="1" as="div">{errMsg(topup.error)}</Text>}
        {topup.isSuccess && <Text size="2" color="green" mt="1" as="div">充值成功</Text>}
      </Box>

      <Box mt="3" pt="3" style={{ borderTop: "1px solid var(--c-outline-variant, var(--gray-4))" }}>
        <Heading size="3" mb="2">重置密码</Heading>
        <Flex gap="2" align="end" wrap="wrap">
          <Box style={{ minWidth: 180, flex: 1 }}>
            <Text size="1" color="gray" as="p" mb="1">新密码（至少 8 位）</Text>
            <ClearableTextField type="password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} />
          </Box>
          <Box style={{ minWidth: 180, flex: 1 }}>
            <Text size="1" color="gray" as="p" mb="1">重复新密码</Text>
            <ClearableTextField type="password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} />
          </Box>
          <Button color="red" variant="soft" disabled={!canResetPassword} loading={resetPassword.isPending} onClick={() => resetPassword.mutate()}>
            重置密码
          </Button>
        </Flex>
        {confirmPassword !== "" && newPassword !== confirmPassword && (
          <Text size="1" color="red" mt="1" as="div">两次输入不一致</Text>
        )}
        {resetPassword.isError && <Text size="2" color="red" mt="1" as="div">{errMsg(resetPassword.error)}</Text>}
        {resetPassword.isSuccess && <Text size="2" color="green" mt="1" as="div">密码已重置，用户旧会话已退出</Text>}
      </Box>
    </Flex>
  );
}

/* ----- API keys tab: list + revoke + admin-create ----- */

function KeysTab({ userId }: { userId: string }) {
  const qc = useQueryClient();
  const { data: keys } = useQuery({ queryKey: ["admin", "user-keys", userId], queryFn: () => adminApi.userApikeys(userId) });
  const { data: groups } = useQuery({ queryKey: ["admin", "groups"], queryFn: adminApi.groups });
  const [newName, setNewName] = useState("");
  const [newGroups, setNewGroups] = useState<string[]>([]);
  const [secret, setSecret] = useState<string | null>(null);
  const [limits, setLimits] = useState(emptyCostLimits);

  const inv = () => qc.invalidateQueries({ queryKey: ["admin", "user-keys", userId] });
  const create = useMutation({
    mutationFn: () => adminApi.createUserApikey(userId, { name: newName.trim(), allowed_group_ids: newGroups, ...costLimitPayload(limits) }),
    onSuccess: (r) => { setSecret(r.secret); setNewName(""); setNewGroups([]); setLimits(emptyCostLimits); inv(); },
  });
  const revoke = useMutation({
    mutationFn: (kid: string) => adminApi.revokeUserApikey(userId, kid),
    onSuccess: () => inv(),
  });

  // The admin group list includes disabled groups, so keys still bound to
  // one can show its name and a disabled mark; new bindings only offer
  // enabled groups.
  const groupById = new Map((groups ?? []).map((g: ModelGroup) => [g.id, g]));
  const bindableGroups = (groups ?? []).filter((g: ModelGroup) => g.enabled !== false);
  const toggle = (id: string) => setNewGroups((s) => s.includes(id) ? s.filter((x) => x !== id) : [...s, id]);

  return (
    <Flex direction="column" gap="3">
      <Card>
        <Text size="2" color="gray" mb="2" as="div">已有 Keys</Text>
        <div className="jc-local-table-scroll">
        <Table.Root size="1">
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>名称</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>前缀</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>允许分组</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>限额用量</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>最近使用</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">操作</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {(keys ?? []).map((k: any) => (
              <AdminKeyRow key={k.id} userId={userId} k={k} groupById={groupById} onRevoke={() => revoke.mutate(k.id)} />
            ))}
            {(keys?.length ?? 0) === 0 && (
              <Table.Row><Table.Cell colSpan={6}><Text color="gray">该用户尚无 API Key</Text></Table.Cell></Table.Row>
            )}
          </Table.Body>
        </Table.Root>
        </div>
      </Card>

      <Card>
        <Text size="2" color="gray" mb="2" as="div">代用户创建新 Key</Text>
        <Flex direction="column" gap="2">
          <Box>
            <Text size="1" color="gray" as="p" mb="1">名称</Text>
            <ClearableTextField placeholder="便于辨识" value={newName} onChange={(e) => setNewName(e.target.value)} />
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">允许的模型分组（不勾选 = 全部）</Text>
            {bindableGroups.length === 0 ? (
              <Text size="2" color="gray">尚未创建任何模型分组</Text>
            ) : (
              <Flex gap="3" wrap="wrap">
                {bindableGroups.map((g) => (
                  <Tooltip key={g.id} content={g.description || ""}>
                    <label style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}>
                      <Checkbox checked={newGroups.includes(g.id)} onCheckedChange={() => toggle(g.id)} />
                      <Text size="2">{g.name}</Text>
                    </label>
                  </Tooltip>
                ))}
              </Flex>
            )}
          </Box>
          <CostLimitFields values={limits} onChange={setLimits} />
          {create.isError && <Text size="2" color="red">{errMsg(create.error)}</Text>}
          <Flex justify="end" mt="1">
            <Button disabled={!newName.trim() || !validCostLimits(limits)} loading={create.isPending} onClick={() => create.mutate()}>创建</Button>
          </Flex>
        </Flex>
      </Card>

      {secret && (
        <AlertDialog.Root open onOpenChange={() => setSecret(null)}>
          <AlertDialog.Content>
            <AlertDialog.Title>Key 已创建</AlertDialog.Title>
            <AlertDialog.Description>
              下面是该用户的完整密钥。后续也可以在该用户的 Key 列表里随时「查看」。
            </AlertDialog.Description>
            <Box my="3" p="2" style={{ background: "var(--gray-3)", borderRadius: 6 }}>
              <Code style={{ wordBreak: "break-all" }}>{secret}</Code>
            </Box>
            <Flex justify="end">
              <AlertDialog.Action><Button>我已保存</Button></AlertDialog.Action>
            </Flex>
          </AlertDialog.Content>
        </AlertDialog.Root>
      )}
    </Flex>
  );
}

// AdminKeyRow mirrors the user-side KeyRow but uses the admin-only
// /admin/users/:uid/apikeys/:kid/reveal passthrough. Each row owns its
// own reveal state so a refetch of the keys list doesn't surprise the
// admin by collapsing what they were looking at.
function AdminKeyRow({
  userId,
  k,
  groupById,
  onRevoke,
}: {
  userId: string;
  k: ApiKeyView;
  groupById: Map<string, ModelGroup>;
  onRevoke: () => void;
}) {
  const [secret, setSecret] = useState<string | null>(null);
  const [revealErr, setRevealErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [editing, setEditing] = useState(false);
  const [limits, setLimits] = useState({ daily: k.daily_cost_limit ?? "", monthly: k.monthly_cost_limit ?? "", total: k.total_cost_limit ?? "" });
  const qc = useQueryClient();
  const update = useMutation({ mutationFn: () => adminApi.updateUserApikey(userId, k.id, costLimitPayload(limits)), onSuccess: () => { setEditing(false); qc.invalidateQueries({ queryKey: ["admin", "user-keys", userId] }); } });
  const reveal = useMutation({
    mutationFn: () => adminApi.revealUserApikey(userId, k.id),
    onSuccess: (s) => { setSecret(s); setRevealErr(null); },
    onError: (e: Error) => setRevealErr(errMsg(e)),
  });
  const doCopy = async () => {
    if (!secret) return;
    if (await copyText(secret)) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    }
  };
  return (
    <Table.Row>
      <Table.Cell>
        <Flex align="center" gap="2">
          <Text size="2">{k.name || "—"}</Text>
          {k.system && <Badge variant="soft" size="1">系统</Badge>}
        </Flex>
      </Table.Cell>
      <Table.Cell>
        {secret ? (
          <Flex align="center" gap="2">
            <Code size="1" style={{ wordBreak: "break-all" }}>{secret}</Code>
            <Tooltip content={copied ? "已复制" : "复制"}>
              <IconButton size="1" variant="soft" onClick={doCopy}>
                {copied ? <Check size={12} /> : <Copy size={12} />}
              </IconButton>
            </Tooltip>
            <Tooltip content="隐藏">
              <IconButton size="1" variant="ghost" onClick={() => setSecret(null)}>
                <EyeOff size={12} />
              </IconButton>
            </Tooltip>
          </Flex>
        ) : (
          <Flex align="center" gap="2">
            <Code>{k.prefix}…</Code>
            <Tooltip content={revealErr || "显示完整 Key"}>
              <IconButton size="1" variant="ghost" color={revealErr ? "amber" : "gray"} loading={reveal.isPending} onClick={() => reveal.mutate()}>
                <Eye size={12} />
              </IconButton>
            </Tooltip>
          </Flex>
        )}
      </Table.Cell>
      <Table.Cell>
        {k.allowed_groups_unrestricted
          ? <Text size="2" color="gray">全部</Text>
          : (k.allowed_group_ids ?? []).length === 0
            ? <Text size="2" color="red">分组均已删除</Text>
            : (
              <Flex gap="1" wrap="wrap">
                {k.allowed_group_ids.map((id: string) => {
                  const g = groupById.get(id);
                  const disabled = g?.enabled === false;
                  return (
                    <Badge key={id} variant="soft" size="1" color={disabled ? "gray" : undefined}>
                      {g?.name ?? id.slice(0, 6)}{disabled ? "（已停用）" : ""}
                    </Badge>
                  );
                })}
              </Flex>
            )}
      </Table.Cell>
      <Table.Cell><Flex direction="column" gap="1">{costLimitUsage(k).map(({ label, used, limit }) => <Text key={label} size="1">{label} <PointAmount amount={used} decimals="preserve" iconSize={11} /> / <PointAmount amount={limit} decimals="preserve" iconSize={11} /></Text>)}</Flex></Table.Cell>
      <Table.Cell>{k.last_used_at ? new Date(k.last_used_at).toLocaleString() : "—"}</Table.Cell>
      <Table.Cell align="right">
        <Button size="1" variant="soft" color="gray" onClick={() => setEditing(true)}>编辑限额</Button>
        {!k.system && <Button size="1" color="red" variant="soft" onClick={() => { if (confirm("撤销该 Key？")) onRevoke(); }}>撤销</Button>}
      </Table.Cell>
      {editing && <Dialog.Root open onOpenChange={(o) => { if (!o) setEditing(false); }}><Dialog.Content style={{ maxWidth: 420 }}><Dialog.Title>编辑消费限额</Dialog.Title><Flex direction="column" gap="3" mt="3"><CostLimitFields values={limits} onChange={setLimits} />{update.isError && <Text size="2" color="red">{errMsg(update.error)}</Text>}<Flex justify="end" gap="2"><Button variant="soft" color="gray" onClick={() => setEditing(false)}>取消</Button><Button disabled={!validCostLimits(limits)} loading={update.isPending} onClick={() => update.mutate()}>保存</Button></Flex></Flex></Dialog.Content></Dialog.Root>}
    </Table.Row>
  );
}

type CostLimitValues = { daily: string; monthly: string; total: string };
const emptyCostLimits: CostLimitValues = { daily: "", monthly: "", total: "" };
const validCostLimits = (v: CostLimitValues) => Object.values(v).every((x) => x.trim() === "" || (Number.isFinite(Number(x)) && Number(x) >= 0));
const costLimitPayload = (v: CostLimitValues) => ({ daily_cost_limit: v.daily.trim() || null, monthly_cost_limit: v.monthly.trim() || null, total_cost_limit: v.total.trim() || null });
function CostLimitFields({ values, onChange }: { values: CostLimitValues; onChange: (v: CostLimitValues) => void }) { return <Box><Text size="1" color="gray" as="p" mb="1">消费限额（积分）</Text><Flex gap="2" wrap="wrap">{([["daily", "自然日"], ["monthly", "自然月"], ["total", "累计"]] as const).map(([key, label]) => <Box key={key} style={{ flex: "1 1 110px" }}><ClearableTextField type="number" min="0" step="any" placeholder={label} value={values[key]} onChange={(e) => onChange({ ...values, [key]: e.target.value })} /></Box>)}</Flex></Box>; }
function costLimitUsage(k: ApiKeyView) { return ([["自然日", k.daily_cost_used, k.daily_cost_limit], ["自然月", k.monthly_cost_used, k.monthly_cost_limit], ["累计", k.total_cost_used, k.total_cost_limit]] as const).filter((x) => x[2] !== null).map(([label, used, limit]) => ({ label, used, limit: limit as string })); }

function UserExclusiveGroups({ userId }: { userId: string }) {
  const { data: groups } = useQuery({ queryKey: ["admin", "groups"], queryFn: adminApi.groups });
  const exclusive = (groups ?? []).filter((g) => g.exclusive);
  const { data: current } = useQuery({
    queryKey: ["admin", "users", userId, "model-group-grants"],
    queryFn: () => adminApi.userModelGroupGrants(userId),
  });
  const [picked, setPicked] = useState<string[]>([]);
  useEffect(() => { if (current) setPicked(current); }, [current]);
  const save = useMutation({
    mutationFn: () => adminApi.replaceUserModelGroupGrants(userId, picked),
  });
  if (exclusive.length === 0) return null;
  const dirty = JSON.stringify([...picked].sort()) !== JSON.stringify([...(current ?? [])].sort());
  return (
    <Box>
      <Text size="1" color="gray" as="p" mb="1">专属模型分组（个人授权，不改用户组）</Text>
      <Flex gap="3" wrap="wrap">
        {exclusive.map((g) => (
          <label key={g.id} style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}>
            <Checkbox
              checked={picked.includes(g.id)}
              onCheckedChange={() => setPicked((s) => s.includes(g.id) ? s.filter((x) => x !== g.id) : [...s, g.id])}
            />
            <Text size="2">{g.name}</Text>
            <Badge size="1" variant="soft">×{g.rate_multiplier}</Badge>
          </label>
        ))}
      </Flex>
      {save.isError && <Text size="2" color="red" as="div" mt="1">{errMsg(save.error)}</Text>}
      <Flex justify="end" mt="2">
        <Button size="1" disabled={!dirty} loading={save.isPending} onClick={() => save.mutate()}>保存专属授权</Button>
      </Flex>
    </Box>
  );
}

function pendingTitle(p: PendingAction | null): string {
  switch (p?.kind) {
    case "ban": return "封禁该用户？";
    case "reject": return "拒绝该注册？";
    case "restore": return "恢复该用户？";
    case "delete": return "删除该用户？";
    case "bulk-ban": return `封禁选中的 ${p.ids.length} 位用户？`;
    case "bulk-delete": return `删除选中的 ${p.ids.length} 位用户？`;
    default: return "";
  }
}

function pendingDescription(p: PendingAction | null): string {
  switch (p?.kind) {
    case "ban": return `${p.user.email} 将无法登录和调用接口，可随时解除。`;
    case "reject": return `${p.user.email} 的注册将被拒绝，该邮箱无法再次自动注册。`;
    case "restore": return `${p.user.email} 将恢复为正常状态。`;
    case "delete": return `${p.user.email} 及其 API Key、套餐记录将被删除，此操作不可恢复。`;
    case "bulk-ban": return "被封禁的用户将无法登录和调用接口，可随时解除。";
    case "bulk-delete": return "用户及其 API Key、套餐记录将被删除，此操作不可恢复。";
    default: return "";
  }
}

function pendingConfirmLabel(p: PendingAction | null): string {
  switch (p?.kind) {
    case "ban": case "bulk-ban": return "封禁";
    case "reject": return "拒绝";
    case "restore": return "恢复";
    case "delete": case "bulk-delete": return "删除";
    default: return "确认";
  }
}

function statusBadgeColor(s: string): "green" | "red" | "amber" | "gray" {
  switch (s) {
    case "active":
      return "green";
    case "pending_approval":
      return "amber";
    case "banned":
      return "red";
    case "rejected":
      return "gray";
    default:
      return "gray";
  }
}
