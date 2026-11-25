// Account health/badge primitives shared by the Accounts list and the account
// detail dialog. All of this is admin-only: account identity never appears on a
// user-facing surface.
import { useEffect, useState } from "react";
import { Badge, Flex, Text, Tooltip } from "@radix-ui/themes";
import type { Account, AccountProtocol, PoolMode } from "../../api/endpoints";

export const POOL_MODE_LABEL: Record<string, string> = {
  balanced: "均衡",
  conservative: "保守",
  lenient: "宽松（上游错误不写状态）",
  observe: "只观察",
};

export const ACCOUNT_PROTOCOL_LABEL: Record<AccountProtocol, string> = {
  chat_completions: "Chat 补全",
  responses: "Responses",
  messages: "Messages",
  web: "网页工具",
};

export function protocolLabel(p: string): string {
  return ACCOUNT_PROTOCOL_LABEL[p as AccountProtocol] ?? p;
}

export function accountLabel(a: Pick<Account, "name" | "id">): string {
  return a.name || `账号 ${a.id.slice(0, 8)}`;
}

// The account view says whether the vendor has a usage endpoint the backend
// can probe, so "查询额度" is only offered where it can work.
export function accountSupportsQuotaProbe(a: Account): boolean {
  return a.has_usage_endpoint === true;
}

export function fmtTime(value?: string | null): string {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString();
}

// fmtBalanceAmount trims float noise off a balance value
// (2934.63000002 → "2,934.63"). Values too small for 2 decimals keep their
// precision; unparseable input is shown as-is.
export function fmtBalanceAmount(amount: number | string): string {
  const v = Number(amount);
  if (!Number.isFinite(v)) return String(amount);
  if (v !== 0 && Math.abs(v) < 0.01) return String(amount);
  return v.toLocaleString(undefined, { minimumFractionDigits: 0, maximumFractionDigits: 2 });
}

// useNow drives the cooldown countdowns: the state machine stores absolute
// timestamps, so the UI re-renders once a second and lets them tick down.
export function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [active]);
  return now;
}

export function countdown(target: string | null | undefined, now: number): string {
  if (!target) return "";
  const ms = new Date(target).getTime() - now;
  if (!Number.isFinite(ms)) return "";
  if (ms <= 0) return "即将恢复";
  const minutes = Math.floor(ms / 60000);
  if (minutes < 1) return `${Math.ceil(ms / 1000)} 秒后恢复`;
  if (minutes < 60) return `${minutes} 分钟后恢复`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours} 小时后恢复`;
  return `${Math.floor(hours / 24)} 天后恢复`;
}

const STATUS_MAP: Record<Account["status"], { color: "green" | "gray" | "red" | "amber"; label: string }> = {
  active: { color: "green", label: "正常" },
  disabled: { color: "gray", label: "已停用" },
  expired: { color: "red", label: "已过期" },
  error: { color: "amber", label: "异常" },
};

export function StatusBadge({ account }: { account: Account }) {
  const s = STATUS_MAP[account.status] ?? STATUS_MAP.active;
  return <Badge color={s.color} variant="soft">{s.label}</Badge>;
}

export function healthColor(health: string): "green" | "amber" | "red" | "gray" {
  if (health === "healthy") return "green";
  if (health === "degraded") return "amber";
  if (health === "unhealthy") return "red";
  return "gray";
}

// AccountHealthBadges is the account state machine's verdict, read-only: which
// cooldown is active (and when it lifts), whether an operator parked the
// account, and the remaining-quota windows we last observed.
export function AccountHealthBadges({ account, now }: { account: Account; now: number }) {
  const badges: { label: string; color: "amber" | "red" | "gray" | "blue"; hint: string }[] = [];
  if (!account.schedulable) {
    badges.push({ label: "已暂停调度", color: "gray", hint: "管理员暂停：不会参与调度，状态保持不变" });
  }
  if (account.status === "error") {
    badges.push({
      label: "已禁用",
      color: "red",
      hint: `上游拒绝后移出调度：${account.last_error_class || "unknown"}。修复后点「恢复」重新启用。`,
    });
  }
  if (account.rate_limit_reset_at && new Date(account.rate_limit_reset_at).getTime() > now) {
    badges.push({
      label: `429 限流 · ${countdown(account.rate_limit_reset_at, now)}`,
      color: "amber",
      hint: "上游按窗口限流，到点自动恢复（时间取自上游 reset 头）。",
    });
  }
  if (account.overload_until && new Date(account.overload_until).getTime() > now) {
    badges.push({
      label: `过载 · ${countdown(account.overload_until, now)}`,
      color: "amber",
      hint: "上游返回过载/5xx，短暂降温。",
    });
  }
  if (account.temp_unschedulable_until && new Date(account.temp_unschedulable_until).getTime() > now) {
    badges.push({
      label: `临时不可用 · ${countdown(account.temp_unschedulable_until, now)}`,
      color: "amber",
      hint: account.temp_unschedulable_reason || "临时移出调度，到点自动恢复。",
    });
  }
  const modelCooldowns = Object.entries(account.model_cooldowns ?? {}).filter(
    ([, v]) => (v?.until_ms ?? 0) > now,
  );
  if (modelCooldowns.length > 0) {
    badges.push({
      label: `${modelCooldowns.length} 个模型冷却`,
      color: "amber",
      hint: `仅这些模型被跳过，账号其余模型仍可用：${modelCooldowns.map(([m]) => m).join("、")}`,
    });
  }
  if (account.cooldown_active) {
    badges.push({
      label: `冷却 ${account.cooldown_ttl_seconds ? `${account.cooldown_ttl_seconds}s` : ""}`.trim(),
      color: "amber",
      hint: account.cooldown_reason ? `冷却原因：${account.cooldown_reason}` : "运行时冷却中",
    });
  }
  if (account.consecutive_failures >= 3 && account.schedulable_now) {
    badges.push({
      label: `连续失败 ${account.consecutive_failures}`,
      color: "amber",
      hint: `最近失败类型：${account.last_error_class || "unknown"}`,
    });
  }

  return (
    <Flex gap="1" align="center" wrap="wrap">
      {badges.length === 0 ? (
        <Text size="1" color="gray">可调度</Text>
      ) : (
        badges.map((b) => (
          <Tooltip key={b.label} content={b.hint}>
            <Badge color={b.color} variant="surface">{b.label}</Badge>
          </Tooltip>
        ))
      )}
    </Flex>
  );
}

// QuotaWindows renders the observed quota window progress (5h / 7d ...).
export function QuotaWindows({ account }: { account: Account }) {
  const windows = account.quota_snapshot?.windows ?? [];
  const balance = account.quota_snapshot?.balance;
  if (windows.length === 0 && !balance) {
    return <Text size="1" color="gray">—</Text>;
  }
  return (
    <Flex gap="2" align="center" wrap="wrap">
      {balance && (
        <Tooltip content={balance.currency ? `单位 ${balance.currency}` : "剩余额度"}>
          <Badge color="green" variant="soft">
            剩余 {fmtBalanceAmount(balance.amount)}{balance.currency ? ` ${balance.currency}` : ""}
          </Badge>
        </Tooltip>
      )}
      {windows.map((w) => {
        const pct = Math.max(0, Math.min(100, w.used_percent ?? 0));
        return (
          <Tooltip
            key={w.name}
            content={`${w.name} 窗口已用 ${pct.toFixed(0)}%${w.reset_at ? ` · ${new Date(w.reset_at * 1000).toLocaleString()} 重置` : ""}`}
          >
            <Flex align="center" gap="1">
              <Text size="1" color="gray">{w.name}</Text>
              <Badge color={pct >= 95 ? "red" : pct >= 80 ? "amber" : "green"} variant="soft">
                {pct.toFixed(0)}%
              </Badge>
            </Flex>
          </Tooltip>
        );
      })}
      {account.quota_snapshot?.source && (
        <Text size="1" color="gray">
          {account.quota_snapshot.source === "probe" ? "主动查询" : "响应头采样"}
        </Text>
      )}
    </Flex>
  );
}

export const POOL_MODE_OPTIONS: { value: PoolMode; label: string }[] = [
  { value: "", label: "跟随站点默认" },
  { value: "balanced", label: "均衡：按错误类型冷却，明确的失效才禁用" },
  { value: "conservative", label: "保守：冷却更长、更早禁用（稀缺账号）" },
  { value: "lenient", label: "宽松：上游错误不写状态，同账号重试（大量可互换的 key）" },
  { value: "observe", label: "只观察：只记录不撤下调度" },
];
