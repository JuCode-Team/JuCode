import "../styles/self-refund.css";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button, Dialog, Flex, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { RotateCcw } from "lucide-react";
import { meApi, type SelfRefundPlan, type SelfRefundPlanItem, type SelfRefundResult } from "../api/endpoints";
import { errMsg } from "../api/client";
import { formatAmount } from "../lib/currency";
import { useSite } from "../store/site";

// 自助退款：站点开启 self_refund_enabled 后，用户把未消费的余额和生效中的
// Coding Plan 按原支付渠道退回，不用提交工单。支付渠道不支持自动退款的部分
// 由客服通过工单处理。

export function SelfRefundDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="560px">
        <Dialog.Title>{t("pub.refund.selfTitle")}</Dialog.Title>
        <Text size="2" color="gray" as="p" mb="3">
          {t("pub.refund.dialogLead")}
        </Text>
        {open && <SelfRefundBody />}
      </Dialog.Content>
    </Dialog.Root>
  );
}

// Mounted only while the dialog is open, so reopening it starts from a
// fresh plan instead of the previous run's result.
function SelfRefundBody() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const plan = useQuery({ queryKey: ["me", "self-refund"], queryFn: meApi.selfRefundPlan });
  const run = useMutation({
    mutationFn: meApi.runSelfRefund,
    onSettled: () => { void qc.invalidateQueries({ queryKey: ["me"] }); },
  });
  return (
    <>
      {plan.isLoading && <Text size="2" color="gray">{t("pub.refund.calculating")}</Text>}
      {plan.isError && <Text size="2" color="red">{errMsg(plan.error)}</Text>}
      {plan.data && (
        <SelfRefundPanel
          plan={plan.data}
          running={run.isPending}
          result={run.data}
          error={run.error ?? undefined}
          onRun={() => run.mutate()}
        />
      )}
    </>
  );
}

// 先展示会怎么退，确认后执行。
export function SelfRefundPanel({
  plan,
  running,
  result,
  error,
  onRun,
}: {
  plan: SelfRefundPlan;
  running: boolean;
  result?: SelfRefundResult;
  error?: unknown;
  onRun: () => void;
}) {
  const { t } = useTranslation();
  const [confirming, setConfirming] = useState(false);
  const orders = plan.orders ?? [];
  const plans = plan.plans ?? [];
  const autoPlans = plans.filter((p) => p.auto);
  const manualPlans = plans.filter((p) => !p.auto);
  const autoTotal = Number(plan.cash) + sum(autoPlans);
  const manualTotal = Number(plan.unsupported) + sum(manualPlans);
  const nothing = autoTotal <= 0;

  if (result) {
    const refunded = result.refunded ?? [];
    const refundedPlans = result.refunded_plans ?? [];
    const cash = refunded.reduce((acc, o) => acc + Number(o.cash), 0) + sum(refundedPlans);
    return (
      <div className="jc-refund-panel">
        {cash > 0 && <p>{t("pub.refund.done.refunded", { amount: formatAmount(String(cash)), count: refunded.length + refundedPlans.length })}</p>}
        {refundedPlans.map((p) => <p key={p.order_id}>{t("pub.refund.done.plan", { name: p.plan_name, count: p.remaining_days })}</p>)}
        {Number(result.unsupported) > 0 && (
          <p>{t("pub.refund.done.unsupported", { amount: formatAmount(result.unsupported) })}</p>
        )}
        {(result.failed ?? []).map((f) => <p key={f} className="jc-refund-panel-note">{t("pub.refund.done.failed", { reason: f })}</p>)}
      </div>
    );
  }

  return (
    <div className="jc-refund-panel">
      <dl className="jc-refund-summary">
        <div><dt>{t("pub.refund.auto")}</dt><dd>{formatAmount(String(autoTotal))}</dd></div>
        <div><dt>{t("pub.refund.manual")}</dt><dd>{formatAmount(String(manualTotal))}</dd></div>
      </dl>
      {Number(plan.refundable) > 0 && (
        <p>
          {orders.length > 0
            ? t("pub.refund.refundableOrders", { amount: formatAmount(plan.refundable), count: orders.length })
            : t("pub.refund.refundable", { amount: formatAmount(plan.refundable) })}
        </p>
      )}
      {plans.map((p) => <PlanLine key={p.order_id} p={p} />)}
      {manualTotal > 0 && <p>{t("pub.refund.manualNote")}</p>}
      {error != null && <p className="jc-refund-panel-error">{errMsg(error)}</p>}
      <Flex gap="3" align="center" mt="2">
        <Button disabled={!plan.enabled || nothing || running} loading={running} onClick={() => setConfirming(true)}>
          {t("pub.refund.request")}
        </Button>
        {!plan.enabled && <span className="jc-refund-panel-note">{t("pub.refund.notOpen")}</span>}
        {plan.enabled && nothing && <span className="jc-refund-panel-note">{t("pub.refund.nothing")}</span>}
      </Flex>

      <Dialog.Root open={confirming} onOpenChange={setConfirming}>
        <Dialog.Content style={{ maxWidth: 420 }}>
          <Dialog.Title>{t("pub.refund.confirmTitle")}</Dialog.Title>
          <Dialog.Description size="2">
            {[
              t("pub.refund.confirmAmount", { amount: formatAmount(String(autoTotal)) }),
              Number(plan.cash) > 0 ? t("pub.refund.confirmBalance") : "",
              autoPlans.length > 0 ? t("pub.refund.confirmPlans") : "",
              t("pub.refund.confirmIrreversible"),
            ].filter(Boolean).join(t("pub.refund.sentenceSep"))}
          </Dialog.Description>
          <Flex gap="2" justify="end" mt="4">
            <Button variant="soft" color="gray" onClick={() => setConfirming(false)}>{t("pub.refund.cancel")}</Button>
            <Button onClick={() => { setConfirming(false); onRun(); }}>{t("pub.refund.confirmTitle")}</Button>
          </Flex>
        </Dialog.Content>
      </Dialog.Root>
    </div>
  );
}

function sum(plans: SelfRefundPlanItem[]): number {
  return plans.reduce((acc, p) => acc + Number(p.cash), 0);
}

function PlanLine({ p }: { p: SelfRefundPlanItem }) {
  const { t } = useTranslation();
  const values = { name: p.plan_name, remaining: p.remaining_days, count: p.total_days, amount: formatAmount(p.cash) };
  return <p>{p.auto ? t("pub.refund.planAuto", values) : t("pub.refund.planManual", values)}</p>;
}

// 工单、新建工单弹窗顶部的自助退款入口：开启自助退款后才显示。
export function SelfRefundCallout() {
  const { t } = useTranslation();
  const site = useSite();
  const [open, setOpen] = useState(false);
  if (!site.self_refund_enabled) return null;
  return (
    <div className="jc-self-refund-callout">
      <div>
        <div className="jc-self-refund-callout-title">{t("pub.refund.calloutTitle")}</div>
        <p>{t("pub.refund.calloutBody")}</p>
      </div>
      <Button size="3" onClick={() => setOpen(true)}><RotateCcw size={15} />{t("pub.refund.selfTitle")}</Button>
      <SelfRefundDialog open={open} onClose={() => setOpen(false)} />
    </div>
  );
}
