import { useState } from "react";
import { Badge, Box, Card, Flex, Select, Table, Text } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { agentApi, type AgentEntryKind } from "../../api/endpoints";
import { PointAmount } from "../../components/PointAmount";
import { PageHeader } from "../../components/PageHeader";

const kindLabel: Record<AgentEntryKind, { text: string; color: "green" | "amber" | "iris" | "red" }> = {
  sale: { text: "卡密销售", color: "green" },
  refund: { text: "卡密退款", color: "amber" },
  coupon_rebate: { text: "优惠码返利", color: "iris" },
  coupon_refund: { text: "返利冲减", color: "red" },
  balance_withdraw: { text: "提现到余额", color: "amber" },
};

// Settlement ledger: redeem-code sales/refunds and coupon rebates/reversals.
// Filterable by status (pending = settlement_id NULL, settled = bundled).
// platform_payable is what the agent owes the platform; coupon rebates are
// negative (the platform owes the agent). Amounts are platform points
// (1 积分 = 1 元, see biz/agent.go).
export default function AgentSales() {
  const [kind, setKind] = useState<string>("");
  const [status, setStatus] = useState<string>("pending");
  const { data } = useQuery({
    queryKey: ["agent", "sales", kind, status],
    queryFn: () => agentApi.sales({
      kind: (kind || undefined) as AgentEntryKind | undefined,
      status: (status || undefined) as "pending" | "settled" | undefined,
    }),
  });

  return (
    <Box>
      <PageHeader
        title="销售流水"
        description="卡密销售、退款与优惠码返利的结算明细"
      />
      <Card>
        <Flex gap="2" mb="3">
          <Select.Root value={kind || "_all"} onValueChange={(v) => setKind(v === "_all" ? "" : v)}>
            <Select.Trigger placeholder="类型" />
            <Select.Content>
              <Select.Item value="_all">全部</Select.Item>
              {(Object.keys(kindLabel) as AgentEntryKind[]).map((k) => (
                <Select.Item key={k} value={k}>{kindLabel[k].text}</Select.Item>
              ))}
            </Select.Content>
          </Select.Root>
          <Select.Root value={status || "_all"} onValueChange={(v) => setStatus(v === "_all" ? "" : v)}>
            <Select.Trigger placeholder="结算状态" />
            <Select.Content>
              <Select.Item value="_all">全部</Select.Item>
              <Select.Item value="pending">未结算</Select.Item>
              <Select.Item value="settled">已结算</Select.Item>
            </Select.Content>
          </Select.Root>
        </Flex>
        <div className="jc-local-table-scroll">
        <Table.Root>
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>时间</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>类型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>用户ID</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">金额</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">应付平台</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">比例</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>状态</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {(data?.entries ?? []).map((e) => (
              <Table.Row key={e.id}>
                <Table.Cell>{new Date(e.created_at).toLocaleString()}</Table.Cell>
                <Table.Cell>
                  <Badge size="1" color={kindLabel[e.kind]?.color ?? "gray"} variant="soft">
                    {kindLabel[e.kind]?.text ?? e.kind}
                  </Badge>
                </Table.Cell>
                <Table.Cell><code style={{ fontSize: 11 }}>{e.user_id?.slice(0, 8) ?? "—"}</code></Table.Cell>
                <Table.Cell align="right"><PointAmount amount={e.sale_amount} iconSize={12} /></Table.Cell>
                <Table.Cell align="right"><PointAmount amount={e.platform_payable} iconSize={12} /></Table.Cell>
                <Table.Cell align="right">{e.share_pct_at_event}%</Table.Cell>
                <Table.Cell>
                  <Badge size="1" variant="soft" color={e.settlement_id ? "gray" : "blue"}>
                    {e.settlement_id ? "已结算" : "未结算"}
                  </Badge>
                </Table.Cell>
              </Table.Row>
            ))}
            {(data?.entries ?? []).length === 0 && (
              <Table.Row>
                <Table.Cell colSpan={7}><Text size="2" color="gray">无流水</Text></Table.Cell>
              </Table.Row>
            )}
          </Table.Body>
        </Table.Root>
        </div>
      </Card>
    </Box>
  );
}
