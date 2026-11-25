// Topology: live routing topology drawn as a bipartite graph — model groups
// on the left, accounts (deduplicated across groups) on the right, one edge
// per (group, account) binding. Edge particles flow faster and denser as the
// edge's request rate climbs; edge and node colour follow the failure rate
// (green → amber → red, grey when idle or unavailable).
//
// Loads come from /v1/admin/observability/load-map (usage_logs grouped by
// group_id × provider_account_id) polled on a short cycle, so "load" is the
// last few minutes, not today.
//
// Folding keeps the graph readable when many accounts exist: fully
// unavailable accounts collapse into one "不可用 ×N" node, same-endpoint
// accounts into a "×N" stack, zero-traffic accounts into "空闲 ×N", and the
// low-traffic tail past the visible cap into "低流量 ×N". Every fold node
// expands in place on click.
//
// Node order uses a barycenter heuristic: accounts sort by the mean position
// of the groups they touch, then groups re-sort by the mean position of
// their accounts — two cheap passes that remove most edge crossings.
import { useMemo, useState } from "react";
import { Badge, Box, Callout, Checkbox, Flex, Select, Text, Tooltip } from "@radix-ui/themes";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Activity } from "lucide-react";
import {
  adminApi,
  type Account,
  type GroupAccount,
  type LoadMapWindow,
  type ModelGroup,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { AdminPage } from "../../components/AdminPage";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { AccountDetailDialog } from "../../components/accounts/AccountDetailDialog";
import { accountLabel, healthColor } from "../../components/accounts/AccountHealthBadges";

// ---- geometry ----
const CANVAS_W = 1120;
const GROUP_X = 20;
const GROUP_W = 200;
const ACCT_W = 216;
const ACCT_X = CANVAS_W - ACCT_W - 20;
const NODE_H = 38;
const ROW_GAP = 10;
const TOP_PAD = 16;
const BOTTOM_PAD = 16;

type Tone = "green" | "amber" | "red" | "gray";

const TONE_FILL: Record<Tone, string> = {
  green: "var(--green-9)",
  amber: "var(--amber-9)",
  red: "var(--red-9)",
  gray: "var(--gray-8)",
};
const TONE_PANEL: Record<Tone, string> = {
  green: "var(--green-3)",
  amber: "var(--amber-3)",
  red: "var(--red-3)",
  gray: "var(--gray-3)",
};

interface MemberStats {
  requests: number;
  failures: number;
  providerFaults: number;
  avgLatencyMs: number | null;
}

const ZERO_STATS: MemberStats = { requests: 0, failures: 0, providerFaults: 0, avgLatencyMs: null };

function edgeTone(stats: MemberStats): Tone {
  if (stats.requests === 0) return "gray";
  const rate = stats.failures / stats.requests;
  return rate >= 0.1 ? "red" : rate >= 0.02 ? "amber" : "green";
}

function isUnavailable(m: GroupAccount): boolean {
  return m.enabled === false || m.status !== "active" || !m.schedulable_now || !m.has_credential;
}

function hostOf(url?: string): string {
  if (!url) return "";
  try {
    return new URL(url).hostname;
  } catch {
    return url;
  }
}

function statsSum(list: MemberStats[]): MemberStats {
  const out = { ...ZERO_STATS };
  let latSum = 0, latN = 0;
  for (const s of list) {
    out.requests += s.requests;
    out.failures += s.failures;
    out.providerFaults += s.providerFaults;
    if (s.avgLatencyMs != null && s.requests > 0) {
      latSum += s.avgLatencyMs * s.requests;
      latN += s.requests;
    }
  }
  out.avgLatencyMs = latN > 0 ? Math.round(latSum / latN) : null;
  return out;
}

// ---- graph model ----

// One account as it appears across all groups. `edges` holds the per-group
// traffic so a shared account still draws one edge per binding.
interface AcctBundle {
  accountId: string;
  account?: Account;
  label: string;
  dead: boolean;
  host: string;
  edges: { groupId: string; stats: MemberStats }[];
  total: MemberStats;
}

interface RightNode {
  key: string;
  label: string;
  sub: string;
  tone: Tone;
  dashed?: boolean;
  account?: Account;
  fold?: { key: string; members: AcctBundle[] };
  edges: { groupId: string; stats: MemberStats }[];
  total: MemberStats;
}

interface GraphData {
  groups: { group: ModelGroup; total: MemberStats; memberCount: number }[];
  right: RightNode[];
}

interface FoldOptions {
  dead: boolean;
  dup: boolean;
  idle: boolean;
  maxVisible: number;
}

function accountNode(b: AcctBundle): RightNode {
  return {
    key: `acct:${b.accountId}`,
    label: b.label,
    sub: b.total.requests > 0
      ? `${b.total.requests} 请求${b.total.avgLatencyMs != null ? ` · ${b.total.avgLatencyMs}ms` : ""}`
      : b.dead ? "不可用" : "无流量",
    tone: b.dead ? "gray" : edgeTone(b.total),
    dashed: b.dead,
    account: b.account,
    edges: b.edges,
    total: b.total,
  };
}

function buildGraph(
  groups: ModelGroup[],
  bindings: Map<string, GroupAccount[]>,
  acctById: Map<string, Account>,
  statsByEdge: Map<string, MemberStats>,
  opts: FoldOptions,
  expanded: Set<string>,
): GraphData {
  const statsOf = (groupId: string, accountId: string) =>
    statsByEdge.get(`${groupId}|${accountId}`) ?? ZERO_STATS;

  // 1. Deduplicate members into account bundles.
  const bundles = new Map<string, AcctBundle>();
  const groupTotals = new Map<string, { total: MemberStats; memberCount: number }>();
  for (const g of groups) {
    const members = bindings.get(g.id) ?? [];
    groupTotals.set(g.id, { total: statsSum(members.map((m) => statsOf(g.id, m.account_id))), memberCount: members.length });
    for (const m of members) {
      let b = bundles.get(m.account_id);
      if (!b) {
        const acct = acctById.get(m.account_id);
        b = {
          accountId: m.account_id,
          account: acct,
          label: m.account_name || (acct ? accountLabel(acct) : m.account_id.slice(0, 8)),
          dead: true,
          host: hostOf(acct?.base_url ?? m.base_url),
          edges: [],
          total: { ...ZERO_STATS },
        };
        bundles.set(m.account_id, b);
      }
      b.edges.push({ groupId: g.id, stats: statsOf(g.id, m.account_id) });
      if (!isUnavailable(m)) b.dead = false;
    }
  }
  for (const b of bundles.values()) b.total = statsSum(b.edges.map((e) => e.stats));

  const foldEdges = (ms: AcctBundle[]) => {
    const byGroup = new Map<string, MemberStats[]>();
    for (const b of ms) for (const e of b.edges) byGroup.set(e.groupId, [...(byGroup.get(e.groupId) ?? []), e.stats]);
    return [...byGroup.entries()].map(([groupId, ss]) => ({ groupId, stats: statsSum(ss) }));
  };
  const foldNode = (key: string, reason: string, ms: AcctBundle[]): RightNode => {
    const total = statsSum(ms.map((b) => b.total));
    const open = expanded.has(key);
    return {
      key: `fold:${key}`,
      label: `${reason} ×${ms.length}`,
      sub: open ? "点击收起" : `点击展开 · ${total.requests} 请求`,
      tone: open ? "gray" : edgeTone(total),
      dashed: true,
      edges: foldEdges(ms),
      total,
      fold: { key, members: ms },
    };
  };

  // 2. Global fold pipeline.
  let stream = [...bundles.values()];
  const folds: RightNode[] = [];
  if (opts.dead) {
    const dead = stream.filter((b) => b.dead);
    if (dead.length > 0) folds.push(foldNode("dead", "不可用", dead));
    stream = stream.filter((b) => !b.dead);
  }
  if (opts.dup) {
    const buckets = new Map<string, AcctBundle[]>();
    for (const b of stream) {
      const k = b.host;
      buckets.set(k, [...(buckets.get(k) ?? []), b]);
    }
    const next: AcctBundle[] = [];
    for (const [k, ms] of buckets) {
      if (ms.length > 1) folds.push(foldNode(`dup:${ms.map((b) => b.accountId).sort().join(",")}`, ms[0].label || k, ms));
      else next.push(...ms);
    }
    stream = next;
  }
  if (opts.idle) {
    const idle = stream.filter((b) => b.total.requests === 0);
    if (idle.length > 0) folds.push(foldNode("idle", "空闲", idle));
    stream = stream.filter((b) => b.total.requests > 0);
  }
  stream.sort((a, b) => b.total.requests - a.total.requests);
  if (stream.length > opts.maxVisible) {
    folds.push(foldNode("low", "低流量", stream.slice(opts.maxVisible)));
    stream = stream.slice(0, opts.maxVisible);
  }

  // Expanded folds keep their header as the collapse handle and re-insert
  // their members as regular nodes right behind it.
  const expandedMembers = new Map<string, RightNode[]>();
  for (const f of folds) {
    if (f.fold && expanded.has(f.fold.key)) {
      expandedMembers.set(f.key, f.fold.members.map(accountNode));
    }
  }
  const right: RightNode[] = [...stream.map(accountNode), ...folds];

  // 3. Barycenter ordering: groups by mean right-node position, right nodes
  // by mean group position. Two passes is plenty for this size of graph.
  const groupIndex = new Map<string, number>();
  const rightIndex = new Map<string, number>();
  const bary = (idx: Map<string, number>, ids: string[]) =>
    ids.length === 0 ? Number.MAX_SAFE_INTEGER : ids.reduce((s, id) => s + (idx.get(id) ?? 0), 0) / ids.length;

  const orderedGroups = [...groups].sort(
    (a, b) => (groupTotals.get(b.id)?.total.requests ?? 0) - (groupTotals.get(a.id)?.total.requests ?? 0),
  );
  for (let pass = 0; pass < 2; pass++) {
    orderedGroups.forEach((g, i) => groupIndex.set(g.id, i));
    right.sort((a, b) => bary(groupIndex, a.edges.map((e) => e.groupId)) - bary(groupIndex, b.edges.map((e) => e.groupId)));
    right.forEach((n, i) => rightIndex.set(n.key, i));
    if (pass === 0) {
      orderedGroups.sort((a, b) => {
        const keysOf = (g: ModelGroup) => right.filter((n) => n.edges.some((e) => e.groupId === g.id)).map((n) => n.key);
        return bary(rightIndex, keysOf(a)) - bary(rightIndex, keysOf(b));
      });
    }
  }

  // Splice expanded members in directly behind their fold header.
  const finalRight: RightNode[] = [];
  for (const n of right) {
    finalRight.push(n);
    const extra = expandedMembers.get(n.key);
    if (extra) finalRight.push(...extra);
  }

  return {
    groups: orderedGroups.map((group) => ({ group, ...(groupTotals.get(group.id) ?? { total: { ...ZERO_STATS }, memberCount: 0 }) })),
    right: finalRight,
  };
}

// ---- page ----

export default function Topology() {
  const [window_, setWindow] = useState<LoadMapWindow>("15m");
  const [foldDead, setFoldDead] = useState(true);
  const [foldDup, setFoldDup] = useState(true);
  const [foldIdle, setFoldIdle] = useState(true);
  const [maxVisible, setMaxVisible] = useState(12);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [detail, setDetail] = useState<Account | null>(null);

  const groupsQ = useQuery({ queryKey: ["admin", "groups"], queryFn: adminApi.groups, refetchInterval: 30_000 });
  const accountsQ = useQuery({
    queryKey: ["admin", "accounts", "topology-all"],
    queryFn: () => adminApi.accounts({ limit: 1000 }),
    refetchInterval: 30_000,
  });
  const groups = useMemo(() => groupsQ.data ?? [], [groupsQ.data]);
  const bindingQs = useQueries({
    queries: groups.map((g) => ({
      queryKey: ["admin", "group-accounts", g.id],
      queryFn: () => adminApi.groupAccounts(g.id),
      refetchInterval: 30_000,
    })),
  });
  const loadQ = useQuery({
    queryKey: ["admin", "load-map", window_],
    queryFn: () => adminApi.observabilityLoadMap({ window: window_ }),
    refetchInterval: 10_000,
    placeholderData: (prev) => prev,
  });

  const acctById = useMemo(
    () => new Map((accountsQ.data ?? []).map((a) => [a.id, a])),
    [accountsQ.data],
  );
  const bindings = useMemo(() => {
    const m = new Map<string, GroupAccount[]>();
    groups.forEach((g, i) => m.set(g.id, bindingQs[i]?.data ?? []));
    return m;
  }, [groups, bindingQs]);
  const statsByEdge = useMemo(() => {
    const m = new Map<string, MemberStats>();
    for (const e of loadQ.data?.edges ?? []) {
      m.set(`${e.group_id}|${e.account_id}`, {
        requests: e.requests,
        failures: e.failures,
        providerFaults: e.provider_faults,
        avgLatencyMs: e.avg_latency_ms,
      });
    }
    return m;
  }, [loadQ.data]);

  const graph = useMemo(
    () => buildGraph(groups, bindings, acctById, statsByEdge, { dead: foldDead, dup: foldDup, idle: foldIdle, maxVisible }, expanded),
    [groups, bindings, acctById, statsByEdge, foldDead, foldDup, foldIdle, maxVisible, expanded],
  );

  // Accounts that belong to no group sit in the unbound strip — they carry
  // no edges and would only add dead space to the canvas.
  const boundIds = useMemo(() => {
    const ids = new Set<string>();
    for (const ms of bindings.values()) ms.forEach((m) => ids.add(m.account_id));
    return ids;
  }, [bindings]);
  const unbound = (accountsQ.data ?? []).filter((a) => !boundIds.has(a.id));

  if (groupsQ.isLoading || accountsQ.isLoading) return <RouteSkeleton />;
  const loadError = loadQ.error;

  // ---- layout: two evenly-spaced columns ----
  const rowH = NODE_H + ROW_GAP;
  const groupPos = new Map<string, { y: number; total: MemberStats; memberCount: number; group: ModelGroup }>();
  graph.groups.forEach((g, i) => groupPos.set(g.group.id, { y: TOP_PAD + i * rowH, total: g.total, memberCount: g.memberCount, group: g.group }));
  const rightPos = new Map<string, { y: number; node: RightNode }>();
  graph.right.forEach((n, i) => rightPos.set(n.key, { y: TOP_PAD + i * rowH, node: n }));
  const canvasH = Math.max(160, TOP_PAD + Math.max(graph.groups.length, graph.right.length) * rowH + BOTTOM_PAD);

  const windowMinutes = (loadQ.data?.window_seconds ?? 900) / 60;
  const edges: { key: string; d: string; stats: MemberStats; tone: Tone; rpm: number }[] = [];
  for (const n of graph.right) {
    const rp = rightPos.get(n.key);
    if (!rp) continue;
    for (const e of n.edges) {
      const gp = groupPos.get(e.groupId);
      if (!gp) continue;
      const x1 = GROUP_X + GROUP_W;
      const y1 = gp.y + NODE_H / 2;
      const x2 = ACCT_X;
      const y2 = rp.y + NODE_H / 2;
      const cx = (x1 + x2) / 2;
      edges.push({
        key: `${e.groupId}->${n.key}`,
        d: `M ${x1} ${y1} C ${cx} ${y1}, ${cx} ${y2}, ${x2} ${y2}`,
        stats: e.stats,
        tone: edgeTone(e.stats),
        rpm: e.stats.requests / windowMinutes,
      });
    }
  }

  const toggleFold = (key: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });
  };

  // Particle cadence: faster + more particles as the edge rate climbs.
  const particleSpec = (rpm: number) => {
    if (rpm <= 0) return { n: 0, dur: 0 };
    if (rpm < 2) return { n: 1, dur: 5 };
    if (rpm < 10) return { n: 2, dur: 3 };
    if (rpm < 40) return { n: 3, dur: 1.8 };
    return { n: 4, dur: 1.1 };
  };

  const controls = (
    <Flex gap="3" align="center" wrap="wrap" mb="3">
      <Select.Root value={window_} onValueChange={(v) => setWindow(v as LoadMapWindow)}>
        <Select.Trigger />
        <Select.Content>
          <Select.Item value="5m">最近 5 分钟</Select.Item>
          <Select.Item value="15m">最近 15 分钟</Select.Item>
          <Select.Item value="1h">最近 1 小时</Select.Item>
        </Select.Content>
      </Select.Root>
      <label style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}>
        <Checkbox checked={foldDead} onCheckedChange={(v) => setFoldDead(v === true)} />
        <Text size="1" color="gray">折叠不可用</Text>
      </label>
      <label style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}>
        <Checkbox checked={foldDup} onCheckedChange={(v) => setFoldDup(v === true)} />
        <Text size="1" color="gray">折叠同类账号</Text>
      </label>
      <label style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer" }}>
        <Checkbox checked={foldIdle} onCheckedChange={(v) => setFoldIdle(v === true)} />
        <Text size="1" color="gray">折叠零流量</Text>
      </label>
      <Flex align="center" gap="1">
        <Text size="1" color="gray">最多展开</Text>
        <Select.Root value={String(maxVisible)} onValueChange={(v) => setMaxVisible(Number(v))}>
          <Select.Trigger />
          <Select.Content>
            {[6, 12, 20, 40].map((n) => <Select.Item key={n} value={String(n)}>{n}</Select.Item>)}
          </Select.Content>
        </Select.Root>
      </Flex>
      <Text size="1" color="gray" ml="auto">
        {loadQ.isFetching ? "刷新中…" : "每 10 秒刷新"} · 线速=请求速率 · 颜色=失败率
      </Text>
    </Flex>
  );

  return (
    <AdminPage
      title="负载拓扑"
      description="分组与账号的实时连接图：每条边是一组绑定，光点流速对应请求速率，颜色对应失败率；不可用、空闲与同类账号自动折叠，点击折叠节点展开。"
      actions={<Badge color="green" variant="soft"><Activity size={12} /> 实时</Badge>}
    >
      {loadError && <Callout.Root color="red" mb="3"><Callout.Text>负载数据加载失败：{errMsg(loadError)}</Callout.Text></Callout.Root>}
      {controls}
      <Box style={{ overflowX: "auto", border: "1px solid var(--gray-5)", borderRadius: 10, background: "var(--color-panel-solid)" }}>
        <svg viewBox={`0 0 ${CANVAS_W} ${canvasH}`} width="100%" style={{ display: "block", minWidth: 860 }} role="img" aria-label="负载拓扑图">
          {edges.map((e) => {
            const spec = particleSpec(e.rpm);
            const width = 1.2 + Math.min(2.6, Math.log10(1 + e.stats.requests));
            return (
              <g key={e.key}>
                <path
                  d={e.d}
                  fill="none"
                  stroke={TONE_FILL[e.tone]}
                  strokeWidth={width}
                  strokeOpacity={e.stats.requests === 0 ? 0.2 : 0.5}
                  strokeDasharray={e.stats.requests === 0 ? "4 4" : undefined}
                >
                  <title>{`${e.stats.requests} 请求 · ${e.stats.failures} 失败${e.stats.avgLatencyMs != null ? ` · 均延 ${e.stats.avgLatencyMs}ms` : ""}`}</title>
                </path>
                {Array.from({ length: spec.n }).map((_, k) => (
                  <circle key={k} r={2.6} fill={TONE_FILL[e.tone]}>
                    <animateMotion dur={`${spec.dur}s`} begin={`${(-spec.dur * k) / spec.n}s`} repeatCount="indefinite" path={e.d} />
                  </circle>
                ))}
              </g>
            );
          })}

          {graph.groups.map(({ group, total, memberCount }) => {
            const gp = groupPos.get(group.id);
            if (!gp) return null;
            const tone: Tone = !group.enabled ? "gray" : edgeTone(total);
            return (
              <g key={group.id} transform={`translate(${GROUP_X},${gp.y})`}>
                <rect
                  width={GROUP_W}
                  height={NODE_H}
                  rx={8}
                  fill={TONE_PANEL[tone]}
                  stroke={TONE_FILL[tone]}
                  strokeWidth={1.1}
                  strokeDasharray={!group.enabled ? "4 3" : undefined}
                />
                <text x={10} y={16} fontSize={11.5} fontWeight={600} fill="var(--gray-12)">
                  {group.name.length > 18 ? group.name.slice(0, 17) + "…" : group.name}
                </text>
                <text x={10} y={30} fontSize={9.5} fill="var(--gray-11)">
                  {`${memberCount} 账号 · ${total.requests} 请求${total.failures > 0 ? ` · ${total.failures} 失败` : ""}`}
                </text>
              </g>
            );
          })}

          {graph.right.map((node) => {
            const rp = rightPos.get(node.key);
            if (!rp) return null;
            return (
              <g
                key={node.key}
                transform={`translate(${ACCT_X},${rp.y})`}
                style={{ cursor: node.fold || node.account ? "pointer" : "default" }}
                onClick={() => {
                  if (node.fold) toggleFold(node.fold.key);
                  else if (node.account) setDetail(node.account);
                }}
              >
                <rect
                  width={ACCT_W}
                  height={NODE_H}
                  rx={8}
                  fill={TONE_PANEL[node.tone]}
                  stroke={node.dashed ? "var(--gray-7)" : TONE_FILL[node.tone]}
                  strokeWidth={1}
                  strokeDasharray={node.dashed ? "5 3" : undefined}
                />
                <text x={10} y={16} fontSize={11} fontWeight={500} fill="var(--gray-12)">
                  {node.label.length > 24 ? node.label.slice(0, 23) + "…" : node.label}
                </text>
                <text x={10} y={30} fontSize={9.5} fill="var(--gray-11)">{node.sub}</text>
                {node.total.requests > 0 && (
                  <text x={ACCT_W - 8} y={16} fontSize={9.5} textAnchor="end" fill={TONE_FILL[node.tone]}>
                    {node.total.failures > 0 ? `${Math.round((node.total.failures / node.total.requests) * 100)}%` : "ok"}
                  </text>
                )}
              </g>
            );
          })}

          {graph.groups.length === 0 && (
            <text x={CANVAS_W / 2} y={canvasH / 2} textAnchor="middle" fontSize={13} fill="var(--gray-10)">
              还没有分组 — 去「分组」页创建
            </text>
          )}
        </svg>
      </Box>

      {unbound.length > 0 && (
        <Box mt="3">
          <Text size="1" color="gray" as="div" mb="1">未绑定到任何分组的账号（{unbound.length}）— 不参与路由</Text>
          <Flex gap="1" wrap="wrap">
            {unbound.map((a) => (
              <Tooltip key={a.id} content={`${a.base_url ?? ""} · ${a.health}`}>
                <Badge variant="soft" color={healthColor(a.health)} style={{ cursor: "pointer" }} onClick={() => setDetail(a)}>
                  {accountLabel(a)}
                </Badge>
              </Tooltip>
            ))}
          </Flex>
        </Box>
      )}

      <AccountDetailDialog
        account={detail}
        open={!!detail}
        onOpenChange={(open) => { if (!open) setDetail(null); }}
      />
    </AdminPage>
  );
}
