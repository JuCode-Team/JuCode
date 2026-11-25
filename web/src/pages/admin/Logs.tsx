import { Fragment, useEffect, useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import { Badge, Box, Button, Card, Code, Flex, Heading, IconButton, Switch, Table, Text, Tooltip } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import LiveRateBar from "../../components/LiveRateBar";
import { UsageSubjectIcon, usageSubjectName } from "../../components/UsageSubject";
import { isWebToolEndpoint } from "../../api/webTools";
import { ChevronRight, RotateCcw } from "lucide-react";
import { adminApi, type AnomalyFilter } from "../../api/endpoints";
import { TableSkeletonRows } from "../../components/ConsoleSkeleton";
import CopyableId from "../../components/CopyableId";
import { DateRangeInput, last24HoursStart } from "../../components/UsageLogFilters";
import { formatAmount } from "../../lib/currency";
import { AdminPage } from "../../components/AdminPage";
import { FilterBar, FilterField, FilterInput, FilterSelect, type FilterChip, type FilterOption } from "../../components/list/FilterBar";
import { ListPager } from "../../components/list/ListPager";
import { ColumnPicker, useColumnVisibility, type ColumnDef } from "../../components/list/ColumnPicker";
import { useDebouncedValue } from "../../lib/useDebouncedValue";
import { errorClassColor, errorClassLabel } from "../../lib/errorClass";
import { anomalyBadge, parseAnomalyFilter } from "../../lib/logAnomaly";

const PAGE_SIZE = 50;

const statusColor = (s: string): "green" | "orange" | "red" | "gray" =>
  s === "ok" ? "green" : s === "quota_exceeded" ? "orange" : s === "internal_error" || s === "upstream_error" ? "red" : "gray";

function statusLabel(status: string): string {
  if (status === "ok") return "成功";
  if (status === "upstream_error") return "上游错误";
  if (status === "quota_exceeded") return "超额";
  if (status === "banned") return "屏蔽";
  if (status === "unauthorized") return "未授权";
  if (status === "internal_error") return "内部错误";
  return status || "—";
}

const STATUS_OPTIONS: FilterOption[] = [
  { v: "ok", l: "成功" },
  { v: "upstream_error", l: "上游错误" },
  { v: "quota_exceeded", l: "超额" },
  { v: "banned", l: "屏蔽" },
  { v: "unauthorized", l: "未授权" },
  { v: "internal_error", l: "内部错误" },
];
const ANOMALY_OPTIONS: FilterOption[] = [
  { v: "any", l: "仅异常" },
  { v: "time", l: "时间异常" },
  { v: "error", l: "错误异常" },
  { v: "none", l: "非异常" },
];
const ENDPOINT_OPTIONS: FilterOption[] = [
  { v: "chat_completions", l: "chat completions" },
  { v: "responses", l: "responses" },
  { v: "messages", l: "messages (Anthropic)" },
  { v: "embeddings", l: "embeddings" },
  { v: "image_generation", l: "image generation" },
  { v: "image_edit", l: "image edit" },
  { v: "image_variation", l: "image variation" },
  { v: "audio_speech", l: "audio speech (TTS)" },
  { v: "audio_transcription", l: "audio transcription" },
  { v: "audio_translation", l: "audio translation" },
  { v: "rerank", l: "rerank" },
  { v: "video_generation", l: "video generation" },
  { v: "web_search", l: "web search" },
  { v: "web_fetch", l: "web fetch" },
];
const BILLING_OPTIONS: FilterOption[] = [
  { v: "plan", l: "套餐" },
  { v: "plan_balance", l: "按量套餐" },
  { v: "balance", l: "余额" },
  { v: "none", l: "无" },
];
const optionLabel = (opts: FilterOption[], v: string) => opts.find((o) => o.v === v)?.l ?? v;

// Column registry for the log table. Nine columns show by default; the rest
// are one click away in the 「列」 menu and the choice is remembered locally.
// Request ID stays reachable through the expanded detail panel.
const LOG_COLUMNS: ColumnDef[] = [
  { key: "request_id", label: "Request ID", defaultHidden: true },
  { key: "time", label: "时间", locked: true },
  { key: "user", label: "用户" },
  { key: "api_key", label: "令牌", defaultHidden: true },
  { key: "endpoint", label: "端点", defaultHidden: true },
  { key: "model", label: "模型", locked: true },
  { key: "tokens", label: "tokens" },
  { key: "cost", label: "积分" },
  { key: "status", label: "状态", locked: true },
  { key: "anomaly", label: "异常" },
  { key: "http", label: "HTTP", defaultHidden: true },
  { key: "retry", label: "重试", defaultHidden: true },
  { key: "provider", label: "渠道 / 尝试" },
  { key: "stream", label: "流式", defaultHidden: true },
  { key: "reasoning", label: "推理", defaultHidden: true },
  { key: "ttfb", label: "首包", defaultHidden: true },
  { key: "ttft", label: "首 token", defaultHidden: true },
  { key: "latency", label: "延迟" },
  { key: "stages", label: "阶段", defaultHidden: true },
  { key: "ip", label: "IP", defaultHidden: true },
  { key: "ua", label: "UA", defaultHidden: true },
];

type UsageAttempt = {
  attempt?: number;
  provider?: string;
  provider_id?: string;
  provider_kind?: string;
  upstream_model?: string;
  strategy_priority?: number;
  http_status?: number;
  error_message?: string;
  error_class?: string;
  switch_strategy?: string;
  latency_ms?: Record<string, number>;
  final?: boolean;
};

function shortId(v?: string): string {
  if (!v) return "—";
  return v.length > 12 ? `${v.slice(0, 12)}…` : v;
}

function attemptProviderChain(l: any): string {
  const attempts = (l.attempts ?? []) as UsageAttempt[];
  if (attempts.length > 0) {
    return attempts.map((a) => a.provider || shortId(a.provider_id)).join(" → ");
  }
  return (l.provider_chain ?? []).join(" → ");
}

const latencyLabels: Record<string, string> = {
  body_read_ms: "读 body",
  body_bytes: "请求体大小",
  content_length_bytes: "Content-Length",
  json_decode_ms: "解析 JSON",
  policy_scan_ms: "关键词检测",
  route_ms: "路由",
  route_candidate_groups: "候选分组数",
  route_group_index: "落地分组位次",
  cross_group_switches: "跨组切换次数",
  access_check_ms: "权限",
  concurrency_ms: "并发",
  billing_precheck_ms: "计费预检",
  request_translate_ms: "协议转换",
  image_output_policy_ms: "图片策略",
  first_sse_frame_ms: "首 SSE 帧",
  first_token_delta_ms: "首 token",
  first_token_timeout_ms: "首 token 超时",
  first_text_delta_ms: "首文本 token",
};

const latencyOrder = [
  "body_read_ms",
  "body_bytes",
  "content_length_bytes",
  "json_decode_ms",
  "policy_scan_ms",
  "route_ms",
  "route_candidate_groups",
  "route_group_index",
  "cross_group_switches",
  "access_check_ms",
  "concurrency_ms",
  "billing_precheck_ms",
  "request_translate_ms",
  "image_output_policy_ms",
  "first_sse_frame_ms",
  "first_token_delta_ms",
  "first_token_timeout_ms",
  "first_text_delta_ms",
];

const attemptLatencyLabels: Record<string, string> = {
  request_prepare_ms: "准备请求",
  provider_build_ms: "构建渠道",
  upstream_header_ms: "上游响应头",
};

function orderedLatencyEntries(values?: Record<string, number>, order = latencyOrder) {
  if (!values) return [];
  const seen = new Set<string>();
  const out: Array<[string, number]> = [];
  for (const key of order) {
    if (typeof values[key] === "number") {
      out.push([key, values[key]]);
      seen.add(key);
    }
  }
  for (const [key, value] of Object.entries(values)) {
    if (!seen.has(key) && typeof value === "number") out.push([key, value]);
  }
  return out;
}

function formatBytes(value: number): string {
  if (!Number.isFinite(value)) return `${value}`;
  const units = ["B", "KB", "MB", "GB"];
  let n = value;
  let i = 0;
  while (Math.abs(n) >= 1024 && i < units.length - 1) {
    n /= 1024;
    i += 1;
  }
  const digits = i === 0 ? 0 : n >= 10 ? 1 : 2;
  return `${n.toFixed(digits)}${units[i]}`;
}

function latencyValueText(key: string, value: number): string {
  if (key.endsWith("_bytes")) return formatBytes(value);
  return `${value}ms`;
}

function latencyText(values?: Record<string, number>): string {
  const entries = orderedLatencyEntries(values);
  if (entries.length === 0) return "—";
  return entries.map(([key, value]) => `${latencyLabels[key] ?? key.replace(/_ms$/, "")}:${latencyValueText(key, value)}`).join(" · ");
}

function LatencyTooltip({ log }: { log: any }) {
  const details = orderedLatencyEntries(log.latency_detail);
  const attempts = (log.attempts ?? []) as UsageAttempt[];
  return (
    <Box style={{ maxWidth: 620 }}>
      {details.length > 0 ? details.map(([key, value]) => (
        <Text key={key} size="1" as="div">
          {latencyLabels[key] ?? key}: <Code>{latencyValueText(key, value)}</Code>
        </Text>
      )) : <Text size="1" color="gray">无阶段耗时</Text>}
      {attempts.length > 0 && (
        <Box mt="2">
          <Text size="1" as="div" color="gray">渠道尝试耗时</Text>
          {attempts.map((a) => {
            const entries = orderedLatencyEntries(a.latency_ms, ["request_prepare_ms", "provider_build_ms", "upstream_header_ms"]);
            if (entries.length === 0) return null;
            return (
              <Text key={`lat-${a.attempt}-${a.provider_id}`} size="1" as="div">
                #{a.attempt ?? "?"} {a.provider || shortId(a.provider_id)}：{entries.map(([key, value]) => `${attemptLatencyLabels[key] ?? key} ${value}ms`).join(" · ")}
              </Text>
            );
          })}
        </Box>
      )}
    </Box>
  );
}

// AnomalyCell stays empty for a normal row — and for a backend that
// predates the anomaly field, where every row reads as normal.
function AnomalyCell({ log }: { log: any }) {
  const badge = anomalyBadge(log.anomaly);
  if (!badge) return <Text size="1" color="gray">—</Text>;
  return (
    <Tooltip content={badge.reasons.join(" · ") || "异常请求"}>
      <Badge size="1" variant="soft" color={badge.color} style={{ cursor: "help" }}>{badge.label}</Badge>
    </Tooltip>
  );
}

function AttemptsTooltip({ log }: { log: any }) {
  const attempts = (log.attempts ?? []) as UsageAttempt[];
  if (attempts.length === 0) {
    return (
      <Box style={{ maxWidth: 420 }}>
        {log.group && <Text size="1" as="div">分组：{log.group}</Text>}
        {log.provider && <Text size="1" as="div">最终渠道：{log.provider}</Text>}
        {(log.provider_chain?.length ?? 0) > 0 && (
          <Text size="1" as="div" color="gray">尝试链：{log.provider_chain.join(" → ")}</Text>
        )}
        {log.error_message && <Text size="1" as="div" mt="1" style={{ whiteSpace: "pre-wrap" }}>{log.error_message}</Text>}
      </Box>
    );
  }
  return (
    <Box style={{ maxWidth: 560 }}>
      {attempts.map((a) => (
        <Box key={`${a.attempt}-${a.provider_id}`} mb="2">
          <Text size="1" as="div">
            #{a.attempt ?? "?"} {a.provider || shortId(a.provider_id)}
            {a.final ? " · final" : ""}
          </Text>
          <Text size="1" as="div" color="gray">
            status={a.http_status ?? "—"} priority={a.strategy_priority ?? "—"}
            {a.switch_strategy ? ` strategy=${a.switch_strategy}` : ""}
            {a.provider_kind ? ` kind=${a.provider_kind}` : ""}
          </Text>
          {a.upstream_model && <Text size="1" as="div" color="gray">model={a.upstream_model}</Text>}
          {a.latency_ms && <Text size="1" as="div" color="gray">latency={latencyText(a.latency_ms)}</Text>}
          {a.error_message && <Text size="1" as="div" style={{ whiteSpace: "pre-wrap" }}>{a.error_message}</Text>}
        </Box>
      ))}
    </Box>
  );
}

// RequestDetailPanel is the per-request drill-down: full failover chain,
// per-attempt error_class / status / latency, and the upstream request id —
// rendered inline beneath a log row when an operator expands it to triage a
// single request.
function RequestDetailPanel({ log }: { log: any }) {
  const attempts = (log.attempts ?? []) as UsageAttempt[];
  const anomaly = anomalyBadge(log.anomaly);
  const kv = (k: string, v: ReactNode) => (
    <Flex gap="2" align="center">
      <Text size="1" color="gray" style={{ width: 120, flexShrink: 0 }}>{k}</Text>
      <Box style={{ minWidth: 0 }}>{v}</Box>
    </Flex>
  );
  return (
    <Box
      style={{
        padding: "14px 18px",
        background: "var(--c-surface-container-low)",
        borderTop: "1px solid var(--c-outline-variant)",
      }}
    >
      <Flex direction="column" gap="1" mb="3">
        {kv("request_id", <CopyableId value={log.request_id || log.id} label="点击复制请求ID" />)}
        {log.upstream_request_id ? kv("上游 request_id", <CopyableId value={log.upstream_request_id} color="gray" label="点击复制上游请求ID" />) : null}
        {kv(isWebToolEndpoint(log.endpoint) ? "工具" : "模型", <Text size="1">{usageSubjectName(log.endpoint, log.model)} {log.upstream_model ? `→ ${log.upstream_model}` : ""}</Text>)}
        {kv("状态", <><Badge size="1" color={statusColor(log.status)} variant="soft">{statusLabel(log.status)}</Badge> <Text size="1" color="gray">HTTP {log.http_status}</Text></>)}
        {log.error_class ? kv("错误分类", (
          <Tooltip content={log.error_class}>
            <Badge size="1" color={errorClassColor(log.error_class)} variant="soft">{errorClassLabel(log.error_class)}</Badge>
          </Tooltip>
        )) : null}
        {anomaly ? kv("异常", (
          <Flex gap="1" align="center" wrap="wrap">
            <Badge size="1" variant="soft" color={anomaly.color}>{anomaly.label}</Badge>
            <Text size="1" color="gray">{anomaly.reasons.join(" · ")}</Text>
          </Flex>
        )) : null}
        {kv("重试次数", <Text size="1">{log.retry_count ?? 0}</Text>)}
        {kv("用量", <Text size="1">输入 {log.tokens_in ?? 0} · 缓存 {log.cached_tokens_in ?? 0} · 写入 {log.cache_creation_tokens_in ?? 0}{(log.cache_creation_1h_tokens_in ?? 0) > 0 ? `（5m ${log.cache_creation_5m_tokens_in ?? 0} + 1h ${log.cache_creation_1h_tokens_in}）` : ""} · 输出 {log.tokens_out ?? 0} · 生图 {log.image_tokens_in ?? 0}/{log.image_tokens_out ?? 0} · 语音 {log.audio_tokens_in ?? 0}/{log.audio_tokens_out ?? 0} · 图片 {log.output_images ?? 0}</Text>)}
        {kv("延迟", <Text size="1">总 {log.latency_ms}ms · TTFT {log.ttft_ms ?? "—"} · TTFB {log.ttfb_ms ?? "—"}</Text>)}
        {log.error_message ? kv("错误", <Text size="1" color="red" style={{ whiteSpace: "pre-wrap" }}>{log.error_message}</Text>) : null}
      </Flex>

      <Heading size="2" mb="1">尝试链路（{attempts.length || (log.provider_chain?.length ?? 0)}）</Heading>
      {attempts.length > 0 ? (
        <Box style={{ overflowX: "auto" }}>
          <div className="jc-local-table-scroll">
          <Table.Root size="1" variant="surface">
            <Table.Header>
              <Table.Row>
                <Table.ColumnHeaderCell>#</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell>渠道</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell>HTTP</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell>错误分类</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell>策略</Table.ColumnHeaderCell>
                <Table.ColumnHeaderCell>耗时</Table.ColumnHeaderCell>
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {attempts.map((a) => (
                <Table.Row key={`${a.attempt}-${a.provider_id}`}>
                  <Table.Cell>{a.attempt ?? "?"}{a.final ? " ·终" : ""}</Table.Cell>
                  <Table.Cell>
                    <Text size="1">{a.provider || shortId(a.provider_id)}</Text>
                    {a.provider_kind ? <Text size="1" color="gray" as="div">{a.provider_kind}</Text> : null}
                  </Table.Cell>
                  <Table.Cell>{a.http_status || "—"}</Table.Cell>
                  <Table.Cell>{a.error_class ? (
                    <Tooltip content={a.error_class}>
                      <Badge size="1" variant="soft" color={errorClassColor(a.error_class)}>{errorClassLabel(a.error_class)}</Badge>
                    </Tooltip>
                  ) : <Text size="1" color="gray">—</Text>}</Table.Cell>
                  <Table.Cell><Text size="1" color="gray">{a.switch_strategy || "—"}</Text></Table.Cell>
                  <Table.Cell><Text size="1" color="gray">{a.latency_ms ? latencyText(a.latency_ms) : "—"}</Text></Table.Cell>
                </Table.Row>
              ))}
            </Table.Body>
          </Table.Root>
          </div>
        </Box>
      ) : (
        <Text size="1" color="gray">{(log.provider_chain ?? []).join(" → ") || "无尝试记录"}</Text>
      )}

      {log.latency_detail && Object.keys(log.latency_detail).length > 0 && (
        <>
          <Heading size="2" mt="3" mb="1">阶段耗时</Heading>
          <Flex gap="2" wrap="wrap">
            {Object.entries(log.latency_detail as Record<string, number>).map(([k, v]) => (
              <Badge key={k} size="1" variant="soft" color="gray">{k}={v}</Badge>
            ))}
          </Flex>
        </>
      )}
    </Box>
  );
}

// HTML <input type=datetime-local> emits local-wall-clock; convert to RFC3339
// (timezone-aware) before sending to the backend so the same string isn't
// silently reinterpreted as UTC on the way in.
function localToRFC(s: string): string | undefined {
  if (!s) return undefined;
  const d = new Date(s);
  if (isNaN(d.getTime())) return undefined;
  return d.toISOString();
}

// The 性能观测 drill-down links here with an RFC3339 start; the picker
// stores local wall-clock, so convert on the way in.
function rfcToLocal(value: string | null): string {
  if (!value) return "";
  const d = new Date(value);
  if (isNaN(d.getTime())) return "";
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export default function Logs() {
  // Deep links from 性能观测 ("查看异常请求") arrive as query params. Read
  // them once at mount: the route remounts on each inbound link, so
  // initializers are enough and the operator stays free to edit after.
  const [searchParams] = useSearchParams();
  const [endpoint, setEndpoint] = useState("");
  const [billingSource, setBillingSource] = useState("");
  const [status, setStatus] = useState("");
  const [model, setModel] = useState(() => searchParams.get("model") ?? "");
  const [ip, setIp] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [requestId, setRequestId] = useState("");
  const [anomaly, setAnomalyValue] = useState<AnomalyFilter | "">(() => parseAnomalyFilter(searchParams.get("anomaly")));
  const [providerId, setProviderId] = useState(() => searchParams.get("provider_id") ?? "");
  const [groupId, setGroupId] = useState(() => searchParams.get("group_id") ?? "");
  const [start, setStart] = useState(() => {
    const fromUrl = rfcToLocal(searchParams.get("start"));
    if (fromUrl) return fromUrl;
    return parseAnomalyFilter(searchParams.get("anomaly")) ? last24HoursStart() : "";
  });
  const [end, setEnd] = useState(() => rfcToLocal(searchParams.get("end")));
  const [user, setUser] = useState("");
  const [userId, setUserId] = useState("");
  const [offset, setOffset] = useState(0);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [expandedRows, setExpandedRows] = useState<Set<string>>(() => new Set());
  const cols = useColumnVisibility("admin.logs.columns", LOG_COLUMNS);

  // Text filters trail the input by 300ms so a request goes out per pause,
  // not per keystroke; polling also pauses while the operator is typing.
  const modelQ = useDebouncedValue(model.trim());
  const ipQ = useDebouncedValue(ip.trim());
  const apiKeyQ = useDebouncedValue(apiKey.trim());
  const requestIdQ = useDebouncedValue(requestId.trim());
  const userQ = useDebouncedValue(user.trim());
  const userIdQ = useDebouncedValue(userId.trim());
  const typing =
    modelQ !== model.trim() || ipQ !== ip.trim() || apiKeyQ !== apiKey.trim() ||
    requestIdQ !== requestId.trim() || userQ !== user.trim() || userIdQ !== userId.trim();

  // Without an explicit start the backend floors an anomaly query at the
  // last 24h. Mirror that into the picker so the range on screen is the
  // range being queried.
  const setAnomaly = (v: string) => {
    const next = parseAnomalyFilter(v);
    setAnomalyValue(next);
    if (next && !start) setStart(last24HoursStart());
  };

  const toggleExpanded = (id: string) => {
    setExpandedRows((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // Server-side filters resolve to the actual query params.
  const filterParams = {
    model: modelQ || undefined,
    status: status || undefined,
    endpoint: endpoint || undefined,
    billing_source: billingSource || undefined,
    ip: ipQ || undefined,
    api_key: apiKeyQ || undefined,
    request_id: requestIdQ || undefined,
    user: userQ || undefined,
    user_id: userIdQ || undefined,
    anomaly: anomaly || undefined,
    provider_id: providerId || undefined,
    group_id: groupId || undefined,
    start: localToRFC(start),
    end: localToRFC(end),
  };
  const filterSig = JSON.stringify(filterParams);

  // The total is counted once per filter set. Turning pages inside the same
  // set asks the backend to skip the COUNT (skip_count=1 → total -1) and
  // reuses the number already on screen. A backend without the flag simply
  // keeps returning the count, which the effect below records again.
  const [knownTotal, setKnownTotal] = useState<{ sig: string; total: number } | null>(null);
  const skipCount = offset > 0 && knownTotal?.sig === filterSig;
  const params = { limit: PAGE_SIZE, offset, ...filterParams, skip_count: skipCount ? 1 : undefined };

  const { data, isLoading, isFetching, refetch } = useQuery({
    queryKey: ["admin", "logs", params],
    queryFn: () => adminApi.usageLogs(params),
    refetchInterval: autoRefresh && !typing ? 5000 : false,
    placeholderData: (prev) => prev,
  });

  useEffect(() => {
    if (data && data.total >= 0) setKnownTotal({ sig: filterSig, total: data.total });
  }, [data, filterSig]);

  // Jump back to the first page on any filter change.
  useEffect(() => {
    setOffset(0);
  }, [filterSig]);

  const reset = () => {
    setEndpoint(""); setBillingSource(""); setStatus("");
    setModel(""); setIp(""); setApiKey(""); setRequestId("");
    setAnomalyValue(""); setProviderId(""); setGroupId("");
    setStart(""); setEnd("");
    setUser(""); setUserId("");
    setOffset(0);
  };

  const total = data && data.total >= 0 ? data.total : knownTotal?.sig === filterSig ? knownTotal.total : null;
  const logs: any[] = data?.logs ?? [];
  const page = Math.floor(offset / PAGE_SIZE);
  const hasPrev = offset > 0;
  const hasNext = total !== null ? offset + PAGE_SIZE < total : logs.length === PAGE_SIZE;

  const chips: FilterChip[] = [];
  if (start || end) {
    chips.push({ key: "range", label: "时间", value: `${start ? start.replace("T", " ") : "—"} 至 ${end ? end.replace("T", " ") : "现在"}`, onRemove: () => { setStart(""); setEnd(""); } });
  }
  if (status) chips.push({ key: "status", label: "状态", value: statusLabel(status), onRemove: () => setStatus("") });
  if (anomaly) chips.push({ key: "anomaly", label: "异常", value: optionLabel(ANOMALY_OPTIONS, anomaly), onRemove: () => setAnomalyValue("") });
  if (model.trim()) chips.push({ key: "model", label: "模型", value: model.trim(), onRemove: () => setModel("") });
  if (endpoint) chips.push({ key: "endpoint", label: "端点", value: optionLabel(ENDPOINT_OPTIONS, endpoint), onRemove: () => setEndpoint("") });
  if (billingSource) chips.push({ key: "billing", label: "计费源", value: optionLabel(BILLING_OPTIONS, billingSource), onRemove: () => setBillingSource("") });
  if (apiKey.trim()) chips.push({ key: "apiKey", label: "API Key", value: apiKey.trim(), onRemove: () => setApiKey("") });
  if (ip.trim()) chips.push({ key: "ip", label: "IP", value: ip.trim(), onRemove: () => setIp("") });
  if (requestId.trim()) chips.push({ key: "requestId", label: "Request ID", value: requestId.trim(), onRemove: () => setRequestId("") });
  if (user.trim()) chips.push({ key: "user", label: "用户", value: user.trim(), onRemove: () => setUser("") });
  if (userId.trim()) chips.push({ key: "userId", label: "用户 ID", value: userId.trim(), onRemove: () => setUserId("") });
  if (providerId) chips.push({ key: "provider", label: "渠道", value: shortId(providerId), onRemove: () => setProviderId("") });
  if (groupId) chips.push({ key: "group", label: "分组", value: shortId(groupId), onRemove: () => setGroupId("") });

  const advancedActive = [endpoint, billingSource, apiKey.trim(), ip.trim(), requestId.trim(), user.trim(), userId.trim()].filter(Boolean).length;

  const visibleColumns = useMemo(() => LOG_COLUMNS.filter((c) => cols.isVisible(c.key)), [cols]);
  const colSpan = visibleColumns.length + 1;

  const summary = total === null
    ? <>已加载 <strong>{logs.length}</strong> 条</>
    : <>共 <strong>{total.toLocaleString()}</strong> 条{isFetching ? " · 刷新中" : ""}</>;

  return (
    <AdminPage
      title="使用日志"
      description="全平台调用流水。筛选后自动回到第一页；文本筛选在停止输入 300ms 后生效。"
    >
      {/* Platform-wide live throughput, above the log for the same reason as
          the per-user bar on the usage page. No cap applies platform-wide, so
          the limit badges stay hidden here. */}
      <LiveRateBar queryKey={["admin"]} fetcher={adminApi.statsRate} title="平台当前速率" />

      <Card mb="3" className="jc-filter-card">
        <FilterBar
          primary={
            <>
              <FilterField label="时间范围" className={start || end ? "is-active" : ""}>
                <DateRangeInput start={start} setStart={setStart} end={end} setEnd={setEnd} />
              </FilterField>
              <FilterSelect label="状态" value={status} onChange={setStatus} options={STATUS_OPTIONS} />
              <FilterSelect label="异常" value={anomaly} onChange={setAnomaly} options={ANOMALY_OPTIONS} allLabel="不限" />
              <FilterInput label="模型" value={model} onChange={setModel} placeholder="模糊匹配" width={190} />
            </>
          }
          advanced={
            <>
              <FilterSelect label="端点" value={endpoint} onChange={setEndpoint} options={ENDPOINT_OPTIONS} />
              <FilterSelect label="计费源" value={billingSource} onChange={setBillingSource} options={BILLING_OPTIONS} />
              <FilterInput label="API Key" value={apiKey} onChange={setApiKey} placeholder="名称" width={170} />
              <FilterInput label="IP" value={ip} onChange={setIp} width={150} />
              <FilterInput label="Request ID" value={requestId} onChange={setRequestId} width={230} />
              <FilterInput label="用户" value={user} onChange={setUser} placeholder="邮箱 / 昵称" width={190} />
              <FilterInput label="用户 ID" value={userId} onChange={setUserId} placeholder="UUID" width={230} />
            </>
          }
          advancedActive={advancedActive}
          trailing={
            <>
              <Text as="label" size="1" className="jc-filter-auto">
                <Flex align="center" gap="2">
                  <Switch size="1" checked={autoRefresh} onCheckedChange={setAutoRefresh} />
                  自动刷新 5s
                </Flex>
              </Text>
              <Tooltip content="立即刷新">
                <IconButton size="2" variant="soft" color="gray" aria-label="刷新" onClick={() => refetch()} loading={isFetching && !isLoading}>
                  <RotateCcw size={14} />
                </IconButton>
              </Tooltip>
            </>
          }
          chips={chips}
          onClearAll={reset}
          summary={summary}
        />
      </Card>

      <Card className="jc-list-card">
        <div className="jc-list-head">
          <div className="jc-list-head-title">
            {total === null
              ? `第 ${offset + 1}–${offset + logs.length} 条`
              : total === 0 ? "0 条记录" : <>第 {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} 条 · 共 <strong>{total.toLocaleString()}</strong></>}
          </div>
          <div className="jc-list-head-actions">
            {expandedRows.size > 0 && (
              <Button size="1" variant="ghost" color="gray" onClick={() => setExpandedRows(new Set())}>收起全部</Button>
            )}
            <ColumnPicker defs={LOG_COLUMNS} isVisible={cols.isVisible} onToggle={cols.toggle} onReset={cols.reset} isDefault={cols.isDefault} />
          </div>
        </div>
        <Box style={{ overflowX: "auto" }}>
          <div className="jc-local-table-scroll">
          <Table.Root variant="surface" size="1">
            <Table.Header>
              <Table.Row>
                <Table.ColumnHeaderCell className="jc-cell-expand" aria-label="展开" />
                {visibleColumns.map((c) => (
                  <Table.ColumnHeaderCell key={c.key} className={numericColumn(c.key) ? "jc-cell-num" : undefined}>
                    {c.key === "tokens" ? (
                      <Tooltip content="顺序：输入 / 缓存命中 / 缓存写入（~ 为网关估算，1h>0 时显示 5m+1h）/ 输出；+img 为输出图片数"><span>tokens</span></Tooltip>
                    ) : c.label}
                  </Table.ColumnHeaderCell>
                ))}
              </Table.Row>
            </Table.Header>
            <Table.Body>
              {logs.map((l: any) => {
                const isExpanded = expandedRows.has(l.id);
                return (
                <Fragment key={l.id}>
                <Table.Row
                  onClick={() => toggleExpanded(l.id)}
                  style={{ cursor: "pointer" }}
                  className={isExpanded ? "jc-admin-log-row-active is-expanded" : undefined}
                  aria-expanded={isExpanded}
                >
                  <Table.Cell className="jc-cell-expand"><ChevronRight size={14} /></Table.Cell>
                  {visibleColumns.map((c) => (
                    <Table.Cell key={c.key} className={cellClass(c.key)}>{renderLogCell(c.key, l)}</Table.Cell>
                  ))}
                </Table.Row>
                {isExpanded && (
                  <Table.Row className="jc-admin-log-detail-row jc-list-detail-row">
                    <Table.Cell colSpan={colSpan}>
                      <RequestDetailPanel log={l} />
                    </Table.Cell>
                  </Table.Row>
                )}
                </Fragment>
                );
              })}
              {isLoading && <TableSkeletonRows cols={colSpan} rows={8} />}
              {!isLoading && logs.length === 0 && (
                <Table.Row className="jc-list-detail-row">
                  <Table.Cell colSpan={colSpan}>
                    <div className="jc-list-empty">
                      <div className="jc-list-empty-title">没有匹配的记录</div>
                      {chips.length > 0 && (
                        <>
                          <div className="jc-list-empty-hint">当前有 {chips.length} 个筛选条件生效</div>
                          <Box mt="2"><Button size="1" variant="soft" color="gray" onClick={reset}>清空筛选</Button></Box>
                        </>
                      )}
                    </div>
                  </Table.Cell>
                </Table.Row>
              )}
            </Table.Body>
          </Table.Root>
          </div>
        </Box>
        <ListPager
          page={page}
          pageSize={PAGE_SIZE}
          total={total}
          loaded={logs.length}
          hasPrev={hasPrev}
          hasNext={hasNext}
          onPage={(p) => setOffset(p * PAGE_SIZE)}
          disabled={isFetching}
        />
      </Card>
    </AdminPage>
  );
}

const NUMERIC_COLUMNS = new Set(["tokens", "cost", "http", "retry", "ttfb", "ttft", "latency"]);
const MONO_COLUMNS = new Set(["request_id", "time", "model", "ip"]);
function numericColumn(key: string) { return NUMERIC_COLUMNS.has(key); }
function cellClass(key: string): string | undefined {
  if (NUMERIC_COLUMNS.has(key)) return "jc-cell-num";
  if (MONO_COLUMNS.has(key)) return "jc-cell-mono";
  return undefined;
}

const clip = (max: number): React.CSSProperties => ({
  maxWidth: max, display: "inline-block", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", verticalAlign: "middle",
});

function tokensText(l: any): string {
  const cacheWrite = l.cache_creation_tokens_in
    ? `${l.cache_write_estimated ? "~" : ""}${(l.cache_creation_1h_tokens_in ?? 0) > 0 ? `${l.cache_creation_5m_tokens_in ?? 0}+${l.cache_creation_1h_tokens_in}` : l.cache_creation_tokens_in}`
    : "-";
  let s = `${l.tokens_in}/${l.cached_tokens_in ?? 0}/${cacheWrite}/${l.tokens_out}`;
  if (l.image_tokens_in || l.image_tokens_out) s += ` +imgtok ${l.image_tokens_in ?? 0}/${l.image_tokens_out ?? 0}`;
  if (l.audio_tokens_in || l.audio_tokens_out) s += ` +audtok ${l.audio_tokens_in ?? 0}/${l.audio_tokens_out ?? 0}`;
  if (l.output_images) s += ` +img ${l.output_images}`;
  return s;
}

function renderLogCell(key: string, l: any): ReactNode {
  const dash = <Text size="1" color="gray">—</Text>;
  switch (key) {
    case "request_id":
      return <CopyableId value={l.request_id || l.id} display={shortId(l.request_id || l.id)} label="点击复制请求ID" />;
    case "time":
      return new Date(l.created_at).toLocaleString([], { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" });
    case "user":
      return (
        <Tooltip content={`${l.user_email ? l.user_email + " · " : ""}${l.user_id}`}>
          <span style={{ ...clip(150), cursor: "help" }}>
            {l.user_nickname || l.user_name || l.user_email || `${l.user_id.slice(0, 8)}…`}
          </span>
        </Tooltip>
      );
    case "api_key":
      return l.api_key_name
        ? <Tooltip content={l.api_key_id ?? ""}><span style={{ cursor: "help" }}>{l.api_key_name}</span></Tooltip>
        : dash;
    case "endpoint":
      return <span className="jc-cell-muted">{l.endpoint}</span>;
    case "model":
      return (
        <Flex align="center" gap="2">
          <UsageSubjectIcon endpoint={l.endpoint} model={l.model} size={14} />
          <span style={clip(220)}>{usageSubjectName(l.endpoint, l.model)}</span>
        </Flex>
      );
    case "tokens":
      return tokensText(l);
    case "cost":
      return formatAmount(l.cost_final, { decimals: "preserve" }) || "0";
    case "status": {
      const badge = <Badge size="1" variant="soft" color={statusColor(l.status)}>{statusLabel(l.status)}</Badge>;
      return l.error_message || (l.provider_chain?.length ?? 0) > 1 || (l.attempts?.length ?? 0) > 0
        ? <Tooltip content={<AttemptsTooltip log={l} />}><span style={{ cursor: "help" }}>{badge}</span></Tooltip>
        : badge;
    }
    case "anomaly":
      return <AnomalyCell log={l} />;
    case "http":
      return l.http_status;
    case "retry":
      return l.retry_count ?? 0;
    case "provider":
      return attemptProviderChain(l) ? (
        <Tooltip content={<AttemptsTooltip log={l} />}>
          <span style={{ ...clip(170), cursor: "help" }}>{attemptProviderChain(l)}</span>
        </Tooltip>
      ) : dash;
    case "stream":
      return l.stream ? <Badge size="1" color="blue" variant="soft">SSE</Badge> : <Badge size="1" color="gray" variant="soft">同步</Badge>;
    case "reasoning":
      return l.reasoning_effort ? <Badge size="1" variant="soft">{l.reasoning_effort}</Badge> : dash;
    case "ttfb":
      return l.ttfb_ms ? `${l.ttfb_ms}ms` : "—";
    case "ttft":
      return l.ttft_ms ? `${l.ttft_ms}ms` : "—";
    case "latency":
      return `${l.latency_ms}ms`;
    case "stages":
      return l.latency_detail ? (
        <Tooltip content={<LatencyTooltip log={l} />}>
          <span style={{ ...clip(160), cursor: "help" }}>{latencyText(l.latency_detail)}</span>
        </Tooltip>
      ) : dash;
    case "ip":
      return l.ip || "—";
    case "ua":
      return <Tooltip content={l.user_agent || ""}><span style={clip(140)}>{l.user_agent || "—"}</span></Tooltip>;
    default:
      return null;
  }
}
