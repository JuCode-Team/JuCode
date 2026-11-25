import "../../personal-responsive.css";
import { useState, type CSSProperties } from "react";
import { Badge, Box, Button, Card, Dialog, Flex, IconButton, Select, Table, Text, TextArea, Tooltip } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, RotateCcw, Settings as SettingsIcon, Wallet } from "lucide-react";
import { useTranslation } from "react-i18next";
import i18n from "../../i18n";
import { meApi, type Organization, type OrganizationMember } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../../components/ClearableTextField";
import { PointAmount } from "../../components/PointAmount";
import { useAuthStore } from "../../store/auth";
import { useActiveOrg } from "../../hooks/useOrgContext";
import { PageHeader } from "../../components/PageHeader";

// 组织管理：组织上下文专属 tab（仅 owner/admin 在侧边栏可见）。
// 管理的对象永远是切换器里选中的当前组织 —— 页面内不再单独选组织。
// 一张总览卡（余额/限额/成员数 + 充值、设置入口）+ 成员表格；
// 所有写操作走弹窗。

type OrgRole = OrganizationMember["role"];

function roleLabel(role: OrgRole) {
  return role === "owner" ? "Owner" : role === "admin" ? i18n.t("keys.org.roleAdmin") : i18n.t("keys.org.roleMember");
}

function roleColor(role: OrgRole): "iris" | "blue" | "gray" {
  return role === "owner" ? "iris" : role === "admin" ? "blue" : "gray";
}

export default function OrgManage() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);
  const activeOrg = useActiveOrg();
  const orgId = activeOrg?.id ?? "";
  const detail = useQuery({
    queryKey: ["me", "organizations", orgId],
    queryFn: () => meApi.organizationDetail(orgId),
    enabled: !!orgId,
  });
  const org = detail.data;
  const myRole = activeOrg?.role ?? (org?.members ?? []).find((m) => m.user_id === user?.id)?.role ?? null;
  const canManage = myRole === "owner" || myRole === "admin";

  const [topupOpen, setTopupOpen] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [memberDialog, setMemberDialog] = useState<{ mode: "add" } | { mode: "edit"; member: OrganizationMember } | null>(null);

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["me", "organizations"] });
    if (orgId) qc.invalidateQueries({ queryKey: ["me", "organizations", orgId] });
  };

  if (!activeOrg) {
    return (
      <PageHeader
        title={t("keys.org.title")}
        description={t("keys.org.noOrg")}
      />
    );
  }

  return (
    <Box>
      <PageHeader
        title={t("keys.org.title")}
        description={t("keys.org.desc", { org: activeOrg.org?.name ?? t("keys.shared.orgFallback") })}
        actions={
          <Tooltip content={t("keys.shared.refresh")}>
            <IconButton variant="soft" color="gray" onClick={() => detail.refetch()}><RotateCcw size={14} /></IconButton>
          </Tooltip>
        }
      />

      {org && (
        <Flex direction="column" gap="3">
          <Card className="pn-card-flat">
            <Flex align="start" justify="between" gap="3" wrap="wrap">
              <Box style={{ minWidth: 0 }}>
                <Flex align="center" gap="2" wrap="wrap">
                  <Text size="4" weight="bold" truncate>{org.name}</Text>
                  {myRole && <Badge size="1" variant="soft" color={roleColor(myRole)}>{roleLabel(myRole)}</Badge>}
                </Flex>
                <Text size="1" color="gray" as="div" mt="1">{t("keys.org.balance")}</Text>
                <Text size="7" weight="bold" className="jc-num" as="div" mt="1" style={{ lineHeight: 1.1 }}>
                  <PointAmount amount={org.balance} decimals="preserve" iconSize={20} />
                </Text>
              </Box>
              {canManage && (
                <Flex gap="2">
                  <Button onClick={() => setTopupOpen(true)}><Wallet size={14} /> {t("keys.org.topup")}</Button>
                  <Tooltip content={t("keys.org.settings")}>
                    <IconButton variant="soft" color="gray" onClick={() => setSettingsOpen(true)}>
                      <SettingsIcon size={15} />
                    </IconButton>
                  </Tooltip>
                </Flex>
              )}
            </Flex>
            <div
              className="jc-overview-metric-grid jc-me-metric-strip"
              style={{ "--jc-overview-cols": 2 } as CSSProperties}
            >
              <div className="jc-overview-metric" data-tone="amber">
                <div className="jc-overview-metric-label">{t("keys.org.monthlyLimit")}</div>
                <div className="jc-overview-metric-value jc-num">
                  {Number(org.monthly_spend_limit) > 0
                    ? <PointAmount amount={org.monthly_spend_limit} decimals="preserve" iconSize={14} />
                    : t("keys.shared.unlimited")}
                </div>
              </div>
              <div className="jc-overview-metric" data-tone="iris">
                <div className="jc-overview-metric-label">{t("keys.org.memberCount")}</div>
                <div className="jc-overview-metric-value jc-num">{(org.members ?? []).length}</div>
              </div>
            </div>
          </Card>

          <MembersCard
            org={org}
            canManage={canManage}
            loading={detail.isLoading}
            currentUserId={user?.id}
            onAdd={() => setMemberDialog({ mode: "add" })}
            onEdit={(member) => setMemberDialog({ mode: "edit", member })}
            onChanged={invalidate}
          />
        </Flex>
      )}

      {org && (
        <>
          <TopupDialog open={topupOpen} onClose={() => setTopupOpen(false)} org={org} onSaved={invalidate} />
          <SettingsDialog open={settingsOpen} onClose={() => setSettingsOpen(false)} org={org} onSaved={invalidate} />
          <MemberDialog state={memberDialog} onClose={() => setMemberDialog(null)} org={org} onSaved={invalidate} />
        </>
      )}
    </Box>
  );
}

function MembersCard({
  org, canManage, loading, currentUserId, onAdd, onEdit, onChanged,
}: {
  org: Organization;
  canManage: boolean;
  loading: boolean;
  currentUserId?: string;
  onAdd: () => void;
  onEdit: (member: OrganizationMember) => void;
  onChanged: () => void;
}) {
  const { t } = useTranslation();
  const remove = useMutation({
    mutationFn: (id: string) => meApi.removeOrganizationMember(org.id, id),
    onSuccess: onChanged,
  });
  const members = org.members ?? [];
  return (
    <Card className="pn-card-flat">
      <Flex justify="between" align="center" mb="3" gap="2" wrap="wrap">
        <Flex align="center" gap="2">
          <Text size="3" weight="bold">{t("keys.org.members")}</Text>
          <Badge size="1" variant="soft" color="gray">{members.length}</Badge>
          {loading && <Badge variant="soft" color="gray">{t("keys.shared.loading")}</Badge>}
        </Flex>
        {canManage && (
          <Button size="1" variant="soft" onClick={onAdd}><Plus size={13} /> {t("keys.org.addMember")}</Button>
        )}
      </Flex>
      <Box style={{ overflowX: "auto" }}>
        <Table.Root variant="surface" size="2">
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>{t("keys.org.user")}</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>{t("keys.org.role")}</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">{t("keys.org.memberMonthly")}</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">{t("keys.org.memberTotal")}</Table.ColumnHeaderCell>
              {canManage && <Table.ColumnHeaderCell align="right">{t("keys.org.actions")}</Table.ColumnHeaderCell>}
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {members.map((m) => (
              <Table.Row key={m.user_id}>
                <Table.Cell>
                  <Flex align="center" gap="2">
                    <Text weight="medium">{m.email || m.nickname || m.user_id}</Text>
                    {m.user_id === currentUserId && <Badge size="1" variant="soft" color="green">{t("keys.org.me")}</Badge>}
                  </Flex>
                  <Text size="1" color="gray" as="div" className="jc-mono">{m.user_id}</Text>
                </Table.Cell>
                <Table.Cell><Badge variant="soft" color={roleColor(m.role)}>{roleLabel(m.role)}</Badge></Table.Cell>
                <Table.Cell align="right" className="jc-num">
                  {Number(m.monthly_spend_limit) > 0 ? <PointAmount amount={m.monthly_spend_limit} decimals="preserve" iconSize={12} /> : t("keys.shared.unlimited")}
                </Table.Cell>
                <Table.Cell>
                  {Number(m.total_spend_limit) > 0 ? <PointAmount amount={m.total_spend_limit} decimals="preserve" iconSize={12} /> : t("keys.shared.unlimited")}
                </Table.Cell>
                {canManage && (
                  <Table.Cell align="right">
                    <Flex gap="1" justify="end">
                      <Tooltip content={t("keys.org.editTooltip")}>
                        <IconButton size="1" variant="soft" color="gray" onClick={() => onEdit(m)}>
                          <Pencil size={12} />
                        </IconButton>
                      </Tooltip>
                      <Button size="1" variant="soft" color="red" disabled={m.role === "owner" || remove.isPending}
                        onClick={() => confirm(t("keys.org.removeConfirm")) && remove.mutate(m.user_id)}>
                        {t("keys.org.remove")}
                      </Button>
                    </Flex>
                  </Table.Cell>
                )}
              </Table.Row>
            ))}
            {members.length === 0 && (
              <Table.Row><Table.Cell colSpan={canManage ? 4 : 3}><Text size="2" color="gray">{t("keys.org.noMembers")}</Text></Table.Cell></Table.Row>
            )}
          </Table.Body>
        </Table.Root>
      </Box>
      {remove.error != null && <Text size="2" color="red" mt="2" as="div">{errMsg(remove.error)}</Text>}
    </Card>
  );
}

function TopupDialog({ open, onClose, org, onSaved }: { open: boolean; onClose: () => void; org: Organization; onSaved: () => void }) {
  const { t } = useTranslation();
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const topup = useMutation({
    mutationFn: () => meApi.topupOrganization(org.id, { amount, note }),
    onSuccess: () => { setAmount(""); setNote(""); onSaved(); onClose(); },
  });
  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="420px">
        <Dialog.Title>{t("keys.org.topupTitle")}</Dialog.Title>
        <Dialog.Description size="2" color="gray" mb="3">
          {t("keys.org.topupDesc", { org: org.name })}
        </Dialog.Description>
        <Flex direction="column" gap="3">
          <Box>
            <Text size="1" color="gray" as="div" mb="1">{t("keys.org.amount")}</Text>
            <ClearableTextField type="number" min="0" step="0.01" value={amount} onChange={(e) => setAmount(e.target.value)} autoFocus />
          </Box>
          <Box>
            <Text size="1" color="gray" as="div" mb="1">{t("keys.org.note")}</Text>
            <TextArea value={note} onChange={(e) => setNote(e.target.value)} rows={2} />
          </Box>
          {topup.isError && <Text size="2" color="red">{errMsg(topup.error)}</Text>}
          <Flex gap="2" justify="end">
            <Button variant="soft" color="gray" onClick={onClose}>{t("keys.shared.cancel")}</Button>
            <Button disabled={!amount.trim() || Number(amount) <= 0} loading={topup.isPending} onClick={() => topup.mutate()}>{t("keys.org.confirmTopup")}</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function SettingsDialog({ open, onClose, org, onSaved }: { open: boolean; onClose: () => void; org: Organization; onSaved: () => void }) {
  const { t } = useTranslation();
  const [orgName, setOrgName] = useState(org.name);
  const [orgLimit, setOrgLimit] = useState(org.monthly_spend_limit);
  const update = useMutation({
    mutationFn: () => meApi.updateOrganization(org.id, {
      name: orgName.trim(),
      monthly_spend_limit: orgLimit.trim() || "0",
    }),
    onSuccess: () => { onSaved(); onClose(); },
  });
  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="420px">
        <Dialog.Title>{t("keys.org.settings")}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Box>
            <Text size="1" color="gray" as="div" mb="1">{t("keys.org.name")}</Text>
            <ClearableTextField value={orgName} onChange={(e) => setOrgName(e.target.value)} />
          </Box>
          <Box>
            <Text size="1" color="gray" as="div" mb="1">{t("keys.org.monthlyLimitInput")}</Text>
            <ClearableTextField type="number" min="0" step="0.01" value={orgLimit} onChange={(e) => setOrgLimit(e.target.value)} />
          </Box>
          {update.isError && <Text size="2" color="red">{errMsg(update.error)}</Text>}
          <Flex gap="2" justify="end">
            <Button variant="soft" color="gray" onClick={onClose}>{t("keys.shared.cancel")}</Button>
            <Button disabled={!orgName.trim()} loading={update.isPending} onClick={() => update.mutate()}>{t("keys.shared.save")}</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

// MemberDialog covers both 添加 (free user-id input) and 编辑 (id locked,
// role/limit prefilled) — same upsert endpoint underneath. The body owns
// its form state and is remounted per dialog identity via `key`, so
// prefill happens naturally in the useState initializers.
function MemberDialog({
  state, onClose, org, onSaved,
}: {
  state: { mode: "add" } | { mode: "edit"; member: OrganizationMember } | null;
  onClose: () => void;
  org: Organization;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  if (!state) return null;
  const editing = state.mode === "edit" ? state.member : null;
  return (
    <Dialog.Root open onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="420px">
        <Dialog.Title>{editing ? t("keys.org.editMember") : t("keys.org.addMember")}</Dialog.Title>
        <MemberDialogBody key={editing?.user_id ?? "add"} editing={editing} org={org} onClose={onClose} onSaved={onSaved} />
      </Dialog.Content>
    </Dialog.Root>
  );
}

function MemberDialogBody({
  editing, org, onClose, onSaved,
}: {
  editing: OrganizationMember | null;
  org: Organization;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation();
  const [userId, setUserId] = useState(editing?.user_id ?? "");
  const [role, setRole] = useState<OrgRole>(editing?.role ?? "member");
  const [limit, setLimit] = useState(editing?.monthly_spend_limit || "0");
  const [totalLimit, setTotalLimit] = useState(editing?.total_spend_limit || "0");
  const upsert = useMutation({
    mutationFn: () => meApi.upsertOrganizationMember(
      org.id,
      userId.trim(),
      {
        role,
        monthly_spend_limit: limit.trim() || "0",
        total_spend_limit: totalLimit.trim() || "0",
      },
    ),
    onSuccess: () => { onSaved(); onClose(); },
  });
  return (
    <>
      <Flex direction="column" gap="3" mt="3">
        <Box>
          <Text size="1" color="gray" as="div" mb="1">{t("keys.org.userId")}</Text>
          <ClearableTextField
            value={userId}
            disabled={!!editing}
            onChange={(e) => setUserId(e.target.value)}
            placeholder={t("keys.org.userIdPlaceholder")}
          />
          {editing && (
            <Text size="1" color="gray" as="div" mt="1">{editing.email || editing.nickname || ""}</Text>
          )}
        </Box>
        <Box>
          <Text size="1" color="gray" as="div" mb="1">{t("keys.org.role")}</Text>
          <Select.Root value={role} onValueChange={(v) => setRole(v as OrgRole)}>
            <Select.Trigger style={{ width: "100%" }} />
            <Select.Content>
              <Select.Item value="member">{t("keys.org.roleMember")}</Select.Item>
              <Select.Item value="admin">{t("keys.org.roleAdmin")}</Select.Item>
              <Select.Item value="owner">Owner</Select.Item>
            </Select.Content>
          </Select.Root>
        </Box>
        <Box>
          <Text size="1" color="gray" as="div" mb="1">{t("keys.org.memberMonthlyInput")}</Text>
          <ClearableTextField type="number" min="0" step="0.01" value={limit} onChange={(e) => setLimit(e.target.value)} />
          <Text size="1" color="gray" as="div" mt="1">{t("keys.org.monthlyReset")}</Text>
        </Box>
        <Box>
          <Text size="1" color="gray" as="div" mb="1">{t("keys.org.memberTotalInput")}</Text>
          <ClearableTextField type="number" min="0" step="0.01" value={totalLimit} onChange={(e) => setTotalLimit(e.target.value)} />
          <Text size="1" color="gray" as="div" mt="1">
            {t("keys.org.totalHint")}
          </Text>
        </Box>
      </Flex>
      {upsert.isError && <Text size="2" color="red" as="div" mt="2">{errMsg(upsert.error)}</Text>}
      <Flex gap="2" justify="end" mt="3">
        <Button variant="soft" color="gray" onClick={onClose}>{t("keys.shared.cancel")}</Button>
        <Button disabled={!userId.trim()} loading={upsert.isPending} onClick={() => upsert.mutate()}>{t("keys.shared.save")}</Button>
      </Flex>
    </>
  );
}
