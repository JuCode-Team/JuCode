import { useState } from "react";
import { Badge, Box, Button, Dialog, Flex, Select, Table, Text, TextArea } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Building2, Plus } from "lucide-react";
import { adminApi, type Organization, type OrganizationMember } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { sumAmounts } from "../../lib/currency";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { AdminMetricCard, AdminMetricGrid, AdminPage, AdminSection } from "../../components/AdminPage";
import { ClearableTextField } from "../../components/ClearableTextField";
import { PointAmount } from "../../components/PointAmount";

type OrgRole = OrganizationMember["role"];
type DialogState = { mode: "create" } | { mode: "edit"; org: Organization } | null;
type UserOption = { id: string; email?: string; username?: string | null; nickname?: string | null };

const blankOrg = () => ({ name: "", owner_user_id: "", monthly_spend_limit: "0" });

function roleLabel(role: OrgRole) {
  return role === "owner" ? "Owner" : role === "admin" ? "管理员" : "成员";
}

export default function Organizations() {
  const qc = useQueryClient();
  const paged = usePagedQuery<Organization>({
    queryKey: ["admin", "organizations"],
    queryFn: (params) => adminApi.organizationsPaged(params),
    pageSize: 50,
  });
  const [dialog, setDialog] = useState<DialogState>(null);
  const [form, setForm] = useState(blankOrg());
  const [selected, setSelected] = useState<Organization | null>(null);

  const detail = useQuery({
    queryKey: ["admin", "organizations", selected?.id],
    queryFn: () => adminApi.organizationDetail(selected!.id),
    enabled: !!selected,
  });

  const invalidate = (id?: string) => {
    qc.invalidateQueries({ queryKey: ["admin", "organizations"] });
    if (id) qc.invalidateQueries({ queryKey: ["admin", "organizations", id] });
  };

  const create = useMutation({
    mutationFn: () => adminApi.createOrganization({
      name: form.name.trim(),
      owner_user_id: form.owner_user_id.trim(),
      monthly_spend_limit: form.monthly_spend_limit.trim() || "0",
    }),
    onSuccess: (org) => { setDialog(null); invalidate(org.id); setSelected(org); },
  });
  const update = useMutation({
    mutationFn: (org: Organization) => adminApi.updateOrganization(org.id, {
      name: form.name.trim(),
      owner_user_id: form.owner_user_id.trim(),
      monthly_spend_limit: form.monthly_spend_limit.trim() || "0",
    }),
    onSuccess: (org) => { setDialog(null); invalidate(org.id); setSelected(org); },
  });

  const openCreate = () => { setForm(blankOrg()); setDialog({ mode: "create" }); };
  const openEdit = (org: Organization) => {
    setForm({
      name: org.name,
      owner_user_id: org.owner_user_id,
      monthly_spend_limit: org.monthly_spend_limit,
    });
    setDialog({ mode: "edit", org });
  };

  const columns: Column<Organization>[] = [
    {
      header: "组织",
      cell: (org) => (
        <Flex align="center" gap="3" style={{ minWidth: 0 }}>
          <Box
            style={{
              width: 34, height: 34, borderRadius: 9, flex: "0 0 auto",
              display: "flex", alignItems: "center", justifyContent: "center",
              background: "color-mix(in oklab, var(--c-primary) 16%, transparent)",
              color: "var(--c-primary-bright)",
            }}
          >
            <Building2 size={17} strokeWidth={1.9} />
          </Box>
          <Box style={{ minWidth: 0 }}>
            <Text weight="medium" as="div" className="jc-truncate">{org.name}</Text>
            <Text size="1" color="gray" as="div" className="jc-mono">{org.id}</Text>
          </Box>
        </Flex>
      ),
      wrap: true,
    },
    {
      header: "余额",
      width: "150px",
      align: "right",
      cell: (org) => <Text className="jc-num"><PointAmount amount={org.balance} decimals="preserve" iconSize={12} /></Text>,
    },
    {
      header: "月限额",
      width: "150px",
      align: "right",
      cell: (org) => Number(org.monthly_spend_limit) > 0
        ? <Text className="jc-num"><PointAmount amount={org.monthly_spend_limit} decimals="preserve" iconSize={12} /></Text>
        : <Text color="gray">不限</Text>,
    },
    {
      header: "Owner",
      width: "220px",
      cell: (org) => <Text size="2" color="gray">{org.owner_user_id}</Text>,
    },
    {
      header: "操作",
      width: "180px",
      align: "right",
      cell: (org) => (
        <Flex gap="2" justify="end">
          <Button size="1" variant="soft" onClick={() => setSelected(org)}>管理</Button>
          <Button size="1" variant="soft" color="gray" onClick={() => openEdit(org)}>编辑</Button>
        </Flex>
      ),
    },
  ];

  return (
    <AdminPage
      title="组织"
      description="创建组织、指定 owner、管理成员额度和组织余额。组织 API Key 可选择使用组织余额。"
      actions={<Button onClick={openCreate}><Plus size={14} />新建组织</Button>}
    >
      <AdminMetricGrid>
        <AdminMetricCard label="组织数" value={paged.total.toLocaleString()} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="当前页余额" value={<PointAmount amount={sumAmounts(paged.items.map((o) => o.balance))} decimals="preserve" iconSize={15} />} detail="仅当前页" tone="green" />
        <AdminMetricCard label="限额组织" value={paged.items.filter((o) => Number(o.monthly_spend_limit) > 0).length} detail="当前页" tone="amber" />
        <AdminMetricCard label="不限额组织" value={paged.items.filter((o) => Number(o.monthly_spend_limit) <= 0).length} detail="当前页" />
      </AdminMetricGrid>

      <DataTable<Organization>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(org) => org.id}
        searchPlaceholder="搜索组织..."
        emptyText="尚未创建组织"
      />

      <OrgDialog
        dialog={dialog}
        form={form}
        setForm={setForm}
        onClose={() => setDialog(null)}
        onSubmit={() => dialog?.mode === "edit" ? update.mutate(dialog.org) : create.mutate()}
        saving={create.isPending || update.isPending}
        error={(create.isError || update.isError) ? errMsg(create.error || update.error) : ""}
      />

      {selected && (
        <OrgManageDialog
          org={detail.data ?? selected}
          loading={detail.isLoading}
          onClose={() => setSelected(null)}
          onChanged={() => invalidate(selected.id)}
        />
      )}
    </AdminPage>
  );
}

function OrgDialog({
  dialog, form, setForm, onClose, onSubmit, saving, error,
}: {
  dialog: DialogState;
  form: ReturnType<typeof blankOrg>;
  setForm: (v: ReturnType<typeof blankOrg>) => void;
  onClose: () => void;
  onSubmit: () => void;
  saving: boolean;
  error: string;
}) {
  return (
    <Dialog.Root open={dialog !== null} onOpenChange={(o) => { if (!o) onClose(); }}>
      <Dialog.Content style={{ maxWidth: 520 }}>
        <Dialog.Title>{dialog?.mode === "edit" ? "编辑组织" : "新建组织"}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">组织名称</Text>
            <ClearableTextField value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </Box>
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">Owner</Text>
            <UserSearchSelect
              selectedUserID={form.owner_user_id}
              onSelect={(id) => setForm({ ...form, owner_user_id: id })}
            />
          </Box>
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">组织月限额</Text>
            <ClearableTextField type="number" min="0" step="0.01" value={form.monthly_spend_limit}
              onChange={(e) => setForm({ ...form, monthly_spend_limit: e.target.value })} />
            <Text size="1" color="gray" as="div" mt="1">0 表示不限额</Text>
          </Box>
          {error && <Text size="2" color="red">{error}</Text>}
          <Flex gap="2" justify="end">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button disabled={!form.name.trim() || !form.owner_user_id.trim()} loading={saving} onClick={onSubmit}>
              {dialog?.mode === "edit" ? "保存" : "创建"}
            </Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function OrgManageDialog({
  org, loading, onClose, onChanged,
}: {
  org: Organization;
  loading: boolean;
  onClose: () => void;
  onChanged: () => void;
}) {
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const [userId, setUserId] = useState("");
  const [role, setRole] = useState<OrgRole>("member");
  const [limit, setLimit] = useState("0");
  const [totalLimit, setTotalLimit] = useState("0");
  const topup = useMutation({
    mutationFn: () => adminApi.topupOrganization(org.id, { amount, note }),
    onSuccess: () => { setAmount(""); setNote(""); onChanged(); },
  });
  const upsert = useMutation({
    mutationFn: () => adminApi.upsertOrganizationMember(org.id, userId.trim(), {
      role,
      monthly_spend_limit: limit.trim() || "0",
      total_spend_limit: totalLimit.trim() || "0",
    }),
    onSuccess: () => { setUserId(""); setLimit("0"); setTotalLimit("0"); setRole("member"); onChanged(); },
  });
  const remove = useMutation({
    mutationFn: (id: string) => adminApi.removeOrganizationMember(org.id, id),
    onSuccess: onChanged,
  });
  const error = errMsg(topup.error || upsert.error || remove.error);

  return (
    <Dialog.Root open onOpenChange={(o) => { if (!o) onClose(); }}>
      <Dialog.Content style={{ maxWidth: 840 }}>
        <Dialog.Title>
          <Flex align="center" gap="2">
            <Building2 size={18} strokeWidth={1.9} style={{ color: "var(--c-primary-bright)" }} />
            {org.name}
            {loading && <Badge variant="soft" color="gray" ml="1">加载中</Badge>}
          </Flex>
        </Dialog.Title>
        <Flex gap="2" wrap="wrap" mb="4" mt="2">
          <Box
            style={{
              flex: "1 1 180px", minWidth: 0, padding: "10px 14px", borderRadius: 10,
              border: "1px solid var(--c-outline-variant)", background: "var(--c-card-faint-bg)",
            }}
          >
            <Text size="1" color="gray" as="div">组织余额</Text>
            <Text size="4" weight="bold" as="div" mt="1" className="jc-num" style={{ color: "var(--green-11)" }}>
              <PointAmount amount={org.balance} decimals="preserve" iconSize={15} />
            </Text>
          </Box>
          <Box
            style={{
              flex: "1 1 180px", minWidth: 0, padding: "10px 14px", borderRadius: 10,
              border: "1px solid var(--c-outline-variant)", background: "var(--c-card-faint-bg)",
            }}
          >
            <Text size="1" color="gray" as="div">组织月限额</Text>
            <Text size="4" weight="bold" as="div" mt="1" className="jc-num">
              {Number(org.monthly_spend_limit) > 0 ? <PointAmount amount={org.monthly_spend_limit} decimals="preserve" iconSize={15} /> : "不限"}
            </Text>
          </Box>
        </Flex>
        <Flex direction="column" gap="4">
          <AdminSection title="充值" description="为组织余额充值，组织额度 API Key 会从这里扣费。">
            <Flex gap="2" wrap="wrap" align="end">
              <Box style={{ width: 180 }}>
                <Text size="1" color="gray" as="div" mb="1">金额</Text>
                <ClearableTextField type="number" min="0" step="0.01" value={amount} onChange={(e) => setAmount(e.target.value)} />
              </Box>
              <Box style={{ flex: 1, minWidth: 220 }}>
                <Text size="1" color="gray" as="div" mb="1">备注</Text>
                <TextArea value={note} onChange={(e) => setNote(e.target.value)} rows={1} />
              </Box>
              <Button disabled={!amount.trim()} loading={topup.isPending} onClick={() => topup.mutate()}>充值</Button>
            </Flex>
          </AdminSection>

          <AdminSection title="成员" description="添加成员并设置角色与限额。月限额每个自然月重置，累计限额不重置。0 表示不限。">
            <Flex gap="2" wrap="wrap" align="end" mb="3">
              <Box style={{ flex: "1 1 280px" }}>
                <Text size="1" color="gray" as="div" mb="1">用户</Text>
                <UserSearchSelect selectedUserID={userId} onSelect={setUserId} />
              </Box>
              <Box>
                <Text size="1" color="gray" as="div" mb="1">角色</Text>
                <Select.Root value={role} onValueChange={(v) => setRole(v as OrgRole)}>
                  <Select.Trigger />
                  <Select.Content>
                    <Select.Item value="member">成员</Select.Item>
                    <Select.Item value="admin">管理员</Select.Item>
                    <Select.Item value="owner">Owner</Select.Item>
                  </Select.Content>
                </Select.Root>
              </Box>
              <Box style={{ width: 160 }}>
                <Text size="1" color="gray" as="div" mb="1">成员月限额</Text>
                <ClearableTextField type="number" min="0" step="0.01" value={limit} onChange={(e) => setLimit(e.target.value)} />
              </Box>
              <Box style={{ width: 160 }}>
                <Text size="1" color="gray" as="div" mb="1">成员累计限额</Text>
                <ClearableTextField type="number" min="0" step="0.01" value={totalLimit} onChange={(e) => setTotalLimit(e.target.value)} />
              </Box>
              <Button disabled={!userId.trim()} loading={upsert.isPending} onClick={() => upsert.mutate()}>保存成员</Button>
            </Flex>

            <div className="jc-local-table-scroll">
            <Table.Root variant="surface" size="1">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>用户</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>角色</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">成员月限额</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">成员累计限额</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell align="right">操作</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {(org.members ?? []).map((m) => (
                  <Table.Row key={m.user_id}>
                    <Table.Cell>
                      <Text weight="medium">{m.email || m.nickname || m.user_id}</Text>
                      <Text size="1" color="gray" as="div">{m.user_id}</Text>
                    </Table.Cell>
                    <Table.Cell><Badge variant="soft">{roleLabel(m.role)}</Badge></Table.Cell>
                    <Table.Cell align="right" className="jc-num">
                      {Number(m.monthly_spend_limit) > 0 ? <PointAmount amount={m.monthly_spend_limit} decimals="preserve" iconSize={12} /> : "不限"}
                    </Table.Cell>
                    <Table.Cell align="right" className="jc-num">
                      {Number(m.total_spend_limit) > 0 ? <PointAmount amount={m.total_spend_limit} decimals="preserve" iconSize={12} /> : "不限"}
                    </Table.Cell>
                    <Table.Cell align="right">
                      <Button size="1" variant="soft" color="red" disabled={m.role === "owner"}
                        onClick={() => confirm("移除该组织成员？") && remove.mutate(m.user_id)}>
                        移除
                      </Button>
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Root>
            </div>
          </AdminSection>
          {error && <Text color="red" size="2">{error}</Text>}
          <Flex justify="end">
            <Button variant="soft" color="gray" onClick={onClose}>关闭</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function UserSearchSelect({
  selectedUserID,
  onSelect,
}: {
  selectedUserID: string;
  onSelect: (id: string) => void;
}) {
  const [q, setQ] = useState("");
  const [showResults, setShowResults] = useState(false);
  const query = useQuery({
    queryKey: ["admin", "users", "search", q],
    queryFn: () => adminApi.users({ q: q.trim(), limit: 8 }),
    enabled: showResults && q.trim().length > 0,
  });
  const users = (query.data?.users ?? []) as UserOption[];

  return (
    <Box>
      <ClearableTextField
        placeholder="输入邮箱、用户名或昵称搜索"
        value={q}
        onChange={(e) => { setQ(e.target.value); setShowResults(true); }}
      />
      {selectedUserID && (
        <Text size="1" color="gray" as="div" mt="1">
          已选择：{selectedUserID}
        </Text>
      )}
      {showResults && q.trim() && (
        <Box mt="2" style={{ border: "1px solid var(--gray-a5)", borderRadius: 6, overflow: "hidden" }}>
          {query.isLoading && (
            <Box p="2"><Text size="2" color="gray">搜索中...</Text></Box>
          )}
          {!query.isLoading && users.length === 0 && (
            <Box p="2"><Text size="2" color="gray">没有匹配用户</Text></Box>
          )}
          {users.map((u) => (
            <button
              key={u.id}
              type="button"
              onClick={() => { onSelect(u.id); setQ(userLabel(u)); setShowResults(false); }}
              style={{
                display: "block",
                width: "100%",
                textAlign: "left",
                padding: "8px 10px",
                border: 0,
                borderBottom: "1px solid var(--gray-a4)",
                background: u.id === selectedUserID ? "var(--accent-a3)" : "transparent",
                color: "var(--gray-12)",
                cursor: "pointer",
              }}
            >
              <Text size="2" weight="medium" as="span" style={{ display: "block" }}>{userLabel(u)}</Text>
              <Text size="1" color="gray" as="span" style={{ display: "block" }}>{u.id}</Text>
            </button>
          ))}
        </Box>
      )}
    </Box>
  );
}

function userLabel(u: UserOption) {
  return [u.email, u.username, u.nickname].filter(Boolean).join(" / ") || u.id;
}
