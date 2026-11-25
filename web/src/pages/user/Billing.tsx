import "../../personal-responsive.css";
import { Badge, Box, Button, Card, Dialog, Flex, SegmentedControl, Select, Text, TextArea } from "@radix-ui/themes";
import type { UseQueryResult } from "@tanstack/react-query";
import { useMemo, useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { Plus, RotateCcw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { meApi, type BillingEntry, type BillingSummary, type InviteCouponConflict, type OrgBillingEntry, type SiteOrder } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { InviteCouponConfirm, hasInviteConflict } from "../../components/InviteCouponConfirm";
import PaymentDialog from "../../components/PaymentDialog";
import { useActiveOrg, type ActiveOrg } from "../../hooks/useOrgContext";
import { DateRangeInput, last30DaysRange, rangeToIso } from "../../components/UsageLogFilters";
import { IncomeExpenseChart } from "../../components/IncomeExpenseChart";
import { formatAmount, sumAmounts } from "../../lib/currency";
import { OrderRefundableHint } from "../../components/OrderRefundableHint";
import { SelfRefundDialog } from "../../components/SelfRefund";
import { useSite } from "../../store/site";
import { formatDateTime } from "../../lib/formatTime";
import { PageHeader } from "../../components/PageHeader";
import i18n from "../../i18n";

// Billing follows the account context. Personal: every BalanceTx row for
// the calling user (topups, per-request deducts, refunds, adjustments) +
// site orders. Organization (owner/admin only — the tab is hidden for
// members): the org's balance ledger, with the acting member per row.
// Org topups live in the 组织管理 tab, not here.
export default function Billing() {
  const activeOrg = useActiveOrg();
  if (activeOrg) return <OrgBilling activeOrg={activeOrg} />;
  return <PersonalBilling />;
}

function OrgBilling({ activeOrg }: { activeOrg: ActiveOrg }) {
  const [kind, setKind] = useState<string>("");
  const defaultRange = useMemo(() => last30DaysRange(), []);
  const [start, setStart] = useState(defaultRange.start);
  const [end, setEnd] = useState(defaultRange.end);
  const { t } = useTranslation();
  const range = rangeToIso(start, end);
  const orgName = activeOrg.org?.name ?? t("billing.page.orgFallback");

  const summary = useQuery({
    queryKey: ["me-scoped", activeOrg.id, "org-billing-summary", range.from, range.to],
    queryFn: () => meApi.orgBillingSummary(activeOrg.id, range),
  });

  const paged = usePagedQuery<OrgBillingEntry>({
    queryKey: ["me-scoped", activeOrg.id, "org-billing"],
    queryFn: (p) => meApi.orgBillingPaged(activeOrg.id, { ...p, kind: kind || undefined, ...range }),
    extras: [kind, range.from, range.to],
    pageSize: 50,
  });

  const cols: Column<OrgBillingEntry>[] = [
    { header: t("billing.col.time"), minWidth: "112px", cell: (r) => <TimeCell iso={r.created_at} /> },
    { header: t("billing.col.type"), minWidth: "72px", cell: (r) => kindLabel(r.kind) },
    {
      header: t("billing.col.actor"), minWidth: "160px",
      cell: (r) => <Text size="2" color="gray" truncate style={{ display: "block", maxWidth: 220 }}>{r.actor_email || r.actor_user_id || "—"}</Text>,
    },
    {
      header: t("billing.col.amount"), minWidth: "128px", align: "right",
      cell: (r) => <AmountCell amount={r.amount} hold={r.hold_amount} forceNegative={r.kind === "deduct"} />,
    },
    {
      header: t("billing.col.orgBalance"), minWidth: "140px", align: "right",
      cell: (r) => <span className="jc-cell-num jc-cell-muted">{formatAmount(r.balance_after, { decimals: 4 })}</span>,
    },
    { header: t("billing.col.note"), minWidth: "220px", cell: (r) => <Text size="2" color="gray">{r.note || "—"}</Text> },
  ];

  return (
    <Box className="jc-ledger jc-billing-ledger">
      <PageHeader
        title={t("billing.page.title")}
        description={t("billing.page.orgDescription", { name: orgName })}
      />
      <BillingSummaryStrip summary={summary} range={<DateRangeInput start={start} setStart={setStart} end={end} setEnd={setEnd} compact />} />
      <DailyChart summary={summary} />
      <section className="jc-billing-section">
        <div className="jc-ledger-section-head">
          <Box>
            <Text size="3" weight="medium" as="div">{t("billing.page.orgTxTitle")}</Text>
            <p>{t("billing.page.orgTxHint")}</p>
          </Box>
        </div>
        <DataTable
          {...paged}
          isLoading={paged.query.isLoading}
          isFetching={paged.query.isFetching}
          refetch={() => paged.query.refetch()}
          columns={cols}
          rowKey={(r) => r.id}
          searchPlaceholder={t("billing.page.searchNote")}
          filters={
            <Select.Root value={kind || "_all"} onValueChange={(v) => setKind(v === "_all" ? "" : v)}>
              <Select.Trigger placeholder={t("billing.filter.type")} />
              <Select.Content>
                <Select.Item value="_all">{t("billing.filter.typeAll")}</Select.Item>
                <Select.Item value="topup">{t("billing.kind.topup")}</Select.Item>
                <Select.Item value="deduct">{t("billing.kind.deduct")}</Select.Item>
                <Select.Item value="adjust">{t("billing.kind.adjust")}</Select.Item>
              </Select.Content>
            </Select.Root>
          }
        />
      </section>
    </Box>
  );
}

function PersonalBilling() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [kind, setKind] = useState<string>("");
  const [tab, setTab] = useState<"tx" | "orders">("tx");
  const [topupOpen, setTopupOpen] = useState(false);
  const [refundOpen, setRefundOpen] = useState(false);
  const selfRefund = useSite().self_refund_enabled === true;
  const [refundOrder, setRefundOrder] = useState<SiteOrder | null>(null);
  const [payOrder, setPayOrder] = useState<SiteOrder | null>(null);
  const defaultRange = useMemo(() => last30DaysRange(), []);
  const [start, setStart] = useState(defaultRange.start);
  const [end, setEnd] = useState(defaultRange.end);
  const range = rangeToIso(start, end);

  const summary = useQuery({
    queryKey: ["me", "billing-summary", range.from, range.to],
    queryFn: () => meApi.myBillingSummary(range),
  });

  const paged = usePagedQuery<BillingEntry>({
    queryKey: ["me", "billing"],
    queryFn: (p) => meApi.myBillingPaged({ ...p, kind: kind || undefined, ...range }),
    extras: [kind, range.from, range.to],
    pageSize: 50,
  });

  const cols: Column<BillingEntry>[] = [
    { header: t("billing.col.time"), minWidth: "112px", cell: (r) => <TimeCell iso={r.created_at} /> },
    { header: t("billing.col.type"), minWidth: "72px", cell: (r) => kindLabel(r.kind) },
    {
      header: t("billing.col.amount"), minWidth: "128px", align: "right",
      cell: (r) => <AmountCell amount={r.amount} hold={r.hold_amount} />,
    },
    {
      header: t("billing.col.balance"), minWidth: "128px", align: "right",
      cell: (r) => <span className="jc-cell-num jc-cell-muted">{formatAmount(r.balance_after, { decimals: 4 })}</span>,
    },
    { header: t("billing.col.note"), minWidth: "220px", cell: (r) => <Text size="2" color="gray">{r.note || "—"}</Text> },
  ];

  const filters = (
    <Select.Root value={kind || "_all"} onValueChange={(v) => setKind(v === "_all" ? "" : v)}>
      <Select.Trigger placeholder={t("billing.filter.type")} />
      <Select.Content>
        <Select.Item value="_all">{t("billing.filter.typeAll")}</Select.Item>
        <Select.Item value="topup">{t("billing.kind.topup")}</Select.Item>
        <Select.Item value="deduct">{t("billing.kind.deduct")}</Select.Item>
        <Select.Item value="refund">{t("billing.kind.refund")}</Select.Item>
        <Select.Item value="adjust">{t("billing.kind.adjust")}</Select.Item>
      </Select.Content>
    </Select.Root>
  );

  return (
    <Box className="jc-ledger jc-billing-ledger">
      <PageHeader
        title={t("billing.page.title")}
        description={t("billing.page.description")}
        actions={
          <Flex gap="2">
            {selfRefund && (
              <Button size="2" variant="soft" color="gray" onClick={() => setRefundOpen(true)}><RotateCcw size={14} />{t("billing.page.selfRefund")}</Button>
            )}
            <Button size="2" onClick={() => setTopupOpen(true)}><Plus size={14} />{t("billing.page.topup")}</Button>
          </Flex>
        }
      />
      {selfRefund && <SelfRefundDialog open={refundOpen} onClose={() => setRefundOpen(false)} />}
      <BillingSummaryStrip summary={summary} range={<DateRangeInput start={start} setStart={setStart} end={end} setEnd={setEnd} compact />} />
      <DailyChart summary={summary} />
      <div className="jc-ledger-section-head">
        <Box>
          <SegmentedControl.Root value={tab} onValueChange={(v) => setTab(v as "tx" | "orders")} size="2">
            <SegmentedControl.Item value="tx">{t("billing.page.tabTx")}</SegmentedControl.Item>
            <SegmentedControl.Item value="orders">{t("billing.page.tabOrders")}</SegmentedControl.Item>
          </SegmentedControl.Root>
        </Box>
        <p>
          {tab === "tx"
            ? t("billing.page.txHint")
            : selfRefund
              ? t("billing.page.ordersHintSelfRefund")
              : t("billing.page.ordersHintTicket")}
        </p>
      </div>
      {tab === "tx" ? (
        <section className="jc-billing-section">
          <DataTable
            {...paged}
            isLoading={paged.query.isLoading}
            isFetching={paged.query.isFetching}
            refetch={() => paged.query.refetch()}
            columns={cols}
            rowKey={(r) => r.id}
            searchPlaceholder={t("billing.page.searchNote")}
            filters={filters}
          />
        </section>
      ) : (
        <OrdersPanel
          onRefund={(o) => (selfRefund ? setRefundOpen(true) : setRefundOrder(o))}
          onPay={setPayOrder}
        />
      )}
      <TopupDialog
        open={topupOpen}
        onClose={() => setTopupOpen(false)}
        onSaved={(order) => {
          qc.invalidateQueries({ queryKey: ["me", "orders"] });
          setPayOrder(order);
        }}
      />
      <PaymentDialog
        order={payOrder}
        open={!!payOrder}
        onClose={() => setPayOrder(null)}
        onPaid={() => {
          qc.invalidateQueries({ queryKey: ["me", "orders"] });
          qc.invalidateQueries({ queryKey: ["me", "billing"] });
          qc.invalidateQueries({ queryKey: ["me"] });
        }}
      />
      <RefundTicketDialog
        order={refundOrder}
        onClose={() => setRefundOrder(null)}
        onSaved={(ticketId) => {
          qc.invalidateQueries({ queryKey: ["me", "orders"] });
          qc.invalidateQueries({ queryKey: ["me", "tickets"] });
          navigate(`/me/tickets?id=${ticketId}`);
        }}
      />
    </Box>
  );
}

const KIND_KEYS = ["topup", "refund", "deduct", "adjust", "agent_commission"] as const;

function kindLabel(k: string): string {
  const key = KIND_KEYS.find((x) => x === k);
  return key ? i18n.t(`billing.kind.${key}`) : k;
}

function TimeCell({ iso }: { iso: string }) {
  return <span className="jc-cell-mono" title={formatDateTime(iso, "full")}>{formatDateTime(iso)}</span>;
}

// Signed points, four fixed decimals, sign glued to the number. Credits are
// green; debits stay in the text color — the sign already says it.
function AmountCell({ amount, hold, forceNegative }: { amount: string; hold?: string; forceNegative?: boolean }) {
  const negative = amount.startsWith("-") || (!!forceNegative && Number(amount) !== 0);
  const abs = amount.replace(/^-/, "");
  const zero = Number(abs) === 0;
  return (
    <span className={`jc-cell-num${!negative && !zero ? " jc-amount-pos" : ""}`}>
      {zero ? "" : negative ? "-" : "+"}{formatAmount(abs, { decimals: 4 })}
      {hold && <span className="jc-cell-sub">{i18n.t("billing.amount.held", { amount: formatAmount(hold, { decimals: 4 }) })}</span>}
    </span>
  );
}

function OrdersPanel({
  onRefund,
  onPay,
}: {
  onRefund: (order: SiteOrder) => void;
  onPay: (order: SiteOrder) => void;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [status, setStatus] = useState<string>("");
  // /v1/me/orders answers with limit/offset/total, so the list pages
  // server-side instead of silently stopping at the first 50 orders.
  const paged = usePagedQuery<SiteOrder>({
    queryKey: ["me", "orders"],
    queryFn: async (p) => {
      const res = await meApi.myOrders({ limit: p.limit, offset: p.offset, status: status || undefined });
      return { items: res.orders, total: res.total, limit: res.limit, offset: res.offset };
    },
    extras: [status],
    pageSize: 20,
  });

  const cols: Column<SiteOrder>[] = [
    { header: t("billing.col.time"), minWidth: "112px", cell: (o) => <TimeCell iso={o.created_at} /> },
    { header: t("billing.col.orderNo"), minWidth: "170px", cell: (o) => <span className="jc-cell-mono">{o.order_no}</span> },
    { header: t("billing.col.type"), minWidth: "64px", cell: (o) => (o.kind === "plan" ? t("billing.orders.kindPlan") : t("billing.orders.kindBalance")) },
    {
      header: t("billing.col.amount"), minWidth: "120px", align: "right",
      cell: (o) => (
        <span className="jc-cell-num">
          {formatAmount(o.amount)}
          {o.credit_amount && Number(o.credit_amount) > Number(o.amount) && (
            <span className="jc-cell-sub jc-amount-pos">{t("billing.amount.credited", { amount: formatAmount(o.credit_amount) })}</span>
          )}
        </span>
      ),
    },
    {
      header: t("billing.col.status"), minWidth: "96px",
      cell: (o) => <Badge size="1" variant="outline" color={orderColor(o.status)}>{orderLabel(o.status)}</Badge>,
    },
    {
      header: t("billing.col.refund"), minWidth: "120px",
      cell: (o) => <OrderRefundStatus order={o} onOpenTicket={(id) => navigate(`/me/tickets?id=${id}`)} />,
    },
    {
      header: t("billing.col.actions"), width: "120px", align: "right",
      cell: (o) => (
        <>
          {o.status === "pending" && (
            <Button size="1" variant="soft" onClick={() => onPay(o)}>
              {t("billing.orders.payNow")}
            </Button>
          )}
          {(o.status === "paid" || o.status === "partially_refunded") && !openRefundTicket(o) && (
            <Button size="1" variant="soft" color="amber" onClick={() => onRefund(o)}>
              <RotateCcw size={13} />{t("billing.orders.requestRefund")}
            </Button>
          )}
        </>
      ),
    },
  ];

  return (
    <section className="jc-billing-section">
      <DataTable<SiteOrder>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={cols}
        rowKey={(o) => o.id}
        emptyText={t("billing.orders.empty")}
        filters={
          <Select.Root value={status || "_all"} onValueChange={(v) => setStatus(v === "_all" ? "" : v)}>
            <Select.Trigger placeholder={t("billing.filter.status")} />
            <Select.Content>
              <Select.Item value="_all">{t("billing.filter.statusAll")}</Select.Item>
              <Select.Item value="pending">{t("billing.orderStatus.pending")}</Select.Item>
              <Select.Item value="paid">{t("billing.orderStatus.paid")}</Select.Item>
              <Select.Item value="partially_refunded">{t("billing.orderStatus.partially_refunded")}</Select.Item>
              <Select.Item value="refunded">{t("billing.orderStatus.refunded")}</Select.Item>
              <Select.Item value="canceled">{t("billing.orderStatus.canceled")}</Select.Item>
            </Select.Content>
          </Select.Root>
        }
      />
    </section>
  );
}

function openRefundTicket(order: SiteOrder): string | undefined {
  const st = order.refund_ticket?.status;
  if (order.refund_ticket && st && st !== "closed" && st !== "resolved") {
    return order.refund_ticket.id;
  }
  return undefined;
}

function OrderRefundStatus({ order, onOpenTicket }: { order: SiteOrder; onOpenTicket: (id: string) => void }) {
  const { t } = useTranslation();
  const pending = (order.refunds ?? []).find((r) => r.status === "pending");
  const approved = (order.refunds ?? []).filter((r) => r.status === "approved");
  const ticketId = openRefundTicket(order);
  if (pending) {
    return <Badge size="1" color="amber" variant="soft">{t("billing.orders.refundPending")}</Badge>;
  }
  if (approved.length > 0) {
    return (
      <Text size="1">
        {t("billing.orders.refunded")} <PointAmount amount={sumAmounts(approved.map((r) => r.amount))} iconSize={11} />
      </Text>
    );
  }
  if (ticketId) {
    return (
      <Button size="1" variant="ghost" onClick={() => onOpenTicket(ticketId)}>{t("billing.orders.refundTicketOpen")}</Button>
    );
  }
  return <Text size="1" color="gray">—</Text>;
}

function TopupDialog({ open, onClose, onSaved }: { open: boolean; onClose: () => void; onSaved: (order: SiteOrder) => void }) {
  const { t } = useTranslation();
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const [couponCode, setCouponCode] = useState("");
  const [discount, setDiscount] = useState(0);
  const [couponApplied, setCouponApplied] = useState(false);
  const [inviteConflict, setInviteConflict] = useState<InviteCouponConflict | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const requested = Number(amount);
  const payable = Number.isFinite(requested) ? (Math.round(requested * (100 - discount)) / 100).toFixed(2) : amount;
  const checkCoupon = useMutation({
    mutationFn: () => meApi.couponQuote(couponCode.trim()),
    onSuccess: (quote) => {
      setCouponCode(quote.coupon_code);
      setDiscount(Number(quote.discount_pct) || 0);
      setCouponApplied(true);
      setInviteConflict(quote.invite_conflict ?? null);
    },
  });
  const create = useMutation({
    mutationFn: (withCoupon: boolean) => meApi.createMyOrder({
      kind: "balance",
      amount,
      currency: "CNY",
      note,
      coupon_code: withCoupon ? couponCode.trim() || undefined : undefined,
    }),
    onSuccess: (order) => {
      setAmount("");
      setNote("");
      setCouponCode("");
      setDiscount(0);
      setCouponApplied(false);
      setInviteConflict(null);
      checkCoupon.reset();
      onSaved(order);
      onClose();
    },
    onSettled: () => setConfirmOpen(false),
  });
  const submit = () => {
    if (couponApplied && hasInviteConflict(inviteConflict)) setConfirmOpen(true);
    else create.mutate(true);
  };
  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="460px">
        <Dialog.Title>{t("billing.topupDialog.title")}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">{t("billing.topupDialog.amount")}</Text>
            <ClearableTextField type="number" value={amount} onChange={(e) => setAmount(e.target.value)} />
            <Text size="1" color={discount > 0 ? "green" : "gray"} as="div" mt="1">
              {discount > 0 && requested > 0
                ? t("billing.topupDialog.discountHint", { discount, payable, amount })
                : t("billing.topupDialog.rateHint")}
            </Text>
          </Box>
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">{t("billing.topupDialog.coupon")}</Text>
            <Flex gap="2" align="center">
              <Box style={{ flex: 1 }}>
                <ClearableTextField
                  value={couponCode}
                  onChange={(event) => {
                    setCouponCode(event.target.value);
                    setDiscount(0);
                    setCouponApplied(false);
                    setInviteConflict(null);
                    checkCoupon.reset();
                  }}
                  placeholder={t("billing.topupDialog.couponPlaceholder")}
                  autoComplete="off"
                  spellCheck={false}
                />
              </Box>
              <Button variant="soft" disabled={!couponCode.trim()} loading={checkCoupon.isPending} onClick={() => checkCoupon.mutate()}>{t("billing.topupDialog.apply")}</Button>
            </Flex>
            {couponApplied && (
              <Text size="1" color="green" as="div" mt="1">
                {discount > 0 ? t("billing.topupDialog.couponValidDiscount", { discount }) : t("billing.topupDialog.couponValid")}
              </Text>
            )}
            {checkCoupon.isError && <Text size="1" color="red" as="div" mt="1">{t("billing.topupDialog.couponInvalid", { error: errMsg(checkCoupon.error) })}</Text>}
          </Box>
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">{t("billing.topupDialog.note")}</Text>
            <TextArea value={note} onChange={(e) => setNote(e.target.value)} rows={3} />
          </Box>
          {create.isError && <Text size="2" color="red">{errMsg(create.error)}</Text>}
          <Flex justify="end" gap="2">
            <Button variant="soft" color="gray" onClick={onClose}>{t("billing.topupDialog.cancel")}</Button>
            <Button disabled={!amount.trim() || (!!couponCode.trim() && !couponApplied)} loading={create.isPending} onClick={submit}>{t("billing.topupDialog.create")}</Button>
          </Flex>
          <InviteCouponConfirm
            conflict={inviteConflict}
            open={confirmOpen}
            loading={create.isPending}
            onUseCoupon={() => create.mutate(true)}
            onDropCoupon={() => create.mutate(false)}
            onCancel={() => setConfirmOpen(false)}
          />
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function RefundTicketDialog({
  order,
  onClose,
  onSaved,
}: {
  order: SiteOrder | null;
  onClose: () => void;
  onSaved: (ticketId: string) => void;
}) {
  const { t } = useTranslation();
  const [reason, setReason] = useState("");
  const refundable = useQuery({
    queryKey: ["me", "order-refundable", order?.id],
    queryFn: () => meApi.orderRefundable(order!.id),
    enabled: !!order,
  });
  const nothing = refundable.data?.balance_bearing === true && Number(refundable.data.cash) <= 0;
  const create = useMutation({
    mutationFn: () => meApi.createMyTicket({
      title: t("billing.refundDialog.ticketTitle", { orderNo: order!.order_no }),
      body: reason.trim(),
      category: "refund",
      order_id: order!.id,
    }),
    onSuccess: (t) => {
      setReason("");
      onClose();
      onSaved(t.id);
    },
    onError: (err) => {
      const data = (err as { response?: { data?: { ticket_id?: string } } }).response?.data;
      if (data?.ticket_id) {
        onClose();
        onSaved(data.ticket_id);
      }
    },
    // "已有退款工单" comes back as an error but is handled above by opening
    // that ticket, so the global failure toast would contradict the UI.
    meta: { suppressErrorToast: true },
  });
  return (
    <Dialog.Root open={!!order} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="480px">
        <Dialog.Title>{t("billing.refundDialog.title")}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Text size="2" color="gray">
            {t("billing.refundDialog.order", { orderNo: order?.order_no ?? "" })}{" "}
            {order ? <PointAmount amount={order.amount} iconSize={12} /> : null}
            {t("billing.refundDialog.orderEnd")}
            {t("billing.refundDialog.ticketNote")}
          </Text>
          <OrderRefundableHint data={refundable.data} loading={refundable.isLoading} />
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">{t("billing.refundDialog.reason")}</Text>
            <TextArea
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              rows={5}
              placeholder={t("billing.refundDialog.reasonPlaceholder")}
            />
          </Box>
          {create.isError && !(create.error as { response?: { data?: { ticket_id?: string } } }).response?.data?.ticket_id && (
            <Text size="2" color="red">{errMsg(create.error)}</Text>
          )}
          <Flex justify="end" gap="2">
            <Button variant="soft" color="gray" onClick={onClose}>{t("billing.topupDialog.cancel")}</Button>
            <Button disabled={!reason.trim() || nothing} loading={create.isPending} onClick={() => create.mutate()}>{t("billing.refundDialog.submit")}</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

// Period rollup in one strip: three readouts sharing a border, the range
// picker that drives them (and the chart and the ledger below) at the end.
// Loading and failure are explicit states — an empty number is never shown
// as a zero.
function BillingSummaryStrip({ summary, range }: { summary: UseQueryResult<BillingSummary>; range: ReactNode }) {
  const { t } = useTranslation();
  const income = summary.data?.income;
  const expense = summary.data?.expense;
  const hasBoth = income != null && expense != null;
  const negExpense = !expense || expense === "0" ? "0" : (expense.startsWith("-") ? expense : `-${expense}`);
  const net = hasBoth ? sumAmounts([income, negExpense]) : undefined;
  return (
    <section className="jc-ledger-summary">
      <div className="jc-ledger-metrics" aria-busy={summary.isPending}>
        {summary.isError ? (
          <div className="jc-ledger-error">
            <span>{t("billing.summary.loadFailed", { error: errMsg(summary.error) })}</span>
            <Button size="1" variant="soft" color="gray" onClick={() => summary.refetch()}>{t("billing.summary.retry")}</Button>
          </div>
        ) : (
          <>
            <LedgerMetric label={t("billing.summary.income")} value={income} hint={t("billing.summary.incomeHint")} loading={summary.isPending} />
            <LedgerMetric label={t("billing.summary.expense")} value={expense} hint={t("billing.summary.expenseHint")} loading={summary.isPending} />
            <LedgerMetric label={t("billing.summary.net")} value={net} hint={t("billing.summary.netHint")} loading={summary.isPending} tone={net?.startsWith("-") ? "neg" : undefined} />
          </>
        )}
      </div>
      <div className="jc-ledger-range">{range}</div>
    </section>
  );
}

function LedgerMetric({ label, value, hint, loading, tone }: { label: string; value?: string; hint: string; loading: boolean; tone?: "neg" }) {
  const { t } = useTranslation();
  return (
    <div className="jc-ledger-metric">
      <div className="jc-ledger-metric-label">{label}</div>
      <div className={`jc-ledger-metric-value${loading ? " is-loading" : ""}`} data-tone={tone}>
        {loading ? "…" : value == null ? "—" : formatAmount(value)}
        <small>{t("billing.summary.unit")}</small>
      </div>
      <div className="jc-ledger-metric-hint">{hint}</div>
    </div>
  );
}

function DailyChart({ summary }: { summary: UseQueryResult<BillingSummary> }) {
  const { t } = useTranslation();
  return (
    <Card className="pn-card-flat jc-ledger-chart">
      <div className="jc-ledger-chart-head">
        <h3>{t("billing.summary.dailyTitle")}</h3>
        <Text size="1" color="gray">{t("billing.summary.dailyUnit")}</Text>
      </div>
      {summary.isError ? (
        <div style={{ height: 120, display: "grid", placeItems: "center" }}>
          <Text size="2" color="gray">{t("billing.summary.chartUnavailable")}</Text>
        </div>
      ) : (
        <IncomeExpenseChart days={summary.data?.days ?? []} />
      )}
    </Card>
  );
}

function orderLabel(status: SiteOrder["status"]): string {
  return i18n.t(`billing.orderStatus.${status}`);
}

function orderColor(status: SiteOrder["status"]): "gray" | "green" | "amber" | "red" | "blue" {
  return ({
    pending: "amber",
    paid: "green",
    canceled: "gray",
    partially_refunded: "blue",
    refunded: "red",
  } as const)[status];
}
