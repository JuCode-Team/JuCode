import "../../personal-responsive.css";
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { Badge, Box, Button, Card, Checkbox, Dialog, Flex, Select, Text, TextArea } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Inbox } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import { OrderRefundableHint } from "../../components/OrderRefundableHint";
import { csApi, type TicketAssignee, type TicketAttachment, type TicketCategory, type TicketDetail, type TicketMessage, type TicketPriority, type TicketRow, type TicketStatus } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AttachmentPicker, MessageAttachments } from "../../components/TicketAttachments";
import { useAuthStore } from "../../store/auth";
import { PageHeader } from "../../components/PageHeader";

// CS-side ticket queue. Shares the conversation UI with the user
// portal but exposes assignment, status overrides, and internal
// notes that the user view hides.
export default function CSTickets() {
  const qc = useQueryClient();
  const [status, setStatus] = useState<string>("");
  const [category, setCategory] = useState<string>("");
  const [assignee, setAssignee] = useState<string>("");
  const [q, setQ] = useState("");
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedId = searchParams.get("id");
  const userFilter = searchParams.get("user") || "";
  const isAdminUser = useAuthStore((state) => state.user?.role === "admin");
  const currentUserId = useAuthStore((state) => state.user?.id ?? "");
  const openTicket = (id: string | null) => {
    const next = new URLSearchParams(searchParams);
    if (id) next.set("id", id);
    else next.delete("id");
    setSearchParams(next);
  };

  const list = useQuery({
    queryKey: ["cs", "tickets", status, category, assignee, q, userFilter],
    queryFn: () => csApi.tickets({
      status: (status || undefined) as TicketStatus | undefined,
      category: (category || undefined) as TicketCategory | undefined,
      assignee: (assignee || undefined) as "me" | "unassigned" | undefined,
      q: q || undefined,
      user_id: userFilter || undefined,
    }),
  });
  const counts = useQuery({ queryKey: ["cs", "ticket-counts"], queryFn: csApi.ticketCounts });
  const assignees = useQuery({ queryKey: ["cs", "ticket-assignees"], queryFn: csApi.ticketAssignees, enabled: isAdminUser });
  const detail = useQuery({
    queryKey: ["cs", "ticket", selectedId],
    queryFn: () => csApi.getTicket(selectedId!),
    enabled: !!selectedId,
  });
  // Loading a ticket can change its unread state: refresh the list once it loads.
  const loadedTicketId = detail.data?.ticket.id;
  useEffect(() => {
    if (!loadedTicketId) return;
    qc.invalidateQueries({ queryKey: ["cs", "tickets"] });
    qc.invalidateQueries({ queryKey: ["cs", "ticket-counts"] });
  }, [loadedTicketId, qc]);
  const tickets = [...(list.data?.tickets ?? [])].sort(compareTickets);

  return (
    <Box className="jc-ticket-page" style={{ height: "calc(100dvh - 156px)", minHeight: 0, display: "flex", flexDirection: "column" }}>
      <PageHeader
        title="工单队列"
        description="处理用户提交的支持请求"
        actions={
          <Flex gap="2" wrap="wrap">
            {typeof counts.data?.unread === "number" && counts.data.unread > 0 && (
              <Badge size="2" variant="solid" color="red">未读 {counts.data.unread}</Badge>
            )}
            {counts.data?.counts && Object.entries(counts.data.counts).map(([k, n]) => (
              <Badge key={k} size="2" variant="soft" color={statusColor(k as TicketStatus)}>
                {statusLabel(k as TicketStatus)} {n}
              </Badge>
            ))}
          </Flex>
        }
      />

      <Flex className="jc-ticket-layout" gap="3" align="stretch" wrap="wrap" style={{ flex: 1, minHeight: 0 }}>
        <Box className="jc-ticket-list-pane" style={{ flex: "0 0 420px", minWidth: 0, display: "flex", minHeight: 0 }}>
          <Card className="jc-ticket-list-card" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
            <Flex direction="column" gap="2" style={{ flex: 1, minHeight: 0 }}>
              <Flex className="jc-ticket-filter-row" gap="2" wrap="wrap">
                <Select.Root value={status || "_all"} onValueChange={(v) => setStatus(v === "_all" ? "" : v)}>
                  <Select.Trigger placeholder="状态" />
                  <Select.Content>
                    <Select.Item value="_all">全部状态</Select.Item>
                    <Select.Item value="open">待处理</Select.Item>
                    <Select.Item value="in_progress">处理中</Select.Item>
                    <Select.Item value="waiting_user">等待回复</Select.Item>
                    <Select.Item value="resolved">已解决</Select.Item>
                    <Select.Item value="closed">已关闭</Select.Item>
                  </Select.Content>
                </Select.Root>
                <Select.Root value={category || "_all"} onValueChange={(v) => setCategory(v === "_all" ? "" : v)}>
                  <Select.Trigger placeholder="类型" />
                  <Select.Content>
                    <Select.Item value="_all">全部类型</Select.Item>
                    <Select.Item value="general">一般</Select.Item>
                    <Select.Item value="refund">退款</Select.Item>
                    <Select.Item value="billing">账单</Select.Item>
                    <Select.Item value="technical">技术</Select.Item>
                    <Select.Item value="account">账号</Select.Item>
                  </Select.Content>
                </Select.Root>
                <Select.Root value={assignee || "_any"} onValueChange={(v) => setAssignee(v === "_any" ? "" : v)}>
                  <Select.Trigger placeholder="责任人" />
                  <Select.Content>
                    <Select.Item value="_any">全部</Select.Item>
                    <Select.Item value="me">分配给我</Select.Item>
                    <Select.Item value="unassigned">未分配</Select.Item>
                  </Select.Content>
                </Select.Root>
              </Flex>
              <ClearableTextField placeholder="搜索工单号 / 标题 / 内容 / 邮箱 / 订单号" value={q} onChange={(e) => setQ(e.target.value)} />
              {userFilter && (
                <Flex align="center" gap="2">
                  <Badge variant="soft">该用户的工单</Badge>
                  <Button size="1" variant="ghost" onClick={() => {
                    const next = new URLSearchParams(searchParams);
                    next.delete("user");
                    setSearchParams(next);
                  }}>清除</Button>
                </Flex>
              )}
              <Box style={{ flex: "1 1 0", height: 0, minHeight: 0, overflowY: "auto" }}>
                {tickets.map((t) => (
                  <TicketListItem key={t.id} t={t} selected={t.id === selectedId} onClick={() => openTicket(t.id)} />
                ))}
                {!list.isLoading && tickets.length === 0 && (
                  <Text size="2" color="gray">无匹配工单</Text>
                )}
              </Box>
            </Flex>
          </Card>
        </Box>

        <Box className="jc-ticket-detail-pane" style={{ flex: 1, minWidth: 0, display: "flex", minHeight: 0, overflow: "visible" }}>
          {selectedId ? detail.data ? (
            <TicketThread
              key={detail.data.ticket.id}
              detail={detail.data}
              assignees={assignees.data ?? []}
              canAssignOthers={isAdminUser}
              currentUserId={currentUserId}
              onChanged={() => {
                qc.invalidateQueries({ queryKey: ["cs", "tickets"] });
                qc.invalidateQueries({ queryKey: ["cs", "ticket-counts"] });
                qc.invalidateQueries({ queryKey: ["cs", "ticket", selectedId] });
              }}
            />
          ) : (
            <Card className="jc-ticket-detail-card" style={{ flex: 1, minHeight: 0 }}>
              <Text size="2" color="gray">正在加载工单详情…</Text>
            </Card>
          ) : (
            <Card className="jc-ticket-detail-card" style={{ flex: 1, minHeight: 0 }}>
              <Flex direction="column" align="center" justify="center" gap="3" style={{ height: "100%", minHeight: 220 }}>
                <Inbox size={32} style={{ opacity: 0.35 }} />
                <Text size="2" color="gray">选择左侧的工单进行处理</Text>
              </Flex>
            </Card>
          )}
        </Box>
      </Flex>
    </Box>
  );
}

function TicketListItem({ t, selected, onClick }: { t: TicketRow; selected: boolean; onClick: () => void }) {
  return (
    <div
      className={`jc-ticket-list-item${selected ? " is-selected" : ""}`}
      onClick={onClick}
    >
      <Flex justify="between" align="center" mb="1" gap="2">
        <Flex align="center" gap="2" style={{ minWidth: 0 }}>
          {t.unread_kind === "reply" && <Badge size="1" color="red" variant="solid">新回复</Badge>}
          {t.unread_kind === "unread" && <span title="未读" style={{ width: 8, height: 8, borderRadius: 99, background: "var(--accent-9)", flexShrink: 0, display: "inline-block" }} />}
          <Text size="2" weight="medium" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
            {t.ticket_no_label ? `${t.ticket_no_label} · ` : ""}{t.title}
          </Text>
        </Flex>
        <Flex gap="1">
          {t.category && t.category !== "general" && (
            <Badge size="1" color={categoryColor(t.category)} variant="soft">{categoryLabel(t.category)}</Badge>
          )}
          <Badge size="1" color={priorityColor(t.priority)} variant="soft">{priorityLabel(t.priority)}</Badge>
          <Badge size="1" color={statusColor(t.status)} variant="soft">{statusLabel(t.status)}</Badge>
        </Flex>
      </Flex>
      <Flex justify="between">
        <Text size="1" color="gray">
          {t.user_email || t.user_id?.slice(0, 8)}
          {t.order_id ? ` · 订单 ${t.order_id.slice(0, 8)}` : ""}
        </Text>
        <Text size="1" color="gray">{new Date(t.last_reply_at).toLocaleString()}</Text>
      </Flex>
    </div>
  );
}

function assigneeLabel(u: TicketAssignee): string {
  const name = u.nickname?.trim();
  return name ? `${name} (${u.email})` : u.email;
}

function compareTickets(a: TicketRow, b: TicketRow) {
  const unreadOrder = Number(!!b.unread) - Number(!!a.unread);
  if (unreadOrder !== 0) return unreadOrder;
  const closedOrder = Number(a.status === "closed") - Number(b.status === "closed");
  if (closedOrder !== 0) return closedOrder;
  return Date.parse(b.last_reply_at) - Date.parse(a.last_reply_at);
}

function TicketThread({ detail, assignees, canAssignOthers, currentUserId, onChanged }: { detail: TicketDetail; assignees: TicketAssignee[]; canAssignOthers: boolean; currentUserId: string; onChanged: () => void }) {
  const [body, setBody] = useState("");
  const [internalNote, setInternalNote] = useState(false);
  const [attachments, setAttachments] = useState<TicketAttachment[]>([]);
  const messagesRef = useRef<HTMLDivElement | null>(null);
  const reply = useMutation({
    mutationFn: () => csApi.replyTicket(detail.ticket.id, body, internalNote, attachments),
    onSuccess: () => { setBody(""); setInternalNote(false); setAttachments([]); onChanged(); },
  });
  const assign = useMutation({
    mutationFn: (v: "me" | "" | string) => csApi.assignTicket(detail.ticket.id, v),
    onSuccess: onChanged,
  });
  const setStatus = useMutation({
    mutationFn: (s: TicketStatus) => csApi.setTicketStatus(detail.ticket.id, s),
    onSuccess: onChanged,
  });
  const [refundOpen, setRefundOpen] = useState(false);
  const createRefund = useMutation({
    mutationFn: (body: { amount?: string; note?: string }) => csApi.createTicketRefund(detail.ticket.id, body),
    onSuccess: () => { setRefundOpen(false); onChanged(); },
  });
  const canReply = detail.ticket.status !== "closed" && (body.trim().length > 0 || attachments.length > 0) && !reply.isPending;
  const assigneeOptions = assignees.filter((u) => u.id !== currentUserId);
  const missingAssignee = detail.ticket.assignee_user_id && detail.ticket.assignee_user_id !== currentUserId && !assigneeOptions.some((u) => u.id === detail.ticket.assignee_user_id);
  const userEditHref = detail.ticket.user_id
    ? `${canAssignOthers ? "/admin/users?edit=" : "/cs/users?user="}${encodeURIComponent(detail.ticket.user_id)}`
    : "";
  const sendReply = () => {
    if (!canReply) return;
    reply.mutate();
  };
  const handleReplyKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    sendReply();
  };

  useEffect(() => {
    const el = messagesRef.current;
    if (!el) return;

    const frame = requestAnimationFrame(() => {
      el.scrollTop = el.scrollHeight;
    });

    return () => cancelAnimationFrame(frame);
  }, [detail.ticket.id, detail.messages.length]);

  return (
    <Card className="jc-ticket-detail-card" style={{ flex: 1, minWidth: 0, minHeight: 0, overflow: "hidden", display: "flex", flexDirection: "column" }}>
      <Flex justify="between" align="start" mb="3" gap="2" wrap="wrap">
        <Box>
          <Text size="3" weight="bold">
            {detail.ticket.ticket_no_label ? `${detail.ticket.ticket_no_label} · ` : ""}{detail.ticket.title}
          </Text>
          <Flex gap="2" mt="1" align="center" wrap="wrap">
            <Badge size="1" color={statusColor(detail.ticket.status)} variant="soft">{statusLabel(detail.ticket.status)}</Badge>
            {detail.ticket.category && (
              <Badge size="1" color={categoryColor(detail.ticket.category)} variant="soft">{categoryLabel(detail.ticket.category)}</Badge>
            )}
            <Badge size="1" color={priorityColor(detail.ticket.priority)} variant="soft">{priorityLabel(detail.ticket.priority)}</Badge>
            {detail.ticket.order_id && <Badge size="1" variant="soft">订单 {detail.ticket.order_id.slice(0, 8)}</Badge>}
            <Text size="1" color="gray">用户: {detail.ticket.user_email || detail.ticket.user_id}</Text>
            {userEditHref && (
              <Button asChild size="1" variant="soft" color="gray">
                <Link to={userEditHref}>编辑用户</Link>
              </Button>
            )}
          </Flex>
        </Box>
        <Flex className="jc-ticket-thread-actions" gap="2" wrap="wrap">
          {canAssignOthers ? (
            <Select.Root
              value={detail.ticket.assignee_user_id || "_unassigned"}
              onValueChange={(v) => assign.mutate(v === "_unassigned" ? "" : v)}
            >
              <Select.Trigger placeholder="指派客服" />
              <Select.Content>
                <Select.Item value="_unassigned">未分配</Select.Item>
                {currentUserId && <Select.Item value={currentUserId}>指派给我</Select.Item>}
                {assigneeOptions.map((u) => (
                  <Select.Item key={u.id} value={u.id}>{assigneeLabel(u)}</Select.Item>
                ))}
                {missingAssignee && (
                  <Select.Item value={detail.ticket.assignee_user_id!}>当前指派 {detail.ticket.assignee_user_id!.slice(0, 8)}</Select.Item>
                )}
              </Select.Content>
            </Select.Root>
          ) : detail.ticket.assignee_user_id ? (
            <Button color="gray" variant="soft" size="2" onClick={() => assign.mutate("")} loading={assign.isPending}>取消分配</Button>
          ) : (
            <Button color="blue" variant="soft" size="2" onClick={() => assign.mutate("me")} loading={assign.isPending}>认领</Button>
          )}
          <Select.Root value={detail.ticket.status} onValueChange={(v) => setStatus.mutate(v as TicketStatus)}>
            <Select.Trigger />
            <Select.Content>
              <Select.Item value="open">待处理</Select.Item>
              <Select.Item value="in_progress">处理中</Select.Item>
              <Select.Item value="waiting_user">等待回复</Select.Item>
              <Select.Item value="resolved">已解决</Select.Item>
              <Select.Item value="closed">已关闭</Select.Item>
            </Select.Content>
          </Select.Root>
          {detail.ticket.category === "refund" && detail.ticket.order_id && detail.ticket.status !== "closed" && (
            <Button
              size="2"
              variant="soft"
              color="amber"
              disabled={createRefund.isSuccess}
              onClick={() => setRefundOpen(true)}
            >
              {createRefund.isSuccess ? "已提交退款单" : "提交退款单…"}
            </Button>
          )}
        </Flex>
      </Flex>
      {assign.isError && <Text size="2" color="red" mb="2">指派失败: {errMsg(assign.error)}</Text>}
      {setStatus.isError && <Text size="2" color="red" mb="2">改状态失败: {errMsg(setStatus.error)}</Text>}
      {reply.isError && <Text size="2" color="red" mb="2">回复失败: {errMsg(reply.error)}</Text>}
      {createRefund.isSuccess && <Text size="2" color="green" mb="2">已提交退款单，请到「退款审批」确认</Text>}
      <TicketRefundDialog
        open={refundOpen}
        orderId={detail.ticket.order_id ?? ""}
        pending={createRefund.isPending}
        error={createRefund.error}
        onClose={() => setRefundOpen(false)}
        onSubmit={(body) => createRefund.mutate(body)}
      />
      <Box ref={messagesRef} className="jc-ticket-messages" style={{ flex: "1 1 0", minHeight: 180, overflowY: "auto", paddingRight: 4 }}>
        {detail.messages.map((m) => (
          <MessageBubble key={m.id} m={m} />
        ))}
      </Box>
      {detail.ticket.status !== "closed" && (
        <Box mt="3" style={{ flexShrink: 0 }}>
          <TextArea
            placeholder="回复内容…"
            rows={4}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            onKeyDown={handleReplyKeyDown}
          />
          <Flex justify="between" mt="2" align="center">
            <Flex gap="3" align="center" wrap="wrap">
              <Text as="label" size="2">
                <Flex gap="2" align="center">
                  <Checkbox checked={internalNote} onCheckedChange={(v) => setInternalNote(!!v)} />
                  内部备注 (用户不可见)
                </Flex>
              </Text>
              <AttachmentPicker attachments={attachments} onChange={setAttachments} />
            </Flex>
            <Button disabled={!canReply} onClick={sendReply}>发送</Button>
          </Flex>
        </Box>
      )}
    </Card>
  );
}

// Same two fields as the admin refund dialog in admin/Finance.tsx: an amount
// that may be left blank so the backend applies the refund policy, plus a note.
function TicketRefundDialog({
  open,
  orderId,
  pending,
  error,
  onClose,
  onSubmit,
}: {
  open: boolean;
  orderId: string;
  pending: boolean;
  error: unknown;
  onClose: () => void;
  onSubmit: (body: { amount?: string; note?: string }) => void;
}) {
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const refundable = useQuery({
    queryKey: ["cs", "order-refundable", orderId],
    queryFn: () => csApi.orderRefundable(orderId),
    enabled: open && !!orderId,
  });
  // 打开时把可退金额填进去，客服可以改。
  useEffect(() => {
    if (open && refundable.data?.balance_bearing) setAmount(refundable.data.cash);
  }, [open, refundable.data]);
  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="480px">
        <Dialog.Title>提交退款单</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Text size="2" color="gray">订单 {orderId.slice(0, 8)}，提交后到「退款审批」确认才会实际退款。支付渠道支持自动退款的订单审批后原路退回。</Text>
          <OrderRefundableHint data={refundable.data} loading={refundable.isLoading} />
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">退款金额</Text>
            <ClearableTextField
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              placeholder="留空按可退金额"
            />
          </Box>
          <Box>
            <Text size="2" weight="medium" as="div" mb="1">备注</Text>
            <TextArea value={note} onChange={(e) => setNote(e.target.value)} rows={3} />
          </Box>
          {error != null && <Text size="2" color="red">提交失败: {errMsg(error)}</Text>}
          <Flex justify="end" gap="2">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button
              loading={pending}
              onClick={() => onSubmit({ amount: amount.trim() || undefined, note: note.trim() || undefined })}
            >
              提交退款单
            </Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function MessageBubble({ m }: { m: TicketMessage }) {
  const fromStaff = m.sender_role === "cs" || m.sender_role === "admin";
  const bg = m.internal_note ? "var(--amber-3)" : fromStaff ? "var(--accent-3)" : "var(--gray-3)";
  return (
    <Flex justify={fromStaff ? "end" : "start"} mb="3">
      <Box style={{ maxWidth: "82%", padding: "10px 14px", borderRadius: 12, background: bg }}>
        <Text size="1" color="gray">
          {m.sender_role === "cs" ? "客服" : m.sender_role === "admin" ? "管理员" : m.sender_role === "user" ? "用户" : "系统"}
          {m.internal_note ? " · 内部备注" : ""} · {new Date(m.created_at).toLocaleString()}
        </Text>
        <div style={{ whiteSpace: "pre-wrap", marginTop: 4 }}>{m.body}</div>
        <MessageAttachments attachments={m.attachments ?? []} />
      </Box>
    </Flex>
  );
}

function categoryLabel(c: TicketCategory): string {
  return ({ general: "一般", refund: "退款", billing: "账单", technical: "技术", account: "账号" } as const)[c];
}
function categoryColor(c: TicketCategory): "gray" | "amber" | "blue" | "violet" {
  return ({ general: "gray", refund: "amber", billing: "blue", technical: "violet", account: "gray" } as const)[c];
}
function statusLabel(s: TicketStatus): string {
  return ({ open: "待处理", in_progress: "处理中", waiting_user: "等待回复", resolved: "已解决", closed: "已关闭" } as const)[s];
}
function statusColor(s: TicketStatus): "blue" | "amber" | "green" | "gray" | "orange" {
  return ({ open: "blue", in_progress: "orange", waiting_user: "amber", resolved: "green", closed: "gray" } as const)[s];
}
function priorityLabel(p: TicketPriority): string {
  return ({ low: "低", normal: "普通", high: "高", urgent: "紧急" } as const)[p];
}
function priorityColor(p: TicketPriority): "gray" | "blue" | "amber" | "red" {
  return ({ low: "gray", normal: "blue", high: "amber", urgent: "red" } as const)[p];
}
