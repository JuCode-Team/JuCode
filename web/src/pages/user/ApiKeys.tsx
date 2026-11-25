import "../../personal-responsive.css";
import { useMemo, useState, type ReactNode } from "react";
import { AlertDialog, Box, Button, Dialog, Flex, IconButton, Skeleton, TextArea, Tooltip } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import i18n from "../../i18n";
import { Check, ChevronDown, ChevronRight, Copy, Eye, EyeOff, SquareTerminal, Trash2 } from "lucide-react";
import { accountApi } from "../../api/account";
import { meApi, type ApiKeyView, type PlanUsage } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { copyText } from "../../lib/copy";
import CcSwitchImportDialog, { CreatedKeyGuide, type CcSwitchTarget } from "../../components/CcSwitchImportDialog";
import { ClearableTextField } from "../../components/ClearableTextField";
import GroupAccessPicker from "../../components/GroupAccessPicker";
import { formatPlanDate } from "../../components/KeyAccountStatus";
import { PointAmount } from "../../components/PointAmount";
import {
  AutoRouteFields,
  costLimitPayload,
  emptyCostLimits,
  validCostLimits,
  type CostLimitValues,
  type ModelPinValue,
  type RouteMode,
} from "../../components/ApiKeyFormExtras";
import { formatAmount } from "../../lib/currency";
import { formatDateTime, formatRelative } from "../../lib/formatTime";
import { hasSubscription, keptPins, overlappingModels, type AccessibleGroup, type BillingSource } from "../../lib/groupAccess";
import { queryKeys } from "../../lib/queryClient";
import { useActiveOrg } from "../../hooks/useOrgContext";
import { useAuthStore } from "../../store/auth";
import { useOrgScope } from "../../store/orgContext";
import { PageHeader } from "../../components/PageHeader";
import { HeldBalance } from "../../components/HeldBalance";

// API Keys 页跟随账号上下文：个人上下文列出并创建个人 Key，组织上下文
// 列出并创建组织 Key。归属由上下文锁定，创建/编辑时不再提供"额度来源"
// 选择；每个分组自带计费方式（订阅 = 套餐额度，按量 = 钱包或组织余额），
// Key 能走哪种计费由它绑定的分组决定。组织 owner/admin 能看到全组织的
// Key（含创建者），可改名/限额/删除，但完整密钥仅创建者本人可查看。
//
// 列表是对象行而不是十列表格：一行只放名称、掩码密钥、最近使用三层信息，
// 偏离默认的设置（分组限制 / 限额 / 白名单 / 过期 / 自动路由）以描边 chip
// 出现，默认值不占位。
//
// 套餐额度只对 billing_source = plan_only 的分组生效，而 Key 默认不限分组；
// 有生效套餐的用户需要一把只绑定套餐分组的 Key，页首的引导卡为此服务：
// 一键打开创建对话框，分组选择器落在「订阅」并勾选全部套餐分组。

type KeyFormValues = {
  name: string;
  allowed: string[];
  expiresAt: string;
  ipWhitelist: string;
  autoRoute: boolean;
  autoRouteMode: RouteMode;
  autoRouteMaxRate: string;
  pins: ModelPinValue[];
  costLimits: CostLimitValues;
};

type KeyFormPayload = {
  name: string;
  expires_at: string | null;
  allowed_group_ids: string[];
  ip_whitelist: string[];
  auto_route: boolean;
  auto_route_mode: RouteMode;
  auto_route_max_rate: number;
  model_pins: ModelPinValue[];
  daily_cost_limit: string | null;
  monthly_cost_limit: string | null;
  total_cost_limit: string | null;
};

const emptyForm: KeyFormValues = {
  name: "",
  allowed: [],
  expiresAt: "",
  ipWhitelist: "",
  autoRoute: false,
  autoRouteMode: "price",
  autoRouteMaxRate: "0",
  pins: [],
  costLimits: emptyCostLimits,
};

// 只绑定套餐分组的 Key：调用全部走套餐额度。
function isPlanOnlyKey(k: ApiKeyView, planGroupIds: Set<string>): boolean {
  const allowed = k.allowed_group_ids ?? [];
  return !k.allowed_groups_unrestricted && allowed.length > 0 && allowed.every((id) => planGroupIds.has(id));
}

// 绑定的分组全部停用或删除：列表为空但并非不限分组，Key 当前无法调用。
function groupsAllGone(k: ApiKeyView): boolean {
  return !k.allowed_groups_unrestricted && (k.allowed_group_ids ?? []).length === 0;
}

type PlanPreset = { planName: string; groupIds: string[] };

function formFromKey(k: ApiKeyView): KeyFormValues {
  return {
    name: k.name,
    allowed: k.allowed_group_ids ?? [],
    expiresAt: isoToLocalInput(k.expires_at),
    ipWhitelist: (k.ip_whitelist ?? []).join("\n"),
    autoRoute: k.auto_route,
    autoRouteMode: k.auto_route_mode === "stable" || k.auto_route_mode === "fast" ? k.auto_route_mode : "price",
    autoRouteMaxRate: String(k.auto_route_max_rate ?? 0),
    pins: k.model_pins ?? [],
    costLimits: { daily: k.daily_cost_limit ?? "", monthly: k.monthly_cost_limit ?? "", total: k.total_cost_limit ?? "" },
  };
}

function formToPayload(v: KeyFormValues, groups: AccessibleGroup[]): KeyFormPayload {
  return {
    name: v.name,
    expires_at: localInputToISO(v.expiresAt),
    allowed_group_ids: v.allowed,
    ip_whitelist: parseIPWhitelist(v.ipWhitelist),
    auto_route: v.autoRoute,
    auto_route_mode: v.autoRouteMode,
    auto_route_max_rate: Number(v.autoRouteMaxRate) || 0,
    model_pins: keptPins(v.pins, overlappingModels(groups, v.allowed)),
    ...costLimitPayload(v.costLimits),
  };
}

export default function ApiKeys() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);
  const scope = useOrgScope();
  const activeOrg = useActiveOrg();
  // Manager view spans other members' keys → show the creator.
  const managerView = !!activeOrg?.isManager;
  const { data, isLoading } = useQuery({ queryKey: ["apikeys", scope], queryFn: meApi.apiKeys });
  const { data: groupsData, isLoading: groupsLoading } = useQuery({ queryKey: ["me", "available-groups"], queryFn: meApi.availableGroups });
  // 订阅用量：引导卡需要套餐名，对话框里的账户额度与分组选择器也用它判断有无订阅。
  const usageQ = useQuery({ queryKey: ["me", "usage"], queryFn: meApi.usage });
  const [open, setOpen] = useState(false);
  const [createInitial, setCreateInitial] = useState<KeyFormValues>(emptyForm);
  const [editing, setEditing] = useState<ApiKeyView | null>(null);
  const [secret, setSecret] = useState<string | null>(null);
  // The create form clears itself on success, so remember the name for the
  // "Key 已创建" dialog to label the CC Switch provider with.
  const [createdName, setCreatedName] = useState("");
  // Lifted out of the rows so the CC Switch dialog never nests inside the
  // "Key 已创建" AlertDialog — two stacked Radix modals fight over focus.
  const [ccSwitch, setCcSwitch] = useState<CcSwitchTarget | null>(null);
  const accountOpen = open || editing !== null;
  const profileQ = useQuery({
    queryKey: queryKeys.me.profile,
    queryFn: accountApi.profile,
    enabled: !!user && accountOpen,
  });
  const orgQ = useQuery({
    queryKey: ["me-scoped", activeOrg?.id ?? "", "org-overview"],
    queryFn: () => meApi.orgOverview(activeOrg!.id),
    enabled: accountOpen && !!activeOrg,
  });
  const groups: AccessibleGroup[] = useMemo(() => groupsData ?? [], [groupsData]);

  const createMut = useMutation({
    mutationFn: (v: KeyFormValues) => {
      const p = formToPayload(v, groups);
      return meApi.createApiKey({ ...p, expires_at: p.expires_at ?? undefined });
    },
    onSuccess: (r, v) => {
      setSecret(r.secret);
      setCreatedName(v.name);
      setOpen(false);
      qc.invalidateQueries({ queryKey: ["apikeys"] });
    },
  });

  const updateMut = useMutation({
    mutationFn: ({ id, body }: { id: string; body: KeyFormPayload }) => meApi.updateApiKey(id, body),
    onSuccess: () => {
      setEditing(null);
      qc.invalidateQueries({ queryKey: ["apikeys"] });
    },
  });

  const revokeMut = useMutation({
    mutationFn: (id: string) => meApi.revokeApiKey(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["apikeys"] }),
  });

  const balanceLabel = activeOrg ? t("keys.apiKeys.balanceOrg") : t("keys.apiKeys.balanceWallet");
  const billingScope = activeOrg ? "organization" : "personal";
  // 系统 Key（实验室用）不在这里管理。
  const keys = (data ?? []).filter((k) => !k.system);

  // 套餐只作用于个人 Key；available-groups 只在有生效套餐时返回 plan_only 分组。
  const planGroups = useMemo(() => groups.filter((g) => g.billing_source === "plan_only"), [groups]);
  const plan = activeOrg ? undefined : usageQ.data;
  const planPreset: PlanPreset | undefined = hasSubscription(plan) && planGroups.length > 0
    ? { planName: plan.plan_name || t("keys.apiKeys.planFallback"), groupIds: planGroups.map((g) => g.id) }
    : undefined;
  const planGroupIds = useMemo(() => new Set(planGroups.map((g) => g.id)), [planGroups]);
  const planKey = planPreset ? keys.find((k) => isPlanOnlyKey(k, planGroupIds)) : undefined;

  const openCreate = (preset?: PlanPreset) => {
    createMut.reset();
    setCreateInitial(preset
      ? { ...emptyForm, name: t("keys.apiKeys.planPresetName", { plan: preset.planName }), allowed: preset.groupIds }
      : emptyForm);
    setOpen(true);
  };

  // 分组选择器扣费说明后面接的一小段账户上下文；额度明细在概览页，这里只给名称、到期与余额。
  const planExpire = formatPlanDate(usageQ.data?.expire_at);
  const walletBalance = activeOrg ? orgQ.data?.balance : profileQ.data?.balance;
  const walletHeld = activeOrg ? orgQ.data?.held_balance : profileQ.data?.held_balance;
  const accountContext = {
    plan_only: hasSubscription(usageQ.data)
      ? `${usageQ.data.plan_name || t("keys.apiKeys.subscriptionFallback")}${planExpire ? t("keys.apiKeys.planUntil", { date: planExpire }) : ""}`
      : undefined,
    balance_only: walletBalance != null
      ? <>{activeOrg ? t("keys.apiKeys.orgBalance") : t("keys.apiKeys.walletBalance")} <PointAmount amount={walletBalance} iconSize={12} />{t("keys.apiKeys.heldOpen")}<HeldBalance amount={walletHeld} />{t("keys.apiKeys.heldClose")}</>
      : undefined,
  };
  const dialogShared = {
    groups,
    groupsLoading,
    balanceLabel,
    billingScope,
    plan: usageQ.data,
    planLoading: usageQ.isLoading,
    accountContext,
  } as const;

  return (
    <Box className="jc-api-keys-page">
      <PageHeader
        title={t("keys.apiKeys.title")}
        description={activeOrg
          ? t("keys.apiKeys.descOrg", { org: activeOrg.org?.name ?? t("keys.shared.orgFallback") })
          : t("keys.apiKeys.descPersonal")}
        actions={<Button size="2" variant={planPreset && !planKey ? "soft" : "solid"} onClick={() => openCreate()}>{t("keys.apiKeys.create")}</Button>}
      />

      {planPreset && !isLoading && (planKey ? (
        <div className="jc-plan-key-guide is-done">
          <span>{t("keys.apiKeys.planKeyDone")}<strong>{planKey.name || t("keys.shared.unnamed")}</strong></span>
          <span className="jc-plan-key-guide-sub">{t("keys.apiKeys.planKeyDoneSub", { plan: planPreset.planName })}</span>
        </div>
      ) : (
        <section className="jc-plan-key-guide">
          <div className="jc-plan-key-guide-body">
            <h2>{t("keys.apiKeys.planGuideTitle", { plan: planPreset.planName })}</h2>
            <div className="jc-plan-key-guide-groups">
              <span>{t("keys.apiKeys.planGroups")}</span>
              <span className="jc-fact-chips">
                {planGroups.map((g) => <Chip key={g.id} tone="primary">{g.name}</Chip>)}
              </span>
            </div>
          </div>
          <Button size="2" onClick={() => openCreate(planPreset)}>{t("keys.apiKeys.createPlanKey")}</Button>
        </section>
      ))}

      {open && (
        <KeyFormDialog
          {...dialogShared}
          mode="create"
          initial={createInitial}
          contextNote={activeOrg
            ? t("keys.apiKeys.contextOrg", { org: activeOrg.org?.name ?? t("keys.shared.orgFallback") })
            : t("keys.apiKeys.contextPersonal")}
          saving={createMut.isPending}
          error={createMut.isError ? errMsg(createMut.error) : ""}
          onClose={() => setOpen(false)}
          onSubmit={(v) => createMut.mutate(v)}
        />
      )}

      {secret && (
        <AlertDialog.Root open onOpenChange={() => setSecret(null)}>
          <CreatedKeyGuide
            secret={secret}
                        onImport={() => setCcSwitch({ secret, keyName: createdName })}
          />
        </AlertDialog.Root>
      )}

      <div className="jc-object-list">
        {isLoading && <ListSkeleton />}
        {keys.map((k) => (
          <KeyRow
            key={k.id}
            k={k}
            groups={groups}
            balanceLabel={balanceLabel}
            planOnly={!!planPreset && isPlanOnlyKey(k, planGroupIds)}
            managerView={managerView}
            canReveal={!k.user_id || k.user_id === user?.id}
            revoking={revokeMut.isPending && revokeMut.variables === k.id}
            onRevoke={() => revokeMut.mutate(k.id)}
            onEdit={() => { updateMut.reset(); setEditing(k); }}
            onCcSwitch={(t) => setCcSwitch(t)}
          />
        ))}
        {!isLoading && keys.length === 0 && (
          <div className="jc-object-empty">
            <h3>{activeOrg ? t("keys.apiKeys.emptyOrg") : t("keys.apiKeys.empty")}</h3>
            <p>{t("keys.apiKeys.emptyCopy")}</p>
            <div className="jc-object-empty-actions">
              <Button size="2" variant="soft" onClick={() => openCreate()}>{t("keys.apiKeys.create")}</Button>
            </div>
          </div>
        )}
        {revokeMut.isError && <div className="jc-object-error">{errMsg(revokeMut.error)}</div>}
      </div>

      {editing && (
        <KeyFormDialog
          {...dialogShared}
          mode="edit"
          initial={formFromKey(editing)}
          groupsGone={groupsAllGone(editing)}
          saving={updateMut.isPending}
          error={updateMut.isError ? errMsg(updateMut.error) : ""}
          onClose={() => setEditing(null)}
          onSubmit={(v) => updateMut.mutate({ id: editing.id, body: formToPayload(v, groups) })}
        />
      )}

      <CcSwitchImportDialog target={ccSwitch} onClose={() => setCcSwitch(null)} />
    </Box>
  );
}

function ListSkeleton() {
  return (
    <>
      {Array.from({ length: 3 }, (_, i) => (
        <div key={i} className="jc-object-skeleton-row">
          <Skeleton width="28%" height="16px" style={{ borderRadius: 4 }} />
          <Skeleton width="52%" height="12px" style={{ borderRadius: 4 }} />
        </div>
      ))}
    </>
  );
}

// ---------- 表单对话框 ----------

// 创建与编辑共用一张表单：「名称 / 分组 / 自动路由 / 高级设置」四段；
// 高级设置默认折叠，编辑时若已有任一高级值则展开。扣费方式与账户上下文
// 由 GroupAccessPicker 在订阅 / 按量 tab 下用一行灰字给出。
function KeyFormDialog({
  mode, initial, groups, groupsLoading, balanceLabel, billingScope, plan, planLoading, accountContext,
  groupsGone = false, contextNote, saving, error, onClose, onSubmit,
}: {
  mode: "create" | "edit";
  initial: KeyFormValues;
  groups: AccessibleGroup[];
  groupsLoading: boolean;
  balanceLabel: string;
  billingScope: "personal" | "organization";
  plan?: PlanUsage | null;
  planLoading: boolean;
  accountContext: Partial<Record<BillingSource, ReactNode>>;
  // 编辑一把分组已全部失效的 Key：保持未勾选，并提示当前不可用。
  groupsGone?: boolean;
  contextNote?: string;
  saving: boolean;
  error: string;
  onClose: () => void;
  onSubmit: (v: KeyFormValues) => void;
}) {
  const { t } = useTranslation();
  const [v, setV] = useState<KeyFormValues>(initial);
  const patch = (p: Partial<KeyFormValues>) => setV((s) => ({ ...s, ...p }));
  const hasAdvanced = !!initial.expiresAt || initial.ipWhitelist.trim() !== ""
    || Object.values(initial.costLimits).some((x) => x.trim() !== "");
  const [advancedOpen, setAdvancedOpen] = useState(hasAdvanced);
  const valid = validCostLimits(v.costLimits);

  return (
    <Dialog.Root open onOpenChange={(o) => { if (!o) onClose(); }}>
      <Dialog.Content maxWidth="640px">
        <Dialog.Title>{mode === "create" ? t("keys.apiKeys.create") : t("keys.apiKeys.form.editTitle")}</Dialog.Title>
        {contextNote && <Dialog.Description size="2" color="gray">{contextNote}</Dialog.Description>}
        {mode === "edit" && <Dialog.Description size="2" color="gray">{t("keys.apiKeys.form.editNote")}</Dialog.Description>}

        <div className="jc-form">
          <section className="jc-form-section">
            <Field label={t("keys.apiKeys.form.name")}>
              <ClearableTextField
                placeholder={t("keys.apiKeys.form.namePlaceholder")}
                value={v.name}
                onChange={(e) => patch({ name: e.target.value })}
                autoFocus={mode === "create"}
              />
            </Field>
          </section>

          <section className="jc-form-section">
            {groupsGone && v.allowed.length === 0 && (
              <span className="jc-field-warn">{t("keys.apiKeys.form.groupsGone")}</span>
            )}
            <GroupAccessPicker
              groups={groups}
              value={v.allowed}
              onChange={(ids) => setV((s) => ({ ...s, allowed: ids, pins: keptPins(s.pins, overlappingModels(groups, ids)) }))}
              balanceLabel={balanceLabel}
              loading={groupsLoading}
              scope={billingScope}
              plan={plan}
              planLoading={planLoading}
              emptySummary={groupsGone && v.allowed.length === 0 ? "" : undefined}
              context={accountContext}
            />
          </section>

          <section className="jc-form-section">
            <AutoRouteFields
              checked={v.autoRoute}
              onCheckedChange={(autoRoute) => patch({ autoRoute })}
              mode={v.autoRouteMode}
              onModeChange={(autoRouteMode) => patch({ autoRouteMode })}
              maxRate={v.autoRouteMaxRate}
              onMaxRateChange={(autoRouteMaxRate) => patch({ autoRouteMaxRate })}
              groups={groups}
              selectedIds={v.allowed}
              pins={v.pins}
              onPinsChange={(pins) => patch({ pins })}
            />
          </section>

          <section className="jc-form-section">
            <button
              type="button"
              className="jc-form-toggle"
              aria-expanded={advancedOpen}
              onClick={() => setAdvancedOpen((s) => !s)}
            >
              {advancedOpen ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
              {t("keys.apiKeys.form.advanced")}
              <small>{t("keys.apiKeys.form.advancedSub")}</small>
            </button>
            {advancedOpen && (
              <div className="jc-form-section" style={{ paddingTop: 4 }}>
                <Field label={t("keys.apiKeys.form.costLimit")} hint={t("keys.apiKeys.form.costLimitHint")}>
                  <div className="jc-field-row">
                    {(["daily", "monthly", "total"] as const).map((key) => ({ key, label: t(`keys.apiKeys.period.${key}`) })).map(({ key, label }) => (
                      <ClearableTextField
                        key={key}
                        type="number"
                        min="0"
                        step="any"
                        placeholder={label}
                        aria-label={t("keys.apiKeys.form.limitAria", { label })}
                        value={v.costLimits[key]}
                        onChange={(e) => patch({ costLimits: { ...v.costLimits, [key]: e.target.value } })}
                      />
                    ))}
                  </div>
                </Field>
                <Field label={t("keys.apiKeys.form.expires")} hint={t("keys.apiKeys.form.expiresHint")}>
                  <ClearableTextField type="datetime-local" value={v.expiresAt} onChange={(e) => patch({ expiresAt: e.target.value })} />
                </Field>
                <Field label={t("keys.apiKeys.form.ip")} hint={t("keys.apiKeys.form.ipHint")}>
                  <TextArea
                    className="jc-mono-input"
                    rows={3}
                    placeholder={"203.0.113.7\n10.0.0.0/8"}
                    value={v.ipWhitelist}
                    onChange={(e) => patch({ ipWhitelist: e.target.value })}
                  />
                </Field>
              </div>
            )}
          </section>

          {error && <div className="jc-form-error">{error}</div>}
          <div className="jc-form-footer">
            <Button variant="soft" color="gray" onClick={onClose}>{t("keys.shared.cancel")}</Button>
            <Button disabled={!valid} loading={saving} onClick={() => onSubmit(v)}>
              {mode === "create" ? t("keys.apiKeys.create") : t("keys.shared.save")}
            </Button>
          </div>
        </div>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="jc-field">
      <span className="jc-field-label">{label}</span>
      {children}
      {hint && <span className="jc-field-hint">{hint}</span>}
    </label>
  );
}

// datetime-local ↔ ISO helpers for the expiry field. Empty string means
// "never expires" on both sides.
function isoToLocalInput(iso?: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
function localInputToISO(v: string): string | null {
  if (!v.trim()) return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}
function parseIPWhitelist(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

function costLimitUsage(k: ApiKeyView) {
  return ([
    [i18n.t("keys.apiKeys.period.daily"), k.daily_cost_used, k.daily_cost_limit],
    [i18n.t("keys.apiKeys.period.monthly"), k.monthly_cost_used, k.monthly_cost_limit],
    [i18n.t("keys.apiKeys.period.total"), k.total_cost_used, k.total_cost_limit],
  ] as const).filter((x) => x[2] !== null).map(([label, used, limit]) => ({ label, used, limit: limit as string }));
}

// 分组摘要：按计费方式计数，已不可访问的单列。
function accessLabel(k: ApiKeyView, groups: AccessibleGroup[], balanceLabel: string): { text: string; missing: number } {
  const ids = k.allowed_group_ids ?? [];
  const byId = new Map(groups.map((group) => [group.id, group]));
  let plan = 0;
  let balance = 0;
  let missing = 0;
  for (const id of ids) {
    const group = byId.get(id);
    if (!group) missing += 1;
    else if (group.billing_source === "plan_only") plan += 1;
    else balance += 1;
  }
  if (ids.length === 1 && missing === 0) return { text: i18n.t("keys.apiKeys.access.only", { name: byId.get(ids[0])!.name }), missing };
  const parts: string[] = [];
  if (plan) parts.push(i18n.t("keys.apiKeys.access.plan", { n: plan }));
  if (balance) parts.push(i18n.t("keys.apiKeys.access.balance", { label: balanceLabel, n: balance }));
  if (missing) parts.push(i18n.t("keys.apiKeys.access.missing", { n: missing }));
  return { text: parts.join(" · ") || i18n.t("keys.apiKeys.access.groups", { n: ids.length }), missing };
}

// 路由摘要：策略 + 倍率上限 + 指定模型数。
function routeLabel(k: ApiKeyView): string {
  const modeKey = k.auto_route_mode === "stable" ? "stable" : k.auto_route_mode === "fast" ? "fast" : "price";
  const mode = i18n.t(`keys.apiKeys.route.${modeKey}`);
  const cap = modeKey === "price" && k.auto_route_max_rate > 0 ? ` ≤ ${k.auto_route_max_rate}×` : "";
  const pinned = (k.model_pins?.length ?? 0) > 0 ? i18n.t("keys.apiKeys.route.pinned", { n: k.model_pins?.length }) : "";
  return `${mode}${cap}${pinned}`;
}

// ---------- 列表行 ----------

function Chip({ tone = "neutral", title, children }: { tone?: "neutral" | "primary" | "info" | "success" | "warn" | "danger"; title?: string; children: ReactNode }) {
  return <span className="jc-fact-chip" data-tone={tone} title={title}>{children}</span>;
}

// 只在值偏离默认时出现的设置 chip。
function KeyFacts({ k, groups, balanceLabel }: { k: ApiKeyView; groups: AccessibleGroup[]; balanceLabel: string }) {
  const { t } = useTranslation();
  const [now] = useState(() => Date.now());
  const chips: ReactNode[] = [];
  const allowed = k.allowed_group_ids ?? [];
  if (groupsAllGone(k)) {
    chips.push(<Chip key="groups" tone="danger" title={t("keys.apiKeys.chip.groupsGoneTitle")}>{t("keys.apiKeys.chip.groupsGone")}</Chip>);
  } else if (!k.allowed_groups_unrestricted && allowed.length > 0) {
    const names = allowed.map((id) => groups.find((g) => g.id === id)?.name ?? id.slice(0, 6));
    const access = accessLabel(k, groups, balanceLabel);
    chips.push(
      <Chip key="groups" tone={access.missing > 0 ? "warn" : "neutral"} title={names.join(t("keys.shared.listSep"))}>
        {access.text}
      </Chip>,
    );
  }
  if (k.auto_route) {
    chips.push(<Chip key="route" tone="info" title={t("keys.apiKeys.chip.routeTitle")}>{t("keys.apiKeys.chip.route", { label: routeLabel(k) })}</Chip>);
  }
  for (const { label, used, limit } of costLimitUsage(k)) {
    const ratio = Number(used) / Math.max(Number(limit), Number.EPSILON);
    chips.push(
      <Chip key={label} tone={ratio >= 1 ? "danger" : ratio >= 0.9 ? "warn" : "neutral"} title={t("keys.apiKeys.chip.limitTitle", { label, used: formatAmount(used, { decimals: "preserve" }), limit: formatAmount(limit, { decimals: "preserve" }) })}>
        {label} {formatAmount(used)} / {formatAmount(limit, { decimals: "preserve" })}
      </Chip>,
    );
  }
  const ips = k.ip_whitelist ?? [];
  if (ips.length > 0) {
    chips.push(<Chip key="ip" title={ips.join(t("keys.shared.listSep"))}>{t("keys.apiKeys.chip.ip", { n: ips.length })}</Chip>);
  }
  if (k.expires_at) {
    const expired = new Date(k.expires_at).getTime() <= now;
    chips.push(
      <Chip key="exp" tone={expired ? "danger" : "neutral"} title={formatDateTime(k.expires_at, "full")}>
        {expired ? t("keys.shared.expired") : t("keys.apiKeys.chip.expiresOn", { date: formatDateTime(k.expires_at, "date") })}
      </Chip>,
    );
  }
  if (chips.length === 0) return null;
  return <span className="jc-fact-chips">{chips}</span>;
}

// KeyRow 自持 reveal/copy 状态，避免无关的 refetch 冲掉已显示的完整密钥。
// canReveal=false（管理员看其他成员的 Key）时隐藏查看/复制/导入。
function KeyRow({
  k, groups, balanceLabel, planOnly, managerView, canReveal, revoking, onRevoke, onEdit, onCcSwitch,
}: {
  k: ApiKeyView;
  groups: AccessibleGroup[];
  balanceLabel: string;
  planOnly: boolean;
  managerView: boolean;
  canReveal: boolean;
  revoking: boolean;
  onRevoke: () => void;
  onEdit: () => void;
  onCcSwitch: (target: CcSwitchTarget) => void;
}) {
  const { t } = useTranslation();
  const [secret, setSecret] = useState<string | null>(null);
  const [revealErr, setRevealErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [copying, setCopying] = useState(false);

  const reveal = useMutation({
    mutationFn: () => meApi.revealApiKey(k.id),
    onSuccess: (s) => { setSecret(s); setRevealErr(null); },
    onError: (e: Error) => setRevealErr(errMsg(e)),
  });

  const doCopy = async () => {
    let value = secret;
    if (!value) {
      setCopying(true);
      try {
        value = await meApi.revealApiKey(k.id);
        setRevealErr(null);
      } catch (e) {
        setRevealErr(errMsg(e as Error));
        setCopying(false);
        return;
      }
      setCopying(false);
    }
    if (value) {
      const ok = await copyText(value);
      if (ok) {
        setCopied(true);
        window.setTimeout(() => setCopied(false), 1500);
      }
    }
  };

  const displayName = k.name || t("keys.shared.unnamed");

  return (
    <div className="jc-object-row">
      <div className="jc-object-main">
        <div className="jc-object-title">
          <strong>{displayName}</strong>
          {planOnly && <Chip tone="primary" title={t("keys.apiKeys.chip.planTitle")}>{t("keys.apiKeys.chip.plan")}</Chip>}
          <KeyFacts k={k} groups={groups} balanceLabel={balanceLabel} />
        </div>
        <div className="jc-object-sub">
          <span className="jc-key-secret">
            <code className={secret ? "is-revealed" : undefined}>{secret ? secret : `${k.prefix}…`}</code>
            {canReveal ? (
              <>
                <Tooltip content={copied ? t("keys.shared.copied") : t("keys.apiKeys.row.copyFull")}>
                  <IconButton size="1" variant="ghost" color={copied ? "green" : "gray"} loading={copying} onClick={doCopy} aria-label={t("keys.apiKeys.row.copyFull")}>
                    {copied ? <Check size={13} /> : <Copy size={13} />}
                  </IconButton>
                </Tooltip>
                {secret ? (
                  <Tooltip content={t("keys.apiKeys.row.hide")}>
                    <IconButton size="1" variant="ghost" color="gray" onClick={() => setSecret(null)} aria-label={t("keys.apiKeys.row.hideFull")}><EyeOff size={13} /></IconButton>
                  </Tooltip>
                ) : (
                  <Tooltip content={revealErr || t("keys.apiKeys.row.showFull")}>
                    <IconButton size="1" variant="ghost" color={revealErr ? "amber" : "gray"} loading={reveal.isPending} onClick={() => reveal.mutate()} aria-label={t("keys.apiKeys.row.showFull")}>
                      <Eye size={13} />
                    </IconButton>
                  </Tooltip>
                )}
              </>
            ) : (
              <Chip title={t("keys.apiKeys.row.creatorOnlyTitle")}>{t("keys.apiKeys.row.creatorOnly")}</Chip>
            )}
          </span>
          {managerView && (k.user_email || k.user_id) && <span>{t("keys.apiKeys.row.creator", { who: k.user_email || k.user_id })}</span>}
          <span title={k.last_used_at ? formatDateTime(k.last_used_at, "full") : undefined}>
            {k.last_used_at ? t("keys.shared.lastUsed", { when: formatRelative(k.last_used_at) }) : t("keys.shared.neverUsed")}
          </span>
          <span title={formatDateTime(k.created_at, "full")}>{t("keys.apiKeys.row.created", { date: formatDateTime(k.created_at, "date") })}</span>
        </div>
      </div>

      <div className="jc-object-actions">
        {/* Needs the plaintext key, so it follows the same rule as reveal/copy. */}
        {canReveal && (
          <Button size="1" variant="soft" color="gray" onClick={() => onCcSwitch({ keyId: k.id, keyName: k.name, secret })}>
            <SquareTerminal size={13} /> {t("keys.apiKeys.row.ccSwitch")}
          </Button>
        )}
        <Button size="1" variant="soft" color="gray" onClick={onEdit}>{t("keys.shared.edit")}</Button>
        <AlertDialog.Root>
          <Tooltip content={t("keys.shared.delete")}>
            <AlertDialog.Trigger>
              <IconButton size="1" variant="ghost" color="red" loading={revoking} aria-label={t("keys.apiKeys.row.deleteAria", { name: displayName })}>
                <Trash2 size={14} />
              </IconButton>
            </AlertDialog.Trigger>
          </Tooltip>
          <AlertDialog.Content maxWidth="440px">
            <AlertDialog.Title>{t("keys.apiKeys.row.deleteTitle", { name: displayName })}</AlertDialog.Title>
            <AlertDialog.Description size="2" color="gray">
              {t("keys.apiKeys.row.deleteDesc")}
            </AlertDialog.Description>
            <Flex gap="2" justify="end" mt="4">
              <AlertDialog.Cancel><Button variant="soft" color="gray">{t("keys.shared.cancel")}</Button></AlertDialog.Cancel>
              <AlertDialog.Action><Button color="red" onClick={onRevoke}>{t("keys.shared.delete")}</Button></AlertDialog.Action>
            </Flex>
          </AlertDialog.Content>
        </AlertDialog.Root>
      </div>
    </div>
  );
}
