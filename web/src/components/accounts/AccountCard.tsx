// AccountCard: one account as a compact card in the card-flow layout.
// The leading icon and accent colour distinguish the account kind —
// provider-definition accounts in iris, plain API keys in gray — while the
// health badge carries the health colour.
import { Badge, Box, Checkbox, DropdownMenu, Flex, IconButton, Text, Tooltip } from "@radix-ui/themes";
import {
  FlaskConical, Gauge, KeyRound, MoreHorizontal, Pause, Pencil, Play, Puzzle, ShieldCheck, Trash2, Zap,
} from "lucide-react";
import type { Account } from "../../api/endpoints";
import {
  AccountHealthBadges, QuotaWindows, StatusBadge, accountLabel, accountSupportsQuotaProbe,
  fmtTime, healthColor, protocolLabel,
} from "./AccountHealthBadges";

export type AccountKind = "definition" | "api_key";

export function accountKind(a: Account): AccountKind {
  return a.definition_id ? "definition" : "api_key";
}

const KIND_META: Record<AccountKind, { icon: typeof KeyRound; color: string; label: string }> = {
  definition: { icon: Puzzle, color: "iris", label: "定义账号" },
  api_key: { icon: KeyRound, color: "gray", label: "API Key" },
};

const HEALTH_DOT: Record<string, string> = {
  green: "var(--green-9)",
  amber: "var(--amber-9)",
  red: "var(--red-9)",
  gray: "var(--gray-8)",
};

function hostOf(url?: string): string {
  if (!url) return "";
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

export interface AccountCardActions {
  onDetail: (a: Account) => void;
  onRecover: (a: Account) => void;
  onToggleSchedulable: (a: Account) => void;
  onTest: (a: Account) => void;
  onClearCooldown: (a: Account) => void;
  onRefreshQuota: (a: Account) => void;
  onJoinPool: (a: Account) => void;
  onDelete: (a: Account) => void;
  pending: {
    recover?: boolean;
    schedulable?: boolean;
    clearCooldown?: boolean;
    refreshQuota?: boolean;
  };
}

export function AccountCard({
  account: a, now, selected, onSelectChange, actions,
}: {
  account: Account;
  now: number;
  selected: boolean;
  onSelectChange: (checked: boolean) => void;
  actions: AccountCardActions;
}) {
  const kind = KIND_META[accountKind(a)];
  const KindIcon = kind.icon;
  const dot = HEALTH_DOT[healthColor(a.health)] ?? HEALTH_DOT.gray;

  return (
    <Box
      p="3"
      style={{
        border: "1px solid var(--gray-5)",
        borderRadius: 10,
        background: "var(--color-panel-solid)",
        minWidth: 0,
      }}
    >
      <Flex align="start" gap="2">
        <Checkbox
          checked={selected}
          onCheckedChange={(v) => onSelectChange(v === true)}
          aria-label={`选择 ${accountLabel(a)}`}
          mt="1"
        />
        <Tooltip content={kind.label}>
          <Box mt="1" style={{ color: `var(--${kind.color}-9)` }}>
            <KindIcon size={15} />
          </Box>
        </Tooltip>
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Flex align="center" gap="2" wrap="wrap">
            <Tooltip content={`健康：${a.health}`}>
              <Box
                as="span"
                mt="1"
                style={{
                  width: 6,
                  height: 6,
                  borderRadius: "50%",
                  background: dot,
                  flexShrink: 0,
                }}
              />
            </Tooltip>
            <Text size="2" weight="medium" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
              {accountLabel(a)}
            </Text>
            <StatusBadge account={a} />
            <Badge color={healthColor(a.health)} variant="soft">{a.health}</Badge>
            {!a.has_credential && <Badge color="red">缺凭据</Badge>}
          </Flex>
          <Text size="1" color="gray" style={{ fontFamily: "ui-monospace, monospace", display: "block", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
            {hostOf(a.base_url)}
          </Text>
        </Box>
        <Flex gap="1" align="center" style={{ flexShrink: 0 }}>
          <Tooltip content="恢复（清除禁用、冷却与失败计数）">
            <IconButton size="1" variant="soft" color="grass" aria-label="恢复账号"
              loading={actions.pending.recover} onClick={() => actions.onRecover(a)}>
              <ShieldCheck size={13} />
            </IconButton>
          </Tooltip>
          <Tooltip content={a.schedulable ? "暂停调度" : "恢复调度"}>
            <IconButton size="1" variant="soft" color={a.schedulable ? "gray" : "amber"} aria-label="切换调度"
              loading={actions.pending.schedulable} onClick={() => actions.onToggleSchedulable(a)}>
              {a.schedulable ? <Pause size={13} /> : <Play size={13} />}
            </IconButton>
          </Tooltip>
          <Tooltip content="详情 / 编辑">
            <IconButton size="1" variant="soft" aria-label="账号详情" onClick={() => actions.onDetail(a)}>
              <Pencil size={13} />
            </IconButton>
          </Tooltip>
          <DropdownMenu.Root modal={false}>
            <DropdownMenu.Trigger>
              <IconButton size="1" variant="soft" color="gray" aria-label="更多操作">
                <MoreHorizontal size={13} />
              </IconButton>
            </DropdownMenu.Trigger>
            <DropdownMenu.Content size="1" align="end">
              <DropdownMenu.Item onSelect={() => actions.onTest(a)}>
                <FlaskConical size={13} /> 连通性测试
              </DropdownMenu.Item>
              {a.cooldown_active && (
                <DropdownMenu.Item disabled={actions.pending.clearCooldown} onSelect={() => actions.onClearCooldown(a)}>
                  <Zap size={13} /> 解除冷却
                </DropdownMenu.Item>
              )}
              {accountSupportsQuotaProbe(a) && (
                <DropdownMenu.Item disabled={actions.pending.refreshQuota} onSelect={() => actions.onRefreshQuota(a)}>
                  <Gauge size={13} /> 查询额度
                </DropdownMenu.Item>
              )}
              <DropdownMenu.Item onSelect={() => actions.onJoinPool(a)}>加入管理分组</DropdownMenu.Item>
              <DropdownMenu.Separator />
              <DropdownMenu.Item color="red" onSelect={() => actions.onDelete(a)}>
                <Trash2 size={13} /> 删除
              </DropdownMenu.Item>
            </DropdownMenu.Content>
          </DropdownMenu.Root>
        </Flex>
      </Flex>

      <Flex gap="1" wrap="wrap" mt="2">
        {(a.native_protocols?.length ? a.native_protocols : ["chat_completions" as const]).map((p) => (
          <Badge key={p} variant="soft" color="blue" size="1">{protocolLabel(p)}</Badge>
        ))}
        {a.allow_conversion && <Badge variant="soft" color="iris" size="1">方言转换</Badge>}
      </Flex>

      <Flex direction="column" gap="1" mt="2">
        <AccountHealthBadges account={a} now={now} />
        <QuotaWindows account={a} />
        {a.last_error && (
          <Text size="1" color="red" style={{ whiteSpace: "normal" }}>{a.last_error}</Text>
        )}
      </Flex>

      <Flex align="center" justify="between" mt="2" pt="2" style={{ borderTop: "1px dashed var(--gray-5)" }}>
        <Text size="1" color="gray">
          P/W <span className="jc-num">{a.priority}/{a.weight}</span>
          {" · 并发 "}<span className="jc-num">{a.max_concurrency > 0 ? a.max_concurrency : "不限"}</span>
        </Text>
        <Text size="1" color="gray">{fmtTime(a.last_used_at)}</Text>
      </Flex>
    </Box>
  );
}
