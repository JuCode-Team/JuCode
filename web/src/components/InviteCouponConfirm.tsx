import { AlertDialog, Button, Flex } from "@radix-ui/themes";
import { useTranslation } from "react-i18next";
import type { InviteCouponConflict } from "../api/endpoints";
import i18n from "../i18n";

export function hasInviteConflict(c?: InviteCouponConflict | null): boolean {
  return !!c && (c.topup_rebate || c.signup_reward);
}

function conflictText(c: InviteCouponConflict): string {
  const lost = [
    c.topup_rebate && i18n.t("billing.inviteCoupon.lostRebate"),
    c.signup_reward && i18n.t("billing.inviteCoupon.lostSignup"),
  ].filter(Boolean).join(i18n.t("billing.inviteCoupon.separator"));
  return i18n.t("billing.inviteCoupon.body", { lost });
}

// Shown before creating a balance order that carries a coupon while the
// buyer's inviter still has payouts riding on their top-ups.
export function InviteCouponConfirm({
  conflict,
  open,
  loading,
  onUseCoupon,
  onDropCoupon,
  onCancel,
}: {
  conflict: InviteCouponConflict | null;
  open: boolean;
  loading?: boolean;
  onUseCoupon: () => void;
  onDropCoupon: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  return (
    <AlertDialog.Root open={open} onOpenChange={(o) => { if (!o) onCancel(); }}>
      <AlertDialog.Content maxWidth="440px">
        <AlertDialog.Title>{t("billing.inviteCoupon.title")}</AlertDialog.Title>
        {conflict && <AlertDialog.Description size="2" color="gray">{conflictText(conflict)}</AlertDialog.Description>}
        <Flex gap="2" justify="end" mt="4" wrap="wrap">
          <AlertDialog.Cancel>
            <Button variant="soft" color="gray" disabled={loading}>{t("billing.inviteCoupon.cancel")}</Button>
          </AlertDialog.Cancel>
          <Button variant="soft" disabled={loading} onClick={onDropCoupon}>{t("billing.inviteCoupon.dropCoupon")}</Button>
          <Button loading={loading} onClick={onUseCoupon}>{t("billing.inviteCoupon.useCoupon")}</Button>
        </Flex>
      </AlertDialog.Content>
    </AlertDialog.Root>
  );
}
