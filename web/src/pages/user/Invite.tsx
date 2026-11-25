import "../../personal-responsive.css";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Badge, Box, Button, Flex, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, Copy, Link2, Mail, RefreshCw } from "lucide-react";
import { meApi, type InviteHeldReason, type Invitee, type InviteProgram, type InviteProgramPublic } from "../../api/endpoints";
import { errCode, errMsg } from "../../api/client";
import axios from "axios";
import { useTranslation } from "react-i18next";
import i18n, { localeTag } from "../../i18n";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { PointAmount } from "../../components/PointAmount";
import { copyText } from "../../lib/copy";
import { formatAmount } from "../../lib/currency";
import { PageHeader } from "../../components/PageHeader";

const PAGE_SIZE = 20;

type CopyTarget = "code" | "link";
type CopyFeedback = { target: CopyTarget; ok: boolean };

function displayAmount(value: string): string {
  return formatAmount(value, { decimals: "preserve" }) || value;
}

function signupCondition(program: InviteProgram): ReactNode {
  switch (program.signup_condition) {
    case "email_verified":
      return i18n.t("billing.invite.condition.emailVerified");
    case "any_topup":
      return i18n.t("billing.invite.condition.anyTopup");
    case "min_topup":
      return (
        <>
          {i18n.t("billing.invite.condition.minTopup")}{" "}
          <PointAmount amount={program.signup_min_topup_amount} decimals="preserve" iconSize={12} />
          {i18n.t("billing.invite.condition.minTopupEnd")}
        </>
      );
    default:
      return i18n.t("billing.invite.condition.registered");
  }
}

function ProgramRules({ program }: { program: InviteProgramPublic }) {
  const { t } = useTranslation();
  // A paused program must not promise rewards in the present tense —
  // the banner above already says the campaign is off.
  const showSignupReward = program.enabled && program.signup_reward_enabled && Number(program.signup_reward_amount) > 0;
  const showRebate = program.enabled && program.topup_rebate_enabled;
  const hasReward = showSignupReward || showRebate;
  const windowDays = program.topup_rebate_window_days;
  return (
    <div className="jc-invite-rules">
      <div className="jc-invite-section-label">{t("billing.invite.rules.title")}</div>
      <ul>
        {showSignupReward && (
          <li>
            {t("billing.invite.rules.signupRewardBefore")}{signupCondition(program)}{t("billing.invite.rules.signupRewardAfter")}{" "}
            <PointAmount amount={program.signup_reward_amount} decimals="preserve" iconSize={12} />
            {t("billing.invite.rules.signupRewardEnd")}
          </li>
        )}
        {showSignupReward && (program.signup_condition === "any_topup" || program.signup_condition === "min_topup") && (
          <li>{t("billing.invite.rules.paidScope")}</li>
        )}
        {showRebate && (
          <li>
            {windowDays > 0 ? t("billing.invite.rules.rebateWindow", { days: windowDays }) : t("billing.invite.rules.rebate")}{" "}
            {t("billing.invite.rules.rebateRate", { rate: displayAmount(program.topup_rebate_rate_pct) })}
            {windowDays > 0 && t("billing.invite.rules.rebateExpiry", { days: windowDays })}
          </li>
        )}
        {showRebate && <li>{t("billing.invite.rules.rebateExclusions")}</li>}
        {showRebate && <li>{t("billing.invite.rules.rebateRefund")}</li>}
        {hasReward && program.email_verification_hold && (
          <li>{t("billing.invite.rules.emailHold")}</li>
        )}
        {hasReward && program.risk_review && (
          <li>{t("billing.invite.rules.riskHold")}</li>
        )}
        {!hasReward && <li>{t("billing.invite.rules.none")}</li>}
        {hasReward && <li>{t("billing.invite.rules.inviterOnly")}</li>}
      </ul>
    </div>
  );
}

// emailUnverifiedNow tells "still unverified" apart from "was unverified
// when the payout was made".
function heldReasonText(reasons: InviteHeldReason[], emailUnverifiedNow: boolean): string {
  const texts = reasons.map((reason): string | null => {
    switch (reason) {
      case "inviter_email_unverified":
        return emailUnverifiedNow ? i18n.t("billing.invite.heldReason.emailUnverifiedNow") : i18n.t("billing.invite.heldReason.emailUnverifiedThen");
      case "inviter_account_too_new":
        return i18n.t("billing.invite.heldReason.accountTooNew");
      case "inviter_account_restricted":
        return i18n.t("billing.invite.heldReason.accountRestricted");
      case "self_referral_suspected":
        return i18n.t("billing.invite.heldReason.selfReferral");
      case "daily_limit":
        return i18n.t("billing.invite.heldReason.dailyLimit");
      case "amount_review":
        return i18n.t("billing.invite.heldReason.amountReview");
      default:
        return null;
    }
  }).filter((text): text is string => text !== null);
  return texts.length > 0 ? texts.join(i18n.t("billing.invite.heldReason.separator")) : i18n.t("billing.invite.heldReason.notYet");
}

function statusMeta(status: string): {
  label: "pending" | "qualified" | "held" | "paid" | "rejected" | "voided" | "none";
  color: "gray" | "amber" | "green" | "red" | "blue";
} {
  switch (status.trim().toLowerCase()) {
    // No reward row exists for this invitee at all.
    case "none":
    case "":
      return { label: "none", color: "gray" };
    // The binding stays listed after a reward is reversed (order refunded)
    // or an admin voids it — a reward that came and went is not the same as
    // never having had one.
    case "voided":
      return { label: "voided", color: "gray" };
    case "pending":
    case "waiting":
    case "unqualified":
      return { label: "pending", color: "gray" };
    case "qualified":
      return { label: "qualified", color: "blue" };
    case "paid":
    case "credited":
    case "approved":
      return { label: "paid", color: "green" };
    case "rejected":
    case "failed":
    case "invalid":
      return { label: "rejected", color: "red" };
    case "held":
    case "reviewing":
      return { label: "held", color: "amber" };
    default:
      return { label: "pending", color: "gray" };
  }
}

function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString(localeTag(), {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  });
}

function InviteeStatus({ invitee, verifyAction }: { invitee: Invitee; verifyAction: ReactNode }) {
  const { t } = useTranslation();
  const meta = statusMeta(invitee.signup_reward_status);
  return (
    <Flex direction="column" gap="2">
      <Flex align="center" gap="2" wrap="wrap">
        <Badge size="1" variant="soft" color={meta.color}>{t(`billing.invite.status.${meta.label}`)}</Badge>
        {invitee.signup_reward_status !== "none" && invitee.signup_reward_amount && Number(invitee.signup_reward_amount) > 0 && (
          <span className="jc-invite-table-amount">
            <PointAmount amount={invitee.signup_reward_amount} decimals="preserve" iconSize={11} />
          </span>
        )}
      </Flex>
      {invitee.signup_email_verification_required ? (
        <Flex direction="column" align="start" gap="2">
          <Text size="1" color="amber">
            {invitee.signup_reward_held_reasons.length === 1
              ? t("billing.invite.row.emailUnverifiedAuto")
              : t("billing.invite.row.emailUnverifiedFirst")}
          </Text>
          {verifyAction}
        </Flex>
      ) : invitee.signup_reward_status === "held" && (
        <Text size="1" color="amber">
          {t("billing.invite.row.rewardHeld", { reason: heldReasonText(invitee.signup_reward_held_reasons, false) })}
        </Text>
      )}
    </Flex>
  );
}

function SignupProgress({ invitee }: { invitee: Invitee }) {
  const { t } = useTranslation();
  if (invitee.signup_reward_status === "none") return <>—</>;
  if (invitee.signup_condition === "min_topup" || invitee.signup_condition === "any_topup") {
    const paid = invitee.signup_paid_amount;
    const required = invitee.signup_required_amount;
    return (
      <Flex direction="column" gap="1">
        <Text size="1">{t("billing.invite.row.paidSoFar")} <PointAmount amount={paid} decimals="preserve" iconSize={11} />
          {invitee.signup_condition === "min_topup" && <> / <PointAmount amount={required} decimals="preserve" iconSize={11} /></>}
        </Text>
        <Text size="1" color="gray">
          {invitee.signup_condition === "min_topup"
            ? (Number(paid) >= Number(required) ? t("billing.invite.row.reached") : t("billing.invite.row.belowThreshold"))
            : (Number(paid) > 0 ? t("billing.invite.row.reached") : t("billing.invite.row.waitingFirstPay"))}
        </Text>
      </Flex>
    );
  }
  if (invitee.signup_condition === "email_verified") {
    return <Text size="1">{invitee.signup_email_verified ? t("billing.invite.row.inviteeVerified") : t("billing.invite.row.waitingInviteeVerify")}</Text>;
  }
  return <Text size="1">{t("billing.invite.row.noThreshold")}</Text>;
}

export default function Invite() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [offset, setOffset] = useState(0);
  const [cooldown, setCooldown] = useState(0);
  const [copyFeedback, setCopyFeedback] = useState<CopyFeedback | null>(null);
  const feedbackTimer = useRef<number | undefined>(undefined);
  const invite = useQuery({
    queryKey: ["me", "invite", PAGE_SIZE, offset],
    queryFn: () => meApi.invite({ limit: PAGE_SIZE, offset }),
    retry: 1,
    refetchOnWindowFocus: "always",
  });
  const verification = useMutation({
    mutationFn: meApi.sendEmailVerification,
    meta: { suppressErrorToast: true },
    onSuccess: (result) => setCooldown(result.cooldown_seconds),
    onError: (error) => {
      if (errCode(error) === "email_already_verified") {
        qc.invalidateQueries({ queryKey: ["me"] });
      }
      if (axios.isAxiosError(error) && error.response?.status === 429) {
        setCooldown(error.response.data.cooldown_seconds ?? 60);
      }
    },
  });

  useEffect(() => {
    if (cooldown <= 0) return;
    const timer = window.setTimeout(() => setCooldown((value) => value - 1), 1000);
    return () => window.clearTimeout(timer);
  }, [cooldown]);

  useEffect(() => () => {
    if (feedbackTimer.current !== undefined) window.clearTimeout(feedbackTimer.current);
  }, []);

  const handleCopy = async (target: CopyTarget, value: string) => {
    const ok = await copyText(value);
    setCopyFeedback({ target, ok });
    if (feedbackTimer.current !== undefined) window.clearTimeout(feedbackTimer.current);
    feedbackTimer.current = window.setTimeout(() => setCopyFeedback(null), 1800);
  };

  if (invite.isPending) return <RouteSkeleton />;

  if (invite.isError || !invite.data) {
    return (
      <Box className="jc-invite-page">
        <PageHeader
          title={t("billing.invite.title")}
          description={t("billing.invite.description")}
        />
        <div className="jc-invite-error" role="alert">
          <div>
            <strong>{t("billing.invite.loadFailedTitle")}</strong>
            <p>{t("billing.invite.loadFailedBody")}</p>
          </div>
          <Button variant="soft" onClick={() => invite.refetch()}>
            <RefreshCw size={15} /> {t("billing.invite.reload")}
          </Button>
        </div>
      </Box>
    );
  }

  const data = invite.data;
  const invitees = Array.isArray(data.invitees) ? data.invitees : [];
  const total = typeof data.total === "number" ? data.total : invitees.length;
  const hasPrevious = offset > 0;
  // Without a server-side total, a full page is the only hint that
  // more rows may exist.
  const hasNext = typeof data.total === "number"
    ? offset + invitees.length < data.total
    : invitees.length === PAGE_SIZE;
  const firstRow = total === 0 ? 0 : offset + 1;
  const lastRow = Math.min(offset + invitees.length, total);
  const codeFeedback = copyFeedback?.target === "code" ? copyFeedback : null;
  const linkFeedback = copyFeedback?.target === "link" ? copyFeedback : null;
  const needsVerification = data.inviter_email_verification_required || invitees.some(
    (item) => item.signup_email_verification_required || item.rebate_email_verification_required,
  );
  const verifyAction = (
    <Button
      size="1"
      variant="soft"
      loading={verification.isPending}
      disabled={cooldown > 0}
      onClick={() => verification.mutate()}
    >
      <Mail size={14} />
      {cooldown > 0
        ? t("billing.invite.resendIn", { seconds: cooldown })
        : verification.isSuccess ? t("billing.invite.resend") : t("billing.invite.verifyEmail")}
    </Button>
  );

  return (
    <Box className="jc-invite-page">
      <PageHeader
        title={t("billing.invite.title")}
        description={t("billing.invite.description")}
      />

      {!data.program.enabled && (
        <div className="jc-invite-disabled" role="status">
          <strong>{t("billing.invite.disabledTitle")}</strong>
          <span>{t("billing.invite.disabledBody")}</span>
        </div>
      )}
      {needsVerification && (
        <div className="jc-invite-disabled" role="status">
          <strong>{t("billing.invite.unverifiedTitle")}</strong>
          <Flex direction="column" gap="1" style={{ flex: 1 }}>
            <span>{t("billing.invite.unverifiedBody")}</span>
            {verification.isSuccess && (
              <Text size="2" color="green">{t("billing.invite.verificationSent", { email: verification.data.email })}</Text>
            )}
            {verification.isError && <Text size="2" color="red" role="alert">{errMsg(verification.error)}</Text>}
          </Flex>
          {verifyAction}
        </div>
      )}

      <section className="jc-invite-intro">
        <div className="jc-invite-share">
          <div className="jc-invite-section-label">{t("billing.invite.yourCode")}</div>
          <div className="jc-invite-code">{data.invite_code || "—"}</div>
          <Flex gap="2" wrap="wrap" mt="4">
            <Button
              variant="soft"
              disabled={!data.invite_code}
              onClick={() => handleCopy("code", data.invite_code)}
            >
              <Copy size={15} />
              {codeFeedback ? (codeFeedback.ok ? t("billing.invite.codeCopied") : t("billing.invite.copyFailed")) : t("billing.invite.copyCode")}
            </Button>
            <Button
              disabled={!data.invite_url}
              onClick={() => handleCopy("link", data.invite_url)}
            >
              <Link2 size={15} />
              {linkFeedback ? (linkFeedback.ok ? t("billing.invite.linkCopied") : t("billing.invite.copyFailed")) : t("billing.invite.copyLink")}
            </Button>
          </Flex>
          <div className="jc-invite-link" title={data.invite_url}>{data.invite_url || t("billing.invite.noLink")}</div>
          {copyFeedback && !copyFeedback.ok && (
            <div className="jc-invite-copy-error" role="status">{t("billing.invite.copyFailedManual")}</div>
          )}
        </div>
        <ProgramRules program={data.program} />
      </section>

      <section className="jc-invite-stats" aria-label={t("billing.invite.statsAria")}>
        <div>
          <span>{t("billing.invite.statInvited")}</span>
          <strong>{data.stats.invited_count.toLocaleString(localeTag())}</strong>
        </div>
        <div>
          <span>{t("billing.invite.statPending")}</span>
          <strong>{data.stats.pending_count.toLocaleString(localeTag())}</strong>
        </div>
        <div>
          <span>{t("billing.invite.statSignupPaid")}</span>
          <strong><PointAmount amount={data.stats.signup_reward_paid} decimals="preserve" iconSize={18} /></strong>
        </div>
        <div>
          <span>{t("billing.invite.statRebatePaid")}</span>
          <strong><PointAmount amount={data.stats.topup_rebate_paid} decimals="preserve" iconSize={18} /></strong>
        </div>
      </section>

      <section className="jc-invite-list">
        <Flex justify="between" align="end" gap="3" wrap="wrap" className="jc-invite-list-head">
          <div>
            <h2>{t("billing.invite.listTitle")}</h2>
            <p>{t("billing.invite.listHint")}</p>
          </div>
          {typeof data.total === "number" && total > 0 && (
            <span>{t("billing.invite.range", { first: firstRow, last: lastRow, total: total.toLocaleString(localeTag()) })}</span>
          )}
        </Flex>

        {invitees.length === 0 ? (
          <div className="jc-invite-empty">{t("billing.invite.empty")}</div>
        ) : (
          <div className="jc-invite-table-scroll">
            <table className="jc-invite-table">
              <thead>
                <tr>
                  <th>{t("billing.invite.th.friend")}</th>
                  <th>{t("billing.invite.th.joined")}</th>
                  <th>{t("billing.invite.th.signupReward")}</th>
                  <th>{t("billing.invite.th.progress")}</th>
                  {/* Spans retired consume rebates too, so not "充值返利". */}
                  <th>{t("billing.invite.th.rebate")}</th>
                </tr>
              </thead>
              <tbody>
                {invitees.map((invitee) => (
                  <tr key={invitee.user_id}>
                    <td>{invitee.email_masked || "—"}</td>
                    <td>{formatDate(invitee.created_at)}</td>
                    <td><InviteeStatus invitee={invitee} verifyAction={verifyAction} /></td>
                    <td><SignupProgress invitee={invitee} /></td>
                    <td>
                      <Flex direction="column" gap="1">
                        <span><PointAmount amount={invitee.rebate_paid} decimals="preserve" iconSize={11} /> {t("billing.invite.row.rebatePaid")}</span>
                        {Number(invitee.rebate_held) > 0 && (
                          <Text size="1" color="amber">
                            <PointAmount amount={invitee.rebate_held} decimals="preserve" iconSize={11} /> {t("billing.invite.row.rebateHeld")}
                            {heldReasonText(invitee.rebate_held_reasons, invitee.rebate_email_verification_required)}
                            {invitee.rebate_email_verification_required && invitee.rebate_held_reasons.length === 1 ? t("billing.invite.row.rebateAutoAfterVerify") : t("billing.invite.row.rebateNeedsReview")}
                          </Text>
                        )}
                        {invitee.rebate_email_verification_required && verifyAction}
                      </Flex>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        {(hasPrevious || hasNext) && (
          <Flex justify="end" gap="2" mt="3" className="jc-invite-pager">
            <Button
              size="1"
              variant="soft"
              disabled={!hasPrevious || invite.isFetching}
              onClick={() => setOffset((value) => Math.max(0, value - PAGE_SIZE))}
            >
              <ChevronLeft size={14} /> {t("billing.invite.prev")}
            </Button>
            <Button
              size="1"
              variant="soft"
              disabled={!hasNext || invite.isFetching}
              onClick={() => setOffset((value) => value + PAGE_SIZE)}
            >
              {t("billing.invite.next")} <ChevronRight size={14} />
            </Button>
          </Flex>
        )}
      </section>
    </Box>
  );
}
