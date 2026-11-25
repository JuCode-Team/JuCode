import { useMemo, useState } from "react";
import {
  AlertDialog, Badge, Box, Button, Callout, Checkbox, Flex, Grid, IconButton, SegmentedControl,
  Select, Switch, Text, TextArea, TextField, Tooltip,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import ModelBrandIcon from "../../components/ModelBrandIcon";
import { AlertTriangle, Layers, Plus, Trash2 } from "lucide-react";
import {
  adminApi, type ImportPlatformModelItem, type Model, type PlatformModelsImportResult, type PriceTier,
  type RemoteCatalogModel,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { displayPrice, normalizeCurrency, toStorage } from "../../lib/price";
import { PointIcon } from "../../components/PointAmount";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { ClearableTextField } from "../../components/ClearableTextField";
import { CatalogPickerDialog } from "../../components/CatalogPickerDialog";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";
import { CancelButton, DangerZone, Field, FieldGrid, FormDialog, FormSection, ToggleRow } from "../../components/form/FormLayout";

// Form values are entered per-1M tokens (display-natural — matches
// vendor pricing tables). Conversion to per-1K happens at submit
// time. Prices are platform points (1 积分 = 1 元) — the same unit
// the ledger and the user-facing catalog use.

// One tier row in the form. up_to holds the raw text the admin typed
// ("200000" / "200k" / "1m"); prices are per-1M display strings, empty
// = inherit the base price of the same name.
type TierForm = {
  up_to: string;
  input_unit_price: string;
  output_unit_price: string;
  cache_read_input_price: string;
  cache_write_input_price: string;
  cache_write_1h_input_price: string;
};

type Form = {
  name: string;
  billing_mode: string;
  price_per_request: string;
  input_unit_price: string;
  output_unit_price: string;
  cache_read_input_price: string;
  cache_write_input_price: string;
  cache_write_1h_input_price: string;
  image_input_unit_price: string;
  image_output_unit_price: string;
  audio_input_unit_price: string;
  audio_output_unit_price: string;
  output_image_price: string;
  output_second_price: string;
  input_character_price: string;
  tiers: TierForm[];
  currency: string;
  description: string;
  enabled: boolean;
  vendor_id: string; // "" = no vendor
  display_name: string; // "" = clients show the model name
  picker_hidden: boolean;
  // Capabilities as typed ("272k", "1m"); "" = not configured.
  context_window: string;
  max_output_tokens: string;
  reasoning_efforts: string; // comma-separated ladder
};

const blankForm = (): Form => ({
  name: "", billing_mode: "per_token",
  price_per_request: "", input_unit_price: "", output_unit_price: "",
  cache_read_input_price: "", cache_write_input_price: "", cache_write_1h_input_price: "",
  image_input_unit_price: "", image_output_unit_price: "",
  audio_input_unit_price: "", audio_output_unit_price: "",
  output_image_price: "", output_second_price: "", input_character_price: "",
  tiers: [],
  // Prices below are always platform points. `currency` names the
  // currency the VENDOR publishes this model's list price in — it is
  // display-only (the public catalog's 官方价 column) and billing never
  // reads it. CNY is the safe default: mislabelling a ¥ model as $ is
  // the 6.8× error we're fixing.
  currency: "CNY", description: "", enabled: true,
  vendor_id: "", display_name: "", picker_hidden: false,
  context_window: "", max_output_tokens: "", reasoning_efforts: "",
});

const tierFromModel = (t: PriceTier): TierForm => ({
  up_to: t.up_to_tokens > 0 ? String(t.up_to_tokens) : "",
  input_unit_price: displayPrice(t.input_unit_price, "M"),
  output_unit_price: displayPrice(t.output_unit_price, "M"),
  cache_read_input_price: displayPrice(t.cache_read_input_price, "M"),
  cache_write_input_price: displayPrice(t.cache_write_input_price, "M"),
  cache_write_1h_input_price: displayPrice(t.cache_write_1h_input_price, "M"),
});

const formFromModel = (m: Model): Form => ({
  name: m.name,
  billing_mode: m.billing_mode,
  price_per_request: m.price_per_request ?? "",
  input_unit_price: displayPrice(m.input_unit_price, "M"),
  output_unit_price: displayPrice(m.output_unit_price, "M"),
  cache_read_input_price: displayPrice(m.cache_read_input_price, "M"),
  cache_write_input_price: displayPrice(m.cache_write_input_price, "M"),
  cache_write_1h_input_price: displayPrice(m.cache_write_1h_input_price, "M"),
  image_input_unit_price: displayPrice(m.image_input_unit_price, "M"),
  image_output_unit_price: displayPrice(m.image_output_unit_price, "M"),
  audio_input_unit_price: displayPrice(m.audio_input_unit_price, "M"),
  audio_output_unit_price: displayPrice(m.audio_output_unit_price, "M"),
  output_image_price: m.output_image_price ?? "",
  output_second_price: m.output_second_price ?? "",
  input_character_price: m.input_character_price ?? "",
  tiers: (m.price_tiers ?? []).map(tierFromModel),
  currency: m.currency,
  description: m.description ?? "",
  enabled: m.enabled,
  vendor_id: m.vendor_id ?? "",
  display_name: m.display_name ?? "",
  picker_hidden: m.picker_hidden,
  context_window: m.context_window ? String(m.context_window) : "",
  max_output_tokens: m.max_output_tokens ? String(m.max_output_tokens) : "",
  reasoning_efforts: (m.reasoning_efforts ?? []).join(", "),
});

// fmtTokens renders a token count the way the clients show it: 272K, 1M.
const fmtTokens = (n: number) =>
  n >= 1_000_000 ? `${+(n / 1_000_000).toFixed(2)}M` : n >= 1000 ? `${Math.round(n / 1000)}K` : String(n);

// parseTokenBound accepts "200000", "200k"/"200K" and "1m"/"1M"
// shorthand. Empty = 0 = unbounded. NaN on garbage.
function parseTokenBound(s: string): number {
  const t = s.trim().toLowerCase();
  if (t === "") return 0;
  const m = /^(\d+(?:\.\d+)?)([km]?)$/.exec(t);
  if (!m) return NaN;
  const mult = m[2] === "k" ? 1_000 : m[2] === "m" ? 1_000_000 : 1;
  return Math.round(parseFloat(m[1]) * mult);
}

// fmtTokenBound renders a bound compactly for the tier band label:
// 200000 → "200K", 1048576 → "1.05M".
function fmtTokenBound(n: number): string {
  if (n >= 1_000_000) return `${stripZeros((n / 1_000_000).toFixed(2))}M`;
  if (n >= 1_000) return `${stripZeros((n / 1_000).toFixed(1))}K`;
  return String(n);
}
const stripZeros = (s: string) => s.replace(/\.?0+$/, "");

function tierProblems(tiers: TierForm[]): string[] {
  const out: string[] = [];
  let unbounded = 0;
  const bounds = new Set<number>();
  tiers.forEach((t, i) => {
    const n = parseTokenBound(t.up_to);
    if (Number.isNaN(n)) out.push(`第 ${i + 1} 档：无法识别的 token 上限「${t.up_to}」（支持 200000 / 200k / 1m）`);
    else if (n === 0) unbounded++;
    else if (bounds.has(n)) out.push(`第 ${i + 1} 档：token 上限与其他档位重复`);
    else bounds.add(n);
    const hasPrice = [t.input_unit_price, t.output_unit_price, t.cache_read_input_price, t.cache_write_input_price, t.cache_write_1h_input_price]
      .some((p) => p.trim() !== "");
    if (!hasPrice) out.push(`第 ${i + 1} 档：至少填写一项价格（留空的价格继承基础价）`);
  });
  if (unbounded > 1) out.push("「无上限」档位最多只能有一个（token 上限留空即无上限）");
  return out;
}

function priceSuspicions(f: Form): string[] {
  if (f.billing_mode !== "per_token") return [];
  const inP = parseFloat(f.input_unit_price);
  const outP = parseFloat(f.output_unit_price);
  const cr = parseFloat(f.cache_read_input_price);
  const out: string[] = [];
  if (Number.isFinite(inP) && Number.isFinite(outP) && outP > 0 && outP < inP) {
    out.push(`output (${outP}) 比 input (${inP}) 还低 — 多半填反了。OpenAI/Anthropic 价目里 output 总是 ≥ input。`);
  }
  if (Number.isFinite(inP) && Number.isFinite(cr) && cr > 0 && cr > inP) {
    out.push(`cache_read (${cr}) 比 input (${inP}) 还高 — 多半填反了。cache_read 通常是 input 的 0.1~0.5×。`);
  }
  return out;
}

function modelDeleteError(e: unknown): string {
  // Deletion auto-detaches the model's group bindings, so it normally
  // succeeds; this only fires on an unexpected server error.
  return `删除失败：${errMsg(e)}`;
}

// ---------------- price input with in-field unit ----------------

// PriceInput wraps the shared Field: label + hint come from the form spec, the
// control carries the unit in its right slot.
function PriceInput({ label, value, onChange, placeholder, unit, hint, required }: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  unit: string;
  hint?: string;
  required?: boolean;
}) {
  return (
    <Field label={label} hint={hint} required={required}>
      <TextField.Root
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        className="jc-num"
      >
        <TextField.Slot side="right">
          <Text size="1" color="gray">{unit}</Text>
        </TextField.Slot>
      </TextField.Root>
    </Field>
  );
}

// ---------------- tier editor ----------------

// bandLabel derives the human-readable context range each tier covers
// from the *sorted* bounds, e.g. a 200k tier = "0 – 200K", the
// unbounded tier = "> 200K". Bands are recomputed live as the admin
// types so the editor always reads like the vendor's price table.
function bandLabel(tiers: TierForm[], idx: number): string {
  const mine = parseTokenBound(tiers[idx].up_to);
  if (Number.isNaN(mine)) return "—";
  const bounds = tiers
    .map((t) => parseTokenBound(t.up_to))
    .filter((n) => !Number.isNaN(n) && n > 0)
    .sort((a, b) => a - b);
  if (mine === 0) {
    const max = bounds[bounds.length - 1];
    return max ? `> ${fmtTokenBound(max)} tokens` : "全部上下文";
  }
  const below = bounds.filter((n) => n < mine);
  const lo = below.length ? below[below.length - 1] : 0;
  return `${lo === 0 ? "0" : `> ${fmtTokenBound(lo)}`} – ${fmtTokenBound(mine)} tokens`;
}

function TierEditor({ tiers, onChange, unit }: {
  tiers: TierForm[];
  onChange: (tiers: TierForm[]) => void;
  unit: string;
}) {
  const setTier = (i: number, patch: Partial<TierForm>) =>
    onChange(tiers.map((t, j) => (j === i ? { ...t, ...patch } : t)));
  const remove = (i: number) => onChange(tiers.filter((_, j) => j !== i));
  const add = () =>
    onChange([...tiers, {
      up_to: "", input_unit_price: "", output_unit_price: "",
      cache_read_input_price: "", cache_write_input_price: "", cache_write_1h_input_price: "",
    }]);

  return (
    <Flex direction="column" gap="2">
      {tiers.map((t, i) => (
        <Box
          key={i}
          p="3"
          style={{ border: "1px solid var(--c-outline-variant)", borderRadius: "var(--radius-control)" }}
        >
          <Flex align="center" justify="between" mb="3" gap="2">
            <Flex align="center" gap="2" wrap="wrap">
              <Text size="2" weight="medium">第 {i + 1} 档</Text>
              <Text size="1" color="gray">适用上下文 {bandLabel(tiers, i)}</Text>
            </Flex>
            <Tooltip content="删除此档">
              <IconButton size="1" variant="ghost" color="gray" onClick={() => remove(i)} aria-label="删除此档">
                <Trash2 size={13} />
              </IconButton>
            </Tooltip>
          </Flex>
          <FieldGrid style={{ marginBottom: 12 }}>
            <Field label="上下文 token 上限" hint="支持 200k / 1m 简写；按输入+缓存 token 总量选档。留空为无上限。">
              <TextField.Root
                value={t.up_to}
                placeholder="留空为无上限"
                onChange={(e) => setTier(i, { up_to: e.target.value })}
                className="jc-num"
              >
                <TextField.Slot side="right"><Text size="1" color="gray">tokens</Text></TextField.Slot>
              </TextField.Root>
            </Field>
          </FieldGrid>
          <Grid columns={{ initial: "1", xs: "2", sm: "3" }} gap="3">
            <PriceInput label="input" unit={unit} placeholder="继承基础价"
              value={t.input_unit_price} onChange={(v) => setTier(i, { input_unit_price: v })} />
            <PriceInput label="output" unit={unit} placeholder="继承基础价"
              value={t.output_unit_price} onChange={(v) => setTier(i, { output_unit_price: v })} />
            <PriceInput label="cache read" unit={unit} placeholder="继承基础价"
              value={t.cache_read_input_price} onChange={(v) => setTier(i, { cache_read_input_price: v })} />
            <PriceInput label="cache write 5m" unit={unit} placeholder="继承基础价"
              value={t.cache_write_input_price} onChange={(v) => setTier(i, { cache_write_input_price: v })} />
            <PriceInput label="cache write 1h" unit={unit} placeholder="继承 5m / 基础价"
              value={t.cache_write_1h_input_price} onChange={(v) => setTier(i, { cache_write_1h_input_price: v })} />
          </Grid>
        </Box>
      ))}
      <Flex>
        <Button size="1" variant="soft" color="gray" onClick={add}>
          <Plus size={13} /> 添加阶梯
        </Button>
      </Flex>
    </Flex>
  );
}

export default function Models() {
  const qc = useQueryClient();
  const [billingMode, setBillingMode] = useState("");
  const [vendorId, setVendorId] = useState("");
  const [enabledFilter, setEnabledFilter] = useState("");
  const [selected, setSelected] = useState<Set<string | number>>(new Set());
  const [deleteError, setDeleteError] = useState("");
  // Deletion always goes through the AlertDialog below — never window.confirm.
  const [deleteTarget, setDeleteTarget] = useState<Model | null>(null);
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const paged = usePagedQuery<Model>({
    queryKey: ["admin", "models"],
    queryFn: (params) => adminApi.modelsPaged({
      ...params,
      billing_mode: billingMode || undefined,
      vendor_id: vendorId || undefined,
      enabled: enabledFilter ? enabledFilter === "true" : undefined,
    }),
    pageSize: 50,
    extras: [billingMode, vendorId, enabledFilter],
  });
  const { data: vendors } = useQuery({ queryKey: ["admin", "vendors"], queryFn: adminApi.vendors });
  const [dialog, setDialog] = useState<{ mode: "create" } | { mode: "edit"; id: string } | null>(null);
  const [form, setForm] = useState<Form>(blankForm());
  const [catalogOpen, setCatalogOpen] = useState(false);
  const [importResult, setImportResult] = useState<PlatformModelsImportResult | null>(null);

  const vendorMap = new Map((vendors ?? []).map((v) => [v.id, v]));

  const openCreate = () => { setForm(blankForm()); setDialog({ mode: "create" }); };
  const openEdit = (m: Model) => { setForm(formFromModel(m)); setDialog({ mode: "edit", id: m.id }); };
  const close = () => setDialog(null);

  const buildBody = (): any => {
    const b: any = { billing_mode: form.billing_mode, currency: form.currency, description: form.description };
    if (form.billing_mode === "per_request") {
      // per-request prices are not token-denominated, no conversion.
      b.price_per_request = form.price_per_request;
      b.price_tiers = []; // tiers only apply to per_token
    } else {
      b.input_unit_price = toStorage(form.input_unit_price, "M");
      b.output_unit_price = toStorage(form.output_unit_price, "M");
      // Always send optional prices so an emptied field clears the override
      // ("" → NULL → fall back to the text unit price).
      b.cache_read_input_price = toStorage(form.cache_read_input_price, "M");
      b.cache_write_input_price = toStorage(form.cache_write_input_price, "M");
      b.cache_write_1h_input_price = toStorage(form.cache_write_1h_input_price, "M");
      b.image_input_unit_price = toStorage(form.image_input_unit_price, "M");
      b.image_output_unit_price = toStorage(form.image_output_unit_price, "M");
      b.audio_input_unit_price = toStorage(form.audio_input_unit_price, "M");
      b.audio_output_unit_price = toStorage(form.audio_output_unit_price, "M");
      // Always send the full tier list: [] clears back to flat pricing.
      b.price_tiers = form.tiers.map((t) => {
        const tier: any = { up_to_tokens: parseTokenBound(t.up_to) || 0 };
        if (t.input_unit_price.trim()) tier.input_unit_price = toStorage(t.input_unit_price, "M");
        if (t.output_unit_price.trim()) tier.output_unit_price = toStorage(t.output_unit_price, "M");
        if (t.cache_read_input_price.trim()) tier.cache_read_input_price = toStorage(t.cache_read_input_price, "M");
        if (t.cache_write_input_price.trim()) tier.cache_write_input_price = toStorage(t.cache_write_input_price, "M");
        if (t.cache_write_1h_input_price.trim()) tier.cache_write_1h_input_price = toStorage(t.cache_write_1h_input_price, "M");
        return tier;
      });
    }
    // Generation-endpoint flat surcharges (points per unit; stack on the base
    // mode). Sent raw — not token-denominated. "" clears the surcharge.
    b.output_image_price = form.output_image_price.trim();
    b.output_second_price = form.output_second_price.trim();
    b.input_character_price = form.input_character_price.trim();
    // vendor_id: "" → clear (backend treats empty string as null), else uuid.
    b.vendor_id = form.vendor_id;
    b.display_name = form.display_name.trim(); // "" clears
    b.picker_hidden = form.picker_hidden;
    // Always sent: 0 / [] clears back to "not configured".
    b.context_window = parseTokenBound(form.context_window);
    b.max_output_tokens = parseTokenBound(form.max_output_tokens);
    b.reasoning_efforts = form.reasoning_efforts.split(/[,，\s]+/).map((e) => e.trim()).filter(Boolean);
    return b;
  };

  // Reordering is offered only on an unfiltered, unsearched list. The backend
  // splices a partial list correctly, but with rows hidden the drop lands
  // between rows the admin cannot see, which reads as the drag going wrong.
  const sortable = !paged.rawQ.trim() && !billingMode && !vendorId && !enabledFilter;
  const reorder = useMutation({
    mutationFn: (ids: string[]) => adminApi.reorderModels(ids),
    onMutate: async (ids: string[]) => {
      await qc.cancelQueries({ queryKey: paged.queryKey });
      const prev = qc.getQueryData<{ items: Model[]; total: number }>(paged.queryKey);
      if (prev) {
        const byId = new Map(prev.items.map((m) => [m.id, m]));
        const items = ids.map((id) => byId.get(id)).filter((m): m is Model => Boolean(m));
        qc.setQueryData(paged.queryKey, { ...prev, items });
      }
      return { prev };
    },
    onError: (_e, _ids, ctx) => {
      if (ctx?.prev) qc.setQueryData(paged.queryKey, ctx.prev);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["admin", "models"] }),
  });

  const create = useMutation({
    mutationFn: () => adminApi.createModel({ name: form.name, ...buildBody() }),
    onSuccess: () => { close(); qc.invalidateQueries({ queryKey: ["admin", "models"] }); },
  });
  const update = useMutation({
    mutationFn: (id: string) => adminApi.updateModel(id, { ...buildBody(), enabled: form.enabled }),
    onSuccess: () => { close(); qc.invalidateQueries({ queryKey: ["admin", "models"] }); },
  });
  const del = useMutation({
    mutationFn: (id: string) => adminApi.deleteModel(id),
    onSuccess: () => {
      setDeleteTarget(null);
      if (dialog?.mode === "edit") close();
      qc.invalidateQueries({ queryKey: ["admin", "models"] });
    },
    onError: (e) => { setDeleteTarget(null); setDeleteError(modelDeleteError(e)); },
  });
  const toggleEnabled = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) => adminApi.updateModel(id, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "models"] }),
  });
  const batchEnabled = useMutation({
    mutationFn: ({ ids, enabled }: { ids: string[]; enabled: boolean }) =>
      Promise.all(ids.map((id) => adminApi.updateModel(id, { enabled }))),
    onSuccess: () => { setSelected(new Set()); qc.invalidateQueries({ queryKey: ["admin", "models"] }); },
  });
  const batchDelete = useMutation({
    mutationFn: (ids: string[]) => Promise.all(ids.map((id) => adminApi.deleteModel(id))),
    onSuccess: () => { setSelected(new Set()); setBatchDeleteOpen(false); qc.invalidateQueries({ queryKey: ["admin", "models"] }); },
    onError: (e) => { setBatchDeleteOpen(false); setDeleteError(modelDeleteError(e)); },
  });

  const suspicions = priceSuspicions(form);
  const tierErrors = useMemo(
    () => (form.billing_mode === "per_token" ? tierProblems(form.tiers) : []),
    [form.billing_mode, form.tiers],
  );
  const requiredMissing = form.billing_mode === "per_token"
    ? !form.input_unit_price.trim() || !form.output_unit_price.trim()
    : !form.price_per_request.trim();
  const capErrors = [
    Number.isNaN(parseTokenBound(form.context_window)) && "上下文窗口格式不对，可写 272000、272k 或 1m",
    Number.isNaN(parseTokenBound(form.max_output_tokens)) && "最大输出格式不对，可写 128000 或 128k",
  ].filter((e): e is string => Boolean(e));
  const editing = dialog?.mode === "edit" ? paged.items.find((m) => m.id === dialog.id) : undefined;
  const submitBlocked = requiredMissing || tierErrors.length > 0 || capErrors.length > 0 ||
    (dialog?.mode === "create" && !form.name.trim());

  const submit = () => {
    if (submitBlocked) return;
    if (dialog?.mode === "create") create.mutate();
    else if (dialog?.mode === "edit") update.mutate(dialog.id);
  };

  const unitLabel = "积分 /1M";

  const columns: Column<Model>[] = [
    {
      header: "名称",
      cell: (m) => (
        <Flex align="center" gap="2">
          <ModelBrandIcon model={m.name} size={18} />
          <span className="jc-mono">{m.name}</span>
          {m.vendor_id && vendorMap.has(m.vendor_id) && (
            <Badge variant="soft" color="iris">{vendorMap.get(m.vendor_id)!.name}</Badge>
          )}
          {m.picker_hidden && <Badge variant="soft" color="gray">选择器隐藏</Badge>}
        </Flex>
      ),
    },
    {
      header: "计费方式",
      width: "130px",
      cell: (m) => (
        <Flex gap="1" wrap="wrap">
          <Badge>{m.billing_mode}</Badge>
          {(m.price_tiers?.length ?? 0) > 0 && (
            <Tooltip content={`${m.price_tiers!.length} 档按上下文长度阶梯计费`}>
              <Badge variant="soft" color="purple">
                <Layers size={11} /> ×{m.price_tiers!.length}
              </Badge>
            </Tooltip>
          )}
        </Flex>
      ),
    },
    {
      header: "价格",
      minWidth: "96px",
      cell: (m) =>
        m.billing_mode === "per_request" ? (
          <Box>
            <Text size="2" as="p">{m.price_per_request} <PointIcon size={12} />/req</Text>
            {(m.output_image_price || m.output_second_price || m.input_character_price) && (
              <Text size="1" color="gray" as="p">
                {[
                  m.output_image_price ? `image ${m.output_image_price}/图` : "",
                  m.output_second_price ? `second ${m.output_second_price}/秒` : "",
                  m.input_character_price ? `char ${m.input_character_price}/字` : "",
                ].filter(Boolean).join(" · ")}
              </Text>
            )}
          </Box>
        ) : (
          <Box>
            <Text size="2" as="p">
              in {displayPrice(m.input_unit_price, "M")} · out {displayPrice(m.output_unit_price, "M")} · <PointIcon size={12} />/1M
            </Text>
            {(m.cache_read_input_price || m.cache_write_input_price || m.cache_write_1h_input_price || m.image_input_unit_price || m.image_output_unit_price || m.audio_input_unit_price || m.audio_output_unit_price || m.output_image_price || m.output_second_price || m.input_character_price) && (
              <Text size="1" color="gray" as="p">
                {[
                  m.cache_read_input_price ? `cache read ${displayPrice(m.cache_read_input_price, "M")}` : "",
                  m.cache_write_input_price ? `cache write 5m ${displayPrice(m.cache_write_input_price, "M")}` : "",
                  m.cache_write_1h_input_price ? `cache write 1h ${displayPrice(m.cache_write_1h_input_price, "M")}` : "",
                  m.image_input_unit_price ? `image in ${displayPrice(m.image_input_unit_price, "M")}` : "",
                  m.image_output_unit_price ? `image out ${displayPrice(m.image_output_unit_price, "M")}` : "",
                  m.audio_input_unit_price ? `audio in ${displayPrice(m.audio_input_unit_price, "M")}` : "",
                  m.audio_output_unit_price ? `audio out ${displayPrice(m.audio_output_unit_price, "M")}` : "",
                  m.output_image_price ? `image ${m.output_image_price}/图` : "",
                  m.output_second_price ? `second ${m.output_second_price}/秒` : "",
                  m.input_character_price ? `char ${m.input_character_price}/字` : "",
                ].filter(Boolean).join(" · ")}
              </Text>
            )}
          </Box>
        ),
    },
    {
      header: "上下文",
      width: "110px",
      cell: (m) => {
        const lo = m.advertised_context_window, hi = m.advertised_max_context_window;
        if (!lo) return <Text size="2" color="gray">未设置</Text>;
        return (
          <Tooltip content={hi && hi !== lo ? `对外报 ${fmtTokens(lo)}，用户可调大到 ${fmtTokens(hi)}` : `对外报 ${fmtTokens(lo)}`}>
            <Text size="2" className="jc-mono">{hi && hi !== lo ? `${fmtTokens(lo)}–${fmtTokens(hi)}` : fmtTokens(lo)}</Text>
          </Tooltip>
        );
      },
    },
    {
      header: "启用",
      width: "80px",
      cell: (m) => (
        <Switch
          checked={m.enabled}
          onCheckedChange={(checked) => toggleEnabled.mutate({ id: m.id, enabled: checked })}
        />
      ),
    },
    {
      header: "操作",
      width: "140px",
      align: "right",
      cell: (m) => (
        <Flex gap="2" justify="end">
          <Button size="1" variant="soft" onClick={() => openEdit(m)}>编辑</Button>
          <Button size="1" color="red" variant="soft" onClick={() => setDeleteTarget(m)}>删除</Button>
        </Flex>
      ),
    },
  ];

  const selectedModels = paged.items.filter((m) => selected.has(m.id));
  const selectedIds = selectedModels.map((m) => m.id);
  const filters = (
    <>
      <Select.Root value={billingMode || "_any"} onValueChange={(v) => setBillingMode(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="计费方式" />
        <Select.Content>
          <Select.Item value="_any">全部计费方式</Select.Item>
          <Select.Item value="per_token">per_token</Select.Item>
          <Select.Item value="per_request">per_request</Select.Item>
        </Select.Content>
      </Select.Root>
      <Select.Root value={vendorId || "_any"} onValueChange={(v) => setVendorId(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="厂商" />
        <Select.Content>
          <Select.Item value="_any">全部厂商</Select.Item>
          {(vendors ?? []).map((v) => (
            <Select.Item key={v.id} value={v.id}>{v.name}</Select.Item>
          ))}
        </Select.Content>
      </Select.Root>
      <Select.Root value={enabledFilter || "_any"} onValueChange={(v) => setEnabledFilter(v === "_any" ? "" : v)}>
        <Select.Trigger placeholder="启用状态" />
        <Select.Content>
          <Select.Item value="_any">全部启用状态</Select.Item>
          <Select.Item value="true">已启用</Select.Item>
          <Select.Item value="false">已停用</Select.Item>
        </Select.Content>
      </Select.Root>
    </>
  );

  const bulkActions = (
    <>
      <Button size="1" variant="soft" color="green" loading={batchEnabled.isPending}
        onClick={() => batchEnabled.mutate({ ids: selectedIds, enabled: true })}>
        批量启用
      </Button>
      <Button size="1" variant="soft" color="orange" loading={batchEnabled.isPending}
        onClick={() => batchEnabled.mutate({ ids: selectedIds, enabled: false })}>
        批量停用
      </Button>
      <Button size="1" variant="soft" color="red" loading={batchDelete.isPending}
        onClick={() => setBatchDeleteOpen(true)}>
        批量删除
      </Button>
    </>
  );

  return (
    <AdminPage
      title="模型"
      description="维护模型计费、厂商归属和对外可用状态。"
      actions={
        <Flex gap="2">
          <Button variant="soft" onClick={() => setCatalogOpen(true)}>从目录导入</Button>
          <Button onClick={openCreate}>新建模型</Button>
        </Flex>
      }
    >
      <AdminMetricGrid>
        <AdminMetricCard label="匹配模型" value={paged.total.toLocaleString()} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="当前页启用" value={paged.items.filter((m) => m.enabled).length} detail="enabled" tone="green" />
        <AdminMetricCard label="当前页停用" value={paged.items.filter((m) => !m.enabled).length} detail="disabled" tone="amber" />
        <AdminMetricCard label="已选择" value={selected.size} detail="支持批量启停" tone={selected.size ? "blue" : "gray"} />
      </AdminMetricGrid>

      {deleteError && (
        <Callout.Root color="red" mb="3" role="alert">
          <Callout.Icon><AlertTriangle size={16} /></Callout.Icon>
          <Callout.Text style={{ whiteSpace: "pre-line" }}>{deleteError}</Callout.Text>
          <Button size="1" variant="soft" color="gray" ml="auto" onClick={() => setDeleteError("")}>关闭</Button>
        </Callout.Root>
      )}

      {importResult && (
        <Callout.Root color={importResult.failed.length ? "amber" : "green"} variant="surface" mb="3">
          <Callout.Icon><AlertTriangle size={16} /></Callout.Icon>
          <Callout.Text style={{ whiteSpace: "pre-line" }}>
            {`导入完成：新增 ${importResult.added} 个，已存在跳过 ${importResult.existing} 个` +
              (importResult.failed.length
                ? `，失败 ${importResult.failed.length} 个：${importResult.failed.map((f) => `${f.name}（${f.error}）`).join("、")}`
                : "。")}
          </Callout.Text>
          <Button size="1" variant="soft" color="gray" ml="auto" onClick={() => setImportResult(null)}>关闭</Button>
        </Callout.Root>
      )}

      <DataTable<Model>
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={columns}
        rowKey={(m) => m.id}
        searchPlaceholder="搜索模型名..."
        filters={filters}
        selectedRowKeys={selected}
        onSelectedRowKeysChange={setSelected}
        bulkActions={bulkActions}
        reorderable={sortable}
        onReorder={(ids) => reorder.mutate(ids)}
      />
      {!sortable && (
        <Text size="1" color="gray" as="div" mt="2">
          搜索或筛选状态下不能拖拽排序，清空后可调整顺序。
        </Text>
      )}

      <FormDialog
        open={dialog !== null}
        onOpenChange={(o) => { if (!o) close(); }}
        size="lg"
        title={
          <Flex align="center" gap="2">
            {form.name && <ModelBrandIcon model={form.name} size={20} />}
            {dialog?.mode === "edit" ? "编辑模型" : "新建模型"}
            {dialog?.mode === "edit" && <Text size="2" weight="regular" color="gray" className="jc-mono">{form.name}</Text>}
          </Flex>
        }
        description="价格单位为平台积分（1 积分 = 1 元），与账目口径一致；按 token 计费的价格以每 1M tokens 计。"
        notice={(suspicions.length > 0 || tierErrors.length > 0 || capErrors.length > 0) ? (
          <>
            {[...capErrors, ...tierErrors, ...suspicions].map((t, i) => (
              <span key={i} style={{ display: "block" }}>{t}</span>
            ))}
          </>
        ) : undefined}
        error={create.isError ? errMsg(create.error) : update.isError ? errMsg(update.error) : undefined}
        footer={
          <>
            <CancelButton />
            <Button
              loading={create.isPending || update.isPending}
              disabled={submitBlocked}
              onClick={submit}
            >{dialog?.mode === "edit" ? "保存" : "创建"}</Button>
          </>
        }
      >
        <FormSection title="基本信息" description="模型名是客户端请求里使用的标识，创建后不可改。">
          <FieldGrid>
            <Field label="模型名" code="model" required>
              <ClearableTextField
                placeholder="如 gpt-5-codex"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                disabled={dialog?.mode === "edit"}
                className="jc-mono"
              />
            </Field>
            <Field label="显示名称" hint="客户端模型选择器里显示的名字；留空则显示模型名。">
              <ClearableTextField
                placeholder="如 GPT-6.1 Sol"
                value={form.display_name}
                onChange={(e) => setForm({ ...form, display_name: e.target.value })}
              />
            </Field>
            <Field label="厂商" hint="用于公开模型列表的归类与筛选。">
              <Select.Root value={form.vendor_id || "_none"} onValueChange={(v) => setForm({ ...form, vendor_id: v === "_none" ? "" : v })}>
                <Select.Trigger placeholder="未关联" />
                <Select.Content>
                  <Select.Item value="_none">未关联</Select.Item>
                  {(vendors ?? []).map((v) => (
                    <Select.Item key={v.id} value={v.id}>{v.name}</Select.Item>
                  ))}
                </Select.Content>
              </Select.Root>
            </Field>
            <Field label="描述" hint="一句话介绍用途或特点，显示在公开模型列表。" span="full">
              <TextArea
                rows={2}
                placeholder="如：长上下文代码模型，适合仓库级重构"
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
              />
            </Field>
          </FieldGrid>
          <div className="jc-toggle-rows">
            <ToggleRow
              checked={form.picker_hidden}
              onCheckedChange={(c) => setForm({ ...form, picker_hidden: c })}
              label="在模型选择器中隐藏"
              hint="客户端的模型选择器不列出这个模型，按模型名调用不受影响。"
            />
          </div>
        </FormSection>

        <FormSection
          title="能力参数"
          description="下发给客户端（/v1/models）。留空表示未设置，客户端不显示也不猜测。"
        >
          <FieldGrid columns={3}>
            <Field
              label="上下文窗口"
              code="context_window"
              hint={editing?.advertised_context_window
                ? `当前对外：${fmtTokens(editing.advertised_context_window)}${editing.advertised_max_context_window && editing.advertised_max_context_window !== editing.advertised_context_window ? `，可调大到 ${fmtTokens(editing.advertised_max_context_window)}` : ""}。账号绑定里可单独覆盖。`
                : "各账号的默认值，账号绑定里可单独覆盖。对外报所有账号中的最小值。"}
            >
              <ClearableTextField
                placeholder="如 272k、1m"
                value={form.context_window}
                onChange={(e) => setForm({ ...form, context_window: e.target.value })}
                className="jc-mono"
              />
            </Field>
            <Field label="最大输出" code="max_output_tokens" hint="单次回复的 token 上限。">
              <ClearableTextField
                placeholder="如 128k"
                value={form.max_output_tokens}
                onChange={(e) => setForm({ ...form, max_output_tokens: e.target.value })}
                className="jc-mono"
              />
            </Field>
            <Field label="思考档位" code="reasoning_efforts" hint="逗号分隔，按从低到高的顺序。">
              <ClearableTextField
                placeholder="如 low, medium, high"
                value={form.reasoning_efforts}
                onChange={(e) => setForm({ ...form, reasoning_efforts: e.target.value })}
                className="jc-mono"
              />
            </Field>
          </FieldGrid>
        </FormSection>

        <FormSection title="计费方式" description="计费方式决定下方需要填写哪些价格；币种只影响公开页「官方价」一列的显示符号。">
          <FieldGrid>
            <Field label="计费方式" hint={form.billing_mode === "per_token" ? "按输入 / 输出 token 计费。" : "每次请求固定扣费，不看 token。"}>
              <SegmentedControl.Root
                value={form.billing_mode}
                onValueChange={(v) => setForm({ ...form, billing_mode: v })}
              >
                <SegmentedControl.Item value="per_token">按 token</SegmentedControl.Item>
                <SegmentedControl.Item value="per_request">按次</SegmentedControl.Item>
              </SegmentedControl.Root>
            </Field>
            <Field label="官方标价币种" hint="厂商公布价格用的币种，不参与计费。国产模型选人民币，海外模型选美元。">
              <SegmentedControl.Root
                value={normalizeCurrency(form.currency)}
                onValueChange={(v) => setForm({ ...form, currency: v })}
              >
                <SegmentedControl.Item value="CNY">人民币 ¥</SegmentedControl.Item>
                <SegmentedControl.Item value="USD">美元 $</SegmentedControl.Item>
              </SegmentedControl.Root>
            </Field>
          </FieldGrid>
        </FormSection>

        {form.billing_mode === "per_request" ? (
          <FormSection title="价格" description="每次请求扣除的积分。">
            <FieldGrid>
              <PriceInput
                label="单次请求价格"
                required
                unit="积分 /req"
                placeholder="如 0.1"
                value={form.price_per_request}
                onChange={(v) => setForm({ ...form, price_per_request: v })}
              />
            </FieldGrid>
          </FormSection>
        ) : (
          <>
            <FormSection title="基础价格" description={`每 1M tokens 的积分单价。可选项留空并保存即回退到对应的输入 / 输出价。`}>
              <FieldGrid columns={3}>
                <PriceInput label="输入" required unit={unitLabel} placeholder="如 1.25"
                  value={form.input_unit_price} onChange={(v) => setForm({ ...form, input_unit_price: v })} />
                <PriceInput label="输出" required unit={unitLabel} placeholder="如 10"
                  value={form.output_unit_price} onChange={(v) => setForm({ ...form, output_unit_price: v })} />
                <PriceInput label="缓存读取" unit={unitLabel} placeholder="同输入"
                  hint="缓存命中；GPT 系通常 0.1–0.5× 输入价。"
                  value={form.cache_read_input_price} onChange={(v) => setForm({ ...form, cache_read_input_price: v })} />
                <PriceInput label="缓存写入 5m" unit={unitLabel} placeholder="同输入"
                  hint="5 分钟 TTL；Claude 约 1.25× 输入价。无 5m/1h 明细时整段按此计。"
                  value={form.cache_write_input_price} onChange={(v) => setForm({ ...form, cache_write_input_price: v })} />
                <PriceInput label="缓存写入 1h" unit={unitLabel} placeholder="同 5m"
                  hint="1 小时 TTL；Claude 约 2× 输入价。"
                  value={form.cache_write_1h_input_price} onChange={(v) => setForm({ ...form, cache_write_1h_input_price: v })} />
                <PriceInput label="图像输入 token" unit={unitLabel} placeholder="同输入"
                  hint="Responses image_generation 工具的输入 token。"
                  value={form.image_input_unit_price} onChange={(v) => setForm({ ...form, image_input_unit_price: v })} />
                <PriceInput label="图像输出 token" unit={unitLabel} placeholder="同输出"
                  hint="Responses image_generation 工具的输出 token。"
                  value={form.image_output_unit_price} onChange={(v) => setForm({ ...form, image_output_unit_price: v })} />
                <PriceInput label="音频输入 token" unit={unitLabel} placeholder="同输入"
                  hint="Realtime / audio-preview 的语音输入，官方通常数倍于文本价。"
                  value={form.audio_input_unit_price} onChange={(v) => setForm({ ...form, audio_input_unit_price: v })} />
                <PriceInput label="音频输出 token" unit={unitLabel} placeholder="同输出"
                  hint="留空按文本价计，语音会话会少计费。"
                  value={form.audio_output_unit_price} onChange={(v) => setForm({ ...form, audio_output_unit_price: v })} />
              </FieldGrid>
            </FormSection>

            <FormSection
              title="按上下文长度阶梯计费"
              description="适用于长上下文差异化定价的模型（如 Gemini / Grok 超过 200K tokens 单价上浮）。按请求的输入+缓存 token 总量从低到高匹配档位，档内留空的价格继承基础价；最后一档上限留空表示无上限。"
              actions={form.tiers.length === 0 ? (
                <Button size="2" variant="soft" color="gray"
                  onClick={() => setForm({
                    ...form,
                    tiers: [{ up_to: "200k", input_unit_price: "", output_unit_price: "", cache_read_input_price: "", cache_write_input_price: "", cache_write_1h_input_price: "" }],
                  })}>
                  <Plus size={14} /> 启用阶梯计价
                </Button>
              ) : undefined}
            >
              {form.tiers.length > 0 && (
                <TierEditor tiers={form.tiers} unit={unitLabel} onChange={(tiers) => setForm({ ...form, tiers })} />
              )}
            </FormSection>
          </>
        )}

        <FormSection title="生成端点附加计费" description="用于图像 / 音频 / 视频 / 嵌入端点，按单位叠加在基础价之上，直填不换算。留空并保存即清除该项。">
          <FieldGrid columns={3}>
            <PriceInput label="每张图片" unit="积分 /图" placeholder="图像生成"
              value={form.output_image_price} onChange={(v) => setForm({ ...form, output_image_price: v })} />
            <PriceInput label="每秒时长" unit="积分 /秒" placeholder="视频 / 音频"
              value={form.output_second_price} onChange={(v) => setForm({ ...form, output_second_price: v })} />
            <PriceInput label="每字符" unit="积分 /字" placeholder="TTS 输入"
              value={form.input_character_price} onChange={(v) => setForm({ ...form, input_character_price: v })} />
          </FieldGrid>
        </FormSection>

        {dialog?.mode === "edit" && (
          <>
            <FormSection title="状态">
              <div className="jc-toggle-rows">
                <ToggleRow
                  checked={form.enabled}
                  onCheckedChange={(c) => setForm({ ...form, enabled: c })}
                  label="对外可用"
                  hint={form.enabled ? "用户可以在模型列表看到并调用这个模型。" : "已停用：不出现在模型列表，请求会被拒绝。"}
                />
              </div>
            </FormSection>
            <DangerZone
              title="删除模型"
              description="自动从所有模型分组解绑后永久删除，不可恢复。"
              action={
                <Button color="red" variant="soft" onClick={() => {
                  const target = paged.items.find((m) => m.id === dialog.id);
                  if (target) setDeleteTarget(target);
                }}>
                  <Trash2 size={14} /> 删除模型
                </Button>
              }
            />
          </>
        )}
      </FormDialog>

      <AlertDialog.Root open={!!deleteTarget} onOpenChange={(o) => { if (!o) setDeleteTarget(null); }}>
        <AlertDialog.Content maxWidth="440px">
          <AlertDialog.Title>删除模型</AlertDialog.Title>
          <AlertDialog.Description size="2">
            将删除 <Text weight="medium" className="jc-mono">{deleteTarget?.name}</Text>，并自动从所有模型分组解绑。此操作不可恢复。
          </AlertDialog.Description>
          <Flex gap="2" justify="end" mt="4">
            <AlertDialog.Cancel><Button variant="soft" color="gray">取消</Button></AlertDialog.Cancel>
            <Button color="red" loading={del.isPending} onClick={() => deleteTarget && del.mutate(deleteTarget.id)}>删除</Button>
          </Flex>
        </AlertDialog.Content>
      </AlertDialog.Root>

      <AlertDialog.Root open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialog.Content maxWidth="440px">
          <AlertDialog.Title>删除 {selectedIds.length} 个模型</AlertDialog.Title>
          <AlertDialog.Description size="2">
            选中的模型会自动从所有模型分组解绑后永久删除。此操作不可恢复。
          </AlertDialog.Description>
          <Flex gap="2" justify="end" mt="4">
            <AlertDialog.Cancel><Button variant="soft" color="gray">取消</Button></AlertDialog.Cancel>
            <Button color="red" loading={batchDelete.isPending} onClick={() => batchDelete.mutate(selectedIds)}>删除</Button>
          </Flex>
        </AlertDialog.Content>
      </AlertDialog.Root>

      <PlatformCatalogImportDialog
        open={catalogOpen}
        onClose={() => setCatalogOpen(false)}
        onResult={setImportResult}
        onImported={() => qc.invalidateQueries({ queryKey: ["admin", "models"] })}
      />
    </AdminPage>
  );
}

// PlatformCatalogImportDialog: bulk-create platform models from the public
// catalog (models.dev). Prices are the fork in the road — billing settles in
// CNY points regardless of the model's currency label, so catalog USD costs
// are only usable through an explicit admin-chosen conversion factor
// (USD/1M × factor → 积分/1M). Without the checkbox the rows land unpriced
// and disabled; existing names are skipped server-side, never overwritten.
function PlatformCatalogImportDialog({
  open, onClose, onResult, onImported,
}: {
  open: boolean;
  onClose: () => void;
  onResult: (r: PlatformModelsImportResult) => void;
  onImported: () => void;
}) {
  const [priced, setPriced] = useState(true);
  const [factor, setFactor] = useState("8");
  const [enableNow, setEnableNow] = useState(false);

  const fmtCtx = (ctx: number) =>
    ctx >= 1_000_000 ? `${(ctx / 1_000_000).toLocaleString()}M` : `${Math.round(ctx / 1000).toLocaleString()}K`;

  return (
    <CatalogPickerDialog
      merged
      open={open}
      onClose={onClose}
      onImported={onImported}
      hint="按规范模型名列出：各 provider 下的同一模型已归并为一条，行内标注它出现的 provider。价格为 USD/1M 官方价，平台按积分计费，勾选换算后才写入计费价；同名模型跳过不覆盖。"
      onImport={(picked: RemoteCatalogModel[]) => {
        const k = priced ? parseFloat(factor) : NaN;
        const usePrice = Number.isFinite(k) && k > 0;
        const items: ImportPlatformModelItem[] = picked.map((m) => {
          const item: ImportPlatformModelItem = {
            name: m.id,
            billing_mode: "per_token",
            currency: "CNY",
            description: m.name + (m.context ? `（上下文 ${fmtCtx(m.context)}）` : ""),
            enabled: usePrice && enableNow,
            context_window: m.context || undefined,
          };
          if (usePrice && m.cost) {
            item.input_unit_price = toStorage(String(m.cost.input * k), "M");
            item.output_unit_price = toStorage(String(m.cost.output * k), "M");
            if (m.cost.cache_read != null) item.cache_read_input_price = toStorage(String(m.cost.cache_read * k), "M");
            if (m.cost.cache_write != null) item.cache_write_input_price = toStorage(String(m.cost.cache_write * k), "M");
          }
          return item;
        });
        return adminApi.importModels(items).then((r) => { onResult(r); return r; });
      }}
      extra={
        <Flex direction="column" gap="2">
          <Flex align="center" gap="2" wrap="wrap">
            <Checkbox checked={priced} onCheckedChange={(c) => setPriced(c === true)} />
            <Text size="2">按目录价换算填入计费价（USD/1M × 系数 → 积分/1M）</Text>
            <TextField.Root
              size="1"
              type="number"
              value={factor}
              disabled={!priced}
              onChange={(e) => setFactor(e.target.value)}
              style={{ width: 90 }}
            />
            <Text size="1" color="gray">系数含汇率与加价，可随时改；例：$2 ×8 = 16 积分/1M</Text>
          </Flex>
          <Flex align="center" gap="2">
            <Checkbox checked={enableNow} disabled={!priced} onCheckedChange={(c) => setEnableNow(c === true)} />
            <Text size="2" color={priced ? undefined : "gray"}>导入后立即启用</Text>
            {!priced && <Text size="1" color="gray">未设价的模型只能停用导入，设价后手动启用</Text>}
          </Flex>
        </Flex>
      }
    />
  );
}
