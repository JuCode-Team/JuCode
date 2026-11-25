// EndpointTab: 端点与协议 — the endpoint, the wire-protocol declaration, and
// every dialect knob. Fields are grouped by what they speak to: OpenAI 兼容
// (api_format, organization) and Messages 方言 (the Anthropic shaping rules)
// only appear when the declared native protocols can use them.
import { Badge, Flex, Select, Text, TextArea } from "@radix-ui/themes";
import type { AccountProtocol } from "../../../api/endpoints";
import { ClearableTextField } from "../../ClearableTextField";
import { protocolLabel } from "../AccountHealthBadges";
import {
  API_FORMATS, DEFAULT_ENDPOINT_PATH, HEADER_RULES_PLACEHOLDER, PATH_PROTOCOL_OPTIONS, PROTOCOL_OPTIONS, PROTOCOL_SHORT_LABEL,
  type AccountForm, type SetFormField,
} from "../AccountFormFields";
import { Field, FieldGrid, FormSection, ToggleRow } from "../../form/FormLayout";

export function EndpointTab({ form, set }: { form: AccountForm; set: SetFormField }) {
  // Dialect decides which knobs exist at all: api_format and organization are
  // OpenAI-compatible concepts, while cache-write accounting and the
  // official-client rule are Anthropic ones.
  const openAICompatible = form.native_protocols.some((p) => p === "chat_completions" || p === "responses");
  // A web tool account names its vendor in api_format.
  const webTools = form.native_protocols.includes("web");
  const anthropicDialect = form.native_protocols.includes("messages");

  const toggleProtocol = (p: AccountProtocol, on: boolean) => {
    const next = on ? [...form.native_protocols, p] : form.native_protocols.filter((x) => x !== p);
    const unique = Array.from(new Set(next));
    set("native_protocols", unique);
    if (!unique.includes(form.default_protocol as AccountProtocol)) set("default_protocol", "");
  };

  return (
    <>
      <FormSection title="端点" description="名称只在管理端可见；基础地址是发往上游的根 URL。">
        <FieldGrid>
          <Field label="名称" required>
            <ClearableTextField value={form.name} onChange={(e) => set("name", e.target.value)} placeholder="例如 主账号 / team-a" />
          </Field>
          <Field label="基础地址" code="base_url" required>
            <ClearableTextField
              value={form.base_url}
              onChange={(e) => set("base_url", e.target.value)}
              placeholder="https://api.example.com"
              className="jc-mono"
            />
          </Field>
        </FieldGrid>
      </FormSection>

      <FormSection title="协议声明" description="端点本身支持的线协议。多选时可指定方言转换的目标。">
        <Field label="原生协议" required>
          <div className="jc-check-rows">
            {PROTOCOL_OPTIONS.map((o) => (
              <label key={o.value} data-testid={`protocol-${o.value}`} className="jc-check-row">
                <input
                  type="checkbox"
                  aria-label={protocolLabel(o.value)}
                  checked={form.native_protocols.includes(o.value)}
                  onChange={(e) => toggleProtocol(o.value, e.target.checked)}
                />
                <span>{o.label}</span>
              </label>
            ))}
          </div>
        </Field>

        {form.native_protocols.length > 1 && (
          <FieldGrid>
            <Field label="默认协议" code="default_protocol" hint="方言转换的目标协议。">
              <Select.Root
                value={form.default_protocol || "_first"}
                onValueChange={(v) => set("default_protocol", v === "_first" ? "" : (v as AccountProtocol))}
              >
                <Select.Trigger />
                <Select.Content>
                  <Select.Item value="_first">自动（第一个原生协议）</Select.Item>
                  {form.native_protocols.map((p) => (
                    <Select.Item key={p} value={p}>{protocolLabel(p)}</Select.Item>
                  ))}
                </Select.Content>
              </Select.Root>
            </Field>
          </FieldGrid>
        )}

        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.allow_conversion}
            onCheckedChange={(v) => set("allow_conversion", v)}
            label="允许方言转换"
            hint={<>客户端协议不在原生集合内时，改写请求体打给该端点（还需站点级 <code>format_conversion_enabled</code>）。</>}
          />
        </div>
      </FormSection>

      <FormSection
        title="上游路径覆盖"
        description={<>放在基础地址之后的路径。留空即用网关默认（占位符所示）；不在 /v1/* 下的自建厂商（例如对话接口是 <code>/v2/chat/completions</code>）在这里声明。</>}
      >
        <FieldGrid columns={3}>
          {PATH_PROTOCOL_OPTIONS.map((o) => (
            <Field key={o.value} label={protocolLabel(o.value)} code={PROTOCOL_SHORT_LABEL[o.value]}>
              <ClearableTextField
                value={form.endpoint_paths[o.value] ?? ""}
                onChange={(e) => set("endpoint_paths", { ...form.endpoint_paths, [o.value]: e.target.value })}
                placeholder={DEFAULT_ENDPOINT_PATH[o.value]}
                className="jc-mono"
              />
            </Field>
          ))}
        </FieldGrid>
        <Field
          label="上游请求头规则"
          code="header_rules"
          hint={<>
            JSON，按 remove → drop_values → set 的顺序改写发往上游的请求头。<code>set</code> 的值可用 <code>{"{api_key}"}</code> 引用账号凭据，
            用于网关要求的鉴权头（如 <code>api-key</code>、<code>Ocp-Apim-Subscription-Key</code>）；<code>drop_values</code> 从逗号分隔的头里剔除指定值，
            末尾 <code>*</code> 按前缀匹配。留空即原样发送。
          </>}
        >
          <TextArea
            value={form.header_rules}
            onChange={(e) => set("header_rules", e.target.value)}
            placeholder={HEADER_RULES_PLACEHOLDER}
            rows={5}
            className="jc-mono"
          />
        </Field>
      </FormSection>

      {(openAICompatible || webTools) && (
        <FormSection title="生成端点方言" description="Chat Completions / Responses 账号的生成接口方言；网页工具账号在这里选厂商（parallel、brave、jina、firecrawl）。">
          <FieldGrid>
            <Field label="生成端点方言" code="api_format" hint="留空按基础地址推断。">
              <Select.Root
                value={form.api_format || "_auto"}
                onValueChange={(v) => set("api_format", v === "_auto" ? "" : v)}
              >
                <Select.Trigger />
                <Select.Content>
                  <Select.Item value="_auto">自动（按基础地址推断）</Select.Item>
                  {API_FORMATS.map((f) => <Select.Item key={f} value={f}>{f}</Select.Item>)}
                </Select.Content>
              </Select.Root>
            </Field>
            <Field label="上游组织 ID" code="organization" hint="留空表示不使用。">
              <ClearableTextField
                value={form.organization}
                onChange={(e) => set("organization", e.target.value)}
                placeholder="org-…"
              />
            </Field>
          </FieldGrid>
          <div className="jc-toggle-rows">
            <ToggleRow
              checked={form.block_image_generation_tool}
              onCheckedChange={(v) => set("block_image_generation_tool", v)}
              label="拦截 GPT 图像生成工具"
              hint={<>仅拦截发往该账号的 <code>image_generation</code> tool definition，请求里的普通图片内容仍会照常透传。</>}
            />
          </div>
        </FormSection>
      )}

      {anthropicDialect && (
        <FormSection title="Messages 方言" description="只对 Anthropic Messages 协议生效。">
          <div className="jc-toggle-rows">
            <ToggleRow
              checked={form.estimate_cache_write}
              onCheckedChange={(v) => set("estimate_cache_write", v)}
              label="估算缓存写入 tokens"
              hint={<>上游不返回缓存写入用量时，网关把未命中缓存的输入重新归类为缓存写入（日志行标记 <code>cache_write_estimated</code>）。</>}
            />
          </div>
          <FieldGrid>
            <Field label="缓存写入最小上下文" code="cache_write_min_context_tokens" hint="单位 tokens，0 不限制。低于该输入侧上下文总量时不估算缓存写入，按普通输入计费。">
              <ClearableTextField
                type="number"
                value={form.cache_write_min_context_tokens}
                onChange={(e) => set("cache_write_min_context_tokens", e.target.value)}
              />
            </Field>
          </FieldGrid>
        </FormSection>
      )}

      {form.native_protocols.length > 0 && (
        <Flex gap="1" wrap="wrap" mt="4">
          <Text size="1" color="gray">当前声明：</Text>
          {form.native_protocols.map((p) => (
            <Badge key={p} variant="soft" color="gray">{protocolLabel(p)}</Badge>
          ))}
        </Flex>
      )}
    </>
  );
}
