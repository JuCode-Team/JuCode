import "../../personal-responsive.css";
import { AlertDialog, Box, Button, Flex, Skeleton } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Monitor, Terminal, Globe } from "lucide-react";
import { useTranslation } from "react-i18next";
import i18n from "../../i18n";
import { meApi, type DeviceView } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import PremiumButton from "../../components/PremiumButton";
import { formatDateTime, formatRelative } from "../../lib/formatTime";
import { PageHeader } from "../../components/PageHeader";
import { docsUrl } from "../../lib/siblingSite";

function clientLabel(clientId: string): string {
  if (clientId === "jucode-desktop") return "Desktop";
  if (clientId === "jucode-cli") return "CLI";
  if (clientId === "jucode-web") return i18n.t("keys.devices.web");
  return clientId;
}

export default function Devices() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({ queryKey: ["me", "devices"], queryFn: meApi.devices });
  const revokeMut = useMutation({
    mutationFn: (id: string) => meApi.revokeDevice(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["me", "devices"] }),
  });
  const devices = data ?? [];

  return (
    <Box>
      <PageHeader
        title={t("keys.devices.title")}
        description={t("keys.devices.desc")}
      />

      <div className="jc-object-list">
        {isLoading && Array.from({ length: 2 }, (_, i) => (
          <div key={i} className="jc-object-skeleton-row">
            <Skeleton width="26%" height="16px" style={{ borderRadius: 4 }} />
            <Skeleton width="48%" height="12px" style={{ borderRadius: 4 }} />
          </div>
        ))}
        {devices.map((d) => (
          <DeviceRow
            key={d.id}
            device={d}
            revoking={revokeMut.isPending && revokeMut.variables === d.id}
            onRevoke={() => revokeMut.mutate(d.id)}
          />
        ))}
        {!isLoading && devices.length === 0 && (
          <div className="jc-object-empty">
            <h3>{t("keys.devices.empty")}</h3>
            <p>{t("keys.devices.emptyCopy")}</p>
            {docsUrl() && (
              <div className="jc-object-empty-actions">
                <PremiumButton href={docsUrl()} external variant="secondary" size="md">{t("keys.devices.loginDocs")}</PremiumButton>
              </div>
            )}
          </div>
        )}
        {revokeMut.isError && <div className="jc-object-error">{errMsg(revokeMut.error)}</div>}
      </div>
    </Box>
  );
}

// A device counts as "active" if it called the API within the last hour —
// enough to distinguish the machine you're on from stale grants.
function isRecentlyActive(lastUsedAt?: string): boolean {
  if (!lastUsedAt) return false;
  const t = new Date(lastUsedAt).getTime();
  return Number.isFinite(t) && Date.now() - t < 3600_000;
}

function DeviceRow({ device, revoking, onRevoke }: { device: DeviceView; revoking: boolean; onRevoke: () => void }) {
  const { t } = useTranslation();
  const Icon = device.client_id === "jucode-desktop" ? Monitor : device.client_id === "jucode-web" ? Globe : Terminal;
  const active = isRecentlyActive(device.last_used_at);
  const name = device.name || clientLabel(device.client_id);
  return (
    <div className="jc-object-row">
      <div className="jc-object-main">
        <div className="jc-object-title">
          <Icon size={16} strokeWidth={1.8} style={{ color: "var(--c-on-surface-variant)", flex: "none" }} />
          <strong>{name}</strong>
          <span className="jc-fact-chip">{clientLabel(device.client_id)}</span>
          {active && <span className="jc-fact-chip" data-tone="success">{t("keys.devices.active")}</span>}
        </div>
        <div className="jc-object-sub">
          <span title={device.last_used_at ? formatDateTime(device.last_used_at, "full") : undefined}>
            {device.last_used_at ? t("keys.shared.lastUsed", { when: formatRelative(device.last_used_at) }) : t("keys.shared.neverUsed")}
          </span>
          {device.last_ip && <span className="jc-mono">{device.last_ip}</span>}
          <span title={formatDateTime(device.created_at, "full")}>{t("keys.devices.authorizedOn", { date: formatDateTime(device.created_at, "date") })}</span>
          {device.last_user_agent && (
            <span title={device.last_user_agent} style={{ maxWidth: 360, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", display: "block" }}>
              {device.last_user_agent}
            </span>
          )}
        </div>
      </div>
      <div className="jc-object-actions">
        <AlertDialog.Root>
          <AlertDialog.Trigger>
            <Button size="1" variant="soft" color="red" disabled={revoking}>{t("keys.devices.revokeAccess")}</Button>
          </AlertDialog.Trigger>
          <AlertDialog.Content maxWidth="440px">
            <AlertDialog.Title>{t("keys.devices.revokeTitle", { name })}</AlertDialog.Title>
            <AlertDialog.Description size="2" color="gray">
              {t("keys.devices.revokeDesc")}
            </AlertDialog.Description>
            <Flex gap="2" justify="end" mt="4">
              <AlertDialog.Cancel><Button variant="soft" color="gray">{t("keys.shared.cancel")}</Button></AlertDialog.Cancel>
              <AlertDialog.Action><Button color="red" onClick={onRevoke}>{t("keys.shared.revoke")}</Button></AlertDialog.Action>
            </Flex>
          </AlertDialog.Content>
        </AlertDialog.Root>
      </div>
    </div>
  );
}
