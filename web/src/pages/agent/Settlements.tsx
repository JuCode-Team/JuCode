import { Box, Card, Table, Text } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { agentApi } from "../../api/endpoints";
import { PointAmount } from "../../components/PointAmount";
import { PageHeader } from "../../components/PageHeader";

// Agent's settlement history. Each row is one admin-triggered "settle"
// click that closed out a batch of pending entries.
export default function AgentSettlements() {
  const { data } = useQuery({ queryKey: ["agent", "settlements"], queryFn: () => agentApi.settlements({}) });
  return (
    <Box>
      <PageHeader
        title="结算历史"
        description="管理员每次结算后的快照"
      />
      <Card className="pn-card-flat jc-table-panel">
        <div className="jc-local-table-scroll">
        <Table.Root>
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>时间</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>流水数</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">应付平台</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>备注</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {(data?.settlements ?? []).map((s) => (
              <Table.Row key={s.id}>
                <Table.Cell>{new Date(s.created_at).toLocaleString()}</Table.Cell>
                <Table.Cell>{s.entries_count}</Table.Cell>
                <Table.Cell align="right"><PointAmount amount={s.total_payable} iconSize={12} /></Table.Cell>
                <Table.Cell><Text size="1" color="gray">{s.note || "—"}</Text></Table.Cell>
              </Table.Row>
            ))}
            {(data?.settlements ?? []).length === 0 && (
              <Table.Row><Table.Cell colSpan={4}><Text size="2" color="gray">尚无结算记录</Text></Table.Cell></Table.Row>
            )}
          </Table.Body>
        </Table.Root>
        </div>
      </Card>
    </Box>
  );
}
