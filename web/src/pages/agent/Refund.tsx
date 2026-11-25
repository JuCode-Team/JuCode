import { useState } from "react";
import { Badge, Box, Button, Card, Flex, Text, TextArea } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { agentApi, type RefundPreview } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { PageHeader } from "../../components/PageHeader";
import { useSite } from "../../store/site";

// Refund registration form. Flow:
//   1. Agent pastes a code → backend resolves which record + preview math.
//   2. Agent confirms reason and submits → backend writes refund row +
//      settlement entry + deletes user_plan (for plan kind).
//
// Refund payout itself (the money the agent wires back to the user) is
// handled out-of-band — the platform only updates the settlement
// ledger so the agent's eventual settle covers the math.
export default function AgentRefund() {
  const qc = useQueryClient();
  const site = useSite();
  const [code, setCode] = useState("");
  const [preview, setPreview] = useState<RefundPreview | null>(null);
  const [reason, setReason] = useState("");

  const lookup = useMutation({
    mutationFn: () => agentApi.previewRefund(code),
    onSuccess: (p) => setPreview(p),
    onError: () => setPreview(null),
  });
  const submit = useMutation({
    mutationFn: () => agentApi.registerRefund({ redeem_record_id: preview!.redeem_record_id, reason: reason || undefined }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["agent"] });
      setCode(""); setPreview(null); setReason("");
    },
  });

  const refunds = useQuery({ queryKey: ["agent", "refunds-recent"], queryFn: () => agentApi.refunds({ limit: 10 }) });

  return (
    <Box>
      <PageHeader
        title="登记退款"
        description={site.agent_plan_refund_day_divisor
          ? `输入兑换码 → 预览计算 → 确认提交。套餐码按剩余天数 ÷ ${site.agent_plan_refund_day_divisor} 折算原价后退 ${site.agent_plan_refund_rate_pct}%，余额码退面额的 ${site.agent_balance_refund_rate_pct}%。`
          : "输入兑换码 → 预览计算 → 确认提交"}
      />
      <Card>
        <Flex direction="column" gap="3">
          <Box>
            <Text as="label" size="2" weight="medium">兑换码</Text>
            <Flex gap="2">
              <ClearableTextField style={{ flex: 1 }} placeholder="JUC-XXXXXX-XXXXXX" value={code} onChange={(e) => setCode(e.target.value)} />
              <Button disabled={!code || lookup.isPending} onClick={() => lookup.mutate()}>
                <Search size={14} /> 查询
              </Button>
            </Flex>
            {lookup.isError && <Text size="2" color="red" mt="2" as="div">查询失败: {errMsg(lookup.error)}</Text>}
          </Box>
          {preview && (
            <Box style={{ background: "var(--gray-3)", padding: 12, borderRadius: 8 }}>
              <Flex justify="between" wrap="wrap" gap="3">
                <Field label="类型">{preview.kind === "balance" ? "余额码" : "套餐码"}</Field>
                <Field label="用户">{preview.user_id.slice(0, 8)}</Field>
                {preview.plan_name && <Field label="套餐">{preview.plan_name}</Field>}
                {preview.remaining_days !== undefined && <Field label="剩余天数">{preview.remaining_days.toFixed(2)}</Field>}
                {preview.plan_price && <Field label="套餐原价">{preview.plan_price}</Field>}
                <Field label="退款金额" highlight><PointAmount amount={preview.refund_amount} iconSize={13} /></Field>
                <Field label="平台扣减" highlight><PointAmount amount={preview.platform_deduct} iconSize={13} /></Field>
              </Flex>
              {preview.already_refunded && (
                <Badge color="red" variant="soft" mt="2">该兑换码已经退过款</Badge>
              )}
            </Box>
          )}
          <Box>
            <Text as="label" size="2" weight="medium">退款原因 (可选)</Text>
            <TextArea rows={3} value={reason} onChange={(e) => setReason(e.target.value)} placeholder="用户协商退款 / 重复购买 / 等" />
          </Box>
          <Flex justify="end">
            <Button
              color="red"
              disabled={!preview || preview.already_refunded || submit.isPending}
              onClick={() => submit.mutate()}
            >
              确认退款 {preview && <PointAmount amount={preview.refund_amount} iconSize={13} />}
            </Button>
          </Flex>
          {submit.isError && <Text size="2" color="red">登记失败: {errMsg(submit.error)}</Text>}
          {submit.isSuccess && (
            <Text size="2" color="green">退款已登记。{submit.data?.user_plan_removed ? "用户套餐已移除。" : ""}</Text>
          )}
        </Flex>
      </Card>

      <Box mt="4">
        <Card>
          <Text size="2" weight="medium" mb="2" as="div">最近退款</Text>
          {(refunds.data?.refunds ?? []).length === 0 ? (
            <Text size="2" color="gray">暂无退款记录</Text>
          ) : (
            (refunds.data?.refunds ?? []).map((r) => {
              return (
                <Box key={r.id} mb="2" style={{ paddingBottom: 8, borderBottom: "1px solid var(--gray-4)" }}>
                  <Flex justify="between" wrap="wrap" gap="2">
                    <Text size="2">
                      {r.kind === "plan" ? "套餐" : "余额"} · 退款 <PointAmount amount={r.refund_amount} iconSize={12} /> · 扣减 <PointAmount amount={r.platform_deduct} iconSize={12} />
                    </Text>
                    <Text size="1" color="gray">{new Date(r.created_at).toLocaleString()}</Text>
                  </Flex>
                  {r.reason && <Text size="1" color="gray">{r.reason}</Text>}
                </Box>
              );
            })
          )}
        </Card>
      </Box>
    </Box>
  );
}

function Field({ label, children, highlight }: { label: string; children: React.ReactNode; highlight?: boolean }) {
  return (
    <Box>
      <Text size="1" color="gray">{label}</Text>
      <Text size="3" weight={highlight ? "bold" : "regular"} color={highlight ? "iris" : undefined} as="div">{children}</Text>
    </Box>
  );
}
