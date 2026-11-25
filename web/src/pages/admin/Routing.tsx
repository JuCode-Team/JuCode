import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Badge, Box, Button, Card, Code, Dialog, Flex, Heading, Table, Text, TextField } from "@radix-ui/themes";
import {
  adminApi,
  type ObservabilityWindow,
  type ProviderAttemptRow,
  type RoutingBoardProvider,
  type RoutingCooldown,
  type RoutingPreviewGroup,
  type RoutingSkipReason,
} from "../../api/endpoints";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

const SKIP_LABEL: Record<RoutingSkipReason, string> = {
  no_providers: "无可用渠道配置",
  all_providers_cooling: "整组渠道均在冷却，已跳过",
  billing_source_mismatch: "计费来源不同，回退不跨组",
};

function healthColor(h: string): "green" | "amber" | "red" | "gray" {
  switch (h) {
    case "healthy": return "green";
    case "degraded": return "amber";
    case "unhealthy": return "red";
    default: return "gray";
  }
}

function CooldownCell({ cd }: { cd: RoutingCooldown }) {
  if (!cd?.active) return <Text size="1" color="gray">—</Text>;
  return (
    <Flex direction="column" gap="0">
      <Badge size="1" color="red" variant="soft">冷却中 · {cd.ttl_seconds ?? 0}s</Badge>
      {cd.reason ? <Text size="1" color="gray">{cd.reason}</Text> : null}
    </Flex>
  );
}

function formatSeconds(s: number): string {
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}min`;
  return `${Math.round(s / 360) / 10}h`;
}

// Fault rate tone. The number is always printed; colour is only an accent.
function faultTone(rate: number | null): "green" | "iris" | "amber" | "red" | "gray" {
  if (rate == null) return "gray";
  if (rate >= 0.5) return "red";
  if (rate >= 0.2) return "amber";
  if (rate > 0) return "iris";
  return "green";
}

/* Attempt-level per-channel failure rate. The provider leaderboard counts a
   request against the channel that finally served it, so a channel that fails
   every attempt and is rescued by failover looks nearly idle there; these
   counts come from provider_chain and include every attempt it handled. */
function AttemptsCard() {
  const [window, setWindow] = useState<ObservabilityWindow>("1h");
  const q = useQuery({
    queryKey: ["admin", "observability", "provider-attempts", window],
    queryFn: () => adminApi.observabilityProviderAttempts({ window, include_synthetic: false }),
    refetchInterval: 30000,
  });
  const rows: ProviderAttemptRow[] = q.data?.rows ?? [];
  return (
    <Card mt="4">
      <Flex justify="between" align="center" mb="2" wrap="wrap" gap="2">
        <Heading size="3">渠道 attempt 级失败率</Heading>
        <Flex gap="1">
          {(["1h", "6h", "24h"] as ObservabilityWindow[]).map((w) => (
            <Button key={w} size="1" variant={window === w ? "solid" : "soft"} onClick={() => setWindow(w)}>
              {w}
            </Button>
          ))}
        </Flex>
      </Flex>
      <Text size="1" color="gray" as="p" mb="2">
        按 provider_chain 统计每个渠道实际经手的尝试次数与故障数，与"最终由谁服务"无关。
        故障 = provider_failure / rate_limit / timeout / auth；client_canceled、user_request 等用户侧结果不计入分子分母。
      </Text>
      <div className="jc-local-table-scroll">
        <Table.Root size="1" variant="surface">
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>渠道</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>尝试</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>成功</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>渠道故障</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>故障率</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>分类明细</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {rows.map((r) => (
              <Table.Row key={r.provider_id}>
                <Table.Cell><Text size="2" weight="medium">{r.name}</Text></Table.Cell>
                <Table.Cell><Text size="1" className="jc-num">{r.attempts}</Text></Table.Cell>
                <Table.Cell><Text size="1" className="jc-num">{r.ok}</Text></Table.Cell>
                <Table.Cell><Text size="1" className="jc-num">{r.provider_faults}</Text></Table.Cell>
                <Table.Cell>
                  {r.provider_fault_rate == null ? (
                    <Text size="1" color="gray">—</Text>
                  ) : (
                    <Badge size="1" color={faultTone(r.provider_fault_rate)} variant="soft">
                      {(r.provider_fault_rate * 100).toFixed(1)}%
                    </Badge>
                  )}
                </Table.Cell>
                <Table.Cell>
                  <Text size="1" color="gray">
                    {Object.entries(r.by_class ?? {})
                      .filter(([k]) => k !== "ok")
                      .map(([k, v]) => `${k} ${v}`)
                      .join(" · ") || "—"}
                  </Text>
                </Table.Cell>
              </Table.Row>
            ))}
            {rows.length === 0 && (
              <Table.Row>
                <Table.Cell colSpan={6}>
                  <Text size="1" color="gray">{q.isLoading ? "加载中…" : "该窗口内没有带 provider_chain 的请求"}</Text>
                </Table.Cell>
              </Table.Row>
            )}
          </Table.Body>
        </Table.Root>
      </div>
    </Card>
  );
}

/* One candidate group in the auto-route chain: whether it won, and if not
   why it was passed over. Seeing the skipped groups is the point — the old
   preview only ever rendered the winner, which is why a group stuck on a
   cooling channel looked like the only option. */
function PreviewGroup({ group }: { group: RoutingPreviewGroup }) {
  return (
    <Box>
      <Flex align="center" gap="2" mb="2" wrap="wrap">
        <Badge size="1" color={group.chosen ? "green" : "gray"} variant={group.chosen ? "solid" : "soft"}>
          {group.chosen ? "命中" : "备选"}
        </Badge>
        <Code>{group.name}</Code>
        <Text size="1" color="gray">{group.rate_multiplier}×</Text>
        <Text size="1" color="gray">
          {group.billing_source === "plan_only" ? "仅订阅" : "余额"}
        </Text>
        {group.skip_reason && (
          <Text size="1" color="amber">{SKIP_LABEL[group.skip_reason] ?? group.skip_reason}</Text>
        )}
      </Flex>
      <div className="jc-local-table-scroll">
        <Table.Root size="1" variant="surface">
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>#</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>渠道</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>健康</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>优先级</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>权重</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>有效权重</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>上游模型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>冷却</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>可用</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {group.providers.map((p, i) => (
              <Table.Row key={p.id}>
                <Table.Cell><Text size="1" color="gray">{i + 1}</Text></Table.Cell>
                <Table.Cell><Text size="2">{p.name}</Text></Table.Cell>
                <Table.Cell><Badge size="1" color={healthColor(p.health)} variant="soft">{p.health}</Badge></Table.Cell>
                <Table.Cell><Text size="1">{p.priority}</Text></Table.Cell>
                <Table.Cell><Text size="1">{p.weight}</Text></Table.Cell>
                <Table.Cell><Text size="1" weight="medium">{p.effective_weight}</Text></Table.Cell>
                <Table.Cell><Text size="1" color="gray">{p.upstream_model}</Text></Table.Cell>
                <Table.Cell><CooldownCell cd={p.cooldown} /></Table.Cell>
                <Table.Cell>
                  <Badge size="1" color={p.eligible ? "green" : "red"} variant="soft">{p.eligible ? "是" : "否"}</Badge>
                </Table.Cell>
              </Table.Row>
            ))}
            {group.providers.length === 0 && (
              <Table.Row><Table.Cell colSpan={9}><Text size="1" color="gray">无候选渠道</Text></Table.Cell></Table.Row>
            )}
          </Table.Body>
        </Table.Root>
      </div>
    </Box>
  );
}

export default function RoutingPage() {
  const qc = useQueryClient();
  // Live cooldown / health board, auto-refreshing.
  const board = useQuery({
    queryKey: ["admin", "routing", "board"],
    queryFn: () => adminApi.routingBoard(),
    refetchInterval: 5000,
  });
  const providers: RoutingBoardProvider[] = board.data?.providers ?? [];
  const cooling = providers.filter((p) => p.cooldown?.active);
  const unhealthy = providers.filter((p) => p.health === "unhealthy" || p.health === "degraded");

  // Manual cooldown override.
  const [cooldownTarget, setCooldownTarget] = useState<RoutingBoardProvider | null>(null);
  const [seconds, setSeconds] = useState("300");
  const [reason, setReason] = useState("");
  const invalidateBoard = () => qc.invalidateQueries({ queryKey: ["admin", "routing", "board"] });
  const setCd = useMutation({
    mutationFn: ({ id, secs, why }: { id: string; secs: number; why: string }) =>
      adminApi.setAccountCooldown(id, { seconds: secs, reason: why || undefined }),
    onSuccess: () => { setCooldownTarget(null); invalidateBoard(); },
  });
  const clearCd = useMutation({
    mutationFn: (id: string) => adminApi.clearAccountCooldown(id),
    onSuccess: invalidateBoard,
  });
  const openCooldown = (p: RoutingBoardProvider) => {
    setSeconds("300");
    setReason("");
    setCd.reset();
    setCooldownTarget(p);
  };
  const submitCooldown = () => {
    if (!cooldownTarget) return;
    const secs = Number.parseInt(seconds, 10);
    if (!Number.isFinite(secs) || secs <= 0) return;
    setCd.mutate({ id: cooldownTarget.id, secs, why: reason.trim() });
  };

  // Routing preview for a specific model.
  const [modelInput, setModelInput] = useState("");
  const [model, setModel] = useState("");
  const preview = useQuery({
    queryKey: ["admin", "routing", "preview", model],
    queryFn: () => adminApi.routingPreview(model),
    enabled: !!model,
  });

  return (
    <AdminPage
      title="路由 / 渠道实时"
      description="渠道当前健康与冷却状态、以及给定模型当前会路由到哪些渠道。用于故障时快速排查。"
    >
      <AdminMetricGrid>
        <AdminMetricCard label="渠道总数" value={providers.length} detail="全部 provider" tone="iris" />
        <AdminMetricCard label="冷却中" value={cooling.length} detail="runtime cooldown" tone="red" />
        <AdminMetricCard label="非健康" value={unhealthy.length} detail="degraded / unhealthy" tone="amber" />
        <AdminMetricCard label="已启用" value={providers.filter((p) => p.enabled).length} detail="enabled" />
      </AdminMetricGrid>

      <Card mb="4">
        <Flex justify="between" align="center" mb="2">
          <Heading size="3">渠道冷却看板</Heading>
          <Button size="1" variant="soft" onClick={() => board.refetch()} disabled={board.isFetching}>
            {board.isFetching ? "刷新中…" : "刷新"}
          </Button>
        </Flex>
        <div className="jc-local-table-scroll">
        <Table.Root size="1" variant="surface">
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>渠道</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>类型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>启用</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>健康</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>冷却状态</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>最近健康更新</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>操作</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {providers.map((p) => (
              <Table.Row key={p.id}>
                <Table.Cell><Text size="2" weight="medium">{p.name}</Text></Table.Cell>
                <Table.Cell><Text size="1" color="gray">{p.kind}</Text></Table.Cell>
                <Table.Cell>
                  <Badge size="1" color={p.enabled ? "green" : "gray"} variant="soft">{p.enabled ? "是" : "否"}</Badge>
                </Table.Cell>
                <Table.Cell><Badge size="1" color={healthColor(p.health)} variant="soft">{p.health}</Badge></Table.Cell>
                <Table.Cell><CooldownCell cd={p.cooldown} /></Table.Cell>
                <Table.Cell>
                  <Text size="1" color="gray">{p.last_health_at ? new Date(p.last_health_at).toLocaleString() : "—"}</Text>
                </Table.Cell>
                <Table.Cell>
                  {p.cooldown?.active ? (
                    <Button size="1" variant="soft" color="gray" disabled={clearCd.isPending}
                      onClick={() => clearCd.mutate(p.id)}>
                      解除冷却
                    </Button>
                  ) : (
                    <Button size="1" variant="soft" color="red" onClick={() => openCooldown(p)}>
                      设冷却
                    </Button>
                  )}
                </Table.Cell>
              </Table.Row>
            ))}
            {providers.length === 0 && (
              <Table.Row><Table.Cell colSpan={7}><Text size="1" color="gray">{board.isLoading ? "加载中…" : "无渠道"}</Text></Table.Cell></Table.Row>
            )}
          </Table.Body>
        </Table.Root>
        </div>
        {clearCd.isError && (
          <Text size="1" color="red" as="p" mt="2">{(clearCd.error as Error)?.message || "解除失败"}</Text>
        )}
      </Card>

      <AttemptsCard />

      <Dialog.Root open={!!cooldownTarget} onOpenChange={(o) => { if (!o) setCooldownTarget(null); }}>
        <Dialog.Content style={{ maxWidth: 440 }}>
          <Dialog.Title>手动设置冷却</Dialog.Title>
          <Dialog.Description size="2" color="gray">
            <Code>{cooldownTarget?.name}</Code> 立即停止接收新请求，到期自动恢复。
            只影响路由，不改渠道配置，也不清除健康统计。
          </Dialog.Description>
          <Flex direction="column" gap="3" mt="4">
            <Flex gap="2" wrap="wrap">
              {[300, 900, 3600].map((s) => (
                <Button key={s} size="1" variant={seconds === String(s) ? "solid" : "soft"}
                  onClick={() => setSeconds(String(s))}>
                  {formatSeconds(s)}
                </Button>
              ))}
            </Flex>
            <label>
              <Text size="1" color="gray" as="div" mb="1">秒数（上限 86400）</Text>
              <TextField.Root value={seconds} onChange={(e) => setSeconds(e.target.value)} />
            </label>
            <label>
              <Text size="1" color="gray" as="div" mb="1">原因（留空记 manual）</Text>
              <TextField.Root placeholder="manual" value={reason} onChange={(e) => setReason(e.target.value)} />
            </label>
            {setCd.isError && (
              <Text size="1" color="red">{(setCd.error as Error)?.message || "设置失败"}</Text>
            )}
            <Flex gap="2" justify="end">
              <Button variant="soft" color="gray" onClick={() => setCooldownTarget(null)}>取消</Button>
              <Button color="red" disabled={setCd.isPending} onClick={submitCooldown}>确认冷却</Button>
            </Flex>
          </Flex>
        </Dialog.Content>
      </Dialog.Root>

      <Card>
        <Heading size="3" mb="2">路由预览</Heading>
        <Text size="1" color="gray" as="p" mb="2">
          输入模型名，查看当前会路由到哪些渠道（按优先级 + 权重排序，叠加实时冷却）。effective_weight = 权重 × 健康因子，即加权随机实际使用的值。
        </Text>
        <Flex gap="2" mb="3" asChild>
          <form onSubmit={(e) => { e.preventDefault(); setModel(modelInput.trim()); }}>
            <Box style={{ minWidth: 280 }}>
              <TextField.Root placeholder="模型名，如 gpt-5.5" value={modelInput}
                onChange={(e) => setModelInput(e.target.value)} />
            </Box>
            <Button type="submit" disabled={!modelInput.trim()}>查询</Button>
          </form>
        </Flex>
        {preview.isError && <Text size="1" color="red">{(preview.error as Error)?.message || "查询失败"}</Text>}
        {preview.data && (
          <>
            <Text size="1" color="gray" as="p" mb="3">
              命中组：<Code>{preview.data.group}</Code> · 候选组 {preview.data.groups.length} 个
              （按倍率从低到高，自动路由会跳过整组渠道都在冷却的组）
            </Text>
            <Flex direction="column" gap="4">
              {preview.data.groups.map((g) => <PreviewGroup key={g.id} group={g} />)}
            </Flex>
          </>
        )}
      </Card>
    </AdminPage>
  );
}
