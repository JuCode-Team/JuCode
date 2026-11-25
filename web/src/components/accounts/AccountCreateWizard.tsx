// AccountCreateWizard: the only "add an account" path.
//
// Step 0 picks the account source, and the source decides how much the rest of
// the wizard asks:
//   - 提供商定义 (Azure, …): the vendor ships endpoint, protocols and models —
//     the wizard skips the endpoint step and goes straight to its credential
//     field.
//   - 目录模板: the catalog fills the endpoint; the operator confirms it and
//     pastes the API key. Only API-key templates are offered.
//   - 自定义端点: hand-written endpoint — the step-1 form also exposes the
//     advanced overrides (organization, endpoint_paths) that used to live in
//     the manual dialog.
import { useEffect, useMemo, useState } from "react";
import { Badge, Box, Button, Callout, Code, Flex, Select, Tabs, Text, TextField } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Check, Info, Search } from "lucide-react";
import {
  adminApi, type Account, type AccountProtocol, type ProviderCatalogEntry,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { showToast } from "../../lib/appToast";
import { ClearableTextField } from "../ClearableTextField";
import { protocolLabel } from "./AccountHealthBadges";
import {
  DEFAULT_ENDPOINT_PATH, PATH_PROTOCOL_OPTIONS, PROTOCOL_OPTIONS, PROTOCOL_SHORT_LABEL,
} from "./AccountFormFields";
import { CancelButton, Field, FieldGrid, FormDialog, FormSection, ToggleRow } from "../form/FormLayout";

type Step = 0 | 1 | 2 | 3 | 4;
// Step 0 shows one source at a time; the segmented control in the header picks it.
type Source = "definition" | "catalog" | "custom";
const STEP_LABELS = ["选择来源", "端点与协议", "凭据", "模型选择", "完成"];

interface WizardForm {
  name: string;
  base_url: string;
  api_format: string;
  organization: string;
  native_protocols: AccountProtocol[];
  default_protocol: AccountProtocol | "";
  allow_conversion: boolean;
  // 上游路径覆盖：方言 -> 路径。留空即用网关默认（/v1/...）。
  endpoint_paths: Partial<Record<AccountProtocol, string>>;
  api_key: string;
}

const emptyForm: WizardForm = {
  name: "",
  base_url: "",
  api_format: "",
  organization: "",
  native_protocols: ["chat_completions"],
  default_protocol: "",
  allow_conversion: false,
  endpoint_paths: {},
  api_key: "",
};

function loginKindLabel(kind: string): string {
  return kind === "custom" ? "自定义登录（需手工配置）" : "API Key";
}

// Catalog templates the console can create: API-key logins only.
function isAPIKeyEntry(entry: ProviderCatalogEntry): boolean {
  return entry.login_kind === "" || entry.login_kind === "api-key" || entry.login_kind === "custom";
}

function hasURLPlaceholder(url: string): boolean {
  return /\{[^}]*\}/.test(url);
}

// The catalog speaks its own protocol vocabulary ("chat"/"responses"/
// "anthropic"/"azure"); the account API wants the native one.
function catalogToAccountProtocol(protocol?: string): AccountProtocol {
  switch (protocol) {
    case "anthropic": return "messages";
    case "responses":
    case "azure": return "responses";
    default: return "chat_completions";
  }
}

export function AccountCreateWizard({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const qc = useQueryClient();
  const [step, setStep] = useState<Step>(0);
  const [catalogId, setCatalogId] = useState("");
  const [search, setSearch] = useState("");
  const [form, setForm] = useState<WizardForm>(emptyForm);
  const [created, setCreated] = useState<Account | null>(null);
  const [selectedModels, setSelectedModels] = useState<Set<string>>(new Set());
  const [modelsSaved, setModelsSaved] = useState<number | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!open) return;
    setStep(0);
    setCatalogId("");
    setSearch("");
    setForm(emptyForm);
    setCreated(null);
    setSelectedModels(new Set());
    setModelsSaved(null);
    setError("");
  }, [open]);

  const catalog = useQuery({
    queryKey: ["admin", "provider-catalog"],
    queryFn: () => adminApi.providerCatalog(),
    enabled: open,
    staleTime: 5 * 60_000,
  });
  const entry = useMemo(
    () => catalog.data?.providers.find((p) => p.id === catalogId),
    [catalog.data, catalogId],
  );

  // The template's model list backs the model-binding step.
  const detail = useQuery({
    queryKey: ["admin", "provider-catalog-entry", catalogId],
    queryFn: () => adminApi.providerCatalogEntry(catalogId),
    enabled: open && !!catalogId,
    staleTime: 5 * 60_000,
  });

  const searchTerm = search.trim().toLowerCase();
  const groups = useMemo(() => {
    const all = (catalog.data?.providers ?? []).filter((p) => p.usable && isAPIKeyEntry(p));
    return searchTerm
      ? all.filter((p) => `${p.name} ${p.id} ${p.default_model ?? ""}`.toLowerCase().includes(searchTerm))
      : all;
  }, [catalog.data, searchTerm]);

  // Provider definitions are the first-class source: a definition carries the
  // vendor's own endpoint shape, protocols and models.
  const definitions = useQuery({
    queryKey: ["provider-definitions"],
    queryFn: () => adminApi.providerDefinitions(),
    enabled: open,
    staleTime: 60_000,
  });
  const [definitionId, setDefinitionId] = useState("");
  const [source, setSource] = useState<Source>("catalog");
  // Definitions are the first-class source when the deployment has any.
  useEffect(() => {
    if (!open) return;
    setSource((definitions.data ?? []).length > 0 ? "definition" : "catalog");
  }, [open, definitions.data]);

  const pickDefinition = (id: string) => {
    setDefinitionId(id);
    setError("");
    const picked = definitions.data?.find((d) => d.definition_id === id);
    if (!picked) {
      setDefinitionId("");
      return;
    }
    const protocols = (picked.native_protocols ?? []).filter(Boolean) as AccountProtocol[];
    setForm((prev) => ({
      ...prev,
      name: prev.name || picked.name,
      base_url: picked.base_url,
      api_format: picked.api_format ?? (picked.default_protocol === "messages" ? "" : picked.api_format ?? ""),
      organization: "",
      native_protocols: protocols.length ? protocols : prev.native_protocols,
      default_protocol: (picked.default_protocol as AccountProtocol) ?? "",
      endpoint_paths: { ...(picked.endpoint_paths ?? {}) },
    }));
  };

  // Which fields matter is decided by what was picked: a definition already
  // fixes the dialect, so the endpoint fields stay read-only; a catalog or
  // manual account only shows the knobs its dialect actually has.
  const definition = useMemo(
    () => (definitions.data ?? []).find((d) => d.definition_id === definitionId) ?? null,
    [definitions.data, definitionId],
  );
  // A definition whose base_url carries a placeholder (Azure's {resource}) is
  // per-tenant: the endpoint step must stay open so the operator fills it in.
  const baseURLTemplated = !!definition && hasURLPlaceholder(definition.base_url);
  const credentialLabel = definition?.credential_label || "API Key";
  const credentialHint = definition?.credential_hint || "一个账号就是一份凭据。要挂多把 Key（独立冷却、优先级/权重、粘性绑定）时，重复这个流程即可。";
  const openAICompatible = form.native_protocols.some((p) => p === "chat_completions" || p === "responses");
  // A web tool account names its vendor in api_format.
  const showAPIFormat = openAICompatible || form.native_protocols.includes("web");

  const pickTemplate = (id: string) => {
    setCatalogId(id);
    setDefinitionId("");
    setError("");
    const picked = catalog.data?.providers.find((p) => p.id === id);
    if (!picked) {
      setForm((prev) => ({ ...prev, name: "", base_url: "", api_format: "", organization: "", native_protocols: ["chat_completions"], default_protocol: "", endpoint_paths: {} }));
      return;
    }
    const native = catalogToAccountProtocol(picked.protocol);
    setForm((prev) => ({
      ...prev,
      name: picked.name,
      base_url: picked.base_url,
      api_format: picked.protocol === "anthropic" ? "" : picked.protocol,
      organization: "",
      native_protocols: [native],
      default_protocol: "",
      endpoint_paths: {},
    }));
  };

  const accountBody = () => ({
    name: form.name.trim(),
    definition_id: definitionId || undefined,
    base_url: form.base_url.trim(),
    api_format: form.api_format.trim() || undefined,
    organization: form.organization.trim() || undefined,
    native_protocols: form.native_protocols,
    default_protocol: form.default_protocol || undefined,
    endpoint_paths: nonEmptyPaths(form.endpoint_paths),
    allow_conversion: form.allow_conversion,
    api_key: form.api_key.trim() || undefined,
  });

  // A definition ships the models its accounts should serve, so preselect them
  // instead of making the operator find each one again.
  useEffect(() => {
    if (!definition || selectedModels.size > 0) return;
    const names = (definition.models ?? []).map((m) => m.name).filter(Boolean);
    if (names.length) setSelectedModels(new Set(names));
  }, [definition, selectedModels.size]);

  // Default the model selection to the template's curated default model.
  useEffect(() => {
    if (!detail.data) return;
    const preferred = detail.data.provider.default_model;
    const next = new Set<string>();
    if (preferred && detail.data.models.some((m) => m.id === preferred && m.supported)) next.add(preferred);
    setSelectedModels(next);
  }, [detail.data]);

  const submitBasics = () => {
    if (!form.name.trim()) { setError("请填写名称"); return; }
    if (!form.base_url.trim()) { setError("请填写 base_url"); return; }
    if (hasURLPlaceholder(form.base_url)) { setError("base_url 里还有 {…} 占位，请换成实际值"); return; }
    if (form.native_protocols.length === 0) { setError("至少选择一个原生协议"); return; }
    setError("");
    setStep(2);
  };

  const submitLogin = () => {
    if (!form.api_key.trim()) { setError(`请填写${credentialLabel}`); return; }
    setError("");
    setStep(3);
  };

  // finalize creates the account and saves the selected model bindings.
  const finalize = useMutation({
    mutationFn: async () => {
      let acct = created;
      if (!acct) {
        if (!form.api_key.trim()) throw new Error("请填写 API Key");
        acct = await adminApi.createAccount(accountBody());
      }
      const ids = Array.from(selectedModels);
      const results = await Promise.allSettled(
        ids.map((model) => adminApi.addAccountModel(acct!.id, { model_name: model, cost_mode: "none" })),
      );
      return {
        account: acct,
        ok: results.filter((r) => r.status === "fulfilled").length,
        failed: results.filter((r) => r.status === "rejected").length,
      };
    },
    onSuccess: ({ account, ok, failed }) => {
      setCreated(account);
      setModelsSaved(ok);
      setStep(4);
      void qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
      if (failed > 0) {
        showToast({ title: "部分模型未保存", message: `成功 ${ok} 个，失败 ${failed} 个` });
      }
    },
  });

  const finish = () => {
    onOpenChange(false);
    void qc.invalidateQueries({ queryKey: ["admin", "accounts"] });
  };

  const supportedModels = (detail.data?.models ?? []).filter((m) => m.supported);

  // Model choices come from whichever source was picked: a catalog template
  // (with the gateway's supported dialects) or a provider definition (which
  // ships the models its accounts should serve).
  const modelChoices = useMemo(() => {
    if (catalogId) {
      return supportedModels.map((m) => ({ id: m.id, label: m.id, reasoning: m.reasoning, context: m.context_window, max: m.max_tokens }));
    }
    return (definition?.models ?? []).map((m) => ({ id: m.name, label: m.name, reasoning: false, context: 0, max: 0 }));
  }, [catalogId, supportedModels, definition]);

  // Steps already passed can be revisited until the account exists.
  const canGoTo = (target: Step) => target < step && !created && step < 4;
  const goTo = (target: Step) => { if (canGoTo(target)) { setError(""); setStep(target); } };

  const stepDescription = [
    "选择账号来源：提供商定义、目录模板，或自定义端点。",
    "确认端点地址与协议声明。模板已经填好类型与基础地址。",
    "填写这个账号的 API Key。",
    "勾选这个账号要提供哪些模型。",
    "账号已创建，可以在账号列表里继续管理。",
  ][step];

  const stepHeader = (
    <div className="jc-steps" aria-label="步骤">
      {STEP_LABELS.map((label, index) => {
        const state = index < step ? "done" : index === step ? "current" : "todo";
        const clickable = canGoTo(index as Step);
        return (
          <Flex align="center" gap="1" key={label}>
            <button
              type="button"
              className="jc-step"
              data-state={state}
              disabled={!clickable}
              aria-current={index === step ? "step" : undefined}
              onClick={() => goTo(index as Step)}
              style={clickable ? undefined : { cursor: "default" }}
            >
              <span className="jc-step-dot">{index < step ? <Check size={11} /> : index + 1}</span>
              <span className="jc-step-label">{label}</span>
            </button>
            {index < STEP_LABELS.length - 1 && <span className="jc-step-sep" />}
          </Flex>
        );
      })}
      <span className="jc-steps-count">第 {step + 1} 步 / 共 {STEP_LABELS.length}</span>
    </div>
  );

  const sourceTabs = step === 0 && (
    <Tabs.Root value={source} onValueChange={(v) => setSource(v as Source)}>
      <Tabs.List>
        <Tabs.Trigger value="definition" disabled={(definitions.data ?? []).length === 0}>
          提供商定义{definitions.data?.length ? ` ${definitions.data.length}` : ""}
        </Tabs.Trigger>
        <Tabs.Trigger value="catalog">目录模板{catalog.data ? ` ${groups.length}` : ""}</Tabs.Trigger>
        <Tabs.Trigger value="custom">自定义端点</Tabs.Trigger>
      </Tabs.List>
    </Tabs.Root>
  );

  const canLeaveStep0 = source === "custom" || (source === "catalog" && !!catalogId) || (source === "definition" && !!definitionId);
  const leaveStep0 = () => {
    if (source === "custom") { setDefinitionId(""); pickTemplate(""); setStep(1); return; }
    if (source === "catalog" && catalogId) { setStep(1); return; }
    if (source === "definition" && definitionId) { setStep(hasURLPlaceholder(form.base_url) ? 1 : 2); }
  };

  const showFooterBack = step === 1 || step === 2 || (step === 3 && !created);
  const footerRight = (
    <>
      {step === 0 && <><CancelButton /><Button onClick={leaveStep0} disabled={!canLeaveStep0}>下一步</Button></>}
      {step === 1 && <><CancelButton /><Button onClick={submitBasics}>下一步</Button></>}
      {step === 2 && <><CancelButton /><Button onClick={submitLogin}>下一步</Button></>}
      {step === 3 && (
        <Button loading={finalize.isPending} onClick={() => finalize.mutate()}>
          {created ? "保存模型绑定并完成" : "创建账号并完成"}
        </Button>
      )}
      {step === 4 && <Button onClick={finish}>完成</Button>}
    </>
  );
  const footerLeft = showFooterBack ? (
    <Button variant="soft" color="gray" onClick={() => goTo((step - 1) as Step)}>上一步</Button>
  ) : undefined;

  const formError = error || (step === 3 && finalize.isError ? errMsg(finalize.error) : "");

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      size={step === 0 || step === 3 ? "lg" : "md"}
      title="新建账号"
      description={stepDescription}
      header={<>{stepHeader}{sourceTabs}</>}
      footer={footerRight}
      footerLeft={footerLeft}
      error={formError || undefined}
    >
        {step === 0 && source === "definition" && (
          <div className="jc-choice-list">
            <Text size="1" color="gray" as="p" mb="1">厂商自带端点、协议与模型的定义。选择后直接填写凭据。</Text>
            {(definitions.data ?? []).map((d) => (
              <button
                key={d.definition_id}
                type="button"
                className="jc-choice"
                data-selected={definitionId === d.definition_id}
                onClick={() => {
                  pickDefinition(d.definition_id);
                  setStep(hasURLPlaceholder(d.base_url) ? 1 : 2);
                }}
              >
                <div className="jc-choice-main">
                  <div className="jc-choice-title">
                    {d.name}
                    <Badge size="1" variant="soft" color="iris">定义</Badge>
                  </div>
                  <div className="jc-choice-sub">
                    {(d.models ?? []).length ? `${(d.models ?? []).length} 个预置模型` : "无预置模型"}
                  </div>
                </div>
                {definitionId === d.definition_id && <Check size={16} className="jc-choice-check" />}
              </button>
            ))}
          </div>
        )}

        {step === 0 && source === "catalog" && (
          <>
            <ClearableTextField
              placeholder="搜索厂商 / 模型"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              style={{ maxWidth: 360 }}
              mb="3"
            >
              <TextField.Slot><Search size={14} /></TextField.Slot>
            </ClearableTextField>

            {catalog.isLoading && <Text size="2" color="gray">正在加载目录…</Text>}
            {catalog.isError && <Callout.Root color="red"><Callout.Text>{errMsg(catalog.error)}</Callout.Text></Callout.Root>}

            {groups.length > 0 && (
              <div className="jc-choice-list">
                {groups.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    className="jc-choice"
                    data-selected={catalogId === p.id}
                    onClick={() => pickTemplate(p.id)}
                    onDoubleClick={() => { pickTemplate(p.id); setStep(1); }}
                  >
                    <div className="jc-choice-main">
                      <div className="jc-choice-title">{p.name}</div>
                      <div className="jc-choice-sub jc-mono-text">{p.id} · {p.base_url}</div>
                    </div>
                    <div className="jc-choice-meta">
                      {p.protocol && <Badge variant="soft" color="gray">{protocolLabel(catalogToAccountProtocol(p.protocol))}</Badge>}
                      <Badge variant="soft" color="gray">{p.model_count} 模型</Badge>
                    </div>
                    {catalogId === p.id && <Check size={16} className="jc-choice-check" />}
                  </button>
                ))}
              </div>
            )}

            {!catalog.isLoading && groups.length === 0 && (
              <Text size="2" color="gray">没有匹配的模板。</Text>
            )}
          </>
        )}

        {step === 0 && source === "custom" && (
          <Flex direction="column" gap="3">
            <Text size="2" as="p">
              手写基础地址、协议与凭据，不从目录取模板。适用于兼容端点、自建网关和不在目录里的厂商。
            </Text>
            <Text size="2" color="gray" as="p">
              高级字段（上游路径覆盖、组织 ID）在下一步填写；请求头规则与方言塑形在创建后的账号详情里配置。
            </Text>
          </Flex>
        )}

        {step === 1 && (
          <div className="jc-form-stack">
            {definition && (
              <Callout.Root size="1" color="green" variant="surface">
                <Callout.Icon><Check size={14} /></Callout.Icon>
                <Callout.Text>
                  {baseURLTemplated
                    ? `协议与模型由定义「${definition.name}」提供；把基础地址里的 {…} 占位换成你的实际值。`
                    : `端点、协议与模型都由定义「${definition.name}」提供，这一步不需要改。`}
                </Callout.Text>
              </Callout.Root>
            )}
            {entry && !definition && (
              <Callout.Root size="1" color="iris" variant="surface">
                <Callout.Icon><Info size={14} /></Callout.Icon>
                <Callout.Text>
                  模板 {entry.id}：{protocolLabel(catalogToAccountProtocol(entry.protocol))} · {entry.base_url}
                  {entry.default_model ? ` · 默认模型 ${entry.default_model}` : ""} · {loginKindLabel(entry.login_kind)}
                </Callout.Text>
              </Callout.Root>
            )}

            <FormSection title="端点" description="账号的名称只在管理端可见；基础地址是发往上游的根 URL。">
              <FieldGrid>
                <Field label="名称" hint="留空时按模板或定义的名称生成。">
                  <ClearableTextField value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="例如 主账号 / team-a" />
                </Field>
                {(!definition || baseURLTemplated) ? (
                  <Field label="基础地址" code="base_url" required>
                    <ClearableTextField
                      value={form.base_url}
                      onChange={(e) => setForm({ ...form, base_url: e.target.value })}
                      placeholder="https://api.example.com"
                      className="jc-mono"
                    />
                  </Field>
                ) : (
                  <Field label="基础地址" code="base_url">
                    <Code size="2">{form.base_url}</Code>
                  </Field>
                )}
              </FieldGrid>
            </FormSection>

            {definition ? (
              <FormSection title="协议" description="由定义固定，不在这里修改。">
                <FieldGrid>
                  <Field label="原生协议">
                    <Flex gap="1" wrap="wrap">
                      {form.native_protocols.map((p) => <Badge key={p} variant="soft">{protocolLabel(p)}</Badge>)}
                    </Flex>
                  </Field>
                  <Field label="上游路径覆盖">
                    <Flex gap="1" wrap="wrap">
                      {Object.entries(definition.endpoint_paths ?? {}).map(([protocol, path]) => (
                        <Badge key={protocol} variant="soft" color="gray">{protocol} → {path}</Badge>
                      ))}
                      {!Object.keys(definition.endpoint_paths ?? {}).length && (
                        <Text size="1" color="gray">使用网关默认路径</Text>
                      )}
                    </Flex>
                  </Field>
                </FieldGrid>
                <Box mt="3">
                  <Button size="1" variant="soft" color="gray" onClick={() => setDefinitionId("")}>
                    改用自定义端点
                  </Button>
                </Box>
              </FormSection>
            ) : (
              <>
                <FormSection title="协议声明" description="端点本身支持的线协议。多选时可指定转换目标。">
                  <Field label="原生协议" required>
                    <div className="jc-check-rows">
                      {PROTOCOL_OPTIONS.map((o) => (
                        <label key={o.value} className="jc-check-row">
                          <input
                            type="checkbox"
                            aria-label={protocolLabel(o.value)}
                            checked={form.native_protocols.includes(o.value)}
                            onChange={(e) => setForm((prev) => {
                              const next = e.target.checked
                                ? Array.from(new Set([...prev.native_protocols, o.value]))
                                : prev.native_protocols.filter((x) => x !== o.value);
                              return { ...prev, native_protocols: next, default_protocol: next.includes(prev.default_protocol as AccountProtocol) ? prev.default_protocol : "" };
                            })}
                          />
                          <span>{o.label}</span>
                        </label>
                      ))}
                    </div>
                  </Field>
                  {(showAPIFormat || form.native_protocols.length > 1) && (
                    <FieldGrid>
                      {showAPIFormat && (
                        <Field label="生成端点方言" code="api_format" hint="留空按基础地址推断；网页工具账号填厂商名（parallel、brave、jina、firecrawl）。">
                          <ClearableTextField
                            value={form.api_format}
                            onChange={(e) => setForm({ ...form, api_format: e.target.value })}
                            placeholder="openai"
                          />
                        </Field>
                      )}
                      {openAICompatible && (
                        <Field label="上游组织 ID" code="organization" hint="留空表示不使用。">
                          <ClearableTextField
                            value={form.organization}
                            onChange={(e) => setForm({ ...form, organization: e.target.value })}
                            placeholder="org-…"
                          />
                        </Field>
                      )}
                      {form.native_protocols.length > 1 && (
                        <Field label="默认协议" code="default_protocol" hint="方言转换的目标协议。">
                          <Select.Root
                            value={form.default_protocol || "_first"}
                            onValueChange={(v) => setForm({ ...form, default_protocol: v === "_first" ? "" : (v as AccountProtocol) })}
                          >
                            <Select.Trigger />
                            <Select.Content>
                              <Select.Item value="_first">自动（第一个原生协议）</Select.Item>
                              {form.native_protocols.map((p) => <Select.Item key={p} value={p}>{protocolLabel(p)}</Select.Item>)}
                            </Select.Content>
                          </Select.Root>
                        </Field>
                      )}
                    </FieldGrid>
                  )}
                </FormSection>

                <FormSection title="上游路径覆盖" description="放在基础地址之后的路径。留空即用网关默认（占位符所示）；不在 /v1/* 下的自建厂商在这里声明。">
                  <FieldGrid columns={3}>
                    {PATH_PROTOCOL_OPTIONS.map((o) => (
                      <Field key={o.value} label={protocolLabel(o.value)} code={PROTOCOL_SHORT_LABEL[o.value]}>
                        <ClearableTextField
                          value={form.endpoint_paths[o.value] ?? ""}
                          onChange={(e) => setForm({ ...form, endpoint_paths: { ...form.endpoint_paths, [o.value]: e.target.value } })}
                          placeholder={DEFAULT_ENDPOINT_PATH[o.value]}
                          className="jc-mono"
                        />
                      </Field>
                    ))}
                  </FieldGrid>
                  {form.native_protocols.length < 3 && (
                    <div className="jc-toggle-rows">
                      <ToggleRow
                        checked={form.allow_conversion}
                        onCheckedChange={(v) => setForm({ ...form, allow_conversion: v })}
                        label="允许方言转换"
                        hint="客户端协议不在原生集合内时改写请求体；仍受站点级开关约束。"
                      />
                    </div>
                  )}
                </FormSection>
              </>
            )}
          </div>
        )}

        {step === 2 && (
          <div className="jc-form-stack">
            <Field label={credentialLabel} required hint={credentialHint}>
              <ClearableTextField
                type="password"
                value={form.api_key}
                onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                placeholder={definition ? "粘贴令牌" : "sk-..."}
                className="jc-mono"
              />
            </Field>
          </div>
        )}

        {step === 3 && (
          <div className="jc-form-stack">
            <Callout.Root size="1" color="gray" variant="surface">
              <Callout.Icon><Info size={14} /></Callout.Icon>
              <Callout.Text>点击「{created ? "保存模型绑定并完成" : "创建账号并完成"}」会创建账号并保存下面的模型绑定。</Callout.Text>
            </Callout.Root>

            {modelChoices.length > 0 ? (
              <FormSection
                title={`模型绑定 · 已选 ${selectedModels.size} / ${modelChoices.length}`}
                description={catalogId ? "勾选这个账号要提供的模型（仅列出本网关支持的方言）。" : "定义预置的模型，可增删；更多模型创建后在账号详情里添加。"}
                actions={
                  <>
                    <Button size="1" variant="soft" color="gray" onClick={() => setSelectedModels(new Set(modelChoices.map((m) => m.id)))}>全选</Button>
                    <Button size="1" variant="soft" color="gray" onClick={() => setSelectedModels(new Set())}>清空</Button>
                  </>
                }
              >
                {catalogId && detail.isLoading && <Text size="2" color="gray">正在加载模板模型…</Text>}
                {catalogId && detail.isError && <Callout.Root color="red"><Callout.Text>{errMsg(detail.error)}</Callout.Text></Callout.Root>}
                <div className="jc-choice-list">
                  {modelChoices.map((m) => {
                    const on = selectedModels.has(m.id);
                    return (
                      <label key={m.id} className="jc-choice" data-selected={on} style={{ padding: "8px 12px" }}>
                        <input
                          type="checkbox"
                          aria-label={m.id}
                          checked={on}
                          onChange={(e) => {
                            const next = new Set(selectedModels);
                            if (e.target.checked) next.add(m.id);
                            else next.delete(m.id);
                            setSelectedModels(next);
                            setModelsSaved(null);
                          }}
                          style={{ width: 16, height: 16, margin: 0, accentColor: "var(--c-primary)" }}
                        />
                        <div className="jc-choice-main">
                          <div className="jc-choice-title" style={{ fontFamily: "var(--font-mono)", fontWeight: 400 }}>
                            {m.id}
                            {m.reasoning && <Badge size="1" variant="soft" color="iris">reasoning</Badge>}
                          </div>
                        </div>
                        <div className="jc-choice-meta">
                          <Text size="1" color="gray">
                            {m.context ? `${Math.round(m.context / 1000)}k 上下文` : ""}
                            {m.max ? ` · 最大输出 ${Math.round(m.max / 1000)}k` : ""}
                          </Text>
                        </div>
                      </label>
                    );
                  })}
                </div>
              </FormSection>
            ) : (
              <Text size="2" color="gray">自定义账号没有目录模型清单；创建后可在账号详情里逐个添加支持模型。</Text>
            )}

            {modelsSaved != null && <Text size="2" color="green">已保存 {modelsSaved} 个模型绑定</Text>}
          </div>
        )}

        {step === 4 && (
          <div className="jc-form-stack">
            <Callout.Root size="1" color="green" variant="surface">
              <Callout.Icon><Check size={14} /></Callout.Icon>
              <Callout.Text>账号「{created?.name}」已就绪{modelsSaved ? `，已绑定 ${modelsSaved} 个模型` : ""}。</Callout.Text>
            </Callout.Root>
            <Text size="2" color="gray" as="p">
              用户可见的模型与分组在 <Link to="/admin/models">模型</Link> / <Link to="/admin/groups">分组</Link> 页配置；
              账号、冷却与额度在 <Link to="/admin/accounts">账号</Link> 页管理。
            </Text>
          </div>
        )}
    </FormDialog>
  );
}

// nonEmptyPaths strips blank overrides so the body only declares protocols
// that actually keep a custom path.
function nonEmptyPaths(paths: Partial<Record<AccountProtocol, string>>): Partial<Record<AccountProtocol, string>> {
  return Object.fromEntries(Object.entries(paths).filter(([, path]) => (path ?? "").trim() !== ""));
}
