import type { ReactNode } from "react";
import type { OrgOverview, PlanUsage } from "../api/endpoints";
import { useTranslation } from "react-i18next";
import { PointAmount } from "./PointAmount";
import { localeTag } from "../i18n";

export function isUnlimitedCap(amount?: string | null): boolean {
  if (amount == null || amount.trim() === "") return true;
  const value = Number(amount);
  return !Number.isFinite(value) || value <= 0;
}

export function formatPlanDate(iso?: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleDateString(localeTag());
}

export function PlanUsagePanel({
  plan,
  loading,
  orgScope = false,
  action,
}: {
  plan?: PlanUsage | null;
  loading: boolean;
  orgScope?: boolean;
  action?: ReactNode;
}) {
  const { t } = useTranslation();
  if (loading || !plan?.has_active_plan) return null;
  return (
    <section className="jc-overview-plan" aria-label={t("keys.account.subscription")}>
      <div className="jc-overview-plan-head">
        <div className="jc-key-account-label">{t("keys.account.subscription")}</div>
        {action}
      </div>
      <PlanBody plan={plan} loading={false} orgScope={orgScope} balance={undefined} balanceLoading={false} />
    </section>
  );
}

export default function KeyAccountStatus({
  scope,
  plan,
  planLoading,
  balance,
  balanceLoading,
  org,
  orgLoading = false,
}: {
  scope: "personal" | "organization";
  plan?: PlanUsage | null;
  planLoading: boolean;
  balance?: string;
  balanceLoading: boolean;
  org?: OrgOverview;
  orgLoading?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <section className="jc-key-account" aria-label={t("keys.account.quota")}>
      {plan?.has_active_plan && (
        <div className="jc-key-account-block">
          <div className="jc-key-account-label">{t("keys.account.subscription")}</div>
          <PlanBody
            plan={plan}
            loading={planLoading}
            orgScope={scope === "organization"}
            balance={balance}
            balanceLoading={balanceLoading}
          />
        </div>
      )}
      <div className="jc-key-account-block">
        <div className="jc-key-account-label">{scope === "organization" ? t("keys.account.orgQuota") : t("keys.account.wallet")}</div>
        {scope === "organization" ? (
          <OrgBody overview={org} loading={orgLoading} />
        ) : balanceLoading ? (
          <div className="jc-key-account-note">{t("keys.account.reading")}</div>
        ) : (
          <Meter label={t("keys.account.balance")} used={balance || "0"} cap={null} bare />
        )}
      </div>
    </section>
  );
}

function PlanBody({
  plan, loading, orgScope, balance, balanceLoading,
}: {
  plan?: PlanUsage | null;
  loading: boolean;
  orgScope: boolean;
  balance?: string;
  balanceLoading: boolean;
}) {
  const { t } = useTranslation();
  if (loading) return <div className="jc-key-account-note">{t("keys.account.reading")}</div>;
  if (!plan?.has_active_plan) return <div className="jc-key-account-note">{t("keys.account.noPlan")}</div>;
  const expire = formatPlanDate(plan.expire_at);
  const paygo = plan.plan_type === "pay_as_you_go";
  return (
    <>
      <div className="jc-key-account-title">
        {plan.plan_name || t("keys.account.subscription")}
        <span>{paygo ? t("keys.account.paygoPlan") : t("keys.account.quotaPlan")}</span>
        {expire && <span>{t("keys.account.until", { date: expire })}</span>}
      </div>
      {paygo ? (
        <div className="jc-key-account-meters">
          <Meter label={t("keys.account.spend30d")} used={plan.monthly_spend_used} cap={plan.monthly_spend_limit} />
          {orgScope && (balance != null || balanceLoading) && (
            balanceLoading
              ? <div className="jc-key-account-note">{t("keys.account.walletReading")}</div>
              : <Meter label={t("keys.account.personalWallet")} used={balance || "0"} cap={null} bare />
          )}
        </div>
      ) : (
        <div className="jc-key-account-meters">
          <Meter label={t("keys.account.window5h")} used={plan.used_5h} cap={plan.quota_5h} />
          <Meter label={t("keys.account.window7d")} used={plan.used_weekly} cap={plan.quota_weekly} />
          <Meter label={t("keys.account.window30d")} used={plan.used_monthly} cap={plan.quota_monthly} />
        </div>
      )}
      {orgScope && (
        <div className="jc-key-account-note">
          {paygo ? t("keys.account.paygoOrgNote") : t("keys.account.planOrgNote")}
        </div>
      )}
    </>
  );
}

function OrgBody({ overview, loading }: { overview?: OrgOverview; loading: boolean }) {
  const { t } = useTranslation();
  if (loading) return <div className="jc-key-account-note">{t("keys.account.reading")}</div>;
  if (!overview) return <div className="jc-key-account-note">{t("keys.account.orgUnavailable")}</div>;
  const manager = overview.my_role === "owner" || overview.my_role === "admin";
  return (
    <div className="jc-key-account-meters">
      {overview.balance != null && <Meter label={t("keys.account.orgBalance")} used={overview.balance} cap={null} bare />}
      <Meter label={t("keys.account.myMonthly")} used={overview.my_monthly_spend} cap={overview.my_monthly_spend_limit} />
      <Meter label={t("keys.account.myTotal")} used={overview.my_total_spend} cap={overview.my_total_spend_limit} />
      {manager && <Meter label={t("keys.account.orgMonthly")} used={overview.monthly_spend} cap={overview.monthly_spend_limit} />}
    </div>
  );
}

function Meter({ label, used, cap, bare = false }: { label: string; used?: string; cap?: string | null; bare?: boolean }) {
  const { t } = useTranslation();
  return (
    <div className="jc-key-account-meter">
      <span>{label}</span>
      <span className="jc-key-account-value">
        <PointAmount amount={used || "0"} iconSize={12} />
        {!bare && (isUnlimitedCap(cap) ? <span> / {t("keys.shared.unlimited")}</span> : <span> / <PointAmount amount={cap} iconSize={12} /></span>)}
      </span>
    </div>
  );
}
