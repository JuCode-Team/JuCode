import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Badge, Box, Button, Card, Checkbox, Dialog, Flex, Heading, Select, Switch, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  adminApi, type Account, type AccountModel, type AccountPool, type GroupAccount, type GroupProfileDim, type Model, type ModelGroup,
  type ModelGroupModelLink,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";
import { SortableItem, SortableList } from "../../components/SortableList";
import { accountLabel, protocolLabel } from "../../components/accounts/AccountHealthBadges";

export default function Groups() {
  const qc = useQueryClient();
  const { data: groups, isLoading } = useQuery({ queryKey: ["admin", "groups"], queryFn: adminApi.groups });
  const { data: models } = useQuery({ queryKey: ["admin", "models"], queryFn: adminApi.models });
  const { data: accounts } = useQuery({ queryKey: ["admin", "accounts", "all"], queryFn: () => adminApi.accounts() });
  const { data: pools } = useQuery({ queryKey: ["admin", "account-pools"], queryFn: adminApi.accountPools });
  const { data: userGroups } = useQuery({ queryKey: ["admin", "user-groups"], queryFn: adminApi.userGroups });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["admin", "groups"] });

  const updateGroup = useMutation({
    mutationFn: (v: { id: string; patch: Partial<{ name: string; billing_source: string; rate_multiplier: number; description: string; profile: GroupProfileDim[]; allowed_user_group_ids: string[]; enabled: boolean; exclusive: boolean }> }) =>
      adminApi.updateGroup(v.id, v.patch),
    onSuccess: invalidate,
  });

  const ugName = new Map((userGroups ?? []).map((g) => [g.id, g.name]));

  const [open, setOpen] = useState(false);
  const [manageId, setManageId] = useState<string | null>(null);
  const [form, setForm] = useState({ name: "", billing_source: "balance_only", rate_multiplier: "1.0", description: "" });
  const [q, setQ] = useState("");
  const [statusFilter, setStatusFilter] = useState("all");
  const [billingFilter, setBillingFilter] = useState("");

  const create = useMutation({
    mutationFn: () =>
      adminApi.createGroup({
        name: form.name,
        billing_source: form.billing_source,
        rate_multiplier: parseFloat(form.rate_multiplier),
        description: form.description,
      }),
    onSuccess: () => { setOpen(false); invalidate(); },
  });
  const clone = useMutation({
    mutationFn: (id: string) => adminApi.cloneGroup(id, { exclusive: true }),
    onSuccess: (g) => {
      invalidate();
      setManageId(g.id);
    },
  });
  const saveGrants = useMutation({
    mutationFn: (v: { id: string; user_ids: string[]; user_group_ids: string[] }) =>
      adminApi.replaceGroupGrants(v.id, { user_ids: v.user_ids, user_group_ids: v.user_group_ids }),
    onSuccess: invalidate,
  });
  const del = useMutation({
    mutationFn: (id: string) => adminApi.deleteGroup(id),
    onSuccess: () => {
      if (manageId) setManageId(null);
      invalidate();
    },
  });
  const attachModel = useMutation({
    mutationFn: (v: { groupId: string; modelId: string }) => adminApi.attachModel(v.groupId, v.modelId),
    onSuccess: invalidate,
  });
  const detachModel = useMutation({
    mutationFn: (v: { groupId: string; modelId: string }) => adminApi.detachModel(v.groupId, v.modelId),
    onSuccess: invalidate,
  });
  const setModelProbe = useMutation({
    mutationFn: (v: { groupId: string; modelId: string; patch: { probe_enabled?: boolean; probe_interval_seconds?: number } }) =>
      adminApi.setModelProbe(v.groupId, v.modelId, v.patch),
    onSuccess: invalidate,
  });
  const attachAccount = useMutation({
    mutationFn: (v: { groupId: string; accountId: string; patch: { priority?: number; weight?: number; enabled?: boolean; model_names?: string[] } }) =>
      adminApi.attachAccount(v.groupId, { account_id: v.accountId, ...v.patch }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "group-accounts"] });
      invalidate();
    },
  });
  const detachAccount = useMutation({
    mutationFn: (v: { groupId: string; accountId: string }) => adminApi.detachAccount(v.groupId, v.accountId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "group-accounts"] });
      invalidate();
    },
  });
  // 按池批量添加: resolve the pool's members, then bind each to the group.
  const addByPool = useMutation({
    mutationFn: async (v: { groupId: string; poolId: string }) => {
      const members = await adminApi.accountPoolMembers(v.poolId);
      await Promise.all(members.map((m) => adminApi.attachAccount(v.groupId, { account_id: m.account_id })));
      return members.length;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "group-accounts"] });
      invalidate();
    },
  });
  // Reordering writes the new order into the cache before the request lands.
  // Without that the list snaps back to the server order for one frame and the
  // drag looks like it failed.
  const reorder = useMutation({
    mutationFn: (ids: string[]) => adminApi.reorderGroups(ids),
    onMutate: async (ids: string[]) => {
      await qc.cancelQueries({ queryKey: ["admin", "groups"] });
      const prev = qc.getQueryData<ModelGroup[]>(["admin", "groups"]);
      if (prev) {
        const byId = new Map(prev.map((g) => [g.id, g]));
        const next = ids.map((id) => byId.get(id)).filter((g): g is ModelGroup => Boolean(g));
        qc.setQueryData<ModelGroup[]>(["admin", "groups"], next);
      }
      return { prev };
    },
    onError: (_e, _ids, ctx) => {
      if (ctx?.prev) qc.setQueryData(["admin", "groups"], ctx.prev);
    },
    onSettled: invalidate,
  });

  if (isLoading) return <RouteSkeleton />;

  const writeError = updateGroup.error ?? del.error ?? attachModel.error ?? detachModel.error
    ?? setModelProbe.error ?? attachAccount.error ?? detachAccount.error ?? addByPool.error ?? reorder.error
    ?? clone.error ?? saveGrants.error;

  const manageGroup = manageId ? (groups ?? []).find((g) => g.id === manageId) ?? null : null;
  const enabledCount = (groups ?? []).filter((g) => g.enabled !== false).length;
  const disabledCount = (groups ?? []).length - enabledCount;

  const visibleGroups = (groups ?? []).filter((g) => {
    const hit = !q.trim()
      || g.name.toLowerCase().includes(q.trim().toLowerCase())
      || (g.description ?? "").toLowerCase().includes(q.trim().toLowerCase());
    const billingHit = !billingFilter || g.billing_source === billingFilter;
    const statusHit = statusFilter === "all"
      || (statusFilter === "enabled" ? g.enabled !== false : g.enabled === false);
    return hit && billingHit && statusHit;
  });

  const sortable = !q.trim() && !billingFilter && statusFilter === "all";

  const groupCard = (g: ModelGroup, handle?: ReactNode) => {
    const enabled = g.enabled !== false;
    return (
      <Card style={enabled ? undefined : { opacity: 0.7 }}>
        <Flex justify="between" align="center" gap="2" wrap="wrap">
          <Box style={{ minWidth: 0, flex: 1 }}>
            <Flex align="center" gap="2">
              {handle}
              <Heading size="4">{g.name}</Heading>
              <Badge color={enabled ? "green" : "red"} variant="soft">{enabled ? "已启用" : "已禁用"}</Badge>
            </Flex>
            <Flex gap="2" mt="1" wrap="wrap">
              <Badge color={g.billing_source === "plan_only" ? "purple" : "blue"}>{g.billing_source}</Badge>
              <Badge color="gray">×{g.rate_multiplier}</Badge>
              {g.exclusive
                ? <Badge variant="soft" color="amber">专属</Badge>
                : <Badge variant="soft" color="green">公开</Badge>}
              {g.exclusive && (g.allowed_user_group_ids ?? []).map((id) => (
                <Badge key={id} variant="soft" color="iris">用户组: {ugName.get(id) ?? id.slice(0, 6)}</Badge>
              ))}
              {g.exclusive && (g.granted_user_count ?? 0) > 0 && (
                <Badge variant="soft" color="iris">{g.granted_user_count} 人</Badge>
              )}
              <Badge variant="soft" color="gray">{g.model_ids?.length ?? 0} 模型</Badge>
            </Flex>
            {g.description && <Text size="2" color="gray" mt="1" as="div">{g.description}</Text>}
          </Box>
          <Flex gap="2" align="center" style={{ flexShrink: 0 }}>
            <label style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }} title={enabled ? "点击禁用（停止路由并从用户端隐藏）" : "点击启用"}>
              <Switch checked={enabled} onCheckedChange={(v) => updateGroup.mutate({ id: g.id, patch: { enabled: v } })} />
            </label>
            <Button size="1" variant="soft" onClick={() => setManageId(g.id)}>管理分组</Button>
            <Button size="1" variant="soft" loading={clone.isPending && clone.variables === g.id} onClick={() => {
              if (confirm(`复制模型分组「${g.name}」为专属副本？将拷贝模型和账号绑定，探测默认关闭，授权从空开始。`)) clone.mutate(g.id);
            }}>复制</Button>
            <Button size="1" color="red" variant="soft" onClick={() => {
              if (confirm(`删除模型分组 "${g.name}"？`)) del.mutate(g.id);
            }}>删除</Button>
          </Flex>
        </Flex>
      </Card>
    );
  };

  return (
    <AdminPage
      title="模型分组"
      description="控制模型、账号和用户组之间的访问与计费关系。业务分组直接绑定账号（而非渠道）。可复制分组并设为专属，只对授权用户开放。"
      actions={<Button onClick={() => setOpen(true)}>新建分组</Button>}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="分组总数" value={(groups ?? []).length} detail="模型路由分组" tone="iris" />
        <AdminMetricCard label="已启用" value={enabledCount} detail="正常参与路由" tone="green" />
        <AdminMetricCard label="已禁用" value={disabledCount} detail="停止路由并隐藏" tone="red" />
        <AdminMetricCard label="当前匹配" value={visibleGroups.length} detail="本页筛选结果" tone="blue" />
      </AdminMetricGrid>

      <Card className="pn-card-flat" mb="3">
        <Flex gap="2" align="center" wrap="wrap">
          <Box style={{ flex: "1 1 260px", minWidth: 0 }}>
            <ClearableTextField placeholder="搜索分组名称 / 描述" value={q} onChange={(e) => setQ(e.target.value)} />
          </Box>
          <Select.Root value={statusFilter} onValueChange={setStatusFilter}>
            <Select.Trigger placeholder="状态" />
            <Select.Content>
              <Select.Item value="all">全部状态</Select.Item>
              <Select.Item value="enabled">已启用</Select.Item>
              <Select.Item value="disabled">已禁用</Select.Item>
            </Select.Content>
          </Select.Root>
          <Select.Root value={billingFilter || "_any"} onValueChange={(v) => setBillingFilter(v === "_any" ? "" : v)}>
            <Select.Trigger placeholder="计费来源" />
            <Select.Content>
              <Select.Item value="_any">全部计费来源</Select.Item>
              <Select.Item value="balance_only">balance_only</Select.Item>
              <Select.Item value="plan_only">plan_only</Select.Item>
            </Select.Content>
          </Select.Root>
        </Flex>
      </Card>
      {writeError != null && (
        <Text size="2" color="red" as="div" mb="2">操作失败: {errMsg(writeError)}</Text>
      )}
      {!sortable && (
        <Text size="1" color="gray" as="div" mb="2">筛选状态下不能拖拽排序，清空搜索、状态和计费来源筛选后可调整顺序。</Text>
      )}
      <Flex direction="column" gap="3">
        {sortable ? (
          <SortableList
            items={visibleGroups}
            getId={(g) => g.id}
            onReorder={(ids) => reorder.mutate(ids)}
            renderItem={(g) => (
              <SortableItem key={g.id} id={g.id}>
                {(handle) => groupCard(g, handle)}
              </SortableItem>
            )}
          />
        ) : (
          visibleGroups.map((g) => <Box key={g.id}>{groupCard(g)}</Box>)
        )}
        {visibleGroups.length === 0 && (
          <Card className="pn-card-flat">
            <Text size="2" color="gray">没有匹配的模型分组</Text>
          </Card>
        )}
      </Flex>

      <Dialog.Root open={open} onOpenChange={setOpen}>
        <Dialog.Content style={{ maxWidth: 420 }}>
          <Dialog.Title>新建模型分组</Dialog.Title>
          {/* allowed_user_group_ids is set after creation via the "管理分组" dialog */}
          <Flex direction="column" gap="3" mt="3">
            <ClearableTextField placeholder="名称" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
            <Select.Root value={form.billing_source} onValueChange={(v) => setForm({ ...form, billing_source: v })}>
              <Select.Trigger />
              <Select.Content>
                <Select.Item value="balance_only">balance_only</Select.Item>
                <Select.Item value="plan_only">plan_only</Select.Item>
              </Select.Content>
            </Select.Root>
            <ClearableTextField placeholder="rate_multiplier (默认 1.0)" value={form.rate_multiplier} onChange={(e) => setForm({ ...form, rate_multiplier: e.target.value })} />
            <ClearableTextField placeholder="描述（可选）" value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} />
            {create.isError && <Text size="2" color="red">创建失败: {errMsg(create.error)}</Text>}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => setOpen(false)}>取消</Button>
              <Button loading={create.isPending} onClick={() => create.mutate()}>创建</Button>
            </Flex>
          </Flex>
        </Dialog.Content>
      </Dialog.Root>

      <Dialog.Root open={manageId !== null} onOpenChange={(o) => { if (!o) setManageId(null); }}>
        <Dialog.Content style={{ maxWidth: 820, maxHeight: "85vh" }}>
          {manageGroup && (
            <>
              <Flex align="center" justify="between" gap="2" wrap="wrap">
                <Box>
                  <Dialog.Title>{manageGroup.name}</Dialog.Title>
                  <Dialog.Description size="2" color="gray">
                    编辑分组信息、访问权限，以及把账号与模型加入本分组。
                  </Dialog.Description>
                </Box>
                <Badge color={manageGroup.enabled !== false ? "green" : "red"} variant="soft">
                  {manageGroup.enabled !== false ? "已启用" : "已禁用"}
                </Badge>
              </Flex>
              <Flex direction="column" gap="5" mt="3" style={{ maxHeight: "65vh", overflowY: "auto", paddingRight: 6 }}>
                <Box>
                  <Text size="2" color="gray" mb="2" as="p">基础信息：</Text>
                  <GroupDetailsEditor
                    group={manageGroup}
                    onSave={(patch) => updateGroup.mutate({ id: manageGroup.id, patch })}
                    saving={updateGroup.isPending && updateGroup.variables?.id === manageGroup.id && !("allowed_user_group_ids" in (updateGroup.variables.patch ?? {})) && !("enabled" in (updateGroup.variables.patch ?? {}))}
                  />
                </Box>
                <Box>
                  <Flex align="center" gap="2" mb="2">
                    <Switch
                      checked={!!manageGroup.exclusive}
                      onCheckedChange={(v) => updateGroup.mutate({ id: manageGroup.id, patch: { exclusive: v } })}
                    />
                    <Text size="2">专属分组（仅授权用户 / 用户组可访问）</Text>
                  </Flex>
                  {manageGroup.billing_source === "plan_only" && manageGroup.exclusive && (
                    <Text size="1" color="orange" as="p" mb="2">
                      套餐白名单不会自动包含这个副本。请到套餐页把本分组勾进 allowed_group_ids，否则套餐用户有授权也进不去。
                    </Text>
                  )}
                  {manageGroup.exclusive ? (
                    <ExclusiveAccessEditor
                      group={manageGroup}
                      userGroups={userGroups ?? []}
                      saving={saveGrants.isPending}
                      onSave={(grants) => saveGrants.mutate({ id: manageGroup.id, ...grants })}
                    />
                  ) : (
                    <Text size="2" color="gray">公开分组对所有人开放。授权名单在打开专属后生效。</Text>
                  )}
                </Box>
                <Box>
                  <Text size="2" color="gray" mb="2" as="p">账号绑定（priority 越小越先尝试，weight 同优先级下按权重随机）：</Text>
                  <GroupAccountsEditor
                    group={manageGroup}
                    accounts={accounts ?? []}
                    pools={pools ?? []}
                    models={models ?? []}
                    attachAccount={attachAccount}
                    detachAccount={detachAccount}
                    addByPool={addByPool}
                    attachModel={attachModel}
                    detachModel={detachModel}
                    probe={setModelProbe}
                  />
                </Box>
              </Flex>
            </>
          )}
        </Dialog.Content>
      </Dialog.Root>
    </AdminPage>
  );
}

type MemberPatch = { priority?: number; weight?: number; enabled?: boolean; model_names?: string[] };
type AccountMutation = {
  mutate: (v: { groupId: string; accountId: string; patch: MemberPatch }) => void;
  isPending: boolean;
  variables?: { groupId: string; accountId: string; patch: MemberPatch } | null;
};
type RemoveMutation = {
  mutate: (v: { groupId: string; accountId: string }) => void;
  isPending: boolean;
  variables?: { groupId: string; accountId: string } | null;
};

// GroupAccountsEditor owns the member list (only explicitly bound accounts),
// the add-account / add-by-pool controls, and the group's public model set.
function GroupAccountsEditor({
  group, accounts, pools, models, attachAccount, detachAccount, addByPool, attachModel, detachModel, probe,
}: {
  group: ModelGroup;
  accounts: Account[];
  pools: AccountPool[];
  models: Model[];
  attachAccount: AccountMutation;
  detachAccount: RemoveMutation;
  addByPool: { mutate: (v: { groupId: string; poolId: string }) => void; isPending: boolean; variables?: { groupId: string; poolId: string } | null };
  attachModel: { mutate: (v: { groupId: string; modelId: string }) => void; isPending: boolean; variables?: { groupId: string; modelId: string } | null };
  detachModel: { mutate: (v: { groupId: string; modelId: string }) => void; isPending: boolean; variables?: { groupId: string; modelId: string } | null };
  probe: { mutate: (v: ProbeMutationVars) => void; isPending: boolean; variables?: ProbeMutationVars | null };
}) {
  const boundQuery = useQuery({
    queryKey: ["admin", "group-accounts", group.id],
    queryFn: () => adminApi.groupAccounts(group.id),
  });
  const bound = boundQuery.data ?? [];
  const accountById = new Map(accounts.map((a) => [a.id, a]));
  const boundIds = new Set(bound.map((b) => b.account_id));
  const addable = accounts.filter((a) => !boundIds.has(a.id));
  const [addSearch, setAddSearch] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [poolId, setPoolId] = useState("");
  const addableFiltered = useMemo(() => {
    const q = addSearch.trim().toLowerCase();
    if (!q) return addable;
    return addable.filter((a) =>
      accountLabel(a).toLowerCase().includes(q)
      || a.base_url.toLowerCase().includes(q)
      || (a.native_protocols ?? []).some((p) => protocolLabel(p).toLowerCase().includes(q)));
  }, [addable, addSearch]);
  // Attached accounts leave the addable list; prune them from the pick set so
  // the counter can't drift after refetches.
  useEffect(() => {
    setPicked((prev) => {
      const next = new Set([...prev].filter((id) => addable.some((a) => a.id === id)));
      return next.size === prev.size ? prev : next;
    });
  }, [addable]);

  if (accounts.length === 0) {
    return <Text size="2" color="gray">尚未创建任何账号。去「账号」页面新建。</Text>;
  }

  return (
    <Flex direction="column" gap="4">
      {/* 成员账号：只显示已绑定 */}
      <Flex direction="column" gap="2">
        {boundQuery.isLoading && <Text size="2" color="gray">加载成员…</Text>}
        {!boundQuery.isLoading && bound.length === 0 && (
          <Flex p="3" style={{ border: "1px dashed var(--gray-6)", borderRadius: 8 }}>
            <Text size="2" color="gray">还没有成员账号。从下面选择账号添加，成员才会参与本分组的路由。</Text>
          </Flex>
        )}
        {bound.map((link) => (
          <GroupMemberRow
            key={link.account_id}
            groupId={group.id}
            link={link}
            account={accountById.get(link.account_id)}
            saving={attachAccount.isPending && attachAccount.variables?.groupId === group.id && attachAccount.variables?.accountId === link.account_id}
            removing={detachAccount.isPending && detachAccount.variables?.groupId === group.id && detachAccount.variables?.accountId === link.account_id}
            onSave={attachAccount.mutate}
            onRemove={detachAccount.mutate}
          />
        ))}
      </Flex>

      {/* 添加成员：可搜索的勾选列表，一次可加多个 */}
      <Flex direction="column" gap="2" p="2" style={{ border: "1px dashed var(--gray-6)", borderRadius: 8 }}>
        <Flex gap="2" align="center" wrap="wrap">
          <Box style={{ flex: "1 1 220px", minWidth: 0 }}>
            <ClearableTextField
              placeholder="搜索未绑定账号（名称 / 端点 / 协议）..."
              value={addSearch}
              onChange={(e) => setAddSearch(e.target.value)}
            />
          </Box>
          <Text size="1" color="gray">
            {picked.size > 0 ? `已选 ${picked.size} / ` : ""}{addableFiltered.length} 个可添加
          </Text>
          {addableFiltered.length > 0 && (
            <Button
              size="1"
              variant="ghost"
              color="gray"
              onClick={() => {
                const all = addableFiltered.every((a) => picked.has(a.id));
                const next = new Set(picked);
                for (const a of addableFiltered) {
                  if (all) next.delete(a.id); else next.add(a.id);
                }
                setPicked(next);
              }}
            >
              {addableFiltered.every((a) => picked.has(a.id)) ? "取消全选" : "全选"}
            </Button>
          )}
        </Flex>
        {addable.length === 0 && bound.length > 0 ? (
          <Text size="1" color="gray">所有账号都已在本分组</Text>
        ) : addableFiltered.length === 0 ? (
          <Text size="1" color="gray">没有匹配的未绑定账号</Text>
        ) : (
          <Box style={{ maxHeight: 220, overflowY: "auto" }}>
            <Flex direction="column">
              {addableFiltered.map((a) => (
                <Text as="label" size="2" key={a.id} style={{ cursor: "pointer" }}>
                  <Flex gap="2" align="center" py="1" px="1" style={{ borderRadius: 6 }}>
                    <Checkbox
                      checked={picked.has(a.id)}
                      onCheckedChange={(v) => {
                        const next = new Set(picked);
                        if (v === true) next.add(a.id); else next.delete(a.id);
                        setPicked(next);
                      }}
                    />
                    <Text size="2" style={{ minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                      {accountLabel(a)}
                    </Text>
                    {(a.native_protocols ?? []).map((p) => (
                      <Badge key={p} variant="soft" color="blue" style={{ flexShrink: 0 }}>{protocolLabel(p)}</Badge>
                    ))}
                    {!a.schedulable_now && <Badge color="amber" variant="soft" style={{ flexShrink: 0 }}>暂停</Badge>}
                    {a.health !== "healthy" && <Badge color="red" variant="soft" style={{ flexShrink: 0 }}>{a.health}</Badge>}
                  </Flex>
                </Text>
              ))}
            </Flex>
          </Box>
        )}
        <Flex justify="end">
          <Button
            size="2"
            variant="soft"
            disabled={picked.size === 0}
            loading={attachAccount.isPending}
            onClick={() => {
              for (const id of picked) attachAccount.mutate({ groupId: group.id, accountId: id, patch: {} });
              setPicked(new Set());
            }}
          >
            添加选中{picked.size > 0 ? `的 ${picked.size} 个` : ""}账号
          </Button>
        </Flex>
      </Flex>

      {/* 按池批量添加 */}
      <Flex gap="2" align="end" wrap="wrap" p="2" style={{ border: "1px dashed var(--gray-6)", borderRadius: 8 }}>
        <Box style={{ flex: "1 1 220px", minWidth: 0 }}>
          <Text size="1" color="gray" as="div" mb="1">按池批量添加（把某个管理分组里的账号全部绑定到本分组）</Text>
          <Select.Root value={poolId || "_none"} onValueChange={(v) => setPoolId(v === "_none" ? "" : v)}>
            <Select.Trigger style={{ width: "100%" }} placeholder="选择管理分组" />
            <Select.Content>
              <Select.Item value="_none">选择管理分组</Select.Item>
              {pools.map((p) => <Select.Item key={p.id} value={p.id}>{p.name}（{p.account_count}）</Select.Item>)}
            </Select.Content>
          </Select.Root>
        </Box>
        <Button
          size="2"
          variant="soft"
          disabled={!poolId}
          loading={addByPool.isPending && addByPool.variables?.groupId === group.id}
          onClick={() => addByPool.mutate({ groupId: group.id, poolId })}
        >
          按池批量添加
        </Button>
      </Flex>

      {/* 对外启用的模型：对外显示并参与路由 */}
      <Box>
        <Text size="2" color="gray" mb="2" as="p">对外启用的模型（对外显示并参与路由；先选成员账号，再勾选其模型）：</Text>
        <AccountModelPicker group={group} bound={bound} models={models} attach={attachModel} detach={detachModel} probe={probe} />
      </Box>
    </Flex>
  );
}

// GroupMemberRow is one bound account: scheduling inputs, the per-group model
// whitelist (empty = every configured model), and removal.
function GroupMemberRow({
  groupId, link, account, saving, removing, onSave, onRemove,
}: {
  groupId: string;
  link: GroupAccount;
  account?: Account;
  saving: boolean;
  removing: boolean;
  onSave: (v: { groupId: string; accountId: string; patch: MemberPatch }) => void;
  onRemove: (v: { groupId: string; accountId: string }) => void;
}) {
  const [priority, setPriority] = useState(String(link.priority ?? 100));
  const [weight, setWeight] = useState(String(link.weight ?? 100));
  const [modelsOpen, setModelsOpen] = useState(false);

  useEffect(() => {
    setPriority(String(link.priority ?? 100));
    setWeight(String(link.weight ?? 100));
  }, [link.priority, link.weight, link.account_id]);

  const parsedPriority = parseInt(priority) || 0;
  const parsedWeight = parseInt(weight) || 0;
  // 0 inherits the account's own scheduling input — that is a valid value, not
  // missing input.
  const invalid = parsedPriority < 0 || parsedWeight < 0;
  const dirty = parsedPriority !== link.priority || parsedWeight !== link.weight;
  const whitelist = link.model_names ?? [];
  const whitelisted = whitelist.length > 0;

  return (
    <Box
      p="2"
      style={{ border: "1px solid var(--gray-5)", borderRadius: 6, background: "var(--color-panel-solid)" }}
    >
      <Flex align="center" gap="2" wrap="wrap">
        <Box style={{ flex: "1 1 200px", minWidth: 0 }}>
          <Text size="2" weight="medium" as="div">{link.account_name || (account ? accountLabel(account) : "") || link.account_id.slice(0, 8)}</Text>
          <Text size="1" color="gray">
            {(account?.native_protocols ?? link.protocols ?? []).map(protocolLabel).join(" / ") || "—"}
            {link.status && link.status !== "active" ? " · 已停用" : ""}
            {link.cooldown_active ? " · 冷却中" : ""}
            {link.enabled === false ? " · 本分组内已停用" : ""}
          </Text>
        </Box>
        <Box style={{ flex: "0 0 90px" }}>
          <Text size="1" color="gray" as="div" mb="1">priority</Text>
          <ClearableTextField type="number" min="0" value={priority} onChange={(e) => setPriority(e.target.value)} />
        </Box>
        <Box style={{ flex: "0 0 90px" }}>
          <Text size="1" color="gray" as="div" mb="1">weight</Text>
          <ClearableTextField type="number" min="0" value={weight} onChange={(e) => setWeight(e.target.value)} />
        </Box>
        <Flex gap="2" align="center">
          <label style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }} title="停用后该成员不参与本分组路由">
            <Switch
              size="1"
              checked={link.enabled !== false}
              onCheckedChange={(v) => onSave({ groupId, accountId: link.account_id, patch: { enabled: v } })}
            />
          </label>
          <Button
            size="1"
            variant="soft"
            color="green"
            disabled={invalid || !dirty}
            loading={saving}
            onClick={() => onSave({ groupId, accountId: link.account_id, patch: { priority: parsedPriority, weight: parsedWeight } })}
          >
            保存
          </Button>
          <Button size="1" color="red" variant="soft" loading={removing} onClick={() => onRemove({ groupId, accountId: link.account_id })}>
            移除
          </Button>
        </Flex>
        {invalid && <Text size="1" color="red">不能小于 0（0 表示继承账号自身值）</Text>}
      </Flex>
      <MemberModelWhitelist
        groupId={groupId}
        link={link}
        open={modelsOpen}
        onToggle={() => setModelsOpen((v) => !v)}
        whitelisted={whitelisted}
        whitelist={whitelist}
        saving={saving}
        onSave={onSave}
      />
    </Box>
  );
}

// MemberModelWhitelist edits which of the account's configured models serve
// this group. Default is all of them; checking a subset writes the whitelist.
function MemberModelWhitelist({
  groupId, link, open, onToggle, whitelisted, whitelist, saving, onSave,
}: {
  groupId: string;
  link: GroupAccount;
  open: boolean;
  onToggle: () => void;
  whitelisted: boolean;
  whitelist: string[];
  saving: boolean;
  onSave: (v: { groupId: string; accountId: string; patch: MemberPatch }) => void;
}) {
  const { data: am = [], isLoading } = useQuery({
    queryKey: ["admin", "account-models", link.account_id],
    queryFn: () => adminApi.accountModels(link.account_id),
  });
  const configured = useMemo(() => (am as AccountModel[]).filter((m) => m.enabled !== false), [am]);
  const whitelistKey = whitelist.join("|");
  const [checked, setChecked] = useState<Set<string>>(new Set());

  useEffect(() => {
    if (open) {
      setChecked(new Set(whitelisted ? whitelist : configured.map((m) => m.model_name)));
    }
    // Rebuild the draft whenever the panel opens, the stored list changes, or
    // the account's configured models arrive/refresh.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, whitelisted, whitelistKey, configured, link.account_id]);

  const toggle = (name: string) => {
    setChecked((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name); else next.add(name);
      return next;
    });
  };

  const allChecked = configured.length > 0 && configured.every((m) => checked.has(m.model_name));
  const saveWhitelist = () => {
    // Covering every configured model is stored as "all" (empty), so models
    // added to the account later join the group automatically.
    const names = allChecked ? [] : [...checked];
    onSave({ groupId, accountId: link.account_id, patch: { model_names: names } });
    onToggle();
  };

  return (
    <Box mt="2">
      <Flex align="center" gap="2" wrap="wrap">
        <Badge variant="soft" color={whitelisted ? "amber" : "gray"}>
          {whitelisted ? `模型白名单 ${whitelist.length} 个` : `全部 ${configured.length} 个模型`}
        </Badge>
        {whitelisted && whitelist.slice(0, 4).map((n) => (
          <Badge key={n} variant="outline" color="gray">{n}</Badge>
        ))}
        {whitelisted && whitelist.length > 4 && <Text size="1" color="gray">+{whitelist.length - 4}</Text>}
        <Button size="1" variant="ghost" onClick={onToggle}>
          {open ? "收起" : whitelisted ? "编辑白名单" : "设置白名单"}
        </Button>
      </Flex>
      {open && (
        <Box mt="2" p="2" style={{ borderTop: "1px dashed var(--gray-5)" }}>
          {isLoading && <Text size="1" color="gray">加载模型…</Text>}
          {!isLoading && configured.length === 0 && (
            <Text size="1" color="gray">该账号没有配置可用模型</Text>
          )}
          {configured.length > 0 && (
            <>
              <Flex gap="3" wrap="wrap">
                {configured.map((m) => (
                  <label key={m.model_name} style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}>
                    <Checkbox checked={checked.has(m.model_name)} onCheckedChange={() => toggle(m.model_name)} />
                    <Text size="1">{m.model_name}{m.upstream_model && m.upstream_model !== m.model_name ? ` → ${m.upstream_model}` : ""}</Text>
                  </label>
                ))}
              </Flex>
              <Flex gap="2" mt="2" align="center">
                <Button size="1" variant="soft" loading={saving} disabled={checked.size === 0} onClick={saveWhitelist}>
                  {allChecked ? "保存（全部模型）" : `保存白名单（${checked.size} 个）`}
                </Button>
                {whitelisted && (
                  <Button size="1" variant="ghost" color="gray" loading={saving}
                    onClick={() => { onSave({ groupId, accountId: link.account_id, patch: { model_names: [] } }); onToggle(); }}>
                    恢复全部
                  </Button>
                )}
                {checked.size === 0 && <Text size="1" color="gray">一个都不选等于不提供模型；要停用该成员请直接关开关或移除。</Text>}
              </Flex>
            </>
          )}
        </Box>
      )}
    </Box>
  );
}

type ProbeMutationVars = {
  groupId: string;
  modelId: string;
  patch: { probe_enabled?: boolean; probe_interval_seconds?: number; context_window?: number };
};

// AccountModelPicker lists the models served by the group's bound accounts and
// toggles the group's model membership (the link is by model id).
function AccountModelPicker({
  group, bound, models, attach, detach, probe,
}: {
  group: ModelGroup;
  bound: GroupAccount[];
  models: Model[];
  attach: { mutate: (v: { groupId: string; modelId: string }) => void; isPending: boolean; variables?: { groupId: string; modelId: string } | null };
  detach: { mutate: (v: { groupId: string; modelId: string }) => void; isPending: boolean; variables?: { groupId: string; modelId: string } | null };
  probe: { mutate: (v: ProbeMutationVars) => void; isPending: boolean; variables?: ProbeMutationVars | null };
}) {
  const [accountId, setAccountId] = useState("");
  const modelByName = new Map(models.map((m) => [m.name, m]));
  const linkByModel = new Map((group.model_links ?? []).map((link) => [link.model_id, link]));

  useEffect(() => {
    if (accountId && !bound.some((b) => b.account_id === accountId)) setAccountId("");
  }, [accountId, bound]);

  const { data: accountModels = [], isLoading } = useQuery({
    queryKey: ["admin", "account-models", accountId],
    queryFn: () => adminApi.accountModels(accountId),
    enabled: !!accountId,
  });

  const pickable = (accountModels as AccountModel[]).filter((m) => modelByName.has(m.model_name));

  if (bound.length === 0) {
    return <Text size="2" color="gray">该分组还没有绑定任何账号。请先在上面绑定账号，再回来添加模型。</Text>;
  }

  return (
    <Flex direction="column" gap="3">
      <Flex direction="column" gap="2">
        {bound.map((b) => (
          <AccountAddedModels key={b.account_id} link={b} group={group} models={models} />
        ))}
      </Flex>

      <Select.Root value={accountId || "_none"} onValueChange={(v) => setAccountId(v === "_none" ? "" : v)}>
        <Select.Trigger placeholder="选择账号（仅显示已绑定）" />
        <Select.Content>
          <Select.Item value="_none">选择账号</Select.Item>
          {bound.map((b) => (
            <Select.Item key={b.account_id} value={b.account_id}>{b.account_name || b.account_id.slice(0, 8)}</Select.Item>
          ))}
        </Select.Content>
      </Select.Root>
      {!accountId && <Text size="2" color="gray">选择账号后，勾选该账号要加入本分组的模型。</Text>}
      {accountId && isLoading && <Text size="2" color="gray">加载模型…</Text>}
      {accountId && !isLoading && pickable.length === 0 && (
        <Text size="2" color="gray">该账号没有可加入的模型（未配置账号模型，或模型名在本系统不存在）。</Text>
      )}
      {accountId && !isLoading && pickable.length > 0 && (
        <Flex gap="2" wrap="wrap">
          {pickable.map((p) => {
            const m = modelByName.get(p.model_name)!;
            const done = (group.model_ids ?? []).includes(m.id);
            const pending =
              (attach.isPending && attach.variables?.groupId === group.id && attach.variables?.modelId === m.id) ||
              (detach.isPending && detach.variables?.groupId === group.id && detach.variables?.modelId === m.id);
            const toggle = (
              <Button
                size="1"
                variant={done ? "soft" : "outline"}
                color={done ? "green" : undefined}
                loading={pending}
                onClick={() => done
                  ? detach.mutate({ groupId: group.id, modelId: m.id })
                  : attach.mutate({ groupId: group.id, modelId: m.id })
                }
              >
                {done ? "✓" : "+"} {m.name}
              </Button>
            );
            if (!done) return <Box key={m.id}>{toggle}</Box>;
            return (
              <Flex
                key={m.id}
                align="center"
                gap="2"
                px="2"
                py="1"
                style={{ border: "1px solid var(--gray-5)", borderRadius: 6, background: "var(--color-panel-solid)" }}
              >
                {toggle}
                <ModelProbeControls
                  groupID={group.id}
                  model={m}
                  link={linkByModel.get(m.id)}
                  saving={probe.isPending && probe.variables?.groupId === group.id && probe.variables?.modelId === m.id}
                  onSave={probe.mutate}
                />
              </Flex>
            );
          })}
        </Flex>
      )}
    </Flex>
  );
}

// Read-only per-account row: the account name plus the models that are both
// served by this account and already linked to the group.
function AccountAddedModels({
  link, group, models,
}: {
  link: GroupAccount;
  group: ModelGroup;
  models: Model[];
}) {
  const { data: am = [] } = useQuery({
    queryKey: ["admin", "account-models", link.account_id],
    queryFn: () => adminApi.accountModels(link.account_id),
  });
  const modelByName = new Map(models.map((m) => [m.name, m]));
  const added = (am as AccountModel[]).flatMap((p) => {
    const m = modelByName.get(p.model_name);
    return m && (group.model_ids ?? []).includes(m.id) ? [m] : [];
  });
  return (
    <Flex direction="column" gap="1">
      <Text size="2" weight="medium" as="div">
        {link.account_name || link.account_id.slice(0, 8)}
        <Text size="1" color="gray"> · {added.length} 个模型已添加</Text>
      </Text>
      {added.length === 0 ? (
        <Text size="1" color="gray">该账号尚未添加模型到本分组</Text>
      ) : (
        <Flex gap="1" wrap="wrap">
          {added.map((m) => (
            <Badge key={m.id} variant="soft" color="green">✓ {m.name}</Badge>
          ))}
        </Flex>
      )}
    </Flex>
  );
}

// Mirrors the schema default (probe_interval_seconds on ModelGroupModel). Only
// used to prefill the box for a link the list has not carried settings for; the
// server stays the source of truth.
const DEFAULT_PROBE_INTERVAL = 900;

/* Per-link probe settings, sitting beside the model that owns them.
   The switch writes immediately — it is one bit and an accidental flip is
   undone by flipping back — while the interval waits for blur or Enter, so
   typing "1200" does not fire a PATCH at "1", "12" and "120" on the way. */
function ModelProbeControls({
  groupID, model, link, saving, onSave,
}: {
  groupID: string;
  model: Model;
  link?: ModelGroupModelLink;
  saving: boolean;
  onSave: (v: ProbeMutationVars) => void;
}) {
  const enabled = link?.probe_enabled ?? false;
  const interval = link?.probe_interval_seconds ?? DEFAULT_PROBE_INTERVAL;
  const [draft, setDraft] = useState(String(interval));
  const linkWindow = link?.context_window ?? 0;
  const [windowDraft, setWindowDraft] = useState(linkWindow ? String(linkWindow) : "");

  useEffect(() => {
    setDraft(String(interval));
  }, [interval, model.id]);
  useEffect(() => {
    setWindowDraft(linkWindow ? String(linkWindow) : "");
  }, [linkWindow, model.id]);

  // The model's linkWindow through this group; empty inherits the model default.
  const commitWindow = () => {
    const next = parseTokens(windowDraft);
    if (Number.isNaN(next) || next === linkWindow) {
      setWindowDraft(linkWindow ? String(linkWindow) : "");
      return;
    }
    onSave({ groupId: groupID, modelId: model.id, patch: { context_window: next } });
  };

  const parsed = parseInt(draft, 10);
  const valid = Number.isFinite(parsed) && parsed > 0;
  const commit = () => {
    if (!valid || parsed === interval) {
      setDraft(String(interval));
      return;
    }
    onSave({ groupId: groupID, modelId: model.id, patch: { probe_interval_seconds: parsed } });
  };

  return (
    <>
      <label
        style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}
        title={enabled
          ? "关闭探针：不再定期发送合成请求，状态页只依据真实流量判定"
          : "开启探针：定期发送合成请求，让低流量模型在状态页上也有判定依据（会产生上游费用）"}
      >
        <Switch
          size="1"
          checked={enabled}
          disabled={saving}
          onCheckedChange={(v) => onSave({ groupId: groupID, modelId: model.id, patch: { probe_enabled: v } })}
        />
        <Text size="1" color="gray">探针</Text>
      </label>
      <Box style={{ width: 92 }}>
        <ClearableTextField
          size="1"
          type="number"
          min="1"
          clearable={false}
          disabled={saving}
          title="探针间隔（秒）"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={commit}
          onKeyDown={(e) => {
            if (e.key === "Enter") e.currentTarget.blur();
            if (e.key === "Escape") setDraft(String(interval));
          }}
        />
      </Box>
      <Text size="1" color="gray">秒</Text>
      <Box style={{ width: 88 }}>
        <ClearableTextField
          size="1"
          clearable={false}
          disabled={saving}
          title="本分组下该模型的上下文窗口（如 272k、1m）；留空继承模型默认"
          placeholder={model.context_window ? fmtTokens(model.context_window) : "窗口"}
          value={windowDraft}
          onChange={(e) => setWindowDraft(e.target.value)}
          onBlur={commitWindow}
          onKeyDown={(e) => {
            if (e.key === "Enter") e.currentTarget.blur();
            if (e.key === "Escape") setWindowDraft(linkWindow ? String(linkWindow) : "");
          }}
        />
      </Box>
    </>
  );
}

// "272000", "272k", "1m"; "" = 0 (inherit); NaN on garbage.
function parseTokens(raw: string): number {
  const t = raw.trim().toLowerCase();
  if (t === "") return 0;
  const m = /^(\d+(?:\.\d+)?)([km]?)$/.exec(t);
  if (!m) return NaN;
  return Math.round(parseFloat(m[1]) * (m[2] === "k" ? 1_000 : m[2] === "m" ? 1_000_000 : 1));
}

const fmtTokens = (n: number) =>
  n >= 1_000_000 ? `${+(n / 1_000_000).toFixed(2)}M` : n >= 1000 ? `${Math.round(n / 1000)}K` : String(n);

// Six fixed slots mirroring the public radar chart: a dimension only exists
// when both its label and its score are filled; blank rows are unset and the
// chart simply skips them.
const PROFILE_SLOTS = 6;

function dimsFromGroup(group: ModelGroup): { name: string; value: string }[] {
  const rows = (group.profile ?? []).map((d) => ({ name: d.name, value: String(d.value) }));
  while (rows.length < PROFILE_SLOTS) rows.push({ name: "", value: "" });
  return rows.slice(0, PROFILE_SLOTS);
}

function serializeDims(rows: { name: string; value: string }[]): GroupProfileDim[] {
  const out: GroupProfileDim[] = [];
  for (const r of rows) {
    const name = r.name.trim();
    if (!name) continue;
    const v = parseFloat(r.value);
    if (Number.isNaN(v)) continue;
    out.push({ name, value: Math.max(0, Math.min(100, v)) });
  }
  return out;
}

function GroupDetailsEditor({
  group, onSave, saving,
}: {
  group: ModelGroup;
  onSave: (patch: { name: string; billing_source: string; rate_multiplier: number; description: string; profile: GroupProfileDim[] }) => void;
  saving: boolean;
}) {
  const [name, setName] = useState(group.name);
  const [billingSource, setBillingSource] = useState(group.billing_source);
  const [rateMultiplier, setRateMultiplier] = useState(String(group.rate_multiplier));
  const [description, setDescription] = useState(group.description ?? "");
  const [dims, setDims] = useState(() => dimsFromGroup(group));

  useEffect(() => {
    setName(group.name);
    setBillingSource(group.billing_source);
    setRateMultiplier(String(group.rate_multiplier));
    setDescription(group.description ?? "");
    setDims(dimsFromGroup(group));
  }, [group]);

  const profile = serializeDims(dims);
  const savedProfile = serializeDims(dimsFromGroup(group));
  const dirty =
    name !== group.name ||
    billingSource !== group.billing_source ||
    rateMultiplier !== String(group.rate_multiplier) ||
    description !== (group.description ?? "") ||
    JSON.stringify(profile) !== JSON.stringify(savedProfile);
  const parsedRate = parseFloat(rateMultiplier);
  // A named row needs a real score — a label with a blank/non-numeric value
  // would otherwise be silently dropped on save.
  const dimIncomplete = dims.some((d) => d.name.trim() && (d.value.trim() === "" || Number.isNaN(parseFloat(d.value))));
  const invalid = !name.trim() || Number.isNaN(parsedRate) || parsedRate <= 0 || dimIncomplete;

  const setDim = (i: number, patch: Partial<{ name: string; value: string }>) =>
    setDims((rows) => rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  return (
    <Flex direction="column" gap="2">
      <Flex gap="2" wrap="wrap">
        <Box style={{ flex: "1 1 220px" }}>
          <ClearableTextField
            placeholder="分组名称"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </Box>
        <Select.Root value={billingSource} onValueChange={(v) => setBillingSource(v as ModelGroup["billing_source"])}>
          <Select.Trigger />
          <Select.Content>
            <Select.Item value="balance_only">balance_only</Select.Item>
            <Select.Item value="plan_only">plan_only</Select.Item>
          </Select.Content>
        </Select.Root>
        <Box style={{ flex: "0 1 160px" }}>
          <ClearableTextField
            type="number"
            placeholder="倍率"
            value={rateMultiplier}
            onChange={(e) => setRateMultiplier(e.target.value)}
          />
        </Box>
      </Flex>
      <ClearableTextField
        placeholder="描述（可选，用户模型页的分组介绍卡片会显示）"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
      />
      <Box>
        <Text size="1" color="gray" as="p" mb="1">
          六维图（可选）：维度名 + 数值 0–100，留空的维度不会在用户模型页显示
        </Text>
        <Flex direction="column" gap="1">
          {dims.map((d, i) => (
            <Flex key={i} gap="2" align="center">
              <Box style={{ flex: "1 1 160px" }}>
                <ClearableTextField
                  size="1"
                  placeholder={`维度 ${i + 1} 名称`}
                  value={d.name}
                  onChange={(e) => setDim(i, { name: e.target.value })}
                />
              </Box>
              <Box style={{ flex: "0 1 110px" }}>
                <ClearableTextField
                  size="1"
                  type="number"
                  placeholder="0–100"
                  value={d.value}
                  onChange={(e) => setDim(i, { value: e.target.value })}
                />
              </Box>
            </Flex>
          ))}
        </Flex>
        {dimIncomplete && (
          <Text size="1" color="red" as="p" mt="1">填了名称的维度需要补数值。</Text>
        )}
      </Box>
      <Flex gap="2" align="center">
        <Button
          size="1"
          disabled={!dirty || invalid}
          loading={saving}
          onClick={() => onSave({
            name: name.trim(),
            billing_source: billingSource,
            rate_multiplier: parsedRate,
            description,
            profile,
          })}
        >
          保存基础信息
        </Button>
        {dirty && (
          <Button
            size="1"
            variant="soft"
            color="gray"
            onClick={() => {
              setName(group.name);
              setBillingSource(group.billing_source);
              setRateMultiplier(String(group.rate_multiplier));
              setDescription(group.description ?? "");
              setDims(dimsFromGroup(group));
            }}
          >
            撤销
          </Button>
        )}
        {invalid && !dimIncomplete && <Text size="1" color="red">名称不能为空，倍率必须大于 0。</Text>}
      </Flex>
    </Flex>
  );
}

function ExclusiveAccessEditor({
  group, userGroups, onSave, saving,
}: {
  group: ModelGroup;
  userGroups: { id: string; name: string }[];
  onSave: (v: { user_ids: string[]; user_group_ids: string[] }) => void;
  saving: boolean;
}) {
  const [userGroupIds, setUserGroupIds] = useState(group.allowed_user_group_ids ?? []);
  const [userIds, setUserIds] = useState(group.granted_user_ids ?? []);
  useEffect(() => {
    setUserGroupIds(group.allowed_user_group_ids ?? []);
    setUserIds(group.granted_user_ids ?? []);
  }, [group.id, group.allowed_user_group_ids, group.granted_user_ids]);

  const dirty =
    JSON.stringify([...userGroupIds].sort()) !== JSON.stringify([...(group.allowed_user_group_ids ?? [])].sort())
    || JSON.stringify([...userIds].sort()) !== JSON.stringify([...(group.granted_user_ids ?? [])].sort());

  return (
    <Flex direction="column" gap="3">
      <Text size="1" color="gray">授权用户组（或关系；未分组用户不会继承默认组）</Text>
      {userGroups.length === 0 ? (
        <Text size="2" color="gray">尚未创建用户组。</Text>
      ) : (
        <Flex gap="3" wrap="wrap">
          {userGroups.map((ug) => (
            <label key={ug.id} style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}>
              <Checkbox
                checked={userGroupIds.includes(ug.id)}
                onCheckedChange={() => setUserGroupIds((s) => s.includes(ug.id) ? s.filter((x) => x !== ug.id) : [...s, ug.id])}
              />
              <Text size="2">{ug.name}</Text>
            </label>
          ))}
        </Flex>
      )}
      <UserGrantPicker userIds={userIds} onChange={setUserIds} />
      {userIds.length === 0 && userGroupIds.length === 0 && (
        <Text size="1" color="orange">还没有授权任何人，这个专属分组当前锁定。</Text>
      )}
      <Flex gap="2">
        <Button size="1" disabled={!dirty} loading={saving} onClick={() => onSave({ user_ids: userIds, user_group_ids: userGroupIds })}>
          保存授权
        </Button>
        {dirty && (
          <Button size="1" variant="soft" color="gray" onClick={() => {
            setUserGroupIds(group.allowed_user_group_ids ?? []);
            setUserIds(group.granted_user_ids ?? []);
          }}>撤销</Button>
        )}
      </Flex>
    </Flex>
  );
}

function UserGrantPicker({ userIds, onChange }: { userIds: string[]; onChange: (ids: string[]) => void }) {
  const [q, setQ] = useState("");
  const { data } = useQuery({
    queryKey: ["admin", "users", "grant-search", q],
    queryFn: () => adminApi.users({ q: q.trim() || undefined, limit: 20, offset: 0 }),
    enabled: q.trim().length >= 2,
  });
  const hits = data?.users ?? [];
  return (
    <Box>
      <Text size="1" color="gray" as="p" mb="1">授权用户（搜邮箱 / 昵称，至少 2 个字符）</Text>
      <ClearableTextField placeholder="搜索用户" value={q} onChange={(e) => setQ(e.target.value)} />
      {q.trim().length >= 2 && (
        <Flex direction="column" gap="1" mt="2">
          {hits.length === 0 && <Text size="1" color="gray">没有匹配的用户</Text>}
          {hits.map((u: { id: string; email: string; nickname?: string }) => (
            <Flex key={u.id} align="center" justify="between" gap="2">
              <Text size="2">{u.nickname ? `${u.nickname} · ${u.email}` : u.email}</Text>
              <Button size="1" variant="soft" disabled={userIds.includes(u.id)} onClick={() => {
                if (!userIds.includes(u.id)) onChange([...userIds, u.id]);
              }}>加入</Button>
            </Flex>
          ))}
        </Flex>
      )}
      {userIds.length > 0 && (
        <Flex gap="2" wrap="wrap" mt="2">
          {userIds.map((id) => (
            <Badge key={id} variant="soft" color="iris" style={{ cursor: "pointer" }} onClick={() => onChange(userIds.filter((x) => x !== id))}>
              {id.slice(0, 8)}… ×
            </Badge>
          ))}
        </Flex>
      )}
    </Box>
  );
}
