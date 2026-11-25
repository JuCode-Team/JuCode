import { useState } from "react";
import {
  Badge,
  Box,
  Button,
  Card,
  Dialog,
  Flex,
  Select,
  Table,
  Text,
} from "@radix-ui/themes";
import { Copy, Plus } from "lucide-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { PointAmount } from "../../components/PointAmount";
import { adminApi, agentApi } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../../components/ClearableTextField";
import { PageHeader } from "../../components/PageHeader";

// Agent's redeem-code workshop: generate balance / plan codes, see
// usage. Generated codes carry agent_id so redemptions feed the
// agent's settlement ledger automatically.
export default function AgentCodes() {
  const qc = useQueryClient();
  const codes = useQuery({ queryKey: ["agent", "codes"], queryFn: agentApi.codes });
  const [open, setOpen] = useState(false);
  return (
    <Box>
      <PageHeader
        title="兑换码"
        description="生成可售卖给用户的兑换码"
        actions={
          <Button onClick={() => setOpen(true)}><Plus size={14} /> 生成兑换码</Button>
        }
      />
      <Card className="pn-card-flat jc-table-panel">
        <div className="jc-local-table-scroll">
        <Table.Root>
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>兑换码</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>类型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>面值 / 套餐</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>状态</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>使用</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>生成时间</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {(codes.data ?? []).map((c) => (
              <Table.Row key={c.id}>
                <Table.Cell>
                  <Flex gap="1" align="center">
                    <code style={{ fontSize: 12 }}>{c.code}</code>
                    <Button size="1" variant="ghost" onClick={() => navigator.clipboard.writeText(c.code)}>
                      <Copy size={12} />
                    </Button>
                  </Flex>
                </Table.Cell>
                <Table.Cell>{c.kind === "balance" ? "余额" : "套餐"}</Table.Cell>
                <Table.Cell>{c.amount ? <PointAmount amount={c.amount} decimals="preserve" iconSize={12} /> : (c.plan_id?.slice(0, 8) ?? "—")}</Table.Cell>
                <Table.Cell>
                  <Badge size="1" color={statusColor(c.status)} variant="soft">{statusLabel(c.status)}</Badge>
                </Table.Cell>
                <Table.Cell>{c.used_count}/{c.max_uses}</Table.Cell>
                <Table.Cell>{new Date(c.created_at).toLocaleString()}</Table.Cell>
              </Table.Row>
            ))}
            {!codes.isLoading && (codes.data ?? []).length === 0 && (
              <Table.Row>
                <Table.Cell colSpan={6}><Text size="2" color="gray">还没有兑换码</Text></Table.Cell>
              </Table.Row>
            )}
          </Table.Body>
        </Table.Root>
        </div>
      </Card>
      <GenerateDialog open={open} onClose={() => setOpen(false)} onDone={() => qc.invalidateQueries({ queryKey: ["agent", "codes"] })} />
    </Box>
  );
}

function statusLabel(s: string) {
  return ({ active: "可用", exhausted: "已用完", revoked: "已作废", expired: "已过期" } as Record<string, string>)[s] ?? s;
}
function statusColor(s: string): "green" | "gray" | "red" | "amber" {
  return ({ active: "green", exhausted: "gray", revoked: "red", expired: "amber" } as Record<string, "green" | "gray" | "red" | "amber">)[s] ?? "gray";
}

function GenerateDialog({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => void }) {
  const [kind, setKind] = useState<"balance" | "plan">("balance");
  const [amount, setAmount] = useState("");
  const [planId, setPlanId] = useState("");
  const [count, setCount] = useState("1");
  const [generated, setGenerated] = useState<string[] | null>(null);
  const plans = useQuery({ queryKey: ["plans-all"], queryFn: () => adminApi.plans() });
  const m = useMutation({
    mutationFn: () => agentApi.generate({
      kind,
      amount: kind === "balance" ? amount : undefined,
      plan_id: kind === "plan" ? planId : undefined,
      count: parseInt(count || "1", 10),
    }),
    onSuccess: (r) => {
      setGenerated(r.codes.map((c) => c.code));
      onDone();
    },
  });
  const reset = () => {
    setKind("balance"); setAmount(""); setPlanId(""); setCount("1"); setGenerated(null);
  };
  return (
    <Dialog.Root open={open} onOpenChange={(v) => { if (!v) { reset(); onClose(); } }}>
      <Dialog.Content maxWidth="520px">
        <Dialog.Title>生成兑换码</Dialog.Title>
        {generated ? (
          <Flex direction="column" gap="2" mt="3">
            <Text size="2">已生成 {generated.length} 条兑换码，请复制保存:</Text>
            <Box style={{ background: "var(--gray-3)", padding: 12, borderRadius: 8, maxHeight: 200, overflowY: "auto" }}>
              {generated.map((c) => <div key={c} style={{ fontFamily: "monospace", fontSize: 13 }}>{c}</div>)}
            </Box>
            <Flex gap="2" justify="end">
              <Button onClick={() => navigator.clipboard.writeText(generated.join("\n"))}>复制全部</Button>
              <Button variant="soft" onClick={() => { reset(); onClose(); }}>关闭</Button>
            </Flex>
          </Flex>
        ) : (
          <Flex direction="column" gap="4" mt="3">
            <Flex direction="column" gap="1">
              <Text as="div" size="2" weight="medium">类型</Text>
              <Select.Root value={kind} onValueChange={(v) => setKind(v as "balance" | "plan")}>
                <Select.Trigger style={{ width: "100%" }} />
                <Select.Content>
                  <Select.Item value="balance">余额兑换码</Select.Item>
                  <Select.Item value="plan">套餐兑换码</Select.Item>
                </Select.Content>
              </Select.Root>
            </Flex>
            {kind === "balance" ? (
              <Flex direction="column" gap="1">
                <Text as="label" size="2" weight="medium">面值（积分）</Text>
                <ClearableTextField value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="100" style={{ width: "100%" }} />
              </Flex>
            ) : (
              <Flex direction="column" gap="1">
                <Text as="div" size="2" weight="medium">套餐</Text>
                <Select.Root value={planId} onValueChange={setPlanId}>
                  <Select.Trigger placeholder="选择套餐" style={{ width: "100%" }} />
                  <Select.Content>
                    {(plans.data ?? []).filter((p: any) => p.status === "active").map((p: any) => (
                      <Select.Item key={p.id} value={p.id}>{p.name}（{p.price} 积分）</Select.Item>
                    ))}
                  </Select.Content>
                </Select.Root>
              </Flex>
            )}
            <Flex direction="column" gap="1">
              <Text as="label" size="2" weight="medium">数量</Text>
              <ClearableTextField value={count} onChange={(e) => setCount(e.target.value)} style={{ width: "100%" }} />
            </Flex>
            {m.isError && <Text size="2" color="red">生成失败: {errMsg(m.error)}</Text>}
            <Flex gap="2" justify="end" mt="1">
              <Button variant="soft" color="gray" onClick={() => { reset(); onClose(); }}>取消</Button>
              <Button disabled={(kind === "balance" ? !amount : !planId) || m.isPending} onClick={() => m.mutate()}>
                生成
              </Button>
            </Flex>
          </Flex>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}
