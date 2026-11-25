import { useEffect, useState, type ReactNode } from "react";
import { Badge, Box, Button, Callout, Dialog, Flex, Grid, Select, Switch, Tabs, Text, TextArea } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ShieldAlert } from "lucide-react";
import {
  adminApi,
  type InviteBinding,
  type InviteBindingStatus,
  type InvitePayoutStatus,
  type InviteProgramSettings,
  type InviteRebate,
  type InviteReward,
  type InviteSignupCondition,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { AdminPage, AdminSection } from "../../components/AdminPage";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { usePagedQuery } from "../../lib/paged";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";

// All rules default to closed / 0 — the program must stay off until
// 运营 confirms the amounts.
const DEFAULT_PROGRAM: InviteProgramSettings = {
  enabled: false,
  signup_reward_enabled: false,
  signup_reward_amount: "0",
  signup_condition: "none",
  signup_min_topup_amount: "0",
  topup_rebate_enabled: false,
  topup_rebate_rate_pct: "0",
  topup_rebate_window_days: 0,
  topup_rebate_cap_per_invitee: "0",
  signup_daily_count_cap_per_inviter: 0,
  signup_daily_amount_cap_per_inviter: "0",
  inviter_min_age_hours: 0,
  same_ip_daily_cap: 0,
  review_amount_threshold: "0",
  auto_hold_on_risk: false,
  require_inviter_email_verified: false,
};

const SIGNUP_CONDITION_LABEL: Record<InviteSignupCondition, string> = {
  none: "无条件",
  email_verified: "完成邮箱验证",
  any_topup: "完成任意付费",
  min_topup: "累计实付满指定金额",
};

const SIGNUP_CONDITION_HINT: Record<InviteSignupCondition, string> = {
  none: "被邀请人注册成功即计入奖励。没有任何门槛，最容易被批量注册薅走，谨慎使用。",
  email_verified: "被邀请人完成邮箱验证后才计入奖励。能挡住一次性小号，挡不住真实邮箱批量注册。",
  any_topup: "被邀请人完成余额充值或套餐购买后达标，按实付金额计算。",
  min_topup: "余额充值和套餐购买的累计实付达到下方金额后达标，扣除退款；赠送余额和代理优惠码订单不计入。",
};

function payoutStatusColor(s: InvitePayoutStatus): "gray" | "green" | "amber" | "red" | "blue" | "orange" {
  return ({
    pending: "amber",
    qualified: "blue",
    held: "orange",
    paid: "green",
    rejected: "red",
    voided: "gray",
  } as const)[s];
}

function payoutStatusLabel(s: InvitePayoutStatus): string {
  return ({
    pending: "待达标",
    qualified: "待发放",
    held: "已挂起",
    paid: "已发放",
    rejected: "已拒绝",
    voided: "已作废",
  } as const)[s];
}

function bindingStatusLabel(s: InviteBindingStatus): string {
  return s === "bound" ? "已绑定" : "已作废";
}

// Known risk flags get a Chinese label; anything the backend adds later
// falls through and shows raw so it is never silently hidden.
// Keys must match the constants in internal/biz/invite.go verbatim —
// they are the only audit surface for a payout that went through without
// being held.
const RISK_FLAG_LABEL: Record<string, string> = {
  inviter_not_active: "邀请人非正常状态",
  inviter_frozen: "邀请人被冻结",
  inviter_account_too_new: "邀请人账号过新",
  inviter_email_unverified: "邀请人邮箱未验证",
  invitee_ip_matches_inviter: "疑似自邀（同 IP）",
  invitee_payer_matches_inviter: "疑似自邀（同付款账号）",
  same_ip_daily_cap_exceeded: "同 IP 超日上限",
  daily_signup_count_cap_exceeded: "超日拉新次数上限",
  daily_signup_amount_cap_exceeded: "超日拉新金额上限",
  amount_over_threshold: "超单笔审核金额",
};

function RiskFlags({ flags }: { flags?: string[] }) {
  if (!flags || flags.length === 0) return <Text size="1" color="gray">—</Text>;
  return (
    <Flex gap="1" wrap="wrap">
      {flags.map((f) => (
        <Badge key={f} color="red" variant="soft" size="1">{RISK_FLAG_LABEL[f] ?? f}</Badge>
      ))}
    </Flex>
  );
}

// Admin view shows emails in full — no masking. Long addresses truncate
// with the complete value on hover; falls back to the id prefix when the
// list didn't resolve the email.
function Person({ email, id }: { email?: string; id?: string }) {
  const short = id ? id.slice(0, 8) : "—";
  return (
    <Flex direction="column" style={{ minWidth: 0 }} title={email || id || undefined}>
      <Text
        size="1"
        style={{ maxWidth: "100%", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}
      >
        {email || short}
      </Text>
      {email && id && <Text size="1" color="gray" className="jc-mono" title={id}>{short}</Text>}
    </Flex>
  );
}

function dt(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date
    .toLocaleString("zh-CN", {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    })
    .replaceAll("/", "-");
}

// One pending admin action awaiting its second confirmation. Rewards
// carry an optional note; rebates and binding void are body-less.
type PendingAction =
  | { kind: "reward"; action: "pay" | "reject"; row: InviteReward }
  | { kind: "rebate"; action: "pay" | "reject"; row: InviteRebate }
  | { kind: "binding"; action: "void"; row: InviteBinding };

const REVIEW_ACTION_STYLE = { minHeight: 40 } as const;

function validUserID(value: string): boolean {
  return value.trim() === "" || /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value.trim());
}

export default function Invite() {
  const [rewardStatus, setRewardStatus] = useState("");
  const [rewardInviter, setRewardInviter] = useState("");
  const [rewardInvitee, setRewardInvitee] = useState("");
  const [rebateStatus, setRebateStatus] = useState("");
  const [rebateInviter, setRebateInviter] = useState("");
  const [rebateInvitee, setRebateInvitee] = useState("");
  const [bindingStatus, setBindingStatus] = useState("");
  const [pending, setPending] = useState<PendingAction | null>(null);
  const rewardIDsValid = validUserID(rewardInviter) && validUserID(rewardInvitee);
  const rebateIDsValid = validUserID(rebateInviter) && validUserID(rebateInvitee);

  const settings = useQuery({ queryKey: ["admin", "settings"], queryFn: adminApi.getSettings });
  const program = settings.data?.invite_program ?? DEFAULT_PROGRAM;

  const bindings = usePagedQuery<InviteBinding>({
    queryKey: ["admin", "invite", "bindings"],
    queryFn: (p) => adminApi.inviteBindingsPaged({
      q: p.q || undefined,
      status: bindingStatus || undefined,
      limit: p.limit,
      offset: p.offset,
    }),
    extras: [bindingStatus],
    pageSize: 50,
  });

  const rewards = usePagedQuery<InviteReward>({
    queryKey: ["admin", "invite", "rewards"],
    queryFn: (p) => adminApi.inviteRewardsPaged({
      status: rewardStatus || undefined,
      inviter_id: rewardInviter.trim() || undefined,
      invitee_id: rewardInvitee.trim() || undefined,
      limit: p.limit,
      offset: p.offset,
    }),
    extras: [rewardStatus, rewardInviter, rewardInvitee],
    pageSize: 50,
    enabled: rewardIDsValid,
  });

  const rebates = usePagedQuery<InviteRebate>({
    queryKey: ["admin", "invite", "rebates"],
    queryFn: (p) => adminApi.inviteRebatesPaged({
      status: rebateStatus || undefined,
      inviter_id: rebateInviter.trim() || undefined,
      invitee_id: rebateInvitee.trim() || undefined,
      limit: p.limit,
      offset: p.offset,
    }),
    extras: [rebateStatus, rebateInviter, rebateInvitee],
    pageSize: 50,
    enabled: rebateIDsValid,
  });

  const bindingColumns: Column<InviteBinding>[] = [
    { header: "邀请人", width: "165px", cell: (b) => <Person email={b.inviter_email} id={b.inviter_user_id} /> },
    { header: "被邀请人", width: "165px", cell: (b) => <Person email={b.invitee_email} id={b.invitee_user_id} /> },
    {
      header: "状态", width: "90px",
      cell: (b) => (
        <Badge color={b.status === "bound" ? "green" : "gray"} variant="soft">{bindingStatusLabel(b.status)}</Badge>
      ),
    },
    { header: "邀请码", width: "110px", hideOn: "md", cell: (b) => b.invite_code ? <span className="jc-mono">{b.invite_code}</span> : "—" },
    { header: "注册 IP", width: "120px", hideOn: "md", cell: (b) => b.register_ip ? <span className="jc-mono">{b.register_ip}</span> : "—" },
    { header: "绑定时间", width: "150px", cell: (b) => dt(b.created_at) },
    {
      header: "操作", width: "70px", minWidth: "70px", align: "right",
      cell: (b) => b.status === "bound" && (
        <Button size="1" color="red" variant="soft" style={REVIEW_ACTION_STYLE} onClick={() => setPending({ kind: "binding", action: "void", row: b })}>
          作废
        </Button>
      ),
    },
  ];

  const rewardColumns: Column<InviteReward>[] = [
    { header: "邀请人", width: "165px", cell: (r) => <Person email={r.inviter_email} id={r.inviter_user_id} /> },
    { header: "被邀请人", width: "165px", cell: (r) => <Person email={r.invitee_email} id={r.invitee_user_id} /> },
    {
      header: "金额", width: "90px", align: "right",
      cell: (r) => <span className="jc-num"><PointAmount amount={r.amount} iconSize={12} /></span>,
    },
    {
      header: "状态", width: "90px",
      cell: (r) => <Badge color={payoutStatusColor(r.status)} variant="soft">{payoutStatusLabel(r.status)}</Badge>,
    },
    { header: "风险标记", width: "105px", wrap: true, cell: (r) => <RiskFlags flags={r.risk_flags} /> },
    { header: "创建时间", width: "135px", cell: (r) => dt(r.created_at) },
    {
      header: "备注", width: "90px", hideOn: "md",
      cell: (r) => (
        <Text
          size="1"
          color="gray"
          title={r.note}
          style={{ display: "block", maxWidth: 90, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}
        >
          {r.note || "—"}
        </Text>
      ),
    },
    {
      header: "操作", width: "110px", minWidth: "110px", align: "right",
      cell: (r) => (r.status === "pending" || r.status === "qualified" || r.status === "held") && (
        <Flex gap="1" justify="end">
          <Button size="1" color="green" variant="soft" style={REVIEW_ACTION_STYLE} onClick={() => setPending({ kind: "reward", action: "pay", row: r })}>发放</Button>
          <Button size="1" color="red" variant="soft" style={REVIEW_ACTION_STYLE} onClick={() => setPending({ kind: "reward", action: "reject", row: r })}>拒绝</Button>
        </Flex>
      ),
    },
  ];

  const rebateColumns: Column<InviteRebate>[] = [
    { header: "邀请人", width: "165px", cell: (r) => <Person email={r.inviter_email} id={r.inviter_user_id} /> },
    { header: "被邀请人", width: "165px", cell: (r) => <Person email={r.invitee_email} id={r.invitee_user_id} /> },
    {
      header: "返利", width: "110px", align: "right",
      // A refunded order leaves the payout row intact and records what came
      // back, so show both rather than a figure that no longer holds.
      cell: (r) => (
        <Flex direction="column" align="end">
          <span className="jc-num"><PointAmount amount={r.rebate_amount} iconSize={12} /></span>
          {Number(r.clawed_back_amount) > 0 && (
            <Text size="1" color="red">已退回 {r.clawed_back_amount}</Text>
          )}
        </Flex>
      ),
    },
    {
      header: "来源", width: "120px", align: "right", hideOn: "md",
      cell: (r) => (
        <Flex direction="column" align="end">
          <span className="jc-num"><PointAmount amount={r.source_amount} iconSize={11} /></span>
          <Text size="1" color="gray">
            {r.source_kind === "topup" ? "充值" : "消费"} · {r.rate_pct_snapshot}%
          </Text>
        </Flex>
      ),
    },
    {
      header: "状态", width: "90px",
      cell: (r) => <Badge color={payoutStatusColor(r.status)} variant="soft">{payoutStatusLabel(r.status)}</Badge>,
    },
    { header: "风险标记", width: "105px", wrap: true, cell: (r) => <RiskFlags flags={r.risk_flags} /> },
    { header: "创建时间", width: "135px", cell: (r) => dt(r.created_at) },
    {
      header: "操作", width: "110px", minWidth: "110px", align: "right",
      cell: (r) => (r.status === "pending" || r.status === "held") && (
        <Flex gap="1" justify="end">
          <Button size="1" color="green" variant="soft" style={REVIEW_ACTION_STYLE} onClick={() => setPending({ kind: "rebate", action: "pay", row: r })}>发放</Button>
          <Button size="1" color="red" variant="soft" style={REVIEW_ACTION_STYLE} onClick={() => setPending({ kind: "rebate", action: "reject", row: r })}>拒绝</Button>
        </Flex>
      ),
    },
  ];

  // Rebates have no `qualified` state — they are payable or held on
  // insert — so offering it there only ever returns an empty list.
  const payoutStatusFilter = (value: string, onChange: (v: string) => void, kind: "reward" | "rebate" = "reward") => (
    <Select.Root value={value || "_all"} onValueChange={(v) => onChange(v === "_all" ? "" : v)}>
      <Select.Trigger placeholder="状态" style={{ minHeight: 40 }} />
      <Select.Content>
        <Select.Item value="_all">全部状态</Select.Item>
        <Select.Item value="pending">待达标</Select.Item>
        {kind === "reward" && <Select.Item value="qualified">待发放</Select.Item>}
        <Select.Item value="held">已挂起</Select.Item>
        <Select.Item value="paid">已发放</Select.Item>
        <Select.Item value="rejected">已拒绝</Select.Item>
        <Select.Item value="voided">已作废</Select.Item>
      </Select.Content>
    </Select.Root>
  );

  const idFilter = (placeholder: string, value: string, onChange: (v: string) => void) => (
    <Box style={{ width: 150 }}>
      <ClearableTextField size="3" aria-label={placeholder} aria-invalid={!validUserID(value)} placeholder={placeholder} value={value} onChange={(e) => onChange(e.target.value)} />
      {!validUserID(value) && <Text as="p" size="1" color="red" role="status">请输入完整用户 ID（36 位，含连字符）</Text>}
    </Box>
  );

  return (
    <AdminPage
      title="邀请返利"
      description="配置拉新奖励和充值返利规则，审核邀请关系与待发放的奖励。"
    >
      <Grid
        columns={{ initial: "2", lg: "4" }}
        mb="4"
        style={{ borderTop: "1px solid var(--c-outline-variant)", borderBottom: "1px solid var(--c-outline-variant)" }}
      >
        <SummaryMetric
          label="活动状态"
          value={program.enabled ? "已开启" : "已关闭"}
          detail={program.enabled
            ? `拉新 ${program.signup_reward_enabled ? "开" : "关"} · 返利 ${program.topup_rebate_enabled ? "开" : "关"}`
            : "规则不生效，不产生新奖励"}
          color={program.enabled ? "green" : "gray"}
        />
        <SummaryMetric label="邀请关系" value={bindings.total.toLocaleString()} detail="当前筛选匹配" />
        <SummaryMetric label="拉新奖励" value={rewards.total.toLocaleString()} detail="当前筛选匹配" />
        <SummaryMetric label="充值返利" value={rebates.total.toLocaleString()} detail="当前筛选匹配" />
      </Grid>

      <Tabs.Root defaultValue="rules">
        <Tabs.List mb="4">
          <Tabs.Trigger value="rules" style={{ paddingInline: 4 }}>规则</Tabs.Trigger>
          <Tabs.Trigger value="bindings" style={{ paddingInline: 4 }}>邀请关系</Tabs.Trigger>
          <Tabs.Trigger value="rewards" style={{ paddingInline: 4 }}>拉新奖励</Tabs.Trigger>
          <Tabs.Trigger value="rebates" style={{ paddingInline: 4 }}>充值返利</Tabs.Trigger>
        </Tabs.List>

        <Tabs.Content value="rules">
          <ProgramRulesForm
            loaded={settings.data?.invite_program}
            isLoading={settings.isLoading}
            loadError={settings.isError ? errMsg(settings.error) : ""}
          />
        </Tabs.Content>

        <Tabs.Content value="bindings">
          <AdminSection title="邀请关系" description="谁邀请了谁。作废后该关系不再产生任何奖励和返利。">
            <ListLoadError label="邀请关系" error={bindings.query.error} />
            <DataTable<InviteBinding>
              {...bindings}
              card={false}
              controlSize="3"
              isLoading={bindings.query.isLoading}
              isFetching={bindings.query.isFetching}
              refetch={() => bindings.query.refetch()}
              columns={bindingColumns}
              rowKey={(b) => b.id}
              searchPlaceholder="搜索邮箱 / 用户 ID…"
              emptyText="暂无邀请关系"
              filters={(
                <Select.Root value={bindingStatus || "_all"} onValueChange={(v) => setBindingStatus(v === "_all" ? "" : v)}>
                  <Select.Trigger placeholder="状态" style={{ minHeight: 40 }} />
                  <Select.Content>
                    <Select.Item value="_all">全部状态</Select.Item>
                    <Select.Item value="bound">已绑定</Select.Item>
                    <Select.Item value="voided">已作废</Select.Item>
                  </Select.Content>
                </Select.Root>
              )}
            />
          </AdminSection>
        </Tabs.Content>

        <Tabs.Content value="rewards">
          <AdminSection title="拉新奖励" description="待发放 / 已挂起的奖励需要人工确认后发放，发放和拒绝都有二次确认。">
            <ListLoadError label="拉新奖励" error={rewardIDsValid ? rewards.query.error : null} />
            <DataTable<InviteReward>
              {...rewards}
              card={false}
              controlSize="3"
              items={rewardIDsValid ? rewards.items : []}
              isLoading={rewardIDsValid && rewards.query.isLoading}
              isFetching={rewards.query.isFetching}
              refetch={() => { if (rewardIDsValid) void rewards.query.refetch(); }}
              columns={rewardColumns}
              rowKey={(r) => r.id}
              emptyText={rewardIDsValid ? "暂无拉新奖励" : "请填写完整用户 ID 后查询"}
              filters={(
                <>
                  {payoutStatusFilter(rewardStatus, setRewardStatus)}
                  {idFilter("邀请人 ID", rewardInviter, setRewardInviter)}
                  {idFilter("被邀请人 ID", rewardInvitee, setRewardInvitee)}
                </>
              )}
            />
          </AdminSection>
        </Tabs.Content>

        <Tabs.Content value="rebates">
          <AdminSection title="充值返利" description="按被邀请人每笔充值订单计算；订单退款时自动扣回。发放和拒绝都有二次确认。">
            <ListLoadError label="充值返利" error={rebateIDsValid ? rebates.query.error : null} />
            <DataTable<InviteRebate>
              {...rebates}
              card={false}
              controlSize="3"
              items={rebateIDsValid ? rebates.items : []}
              isLoading={rebateIDsValid && rebates.query.isLoading}
              isFetching={rebates.query.isFetching}
              refetch={() => { if (rebateIDsValid) void rebates.query.refetch(); }}
              columns={rebateColumns}
              rowKey={(r) => r.id}
              emptyText={rebateIDsValid ? "暂无充值返利" : "请填写完整用户 ID 后查询"}
              filters={(
                <>
                  {payoutStatusFilter(rebateStatus, setRebateStatus, "rebate")}
                  {idFilter("邀请人 ID", rebateInviter, setRebateInviter)}
                  {idFilter("被邀请人 ID", rebateInvitee, setRebateInvitee)}
                </>
              )}
            />
          </AdminSection>
        </Tabs.Content>
      </Tabs.Root>

      <ConfirmActionDialog
        key={pending ? `${pending.kind}-${pending.action}-${pending.row.id}` : "closed"}
        pending={pending}
        onClose={() => setPending(null)}
      />
    </AdminPage>
  );
}

// ---- rules form ----

function ProgramRulesForm({
  loaded,
  isLoading,
  loadError,
}: {
  loaded?: InviteProgramSettings;
  isLoading: boolean;
  loadError: string;
}) {
  const qc = useQueryClient();
  const [form, setForm] = useState<InviteProgramSettings>(DEFAULT_PROGRAM);

  useEffect(() => {
    if (loaded) setForm({ ...DEFAULT_PROGRAM, ...loaded });
  }, [loaded]);

  const set = <K extends keyof InviteProgramSettings>(key: K, value: InviteProgramSettings[K]) =>
    setForm((f) => ({ ...f, [key]: value }));

  // PUT expects the full SiteSettings blob: re-GET right before saving
  // and merge invite_program in, so other settings are never wiped.
  const save = useMutation({
    mutationFn: async () => {
      const current = await adminApi.getSettings();
      return adminApi.updateSettings({
        ...current,
        invite_program: {
          ...form,
          topup_rebate_window_days: Math.max(0, Math.trunc(Number(form.topup_rebate_window_days) || 0)),
          signup_daily_count_cap_per_inviter: Math.max(0, Math.trunc(Number(form.signup_daily_count_cap_per_inviter) || 0)),
          inviter_min_age_hours: Math.max(0, Math.trunc(Number(form.inviter_min_age_hours) || 0)),
          same_ip_daily_cap: Math.max(0, Math.trunc(Number(form.same_ip_daily_cap) || 0)),
        },
      });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "settings"] }),
  });

  const signupActive = form.enabled && form.signup_reward_enabled && Number(form.signup_reward_amount) > 0;
  const rebateActive = form.enabled && form.topup_rebate_enabled && Number(form.topup_rebate_rate_pct) > 0;

  return (
    <Flex direction="column" gap="4">
      {(signupActive || rebateActive) && (
        <Callout.Root color="amber" variant="surface">
          <Callout.Icon><ShieldAlert size={15} /></Callout.Icon>
          <Callout.Text>
            当前配置已开启非零奖励：
            {signupActive && <>拉新每人 {form.signup_reward_amount} 积分（{SIGNUP_CONDITION_LABEL[form.signup_condition]}）</>}
            {signupActive && rebateActive && "，"}
            {rebateActive && <>充值返利 {form.topup_rebate_rate_pct}%</>}
            。保存前请与运营确认金额，避免误发高额奖励。
          </Callout.Text>
        </Callout.Root>
      )}

      <AdminSection
        title="总开关"
        description="关闭时所有邀请规则不生效，不产生新的绑定、奖励和返利；已产生的记录仍可在其他选项卡审核。"
      >
        <Flex align="center" gap="3">
          <label style={{ display: "inline-flex", alignItems: "center", gap: 12, minHeight: 40 }}>
            <Switch checked={form.enabled} onCheckedChange={(v) => set("enabled", v)} />
            <Text size="2">启用邀请返利</Text>
          </label>
          <Badge color={form.enabled ? "green" : "gray"} variant="soft">{form.enabled ? "开启" : "关闭"}</Badge>
        </Flex>
      </AdminSection>

      <AdminSection
        title="拉新奖励"
        description="被邀请人满足条件后，给邀请人一次性发放固定金额。"
      >
        <Flex direction="column" gap="3">
          <label style={{ display: "inline-flex", alignItems: "center", gap: 12, minHeight: 40 }}>
            <Switch
              checked={form.signup_reward_enabled}
              onCheckedChange={(v) => set("signup_reward_enabled", v)}
              disabled={!form.enabled}
            />
            <Text size="2" color={form.enabled ? undefined : "gray"}>启用拉新奖励</Text>
          </label>
          <Grid columns={{ initial: "1", sm: "2" }} gap="3">
            <Field label="每人奖励金额（积分）" hint="发给邀请人的一次性奖励。0 = 不发。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={form.signup_reward_amount}
                onChange={(e) => set("signup_reward_amount", e.target.value)}
              />
            </Field>
            <Field label="达标条件" hint={SIGNUP_CONDITION_HINT[form.signup_condition]}>
              <Select.Root
                value={form.signup_condition}
                onValueChange={(v) => set("signup_condition", v as InviteSignupCondition)}
              >
                <Select.Trigger style={{ width: "100%", minHeight: 40 }} />
                <Select.Content>
                  {(Object.keys(SIGNUP_CONDITION_LABEL) as InviteSignupCondition[]).map((c) => (
                    <Select.Item key={c} value={c}>{SIGNUP_CONDITION_LABEL[c]}</Select.Item>
                  ))}
                </Select.Content>
              </Select.Root>
            </Field>
            {form.signup_condition === "min_topup" && (
              <Field label="最低实付金额（积分）" hint="余额充值和套餐购买计入，扣除退款。现有未发放奖励也按实付计算，门槛金额保留原记录。">
                <ClearableTextField
                  size="3"
                  type="number"
                  min="0"
                  value={form.signup_min_topup_amount}
                  onChange={(e) => set("signup_min_topup_amount", e.target.value)}
                />
              </Field>
            )}
          </Grid>
        </Flex>
      </AdminSection>

      <AdminSection
        title="充值返利"
        description="被邀请人每笔充值到账时，按固定比例给邀请人返利。订单退款时会按退款比例自动扣回。"
      >
        <Flex direction="column" gap="3">
          <label style={{ display: "inline-flex", alignItems: "center", gap: 12, minHeight: 40 }}>
            <Switch
              checked={form.topup_rebate_enabled}
              onCheckedChange={(v) => set("topup_rebate_enabled", v)}
              disabled={!form.enabled}
            />
            <Text size="2" color={form.enabled ? undefined : "gray"}>启用充值返利</Text>
          </label>
          <Grid columns={{ initial: "1", sm: "2" }} gap="3">
            <Field label="返利比例（%）" hint="按被邀请人每笔充值金额的百分比返给邀请人，向下取整到分。0 = 不返。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={form.topup_rebate_rate_pct}
                onChange={(e) => set("topup_rebate_rate_pct", e.target.value)}
              />
            </Field>
            <Field label="返利窗口（天）" hint="只对绑定后这段时间内的充值返利。0 = 不限时间。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={String(form.topup_rebate_window_days)}
                onChange={(e) => set("topup_rebate_window_days", Number(e.target.value) || 0)}
              />
            </Field>
            <Field label="单个被邀请人返利封顶（积分）" hint="同一个被邀请人最多给邀请人返这么多。0 = 不封顶。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={form.topup_rebate_cap_per_invitee}
                onChange={(e) => set("topup_rebate_cap_per_invitee", e.target.value)}
              />
            </Field>
          </Grid>
        </Flex>
      </AdminSection>

      <AdminSection
        title="风控上限"
        description="限制单个邀请人和单个 IP 的刷量空间。风险标记始终记录在发放记录上；是否因此挂起等人工审核，由下面的开关决定（默认不挂起，直接发放）。"
      >
        <Flex direction="column" gap="3">
          <Grid columns={{ initial: "1", sm: "2" }} gap="3">
            <Field label="邀请人每日拉新次数上限" hint="一个邀请人一天最多计入几个拉新奖励。0 = 不限。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={String(form.signup_daily_count_cap_per_inviter)}
                onChange={(e) => set("signup_daily_count_cap_per_inviter", Number(e.target.value) || 0)}
              />
            </Field>
            <Field label="邀请人每日拉新金额上限（积分）" hint="一个邀请人一天的拉新奖励总额上限。0 = 不限。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={form.signup_daily_amount_cap_per_inviter}
                onChange={(e) => set("signup_daily_amount_cap_per_inviter", e.target.value)}
              />
            </Field>
            <Field label="邀请人最低注册时长（小时）" hint="注册不满这个时长的账号，发出的邀请不计奖励。0 = 不限。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={String(form.inviter_min_age_hours)}
                onChange={(e) => set("inviter_min_age_hours", Number(e.target.value) || 0)}
              />
            </Field>
            <Field label="同一 IP 每日绑定上限" hint="同一个 IP 一天最多产生几条邀请绑定。0 = 不限。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={String(form.same_ip_daily_cap)}
                onChange={(e) => set("same_ip_daily_cap", Number(e.target.value) || 0)}
              />
            </Field>
            <Field label="单笔审核金额" hint="达到这个金额的奖励或返利会先扣住等人工审核，与下面的开关无关。0 = 不按金额扣住（默认）。">
              <ClearableTextField
                size="3"
                type="number"
                min="0"
                value={form.review_amount_threshold}
                onChange={(e) => set("review_amount_threshold", e.target.value)}
              />
            </Field>
          </Grid>
          <Flex direction="column" gap="2">
            <label style={{ display: "inline-flex", alignItems: "center", gap: 8, minHeight: 40 }}>
              <Switch checked={form.auto_hold_on_risk} onCheckedChange={(v) => set("auto_hold_on_risk", v)} />
              <Text size="2">命中风险标记时自动挂起，等待人工审核后再发放（关闭则记录标记但直接发放）</Text>
            </label>
            <label style={{ display: "inline-flex", alignItems: "center", gap: 8, minHeight: 40 }}>
              <Switch
                checked={form.require_inviter_email_verified}
                onCheckedChange={(v) => set("require_inviter_email_verified", v)}
              />
              <Text size="2">只有完成邮箱验证的邀请人才能拿奖励</Text>
            </label>
          </Flex>
        </Flex>
      </AdminSection>

      {loadError && <Text color="red" size="2">加载失败：{loadError}</Text>}
      {save.isError && <Text color="red" size="2">{errMsg(save.error)}</Text>}
      {save.isSuccess && !save.isPending && <Text color="green" size="2">规则已保存</Text>}
      <Flex justify="end">
        <Button size="3" loading={save.isPending || isLoading} onClick={() => save.mutate()}>
          保存规则
        </Button>
      </Flex>
    </Flex>
  );
}

// ---- second confirmation for pay / reject / void ----

function ConfirmActionDialog({
  pending,
  onClose,
}: {
  pending: PendingAction | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [note, setNote] = useState("");

  useEffect(() => {
    if (pending) setNote("");
  }, [pending]);

  const run = useMutation({
    mutationFn: () => {
      const p = pending!;
      if (p.kind === "reward") {
        return p.action === "pay"
          ? adminApi.payInviteReward(p.row.id, note.trim() || undefined)
          : adminApi.rejectInviteReward(p.row.id, note.trim() || undefined);
      }
      if (p.kind === "rebate") {
        return p.action === "pay"
          ? adminApi.payInviteRebate(p.row.id)
          : adminApi.rejectInviteRebate(p.row.id);
      }
      return adminApi.voidInviteBinding(p.row.id);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "invite"] });
      onClose();
    },
  });

  if (!pending) {
    return null;
  }

  const inviter = pending.row.inviter_email || pending.row.inviter_user_id.slice(0, 8);
  const invitee = pending.row.invitee_email || pending.row.invitee_user_id.slice(0, 8);
  // Rewards carry `amount`; rebates carry `rebate_amount` and may have had
  // part of it clawed back by a refund, so confirm on what would actually
  // move rather than on the original payout snapshot.
  const payoutAmount = pending.kind === "reward"
    ? (pending.row as InviteReward).amount
    : pending.kind === "rebate"
      ? String(Number((pending.row as InviteRebate).rebate_amount) - Number((pending.row as InviteRebate).clawed_back_amount))
      : "0";

  const title =
    pending.kind === "binding" ? "作废邀请关系"
      : pending.action === "pay"
        ? (pending.kind === "reward" ? "发放拉新奖励" : "发放充值返利")
        : (pending.kind === "reward" ? "拒绝拉新奖励" : "拒绝充值返利");

  const destructive = pending.kind === "binding" || pending.action === "reject";

  return (
    <Dialog.Root open onOpenChange={(v) => !v && !run.isPending && onClose()}>
      <Dialog.Content maxWidth="440px">
        <Dialog.Title>{title}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Dialog.Description size="2" color="gray">
            邀请人 <Text color="gray" weight="medium">{inviter}</Text>，被邀请人 <Text color="gray" weight="medium">{invitee}</Text>
            {pending.kind !== "binding" && (
              <>，金额 <PointAmount amount={payoutAmount} iconSize={12} /></>
            )}
            。
          </Dialog.Description>
          {pending.kind === "binding" && (
            <Text size="2" color="red">作废后该关系不再产生任何奖励和返利，且不可恢复。</Text>
          )}
          {pending.kind === "reward" && (
            <Box>
              <Text as="div" size="2" weight="medium" mb="1">备注（可选）</Text>
              <TextArea value={note} onChange={(e) => setNote(e.target.value)} rows={2} placeholder="记录审核依据，写入操作记录" />
            </Box>
          )}
          {run.isError && <Text size="2" color="red">{errMsg(run.error)}</Text>}
          <Flex gap="2" justify="end">
            <Button size="3" variant="soft" color="gray" disabled={run.isPending} onClick={onClose}>取消</Button>
            <Button
              size="3"
              color={destructive ? "red" : "green"}
              loading={run.isPending}
              onClick={() => run.mutate()}
            >
              确认{pending.kind === "binding" ? "作废" : pending.action === "pay" ? "发放" : "拒绝"}
            </Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function ListLoadError({ label, error }: { label: string; error: unknown }) {
  if (!error) return null;
  return (
    <Text as="div" color="red" size="2" mb="3">
      加载{label}失败：{errMsg(error)}。请点击刷新重试。
    </Text>
  );
}

function SummaryMetric({
  label,
  value,
  detail,
  color,
}: {
  label: string;
  value: ReactNode;
  detail: string;
  color?: "green" | "gray";
}) {
  return (
    <Box style={{ minWidth: 0, padding: "16px 12px" }}>
      <Text as="div" size="1" color="gray">{label}</Text>
      <Text as="div" size="5" weight="medium" color={color} mt="1">{value}</Text>
      <Text as="div" size="1" color="gray" mt="1">{detail}</Text>
    </Box>
  );
}

function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <Box>
      <Text as="div" size="2" weight="medium" mb="1">{label}</Text>
      {children}
      {hint && <Text as="div" size="1" color="gray" mt="1">{hint}</Text>}
    </Box>
  );
}
