// Accounts: the flat upstream account pool — the primary admin surface that
// replaces the old Providers page.
//
// Admin-only. Accounts are internal infrastructure: this page never renders an
// account identity on any client-facing surface, and nothing here lets a
// request select an account.
import { useEffect, useMemo, useState } from "react";
import {
  AlertDialog, Badge, Box, Button, Callout, Checkbox, Dialog, Flex, IconButton, Select, Text, TextField, Tooltip,
} from "@radix-ui/themes";
import { useMutation, useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { ChevronDown, ChevronLeft, ChevronRight, RotateCcw, Search } from "lucide-react";
import { adminApi, type Account, type AccountListParams, type AccountProtocol } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { showToast } from "../../lib/appToast";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AccountDetailDialog } from "../../components/accounts/AccountDetailDialog";
import { AccountCreateWizard } from "../../components/accounts/AccountCreateWizard";
import { AccountTestDialog } from "../../components/accounts/AccountTestDialog";
import { AccountCard, type AccountCardActions } from "../../components/accounts/AccountCard";
import { accountLabel, protocolLabel, useNow } from "../../components/accounts/AccountHealthBadges";

const PAGE_SIZE = 50;

// The accounts list has no server-side total ({accounts, limit, offset}); this
// hook mirrors usePagedQuery but reports hasNext from a full page instead.
function useAccountsPage(filters: Omit<AccountListParams, "q" | "limit" | "offset">) {
  const [rawQ, setRawQ] = useState("");
  const [q, setQ] = useState("");
  const [page, setPage] = useState(0);

  useEffect(() => {
    const t = window.setTimeout(() => setQ(rawQ.trim()), 250);
    return () => window.clearTimeout(t);
  }, [rawQ]);

  useEffect(() => {
    setPage(0);
  }, [q, filters.status, filters.health, filters.pool_id, filters.protocol]);

  const offset = page * PAGE_SIZE;
  const query = useQuery({
    queryKey: ["admin", "accounts", q, page, filters.status ?? "", filters.health ?? "", filters.pool_id ?? "", filters.protocol ?? ""],
    queryFn: () => adminApi.accountsPage({ ...filters, q: q || undefined, limit: PAGE_SIZE, offset }),
    placeholderData: (prev) => prev,
  });

  const accounts = query.data?.accounts ?? [];
  const hasNext = accounts.length >= PAGE_SIZE;
  return {
    rawQ, setRawQ,
    page, setPage, pageSize: PAGE_SIZE,
    items: accounts,
    total: offset + accounts.length,
    hasPrev: page > 0,
    hasNext,
    query,
  };
}

export default function Accounts() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [status, setStatus] = useState("");
  const [health, setHealth] = useState("");
  const [poolId, setPoolId] = useState("");
  const [protocol, setProtocol] = useState("");
  const [selected, setSelected] = useState<Set<string | number>>(new Set());
  const [detail, setDetail] = useState<Account | null>(null);
  const [detailOpen, setDetailOpen] = useState(false);
  const [wizardOpen, setWizardOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Account | null>(null);
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [poolTarget, setPoolTarget] = useState<Account | null>(null);
  const [poolBulkOpen, setPoolBulkOpen] = useState(false);
  const [testTarget, setTestTarget] = useState<Account | null>(null);
  const [groupOpen, setGroupOpen] = useState(false);
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [error, setError] = useState("");

  const now = useNow(!!detailOpen);
  const paged = useAccountsPage({
    status: status || undefined,
    health: health || undefined,
    pool_id: poolId || undefined,
    protocol: protocol || undefined,
  });

  const pools = useQuery({ queryKey: ["admin", "account-pools"], queryFn: adminApi.accountPools });
  // Pool membership feeds the grouped card sections — pools are few, so one
  // member query per pool is fine and it stays cacheable per pool.
  const poolMemberQs = useQueries({
    queries: (pools.data ?? []).map((p) => ({
      queryKey: ["admin", "account-pool-members", p.id],
      queryFn: () => adminApi.accountPoolMembers(p.id),
    })),
  });
  const accountPoolOf = useMemo(() => {
    const m = new Map<string, string>();
    (pools.data ?? []).forEach((p, i) => {
      for (const mem of poolMemberQs[i]?.data ?? []) {
        // An account can sit in several pools; the first pool (by list order)
        // owns it for display so a card never renders twice.
        if (!m.has(mem.account_id)) m.set(mem.account_id, p.id);
      }
    });
    return m;
  }, [pools.data, poolMemberQs]);

  const invalidate = () => void qc.invalidateQueries({ queryKey: ["admin", "accounts"] });

  const recover = useMutation({
    mutationFn: (id: string) => adminApi.recoverAccount(id),
    onSuccess: () => { showToast({ title: "已恢复", message: "冷却、失败计数与禁用状态已清除" }); invalidate(); },
  });
  const schedulable = useMutation({
    mutationFn: ({ id, next }: { id: string; next: boolean }) => adminApi.setAccountSchedulable(id, next),
    onSuccess: (_d, v) => { showToast({ title: v.next ? "已恢复调度" : "已暂停调度", message: "" }); invalidate(); },
  });
  const clearCooldown = useMutation({
    mutationFn: (id: string) => adminApi.clearAccountCooldown(id),
    onSuccess: () => { showToast({ title: "已解除冷却", message: "该账号可以立即参与调度" }); invalidate(); },
  });
  const refreshQuota = useMutation({
    mutationFn: (id: string) => adminApi.refreshAccountQuota(id),
    onSuccess: () => { showToast({ title: "已查询额度", message: "剩余额度窗口已更新" }); invalidate(); },
  });
  const del = useMutation({
    mutationFn: (id: string) => adminApi.deleteAccount(id),
    onSuccess: () => { setDeleteTarget(null); invalidate(); },
    onError: (e) => setError(`删除失败：${errMsg(e)}`),
  });

  const batchSchedulable = useMutation({
    mutationFn: ({ ids, next }: { ids: string[]; next: boolean }) =>
      Promise.all(ids.map((id) => adminApi.setAccountSchedulable(id, next))),
    onSuccess: (_d, v) => { setSelected(new Set()); showToast({ title: v.next ? "已批量恢复调度" : "已批量暂停调度", message: "" }); invalidate(); },
  });
  const batchCooldown = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.clearAccountCooldown(id))),
    onSuccess: () => { setSelected(new Set()); showToast({ title: "已清除冷却", message: "" }); invalidate(); },
  });
  const batchDelete = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.deleteAccount(id))),
    onSuccess: () => { setBatchDeleteOpen(false); setSelected(new Set()); invalidate(); },
    onError: (e) => setError(`批量删除失败：${errMsg(e)}`),
  });
  const addToPool = useMutation({
    mutationFn: ({ pool, ids }: { pool: string; ids: string[] }) => adminApi.addAccountPoolMembers(pool, ids),
    onSuccess: () => {
      setPoolBulkOpen(false);
      setPoolTarget(null);
      setSelected(new Set());
      showToast({ title: "已加入池", message: "账号已加入所选管理分组" });
      void qc.invalidateQueries({ queryKey: ["admin", "account-pools"] });
    },
    onError: (e) => setError(`加入池失败：${errMsg(e)}`),
  });

  // Create an account pool (管理分组 — a purely internal organization unit,
  // not a user-facing model group) and optionally move the selected accounts
  // into it in the same pass.
  const createPool = useMutation({
    mutationFn: async (v: { name: string; note?: string; accountIds: string[] }) => {
      const p = await adminApi.createAccountPool({ name: v.name, note: v.note });
      if (v.accountIds.length > 0) await adminApi.addAccountPoolMembers(p.id, v.accountIds);
      return p;
    },
    onSuccess: (p, v) => {
      setGroupOpen(false);
      setSelected(new Set());
      void qc.invalidateQueries({ queryKey: ["admin", "account-pools"] });
      void qc.invalidateQueries({ queryKey: ["admin", "account-pool-members"] });
      showToast({ title: "已创建管理分组", message: v.accountIds.length > 0 ? `「${p.name}」已收纳 ${v.accountIds.length} 个账号` : `「${p.name}」已创建` });
    },
    onError: (e) => setError(`创建管理分组失败：${errMsg(e)}`),
  });

  const selectedAccounts = paged.items.filter((a) => selected.has(a.id));
  const selectedIds = selectedAccounts.map((a) => a.id);

  const cardActions = (a: Account): AccountCardActions => ({
    onDetail: (x) => { setDetail(x); setDetailOpen(true); },
    onRecover: (x) => recover.mutate(x.id),
    onToggleSchedulable: (x) => schedulable.mutate({ id: x.id, next: !x.schedulable }),
    onTest: (x) => setTestTarget(x),
    onClearCooldown: (x) => clearCooldown.mutate(x.id),
    onRefreshQuota: (x) => refreshQuota.mutate(x.id),
    onJoinPool: (x) => setPoolTarget(x),
    onDelete: (x) => setDeleteTarget(x),
    pending: {
      recover: recover.isPending && recover.variables === a.id,
      schedulable: schedulable.isPending && schedulable.variables?.id === a.id,
      clearCooldown: clearCooldown.isPending,
      refreshQuota: refreshQuota.isPending,
    },
  });

  // Cards group under their pool (管理分组); anything pool-less falls into
  // 未分组. Sections collapse independently — page-level grouping only, the
  // server-side filter/pagination stays untouched.
  const sections = useMemo(() => {
    const poolList = pools.data ?? [];
    if (poolList.length === 0) {
      return [{ key: "_all", title: "", note: undefined as string | undefined, total: 0, items: paged.items }];
    }
    const byPool = new Map<string, Account[]>();
    const ungrouped: Account[] = [];
    for (const a of paged.items) {
      const pid = accountPoolOf.get(a.id);
      if (pid) byPool.set(pid, [...(byPool.get(pid) ?? []), a]);
      else ungrouped.push(a);
    }
    const out: { key: string; title: string; note?: string; total: number; items: Account[] }[] = [];
    for (const p of poolList) {
      const items = byPool.get(p.id) ?? [];
      if (items.length > 0) out.push({ key: p.id, title: p.name, note: p.note, total: p.account_count, items });
    }
    if (ungrouped.length > 0) out.push({ key: "_ungrouped", title: "未分组", total: ungrouped.length, items: ungrouped });
    return out;
  }, [paged.items, pools.data, accountPoolOf]);

  const toggleSection = (key: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });
  };

  const filters = (
    <>
      <Select.Root value={status || "_any"} onValueChange={(v) => setStatus(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="状态" />
        <Select.Content>
          <Select.Item value="_any">全部状态</Select.Item>
          <Select.Item value="active">正常</Select.Item>
          <Select.Item value="disabled">已停用</Select.Item>
          <Select.Item value="expired">已过期</Select.Item>
          <Select.Item value="error">异常</Select.Item>
        </Select.Content>
      </Select.Root>
      <Select.Root value={health || "_any"} onValueChange={(v) => setHealth(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="健康" />
        <Select.Content>
          <Select.Item value="_any">全部健康状态</Select.Item>
          <Select.Item value="healthy">healthy</Select.Item>
          <Select.Item value="degraded">degraded</Select.Item>
          <Select.Item value="unhealthy">unhealthy</Select.Item>
          <Select.Item value="unknown">unknown</Select.Item>
        </Select.Content>
      </Select.Root>
      <Select.Root value={poolId || "_any"} onValueChange={(v) => setPoolId(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="管理分组" />
        <Select.Content>
          <Select.Item value="_any">全部管理分组</Select.Item>
          {(pools.data ?? []).map((p) => (
            <Select.Item key={p.id} value={p.id}>{p.name}（{p.account_count}）</Select.Item>
          ))}
        </Select.Content>
      </Select.Root>
      <Select.Root value={protocol || "_any"} onValueChange={(v) => setProtocol(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="协议" />
        <Select.Content>
          <Select.Item value="_any">全部协议</Select.Item>
          {(["chat_completions", "responses", "messages"] as AccountProtocol[]).map((p) => (
            <Select.Item key={p} value={p}>{protocolLabel(p)}</Select.Item>
          ))}
        </Select.Content>
      </Select.Root>
    </>
  );

  const bulkActions = (
    <>
      <Button size="1" variant="soft" color="green" loading={batchSchedulable.isPending}
        onClick={() => batchSchedulable.mutate({ ids: selectedIds, next: true })}>启用调度</Button>
      <Button size="1" variant="soft" color="orange" loading={batchSchedulable.isPending}
        onClick={() => batchSchedulable.mutate({ ids: selectedIds, next: false })}>停用调度</Button>
      <Button size="1" variant="soft" color="amber" loading={batchCooldown.isPending}
        onClick={() => batchCooldown.mutate(selectedIds)}>清除冷却</Button>
      <Button size="1" variant="soft" onClick={() => setPoolBulkOpen(true)}>加入池</Button>
      <Button size="1" variant="soft" color="red" loading={batchDelete.isPending}
        onClick={() => setBatchDeleteOpen(true)}>批量删除</Button>
    </>
  );

  const start1 = paged.total === 0 ? 0 : paged.page * paged.pageSize + 1;
  const end1 = Math.min((paged.page + 1) * paged.pageSize, paged.total);

  return (
    <AdminPage
      title="账号"
      description="扁平账号池：端点、方言、凭据、请求塑形、健康与额度都在账号上。账号仅用于服务端调度，客户端看不到也无法指定。"
      actions={(
        <Flex gap="2">
          <Button variant="soft" onClick={() => setGroupOpen(true)}>新建管理分组</Button>
          <Button onClick={() => setWizardOpen(true)}>新建账号</Button>
        </Flex>
      )}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="当前页账号" value={paged.items.length} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="健康" value={paged.items.filter((a) => a.health === "healthy").length} detail="healthy · 当前页" tone="green" />
        <AdminMetricCard label="需关注" value={paged.items.filter((a) => a.health !== "healthy" || a.cooldown_active || !a.schedulable_now).length} detail="冷却/暂停/异常 · 当前页" tone="amber" />
        <AdminMetricCard label="已选择" value={selected.size} detail="支持批量操作" tone={selected.size ? "blue" : "gray"} />
      </AdminMetricGrid>

      {error && <Callout.Root color="red" mb="3"><Callout.Text>{error}</Callout.Text></Callout.Root>}

      <Flex align="start" justify="between" mb="2" wrap="wrap" gap="2">
        <Flex gap="2" align="start" wrap="wrap" style={{ flex: 1, minWidth: 0 }}>
          <Box style={{ width: 220, maxWidth: 360 }}>
            <ClearableTextField
              placeholder="搜索名称 / URL / 外部身份..."
              value={paged.rawQ}
              onChange={(e) => paged.setRawQ(e.target.value)}
            >
              <TextField.Slot><Search size={14} /></TextField.Slot>
            </ClearableTextField>
          </Box>
          <Tooltip content="刷新">
            <IconButton size="2" variant="soft" color="gray" onClick={() => paged.query.refetch()} aria-label="刷新">
              <RotateCcw size={14} />
            </IconButton>
          </Tooltip>
          {filters}
        </Flex>
      </Flex>

      {selected.size > 0 && (
        <Flex align="center" justify="between" gap="2" wrap="wrap" mb="2">
          <Flex align="center" gap="2">
            <Badge color="iris" variant="soft">{selected.size} 已选择</Badge>
            <Button size="1" variant="ghost" color="gray" onClick={() => setSelected(new Set())}>清空</Button>
          </Flex>
          <Flex gap="2" justify="end" wrap="wrap">{bulkActions}</Flex>
        </Flex>
      )}

      {paged.query.isLoading ? (
        <Text size="2" color="gray" style={{ display: "block", textAlign: "center", padding: "40px 0" }}>加载中…</Text>
      ) : paged.items.length === 0 ? (
        <Text size="2" color="gray" style={{ display: "block", textAlign: "center", padding: "40px 0" }}>
          还没有账号 — 点右上角「新建账号」，从提供商定义、目录模板或自定义端点创建
        </Text>
      ) : (
        <Flex direction="column" gap="3">
          {sections.map((sec) => {
            const isCollapsed = collapsed.has(sec.key);
            return (
              <Box key={sec.key}>
                {sec.title && (
                  <Flex
                    align="center"
                    gap="2"
                    mb={isCollapsed ? "0" : "2"}
                    style={{ cursor: "pointer", userSelect: "none" }}
                    onClick={() => toggleSection(sec.key)}
                  >
                    {isCollapsed ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                    <Text size="2" weight="medium">{sec.title}</Text>
                    <Badge variant="soft" color="gray">
                      {sec.items.length === sec.total ? `${sec.items.length} 个账号` : `本页 ${sec.items.length} / 共 ${sec.total}`}
                    </Badge>
                    {sec.note && <Text size="1" color="gray">{sec.note}</Text>}
                  </Flex>
                )}
                {!isCollapsed && (
                  <Box style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(340px, 1fr))", gap: 10 }}>
                    {sec.items.map((a) => (
                      <AccountCard
                        key={a.id}
                        account={a}
                        now={now}
                        selected={selected.has(a.id)}
                        onSelectChange={(checked) => {
                          const next = new Set(selected);
                          if (checked) next.add(a.id); else next.delete(a.id);
                          setSelected(next);
                        }}
                        actions={cardActions(a)}
                      />
                    ))}
                  </Box>
                )}
              </Box>
            );
          })}
        </Flex>
      )}

      <Flex gap="1" align="center" justify="end" mt="2">
        <Text size="1" color="gray">
          {paged.total === 0 ? "0 条" : `${start1}–${end1} / 共 ${paged.total}`}
        </Text>
        <IconButton size="1" variant="soft" disabled={!paged.hasPrev || paged.query.isFetching}
          onClick={() => paged.setPage(Math.max(0, paged.page - 1))} aria-label="上一页">
          <ChevronLeft size={14} />
        </IconButton>
        <Button size="1" variant="ghost" disabled>
          {paged.page + 1} / {Math.max(1, Math.ceil(paged.total / paged.pageSize))}
        </Button>
        <IconButton size="1" variant="soft" disabled={!paged.hasNext || paged.query.isFetching}
          onClick={() => paged.setPage(paged.page + 1)} aria-label="下一页">
          <ChevronRight size={14} />
        </IconButton>
      </Flex>

      <Flex justify="end" mt="3" gap="2">
        <Button variant="soft" color="gray" onClick={() => navigate("/admin/topology")}>负载拓扑</Button>
        <Button variant="soft" color="gray" onClick={() => navigate("/admin/routing")}>查看路由预览</Button>
      </Flex>

      <AccountDetailDialog
        account={detail}
        open={detailOpen}
        onOpenChange={(open) => { setDetailOpen(open); if (!open) setDetail(null); }}
      />
      <AccountCreateWizard open={wizardOpen} onOpenChange={setWizardOpen} />

      <AccountTestDialog
        account={testTarget}
        open={!!testTarget}
        onOpenChange={(open) => { if (!open) setTestTarget(null); }}
      />

      <AlertDialog.Root open={!!deleteTarget} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <AlertDialog.Content style={{ maxWidth: 460 }}>
          <AlertDialog.Title>删除账号</AlertDialog.Title>
          <AlertDialog.Description size="2">
            「{deleteTarget ? accountLabel(deleteTarget) : ""}」会立即退出调度，历史用量记录保留（用于对账）。已经发出的请求不受影响。
          </AlertDialog.Description>
          <Flex gap="2" justify="end" mt="4">
            <AlertDialog.Cancel><Button variant="soft" color="gray">取消</Button></AlertDialog.Cancel>
            <AlertDialog.Action>
              <Button color="red" loading={del.isPending} onClick={() => deleteTarget && del.mutate(deleteTarget.id)}>删除</Button>
            </AlertDialog.Action>
          </Flex>
        </AlertDialog.Content>
      </AlertDialog.Root>

      <AlertDialog.Root open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialog.Content style={{ maxWidth: 460 }}>
          <AlertDialog.Title>批量删除账号</AlertDialog.Title>
          <AlertDialog.Description size="2">
            将删除选中的 {selectedIds.length} 个账号，它们会立即退出调度。历史用量记录保留用于对账。
          </AlertDialog.Description>
          <Flex gap="2" justify="end" mt="4">
            <AlertDialog.Cancel><Button variant="soft" color="gray">取消</Button></AlertDialog.Cancel>
            <AlertDialog.Action>
              <Button color="red" loading={batchDelete.isPending} onClick={() => batchDelete.mutate(selectedIds)}>删除</Button>
            </AlertDialog.Action>
          </Flex>
        </AlertDialog.Content>
      </AlertDialog.Root>

      <PoolPickerDialog
        open={poolBulkOpen || !!poolTarget}
        pools={pools.data ?? []}
        title={poolTarget ? `把「${accountLabel(poolTarget)}」加入管理分组` : `把选中的 ${selectedIds.length} 个账号加入管理分组`}
        onClose={() => { setPoolBulkOpen(false); setPoolTarget(null); }}
        onPick={(pool) => addToPool.mutate({ pool, ids: poolTarget ? [poolTarget.id] : selectedIds })}
      />

      <CreatePoolDialog
        open={groupOpen}
        onClose={() => setGroupOpen(false)}
        selectedCount={selectedIds.length}
        saving={createPool.isPending}
        error={createPool.isError ? errMsg(createPool.error) : ""}
        onCreate={(v) => createPool.mutate({ name: v.name, note: v.note, accountIds: v.bindSelected ? selectedIds : [] })}
      />
    </AdminPage>
  );
}

// PoolPickerDialog adds one or many accounts to an operator-defined pool.
function PoolPickerDialog({
  open, pools, title, onPick, onClose,
}: {
  open: boolean;
  pools: { id: string; name: string; account_count: number }[];
  title: string;
  onPick: (poolId: string) => void;
  onClose: () => void;
}) {
  const [pool, setPool] = useState("");
  return (
    <Dialog.Root open={open} onOpenChange={(o) => !o && onClose()}>
      <Dialog.Content style={{ maxWidth: 460 }}>
        <Dialog.Title>加入管理分组</Dialog.Title>
        <Dialog.Description size="2">{title}</Dialog.Description>
        <Box mt="3">
          <Select.Root value={pool || "_none"} onValueChange={(v) => setPool(v === "_none" ? "" : v)}>
            <Select.Trigger style={{ width: "100%" }} placeholder="选择管理分组" />
            <Select.Content>
              <Select.Item value="_none">选择管理分组</Select.Item>
              {pools.map((p) => <Select.Item key={p.id} value={p.id}>{p.name}（{p.account_count}）</Select.Item>)}
            </Select.Content>
          </Select.Root>
          {pools.length === 0 && (
            <Text size="1" color="gray" as="div" mt="2">还没有管理分组。可到「路由 / 渠道」或管理分组接口创建。</Text>
          )}
        </Box>
        <Flex gap="2" justify="end" mt="4">
          <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
          <Button disabled={!pool} onClick={() => onPick(pool)}>加入</Button>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

// CreatePoolDialog spins up an account pool (管理分组) — a purely internal
// organization unit for the pool view, not a user-facing model group. The
// operator can move the currently-selected accounts in the same pass.
function CreatePoolDialog({
  open, onClose, selectedCount, saving, error, onCreate,
}: {
  open: boolean;
  onClose: () => void;
  selectedCount: number;
  saving: boolean;
  error: string;
  onCreate: (v: { name: string; note?: string; bindSelected: boolean }) => void;
}) {
  const [form, setForm] = useState({ name: "", note: "" });
  const [bindSelected, setBindSelected] = useState(true);
  useEffect(() => {
    if (open) {
      setForm({ name: "", note: "" });
      setBindSelected(true);
    }
  }, [open]);
  return (
    <Dialog.Root open={open} onOpenChange={(o) => !o && onClose()}>
      <Dialog.Content style={{ maxWidth: 420 }}>
        <Dialog.Title>新建管理分组</Dialog.Title>
        <Dialog.Description size="2">
          管理分组是纯粹的内部组织方式，只用于在后台归置账号，不参与路由、不对用户可见。
        </Dialog.Description>
        <Flex direction="column" gap="3" mt="3">
          <label>
            <Text size="1" color="gray" as="div" mb="1">名称</Text>
            <TextField.Root
              placeholder="如 主用 Claude 池"
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
            />
          </label>
          <label>
            <Text size="1" color="gray" as="div" mb="1">备注（可选）</Text>
            <TextField.Root
              placeholder="这批账号的用途"
              value={form.note}
              onChange={(e) => setForm({ ...form, note: e.target.value })}
            />
          </label>
          {selectedCount > 0 && (
            <Text as="label" size="2">
              <Flex gap="2" align="center">
                <Checkbox checked={bindSelected} onCheckedChange={(v) => setBindSelected(v === true)} />
                同时把选中的 {selectedCount} 个账号收纳进去
              </Flex>
            </Text>
          )}
          {error && <Text size="1" color="red">{error}</Text>}
        </Flex>
        <Flex gap="2" justify="end" mt="4">
          <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
          <Button
            loading={saving}
            disabled={!form.name.trim()}
            onClick={() => onCreate({
              name: form.name.trim(),
              note: form.note.trim() || undefined,
              bindSelected,
            })}
          >
            创建
          </Button>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
