import type { ReactNode } from "react";
import { AlertDialog, Button, Flex } from "@radix-ui/themes";
import { useTranslation } from "react-i18next";

// ConfirmDialog replaces window.confirm for reversible or low-impact
// actions. Irreversible actions that affect users should use the
// type-the-name confirmation instead (see AdminPlans delete).
export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  color = "red",
  loading,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  description?: ReactNode;
  confirmLabel?: string;
  color?: "red" | "orange" | "green" | "gray";
  loading?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  return (
    <AlertDialog.Root open={open} onOpenChange={(o) => { if (!o) onCancel(); }}>
      <AlertDialog.Content maxWidth="420px">
        <AlertDialog.Title>{title}</AlertDialog.Title>
        {description && (
          <AlertDialog.Description size="2" color="gray">{description}</AlertDialog.Description>
        )}
        <Flex gap="2" justify="end" mt="4">
          <AlertDialog.Cancel>
            <Button variant="soft" color="gray" disabled={loading}>{t("keys.shared.cancel")}</Button>
          </AlertDialog.Cancel>
          <Button color={color} loading={loading} onClick={onConfirm}>{confirmLabel ?? t("keys.shared.confirm")}</Button>
        </Flex>
      </AlertDialog.Content>
    </AlertDialog.Root>
  );
}
