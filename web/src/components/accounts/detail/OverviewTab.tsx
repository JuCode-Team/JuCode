// OverviewTab: 概览 — the account's live state: health badges, quota windows,
// and the recovery/scheduling actions.
import { useState } from "react";
import { Box, Button, Flex, Separator, Text, Tooltip } from "@radix-ui/themes";
import { useMutation } from "@tanstack/react-query";
import { FlaskConical, Gauge, Pause, Play, Power, ShieldCheck, Zap } from "lucide-react";
import { adminApi, type Account } from "../../../api/endpoints";
import { showToast } from "../../../lib/appToast";
import { ClearableTextField } from "../../ClearableTextField";
import { AccountTestDialog } from "../AccountTestDialog";
import { AccountHealthBadges, QuotaWindows, accountSupportsQuotaProbe, fmtTime } from "../AccountHealthBadges";
import { Field, Section } from "../AccountFormFields";

export function OverviewTab({
  account, now, invalidate,
}: {
  account: Account;
  now: number;
  invalidate: () => void;
}) {
  const [cooldownOpen, setCooldownOpen] = useState(false);
  const [testOpen, setTestOpen] = useState(false);
  const [reason, setReason] = useState("manual");
  const [ttl, setTtl] = useState("600");

  const recover = useMutation({
    mutationFn: () => adminApi.recoverAccount(account.id),
    onSuccess: () => { showToast({ title: "已恢复", message: "冷却、失败计数与禁用状态已清除" }); invalidate(); },
  });
  const toggleSchedulable = useMutation({
    mutationFn: (next: boolean) => adminApi.setAccountSchedulable(account.id, next),
    onSuccess: (_d, next) => { showToast({ title: next ? "已恢复调度" : "已暂停调度", message: next ? "该账号重新参与调度" : "该账号不再参与调度，状态不变" }); invalidate(); },
  });
  const refreshQuota = useMutation({
    mutationFn: () => adminApi.refreshAccountQuota(account.id),
    onSuccess: () => { showToast({ title: "已查询额度", message: "剩余额度窗口已更新" }); invalidate(); },
  });
  const clearCooldown = useMutation({
    mutationFn: () => adminApi.clearAccountCooldown(account.id),
    onSuccess: () => { showToast({ title: "已解除冷却", message: "该账号可以立即参与调度" }); invalidate(); },
  });
  const cooldown = useMutation({
    mutationFn: () => adminApi.setAccountCooldown(account.id, { reason: reason.trim() || "manual", seconds: parseInt(ttl) || 600 }),
    onSuccess: () => {
      showToast({ title: "已冷却", message: "该账号暂时退出调度" });
      setCooldownOpen(false);
      invalidate();
    },
  });
  const clearModelCooldowns = useMutation({
    mutationFn: () => adminApi.clearAccountModelCooldowns(account.id),
    onSuccess: () => { showToast({ title: "已清除", message: "模型级冷却已清空" }); invalidate(); },
  });

  return (
    <Flex direction="column" gap="4">
      <Section title="健康与额度">
        <AccountHealthBadges account={account} now={now} />
        <Flex gap="2" wrap="wrap" align="center" mt="2">
          <Tooltip content="清除禁用、冷却与失败计数">
            <Button size="1" variant="soft" color="grass" loading={recover.isPending} onClick={() => recover.mutate()}>
              <ShieldCheck size={13} /> 恢复
            </Button>
          </Tooltip>
          <Button size="1" variant="soft" color={account.schedulable ? "gray" : "amber"} loading={toggleSchedulable.isPending} onClick={() => toggleSchedulable.mutate(!account.schedulable)}>
            {account.schedulable ? <><Pause size={13} /> 暂停调度</> : <><Play size={13} /> 恢复调度</>}
          </Button>
          {accountSupportsQuotaProbe(account) ? (
            <Button size="1" variant="soft" loading={refreshQuota.isPending} onClick={() => refreshQuota.mutate()}>
              <Gauge size={13} /> 查询额度
            </Button>
          ) : (
            <Text size="1" color="gray">该提供商没有额度查询接口</Text>
          )}
          {account.cooldown_active && (
            <Button size="1" variant="soft" color="amber" loading={clearCooldown.isPending} onClick={() => clearCooldown.mutate()}>
              <Zap size={13} /> 解除冷却
            </Button>
          )}
          {account.model_cooldowns && Object.keys(account.model_cooldowns).length > 0 && (
            <Button size="1" variant="soft" loading={clearModelCooldowns.isPending} onClick={() => clearModelCooldowns.mutate()}>
              清除模型冷却
            </Button>
          )}
          <Button size="1" variant="soft" color="orange" onClick={() => setCooldownOpen((v) => !v)}>
            <Power size={13} /> 手动冷却
          </Button>
          <Tooltip content="指定协议与模型，向该账号自己的上游发一次真实请求">
            <Button size="1" variant="soft" onClick={() => setTestOpen(true)}>
              <FlaskConical size={13} /> 连通性测试
            </Button>
          </Tooltip>
        </Flex>

        {cooldownOpen && (
          <Flex gap="2" align="end" wrap="wrap" mt="2">
            <Box style={{ flex: 1, minWidth: 180 }}>
              <Field label="原因">
                <ClearableTextField value={reason} onChange={(e) => setReason(e.target.value)} />
              </Field>
            </Box>
            <Box style={{ width: 160 }}>
              <Field label="时长（秒）">
                <ClearableTextField type="number" value={ttl} onChange={(e) => setTtl(e.target.value)} />
              </Field>
            </Box>
            <Button loading={cooldown.isPending} onClick={() => cooldown.mutate()}>冷却</Button>
          </Flex>
        )}

        <Separator size="4" my="3" />
        <Flex gap="4" wrap="wrap">
          <Box>
            <Text size="1" color="gray" as="div">额度窗口</Text>
            <QuotaWindows account={account} />
          </Box>
          <Box>
            <Text size="1" color="gray" as="div">最后使用</Text>
            <Text size="2">{fmtTime(account.last_used_at)}</Text>
          </Box>
          <Box>
            <Text size="1" color="gray" as="div">连续失败</Text>
            <Text size="2">{account.consecutive_failures}</Text>
          </Box>
        </Flex>
        {account.last_error && (
          <Text size="1" color="red" as="div" mt="2" style={{ whiteSpace: "pre-wrap" }}>最近错误：{account.last_error}</Text>
        )}
      </Section>

      <AccountTestDialog account={account} open={testOpen} onOpenChange={setTestOpen} />
    </Flex>
  );
}
