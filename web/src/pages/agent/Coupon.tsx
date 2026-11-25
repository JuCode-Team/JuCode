import "../../admin-responsive.css";
import { useEffect, useState } from "react";
import { Badge, Box, Button, Card, Flex, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Link2 } from "lucide-react";

import { agentApi } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { AdminMetricCard, AdminMetricGrid } from "../../components/AdminPage";
import { ClearableTextField } from "../../components/ClearableTextField";
import { copyText } from "../../lib/copy";
import { PageHeader } from "../../components/PageHeader";

// The agent's reusable coupon. The agent splits their incentive budget
// between the buyer discount and their own rebate; rebates land in the
// sales ledger and settle together with redeem-code sales.
export default function AgentCoupon() {
  const qc = useQueryClient();
  const coupon = useQuery({ queryKey: ["agent", "coupon"], queryFn: agentApi.coupon });
  const [customCode, setCustomCode] = useState("");
  const [discount, setDiscount] = useState("");
  useEffect(() => {
    if (coupon.data) setDiscount(coupon.data.discount_pct);
  }, [coupon.data]);
  const refresh = () => qc.invalidateQueries({ queryKey: ["agent", "coupon"] });
  const create = useMutation({
    mutationFn: () => agentApi.createCoupon(customCode.trim() ? `C-${customCode.trim().toUpperCase().replace(/^C-/, "")}` : undefined),
    onSuccess: () => { setCustomCode(""); refresh(); },
  });
  const saveDiscount = useMutation({ mutationFn: () => agentApi.setCouponDiscount(discount.trim()), onSuccess: refresh });

  const data = coupon.data;
  if (!data) return null;
  const code = data.coupon_code;
  const purchaseURL = code ? `${window.location.origin}/me/plans?coupon=${encodeURIComponent(code)}` : "";
  const budget = Number(data.budget_pct);
  const draft = Number(discount);
  const draftValid = discount.trim() !== "" && draft >= 0 && draft <= budget;
  const draftRebate = draftValid ? (budget - draft).toFixed(2).replace(/\.?0+$/, "") : "—";

  return (
    <Box>
      <PageHeader title="专属优惠码" description="用户下单时填写优惠码享受折扣，你获得返利，返利与兑换码销售一起结算" />

      <Card className="pn-card-flat" mb="4">
        <Text size="2" as="div" weight="bold" mb="1">规则调整</Text>
        <Text size="2" color="gray" as="div">
          2026 年 10 月起，让利额度调整为 {data.budget_pct}%，客户折扣与你的返利合计不超过该比例；原折扣超过额度的，按额度执行。
          已开通专属分组的用户不能使用优惠码。请按新额度重新分配客户折扣与返利。
        </Text>
      </Card>

      <AdminMetricGrid>
        <AdminMetricCard label="使用优惠码的用户" value={data.buyers.toLocaleString()} detail="已付款的去重用户" tone="iris" />
        <AdminMetricCard label="让利额度" value={`${data.budget_pct}%`} detail="客户折扣 + 你的返利，由平台设置" tone="blue" />
        <AdminMetricCard label="客户折扣" value={`${data.discount_pct}%`} detail="余额充值和套餐均可使用" tone="amber" />
        <AdminMetricCard label="你的返利" value={`${data.rebate_rate_pct}%`} detail="按订单实付金额计算" tone="green" />
      </AdminMetricGrid>

      <Card className="pn-card-flat" mb="4">
        <Flex direction="column" gap="3">
          <Flex justify="between" align="center" gap="3" wrap="wrap">
            <Box>
              <Text size="2" weight="bold" as="div">优惠码</Text>
              <Text size="1" color="gray">创建后长期有效，每位代理一个。</Text>
            </Box>
            <Badge color={data.status === "active" ? "green" : "amber"} variant="soft">
              {data.status === "active" ? "启用" : "已停用"}
            </Badge>
          </Flex>
          {code ? (
            <Flex align="center" gap="2" wrap="wrap">
              <Text size="5" weight="bold" className="jc-mono">{code}</Text>
              <Button size="1" variant="soft" onClick={() => copyText(code)}><Copy size={14} />复制码</Button>
              <Button size="1" variant="soft" onClick={() => copyText(purchaseURL)}><Link2 size={14} />复制购买链接</Button>
            </Flex>
          ) : (
            <Flex align="end" gap="2" wrap="wrap">
              <Box style={{ minWidth: 220, flex: 1 }}>
                <Text size="1" color="gray" as="div" mb="1">自定义后缀（选填）</Text>
                <ClearableTextField value={customCode} onChange={(e) => setCustomCode(e.target.value)} placeholder="例如 ALICE；留空则自动生成" />
              </Box>
              <Button loading={create.isPending} disabled={data.status !== "active"} onClick={() => create.mutate()}>
                {data.status === "active" ? "创建优惠码" : "代理已停用"}
              </Button>
            </Flex>
          )}
          {create.isError && <Text size="2" color="red">创建失败：{errMsg(create.error)}</Text>}
        </Flex>
      </Card>

      <Card className="pn-card-flat">
        <Flex direction="column" gap="3">
          <Box>
            <Text size="2" weight="bold" as="div">分配让利额度</Text>
            <Text size="1" color="gray">客户折扣可设 0–{data.budget_pct}%，剩余部分为你的返利。修改只影响之后的新订单。已开通专属分组的用户不能使用优惠码。</Text>
          </Box>
          <Flex align="end" gap="2" wrap="wrap">
            <Box style={{ width: 160 }}>
              <Text size="1" color="gray" as="div" mb="1">客户折扣（%）</Text>
              <ClearableTextField type="number" value={discount} onChange={(e) => setDiscount(e.target.value)} />
            </Box>
            <Text size="2" color="gray" mb="2">你的返利 {draftRebate}%</Text>
            <Button loading={saveDiscount.isPending} disabled={!draftValid || discount.trim() === data.discount_pct} onClick={() => saveDiscount.mutate()}>
              保存
            </Button>
          </Flex>
          {saveDiscount.isError && <Text size="2" color="red">保存失败：{errMsg(saveDiscount.error)}</Text>}
        </Flex>
      </Card>
    </Box>
  );
}
