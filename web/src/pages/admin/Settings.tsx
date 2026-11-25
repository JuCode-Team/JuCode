import { useCallback, useDeferredValue, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Badge, Box, Button, Callout, Card, Checkbox, Flex, IconButton, Select, Switch, Text, TextArea } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { BellRing, Brain, ChevronDown, ChevronUp, Coins, CreditCard, Globe, Link2, Mail, Megaphone, Plus, Route, ShieldAlert, ShieldCheck, Trash2, Upload, UserPlus, X } from "lucide-react";
import { adminApi, type Announcement, type OpsAlertWebhookType, type ReasoningParseRule, type SiteSettings } from "../../api/endpoints";
import { apiAssetURL, errMsg } from "../../api/client";
import { useSiteStore } from "../../store/site";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminPage } from "../../components/AdminPage";
import { SortableItem, SortableList } from "../../components/SortableList";
import { Field, FieldGrid, FormSection, SaveBar, ToggleRow } from "../../components/form/FormLayout";

// Settings is one form split across twelve panes. A side navigation (grouped
// 站点 / 接入 / 网关 / 运维) replaces the former horizontal tab strip, which
// clipped past the ninth tab. Panes share a single form state object —
// switching never discards unsaved edits — and the sticky SaveBar at the
// bottom commits every pane at once.

const empty: SiteSettings = {
  site_name: "",
  site_description: "",
  site_url: "",
  site_logo: "",
  icp_number: "",
  contact_email: "",
  contact_group_label: "QQ 群",
  contact_group: "",
  contact_group_qr_url: "",
  contact_dock_enabled: false,
  marketing_banner: {
    enabled: false,
    text: "",
    link_url: "",
  },
  announcement_enabled: false,
  announcement_display_mode: "always",
  announcement_title: "",
  announcement_content: "",
  announcements: [],
  endpoint_openai_responses: "",
  endpoint_openai_compat: "",
  endpoint_anthropic: "",
  registration_enabled: true,
  registration_requires_approval: false,
  payment_alipay_enabled: true,
  topup_presets: [50, 100, 200, 500, 1000],
  self_refund_enabled: false,
  refund_auto_approve_enabled: false,
  refund_auto_approve_max: "",
  sticky_ttl_seconds: 1800,
  status_page_group_ids: [],
  status_page_window_hours: 24,
  status_page_upstream_enabled: true,
  ops_alert_enabled: false,
  ops_alert_webhook_type: "generic",
  ops_alert_webhook_url: "",
  ops_alert_webhook_url_set: false,
  ops_alert_webhook_secret: "",
  ops_alert_webhook_secret_set: false,
  ops_alert_timeout_seconds: 5,
  ops_alert_max_retries: 2,
  ops_alert_silence_seconds: 300,
  smtp_host: "",
  smtp_port: 587,
  smtp_username: "",
  smtp_password: "",
  smtp_password_set: false,
  smtp_from_email: "",
  smtp_from_name: "",
  smtp_use_tls: false,
  require_register_verification: false,
  require_change_email_verification: false,
  allow_password_reset: false,
  allow_image_output: false,
  format_conversion_enabled: false,
  websocket_enabled: true,
  stream_first_text_timeout_seconds: 20,
  moderation_enabled: false,
  moderation_base_url: "https://api.openai.com/v1",
  moderation_model: "omni-moderation-latest",
  moderation_api_key: "",
  moderation_api_key_set: false,
  moderation_threshold: 0.5,
  moderation_sample_rate: 0,
  moderation_max_per_min: 60,
  exchange_cny_per_usd: 7.2,
  max_incentive_pct: 30,
  agent_plan_refund_day_divisor: 30,
  agent_plan_refund_rate_pct: 70,
  agent_balance_refund_rate_pct: 90,
  reasoning_parse_rules: [],
};

// Mirrors the backend's DefaultReasoningParseRules() — used by the
// "恢复默认" button in the reasoning pane.
const defaultReasoningRules: ReasoningParseRule[] = [
  { path: "reasoning.effort", kind: "string" },
  { path: "reasoning_effort", kind: "string" },
  // budget_tokens 记录原始数值（thinking:16384）——思考 tokens 数量直接
  // 展示给用户，不做区间分档。
  { path: "thinking.budget_tokens", kind: "number", label: "thinking" },
];

const MAX_LOGO_BYTES = 256 * 1024; // 256 KB — keep DB row small

// Sub-field setter type, shared by every pane component below so they
// all mutate the same parent form state.
type Setter = <K extends keyof SiteSettings>(k: K, v: SiteSettings[K]) => void;

type PaneId =
  | "site" | "announcement" | "endpoints"
  | "auth" | "payment" | "currency"
  | "gateway" | "reasoning" | "moderation"
  | "status" | "opsalert" | "mail";

interface Pane {
  id: PaneId;
  label: string;
  // One-line summary shown under the pane title.
  summary: string;
  icon: ReactNode;
}

const NAV_GROUPS: { title: string; panes: Pane[] }[] = [
  {
    title: "站点",
    panes: [
      { id: "site", label: "站点信息", summary: "名称、Logo、域名、备案与页面顶部横幅。", icon: <Globe size={14} /> },
      { id: "announcement", label: "公告", summary: "前台弹窗公告的开关与内容。", icon: <Megaphone size={14} /> },
      { id: "endpoints", label: "接入端点", summary: "公开「接入指南」页展示的 base URL。", icon: <Link2 size={14} /> },
    ],
  },
  {
    title: "接入",
    panes: [
      { id: "auth", label: "注册与登录", summary: "注册开关、审核与邮箱验证。", icon: <UserPlus size={14} /> },
      { id: "payment", label: "支付通道", summary: "收款渠道开关、充值档位，以及自助退款和退款自动审批。", icon: <CreditCard size={14} /> },
      { id: "currency", label: "渠道结算", summary: "上游美元成本折算为积分的汇率。", icon: <Coins size={14} /> },
    ],
  },
  {
    title: "网关",
    panes: [
      { id: "gateway", label: "网关行为", summary: "粘性会话、超时与协议能力。", icon: <Route size={14} /> },
      { id: "reasoning", label: "思考解析", summary: "从请求体识别思考强度并记入用量日志。", icon: <Brain size={14} /> },
      { id: "moderation", label: "内容审核", summary: "异步调用审核接口复核请求内容。", icon: <ShieldAlert size={14} /> },
    ],
  },
  {
    title: "运维",
    panes: [
      { id: "status", label: "状态页", summary: "公开 /status 页统计的模型组与窗口。", icon: <ShieldCheck size={14} /> },
      { id: "opsalert", label: "运维告警", summary: "事件与健康翻转外发到 webhook。", icon: <BellRing size={14} /> },
      { id: "mail", label: "邮件 SMTP", summary: "系统邮件的发信服务器与发件人。", icon: <Mail size={14} /> },
    ],
  },
];

const PANES: Record<PaneId, Pane> = Object.fromEntries(
  NAV_GROUPS.flatMap((g) => g.panes).map((p) => [p.id, p]),
) as Record<PaneId, Pane>;

export default function Settings() {
  const qc = useQueryClient();
  const refreshSite = useSiteStore((s) => s.refresh);

  const { data, isLoading } = useQuery({
    queryKey: ["admin", "settings"],
    queryFn: adminApi.getSettings,
  });

  const [form, setForm] = useState<SiteSettings>(empty);
  const [pane, setPane] = useState<PaneId>("site");
  useEffect(() => { if (data) setForm(data); }, [data]);

  const save = useMutation({
    // The agent settings are edited on 代理结算, not here: take the stored
    // values so a stale form cannot revert them.
    mutationFn: async (s: SiteSettings) => {
      const current = await adminApi.getSettings();
      return adminApi.updateSettings({
        ...s,
        max_incentive_pct: current.max_incentive_pct,
        agent_plan_refund_day_divisor: current.agent_plan_refund_day_divisor,
        agent_plan_refund_rate_pct: current.agent_plan_refund_rate_pct,
        agent_balance_refund_rate_pct: current.agent_balance_refund_rate_pct,
      });
    },
    onSuccess: (s) => {
      setForm(s);
      qc.invalidateQueries({ queryKey: ["admin", "settings"] });
      refreshSite();
    },
  });

  // Stable across renders — children that capture `set` (especially
  // memoised panes) shouldn't re-render just because the parent did.
  const set: Setter = useCallback((k, v) => setForm((f) => ({ ...f, [k]: v })), []);

  // Dirty check used to do JSON.stringify(form) twice per render. With
  // site_logo holding a base64 payload up to 256 KB that blows past
  // 100 ms of main-thread work per keystroke — INP climbs to 500 ms+
  // when any input is typed quickly. Fix is two parts:
  //   1. compare site_logo (the big field) by reference only — admin
  //      either swaps the whole string or doesn't touch it; partial
  //      edits don't happen on a data URL.
  //   2. defer the rest behind useDeferredValue so the typing path
  //      commits the value first and the badge updates one frame later.
  const deferredForm = useDeferredValue(form);
  const dirty = useMemo(() => isDirty(deferredForm, data), [deferredForm, data]);

  if (isLoading) return <RouteSkeleton />;

  const current = PANES[pane];

  return (
    <AdminPage
      title="系统设置"
      description="按类别管理站点、接入端点、注册策略、网关行为、状态页、运维告警、渠道结算和邮件配置。"
    >
      <Card>
        <div className="jc-settings-layout">
          <nav className="jc-settings-nav" aria-label="设置分类">
            {NAV_GROUPS.map((g) => (
              <Box key={g.title}>
                <div className="jc-settings-nav-group">{g.title}</div>
                {g.panes.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    className="jc-settings-nav-item"
                    aria-current={pane === p.id ? "true" : undefined}
                    onClick={() => setPane(p.id)}
                  >
                    {p.icon}
                    {p.label}
                  </button>
                ))}
              </Box>
            ))}
          </nav>

          <div className="jc-settings-content">
            <div className="jc-settings-content-head">
              <h2 className="jc-settings-content-title">{current.label}</h2>
              <p className="jc-settings-content-sub">{current.summary}</p>
            </div>
            {pane === "site" && <SitePane form={form} set={set} />}
            {pane === "announcement" && <AnnouncementPane form={form} set={set} />}
            {pane === "endpoints" && <EndpointsPane form={form} set={set} />}
            {pane === "auth" && <AuthPane form={form} set={set} />}
            {pane === "gateway" && <GatewayPane form={form} set={set} />}
            {pane === "payment" && <PaymentPane form={form} set={set} />}
            {pane === "status" && <StatusPagePane form={form} set={set} />}
            {pane === "opsalert" && <OpsAlertPane form={form} set={set} />}
            {pane === "reasoning" && <ReasoningPane form={form} set={set} />}
            {pane === "moderation" && <ModerationPane form={form} set={set} />}
            {pane === "currency" && <CurrencyPane form={form} set={set} />}
            {pane === "mail" && <MailPane form={form} set={set} />}
          </div>
        </div>
      </Card>

      <SaveBar
        error={save.isError ? errMsg(save.error) : undefined}
        status={
          dirty ? (
            <Badge color="amber" variant="soft">有未保存的修改</Badge>
          ) : save.isSuccess ? (
            <Text size="2" color="green">已保存。</Text>
          ) : (
            <Text size="2" color="gray">所有修改已保存</Text>
          )
        }
        actions={
          <>
            <Button variant="soft" color="gray" onClick={() => data && setForm(data)} disabled={!data || !dirty}>
              放弃修改
            </Button>
            <Button onClick={() => save.mutate(form)} loading={save.isPending} disabled={!dirty}>
              保存全部
            </Button>
          </>
        }
      />
    </AdminPage>
  );
}

// isDirty compares the edited form against the server snapshot without
// stringifying the heavy site_logo field. site_logo is either equal by
// reference (no change) or replaced wholesale (upload / paste / clear),
// so identity comparison is sufficient and skips a 200KB+ scan per
// keystroke. Other fields stringify cheaply (a few hundred chars total).
function isDirty(form: SiteSettings, data: SiteSettings | undefined): boolean {
  if (!data) return false;
  if (form.site_logo !== data.site_logo) return true;
  const a = { ...form, site_logo: "" };
  const b = { ...data, site_logo: "" };
  return JSON.stringify(a) !== JSON.stringify(b);
}

// Write-only secrets (SMTP password, webhook URL, moderation key) come back
// from the server as a presence flag only. The field keeps its plain label
// and the hint carries the state.
function secretHint(set: boolean | undefined, base?: string): string {
  const keep = set ? "已配置。留空保持不变，填入新值即覆盖。" : "加密存储，保存后不再回显。";
  return base ? `${base} ${keep}` : keep;
}

// ---------------- pane: site ----------------

// Settings for the bottom-right contact dock on the public site. The QR image
// is uploaded to /v1/admin/site-assets; the returned URL (or a pasted external
// URL) is what gets saved.
function ContactDockSection({ form, set }: { form: SiteSettings; set: Setter }) {
  const fileRef = useRef<HTMLInputElement | null>(null);
  const upload = useMutation({
    mutationFn: (file: File) => adminApi.uploadSiteAsset(file),
    onSuccess: (r) => set("contact_group_qr_url", r.url),
  });
  const group = form.contact_group;
  const qr = form.contact_group_qr_url;

  return (
    <FormSection title="联系浮层" description="首页右下角的「联系」按钮及其弹出卡片，卡片内同时展示上方的联系邮箱。">
      <div className="jc-toggle-rows">
        <ToggleRow
          checked={form.contact_dock_enabled}
          onCheckedChange={(v) => set("contact_dock_enabled", v)}
          label="显示右下角联系按钮"
          hint="关闭后首页不显示联系按钮和「加入群获取支持」入口。"
        />
      </div>
      <FieldGrid>
        <Field label="群名称" code="contact_group_label" hint="卡片和首页入口显示的名称，例如「QQ 群」「微信群」。">
          <ClearableTextField
            value={form.contact_group_label}
            onChange={(e) => set("contact_group_label", e.target.value)}
            placeholder="QQ 群"
          />
        </Field>
        <Field label="群号" code="contact_group" hint="留空则卡片不显示群号。">
          <ClearableTextField
            value={group}
            onChange={(e) => set("contact_group", e.target.value.trim())}
            placeholder="例如 123456789"
          />
        </Field>
        <Field
          label="群二维码"
          hint="上传 png / jpg / webp（不超过 2 MB），或粘贴图片 URL。留空则显示「二维码待提供」。"
          error={upload.isError ? errMsg(upload.error) : null}
          span="full"
        >
          <Flex gap="3" align="center" wrap="wrap">
            <Box
              style={{
                width: 96, height: 96, borderRadius: "var(--radius-control)",
                border: "1px solid var(--c-outline-variant)",
                background: "var(--c-surface-container-low)",
                display: "flex", alignItems: "center", justifyContent: "center",
                flexShrink: 0, overflow: "hidden",
              }}
            >
              {qr ? (
                <img src={apiAssetURL(qr)} alt="群二维码预览" style={{ width: "100%", height: "100%", objectFit: "contain" }} />
              ) : (
                <Text size="1" color="gray">无</Text>
              )}
            </Box>
            <Box style={{ flex: 1, minWidth: 200 }}>
              <ClearableTextField
                placeholder="https://…"
                value={qr}
                onChange={(e) => set("contact_group_qr_url", e.target.value)}
              />
            </Box>
            <input
              ref={fileRef}
              type="file"
              accept="image/png,image/jpeg,image/webp"
              style={{ display: "none" }}
              onChange={(e) => {
                const f = e.target.files?.[0];
                if (f) upload.mutate(f);
                e.target.value = "";
              }}
            />
            <Button variant="soft" color="gray" loading={upload.isPending} onClick={() => fileRef.current?.click()}>
              <Upload size={14} /> 上传
            </Button>
          </Flex>
        </Field>
      </FieldGrid>
    </FormSection>
  );
}

function SitePane({ form, set }: { form: SiteSettings; set: Setter }) {
  const [logoErr, setLogoErr] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const marketingBanner = form.marketing_banner ?? { enabled: false, text: "", link_url: "" };
  const updateMarketingBanner = (patch: Partial<typeof marketingBanner>) => {
    set("marketing_banner", { ...marketingBanner, ...patch });
  };

  const onPickFile = (file: File) => {
    setLogoErr(null);
    if (!file.type.startsWith("image/")) {
      setLogoErr("仅支持图片文件");
      return;
    }
    if (file.size > MAX_LOGO_BYTES) {
      setLogoErr(`文件超过 ${(MAX_LOGO_BYTES / 1024).toFixed(0)} KB（${(file.size / 1024).toFixed(1)} KB）`);
      return;
    }
    const r = new FileReader();
    r.onload = () => set("site_logo", String(r.result));
    r.onerror = () => setLogoErr("读取文件失败");
    r.readAsDataURL(file);
  };

  return (
    <>
      <FormSection title="站点标识" description="出现在导航栏、浏览器标签与页脚的基础信息。">
        <FieldGrid>
          <Field label="站点名称" hint="出现在导航栏与浏览器标签栏。">
            <ClearableTextField
              value={form.site_name}
              onChange={(e) => set("site_name", e.target.value)}
              placeholder="JuCode"
            />
          </Field>
          <Field label="联系邮箱" hint="页脚的支持邮箱。">
            <ClearableTextField
              type="email"
              value={form.contact_email}
              onChange={(e) => set("contact_email", e.target.value)}
              placeholder="support@example.com"
            />
          </Field>
          <Field label="站点地址" hint="对外公布的主域名，含 https://。">
            <ClearableTextField
              type="url"
              value={form.site_url}
              onChange={(e) => set("site_url", e.target.value)}
              placeholder="https://jucode.example.com"
            />
          </Field>
          <Field label="备案号" hint="ICP 备案号，留空则页脚不显示。">
            <ClearableTextField
              value={form.icp_number}
              onChange={(e) => set("icp_number", e.target.value)}
              placeholder="ICP 备 12345678 号"
            />
          </Field>
          <Field label="站点介绍" hint="一句话描述，显示在页脚与首页。" span="full">
            <TextArea
              value={form.site_description}
              onChange={(e) => set("site_description", e.target.value)}
              placeholder="AI 编程平台 · 统一 LLM 网关"
              rows={2}
            />
          </Field>
          <Field
            label="站点 Logo"
            hint="粘贴图片 URL，或上传不超过 256 KB 的图片（自动转 base64 存储）。留空使用首字母徽章。"
            error={logoErr}
            span="full"
          >
            <Flex gap="3" align="center" wrap="wrap">
              <Box
                style={{
                  width: 40, height: 40, borderRadius: "var(--radius-control)",
                  border: "1px solid var(--c-outline-variant)",
                  background: "var(--c-surface-container-low)",
                  display: "flex", alignItems: "center", justifyContent: "center",
                  flexShrink: 0, overflow: "hidden",
                }}
              >
                {form.site_logo ? (
                  <img
                    src={form.site_logo}
                    alt="logo preview"
                    style={{ maxWidth: "100%", maxHeight: "100%", objectFit: "contain" }}
                  />
                ) : (
                  <Text size="1" color="gray">无</Text>
                )}
              </Box>
              <Box style={{ flex: 1, minWidth: 200 }}>
                <ClearableTextField
                  placeholder="https://…"
                  value={form.site_logo}
                  onChange={(e) => set("site_logo", e.target.value)}
                />
              </Box>
              <input
                ref={fileRef}
                type="file"
                accept="image/*"
                style={{ display: "none" }}
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) onPickFile(f);
                  e.target.value = "";
                }}
              />
              <Button variant="soft" color="gray" onClick={() => fileRef.current?.click()}>
                <Upload size={14} /> 上传
              </Button>
              {form.site_logo && (
                <IconButton variant="soft" color="gray" onClick={() => set("site_logo", "")} title="清除 Logo" aria-label="清除 Logo">
                  <X size={14} />
                </IconButton>
              )}
            </Flex>
          </Field>
        </FieldGrid>
      </FormSection>

      <ContactDockSection form={form} set={set} />

      <FormSection
        title="营销横幅"
        description="显示在页面顶部、导航栏上方。关闭或内容为空时不显示。"
        actions={
          <Switch
            checked={marketingBanner.enabled}
            onCheckedChange={(enabled) => updateMarketingBanner({ enabled })}
            aria-label="启用营销横幅"
          />
        }
      >
        <FieldGrid columns={1}>
          <Field label="横幅内容">
            <TextArea
              value={marketingBanner.text}
              onChange={(e) => updateMarketingBanner({ text: e.target.value })}
              placeholder="请输入需要展示的内容"
              rows={2}
            />
          </Field>
          <Field label="跳转链接" hint="留空时横幅仅展示内容；填写后点击横幅在新窗口打开链接。">
            <ClearableTextField
              type="url"
              value={marketingBanner.link_url}
              onChange={(e) => updateMarketingBanner({ link_url: e.target.value })}
              placeholder="https://example.com"
            />
          </Field>
        </FieldGrid>
      </FormSection>
    </>
  );
}

// ---------------- pane: announcement ----------------

function AnnouncementPane({ form, set }: { form: SiteSettings; set: Setter }) {
  const formAnnouncements = form.announcements ?? [];
  const announcements = formAnnouncements.length > 0
    ? formAnnouncements
    : (form.announcement_title.trim() || form.announcement_content.trim())
      ? [{
          id: "legacy",
          title: form.announcement_title,
          content: form.announcement_content,
          enabled: true,
          display_mode: form.announcement_display_mode,
        }]
      : [];

  const updateAnnouncements = (next: Announcement[]) => {
    set("announcements", next);
    const first = next[0];
    set("announcement_title", first?.title ?? "");
    set("announcement_content", first?.content ?? "");
    set("announcement_display_mode", first?.display_mode ?? "always");
  };

  const updateAnnouncement = (idx: number, patch: Partial<Announcement>) => {
    updateAnnouncements(announcements.map((a, i) => i === idx ? { ...a, ...patch } : a));
  };

  const addAnnouncement = () => {
    updateAnnouncements([
      ...announcements,
      {
        id: crypto.randomUUID(),
        title: "",
        content: "",
        enabled: true,
        display_mode: "always",
      },
    ]);
  };

  return (
    <>
      <FormSection
        title="公告弹窗"
        description="关闭后前台不再弹出公告。用户勾选「不再显示」后会在当前浏览器隐藏；标题或正文变更后会重新弹出。"
        actions={
          <Switch
            checked={form.announcement_enabled}
            onCheckedChange={(v) => set("announcement_enabled", v)}
            aria-label="启用公告"
          />
        }
      />

      <FormSection
        title="公告列表"
        description="弹窗内按顺序切换显示；个人中心展示所有启用的公告。"
        actions={
          <Button size="2" variant="soft" color="gray" onClick={addAnnouncement}>
            <Plus size={14} /> 添加公告
          </Button>
        }
      >
        {announcements.length === 0 ? (
          <Text size="2" color="gray">暂无公告。</Text>
        ) : (
          <Flex direction="column" gap="3">
            {announcements.map((announcement, idx) => (
              <Box
                key={announcement.id || idx}
                p="4"
                style={{ border: "1px solid var(--c-outline-variant)", borderRadius: "var(--radius-control)" }}
              >
                <Flex justify="between" align="center" gap="3" mb="3">
                  <Flex align="center" gap="3">
                    <Text size="2" weight="medium">公告 {idx + 1}</Text>
                    <Switch
                      size="1"
                      checked={announcement.enabled}
                      onCheckedChange={(v) => updateAnnouncement(idx, { enabled: v })}
                      aria-label={`启用公告 ${idx + 1}`}
                    />
                  </Flex>
                  <IconButton
                    size="1"
                    variant="ghost"
                    color="gray"
                    onClick={() => updateAnnouncements(announcements.filter((_, i) => i !== idx))}
                    title="删除公告"
                    aria-label="删除公告"
                  >
                    <Trash2 size={14} />
                  </IconButton>
                </Flex>

                <FieldGrid>
                  <Field label="公告标题">
                    <ClearableTextField
                      value={announcement.title}
                      onChange={(e) => updateAnnouncement(idx, { title: e.target.value })}
                      placeholder="服务公告"
                    />
                  </Field>
                  <Field label="显示时机">
                    <Select.Root
                      value={announcement.display_mode}
                      onValueChange={(v) => updateAnnouncement(idx, { display_mode: v as Announcement["display_mode"] })}
                    >
                      <Select.Trigger />
                      <Select.Content>
                        <Select.Item value="always">始终显示</Select.Item>
                        <Select.Item value="after_login">登录后显示</Select.Item>
                      </Select.Content>
                    </Select.Root>
                  </Field>
                  <Field label="公告内容" hint="多行纯文本，前台保留换行。" span="full">
                    <TextArea
                      value={announcement.content}
                      onChange={(e) => updateAnnouncement(idx, { content: e.target.value })}
                      placeholder="请输入需要展示给用户的公告内容"
                      rows={5}
                    />
                  </Field>
                </FieldGrid>
              </Box>
            ))}
          </Flex>
        )}
      </FormSection>
    </>
  );
}

// ---------------- pane: endpoints ----------------

function EndpointsPane({ form, set }: { form: SiteSettings; set: Setter }) {
  return (
    <FormSection
      title="公开接入地址"
      description="展示在公开「接入指南」页，供用户复制到 SDK 配置。留空按站点地址自动拼出默认值。"
    >
      <FieldGrid columns={1}>
        <Field
          label="OpenAI Responses"
          code="base_url"
          hint="Codex CLI、openai SDK、Grok Build 的 base_url；默认 {site_url}/v1。Grok Build 默认 api_backend=responses，不要填 /anthropic。"
        >
          <ClearableTextField
            type="url"
            value={form.endpoint_openai_responses}
            onChange={(e) => set("endpoint_openai_responses", e.target.value)}
            placeholder="https://jucode.example.com/v1"
          />
        </Field>
        <Field
          label="OpenAI Chat Completions"
          code="base_url"
          hint="Chat Completions 兼容客户端的 base_url；默认 {site_url}/v1。"
        >
          <ClearableTextField
            type="url"
            value={form.endpoint_openai_compat}
            onChange={(e) => set("endpoint_openai_compat", e.target.value)}
            placeholder="https://jucode.example.com/v1"
          />
        </Field>
        <Field
          label="Anthropic"
          code="ANTHROPIC_BASE_URL"
          hint="Anthropic SDK 与 Claude Code 的基础地址；默认 {site_url}/anthropic。SDK 自动追加 /v1/messages，不要自己写 /v1。"
        >
          <ClearableTextField
            type="url"
            value={form.endpoint_anthropic}
            onChange={(e) => set("endpoint_anthropic", e.target.value)}
            placeholder="https://jucode.example.com/anthropic"
          />
        </Field>
      </FieldGrid>
    </FormSection>
  );
}

// ---------------- pane: auth (registration + email verification) ----------------

function AuthPane({ form, set }: { form: SiteSettings; set: Setter }) {
  return (
    <>
      <FormSection
        title="用户注册"
        description="控制 /auth/register 的行为。关闭注册后公开注册页直接返回 403；开启审核后新账号默认待审核，需在「用户」页通过后才能登录。"
      >
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.registration_enabled}
            onCheckedChange={(v) => set("registration_enabled", v)}
            label="允许新用户注册"
          />
          <ToggleRow
            checked={form.registration_requires_approval}
            onCheckedChange={(v) => set("registration_requires_approval", v)}
            disabled={!form.registration_enabled}
            label="注册需要管理员审核"
            hint="新账号进入待审核状态，审核通过前无法登录。"
          />
        </div>
      </FormSection>

      <FormSection
        title="邮箱验证"
        description="三个开关相互独立。SMTP 未配置时（见「邮件 SMTP」）这些开关会被静默忽略。"
      >
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.require_register_verification}
            onCheckedChange={(v) => set("require_register_verification", v)}
            label="注册时必须验证邮箱"
            hint="未验证的账号无法登录。"
          />
          <ToggleRow
            checked={form.require_change_email_verification}
            onCheckedChange={(v) => set("require_change_email_verification", v)}
            label="修改邮箱时必须验证新邮箱"
          />
          <ToggleRow
            checked={form.allow_password_reset}
            onCheckedChange={(v) => set("allow_password_reset", v)}
            label="启用「忘记密码」邮件重置"
          />
        </div>
      </FormSection>
    </>
  );
}

// ---------------- pane: gateway ----------------

function GatewayPane({ form, set }: { form: SiteSettings; set: Setter }) {
  return (
    <>
      <FormSection
        title="会话与超时"
        description="粘性会话把同一 (user, model_group) 或同一会话的请求短期内路由到同一提供商；首 token 超时避免 SSE 长时间挂起。"
      >
        <FieldGrid>
          <Field label="粘性 TTL" code="sticky_ttl_seconds" hint="单位秒。0 关闭粘性，回到权重顺序；推荐 1800（30 分钟）。">
            <ClearableTextField
              type="number"
              min="0"
              max="86400"
              value={String(form.sticky_ttl_seconds)}
              onChange={(e) => set("sticky_ttl_seconds", parseInt(e.target.value || "0", 10) || 0)}
              placeholder="1800"
            />
          </Field>
          <Field label="首 token 超时" code="stream_first_text_timeout_seconds" hint="单位秒。流式请求在首个可见文本前超过该时间即中断。默认 20，0 关闭，最大 600。">
            <ClearableTextField
              type="number"
              min="0"
              max="600"
              value={String(form.stream_first_text_timeout_seconds)}
              onChange={(e) => set("stream_first_text_timeout_seconds", parseInt(e.target.value || "0", 10) || 0)}
              placeholder="20"
            />
          </Field>
        </FieldGrid>
      </FormSection>

      <FormSection title="协议能力" description="决定网关对外暴露哪些入口，以及是否在方言之间改写请求。">
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.format_conversion_enabled}
            onCheckedChange={(v) => set("format_conversion_enabled", v)}
            label="启用方言转换"
            hint="在 Chat Completions、Responses 与 Anthropic Messages 之间按上游方言改写请求与响应（含 web_search）。关闭时原生透传，仅保留 Anthropic → Chat 回退。"
          />
          <ToggleRow
            checked={form.websocket_enabled}
            onCheckedChange={(v) => set("websocket_enabled", v)}
            label="启用 WebSocket 入口"
            hint="控制 GET /v1/realtime 与 Responses WebSocket 升级。上游支持时直接透传，不支持时由本地 HTTP 桥接承接同一套事件协议；关闭后两个入口返回 404，客户端据此改用 HTTP。"
          />
          <ToggleRow
            checked={form.allow_image_output}
            onCheckedChange={(v) => set("allow_image_output", v)}
            label="允许模型返回图像"
            hint="关闭时图片块替换为提示文字，不计费；开启后按模型的图像输出单价计价。"
          />
        </div>
        {form.allow_image_output && (
          <Callout.Root size="1" variant="surface" mt="3" color="amber">
            <Callout.Text>
              开启后请到「模型」页为支持图像输出的模型设置生图单价或 <code>output_image_price</code>，否则未配置的图片计费项不会计入余额扣减。
            </Callout.Text>
          </Callout.Root>
        )}
      </FormSection>
    </>
  );
}

// ---------------- pane: payment ----------------

function PaymentPane({ form, set }: { form: SiteSettings; set: Setter }) {
  return (
    <>
      <FormSection
        title="收款渠道"
        description="关闭后前台不再发起新的支付；已生成的收银台链接继续可用。"
      >
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.payment_alipay_enabled}
            onCheckedChange={(v) => set("payment_alipay_enabled", v)}
            label="支付宝"
            hint="跳转支付宝收银台：桌面端电脑网站支付，手机端手机网站支付。"
          />
        </div>
      </FormSection>

      <FormSection title="充值档位" description="「购买」页余额充值的快捷金额（积分），用户仍可输入其他金额。">
        <FieldGrid>
          <Field label="快捷金额" code="topup_presets" hint="用逗号分隔的正整数，按填写顺序展示；留空则不显示快捷按钮。" span="full">
            <TopupPresetsInput value={form.topup_presets ?? []} onChange={(v) => set("topup_presets", v)} />
          </Field>
        </FieldGrid>
      </FormSection>

      <FormSection
        title="退款"
        description="支付宝订单可原路退回，已消费的部分不退；手工加款由客服通过工单处理。"
      >
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.self_refund_enabled ?? false}
            onCheckedChange={(v) => set("self_refund_enabled", v)}
            label="自助退款"
            hint="用户在账单页自行退回未消费的余额和生效中的 Coding Plan（按剩余天数），无需提交工单。"
          />
          <ToggleRow
            checked={form.refund_auto_approve_enabled ?? false}
            onCheckedChange={(v) => set("refund_auto_approve_enabled", v)}
            label="退款工单自动审批"
            hint="用户为订单提交退款工单时自动创建退款申请；可自动退款的渠道且金额不超过上限时直接批准并原路退回，其余仍由客服审批。"
          />
        </div>
        <FieldGrid>
          <Field label="自动审批上限" code="refund_auto_approve_max" hint="单笔退款金额上限（积分），留空或 0 表示不限。">
            <ClearableTextField
              type="number"
              min="0"
              step="0.01"
              placeholder="不限"
              disabled={!form.refund_auto_approve_enabled}
              value={form.refund_auto_approve_max ?? ""}
              onChange={(e) => set("refund_auto_approve_max", e.target.value.trim())}
            />
          </Field>
        </FieldGrid>
      </FormSection>
    </>
  );
}

function parsePresets(text: string): number[] | null {
  const parts = text.split(/[,，\s]+/).filter(Boolean);
  const nums = parts.map(Number);
  return nums.every((n) => Number.isInteger(n) && n > 0) ? nums : null;
}

// The input keeps its own text so a trailing comma survives typing; the form
// only receives a list once every entry parses.
function TopupPresetsInput({ value, onChange }: { value: number[]; onChange: (v: number[]) => void }) {
  const joined = value.join(", ");
  const [text, setText] = useState(joined);
  const [synced, setSynced] = useState(joined);
  if (synced !== joined) {
    setSynced(joined);
    if (parsePresets(text)?.join(", ") !== joined) setText(joined);
  }
  const invalid = parsePresets(text) === null;
  return (
    <>
      <ClearableTextField
        value={text}
        placeholder="50, 100, 200, 500, 1000"
        onChange={(e) => {
          setText(e.target.value);
          const parsed = parsePresets(e.target.value);
          if (parsed) onChange(parsed);
        }}
      />
      {invalid && <Text size="1" color="red" as="div" mt="1">只能填写正整数，用逗号分隔。</Text>}
    </>
  );
}

// ---------------- pane: moderation ----------------

function ModerationPane({ form, set }: { form: SiteSettings; set: Setter }) {
  return (
    <>
      <FormSection
        title="内容审核"
        description="调用 OpenAI Moderation 兼容接口对请求做异步复核，不阻塞主链路。命中关键词或被抽中的请求送审，确认的高危违规自动触发冷却、冻结或封禁。"
        actions={
          <Switch
            checked={form.moderation_enabled}
            onCheckedChange={(v) => set("moderation_enabled", v)}
            aria-label="启用内容审核"
          />
        }
      >
        <FieldGrid>
          <Field label="接口地址" code="base_url" hint="OpenAI 兼容接口地址，请求 {base}/moderations。">
            <ClearableTextField placeholder="https://api.openai.com/v1" value={form.moderation_base_url}
              onChange={(e) => set("moderation_base_url", e.target.value)} />
          </Field>
          <Field label="模型">
            <ClearableTextField placeholder="omni-moderation-latest" value={form.moderation_model}
              onChange={(e) => set("moderation_model", e.target.value)} />
          </Field>
          <Field label="API Key" hint={secretHint(form.moderation_api_key_set)} span="full">
            <ClearableTextField type="password" placeholder={form.moderation_api_key_set ? "••••••" : "sk-..."}
              value={form.moderation_api_key ?? ""}
              onChange={(e) => set("moderation_api_key", e.target.value)} />
          </Field>
        </FieldGrid>
      </FormSection>

      <FormSection title="送审策略" description="阈值决定判罚严格程度，抽查比例与调用上限控制成本。">
        <FieldGrid columns={3}>
          <Field label="判罚阈值" hint="0–1，命中分数 ≥ 阈值判为违规；越低越严格。">
            <ClearableTextField type="number" min="0" max="1" step="0.05"
              value={String(form.moderation_threshold)}
              onChange={(e) => set("moderation_threshold", clamp01(e.target.value, 0.5))} />
          </Field>
          <Field label="抽查比例" hint="0–1，对未命中关键词的流量抽样送审的比例。">
            <ClearableTextField type="number" min="0" max="1" step="0.01"
              value={String(form.moderation_sample_rate)}
              onChange={(e) => set("moderation_sample_rate", clamp01(e.target.value, 0))} />
          </Field>
          <Field label="每分钟调用上限" hint="成本闸，0 不限。">
            <ClearableTextField type="number" min="0"
              value={String(form.moderation_max_per_min)}
              onChange={(e) => set("moderation_max_per_min", Math.max(0, parseInt(e.target.value || "0", 10) || 0))} />
          </Field>
        </FieldGrid>
      </FormSection>
    </>
  );
}

// clamp01 parses a 0..1 ratio input, falling back to def on empty/out-of-range.
function clamp01(raw: string, def: number): number {
  const n = parseFloat(raw);
  if (Number.isNaN(n) || n < 0 || n > 1) return def;
  return n;
}

// ---------------- pane: mail (SMTP) ----------------

function MailPane({ form, set }: { form: SiteSettings; set: Setter }) {
  return (
    <>
      <FormSection title="SMTP 服务器" description="填写后才能发送验证邮件、密码重置等系统邮件。">
        <FieldGrid>
          <Field label="主机" code="smtp_host">
            <ClearableTextField placeholder="smtp.example.com" value={form.smtp_host}
              onChange={(e) => set("smtp_host", e.target.value)} />
          </Field>
          <Field label="端口" code="smtp_port" hint="STARTTLS 通常为 587，隐式 TLS 为 465。">
            <ClearableTextField type="number" value={String(form.smtp_port)}
              onChange={(e) => set("smtp_port", Number(e.target.value) || 0)} />
          </Field>
          <Field label="用户名">
            <ClearableTextField placeholder="noreply@example.com" value={form.smtp_username}
              onChange={(e) => set("smtp_username", e.target.value)} />
          </Field>
          <Field label="密码" hint={secretHint(form.smtp_password_set)}>
            <ClearableTextField type="password" placeholder={form.smtp_password_set ? "••••••" : ""}
              value={form.smtp_password ?? ""}
              onChange={(e) => set("smtp_password", e.target.value)} />
          </Field>
        </FieldGrid>
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.smtp_use_tls}
            onCheckedChange={(v) => set("smtp_use_tls", v)}
            label="隐式 TLS"
            hint="连接建立即加密（端口 465）。关闭时按 STARTTLS 协商。"
          />
        </div>
      </FormSection>

      <FormSection title="发件人" description="系统邮件的显示来源。">
        <FieldGrid>
          <Field label="发件邮箱">
            <ClearableTextField placeholder="noreply@example.com" value={form.smtp_from_email}
              onChange={(e) => set("smtp_from_email", e.target.value)} />
          </Field>
          <Field label="发件人显示名">
            <ClearableTextField placeholder="JuCode" value={form.smtp_from_name}
              onChange={(e) => set("smtp_from_name", e.target.value)} />
          </Field>
        </FieldGrid>
      </FormSection>
    </>
  );
}

// ---------------- pane: status page ----------------

// StatusPagePane hosts the model-group selector for the real-traffic page.
//
// status_page_group_ids is an ordered list, not a set: its order is the display
// order on /status. So selection cannot go through CheckboxGroup.Root, whose
// onValueChange reports values in DOM order and would silently reshuffle a
// hand-arranged list on every toggle. Checking a group appends it to the end;
// order is then changed only by dragging.
function StatusPagePane({
  form,
  set,
}: {
  form: SiteSettings;
  set: Setter;
}) {
  const { data: groups } = useQuery({
    queryKey: ["admin", "model-groups"],
    queryFn: () => adminApi.groups(),
  });
  const selectedIds = form.status_page_group_ids;
  // Resolve ids in configured order, dropping any group deleted since save.
  const selected = useMemo(() => {
    if (!groups) return [];
    const byId = new Map(groups.map((g) => [g.id, g]));
    return selectedIds.map((id) => byId.get(id)).filter((g): g is NonNullable<typeof g> => Boolean(g));
  }, [groups, selectedIds]);

  const toggle = (id: string, checked: boolean) => {
    if (checked) {
      if (selectedIds.includes(id)) return;
      set("status_page_group_ids", [...selectedIds, id]);
    } else {
      set("status_page_group_ids", selectedIds.filter((x) => x !== id));
    }
  };

  return (
    <>
      <FormSection
        title="展示的模型组"
        description="/status 页只统计真实业务请求的最终结果；重试只计一次，用户输入、权限、额度和策略拦截被排除。无流量或样本不足时显示「数据不足」。未勾选任何组时提示「未配置」。"
      >
        <FieldGrid>
          <Field label="模型组" hint="按模型组聚合可用性，每张卡片显示组内 provider 的状态汇总。">
            {!groups ? (
              <Text size="1" color="gray">加载中…</Text>
            ) : groups.length === 0 ? (
              <Text size="1" color="gray">尚未创建模型组</Text>
            ) : (
              <Flex direction="column" gap="1">
                {groups.map((g) => (
                  <Text as="label" size="2" key={g.id}>
                    <Flex align="center" gap="2">
                      <Checkbox
                        checked={selectedIds.includes(g.id)}
                        onCheckedChange={(v) => toggle(g.id, v === true)}
                      />
                      {g.name}
                    </Flex>
                  </Text>
                ))}
              </Flex>
            )}
          </Field>
          {selected.length > 0 && (
            <Field label="展示顺序" hint="拖拽调整，从上到下即 /status 页上的排列顺序。">
              <Box className="sortable-panel">
                <SortableList
                  items={selected}
                  getId={(g) => g.id}
                  onReorder={(ids) => set("status_page_group_ids", ids)}
                  renderItem={(g, i) => (
                    <SortableItem key={g.id} id={g.id}>
                      {(handle) => (
                        <Flex align="center" gap="2" className="sortable-panel-row">
                          {handle}
                          <Text size="1" color="gray" className="sortable-panel-index">{i + 1}</Text>
                          <Text size="2">{g.name}</Text>
                        </Flex>
                      )}
                    </SortableItem>
                  )}
                />
              </Box>
            </Field>
          )}
        </FieldGrid>
      </FormSection>

      <FormSection title="统计口径" description="状态页统计最近一段时间的真实业务请求。">
        <FieldGrid>
          <Field label="统计窗口" code="status_page_window_hours" hint="单位小时，默认 24，可填 1–168。">
            <ClearableTextField
              type="number"
              min="1"
              max="168"
              value={String(form.status_page_window_hours)}
              onChange={(e) => set("status_page_window_hours", parseInt(e.target.value || "24", 10) || 24)}
              placeholder="24"
            />
          </Field>
        </FieldGrid>
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.status_page_upstream_enabled}
            onCheckedChange={(v) => set("status_page_upstream_enabled", v)}
            label="展示厂商官方状态"
            hint="镜像 OpenAI 与 Anthropic 官方状态页。这是厂商的自报口径，与上方基于真实业务请求算出的可用性互相独立。"
          />
        </div>
      </FormSection>
    </>
  );
}

// ---------------- pane: ops alerts ----------------

// OpsAlertPane configures the outbound alert webhook. The URL and the
// signing secret follow the same write-only pattern as the SMTP password
// and the moderation key: the backend returns presence flags only, and an
// empty field on save keeps whatever is already stored.
function OpsAlertPane({ form, set }: { form: SiteSettings; set: Setter }) {
  const urlSet = form.ops_alert_webhook_url_set === true;
  const secretSet = form.ops_alert_webhook_secret_set === true;
  const urlTyped = (form.ops_alert_webhook_url ?? "").trim() !== "";
  const missingURL = form.ops_alert_enabled && !urlSet && !urlTyped;

  const secretBase = form.ops_alert_webhook_type === "generic"
    ? "用于给 payload 附加 HMAC-SHA256 签名头，留空则不签名。"
    : "机器人加签密钥（钉钉/飞书自定义机器人「加签」），未开启加签可留空。";

  return (
    <>
      <FormSection
        title="告警外发"
        description="把状态页事件的开启/恢复和 provider 健康翻转推送到 webhook。外发内容只有事件名、资源名称和前后状态，不含用户、请求内容或密钥。"
        actions={
          <Switch
            checked={form.ops_alert_enabled}
            onCheckedChange={(v) => set("ops_alert_enabled", v)}
            aria-label="启用运维告警外发"
          />
        }
      >
        {missingURL && (
          <Callout.Root size="1" variant="surface" color="amber" mb="4">
            <Callout.Text>还没有配置 webhook 地址，开启后不会有任何告警发出。</Callout.Text>
          </Callout.Root>
        )}
        <FieldGrid>
          <Field label="接收方类型" hint="通用类型发送 JSON 事件；钉钉、飞书按各自机器人的消息格式发送。">
            <Select.Root
              value={form.ops_alert_webhook_type}
              onValueChange={(v) => set("ops_alert_webhook_type", v as OpsAlertWebhookType)}
            >
              <Select.Trigger />
              <Select.Content>
                <Select.Item value="generic">通用 webhook（JSON）</Select.Item>
                <Select.Item value="dingtalk">钉钉自定义机器人</Select.Item>
                <Select.Item value="feishu">飞书自定义机器人</Select.Item>
              </Select.Content>
            </Select.Root>
          </Field>
          <Field label="Webhook 地址" required hint={secretHint(urlSet, "请使用 HTTPS 并在接收端限制来源。")}>
            <ClearableTextField
              type="url"
              placeholder={urlSet ? "••••••" : "https://alerts.example.com/jucode"}
              value={form.ops_alert_webhook_url ?? ""}
              onChange={(e) => set("ops_alert_webhook_url", e.target.value)}
            />
          </Field>
          <Field label="签名密钥" hint={secretHint(secretSet, secretBase)} span="full">
            <ClearableTextField
              type="password"
              placeholder={secretSet ? "••••••" : ""}
              value={form.ops_alert_webhook_secret ?? ""}
              onChange={(e) => set("ops_alert_webhook_secret", e.target.value)}
            />
          </Field>
        </FieldGrid>
      </FormSection>

      <FormSection title="投递参数" description="超出范围的取值会被后端改回默认值。清空已保存的地址或密钥需在后端操作，页面留空只表示不修改。">
        <FieldGrid columns={3}>
          <Field label="请求超时" hint="单位秒，1–30，默认 5。">
            <ClearableTextField
              type="number"
              min="1"
              max="30"
              value={String(form.ops_alert_timeout_seconds)}
              onChange={(e) => set("ops_alert_timeout_seconds", parseInt(e.target.value || "5", 10) || 5)}
            />
          </Field>
          <Field label="失败重试次数" hint="0–5，默认 2；只重试网络错误、429 和 5xx。">
            <ClearableTextField
              type="number"
              min="0"
              max="5"
              value={String(form.ops_alert_max_retries)}
              onChange={(e) => set("ops_alert_max_retries", Math.max(0, parseInt(e.target.value || "0", 10) || 0))}
            />
          </Field>
          <Field label="静默窗口" hint="单位秒，30–86400，默认 300；相同事件在窗口内只发一次。">
            <ClearableTextField
              type="number"
              min="30"
              max="86400"
              value={String(form.ops_alert_silence_seconds)}
              onChange={(e) => set("ops_alert_silence_seconds", parseInt(e.target.value || "300", 10) || 300)}
            />
          </Field>
        </FieldGrid>
      </FormSection>
    </>
  );
}

// ---------------- pane: currency ----------------

// CurrencyPane keeps the single remaining FX knob: the channel
// settlement rate.
//
//   * User-facing amounts are platform points (1 积分 = 1 CNY) — no
//     display-layer conversion exists anymore.
//
//   * exchange_cny_per_usd is ONLY used to settle channel (upstream)
//     costs when a vendor bills in USD. Changing it does NOT rewrite
//     any stored prices and does NOT affect what users see.
function CurrencyPane({ form, set }: { form: SiteSettings; set: Setter }) {
  return (
    <FormSection
      title="渠道结算汇率"
      description="系统以平台积分计价（1 积分 = 1 元人民币）：用户余额、套餐、模型单价、账单全部按积分存储与展示。该汇率仅在上游厂商以美元计价时折算渠道成本，不影响任何面向用户的积分展示。"
    >
      <FieldGrid>
        <Field label="1 USD 折算积分" code="exchange_cny_per_usd" hint="必须大于 0。">
          <ClearableTextField
            type="number"
            step="0.01"
            min="0.01"
            value={String(form.exchange_cny_per_usd)}
            onChange={(e) =>
              set("exchange_cny_per_usd", parseFloat(e.target.value || "7.2") || 7.2)
            }
          />
        </Field>
      </FieldGrid>
    </FormSection>
  );
}

// ---------------- pane: reasoning parse rules ----------------

// Rule editor for SiteSettings.reasoning_parse_rules. Rules probe the
// inbound LLM request body for a thinking-strength label (recorded in
// usage_log.reasoning_effort); they run top-down, first hit wins.
function ReasoningPane({ form, set }: { form: SiteSettings; set: Setter }) {
  const rules = form.reasoning_parse_rules ?? [];
  const setRules = (next: ReasoningParseRule[]) => set("reasoning_parse_rules", next);
  const setRule = (i: number, patch: Partial<ReasoningParseRule>) =>
    setRules(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const move = (i: number, dir: -1 | 1) => {
    const j = i + dir;
    if (j < 0 || j >= rules.length) return;
    const next = [...rules];
    [next[i], next[j]] = [next[j], next[i]];
    setRules(next);
  };

  const kindLabel: Record<string, string> = {
    string: "字符串取值",
    number: "数值取值（原样记录）",
    exists: "存在即标记",
  };

  return (
    <FormSection
      title="解析规则"
      description="从请求体解析「思考强度」，记录到用量日志的 reasoning_effort 字段。字符串字段（如 reasoning.effort）记录取值；数值字段（如 thinking.budget_tokens）原样记录思考 tokens 数量。规则自上而下匹配，命中即停；删除全部规则即关闭解析。"
      actions={
        <>
          <Button size="2" variant="soft" color="gray"
            onClick={() => setRules(defaultReasoningRules.map((r) => ({ ...r })))}>
            恢复默认
          </Button>
          <Button size="2" variant="soft" color="gray"
            onClick={() => setRules([...rules, { path: "", kind: "string" }])}>
            <Plus size={14} /> 添加规则
          </Button>
        </>
      }
    >
      {rules.length === 0 && (
        <Callout.Root size="1" color="amber" variant="surface">
          <Callout.Icon><ShieldAlert size={15} /></Callout.Icon>
          <Callout.Text>当前没有任何规则，思考强度解析已关闭，新请求的 reasoning_effort 将为空。</Callout.Text>
        </Callout.Root>
      )}

      <Flex direction="column" gap="3">
        {rules.map((r, i) => (
          <Box key={i} p="4" style={{ border: "1px solid var(--c-outline-variant)", borderRadius: "var(--radius-control)" }}>
            <Flex align="center" justify="between" mb="3" gap="2">
              <Flex align="center" gap="2">
                <Text size="2" weight="medium">规则 {i + 1}</Text>
                <Text size="1" color="gray">{kindLabel[r.kind] ?? r.kind}</Text>
              </Flex>
              <Flex gap="1">
                <IconButton size="1" variant="ghost" color="gray" disabled={i === 0} onClick={() => move(i, -1)} title="上移" aria-label="上移">
                  <ChevronUp size={14} />
                </IconButton>
                <IconButton size="1" variant="ghost" color="gray" disabled={i === rules.length - 1} onClick={() => move(i, 1)} title="下移" aria-label="下移">
                  <ChevronDown size={14} />
                </IconButton>
                <IconButton size="1" variant="ghost" color="gray" onClick={() => setRules(rules.filter((_, j) => j !== i))} title="删除规则" aria-label="删除规则">
                  <Trash2 size={14} />
                </IconButton>
              </Flex>
            </Flex>

            <FieldGrid>
              <Field label="请求体字段路径" required hint="点号分隔的 JSON 路径，如 reasoning.effort、thinking.budget_tokens。">
                <ClearableTextField
                  className="jc-mono"
                  placeholder="reasoning.effort"
                  value={r.path}
                  onChange={(e) => setRule(i, { path: e.target.value })}
                />
              </Field>
              <Field label="解析方式">
                <Select.Root
                  value={r.kind}
                  onValueChange={(v) => setRule(i, { kind: v as ReasoningParseRule["kind"] })}
                >
                  <Select.Trigger />
                  <Select.Content>
                    <Select.Item value="string">字符串取值</Select.Item>
                    <Select.Item value="number">数值取值（原样记录）</Select.Item>
                    <Select.Item value="exists">存在即标记</Select.Item>
                  </Select.Content>
                </Select.Root>
              </Field>

              {r.kind === "string" && (
                <Field label="取值映射" hint="把原始取值改写为统一标签（如 xhigh → high）；未列出的取值原样记录。" span="full">
                  <Flex direction="column" gap="2">
                    {Object.entries(r.value_map ?? {}).map(([from, to], vi) => (
                      <Flex key={vi} gap="2" align="center">
                        <ClearableTextField
                          placeholder="原始值，如 xhigh"
                          value={from}
                          style={{ flex: 1 }}
                          onChange={(e) => {
                            const entries = Object.entries(r.value_map ?? {});
                            entries[vi] = [e.target.value, to];
                            setRule(i, { value_map: Object.fromEntries(entries) });
                          }}
                        />
                        <Text size="2" color="gray">→</Text>
                        <ClearableTextField
                          placeholder="记录为，如 high"
                          value={to}
                          style={{ flex: 1 }}
                          onChange={(e) => {
                            const entries = Object.entries(r.value_map ?? {});
                            entries[vi] = [from, e.target.value];
                            setRule(i, { value_map: Object.fromEntries(entries) });
                          }}
                        />
                        <IconButton size="1" variant="ghost" color="gray" title="删除映射" aria-label="删除映射"
                          onClick={() => {
                            const vm = { ...(r.value_map ?? {}) };
                            delete vm[from];
                            setRule(i, { value_map: Object.keys(vm).length ? vm : undefined });
                          }}>
                          <Trash2 size={13} />
                        </IconButton>
                      </Flex>
                    ))}
                    <Flex>
                      <Button size="1" variant="soft" color="gray"
                        onClick={() => setRule(i, { value_map: { ...(r.value_map ?? {}), "": "" } })}>
                        <Plus size={13} /> 添加映射
                      </Button>
                    </Flex>
                  </Flex>
                </Field>
              )}

              {r.kind === "number" && (
                <Field label="显示前缀" hint="原样记录字段数值；填写前缀则记录为「前缀:数值」，如 thinking:16384。">
                  <ClearableTextField
                    placeholder="thinking"
                    value={r.label ?? ""}
                    onChange={(e) => setRule(i, { label: e.target.value })}
                  />
                </Field>
              )}

              {r.kind === "exists" && (
                <Field label="记录为" hint="字段存在（非 null）即记录该标签，不看具体取值。">
                  <ClearableTextField
                    placeholder="thinking"
                    value={r.label ?? ""}
                    onChange={(e) => setRule(i, { label: e.target.value })}
                  />
                </Field>
              )}
            </FieldGrid>
          </Box>
        ))}
      </Flex>
    </FormSection>
  );
}
