// AccountDetailDialog: the account editor — a tabbed dialog.
//
// 概览 carries the live state (health, quota); 端点与协议 groups every
// dialect knob by the dialect that owns it; 凭据 rotates the secret; 调度
// holds the scheduling policy; 模型 edits the model
// bindings. All tabs feed one AccountForm and one PATCH — the tab split is
// visual, not a set of separate saves.
//
// Layout follows the admin form spec: fixed header (title, state badges,
// tabs), scrolling body, sticky footer with the form error above the actions.
//
// Creation is the wizard's job (it carries catalog templates and provider
// definitions); this dialog only edits.
//
// Admin-only. Account identity never leaves this surface.
import { useState } from "react";
import { Badge, Button, Flex, Tabs, Text } from "@radix-ui/themes";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  adminApi, type Account, type AccountUpdate,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import {
  StatusBadge, accountLabel, healthColor, useNow,
} from "./AccountHealthBadges";
import { formFromAccount, parseHeaderRules, type AccountForm } from "./AccountFormFields";
import { CancelButton, FormDialog } from "../form/FormLayout";
import { OverviewTab } from "./detail/OverviewTab";
import { EndpointTab } from "./detail/EndpointTab";
import { CredentialTab } from "./detail/CredentialTab";
import { ScheduleTab } from "./detail/ScheduleTab";
import { ModelsTab } from "./detail/ModelsTab";
import { UsageTab } from "./detail/UsageTab";

export function AccountDetailDialog({
  account,
  open,
  onOpenChange,
  onSaved,
}: {
  account: Account | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved?: (account: Account) => void;
}) {
  // Mount only for a real account: the inner dialog seeds its form in the
  // useState initializer, so a fresh mount per account replaces a reset effect.
  if (!account) return null;
  return <DetailDialog account={account} open={open} onOpenChange={onOpenChange} onSaved={onSaved} />;
}

type TabId = "overview" | "endpoint" | "credential" | "schedule" | "models" | "usage";

function DetailDialog({
  account,
  open,
  onOpenChange,
  onSaved,
}: {
  account: Account;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved?: (account: Account) => void;
}) {
  const qc = useQueryClient();
  const [form, setForm] = useState<AccountForm>(() => formFromAccount(account));
  const [error, setError] = useState("");
  const [tab, setTab] = useState<TabId>("overview");
  const now = useNow(open);

  const set = <K extends keyof AccountForm>(key: K, value: AccountForm[K]) =>
    setForm((prev) => ({ ...prev, [key]: value }));

  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
    void qc.invalidateQueries({ queryKey: ["admin", "account", account.id] });
  };

  const save = useMutation({
    mutationFn: () => {
      const body = updateFromForm(form);
      // Only the fields changed in this dialog go out. A full-form PATCH
      // replays every value as it was when the dialog opened, silently
      // reverting whatever another admin (or another tab) saved since.
      const initial = updateFromForm(formFromAccount(account));
      const changed = Object.fromEntries(
        Object.entries(body).filter(([key, value]) => JSON.stringify(value) !== JSON.stringify(initial[key as keyof AccountUpdate])),
      ) as AccountUpdate;
      if (form.api_key.trim()) changed.api_key = form.api_key.trim();
      if (Object.keys(changed).length === 0) return Promise.resolve(account);
      return adminApi.updateAccount(account.id, changed);
    },
    onSuccess: (saved) => {
      invalidate();
      onSaved?.(saved);
      onOpenChange(false);
    },
  });

  const submit = () => {
    if (!form.name.trim()) { setError("请填写名称"); setTab("endpoint"); return; }
    if (!form.base_url.trim()) { setError("请填写基础地址（base_url）"); setTab("endpoint"); return; }
    if (form.native_protocols.length === 0) { setError("至少选择一个原生协议"); setTab("endpoint"); return; }
    setError("");
    save.mutate();
  };

  // 概览 / 模型 / 用量 are live views with their own actions; the save bar
  // only means something on the three form tabs.
  const formTab = tab === "endpoint" || tab === "credential" || tab === "schedule";

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      size="lg"
      title={<>账号详情 <Text as="span" weight="regular" color="gray">{accountLabel(account)}</Text></>}
      description="账号是服务端基础设施：客户端看不到账号，也无法指定使用哪个账号。这里配置端点、方言、凭据与调度策略。"
      header={
        <>
          <Flex gap="2" align="center" wrap="wrap" mt="3">
            <StatusBadge account={account} />
            <Badge color={healthColor(account.health)} variant="soft">{account.health}</Badge>
            <Badge color={account.schedulable_now ? "green" : "gray"} variant="soft">
              {account.schedulable_now ? "可调度" : "不可调度"}
            </Badge>
            {account.definition_id && <Badge variant="soft" color="iris">定义 {account.definition_id}</Badge>}
          </Flex>
          <Tabs.Root value={tab} onValueChange={(v) => setTab(v as TabId)}>
            <Tabs.List>
              <Tabs.Trigger value="overview">概览</Tabs.Trigger>
              <Tabs.Trigger value="endpoint">端点与协议</Tabs.Trigger>
              <Tabs.Trigger value="credential">凭据</Tabs.Trigger>
              <Tabs.Trigger value="schedule">调度</Tabs.Trigger>
              <Tabs.Trigger value="models">模型</Tabs.Trigger>
              <Tabs.Trigger value="usage">用量</Tabs.Trigger>
            </Tabs.List>
          </Tabs.Root>
        </>
      }
      error={error || (save.isError ? errMsg(save.error) : undefined)}
      footerLeft={formTab ? <Text size="1" color="gray">端点、凭据、调度三个标签页的修改一起保存。</Text> : undefined}
      footer={
        <>
          <CancelButton>{formTab ? "取消" : "关闭"}</CancelButton>
          {formTab && <Button loading={save.isPending} onClick={submit}>保存</Button>}
        </>
      }
    >
      {tab === "overview" && <OverviewTab account={account} now={now} invalidate={invalidate} />}
      {tab === "endpoint" && <EndpointTab form={form} set={set} />}
      {tab === "credential" && <CredentialTab form={form} set={set} />}
      {tab === "schedule" && <ScheduleTab account={account} form={form} set={set} />}
      {tab === "models" && <ModelsTab accountId={account.id} />}
      {tab === "usage" && <UsageTab accountId={account.id} />}
    </FormDialog>
  );
}

// updateFromForm maps the editor form onto the PATCH shape. Endpoint, dialect
// and request-shaping fields are patchable, so every tab's fields share it.
function updateFromForm(form: AccountForm): AccountUpdate {
  const priority = parseInt(form.priority);
  const weight = parseInt(form.weight);
  const maxConcurrency = parseInt(form.max_concurrency);
  const cacheWriteMinContextTokens = parseInt(form.cache_write_min_context_tokens);
  if (!Number.isFinite(priority) || !Number.isFinite(weight) || !Number.isFinite(maxConcurrency)) {
    throw new Error("优先级 / 权重 / 并发必须是数字");
  }
  if (!Number.isFinite(cacheWriteMinContextTokens) || cacheWriteMinContextTokens < 0) {
    throw new Error("缓存写入最小上下文必须是 0 或正整数");
  }
  return {
    name: form.name.trim(),
    priority,
    weight,
    max_concurrency: maxConcurrency,
    note: form.note,
    status: form.enabled ? "active" : "disabled",
    pool_mode: form.pool_mode,
    base_url: form.base_url.trim(),
    api_format: form.api_format.trim(),
    native_protocols: form.native_protocols,
    default_protocol: form.default_protocol || "",
    allow_conversion: form.allow_conversion,
    // Whole-map replacement: only the protocols that still carry a path are
    // sent, so clearing an input removes that override.
    endpoint_paths: Object.fromEntries(
      Object.entries(form.endpoint_paths).filter(([, path]) => (path ?? "").trim() !== ""),
    ),
    header_rules: parseHeaderRules(form.header_rules),
    organization: form.organization.trim(),
    block_image_generation_tool: form.block_image_generation_tool,
    estimate_cache_write: form.estimate_cache_write,
    cache_write_min_context_tokens: cacheWriteMinContextTokens,
  };
}
