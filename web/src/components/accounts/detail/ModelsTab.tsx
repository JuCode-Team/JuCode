// ModelsTab: 支持模型 — the account's model bindings. An account only serves
// the enabled models in this list; each binding carries its own cost basis for
// finance accounting. The add/edit dialog lives here too.
import { useEffect, useState } from "react";
import {
  Badge, Box, Button, Callout, Checkbox, Dialog, Flex, SegmentedControl, Select, Switch, Table, Text,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Info, Trash2 } from "lucide-react";
import {
  adminApi,
  type AccountModel, type AccountModelInput, type ImportAccountModelItem,
} from "../../../api/endpoints";
import { errMsg } from "../../../api/client";
import { displayPrice, toStorage } from "../../../lib/price";
import { ClearableTextField } from "../../ClearableTextField";
import { CatalogPickerDialog } from "../../CatalogPickerDialog";
import { Field } from "../AccountFormFields";

type CostMode = "none" | "per_request" | "per_usage";
const MODEL_CURRENCIES = ["CNY", "USD"] as const;
const UPSTREAM_PROTOCOLS = [
  { value: "", label: "继承账号" },
  { value: "chat_completions", label: "chat_completions" },
  { value: "responses", label: "responses" },
  { value: "messages", label: "messages" },
] as const;

function costModeLabel(mode: CostMode): string {
  return ({ none: "无固定成本", per_request: "按请求", per_usage: "按用量" } as const)[mode] ?? mode;
}

// Token counts as typed in the admin: "272000", "272k", "1m"; "" = 0 (unset).
function parseTokens(raw: string): number {
  const t = raw.trim().toLowerCase();
  if (t === "") return 0;
  const m = /^(\d+(?:\.\d+)?)([km]?)$/.exec(t);
  if (!m) return NaN;
  return Math.round(parseFloat(m[1]) * (m[2] === "k" ? 1_000 : m[2] === "m" ? 1_000_000 : 1));
}

const fmtTokens = (n: number) =>
  n >= 1_000_000 ? `${+(n / 1_000_000).toFixed(2)}M` : n >= 1000 ? `${Math.round(n / 1000)}K` : String(n);

export function ModelsTab({ accountId }: { accountId: string }) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [catalogOpen, setCatalogOpen] = useState(false);
  const [editing, setEditing] = useState<AccountModel | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const queryKey = ["admin", "account", accountId, "models"];
  const models = useQuery({ queryKey, queryFn: () => adminApi.accountModels(accountId) });
  const remove = useMutation({
    mutationFn: (id: string) => adminApi.removeAccountModel(accountId, id),
    onSuccess: () => qc.invalidateQueries({ queryKey }),
  });
  const sync = useMutation({
    mutationFn: () => adminApi.syncAccountModels(accountId),
    onSuccess: () => qc.invalidateQueries({ queryKey }),
  });
  const rows = models.data ?? [];
  useEffect(() => {
    const ids = new Set((models.data ?? []).map((m) => m.id));
    setSelected((prev) => {
      const next = new Set([...prev].filter((id) => ids.has(id)));
      return next.size === prev.size ? prev : next;
    });
  }, [models.data]);
  const bulkRemove = useMutation({
    mutationFn: async (ids: string[]) => {
      let failed = 0;
      for (let i = 0; i < ids.length; i += 20) {
        const res = await Promise.allSettled(
          ids.slice(i, i + 20).map((id) => adminApi.removeAccountModel(accountId, id)),
        );
        failed += res.filter((r) => r.status === "rejected").length;
      }
      return { total: ids.length, failed };
    },
    onSuccess: () => { qc.invalidateQueries({ queryKey }); setSelected(new Set()); },
  });
  const toggleAll = (v: boolean | "indeterminate") =>
    setSelected(v === true ? new Set(rows.map((m) => m.id)) : new Set());
  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });

  return (
    <Flex direction="column" gap="3">
      <Flex justify="between" align="center" gap="2" wrap="wrap">
        <Text size="1" color="gray">
          该账号只能接收列表内已启用模型；每个模型都必须选择成本模式。
        </Text>
        <Flex gap="2">
          {selected.size > 0 && (
            <Button
              size="1" color="red" variant="soft"
              loading={bulkRemove.isPending}
              onClick={() => {
                if (confirm(`删除所选 ${selected.size} 个模型绑定？`)) bulkRemove.mutate([...selected]);
              }}
            >
              删除所选（{selected.size}）
            </Button>
          )}
          <Button size="1" variant="soft" loading={sync.isPending} onClick={() => sync.mutate()}>同步模型</Button>
          <Button size="1" variant="soft" onClick={() => setCatalogOpen(true)}>从目录导入</Button>
          <Button size="1" variant="soft" onClick={() => { setEditing(null); setOpen(true); }}>新增模型</Button>
        </Flex>
      </Flex>
      {bulkRemove.isSuccess && (
        <Callout.Root color={bulkRemove.data.failed > 0 ? "amber" : "green"} variant="surface" size="1">
          <Callout.Icon><Info size={16} /></Callout.Icon>
          <Callout.Text>
            已删除 {bulkRemove.data.total - bulkRemove.data.failed} 个绑定
            {bulkRemove.data.failed > 0 && `，${bulkRemove.data.failed} 个删除失败`}。
          </Callout.Text>
        </Callout.Root>
      )}
      {sync.isSuccess && (
        <Callout.Root color="green" variant="surface" size="1">
          <Callout.Icon><Info size={16} /></Callout.Icon>
          <Callout.Text>
            上游返回 {sync.data.remote_count} 个模型：新增 {sync.data.added} 个，已存在 {sync.data.existing} 个
            {sync.data.updated > 0 && `，补齐 ${sync.data.updated} 个`}。
            {sync.data.added > 0 && ` 新增：${sync.data.added_models.join("、")}`}
            {(sync.data.unmatched_models?.length ?? 0) > 0 && (
              <>
                <br />
                {sync.data.unmatched_models!.length} 个上游 id 未匹配到平台模型，已跳过：
                {sync.data.unmatched_models!.slice(0, 10).join("、")}
                {sync.data.unmatched_models!.length > 10 && " 等"}
              </>
            )}
          </Callout.Text>
        </Callout.Root>
      )}
      {sync.isError && <Text size="2" color="red">{errMsg(sync.error)}</Text>}
      <div className="jc-local-table-scroll">
        <Table.Root variant="surface" size="1">
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell width="32px">
                <Checkbox
                  checked={rows.length > 0 && selected.size === rows.length ? true : selected.size > 0 ? "indeterminate" : false}
                  onCheckedChange={toggleAll}
                />
              </Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>模型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>上游模型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>上游协议</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">上下文</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>成本模式</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">输入/1M</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">输出/1M</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">cache read/1M</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">cache write 5m/1M</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">cache write 1h/1M</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>币种</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">参考价 in/out<br />USD/1M</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>启用</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">操作</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {rows.map((m) => (
              <Table.Row key={m.id}>
                <Table.Cell>
                  <Checkbox checked={selected.has(m.id)} onCheckedChange={() => toggle(m.id)} />
                </Table.Cell>
                <Table.Cell>{m.model_name}</Table.Cell>
                <Table.Cell>{m.upstream_model || "-"}</Table.Cell>
                <Table.Cell>{m.upstream_protocol || "继承"}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{m.context_window ? fmtTokens(m.context_window) : <Text color="gray">继承</Text>}</Table.Cell>
                <Table.Cell>{costModeLabel(m.cost_mode)}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{displayPrice(m.input_unit_cost, "M") || "-"}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{displayPrice(m.output_unit_cost, "M") || "-"}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{displayPrice(m.cache_read_input_cost, "M") || "-"}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{displayPrice(m.cache_write_input_cost, "M") || "-"}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{displayPrice(m.cache_write_1h_input_cost, "M") || "-"}</Table.Cell>
                <Table.Cell>{m.currency}</Table.Cell>
                <Table.Cell align="right" className="jc-num">
                  <Text color="gray">{m.ref_input_cost || m.ref_output_cost ? `${m.ref_input_cost || "-"} / ${m.ref_output_cost || "-"}` : "-"}</Text>
                </Table.Cell>
                <Table.Cell>
                  <Badge color={m.enabled ? "green" : "gray"} variant="soft" radius="full">{m.enabled ? "启用" : "停用"}</Badge>
                </Table.Cell>
                <Table.Cell align="right">
                  <Flex gap="1" justify="end">
                    <Button size="1" variant="soft" onClick={() => { setEditing(m); setOpen(true); }}>编辑</Button>
                    <Button
                      size="1" color="red" variant="soft"
                      loading={remove.isPending && remove.variables === m.id}
                      onClick={() => { if (confirm(`删除模型 ${m.model_name}？`)) remove.mutate(m.id); }}
                    >
                      <Trash2 size={13} />
                    </Button>
                  </Flex>
                </Table.Cell>
              </Table.Row>
            ))}
            {!models.isLoading && rows.length === 0 && (
              <Table.Row>
                <Table.Cell colSpan={14}>
                  <Text size="2" color="gray">还没有配置支持模型；该账号不会参与任何模型调用。</Text>
                </Table.Cell>
              </Table.Row>
            )}
          </Table.Body>
        </Table.Root>
      </div>

      <AccountModelDialog
        accountId={accountId}
        model={editing}
        open={open}
        onClose={() => { setOpen(false); setEditing(null); }}
        onSaved={() => qc.invalidateQueries({ queryKey })}
      />
      <CatalogImportDialog
        accountId={accountId}
        open={catalogOpen}
        onClose={() => setCatalogOpen(false)}
        onImported={() => qc.invalidateQueries({ queryKey })}
      />
    </Flex>
  );
}

function AccountModelDialog({
  accountId, model, open, onClose, onSaved,
}: {
  accountId: string;
  model: AccountModel | null;
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
}) {
  const platformModels = useQuery({ queryKey: ["admin", "models"], queryFn: adminApi.models, enabled: open });
  const [form, setForm] = useState({
    model_name: "",
    upstream_model: "",
    upstream_protocol: "",
    enabled: true,
    price_per_request_cost: "",
    input_unit_cost: "",
    output_unit_cost: "",
    cache_read_unit_cost: "",
    cache_write_unit_cost: "",
    cache_write_1h_unit_cost: "",
    ref_input_cost: "",
    ref_output_cost: "",
    currency: "CNY",
    context_window: "",
  });
  const [costMode, setCostMode] = useState<CostMode>("none");

  useEffect(() => {
    if (!open) return;
    setForm({
      model_name: model?.model_name ?? "",
      upstream_model: model?.upstream_model ?? "",
      upstream_protocol: model?.upstream_protocol ?? "",
      enabled: model?.enabled ?? true,
      price_per_request_cost: model?.price_per_request_cost ?? "",
      input_unit_cost: displayPrice(model?.input_unit_cost, "M"),
      output_unit_cost: displayPrice(model?.output_unit_cost, "M"),
      cache_read_unit_cost: displayPrice(model?.cache_read_input_cost, "M"),
      cache_write_unit_cost: displayPrice(model?.cache_write_input_cost, "M"),
      cache_write_1h_unit_cost: displayPrice(model?.cache_write_1h_input_cost, "M"),
      ref_input_cost: model?.ref_input_cost ?? "",
      ref_output_cost: model?.ref_output_cost ?? "",
      currency: model?.currency || "CNY",
      context_window: model?.context_window ? String(model.context_window) : "",
    });
    setCostMode(model?.cost_mode ?? "none");
  }, [model, open]);

  const set = (key: keyof typeof form, value: string | boolean) => setForm((prev) => ({ ...prev, [key]: value }));

  const save = useMutation({
    mutationFn: () => {
      const body: AccountModelInput = {
        model_name: form.model_name.trim(),
        upstream_model: form.upstream_model.trim(),
        upstream_protocol: form.upstream_protocol,
        enabled: form.enabled,
        cost_mode: costMode,
        currency: form.currency,
        // Always sent: 0 clears the override back to the model default.
        context_window: parseTokens(form.context_window),
      };
      if (form.ref_input_cost.trim()) body.ref_input_cost = form.ref_input_cost.trim();
      if (form.ref_output_cost.trim()) body.ref_output_cost = form.ref_output_cost.trim();
      if (costMode === "per_request") {
        body.price_per_request_cost = form.price_per_request_cost.trim();
      } else if (costMode === "per_usage") {
        body.input_unit_cost = toStorage(form.input_unit_cost, "M");
        body.output_unit_cost = toStorage(form.output_unit_cost, "M");
        body.cache_read_unit_cost = toStorage(form.cache_read_unit_cost, "M");
        body.cache_write_unit_cost = toStorage(form.cache_write_unit_cost, "M");
        body.cache_write_1h_unit_cost = toStorage(form.cache_write_1h_unit_cost, "M");
      }
      return model
        ? adminApi.updateAccountModel(accountId, model.id, body)
        : adminApi.addAccountModel(accountId, body);
    },
    onSuccess: () => { onSaved(); onClose(); },
  });

  const platformNames = (platformModels.data ?? []).map((m) => m.name);
  const modelWindow = platformModels.data?.find((m) => m.name === form.model_name)?.context_window;
  const windowInvalid = Number.isNaN(parseTokens(form.context_window));
  const canSave = form.model_name.trim() && !windowInvalid && (
    costMode === "none" ? true :
    costMode === "per_request" ? !!form.price_per_request_cost.trim() :
    !!form.input_unit_cost.trim() && !!form.output_unit_cost.trim()
  );

  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content style={{ maxWidth: 640 }}>
        <Dialog.Title>{model ? "编辑账号模型" : "新增账号模型"}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Field label="模型">
            <Select.Root value={form.model_name || "_none"} onValueChange={(v) => set("model_name", v === "_none" ? "" : v)}>
              <Select.Trigger placeholder="选择模型" />
              <Select.Content>
                <Select.Item value="_none">请选择模型</Select.Item>
                {platformNames.map((name) => <Select.Item key={name} value={name}>{name}</Select.Item>)}
                {form.model_name && !platformNames.includes(form.model_name) && (
                  <Select.Item value={form.model_name}>{form.model_name}（当前）</Select.Item>
                )}
              </Select.Content>
            </Select.Root>
            {!platformModels.isLoading && platformNames.length === 0 && (
              <Text size="1" color="gray" as="div" mt="1">尚未创建平台模型，请先到「模型」页面添加。</Text>
            )}
          </Field>
          <Field label="上游模型名（可选）">
            <ClearableTextField value={form.upstream_model} onChange={(e) => set("upstream_model", e.target.value)} placeholder="留空则使用模型名" />
          </Field>
          <Field label="上下文窗口（可选）">
            <ClearableTextField
              value={form.context_window}
              onChange={(e) => set("context_window", e.target.value)}
              placeholder={modelWindow ? `留空则继承模型默认 ${fmtTokens(modelWindow)}` : "留空则继承模型默认（模型未设置）"}
              className="jc-mono"
            />
            <Text size="1" color={windowInvalid ? "red" : "gray"} as="div" mt="1">
              {windowInvalid ? "格式不对，可写 272000、272k 或 1m。" : "该账号实际给的窗口与模型默认不同时填写，如 Codex 账号上限 272k。"}
            </Text>
          </Field>
          <Field label="上游协议（可选）">
            <Select.Root value={form.upstream_protocol || "_inherit"} onValueChange={(v) => set("upstream_protocol", v === "_inherit" ? "" : v)}>
              <Select.Trigger placeholder="继承账号" />
              <Select.Content>
                {UPSTREAM_PROTOCOLS.map((p) => <Select.Item key={p.value || "_inherit"} value={p.value || "_inherit"}>{p.label}</Select.Item>)}
              </Select.Content>
            </Select.Root>
            <Text size="1" color="gray" as="div" mt="1">
              固定用该协议发到上游；必须是账号原生协议之一，客户端协议不同时走方言转换。
            </Text>
          </Field>
          <Flex align="center" justify="between" gap="3" wrap="wrap">
            <Box>
              <Text size="2" weight="medium" as="div" mb="1">成本模式</Text>
              <SegmentedControl.Root value={costMode} onValueChange={(v) => setCostMode(v as CostMode)}>
                <SegmentedControl.Item value="none">无固定成本</SegmentedControl.Item>
                <SegmentedControl.Item value="per_request">按请求</SegmentedControl.Item>
                <SegmentedControl.Item value="per_usage">按用量</SegmentedControl.Item>
              </SegmentedControl.Root>
            </Box>
            <Box style={{ minWidth: 140 }}>
              <Field label="币种">
                <Select.Root value={form.currency} onValueChange={(v) => set("currency", v)}>
                  <Select.Trigger />
                  <Select.Content>
                    {MODEL_CURRENCIES.map((c) => <Select.Item key={c} value={c}>{c}</Select.Item>)}
                  </Select.Content>
                </Select.Root>
              </Field>
            </Box>
          </Flex>
          {costMode === "per_request" ? (
            <Field label="单请求成本">
              <ClearableTextField type="number" value={form.price_per_request_cost} onChange={(e) => set("price_per_request_cost", e.target.value)} placeholder={`${form.currency}/request`} />
            </Field>
          ) : costMode === "per_usage" ? (
            <Flex gap="3" wrap="wrap">
              {([
                ["input_unit_cost", "输入 token 单价（/1M）"],
                ["output_unit_cost", "输出 token 单价（/1M）"],
                ["cache_read_unit_cost", "cache read 单价（/1M）"],
                ["cache_write_unit_cost", "cache write 5m 单价（/1M）"],
                ["cache_write_1h_unit_cost", "cache write 1h 单价（/1M）"],
              ] as const).map(([key, label]) => (
                <Box key={key} style={{ flex: "1 1 180px" }}>
                  <Field label={label}>
                    <ClearableTextField type="number" value={form[key]} onChange={(e) => set(key, e.target.value)} placeholder={`${form.currency}/1M`} />
                  </Field>
                </Box>
              ))}
            </Flex>
          ) : (
            <Callout.Root color="gray" variant="surface">
              <Callout.Icon><Info size={16} /></Callout.Icon>
              <Callout.Text>该模型显式标记为无固定成本，财务核算按 0 成本处理，但仍会作为该账号支持的模型参与路由。</Callout.Text>
            </Callout.Root>
          )}
          <Flex gap="3" wrap="wrap">
            {([
              ["ref_input_cost", "参考输入价（USD/1M，仅展示）"],
              ["ref_output_cost", "参考输出价（USD/1M，仅展示）"],
            ] as const).map(([key, label]) => (
              <Box key={key} style={{ flex: "1 1 180px" }}>
                <Field label={label}>
                  <ClearableTextField type="number" value={form[key]} onChange={(e) => set(key, e.target.value)} placeholder="例 2.5" />
                </Field>
              </Box>
            ))}
          </Flex>
          <Flex align="center" gap="2">
            <Switch checked={form.enabled} onCheckedChange={(c) => set("enabled", c)} />
            <Text size="2">启用模型</Text>
          </Flex>
          {save.isError && <Text size="2" color="red">{errMsg(save.error)}</Text>}
          <Flex gap="2" justify="end">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button loading={save.isPending} disabled={!canSave} onClick={() => save.mutate()}>保存</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

// CatalogImportDialog: pick models from the cached public catalog
// (models.dev) and bind them to the account. Selected rows import with
// upstream_model = catalog id and the catalog's USD/1M prices as
// display-only ref costs; existing bindings are skipped by the server.
function CatalogImportDialog({
  accountId, open, onClose, onImported,
}: {
  accountId: string;
  open: boolean;
  onClose: () => void;
  onImported: () => void;
}) {
  return (
    <CatalogPickerDialog
      open={open}
      onClose={onClose}
      onImported={onImported}
      hint="目录来自 models.dev，价格为公开的 USD/1M 参考价，仅作展示，不参与计费。已存在的模型不会重复导入。"
      onImport={(picked) =>
        adminApi.importAccountModels(
          accountId,
          picked.map((m): ImportAccountModelItem => ({
            model_name: m.id,
            upstream_model: m.id,
            ref_input_cost: m.cost?.input != null ? String(m.cost.input) : undefined,
            ref_output_cost: m.cost?.output != null ? String(m.cost.output) : undefined,
          })),
        )
      }
    />
  );
}
