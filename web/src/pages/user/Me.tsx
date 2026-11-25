import "../../personal-responsive.css";
import { useEffect, useMemo, useState, type CSSProperties, type ReactNode } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import {
  Badge, Box, Button, Card, Code, Dialog, Flex, SegmentedControl, Skeleton, Text,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip as RTooltip, XAxis, YAxis,
} from "recharts";
import { X } from "lucide-react";
import QRCode from "qrcode";
import { Trans, useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { localeTag } from "../../i18n";
import { meApi, type Announcement, type MeResponse } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { cacheHitRate, DEFAULT_POLL_MS, useTokenTotals } from "../../components/TokenUsageSection";
import { chartAxis, chartBarMaxSize, chartGrid, chartTooltipProps } from "../../lib/chartTheme";
import { colorsForModels } from "../../lib/modelColors";
import { formatAmount } from "../../lib/currency";
import { PointAmount } from "../../components/PointAmount";
import { PlanUsagePanel } from "../../components/KeyAccountStatus";
import { HeldBalance } from "../../components/HeldBalance";
import { SelfRefundDialog } from "../../components/SelfRefund";
import { ClearableTextField } from "../../components/ClearableTextField";
import { useSite } from "../../store/site";
import { useActiveOrg, type ActiveOrg } from "../../hooks/useOrgContext";
import { PageHeader } from "../../components/PageHeader";

/* ============ Edit profile dialog (nickname + email + password tabs) ============ */

function EditDialog({
  open, onOpenChange, initial, email,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  initial: string;
  email: string;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [tab, setTab] = useState<"profile" | "email" | "password">("profile");
  useEffect(() => { if (open) setTab("profile"); }, [open]);

  // Profile tab — nickname
  const [nickname, setNickname] = useState(initial);
  useEffect(() => { if (open) setNickname(initial); }, [open, initial]);
  const updateProfile = useMutation({
    mutationFn: () => meApi.updateMe({ nickname: nickname.trim() }),
    onSuccess: () => {
      onOpenChange(false);
      qc.invalidateQueries({ queryKey: ["me"] });
    },
  });

  // Email tab — request change, sends verification link
  const [newEmail, setNewEmail] = useState("");
  useEffect(() => { if (open) setNewEmail(""); }, [open]);
  const changeEmail = useMutation({
    mutationFn: () => meApi.changeEmail({ new_email: newEmail.trim() }),
  });

  // Password tab — old + new
  const [oldPwd, setOldPwd] = useState("");
  const [newPwd, setNewPwd] = useState("");
  const [confirmPwd, setConfirmPwd] = useState("");
  useEffect(() => { if (open) { setOldPwd(""); setNewPwd(""); setConfirmPwd(""); } }, [open]);
  const changePassword = useMutation({
    mutationFn: () => meApi.changePassword({ old_password: oldPwd, new_password: newPwd }),
  });

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Content style={{ maxWidth: 480 }}>
        <Dialog.Title>{t("usage.me.edit.title")}</Dialog.Title>
        <Flex gap="2" mt="3" mb="3">
          <Button size="1" variant={tab === "profile" ? "solid" : "soft"} onClick={() => setTab("profile")}>{t("usage.me.edit.tabProfile")}</Button>
          <Button size="1" variant={tab === "email" ? "solid" : "soft"} onClick={() => setTab("email")}>{t("usage.me.edit.tabEmail")}</Button>
          <Button size="1" variant={tab === "password" ? "solid" : "soft"} onClick={() => setTab("password")}>{t("usage.me.edit.tabPassword")}</Button>
        </Flex>

        {tab === "profile" && (
          <Flex direction="column" gap="3">
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.edit.currentEmail")}</Text>
              <ClearableTextField value={email} disabled />
            </Box>
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.edit.nickname")}</Text>
              <ClearableTextField
                placeholder={t("usage.me.edit.nicknamePlaceholder")}
                value={nickname}
                onChange={(e) => setNickname(e.target.value)}
              />
            </Box>
            {updateProfile.isError && <Text size="2" color="red">{errMsg(updateProfile.error)}</Text>}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => onOpenChange(false)}>{t("usage.common.cancel")}</Button>
              <Button loading={updateProfile.isPending} onClick={() => updateProfile.mutate()}>{t("usage.me.edit.save")}</Button>
            </Flex>
          </Flex>
        )}

        {tab === "email" && (
          <Flex direction="column" gap="3">
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.edit.currentEmail")}</Text>
              <ClearableTextField value={email} disabled />
            </Box>
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.edit.newEmail")}</Text>
              <ClearableTextField
                type="email"
                placeholder="new@example.com"
                value={newEmail}
                onChange={(e) => setNewEmail(e.target.value)}
              />
              <Text size="1" color="gray" as="p" mt="1">
                {t("usage.me.edit.emailHint")}
              </Text>
            </Box>
            {changeEmail.isError && <Text size="2" color="red">{errMsg(changeEmail.error)}</Text>}
            {changeEmail.isSuccess && (
              <Text size="2" color="green">{t("usage.me.edit.emailSent", { email: newEmail })}</Text>
            )}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => onOpenChange(false)}>{t("usage.common.close")}</Button>
              <Button
                disabled={!newEmail.includes("@") || newEmail === email}
                loading={changeEmail.isPending}
                onClick={() => changeEmail.mutate()}
              >{t("usage.me.edit.sendVerification")}</Button>
            </Flex>
          </Flex>
        )}

        {tab === "password" && (
          <Flex direction="column" gap="3">
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.edit.currentPassword")}</Text>
              <ClearableTextField type="password" value={oldPwd} onChange={(e) => setOldPwd(e.target.value)} />
            </Box>
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.edit.newPassword")}</Text>
              <ClearableTextField type="password" value={newPwd} onChange={(e) => setNewPwd(e.target.value)} />
            </Box>
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.edit.repeatPassword")}</Text>
              <ClearableTextField type="password" value={confirmPwd} onChange={(e) => setConfirmPwd(e.target.value)} />
              {confirmPwd !== "" && confirmPwd !== newPwd && (
                <Text size="1" color="red" as="p" mt="1">{t("usage.me.edit.mismatch")}</Text>
              )}
            </Box>
            {changePassword.isError && <Text size="2" color="red">{errMsg(changePassword.error)}</Text>}
            {changePassword.isSuccess && (
              <Text size="2" color="green">{t("usage.me.edit.passwordUpdated")}</Text>
            )}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => onOpenChange(false)}>{t("usage.common.close")}</Button>
              <Button
                disabled={oldPwd === "" || newPwd.length < 8 || newPwd !== confirmPwd}
                loading={changePassword.isPending}
                onClick={() => changePassword.mutate()}
              >{t("usage.me.edit.updatePassword")}</Button>
            </Flex>
          </Flex>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}

/* ============ TOTP (two-factor) ============ */

function TotpDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const { data: status } = useQuery({
    queryKey: ["me", "totp"],
    queryFn: meApi.totpStatus,
    enabled: open,
  });

  // Bind flow state. recoveryCodes doubles as the "done" marker — once
  // set, the dialog shows only the codes until closed.
  const [setupInfo, setSetupInfo] = useState<{ secret: string; otpauth_url: string } | null>(null);
  const [qr, setQr] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  // Unbind flow state.
  const [pwd, setPwd] = useState("");
  const [disableCode, setDisableCode] = useState("");

  useEffect(() => {
    if (!open) {
      setSetupInfo(null); setQr(null); setCode("");
      setRecoveryCodes(null); setPwd(""); setDisableCode("");
    }
  }, [open]);

  useEffect(() => {
    if (!setupInfo) { setQr(null); return; }
    QRCode.toDataURL(setupInfo.otpauth_url, { width: 192, margin: 1 })
      .then(setQr)
      .catch(() => setQr(null));
  }, [setupInfo]);

  const begin = useMutation({
    mutationFn: meApi.totpSetup,
    onSuccess: (r) => setSetupInfo(r),
  });
  const confirm = useMutation({
    mutationFn: () => meApi.totpConfirm(code.trim()),
    onSuccess: (r) => {
      setRecoveryCodes(r.recovery_codes);
      qc.invalidateQueries({ queryKey: ["me"] });
    },
  });
  const disable = useMutation({
    mutationFn: () => meApi.totpDisable({ password: pwd, code: disableCode.trim() }),
    onSuccess: () => {
      onOpenChange(false);
      qc.invalidateQueries({ queryKey: ["me"] });
    },
  });

  const copyCodes = () => {
    if (recoveryCodes) void navigator.clipboard.writeText(recoveryCodes.join("\n"));
  };

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Content style={{ maxWidth: 480 }}>
        <Dialog.Title>{t("usage.me.totp.title")}</Dialog.Title>

        {recoveryCodes ? (
          /* Post-confirm: the one and only time the codes are visible. */
          <Flex direction="column" gap="3">
            <Text size="2" color="green">{t("usage.me.totp.recoveryCodes")}</Text>
            <Box p="3" style={{ background: "var(--gray-3)", borderRadius: 8, fontFamily: "ui-monospace, monospace", fontSize: 13, lineHeight: 1.9, columnCount: 2 }}>
              {recoveryCodes.map((c) => <div key={c}>{c}</div>)}
            </Box>
            <Flex gap="2" justify="end">
              <Button variant="soft" onClick={copyCodes}>{t("usage.me.totp.copyAll")}</Button>
              <Button onClick={() => onOpenChange(false)}>{t("usage.me.totp.saved")}</Button>
            </Flex>
          </Flex>
        ) : status?.enabled ? (
          /* Already enabled → manage / unbind. */
          <Flex direction="column" gap="3">
            <Text size="2" color="gray">
              {t("usage.me.totp.enabledStatus", {
                count: status.recovery_codes_left,
                bound: status.confirmed_at
                  ? t("usage.me.totp.boundOn", { date: new Date(status.confirmed_at).toLocaleDateString(localeTag()) })
                  : "",
              })}
            </Text>
            <Text size="2" weight="medium">{t("usage.me.totp.unbind")}</Text>
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.totp.accountPassword")}</Text>
              <ClearableTextField type="password" value={pwd} onChange={(e) => setPwd(e.target.value)} />
            </Box>
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.totp.codeLabel")}</Text>
              <ClearableTextField
                value={disableCode}
                onChange={(e) => setDisableCode(e.target.value)}
                placeholder={t("usage.me.totp.codePlaceholder")}
                style={{ fontFamily: "ui-monospace, monospace" }}
              />
            </Box>
            {disable.isError && <Text size="2" color="red">{errMsg(disable.error)}</Text>}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => onOpenChange(false)}>{t("usage.common.close")}</Button>
              <Button
                color="red"
                variant="soft"
                disabled={pwd === "" || disableCode.trim() === ""}
                loading={disable.isPending}
                onClick={() => disable.mutate()}
              >{t("usage.me.totp.unbind")}</Button>
            </Flex>
          </Flex>
        ) : setupInfo ? (
          /* Scanning + first code. */
          <Flex direction="column" gap="3">
            <Text size="2" color="gray">{t("usage.me.totp.scanHint")}</Text>
            <Flex align="center" direction="column" gap="2">
              {qr && <img src={qr} alt={t("usage.me.totp.qrAlt")} width={192} height={192} style={{ borderRadius: 8 }} />}
              <Text size="1" color="gray">{t("usage.me.totp.manualKey")}</Text>
              <Code style={{ fontSize: 12, wordBreak: "break-all" }}>{setupInfo.secret}</Code>
            </Flex>
            <Box>
              <Text size="1" color="gray" as="p" mb="1">{t("usage.me.totp.code")}</Text>
              <ClearableTextField
                value={code}
                onChange={(e) => setCode(e.target.value)}
                placeholder="123456"
                autoFocus
                style={{ fontFamily: "ui-monospace, monospace", letterSpacing: 2 }}
              />
            </Box>
            {confirm.isError && <Text size="2" color="red">{errMsg(confirm.error)}</Text>}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => onOpenChange(false)}>{t("usage.common.cancel")}</Button>
              <Button
                disabled={code.trim().length !== 6}
                loading={confirm.isPending}
                onClick={() => confirm.mutate()}
              >{t("usage.me.totp.confirmEnable")}</Button>
            </Flex>
          </Flex>
        ) : (
          /* Entry point. */
          <Flex direction="column" gap="3">
            <Text size="2" color="gray">
              {t("usage.me.totp.intro")}
            </Text>
            {begin.isError && <Text size="2" color="red">{errMsg(begin.error)}</Text>}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => onOpenChange(false)}>{t("usage.common.cancel")}</Button>
              <Button loading={begin.isPending} onClick={() => begin.mutate()}>{t("usage.me.totp.begin")}</Button>
            </Flex>
          </Flex>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}

/* ============ Redeem dialog ============ */

function RedeemDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [code, setCode] = useState("");
  useEffect(() => { if (!open) setCode(""); }, [open]);

  const redeem = useMutation({
    mutationFn: () => meApi.redeem(code.trim()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["me"] });
      qc.invalidateQueries({ queryKey: ["me", "usage"] });
    },
  });

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Content style={{ maxWidth: 460 }}>
        <Dialog.Title>{t("usage.me.redeem.title")}</Dialog.Title>
        <Dialog.Description size="2" color="gray" mb="3">
          <Trans i18nKey="usage.me.redeem.description" components={{ code: <Code /> }} />
        </Dialog.Description>
        <Flex direction="column" gap="3">
          <ClearableTextField
            placeholder="JUC-XXXXXX-XXXXXX"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            style={{ fontFamily: "ui-monospace, monospace" }}
            autoFocus
          />
          {redeem.isError && <Text size="2" color="red">{errMsg(redeem.error)}</Text>}
          {redeem.isSuccess && redeem.data && (
            <Box p="3" style={{ background: "var(--green-3)", borderRadius: 8 }}>
              {redeem.data.kind === "balance" ? (
                <Text size="2">{t("usage.me.redeem.credited")} <Code><PointAmount amount={redeem.data.amount} iconSize={13} /></Code>
                  {t("usage.me.redeem.balanceAfter")} <Code><PointAmount amount={redeem.data.balance_after} iconSize={13} /></Code></Text>
              ) : (
                <Text size="2">{t("usage.me.redeem.planActivated", {
                  plan: redeem.data.plan_name,
                  date: redeem.data.expire_at ? new Date(redeem.data.expire_at).toLocaleString(localeTag()) : "—",
                })}</Text>
              )}
            </Box>
          )}
          <Flex gap="2" justify="end">
            <Button variant="soft" color="gray" onClick={() => onOpenChange(false)}>{t("usage.common.close")}</Button>
            <Button disabled={!code.trim()} loading={redeem.isPending} onClick={() => redeem.mutate()}>{t("usage.me.redeem.submit")}</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

/* ============ Time range shared by the usage section ============ */

type Range = "24h" | "7d" | "30d";
const RANGE_LABEL_KEY = { "24h": "usage.common.last24h", "7d": "usage.common.last7d", "30d": "usage.common.last30d" } as const;

// Window start is snapped (to the hour for 24h, to midnight for day ranges)
// so the query key stays stable between polls and matches how the backend
// groups rows. `end` is omitted: the backend defaults it to now, so the
// window rolls forward with each refetch.
function rangeParams(range: Range): { start: string; bucket: "hour" | "day" } {
  const now = new Date();
  if (range === "24h") {
    const from = new Date(now.getTime() - 24 * 3600 * 1000);
    from.setMinutes(0, 0, 0);
    return { start: from.toISOString(), bucket: "hour" };
  }
  const days = range === "7d" ? 7 : 30;
  const from = new Date(now);
  from.setHours(0, 0, 0, 0);
  from.setDate(from.getDate() - (days - 1));
  return { start: from.toISOString(), bucket: "day" };
}

/* ============ Announcements: one dismissible strip ============ */

function visibleOverviewAnnouncements(site: ReturnType<typeof useSite>): Announcement[] {
  if (!site.announcement_enabled) return [];
  const siteAnnouncements = site.announcements ?? [];
  const announcements = siteAnnouncements.length > 0
    ? siteAnnouncements
    : (site.announcement_title.trim() || site.announcement_content.trim())
      ? [{
          id: "legacy",
          title: site.announcement_title,
          content: site.announcement_content,
          enabled: true,
          display_mode: site.announcement_display_mode,
        }]
      : [];
  return announcements.filter((announcement) =>
    announcement.enabled &&
    (announcement.title.trim() !== "" || announcement.content.trim() !== "")
  );
}

function announcementSummary(content: string): string {
  const text = content.trim().replace(/\s+/g, " ");
  if (text.length <= 120) return text;
  return `${text.slice(0, 120)}...`;
}

const DISMISSED_KEY = "jucode-me-dismissed-announcements";

function readDismissed(): string[] {
  try {
    const raw = localStorage.getItem(DISMISSED_KEY);
    const parsed = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter((v) => typeof v === "string") : [];
  } catch {
    return [];
  }
}

function NoticeStrip({ announcements }: { announcements: Announcement[] }) {
  const { t } = useTranslation();
  const [dismissed, setDismissed] = useState<string[]>(readDismissed);
  const visible = announcements.filter((a) => !dismissed.includes(a.id || a.title));
  if (visible.length === 0) return null;
  const dismiss = (key: string) => {
    const next = [...dismissed, key];
    setDismissed(next);
    try { localStorage.setItem(DISMISSED_KEY, JSON.stringify(next)); } catch { /* per-viewer convenience only */ }
  };
  return (
    <div className="jc-me-notices">
      {visible.map((a) => {
        const key = a.id || a.title;
        return (
          <div key={key} className="jc-me-notice" role="status">
            <span className="jc-me-notice-label">{t("usage.me.notice.label")}</span>
            <div className="jc-me-notice-body">
              <span className="jc-me-notice-title">{a.title.trim() || t("usage.me.notice.label")}</span>
              {a.content.trim() && <span className="jc-me-notice-text">{announcementSummary(a.content)}</span>}
            </div>
            <button type="button" className="jc-me-notice-close" aria-label={t("usage.me.notice.close")} onClick={() => dismiss(key)}>
              <X size={14} strokeWidth={1.8} />
            </button>
          </div>
        );
      })}
    </div>
  );
}

/* ============ Key figures strip ============ */

function KeyFigures({ children, cols }: { children: ReactNode; cols: number }) {
  return (
    <div className="jc-kf" style={{ "--jc-kf-cols": cols } as CSSProperties}>
      {children}
    </div>
  );
}

function KeyFigure({
  label, value, note, action, loading,
}: {
  label: ReactNode;
  value: ReactNode;
  note?: ReactNode;
  action?: ReactNode;
  loading?: boolean;
}) {
  return (
    <div className="jc-kf-cell">
      <div className="jc-kf-head">
        <span>{label}</span>
        {action}
      </div>
      <div className="jc-kf-value">
        {loading ? <Skeleton width="7ch" height="1em" style={{ borderRadius: 4 }} /> : value}
      </div>
      {note !== undefined && (
        <div className="jc-kf-note">
          {loading ? <Skeleton width="14ch" height="12px" style={{ borderRadius: 4 }} /> : note}
        </div>
      )}
    </div>
  );
}

// Signed change vs the previous window. Raw difference, not a percentage:
// the reader keeps the same unit as the headline value.
function Delta({ current, previous, money }: { current: number; previous: number; money?: boolean }) {
  const diff = current - previous;
  const abs = money ? formatAmount(Math.abs(diff)) : Math.abs(diff).toLocaleString();
  const sign = diff > 0 ? "+" : diff < 0 ? "−" : "±";
  return <span><Trans i18nKey="usage.me.delta" values={{ value: `${sign}${abs}` }} components={{ b: <b /> }} /></span>;
}

/* ============ Consumption chart (stacked bar by model) ============ */

type ChartMetric = "requests" | "cost";

function ConsumptionChart({ scope, range }: { scope: string; range: Range }) {
  const { t } = useTranslation();
  const [metric, setMetric] = useState<ChartMetric>("requests");
  const params = rangeParams(range);
  const bucket = params.bucket;

  const { data, isLoading } = useQuery({
    queryKey: ["me-scoped", scope, "stats-by-model", params.start, params.bucket],
    // usage_log.cost_final values are platform points (1 积分 = 1 元) and the
    // aggregation endpoint sums them in that unit, so the chart sums directly.
    queryFn: () => meApi.myStatsByModel({ start: params.start, bucket: params.bucket }),
    refetchInterval: 60_000,
  });

  // Pivot [{ time, model, requests/cost }] → [{ label, modelA: n, modelB: m }]
  // over a zero-filled bucket list (an idle hour still occupies its slot,
  // so the axis stays evenly spaced), and rank models by window total so
  // the stack order and the legend agree.
  const { chart, models, totals, colors } = useMemo(() => {
    const rows = data?.rows ?? [];
    const now = new Date();
    const from = new Date(data?.start ?? params.start);
    const keyOf = (d: Date) => bucket === "hour"
      ? String(Math.floor(d.getTime() / 3_600_000))
      : `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`;
    const labelOf = (d: Date) => bucket === "hour"
      ? `${String(d.getHours()).padStart(2, "0")}:00`
      : d.toLocaleDateString(localeTag(), { month: "2-digit", day: "2-digit" });
    const slots = new Map<string, Record<string, number | string>>();
    for (const d = new Date(from); d <= now; bucket === "hour" ? d.setHours(d.getHours() + 1) : d.setDate(d.getDate() + 1)) {
      slots.set(keyOf(d), { _t: d.toISOString(), label: labelOf(d) });
    }
    const totals = new Map<string, number>();
    for (const r of rows) {
      const v = metric === "requests" ? r.requests : (parseFloat(r.cost_final_sum) || 0);
      totals.set(r.model, (totals.get(r.model) ?? 0) + v);
      const d = new Date(r.time);
      const key = keyOf(d);
      const slot = slots.get(key) ?? { _t: r.time, label: labelOf(d) };
      slot[r.model] = ((slot[r.model] as number) ?? 0) + v;
      slots.set(key, slot);
    }
    const chart = Array.from(slots.values())
      .sort((a, b) => new Date(a._t as string).getTime() - new Date(b._t as string).getTime());
    const models = Array.from(totals.keys()).sort((a, b) => (totals.get(b) ?? 0) - (totals.get(a) ?? 0));
    return { chart, models, totals, colors: colorsForModels(models) };
  }, [data, metric, bucket, params.start]);

  const fmtValue = (v: number) => (metric === "cost" ? formatAmount(v) : v.toLocaleString());

  return (
    <Card className="pn-card-flat jc-me-panel">
      <div className="jc-me-panel-head">
        <div>
          <div className="jc-me-panel-title">{t("usage.me.chart.title")}</div>
          <div className="jc-me-panel-sub">
            {t("usage.me.chart.subtitle", { unit: metric === "requests" ? t("usage.me.chart.unitRequests") : t("usage.me.chart.unitPoints") })}
          </div>
        </div>
        <SegmentedControl.Root size="1" value={metric} onValueChange={(v) => setMetric(v as ChartMetric)}>
          <SegmentedControl.Item value="requests">{t("usage.me.chart.metricRequests")}</SegmentedControl.Item>
          <SegmentedControl.Item value="cost">{t("usage.me.chart.metricCost")}</SegmentedControl.Item>
        </SegmentedControl.Root>
      </div>

      {isLoading ? (
        <Skeleton width="100%" height="260px" style={{ borderRadius: 8 }} />
      ) : models.length === 0 ? (
        <div className="jc-me-empty">{t("usage.me.chart.empty", { range: t(RANGE_LABEL_KEY[range]) })}</div>
      ) : (
        <Box style={{ width: "100%", height: 260 }}>
          <ResponsiveContainer>
            <BarChart data={chart} margin={{ top: 4, right: 4, bottom: 0, left: -12 }}>
              <CartesianGrid {...chartGrid} vertical={false} />
              <XAxis dataKey="label" {...chartAxis} interval="preserveStartEnd" minTickGap={24} />
              <YAxis {...chartAxis} tickFormatter={(v: number) => fmtValue(v)} width={64} />
              <RTooltip
                {...chartTooltipProps}
                formatter={(v: unknown) => fmtValue(Number(v))}
                labelFormatter={(_label: unknown, payload: ReadonlyArray<{ payload?: { _t?: string } }>) => {
                  const ts = payload?.[0]?.payload?._t;
                  if (!ts) return String(_label);
                  const d = new Date(ts);
                  return bucket === "hour"
                    ? d.toLocaleString(localeTag(), { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" })
                    : d.toLocaleDateString(localeTag(), { year: "numeric", month: "2-digit", day: "2-digit" });
                }}
              />
              {models.map((m, i) => (
                <Bar
                  key={m}
                  dataKey={m}
                  stackId="m"
                  fill={colors[m]}
                  maxBarSize={chartBarMaxSize}
                  radius={i === models.length - 1 ? [3, 3, 0, 0] : 0}
                  isAnimationActive={false}
                />
              ))}
            </BarChart>
          </ResponsiveContainer>
        </Box>
      )}

      {models.length > 0 && (
        <ul className="jc-me-legend" aria-label={t("usage.me.chart.legend")}>
          {models.map((m) => (
            <li key={m} className="jc-me-legend-item">
              <span className="jc-me-legend-swatch" style={{ background: colors[m] }} aria-hidden />
              <span className="jc-me-legend-name">{m}</span>
              <span className="jc-me-legend-value">{fmtValue(totals.get(m) ?? 0)}</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

/* ============ Token mix panel ============ */

function TokenMixPanel({ scope, range }: { scope: readonly unknown[]; range: Range }) {
  const { t } = useTranslation();
  const { totals, isLoading } = useTokenTotals(scope, (p) => meApi.myStats(p), rangeParams(range), DEFAULT_POLL_MS);
  // Personal accounting folds cache writes into 输入; cache hits stay separate.
  const input = totals.in + totals.cacheWrite;
  const total = input + totals.cached + totals.out;
  const parts = [
    { key: "input", label: t("usage.me.mix.input"), note: t("usage.me.mix.inputNote"), value: input },
    { key: "cached", label: t("usage.me.mix.cached"), note: undefined, value: totals.cached },
    { key: "output", label: t("usage.me.mix.output"), note: undefined, value: totals.out },
  ];
  const pct = (v: number) => (total > 0 ? `${((v / total) * 100).toFixed(1)}%` : "—");
  const hitRate = cacheHitRate(totals);

  return (
    <Card className="pn-card-flat jc-me-panel jc-mix">
      <div className="jc-me-panel-head">
        <div>
          <div className="jc-me-panel-title">{t("usage.me.mix.title")}</div>
          <div className="jc-me-panel-sub">{t("usage.me.mix.subtitle", { range: t(RANGE_LABEL_KEY[range]) })}</div>
        </div>
      </div>
      <div className="jc-mix-total">
        {isLoading ? <Skeleton width="9ch" height="1em" style={{ borderRadius: 4 }} /> : total.toLocaleString()}
      </div>
      <div className="jc-mix-track" aria-hidden>
        {parts.map((p) => (
          <span
            key={p.key}
            data-part={p.key}
            style={{ width: total > 0 ? `${(p.value / total) * 100}%` : "0%" }}
          />
        ))}
      </div>
      <div className="jc-mix-rows">
        {parts.map((p) => (
          <div key={p.key} className="jc-mix-row" data-part={p.key}>
            <span className="jc-mix-swatch" aria-hidden />
            <span className="jc-mix-row-label">
              {p.label}
              {p.note && <span className="jc-mix-row-note">{p.note}</span>}
            </span>
            <span className="jc-mix-row-pct">{isLoading ? "" : pct(p.value)}</span>
            <span className="jc-mix-row-value">
              {isLoading ? <Skeleton width="7ch" height="1em" style={{ borderRadius: 3 }} /> : p.value.toLocaleString()}
            </span>
          </div>
        ))}
      </div>
      <div className="jc-mix-foot">
        <span>{t("usage.me.mix.hitRate")}</span>
        <span className={`jc-mix-hit${totals.cached > 0 ? " is-good" : ""}`}>{isLoading ? "—" : hitRate}</span>
      </div>
    </Card>
  );
}

/* ============ Usage section: one range control drives both panels ============ */

function UsageSection({ scope, queryScope }: { scope: string; queryScope: readonly unknown[] }) {
  const { t } = useTranslation();
  const [range, setRange] = useState<Range>("7d");
  return (
    <section className="jc-me-section">
      <div className="jc-me-section-head">
        <div>
          <span className="jc-me-section-title">{t("usage.me.section.title")}</span>
          <span className="jc-me-section-sub">{t(RANGE_LABEL_KEY[range])}</span>
        </div>
        <SegmentedControl.Root size="1" value={range} onValueChange={(v) => setRange(v as Range)}>
          <SegmentedControl.Item value="24h">{t("usage.common.range24h")}</SegmentedControl.Item>
          <SegmentedControl.Item value="7d">{t("usage.common.range7d")}</SegmentedControl.Item>
          <SegmentedControl.Item value="30d">{t("usage.common.range30d")}</SegmentedControl.Item>
        </SegmentedControl.Root>
      </div>
      <div className="jc-me-usage-grid">
        <ConsumptionChart scope={scope} range={range} />
        <TokenMixPanel scope={queryScope} range={range} />
      </div>
    </section>
  );
}

/* ============ Account footer: identity, security, low-frequency actions ============ */

function AccountFooter({
  me, onEdit, onRedeem, onInvite, onTotp,
}: {
  me: MeResponse;
  onEdit: () => void;
  onRedeem: () => void;
  onInvite: () => void;
  onTotp: () => void;
}) {
  const { t } = useTranslation();
  const initial = (me.nickname?.[0] ?? me.email[0] ?? "?").toUpperCase();
  const displayName = me.nickname || me.email.split("@")[0];
  const showTotp = me.totp_setup_required || me.totp_enabled !== undefined;
  const enabled = me.totp_enabled === true;
  const nudge = me.totp_setup_required === true;
  return (
    <section className="jc-me-account" aria-label={t("usage.me.account.aria")}>
      <span className="jc-me-avatar" aria-hidden>{initial}</span>
      <div className="jc-me-identity">
        <div className="jc-me-name">
          <span>{displayName}</span>
          {me.role === "admin" && <span className="jc-me-role">{t("usage.me.account.admin")}</span>}
        </div>
        <div className="jc-me-email">{me.email}</div>
      </div>
      {showTotp && (
        <div className={`jc-me-security${enabled ? " is-on" : nudge ? " is-warn" : ""}`}>
          <span className="jc-me-security-dot" aria-hidden />
          <span>
            {enabled ? t("usage.me.account.totpOn") : nudge ? t("usage.me.account.totpNudge") : t("usage.me.account.totpOff")}
          </span>
          <Button size="1" variant={enabled || !nudge ? "ghost" : "solid"} color={enabled || !nudge ? "gray" : undefined} onClick={onTotp}>
            {enabled ? t("usage.me.account.manage") : nudge ? t("usage.me.account.enableNow") : t("usage.me.account.enable")}
          </Button>
        </div>
      )}
      <div className="jc-me-actions">
        <Button variant="soft" color="gray" onClick={onEdit}>{t("usage.me.account.editProfile")}</Button>
        <Button variant="soft" color="gray" onClick={onRedeem}>{t("usage.me.account.redeem")}</Button>
        <Button variant="soft" color="gray" onClick={onInvite}>{t("usage.me.account.invite")}</Button>
      </div>
    </section>
  );
}

/* ============ Page ============ */

// use48hStats: one hour-bucketed request covering the last 48 whole hours,
// split into the current and the previous 24h window so the key figures
// can show their change. The backend scopes the numbers by account context
// (personal / org manager / org member), so both overview variants share it.
function use48hStats(scope: string) {
  const { data } = useQuery({
    queryKey: ["me-scoped", scope, "overview-48h"],
    queryFn: () => {
      // Window computed at fetch time so refetches keep sliding.
      const start = new Date(Date.now() - 48 * 3600 * 1000);
      start.setMinutes(0, 0, 0);
      return meApi.myStats({ start: start.toISOString(), bucket: "hour" });
    },
    refetchInterval: 30_000,
  });
  return useMemo(() => {
    if (!data) return undefined;
    const split = new Date(data.start).getTime() + 24 * 3600 * 1000;
    const cur = { cost: 0, requests: 0, errors: 0 };
    const prev = { cost: 0, requests: 0, errors: 0 };
    for (const b of data.buckets) {
      const slot = new Date(b.time).getTime() >= split ? cur : prev;
      slot.cost += parseFloat(b.cost_final_sum) || 0;
      slot.requests += b.requests;
      slot.errors += b.errors;
    }
    return { cur, prev };
  }, [data]);
}

function useLifetimeStats(scope: string) {
  return useQuery({
    queryKey: ["me-scoped", scope, "lifetime-stats"],
    queryFn: meApi.myLifetimeStats,
    refetchInterval: 60_000,
  }).data;
}

export default function Me() {
  const activeOrg = useActiveOrg();
  if (activeOrg) return <OrgOverview activeOrg={activeOrg} />;
  return <PersonalOverview />;
}

function PersonalOverview() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { data: me, isLoading, error } = useQuery({ queryKey: ["me"], queryFn: meApi.me });
  const site = useSite();
  const announcements = useMemo(() => visibleOverviewAnnouncements(site), [site]);
  const day = use48hStats("personal");
  const lifetime = useLifetimeStats("personal");
  const planUsage = useQuery({
    queryKey: ["me", "usage"],
    queryFn: meApi.usage,
    refetchInterval: 20_000,
  });

  const [editOpen, setEditOpen] = useState(false);
  const [redeemOpen, setRedeemOpen] = useState(false);
  const [refundOpen, setRefundOpen] = useState(false);
  const [totpOpen, setTotpOpen] = useState(false);
  // ?totp=1 (from the console toolbar nudge) lands straight in the dialog.
  const [searchParams, setSearchParams] = useSearchParams();
  const wantsTotp = searchParams.get("totp") === "1";
  useEffect(() => {
    if (!wantsTotp || !me) return;
    setTotpOpen(true);
    setSearchParams((prev) => { prev.delete("totp"); return prev; }, { replace: true });
  }, [wantsTotp, me, setSearchParams]);

  if (isLoading) return <RouteSkeleton />;
  if (error) return <Text color="red">{String(error)}</Text>;
  if (!me) return null;

  return (
    <Box>
      <PageHeader
        title={t("usage.me.title")}
        description={t("usage.me.description")}
      />

      <div className="jc-me">
        <NoticeStrip announcements={announcements} />

        <KeyFigures cols={3}>
          <KeyFigure
            label={t("usage.me.figures.balance")}
            value={<PointAmount amount={me.balance} iconSize={20} />}
            note={
              <>
                <span>{t("usage.me.figures.rate")}</span>
                <HeldBalance amount={me.held_balance} />
                {site.self_refund_enabled && (
                  <Button variant="ghost" size="1" onClick={() => setRefundOpen(true)}>{t("usage.me.figures.selfRefund")}</Button>
                )}
                <span><Trans i18nKey="usage.me.figures.lifetime" values={{ value: lifetime ? formatAmount(lifetime.total_cost) : "—" }} components={{ b: <b /> }} /></span>
              </>
            }
            action={<Button size="1" onClick={() => navigate("/me/plans")}>{t("usage.me.figures.topUp")}</Button>}
          />
          <KeyFigure
            label={t("usage.me.figures.spend24h")}
            loading={day === undefined}
            value={day ? <PointAmount amount={day.cur.cost} iconSize={20} /> : "—"}
            note={day && <Delta current={day.cur.cost} previous={day.prev.cost} money />}
          />
          <KeyFigure
            label={t("usage.me.figures.requests24h")}
            loading={day === undefined}
            value={day ? day.cur.requests.toLocaleString() : "—"}
            note={day && (
              <>
                <Delta current={day.cur.requests} previous={day.prev.requests} />
                <span className={day.cur.errors > 0 ? "is-bad" : undefined}>
                  <Trans i18nKey="usage.me.figures.failed" values={{ value: day.cur.errors.toLocaleString() }} components={{ b: <b /> }} />
                </span>
              </>
            )}
          />
        </KeyFigures>

        <PlanUsagePanel
          plan={planUsage.data}
          loading={planUsage.isLoading}
          action={<Button size="1" variant="soft" color="gray" onClick={() => navigate("/me/usage")}>{t("usage.me.figures.viewUsage")}</Button>}
        />

        <UsageSection scope="personal" queryScope={["me"]} />

        <AccountFooter
          me={me}
          onEdit={() => setEditOpen(true)}
          onRedeem={() => setRedeemOpen(true)}
          onInvite={() => navigate("/me/invite")}
          onTotp={() => setTotpOpen(true)}
        />
      </div>

      <EditDialog
        open={editOpen}
        onOpenChange={setEditOpen}
        initial={me.nickname ?? ""}
        email={me.email}
      />
      <RedeemDialog open={redeemOpen} onOpenChange={setRedeemOpen} />
      {site.self_refund_enabled && <SelfRefundDialog open={refundOpen} onClose={() => setRefundOpen(false)} />}
      <TotpDialog open={totpOpen} onOpenChange={setTotpOpen} />
    </Box>
  );
}

/* ============ Org-context overview ============ */

function orgRoleLabel(t: TFunction, role?: string) {
  return role === "owner" ? t("usage.me.org.owner") : role === "admin" ? t("usage.me.org.admin") : t("usage.me.org.member");
}

// OrgOverview replaces the personal overview when the console is switched
// to an organization. Owners/admins see the org's finances (balance,
// monthly spend vs limit, member / key counts); members only see their
// own spend against their member limit. The usage section is the same as
// the personal page — the backend already scopes its data by the org
// header, the scoped queryKey keeps the caches apart.
function OrgOverview({ activeOrg }: { activeOrg: ActiveOrg }) {
  const { t } = useTranslation();
  const scope = activeOrg.id;
  const { data: ov, isLoading, error } = useQuery({
    queryKey: ["me-scoped", scope, "org-overview"],
    queryFn: () => meApi.orgOverview(activeOrg.id),
    refetchInterval: 30_000,
  });
  const day = use48hStats(scope);
  const planUsage = useQuery({
    queryKey: ["me", "usage"],
    queryFn: meApi.usage,
    refetchInterval: 20_000,
  });
  const navigate = useNavigate();

  if (isLoading) return <RouteSkeleton />;
  if (error) return <Text color="red">{errMsg(error)}</Text>;
  if (!ov) return null;

  const manager = ov.my_role === "owner" || ov.my_role === "admin";
  const requestsCell = (
    <KeyFigure
      label={t("usage.me.figures.requests24h")}
      loading={day === undefined}
      value={day ? day.cur.requests.toLocaleString() : "—"}
      note={day && (
        <>
          <Delta current={day.cur.requests} previous={day.prev.requests} />
          <span className={day.cur.errors > 0 ? "is-bad" : undefined}>
            <Trans i18nKey="usage.me.figures.failed" values={{ value: day.cur.errors.toLocaleString() }} components={{ b: <b /> }} />
          </span>
        </>
      )}
    />
  );

  return (
    <Box>
      <PageHeader
        title={<><span>{ov.organization.name}</span><Badge size="1" variant="soft" color={ov.my_role === "owner" ? "iris" : ov.my_role === "admin" ? "blue" : "gray"}>
            {orgRoleLabel(t, ov.my_role)}
          </Badge></>}
        description={manager ? t("usage.me.org.descManager") : t("usage.me.org.descMember")}
      />

      <div className="jc-me">
        {manager ? (
          <KeyFigures cols={4}>
            <KeyFigure
              label={t("usage.me.org.balance")}
              value={<PointAmount amount={ov.balance ?? "0"} iconSize={20} />}
              note={<><span>{t("usage.me.org.balanceNote")}</span><HeldBalance amount={ov.held_balance} /></>}
            />
            <KeyFigure
              label={t("usage.me.org.monthSpend")}
              value={<PointAmount amount={ov.monthly_spend ?? "0"} iconSize={20} />}
              note={
                <span>
                  <Trans i18nKey="usage.me.org.monthLimit" values={{ value: Number(ov.monthly_spend_limit) > 0 ? formatAmount(ov.monthly_spend_limit) : t("usage.me.org.unlimited") }} components={{ b: <b /> }} />
                </span>
              }
            />
            {requestsCell}
            <KeyFigure
              label={t("usage.me.org.members")}
              value={ov.member_count ?? "—"}
              note={<span><Trans i18nKey="usage.me.org.apiKeys" values={{ value: ov.api_key_count ?? "—" }} components={{ b: <b /> }} /></span>}
            />
          </KeyFigures>
        ) : (
          <KeyFigures cols={3}>
            <KeyFigure
              label={t("usage.me.org.myMonthSpend")}
              value={<PointAmount amount={ov.my_monthly_spend} iconSize={20} />}
              note={
                <span>
                  <Trans i18nKey="usage.me.org.monthLimit" values={{ value: Number(ov.my_monthly_spend_limit) > 0 ? formatAmount(ov.my_monthly_spend_limit) : t("usage.me.org.unlimited") }} components={{ b: <b /> }} />
                </span>
              }
            />
            <KeyFigure
              label={t("usage.me.figures.spend24h")}
              loading={day === undefined}
              value={day ? <PointAmount amount={day.cur.cost} iconSize={20} /> : "—"}
              note={day && <Delta current={day.cur.cost} previous={day.prev.cost} money />}
            />
            {requestsCell}
          </KeyFigures>
        )}

        <PlanUsagePanel
          plan={planUsage.data}
          loading={planUsage.isLoading}
          orgScope
          action={<Button size="1" variant="soft" color="gray" onClick={() => navigate("/me/usage")}>{t("usage.me.figures.viewUsage")}</Button>}
        />

        <UsageSection scope={scope} queryScope={["me-scoped", scope]} />
      </div>
    </Box>
  );
}
