import { Text } from "@radix-ui/themes";
import { useTranslation } from "react-i18next";
import type { OrderRefundable } from "../api/endpoints";
import { formatAmount } from "../lib/currency";

// 退款弹窗里的可退金额说明：本单可退多少，以及受账户可退余额限制的原因。
export function OrderRefundableHint({ data, loading }: { data?: OrderRefundable; loading?: boolean }) {
  const { t } = useTranslation();
  if (loading) return <Text size="2" color="gray">{t("billing.refundable.loading")}</Text>;
  if (!data) return null;
  if (!data.balance_bearing) {
    return <Text size="2" color="gray">{t("billing.refundable.periodPlan")}</Text>;
  }
  return (
    <Text size="2" as="div">
      {t("billing.refundable.orderRefundable")} <b>{formatAmount(data.cash)}</b>
      <Text color="gray">
        {t("billing.refundable.accountRefundable", { amount: formatAmount(data.refundable) })}
        {t("billing.refundable.consumed")}
      </Text>
    </Text>
  );
}
