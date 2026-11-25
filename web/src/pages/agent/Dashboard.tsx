import { useState } from "react";
import { Badge, Box, Button, Card, Flex, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { agentApi, type AgentDashboard as Dashboard } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../../components/ClearableTextField";
import { PointAmount } from "../../components/PointAmount";
import { PageHeader } from "../../components/PageHeader";

// Agent landing page: pending settlement summary + share rate + status.
// All amounts are platform points (1 积分 = 1 元).
export default function AgentDashboard() {
  const { data } = useQuery({ queryKey: ["agent", "dashboard"], queryFn: agentApi.dashboard });
  if (!data) return null;
  return (
    <Box>
      <PageHeader
        title="代理看板"
        description="本周期待结算与销售概况"
      />
      <Box className="jc-agent-dashboard-grid">
        <StatCard label="平台分成" value={`${data.profile.platform_share_pct}%`} hint={<Badge size="1" color={data.profile.status === "active" ? "green" : "amber"}>{data.profile.status === "active" ? "在线" : "停用"}</Badge>} />
        <StatCard label="待结算净额" value={<PointAmount amount={data.pending.total_payable} iconSize={18} />} hint={`${data.pending.entries_count} 条流水`} accent />
        <StatCard label="销售总额" value={<PointAmount amount={data.pending.sales} iconSize={18} />} hint="本周期累计" />
        <StatCard label="退款总额" value={<PointAmount amount={data.pending.refunds} iconSize={18} />} hint="本周期累计" />
      </Box>
      <WithdrawCard data={data} />
      <Box mt="4">
        <Card className="pn-card-flat jc-note-panel">
          <Text size="2" weight="medium" as="div" mb="2">结算说明</Text>
          <Text size="2" color="gray" as="div">
            每笔被用户兑换的代理兑换码都会写入一条 <b>卡密销售</b> 流水；登记退款会写入一条 <b>卡密退款</b> 流水
            (按该笔销售时的平台分成扣减)。
            用户使用你的优惠码付款后写入一条 <b>优惠码返利</b> 流水（本周期累计 <PointAmount amount={data.pending.coupon_rebates} iconSize={12} />），
            与卡密应付相抵。平台应付你的净额可以随时提现到平台余额，用于调用平台服务，提现会记一条 <b>提现到余额</b> 流水。管理员点击 “结算” 后，所有未结算流水会被打包成一笔结算单；之后产生的流水进入下一周期。
          </Text>
        </Card>
      </Box>
    </Box>
  );
}

// Moves owed commission into the agent's own balance. The limit is the
// pending net the platform owes (card-code sales the agent owes net first).
function WithdrawCard({ data }: { data: Dashboard }) {
  const qc = useQueryClient();
  const [amount, setAmount] = useState("");
  const available = Number(data.pending.withdrawable);
  const draft = Number(amount);
  const valid = amount.trim() !== "" && draft > 0 && draft <= available && /^\d+(\.\d{1,2})?$/.test(amount.trim());
  const withdraw = useMutation({
    mutationFn: () => agentApi.withdraw(amount.trim()),
    onSuccess: () => {
      setAmount("");
      qc.invalidateQueries({ queryKey: ["agent"] });
      qc.invalidateQueries({ queryKey: ["me"] });
    },
  });
  const active = data.profile.status === "active";
  return (
    <Card className="pn-card-flat" mt="4">
      <Flex direction="column" gap="3">
        <Box>
          <Text size="2" weight="bold" as="div">提现到平台余额</Text>
          <Text size="1" color="gray">
            可提现 <PointAmount amount={data.pending.withdrawable} iconSize={12} />，即时到账，可用于调用平台服务。
            卡密销售应付平台的部分会先抵扣返利。
          </Text>
        </Box>
        <Flex align="end" gap="2" wrap="wrap">
          <Box style={{ width: 180 }}>
            <Text size="1" color="gray" as="div" mb="1">提现金额</Text>
            <ClearableTextField type="number" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="0.00" />
          </Box>
          <Button variant="soft" disabled={!active || available <= 0} onClick={() => setAmount(data.pending.withdrawable)}>全部</Button>
          <Button loading={withdraw.isPending} disabled={!active || !valid} onClick={() => withdraw.mutate()}>
            {active ? "提现" : "代理已停用"}
          </Button>
        </Flex>
        {withdraw.isSuccess && <Text size="2" color="green">已提现 {withdraw.data.amount}，当前余额 <PointAmount amount={withdraw.data.balance_after} iconSize={12} /></Text>}
        {withdraw.isError && <Text size="2" color="red">提现失败：{errMsg(withdraw.error)}</Text>}
      </Flex>
    </Card>
  );
}

function StatCard({ label, value, hint, accent }: { label: string; value: React.ReactNode; hint?: React.ReactNode; accent?: boolean }) {
  return (
    <Card className="pn-card-flat jc-agent-dashboard-card jc-admin-metric" data-tone={accent ? "iris" : "gray"}>
      <Text size="1" color="gray">{label}</Text>
      <Text size="6" weight="bold" as="div" mt="1" className="jc-agent-dashboard-value">{value}</Text>
      {hint && <Box mt="2">{typeof hint === "string" ? <Text size="1" color="gray">{hint}</Text> : hint}</Box>}
    </Card>
  );
}
