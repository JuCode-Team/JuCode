import "../../personal-responsive.css";
import { useMemo, useState } from "react";
import { Box, Button, Card, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { meApi, type InviteCouponConflict, type Plan, type SiteOrder } from "../../api/endpoints";
import { publicApi } from "../../api/public";
import { errMsg } from "../../api/client";
import ModelBrandIcon from "../../components/ModelBrandIcon";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import PremiumButton from "../../components/PremiumButton";
import { formatAmount } from "../../lib/currency";
import PaymentDialog from "../../components/PaymentDialog";
import { ClearableTextField } from "../../components/ClearableTextField";
import { InviteCouponConfirm, hasInviteConflict } from "../../components/InviteCouponConfirm";
import { PageHeader } from "../../components/PageHeader";
import { useSite } from "../../store/site";
import i18n from "../../i18n";

// 购买页顺序按频率排：先充值（概览「充值」按钮的落点），再套餐；优惠券是
// 低频动作，折叠在充值面板里。套餐卡去掉每张都一样的说明，只留会变的
// 数字；推荐与选中都用 1px 描边表达，不用渐变和阴影。

function planPeriodLabel(days: number): string {
  if (days === 30 || days === 31) return i18n.t("billing.plans.period.month");
  if (days === 7) return i18n.t("billing.plans.period.week");
  if (days === 365) return i18n.t("billing.plans.period.year");
  return i18n.t("billing.plans.period.days", { count: days });
}

function hasPositiveAmount(value?: string): boolean {
  return parseFloat(value || "0") > 0;
}

function quotaSummary(plan: Plan): string {
  const parts: string[] = [];
  if (hasPositiveAmount(plan.quota_5h)) parts.push(i18n.t("billing.plans.quota5h", { amount: formatAmount(plan.quota_5h) }));
  if (hasPositiveAmount(plan.quota_weekly)) parts.push(i18n.t("billing.plans.quotaWeekly", { amount: formatAmount(plan.quota_weekly) }));
  if (hasPositiveAmount(plan.quota_monthly)) parts.push(i18n.t("billing.plans.quotaMonthly", { amount: formatAmount(plan.quota_monthly) }));
  return parts.length > 0
    ? i18n.t("billing.plans.quotaSummary", { parts: parts.join(i18n.t("billing.plans.quotaSeparator")) })
    : i18n.t("billing.plans.quotaFallback");
}

const COLLAPSED_MODEL_COUNT = 6;

function discountedPrice(value: string, discountPct: number): string {
  const amount = Number(value);
  if (!Number.isFinite(amount)) return value;
  return (Math.round(amount * (100 - discountPct)) / 100).toFixed(2);
}

function trimPrice(value: string): string {
  const n = Number(value);
  if (!Number.isFinite(n)) return value;
  return Math.round(n) === n ? String(n) : n.toFixed(2);
}

type CouponState = {
  code: string;
  discountPct: number;
  applied: boolean;
  checking: boolean;
  error: unknown;
  inviteConflict: InviteCouponConflict | null;
  onChange: (value: string) => void;
  onApply: () => void;
};

// 优惠券折叠在充值面板底部；带 ?coupon= 进入或已应用时默认展开。
function CouponRow({ coupon }: { coupon: CouponState }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(coupon.code.trim() !== "");
  const expanded = open || coupon.applied;
  return (
    <div className="jc-topup-coupon">
      <button type="button" className="jc-form-toggle" aria-expanded={expanded} onClick={() => setOpen((s) => !s)}>
        {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        {t("billing.plans.coupon.toggle")}
        <small>
          {coupon.applied
            ? (coupon.discountPct > 0 ? t("billing.plans.coupon.appliedDiscount", { discount: coupon.discountPct }) : t("billing.plans.coupon.applied"))
            : t("billing.plans.coupon.hint")}
        </small>
      </button>
      {expanded && (
        <div className="jc-topup-coupon-row">
          <ClearableTextField
            value={coupon.code}
            onChange={(event) => coupon.onChange(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && coupon.code.trim()) coupon.onApply();
            }}
            placeholder={t("billing.plans.coupon.placeholder")}
            autoComplete="off"
            spellCheck={false}
          />
          <Button variant="soft" color="gray" disabled={!coupon.code.trim() || coupon.applied} loading={coupon.checking} onClick={coupon.onApply}>
            {coupon.applied ? t("billing.plans.coupon.applied") : t("billing.plans.coupon.apply")}
          </Button>
          {coupon.error != null && <span className="jc-form-error">{t("billing.plans.coupon.invalid", { error: errMsg(coupon.error) })}</span>}
        </div>
      )}
    </div>
  );
}

function TopupPanel({ coupon, couponsEnabled, onCreated }: { coupon: CouponState; couponsEnabled: boolean; onCreated: (order: SiteOrder) => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const presets = (useSite().topup_presets ?? []).filter((n) => n > 0).map(String);
  const [amount, setAmount] = useState("");
  const canSubmit = parseFloat(amount || "0") > 0;
  const couponPending = !!coupon.code.trim() && !coupon.applied;
  const [confirmOpen, setConfirmOpen] = useState(false);
  const createTopup = useMutation({
    mutationFn: (withCoupon: boolean) => meApi.createMyOrder({
      kind: "balance",
      amount,
      currency: "CNY",
      coupon_code: withCoupon ? coupon.code.trim() || undefined : undefined,
    }),
    onSuccess: (order) => {
      setAmount("");
      qc.invalidateQueries({ queryKey: ["me", "orders"] });
      onCreated(order);
    },
    onSettled: () => setConfirmOpen(false),
  });
  const submit = () => {
    if (coupon.applied && hasInviteConflict(coupon.inviteConflict)) setConfirmOpen(true);
    else createTopup.mutate(true);
  };

  return (
    <Card id="topup" className="pn-card-flat jc-topup-panel">
      <div className="jc-topup-grid">
        <div className="jc-topup-intro">
          <h2>{t("billing.plans.topup.title")}</h2>
          <p>{t("billing.plans.topup.intro")}</p>
          <dl>
            <dt>{t("billing.plans.topup.unit")}</dt><dd>{t("billing.plans.topup.unitValue")}</dd>
            <dt>{t("billing.plans.topup.credit")}</dt><dd>{t("billing.plans.topup.creditValue")}</dd>
            {coupon.discountPct > 0 && (
              <><dt>{t("billing.plans.topup.discount")}</dt><dd>{t("billing.plans.topup.discountValue", { discount: coupon.discountPct })}</dd></>
            )}
          </dl>
        </div>

        <div className="jc-topup-form">
          {presets.length > 0 && <div className="jc-field">
            <span className="jc-field-label">{t("billing.plans.topup.amount")}</span>
            <div className="jc-topup-preset-grid" role="group" aria-label={t("billing.plans.topup.presetsAria")}>
              {presets.map((value) => {
                const active = amount === value;
                return (
                  <button
                    key={value}
                    type="button"
                    className={`jc-topup-preset ${active ? "is-active" : ""}`}
                    aria-pressed={active}
                    onClick={() => setAmount(value)}
                  >
                    <span className="jc-topup-preset-amount">{value}</span>
                    <span className="jc-topup-preset-sub">¥ {coupon.discountPct > 0 ? discountedPrice(value, coupon.discountPct) : value}</span>
                  </button>
                );
              })}
            </div>
          </div>}

          <div className="jc-topup-custom">
            <label className="jc-field">
              <span className="jc-field-label">{t("billing.plans.topup.customAmount")}</span>
              <ClearableTextField
                size="3"
                type="number"
                min="1"
                placeholder={t("billing.plans.topup.customPlaceholder")}
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
              {coupon.discountPct > 0 && canSubmit && (
                <span className="jc-field-hint">{t("billing.plans.topup.customHint", { amount, payable: discountedPrice(amount, coupon.discountPct) })}</span>
              )}
            </label>
            <Button
              disabled={!canSubmit || couponPending}
              loading={createTopup.isPending}
              onClick={submit}
              size="3"
              style={{ minWidth: 132 }}
            >
              {couponPending ? t("billing.plans.topup.applyCouponFirst") : t("billing.plans.topup.create")}
            </Button>
          </div>
          {createTopup.isError && <div className="jc-form-error">{errMsg(createTopup.error)}</div>}
          {couponsEnabled && <CouponRow coupon={coupon} />}
          <InviteCouponConfirm
            conflict={coupon.inviteConflict}
            open={confirmOpen}
            loading={createTopup.isPending}
            onUseCoupon={() => createTopup.mutate(true)}
            onDropCoupon={() => createTopup.mutate(false)}
            onCancel={() => setConfirmOpen(false)}
          />
        </div>
      </div>
    </Card>
  );
}

function PlanCard({
  plan, recommended, selected, isCurrent, isPending, onSelect, onCreateOrder, models, discountPct, couponNeedsApply,
}: {
  plan: Plan;
  recommended: boolean;
  selected: boolean;
  isCurrent: boolean;
  isPending: boolean;
  onSelect: () => void;
  onCreateOrder: () => void;
  models: string[];
  discountPct: number;
  couponNeedsApply: boolean;
}) {
  const { t } = useTranslation();
  const [modelsExpanded, setModelsExpanded] = useState(false);
  const regularPrice = trimPrice(plan.price);
  const main = discountPct > 0 ? discountedPrice(plan.price, discountPct) : regularPrice;
  const groups = plan.allowed_groups ?? [];
  const isBalancePlan = plan.type === "pay_as_you_go";
  const showCurrent = !isBalancePlan && isCurrent;
  const granted = isBalancePlan && hasPositiveAmount(plan.initial_balance) && trimPrice(plan.initial_balance) !== regularPrice;
  const facts: Array<[string, string]> = [];
  const points = (amount: string) => t("billing.plans.card.points", { amount: formatAmount(amount) });
  if (granted) facts.push([t("billing.plans.card.granted"), points(plan.initial_balance)]);
  if (!isBalancePlan) {
    if (hasPositiveAmount(plan.quota_5h)) facts.push([t("billing.plans.card.reset5h"), points(plan.quota_5h)]);
    if (hasPositiveAmount(plan.quota_weekly)) facts.push([t("billing.plans.card.resetWeekly"), points(plan.quota_weekly)]);
    if (hasPositiveAmount(plan.quota_monthly)) facts.push([t("billing.plans.card.resetMonthly"), points(plan.quota_monthly)]);
    facts.push([t("billing.plans.card.period"), planPeriodLabel(plan.period_days)]);
  }
  const visibleModels = modelsExpanded ? models : models.slice(0, COLLAPSED_MODEL_COUNT);

  return (
    <div
      className={`jc-plan-card ${selected ? "is-selected" : ""}`}
      data-recommended={recommended || undefined}
      onClick={onSelect}
    >
      <div className="jc-plan-head">
        <h3 className="jc-plan-name">{plan.name}</h3>
        {showCurrent
          ? <span className="jc-fact-chip" data-tone="success">{t("billing.plans.card.current")}</span>
          : recommended && <span className="jc-fact-chip" data-tone="primary">{t("billing.plans.card.recommended")}</span>}
      </div>
      <p className="jc-plan-desc">
        {plan.description || (isBalancePlan ? t("billing.plans.card.balanceDesc") : quotaSummary(plan))}
      </p>

      <div className="jc-plan-price">
        <strong>{main}</strong>
        <span>{t("billing.plans.card.priceUnit")}{!isBalancePlan && ` / ${planPeriodLabel(plan.period_days)}`}</span>
      </div>
      {discountPct > 0 && (
        <div className="jc-plan-price-note">{t("billing.plans.card.couponNote", { discount: discountPct })} <s>{regularPrice}</s></div>
      )}

      {facts.length > 0 && (
        <div className="jc-plan-facts">
          {facts.map(([k, v]) => <div key={k}><span>{k}</span><span>{v}</span></div>)}
        </div>
      )}

      <div className="jc-plan-models">
        <div className="jc-plan-models-label">
          {groups.length === 0 ? t("billing.plans.card.allGroups") : t("billing.plans.card.groups", { count: groups.length })}
          {models.length > 0 && t("billing.plans.card.models", { count: models.length })}
        </div>
        <div className="jc-fact-chips">
          {groups.map((g) => <span key={g.id} className="jc-fact-chip"><span className="jc-fact-chip-text">{g.name}</span></span>)}
          {visibleModels.map((name) => (
            <span key={name} className="jc-fact-chip is-model">
              <ModelBrandIcon model={name} size={12} />
              <span className="jc-fact-chip-text">{name}</span>
            </span>
          ))}
          {models.length > COLLAPSED_MODEL_COUNT && (
            <button
              type="button"
              className="jc-fact-chip is-more"
              onClick={(e) => { e.stopPropagation(); setModelsExpanded((v) => !v); }}
            >
              {modelsExpanded ? t("billing.plans.card.collapse") : t("billing.plans.card.more", { count: models.length - COLLAPSED_MODEL_COUNT })}
            </button>
          )}
        </div>
      </div>

      <div className="jc-plan-cta">
        <PremiumButton
          onClick={(e) => { e.stopPropagation(); onSelect(); onCreateOrder(); }}
          disabled={showCurrent || isPending || couponNeedsApply}
          variant={selected && !showCurrent ? "primary" : "secondary"}
          size="block"
        >
          {showCurrent
            ? t("billing.plans.card.isCurrent")
            : couponNeedsApply
              ? t("billing.plans.card.applyCouponFirst")
              : isPending
                ? t("billing.plans.card.creating")
                : isBalancePlan ? t("billing.plans.card.buyCredit") : t("billing.plans.card.buyPlan")}
        </PremiumButton>
      </div>
    </div>
  );
}

function PlanGroup({
  title, description, plans, currentPlanId, createOrder, emptyText, modelsByGroup, allModels, discountPct, couponNeedsApply,
}: {
  title: string;
  description: string;
  plans: Plan[];
  currentPlanId?: string;
  createOrder: ReturnType<typeof useMutation<SiteOrder, Error, string>>;
  emptyText?: string;
  modelsByGroup: Map<string, string[]>;
  allModels: string[];
  discountPct: number;
  couponNeedsApply: boolean;
}) {
  const { t } = useTranslation();
  const recommendIdx = plans.length >= 3 ? 1 : 0;
  // 选中态只是视觉上的“正在看这一张”，默认落在推荐卡；购买仍由卡内按钮触发。
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const activeId = selectedId ?? plans[recommendIdx]?.id ?? null;
  const modelsFor = (plan: Plan): string[] => {
    const groups = plan.allowed_groups ?? [];
    const names = groups.length === 0
      ? allModels
      : [...new Set(groups.flatMap((g) => modelsByGroup.get(g.id) ?? []))];
    return names.sort((a, b) => a.localeCompare(b));
  };

  return (
    <section className="jc-plan-section">
      <div className="jc-plan-group-head">
        <h2>{title}</h2>
        <p>{description}</p>
      </div>
      {plans.length === 0 ? (
        <div className="jc-plan-empty">{emptyText || t("billing.plans.groupEmpty")}</div>
      ) : (
        <div className={`jc-plan-grid ${plans.length === 2 ? "is-two" : ""}`}>
          {plans.map((p, i) => (
            <PlanCard
              key={p.id}
              plan={p}
              recommended={plans.length > 1 && i === recommendIdx}
              selected={activeId === p.id}
              isCurrent={currentPlanId === p.id}
              isPending={createOrder.isPending && createOrder.variables === p.id}
              onSelect={() => setSelectedId(p.id)}
              onCreateOrder={() => createOrder.mutate(p.id)}
              models={modelsFor(p)}
              discountPct={discountPct}
              couponNeedsApply={couponNeedsApply}
            />
          ))}
        </div>
      )}
    </section>
  );
}

export default function UserPlans() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const site = useSite();
  const [searchParams] = useSearchParams();
  const { data: plans, isLoading } = useQuery({ queryKey: ["available-plans"], queryFn: meApi.availablePlans });
  const { data: me } = useQuery({ queryKey: ["me"], queryFn: meApi.me });
  // The public catalog carries each model's group memberships; inverting it
  // gives the model list behind a plan's allowed_groups without an extra
  // authenticated endpoint.
  const { data: publicModels } = useQuery({
    queryKey: ["public", "models"],
    queryFn: publicApi.models,
    staleTime: 60_000,
  });
  const [payOrder, setPayOrder] = useState<SiteOrder | null>(null);
  const [couponCode, setCouponCode] = useState(() => searchParams.get("coupon") ?? "");
  const [couponDiscountPct, setCouponDiscountPct] = useState(0);
  const [couponApplied, setCouponApplied] = useState(false);
  const [couponInviteConflict, setCouponInviteConflict] = useState<InviteCouponConflict | null>(null);
  // 代理优惠码停用后，链接里带的 ?coupon= 也一并忽略，否则购买按钮会卡在「请先应用优惠券」。
  const couponsEnabled = site.agent_coupons_enabled !== false;
  const activeCouponCode = couponsEnabled ? couponCode : "";

  const checkCoupon = useMutation({
    mutationFn: () => meApi.couponQuote(couponCode.trim()),
    onSuccess: (quote) => {
      setCouponCode(quote.coupon_code);
      setCouponDiscountPct(Number(quote.discount_pct) || 0);
      setCouponApplied(true);
      setCouponInviteConflict(quote.invite_conflict ?? null);
    },
  });

  const clearCoupon = () => {
    setCouponCode("");
    setCouponDiscountPct(0);
    setCouponApplied(false);
    setCouponInviteConflict(null);
    checkCoupon.reset();
  };

  const coupon: CouponState = {
    code: activeCouponCode,
    discountPct: couponDiscountPct,
    applied: couponApplied,
    checking: checkCoupon.isPending,
    error: checkCoupon.error,
    inviteConflict: couponInviteConflict,
    onChange: (value) => {
      setCouponCode(value);
      setCouponDiscountPct(0);
      setCouponApplied(false);
      setCouponInviteConflict(null);
      checkCoupon.reset();
    },
    onApply: () => checkCoupon.mutate(),
  };

  const createOrder = useMutation({
    mutationFn: (id: string) => meApi.createMyOrder({
      kind: "plan",
      plan_id: id,
      coupon_code: activeCouponCode.trim() || undefined,
    }),
    onSuccess: (order) => {
      setPayOrder(order);
      clearCoupon();
      qc.invalidateQueries({ queryKey: ["me", "orders"] });
    },
  });

  const groupedPlans = useMemo(() => {
    const ps = plans ?? [];
    return [...ps].sort((a, b) => Number(a.price) - Number(b.price)).reduce(
      (acc, p) => {
        if (p.type === "pay_as_you_go") acc.balance.push(p);
        else acc.coding.push(p);
        return acc;
      },
      { balance: [] as Plan[], coding: [] as Plan[] },
    );
  }, [plans]);

  const modelsByGroup = useMemo(() => {
    const map = new Map<string, string[]>();
    for (const m of publicModels ?? []) {
      for (const g of m.groups ?? []) {
        const list = map.get(g.id) ?? [];
        list.push(m.name);
        map.set(g.id, list);
      }
    }
    return map;
  }, [publicModels]);

  const allModels = useMemo(
    () => (publicModels ?? []).map((m) => m.name),
    [publicModels],
  );

  const currentPlanId = me?.active_plan?.plan_id;
  const couponNeedsApply = !!activeCouponCode.trim() && !couponApplied;

  if (isLoading) return <RouteSkeleton />;

  return (
    <Box>
      <PageHeader
        title={t("billing.plans.title")}
        description={t("billing.plans.description")}
      />

      <div className="jc-plans-page">
        <TopupPanel
          coupon={coupon}
          couponsEnabled={couponsEnabled}
          onCreated={(order) => {
            setPayOrder(order);
            clearCoupon();
          }}
        />

        <PlanGroup
          title={t("billing.plans.balanceTitle")}
          description={t("billing.plans.balanceDesc")}
          plans={groupedPlans.balance}
          currentPlanId={currentPlanId}
          createOrder={createOrder}
          emptyText={t("billing.plans.balanceEmpty")}
          modelsByGroup={modelsByGroup}
          allModels={allModels}
          discountPct={couponDiscountPct}
          couponNeedsApply={couponNeedsApply}
        />
        <PlanGroup
          title="Coding Plan"
          description={t("billing.plans.codingDesc")}
          plans={groupedPlans.coding}
          currentPlanId={currentPlanId}
          createOrder={createOrder}
          emptyText={t("billing.plans.codingEmpty")}
          modelsByGroup={modelsByGroup}
          allModels={allModels}
          discountPct={couponDiscountPct}
          couponNeedsApply={couponNeedsApply}
        />
      </div>

      {createOrder.isError && <Text color="red" mt="3" as="div">{errMsg(createOrder.error)}</Text>}
      <PaymentDialog
        order={payOrder}
        open={!!payOrder}
        onClose={() => setPayOrder(null)}
        onPaid={() => {
          qc.invalidateQueries({ queryKey: ["me"] });
          qc.invalidateQueries({ queryKey: ["me", "orders"] });
          qc.invalidateQueries({ queryKey: ["me", "usage"] });
          qc.invalidateQueries({ queryKey: ["me", "billing"] });
        }}
      />
    </Box>
  );
}
