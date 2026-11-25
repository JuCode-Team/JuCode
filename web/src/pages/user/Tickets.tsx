import "../../personal-responsive.css";
import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { Box, Button, Card, Dialog, Flex, Select, Skeleton, TextArea } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";
import { meApi, type SiteOrder, type TicketAttachment, type TicketCategory, type TicketDetail, type TicketMessage, type TicketPriority, type TicketRow, type TicketStatus } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AttachmentPicker, MessageAttachments } from "../../components/TicketAttachments";
import { formatDateTime, formatRelative } from "../../lib/formatTime";
import { PageHeader } from "../../components/PageHeader";
import { SelfRefundCallout } from "../../components/SelfRefund";

// Self-service ticket portal. Users open requests here; CS replies via
// /cs/tickets. List-first layout: the page shows the ticket list with one
// primary action; opening a row swaps to a full-width detail view.
// The selected ticket id lives in the URL (?id=…) so browser back and
// reloads land where the user expects.
export default function Tickets() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [status, setStatus] = useState<string>("");
  const [category, setCategory] = useState<string>("");
  const [q, setQ] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedId = searchParams.get("id");
  const openTicket = (id: string | null) => setSearchParams(id ? { id } : {});

  const list = useQuery({
    queryKey: ["me", "tickets", status, category, q],
    queryFn: () => meApi.myTickets({
      status: (status || undefined) as TicketStatus | undefined,
      category: (category || undefined) as TicketCategory | undefined,
      q: q || undefined,
    }),
  });
  const orders = useQuery({
    queryKey: ["me", "orders", "ticket-bind"],
    queryFn: () => meApi.myOrders({ limit: 100 }),
  });

  const detail = useQuery({
    queryKey: ["me", "ticket", selectedId],
    queryFn: () => meApi.getMyTicket(selectedId!),
    enabled: !!selectedId,
  });
  // Loading a ticket can change its unread state: refresh the list once it loads.
  const loadedTicketId = detail.data?.ticket.id;
  useEffect(() => {
    if (!loadedTicketId) return;
    qc.invalidateQueries({ queryKey: ["me", "tickets"] });
  }, [loadedTicketId, qc]);
  const tickets = [...(list.data?.tickets ?? [])].sort(compareTickets);
  const filtered = !!status || !!category || !!q;

  if (selectedId) {
    return (
      <Box className="jc-ticket-page">
        <Flex align="center" gap="3" mb="3">
          <Button variant="ghost" color="gray" onClick={() => openTicket(null)}>
            <ArrowLeft size={15} /> {t("billing.tickets.back")}
          </Button>
        </Flex>
        {detail.data ? (
          <TicketThread
            key={detail.data.ticket.id}
            detail={detail.data}
            onChanged={() => {
              qc.invalidateQueries({ queryKey: ["me", "tickets"] });
              qc.invalidateQueries({ queryKey: ["me", "ticket", selectedId] });
            }}
          />
        ) : (
          <Card className="pn-card-flat">
            <Skeleton width="40%" height="20px" style={{ borderRadius: 4, marginBottom: 12 }} />
            <Skeleton width="60%" height="12px" style={{ borderRadius: 4 }} />
          </Card>
        )}
      </Box>
    );
  }

  return (
    <Box>
      <PageHeader
        title={t("billing.tickets.title")}
        description={t("billing.tickets.description")}
        actions={<Button size="2" onClick={() => setCreateOpen(true)}>{t("billing.tickets.create")}</Button>}
      />
      <SelfRefundCallout />

      <div className="jc-object-list jc-stagger">
        <div className="jc-object-toolbar">
          <ClearableTextField
            size="2"
            placeholder={t("billing.tickets.searchPlaceholder")}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 260 }}
          />
          <Select.Root value={status || "_all"} onValueChange={(v) => setStatus(v === "_all" ? "" : v)}>
            <Select.Trigger placeholder={t("billing.tickets.statusPlaceholder")} />
            <Select.Content>
              <Select.Item value="_all">{t("billing.tickets.allStatus")}</Select.Item>
              {(Object.keys(STATUS_TONE) as TicketStatus[]).map((s) => <Select.Item key={s} value={s}>{t(`billing.tickets.status.${s}`)}</Select.Item>)}
            </Select.Content>
          </Select.Root>
          <Select.Root value={category || "_all"} onValueChange={(v) => setCategory(v === "_all" ? "" : v)}>
            <Select.Trigger placeholder={t("billing.tickets.categoryPlaceholder")} />
            <Select.Content>
              <Select.Item value="_all">{t("billing.tickets.allCategory")}</Select.Item>
              {CATEGORIES.map((c) => <Select.Item key={c} value={c}>{t(`billing.tickets.category.${c}`)}</Select.Item>)}
            </Select.Content>
          </Select.Root>
          <span className="jc-object-toolbar-spacer" />
          {list.data && <span className="jc-object-toolbar-count">{t("billing.tickets.count", { count: list.data.total })}</span>}
        </div>
        {tickets.map((t) => (
          <TicketRowItem key={t.id} t={t} onClick={() => openTicket(t.id)} />
        ))}
        {list.isLoading && Array.from({ length: 3 }, (_, i) => (
          <div key={i} className="jc-object-skeleton-row">
            <Skeleton width="44%" height="16px" style={{ borderRadius: 4 }} />
            <Skeleton width="30%" height="12px" style={{ borderRadius: 4 }} />
          </div>
        ))}
        {!list.isLoading && tickets.length === 0 && (
          <div className="jc-object-empty">
            <h3>{filtered ? t("billing.tickets.emptyFiltered") : t("billing.tickets.empty")}</h3>
            <p>{filtered ? t("billing.tickets.emptyFilteredHint") : t("billing.tickets.emptyHint")}</p>
            <div className="jc-object-empty-actions">
              <Button size="2" variant="soft" onClick={() => setCreateOpen(true)}>{t("billing.tickets.create")}</Button>
            </div>
          </div>
        )}
      </div>

      <CreateTicketDialog
        open={createOpen}
        orders={orders.data?.orders ?? []}
        onClose={() => setCreateOpen(false)}
        onCreated={(id) => {
          qc.invalidateQueries({ queryKey: ["me", "tickets"] });
          openTicket(id);
        }}
      />
    </Box>
  );
}

// 状态色只表状态：蓝 = 客服待处理，紫 = 处理中，琥珀 = 等你回复，
// 绿 = 已解决，灰 = 已关闭。分类与优先级是元信息，不上色。
const STATUS_TONE: Record<TicketStatus, "info" | "primary" | "warn" | "success" | "neutral"> = {
  open: "info",
  in_progress: "primary",
  waiting_user: "warn",
  resolved: "success",
  closed: "neutral",
};
const CATEGORIES: TicketCategory[] = ["general", "refund", "billing", "technical", "account"];
const PRIORITIES: TicketPriority[] = ["low", "normal", "high", "urgent"];

function StatusChip({ status }: { status: TicketStatus }) {
  const { t } = useTranslation();
  return <span className="jc-fact-chip" data-tone={STATUS_TONE[status]}>{t(`billing.tickets.status.${status}`)}</span>;
}

function TicketRowItem({ t: row, onClick }: { t: TicketRow; onClick: () => void }) {
  const { t } = useTranslation();
  const meta: ReactNode[] = [];
  if (row.ticket_no_label) meta.push(<span key="no" className="jc-mono">{row.ticket_no_label}</span>);
  if (row.category && row.category !== "general") meta.push(<span key="cat">{t(`billing.tickets.category.${row.category}`)}</span>);
  if (row.priority !== "normal") {
    meta.push(<span key="pri">{t("billing.tickets.meta.priority", { priority: t(`billing.tickets.priority.${row.priority}`) })}</span>);
  }
  if (row.order_id) meta.push(<span key="order">{t("billing.tickets.meta.order", { id: row.order_id.slice(0, 8) })}</span>);
  meta.push(
    <span key="created" title={formatDateTime(row.created_at, "full")}>
      {t("billing.tickets.meta.created", { date: formatDateTime(row.created_at, "date") })}
    </span>,
  );
  return (
    <div className="jc-ticket-row" onClick={onClick} role="button" tabIndex={0}
      onKeyDown={(e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onClick(); } }}>
      <span className={`jc-ticket-dot ${row.unread ? "is-unread" : ""}`} title={row.unread_kind === "reply" ? t("billing.tickets.meta.newReply") : row.unread ? t("billing.tickets.meta.unread") : undefined} />
      <div className="jc-object-main">
        <div className="jc-object-title">
          <strong>{row.title}</strong>
          <StatusChip status={row.status} />
          {row.unread_kind === "reply" && <span className="jc-fact-chip" data-tone="primary">{t("billing.tickets.meta.newReplyChip")}</span>}
        </div>
        <div className="jc-object-sub">{meta}</div>
      </div>
      <div className="jc-ticket-row-side">
        <time dateTime={row.last_reply_at} title={t("billing.tickets.meta.lastReply", { date: formatDateTime(row.last_reply_at, "full") })}>{formatRelative(row.last_reply_at)}</time>
        <ChevronRight size={16} />
      </div>
    </div>
  );
}

function compareTickets(a: TicketRow, b: TicketRow) {
  const unreadOrder = Number(!!b.unread) - Number(!!a.unread);
  if (unreadOrder !== 0) return unreadOrder;
  const closedOrder = Number(a.status === "closed") - Number(b.status === "closed");
  if (closedOrder !== 0) return closedOrder;
  return Date.parse(b.last_reply_at) - Date.parse(a.last_reply_at);
}

function TicketThread({ detail, onChanged }: { detail: TicketDetail; onChanged: () => void }) {
  const { t: tr } = useTranslation();
  const [body, setBody] = useState("");
  const [attachments, setAttachments] = useState<TicketAttachment[]>([]);
  const endRef = useRef<HTMLDivElement | null>(null);
  const reply = useMutation({
    mutationFn: () => meApi.replyMyTicket(detail.ticket.id, body, attachments),
    onSuccess: () => {
      setBody("");
      setAttachments([]);
      onChanged();
    },
  });
  const close = useMutation({
    mutationFn: () => meApi.closeMyTicket(detail.ticket.id),
    onSuccess: () => onChanged(),
  });
  const reopen = useMutation({
    mutationFn: () => meApi.reopenMyTicket(detail.ticket.id),
    onSuccess: () => onChanged(),
  });
  const canReply = (body.trim().length > 0 || attachments.length > 0) && !reply.isPending;
  const failure = reply.error ?? close.error ?? reopen.error;
  const sendReply = () => {
    if (!canReply) return;
    reply.mutate();
  };
  const handleReplyKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    sendReply();
  };

  // New messages append at the bottom; keep the latest one in view.
  const count = detail.messages.length;
  useEffect(() => {
    if (count <= 3) return;
    endRef.current?.scrollIntoView({ block: "nearest" });
  }, [detail.ticket.id, count]);

  const t = detail.ticket;
  const meta: ReactNode[] = [];
  if (t.ticket_no_label) meta.push(<span key="no" className="jc-mono">{t.ticket_no_label}</span>);
  if (t.category) meta.push(<span key="cat">{tr(`billing.tickets.category.${t.category}`)}</span>);
  if (t.priority !== "normal") {
    meta.push(<span key="pri">{tr("billing.tickets.meta.priority", { priority: tr(`billing.tickets.priority.${t.priority}`) })}</span>);
  }
  if (t.order_id) meta.push(<span key="order">{tr("billing.tickets.meta.order", { id: t.order_id.slice(0, 8) })}</span>);
  meta.push(
    <span key="created" title={formatDateTime(t.created_at, "full")}>
      {tr("billing.tickets.meta.created", { date: formatDateTime(t.created_at) })}
    </span>,
  );

  return (
    <Card className="pn-card-flat jc-ticket-thread">
      <div className="jc-ticket-thread-head">
        <div className="jc-object-main">
          <div className="jc-object-title" style={{ gap: 10 }}>
            <h2>{t.title}</h2>
            <StatusChip status={t.status} />
          </div>
          <div className="jc-object-sub">{meta}</div>
        </div>
        {t.status === "closed" || t.status === "resolved" ? (
          <Button variant="soft" color="gray" size="2" onClick={() => reopen.mutate()} loading={reopen.isPending}>{tr("billing.tickets.reopen")}</Button>
        ) : (
          <Button variant="soft" color="gray" size="2" onClick={() => close.mutate()} loading={close.isPending}>{tr("billing.tickets.close")}</Button>
        )}
      </div>
      {failure != null && <div className="jc-form-error">{errMsg(failure)}</div>}
      <div className="jc-ticket-messages">
        {detail.messages.map((m) => <MessageBubble key={m.id} m={m} />)}
        <div ref={endRef} />
      </div>
      {t.status !== "closed" && (
        <div className="jc-ticket-composer">
          <TextArea
            placeholder={tr("billing.tickets.replyPlaceholder")}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            onKeyDown={handleReplyKeyDown}
            rows={3}
          />
          <div className="jc-ticket-composer-bar">
            <AttachmentPicker attachments={attachments} onChange={setAttachments} />
            <Button disabled={!canReply} onClick={sendReply}>{tr("billing.tickets.send")}</Button>
          </div>
        </div>
      )}
    </Card>
  );
}

function MessageBubble({ m }: { m: TicketMessage }) {
  const mine = m.sender_role === "user";
  const system = m.sender_role === "system";
  const { t } = useTranslation();
  const who = t(`billing.tickets.who.${mine ? "me" : m.sender_role === "cs" ? "cs" : m.sender_role === "admin" ? "admin" : "system"}`);
  return (
    <div className={`jc-msg ${mine ? "is-mine" : ""} ${system ? "is-system" : ""}`}>
      <div className="jc-msg-bubble">
        <div className="jc-msg-meta">
          <span>{who}</span>
          <time dateTime={m.created_at} title={formatDateTime(m.created_at, "full")}>{formatRelative(m.created_at)}</time>
        </div>
        <div className="jc-msg-body">{m.body}</div>
        <MessageAttachments attachments={m.attachments ?? []} />
      </div>
    </div>
  );
}

function CreateTicketDialog({
  open,
  orders,
  onClose,
  onCreated,
}: {
  open: boolean;
  orders: SiteOrder[];
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const { t } = useTranslation();
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [category, setCategory] = useState<TicketCategory>("general");
  const [priority, setPriority] = useState<TicketPriority>("normal");
  const [orderID, setOrderID] = useState("");
  const [attachments, setAttachments] = useState<TicketAttachment[]>([]);
  const create = useMutation({
    mutationFn: () => meApi.createMyTicket({
      title: category === "refund" && !title.trim() ? t("billing.tickets.dialog.defaultRefundTitle") : title,
      body,
      category,
      priority,
      order_id: orderID || undefined,
      attachments,
    }),
    onSuccess: (t) => {
      onCreated(t.id);
      setTitle("");
      setBody("");
      setCategory("general");
      setPriority("normal");
      setOrderID("");
      setAttachments([]);
      onClose();
    },
  });
  const refund = category === "refund";
  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="520px">
        <Dialog.Title>{t("billing.tickets.dialog.title")}</Dialog.Title>
        <Dialog.Description size="2" color="gray">{t("billing.tickets.dialog.description")}</Dialog.Description>
        <div className="jc-form">
          <SelfRefundCallout />
          <section className="jc-form-section">
            <div className="jc-field-row" style={{ gridTemplateColumns: "1fr 1fr" }}>
              <label className="jc-field">
                <span className="jc-field-label">{t("billing.tickets.dialog.category")}</span>
                <Select.Root value={category} onValueChange={(v) => setCategory(v as TicketCategory)}>
                  <Select.Trigger />
                  <Select.Content position="popper">
                    {CATEGORIES.map((c) => <Select.Item key={c} value={c}>{t(`billing.tickets.category.${c}`)}</Select.Item>)}
                  </Select.Content>
                </Select.Root>
              </label>
              <label className="jc-field">
                <span className="jc-field-label">{t("billing.tickets.dialog.priority")}</span>
                <Select.Root value={priority} onValueChange={(v) => setPriority(v as TicketPriority)}>
                  <Select.Trigger />
                  <Select.Content position="popper">
                    {PRIORITIES.map((p) => <Select.Item key={p} value={p}>{t(`billing.tickets.priority.${p}`)}</Select.Item>)}
                  </Select.Content>
                </Select.Root>
              </label>
            </div>
            <label className="jc-field">
              <span className="jc-field-label">{t("billing.tickets.dialog.subject")}</span>
              <ClearableTextField value={title} onChange={(e) => setTitle(e.target.value)} placeholder={refund ? t("billing.tickets.dialog.subjectRefundPlaceholder") : t("billing.tickets.dialog.subjectPlaceholder")} />
            </label>
            <label className="jc-field">
              <span className="jc-field-label">{refund ? t("billing.tickets.dialog.refundOrder") : t("billing.tickets.dialog.relatedOrder")}</span>
              {orders.length > 0 ? (
                <Select.Root value={orderID || "_none"} onValueChange={(v) => setOrderID(v === "_none" ? "" : v)}>
                  <Select.Trigger placeholder={t("billing.tickets.dialog.chooseOrder")} />
                  <Select.Content position="popper">
                    <Select.Item value="_none">{t("billing.tickets.dialog.noOrder")}</Select.Item>
                    {orders.map((o) => (
                      <Select.Item key={o.id} value={o.id}>
                        {o.order_no} · {o.kind === "plan" ? t("billing.tickets.dialog.kindPlan") : t("billing.tickets.dialog.kindBalance")} · {o.status}
                      </Select.Item>
                    ))}
                  </Select.Content>
                </Select.Root>
              ) : (
                <ClearableTextField value={orderID} onChange={(e) => setOrderID(e.target.value)} placeholder={t("billing.tickets.dialog.orderIdPlaceholder")} />
              )}
              <span className="jc-field-hint">{refund ? t("billing.tickets.dialog.refundOrderHint") : t("billing.tickets.dialog.orderHint")}</span>
            </label>
          </section>
          <section className="jc-form-section">
            <label className="jc-field">
              <span className="jc-field-label">{t("billing.tickets.dialog.details")}</span>
              <TextArea
                rows={6}
                placeholder={refund ? t("billing.tickets.dialog.refundDetailsPlaceholder") : t("billing.tickets.dialog.detailsPlaceholder")}
                value={body}
                onChange={(e) => setBody(e.target.value)}
              />
            </label>
            <AttachmentPicker attachments={attachments} onChange={setAttachments} />
          </section>
          {create.isError && <div className="jc-form-error">{t("billing.tickets.dialog.submitFailed", { error: errMsg(create.error) })}</div>}
          <div className="jc-form-footer">
            <Button variant="soft" color="gray" onClick={onClose}>{t("billing.tickets.dialog.cancel")}</Button>
            <Button disabled={(!refund && !title.trim()) || !body.trim() || (refund && !orderID) || create.isPending} onClick={() => create.mutate()}>
              {t("billing.tickets.dialog.submit")}
            </Button>
          </div>
        </div>
      </Dialog.Content>
    </Dialog.Root>
  );
}
