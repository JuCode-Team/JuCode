import "../../personal-responsive.css";
import { useState } from "react";
import { Badge, Button, Dialog, Flex, Select, Text, TextArea } from "@radix-ui/themes";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Check, X } from "lucide-react";
import { csApi, type SiteRefund } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { AdminPage } from "../../components/AdminPage";
import { PointAmount } from "../../components/PointAmount";

type RefundStatus = SiteRefund["status"] | "";
type ReviewAction = "approve" | "reject";

type ReviewTarget = {
  refund: SiteRefund;
  action: ReviewAction;
} | null;

function dt(value?: string): string {
  return value ? new Date(value).toLocaleString() : "-";
}

function shortID(value?: string): string {
  return value ? value.slice(0, 8) : "-";
}

function refundStatusLabel(status: SiteRefund["status"]): string {
  return ({
    pending: "待审批",
    approved: "已通过",
    rejected: "已拒绝",
    canceled: "已取消",
  } as const)[status];
}

function refundStatusColor(status: SiteRefund["status"]): "gray" | "green" | "amber" | "red" {
  return ({
    pending: "amber",
    approved: "green",
    rejected: "red",
    canceled: "gray",
  } as const)[status];
}

export default function CSRefunds() {
  const qc = useQueryClient();
  const [status, setStatus] = useState<RefundStatus>("pending");
  const [target, setTarget] = useState<ReviewTarget>(null);

  // /v1/cs/refunds returns limit/offset/total; without paging the queue
  // silently truncated at 50 and older requests were unreachable.
  const paged = usePagedQuery<SiteRefund>({
    queryKey: ["cs", "refunds"],
    queryFn: async (p) => {
      const res = await csApi.refunds({ status: status || undefined, limit: p.limit, offset: p.offset });
      return { items: res.refunds, total: res.total, limit: res.limit, offset: res.offset };
    },
    extras: [status],
    pageSize: 20,
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["cs", "refunds"] });

  const cols: Column<SiteRefund>[] = [
    { header: "订单", width: "90px", cell: (r) => shortID(r.order_id) },
    { header: "用户", width: "90px", cell: (r) => shortID(r.user_id) },
    {
      header: "退款金额", width: "110px", align: "right",
      cell: (r) => <span className="jc-num"><PointAmount amount={r.amount} decimals="preserve" iconSize={12} /></span>,
    },
    {
      header: "状态", width: "90px",
      cell: (r) => <Badge color={refundStatusColor(r.status)} variant="soft">{refundStatusLabel(r.status)}</Badge>,
    },
    { header: "原因", minWidth: "180px", cell: (r) => <Text size="1" color="gray">{r.reason || "-"}</Text> },
    { header: "创建时间", minWidth: "150px", cell: (r) => dt(r.created_at) },
    {
      header: "操作", width: "150px", align: "right",
      cell: (r) => r.status === "pending" ? (
        <Flex gap="1" justify="end">
          <Button size="1" variant="soft" color="green" onClick={() => setTarget({ refund: r, action: "approve" })}>
            <Check size={13} />通过
          </Button>
          <Button size="1" variant="soft" color="red" onClick={() => setTarget({ refund: r, action: "reject" })}>
            <X size={13} />拒绝
          </Button>
        </Flex>
      ) : null,
    },
  ];

  return (
    <AdminPage title="退款审批" description="审核站内订单退款，审批通过后系统会自动撤销对应权益。">
      <DataTable<SiteRefund>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={cols}
        rowKey={(r) => r.id}
        emptyText="暂无退款申请"
        filters={(
          <Select.Root value={status || "_all"} onValueChange={(v) => setStatus(v === "_all" ? "" : v as RefundStatus)}>
            <Select.Trigger placeholder="状态" />
            <Select.Content>
              <Select.Item value="_all">全部状态</Select.Item>
              <Select.Item value="pending">待审批</Select.Item>
              <Select.Item value="approved">已通过</Select.Item>
              <Select.Item value="rejected">已拒绝</Select.Item>
              <Select.Item value="canceled">已取消</Select.Item>
            </Select.Content>
          </Select.Root>
        )}
      />
      {paged.query.isError && <Text color="red" mt="3" as="div">{errMsg(paged.query.error)}</Text>}
      <ReviewDialog target={target} onClose={() => setTarget(null)} onSaved={invalidate} />
    </AdminPage>
  );
}

function ReviewDialog({
  target,
  onClose,
  onSaved,
}: {
  target: ReviewTarget;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [note, setNote] = useState("");
  const review = useMutation({
    mutationFn: () => {
      if (!target) throw new Error("missing refund");
      return target.action === "approve"
        ? csApi.approveRefund(target.refund.id, note)
        : csApi.rejectRefund(target.refund.id, note);
    },
    onSuccess: () => {
      setNote("");
      onClose();
      onSaved();
    },
  });
  const actionLabel = target?.action === "approve" ? "通过" : "拒绝";

  return (
    <Dialog.Root open={!!target} onOpenChange={(open) => !open && onClose()}>
      <Dialog.Content maxWidth="520px">
        <Dialog.Title>{actionLabel}退款</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Text size="2" color="gray">
            订单 {shortID(target?.refund.order_id)}，退款金额 <PointAmount amount={target?.refund.amount} decimals="preserve" iconSize={12} />
          </Text>
          <TextArea
            value={note}
            onChange={(e) => setNote(e.target.value)}
            rows={4}
            placeholder="审批备注"
          />
          {review.isError && <Text color="red" size="2">{errMsg(review.error)}</Text>}
          <Flex justify="end" gap="2">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button color={target?.action === "approve" ? "green" : "red"} loading={review.isPending} onClick={() => review.mutate()}>
              {actionLabel}
            </Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
