// HeldBalance: the "预扣 X ⓘ" marker shown beside a balance. Reserved
// amounts are already taken out of the balance the API returns, so without
// this the balance appears to drop by more than a request costs.
import { Tooltip } from "@radix-ui/themes";
import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";
import { PointAmount } from "./PointAmount";

export function HeldBalance({ amount, iconSize = 12 }: { amount: string | undefined; iconSize?: number }) {
  const { t } = useTranslation();
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 4 }}>
      {t("billing.held.label")} <PointAmount amount={amount ?? "0"} iconSize={iconSize} />
      <Tooltip content={t("billing.held.hint")}>
        <Info size={13} aria-label={t("billing.held.hintAria")} tabIndex={0} style={{ color: "var(--gray-9)", cursor: "help", flexShrink: 0 }} />
      </Tooltip>
    </span>
  );
}
