import "../../personal-responsive.css";
import { useState } from "react";
import { AlertDialog, Box, Button, Code, Dialog, Flex, Select, Skeleton, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { meApi, type AccessTokenView } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { copyText } from "../../lib/copy";
import { ClearableTextField } from "../../components/ClearableTextField";
import { formatDateTime, formatRelative } from "../../lib/formatTime";
import { PageHeader } from "../../components/PageHeader";

const expiries = [["30", "keys.accessTokens.expiry30"], ["90", "keys.accessTokens.expiry90"], ["365", "keys.accessTokens.expiry365"], ["0", "keys.accessTokens.never"]] as const;

export default function AccessTokens() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const { data, isLoading, isError, error } = useQuery({ queryKey: ["me", "access-tokens"], queryFn: meApi.accessTokens });
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<string | null>(null);
  const revokeMut = useMutation({
    mutationFn: (id: string) => meApi.revokeAccessToken(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["me", "access-tokens"] }),
  });
  const tokens = data ?? [];

  return (
    <Box>
      <PageHeader
        title={t("keys.accessTokens.title")}
        description={t("keys.accessTokens.desc")}
        actions={<Button onClick={() => setCreating(true)}>{t("keys.accessTokens.create")}</Button>}
      />

      <div className="jc-object-list">
        {isLoading && Array.from({ length: 2 }, (_, i) => (
          <div key={i} className="jc-object-skeleton-row">
            <Skeleton width="26%" height="16px" style={{ borderRadius: 4 }} />
            <Skeleton width="48%" height="12px" style={{ borderRadius: 4 }} />
          </div>
        ))}
        {tokens.map((tok) => (
          <TokenRow key={tok.id} token={tok} revoking={revokeMut.isPending && revokeMut.variables === tok.id} onRevoke={() => revokeMut.mutate(tok.id)} />
        ))}
        {isError && <div className="jc-object-error">{errMsg(error)}</div>}
        {!isLoading && !isError && tokens.length === 0 && (
          <div className="jc-object-empty">
            <h3>{t("keys.accessTokens.empty")}</h3>
            <p>{t("keys.accessTokens.emptyBefore")}<code>Authorization: Bearer jcp-…</code>{t("keys.accessTokens.emptyAfter")}</p>
          </div>
        )}
        {revokeMut.isError && <div className="jc-object-error">{errMsg(revokeMut.error)}</div>}
      </div>

      {creating && <CreateDialog onClose={() => setCreating(false)} onCreated={(token) => {
        setCreating(false);
        setCreated(token);
        qc.invalidateQueries({ queryKey: ["me", "access-tokens"] });
      }} />}
      {created && <CreatedDialog token={created} onClose={() => setCreated(null)} />}
    </Box>
  );
}

function CreateDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (token: string) => void }) {
  const { t } = useTranslation();
  const [name, setName] = useState("");
  const [days, setDays] = useState("90");
  const create = useMutation({
    mutationFn: () => meApi.createAccessToken({ name: name.trim(), expires_in_days: Number(days) }),
    onSuccess: (r) => onCreated(r.token),
  });
  return <Dialog.Root open onOpenChange={(open) => { if (!open) onClose(); }}>
    <Dialog.Content maxWidth="440px">
      <Dialog.Title>{t("keys.accessTokens.createTitle")}</Dialog.Title>
      <Flex direction="column" gap="3">
        <ClearableTextField placeholder={t("keys.accessTokens.namePlaceholder")} value={name} onChange={(e) => setName(e.target.value)} />
        <Select.Root value={days} onValueChange={setDays}>
          <Select.Trigger aria-label={t("keys.accessTokens.validity")} />
          <Select.Content>{expiries.map(([v, label]) => <Select.Item key={v} value={v}>{t("keys.accessTokens.validityOption", { label: t(label) })}</Select.Item>)}</Select.Content>
        </Select.Root>
        {create.isError && <Text size="2" color="red" role="alert">{errMsg(create.error)}</Text>}
        <Flex gap="2" justify="end">
          <Button variant="soft" color="gray" onClick={onClose}>{t("keys.shared.cancel")}</Button>
          <Button disabled={!name.trim()} loading={create.isPending} onClick={() => create.mutate()}>{t("keys.shared.create")}</Button>
        </Flex>
      </Flex>
    </Dialog.Content>
  </Dialog.Root>;
}

// An AlertDialog, so a stray outside click or Esc cannot discard the only
// copy of the token; it closes through the button alone.
function CreatedDialog({ token, onClose }: { token: string; onClose: () => void }) {
  const { t } = useTranslation();
  const [copy, setCopy] = useState<"idle" | "done" | "failed">("idle");
  return <AlertDialog.Root open>
    <AlertDialog.Content maxWidth="520px">
      <AlertDialog.Title>{t("keys.accessTokens.createdTitle")}</AlertDialog.Title>
      <AlertDialog.Description size="2" color="gray">{t("keys.accessTokens.createdDesc")}</AlertDialog.Description>
      <Code size="2" style={{ display: "block", wordBreak: "break-all", padding: 12, marginTop: 12 }}>{token}</Code>
      {copy === "failed" && <Text size="2" color="red" role="alert" as="p" mt="2">{t("keys.accessTokens.copyFailed")}</Text>}
      <Flex gap="2" justify="end" mt="4">
        <Button variant="soft" onClick={() => { void copyText(token).then((ok) => setCopy(ok ? "done" : "failed")); }}>{copy === "done" ? t("keys.shared.copied") : t("keys.shared.copy")}</Button>
        <Button onClick={onClose}>{t("keys.shared.done")}</Button>
      </Flex>
    </AlertDialog.Content>
  </AlertDialog.Root>;
}

function isExpired(expiresAt?: string | null): boolean {
  return expiresAt != null && new Date(expiresAt).getTime() <= Date.now();
}

function TokenRow({ token, revoking, onRevoke }: { token: AccessTokenView; revoking: boolean; onRevoke: () => void }) {
  const { t } = useTranslation();
  const expired = isExpired(token.expires_at);
  return (
    <div className="jc-object-row">
      <div className="jc-object-main">
        <div className="jc-object-title">
          <strong>{token.name}</strong>
          <span className="jc-fact-chip jc-mono">{token.prefix}…</span>
          {expired && <span className="jc-fact-chip" data-tone="danger">{t("keys.shared.expired")}</span>}
        </div>
        <div className="jc-object-sub">
          <span>{token.last_used_at ? t("keys.shared.lastUsed", { when: formatRelative(token.last_used_at) }) : t("keys.shared.neverUsed")}</span>
          <span title={formatDateTime(token.created_at, "full")}>{t("keys.accessTokens.createdOn", { date: formatDateTime(token.created_at, "date") })}</span>
          <span>{token.expires_at ? t(expired ? "keys.accessTokens.expiredOn" : "keys.accessTokens.expiresOn", { date: formatDateTime(token.expires_at, "date") }) : t("keys.accessTokens.never")}</span>
        </div>
      </div>
      <div className="jc-object-actions">
        <AlertDialog.Root>
          <AlertDialog.Trigger>
            <Button size="1" variant="soft" color="red" disabled={revoking}>{t("keys.shared.revoke")}</Button>
          </AlertDialog.Trigger>
          <AlertDialog.Content maxWidth="440px">
            <AlertDialog.Title>{t("keys.accessTokens.revokeTitle", { name: token.name })}</AlertDialog.Title>
            <AlertDialog.Description size="2" color="gray">{t("keys.accessTokens.revokeDesc")}</AlertDialog.Description>
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
