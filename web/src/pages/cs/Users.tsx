import "../../personal-responsive.css";
import { useEffect, useState } from "react";
import {
  Avatar,
  Badge,
  Box,
  Button,
  Card,
  Dialog,
  Flex,
  Select,
  Text,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { Wallet } from "lucide-react";
import { csApi, type TicketRow } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { PointAmount } from "../../components/PointAmount";
import { ClearableTextField } from "../../components/ClearableTextField";
import { PageHeader } from "../../components/PageHeader";

function displayName(u: any) {
  return u.nickname || u.username || u.email || u.id.slice(0, 8);
}

function initials(u: any): string {
  const s = (u.nickname || u.username || u.email || u.id || "").trim();
  return s ? s.slice(0, 2).toUpperCase() : "?";
}

function contactLine(u: any) {
  if (u.nickname && u.email) return u.email;
  return u.username || u.email || u.id;
}

// CS-facing user management. Mirrors a slimmed-down /admin/users with
// the only mutation endpoints CS has authority over: balance adjust and
// active-plan maintenance. API-key / role / status surfaces stay admin-only.
export default function CSUsers() {
  const [q, setQ] = useState("");
  const [params] = useSearchParams();
  const selectedUserParam = params.get("user");
  const [selectedId, setSelectedId] = useState<string | null>(selectedUserParam);
  const list = useQuery({ queryKey: ["cs", "users", q], queryFn: () => csApi.users({ q: q || undefined }) });

  useEffect(() => {
    if (selectedUserParam) setSelectedId(selectedUserParam);
  }, [selectedUserParam]);

  return (
    <Box className="jc-ticket-page" style={{ height: "calc(100dvh - 156px)", minHeight: 0, display: "flex", flexDirection: "column" }}>
      <PageHeader
        title="用户管理"
        description="搜索用户 · 调整余额 · 套餐操作"
      />
      <Flex className="jc-ticket-layout" gap="3" align="stretch" wrap="wrap" style={{ flex: 1, minHeight: 0 }}>
        <Box className="jc-ticket-list-pane" style={{ flex: "0 0 420px", minWidth: 0, display: "flex", minHeight: 0 }}>
          <Card className="jc-ticket-list-card jc-ticket-detail-card" style={{ flex: 1, minHeight: 0, display: "flex", flexDirection: "column" }}>
            <Flex direction="column" gap="2" style={{ flex: 1, minHeight: 0 }}>
              <ClearableTextField clearLabel="清空搜索" placeholder="搜索邮箱 / 用户名 / 昵称" value={q} onChange={(e) => setQ(e.target.value)} />
              <Box style={{ flex: "1 1 0", height: 0, minHeight: 0, overflowY: "auto" }}>
                {(list.data?.users ?? []).map((u: any) => (
                  <div
                    key={u.id}
                    className={`jc-ticket-list-item${u.id === selectedId ? " is-selected" : ""}`}
                    onClick={() => setSelectedId(u.id)}
                  >
                    <Flex gap="3" align="center" style={{ minWidth: 0 }}>
                      <Avatar size="2" radius="full" fallback={initials(u)} color={u.status === "active" ? "iris" : "amber"} variant="soft" />
                      <Box style={{ minWidth: 0, flex: 1 }}>
                        <Flex justify="between" align="center" gap="2">
                          <Text size="2" weight="medium" truncate>{displayName(u)}</Text>
                          <Badge size="1" variant="soft" color={u.status === "active" ? "green" : "amber"}>{u.status}</Badge>
                        </Flex>
                        <Text size="1" color="gray" as="div" truncate>{contactLine(u)}</Text>
                        <Text size="1" color="gray" as="div" className="jc-num">余额 <PointAmount amount={u.balance} decimals="preserve" iconSize={11} /></Text>
                      </Box>
                    </Flex>
                  </div>
                ))}
                {!list.isLoading && (list.data?.users ?? []).length === 0 && (
                  <Text size="2" color="gray">无匹配用户</Text>
                )}
              </Box>
            </Flex>
          </Card>
        </Box>
        <Box className="jc-ticket-detail-pane" style={{ flex: 1, minWidth: 0, display: "flex", minHeight: 0, overflow: "visible" }}>
          {selectedId ? (
            <UserDetail key={selectedId} uid={selectedId} />
          ) : (
            <Card className="jc-ticket-detail-card" style={{ flex: 1, minHeight: 0 }}>
              <Text size="2" color="gray">选择左侧用户查看详情</Text>
            </Card>
          )}
        </Box>
      </Flex>
    </Box>
  );
}

function UserDetail({ uid }: { uid: string }) {
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({ queryKey: ["cs", "user", uid], queryFn: () => csApi.userDetail(uid) });
  const tickets = useQuery({
    queryKey: ["cs", "tickets", "user", uid],
    queryFn: () => csApi.tickets({ user_id: uid, limit: 50 }),
  });

  const [adjustOpen, setAdjustOpen] = useState(false);
  const [passwordOpen, setPasswordOpen] = useState(false);

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["cs", "user", uid] });
    qc.invalidateQueries({ queryKey: ["cs", "users"] });
  };

  if (isLoading || !data) {
    return (
      <Card className="jc-ticket-detail-card" style={{ flex: 1, minHeight: 0 }}>
        <Text size="2" color="gray">加载中…</Text>
      </Card>
    );
  }
  const u = data;
  const userPlans: any[] = u.plans ?? [];

  return (
    <Card className="jc-ticket-detail-card" style={{ flex: 1, minWidth: 0, minHeight: 0, overflow: "auto" }}>
      <Flex direction="column" gap="3" style={{ minHeight: 0 }}>
        <Flex gap="3" align="center" style={{ minWidth: 0 }}>
          <Avatar size="4" radius="full" fallback={initials(u)} color={u.status === "active" ? "iris" : "amber"} variant="soft" />
          <Box style={{ minWidth: 0 }}>
            <Flex align="center" gap="2">
              <Text size="4" weight="bold" truncate>{displayName(u)}</Text>
              <Badge variant="soft" color={u.status === "active" ? "green" : "amber"}>{u.status}</Badge>
            </Flex>
            <Flex gap="2" mt="1" wrap="wrap">
              <Text size="1" color="gray">{contactLine(u)}</Text>
              <Text size="1" color="gray">· 注册于 {new Date(u.created_at).toLocaleString()}</Text>
            </Flex>
          </Box>
        </Flex>
        <Flex gap="3" wrap="wrap">
          <Box style={{ flex: 1, minWidth: 200 }}>
            <Card>
              <Flex align="start" justify="between" gap="2">
                <Box>
                  <Text size="1" color="gray" as="div">余额</Text>
                  <Text size="6" weight="bold" as="div" className="jc-num" mt="1"><PointAmount amount={u.balance} decimals="preserve" iconSize={18} /></Text>
                </Box>
                <Box
                  style={{
                    width: 38, height: 38, borderRadius: 10, flex: "0 0 auto",
                    display: "flex", alignItems: "center", justifyContent: "center",
                    background: "color-mix(in oklab, var(--c-primary) 16%, transparent)",
                    color: "var(--c-primary-bright)",
                  }}
                >
                  <Wallet size={19} strokeWidth={1.8} />
                </Box>
              </Flex>
              <Flex gap="2" mt="3">
                <Button size="2" onClick={() => setAdjustOpen(true)}>调整余额</Button>
                <Button size="2" variant="soft" color="red" onClick={() => setPasswordOpen(true)}>重置密码</Button>
              </Flex>
            </Card>
          </Box>
          <Box style={{ flex: 1, minWidth: 240 }}>
            <Card>
              <Flex justify="between" align="center" mb="2">
                <Text size="2" weight="medium">套餐</Text>
              </Flex>
              {userPlans.length === 0 ? (
                <Text size="1" color="gray">用户暂无套餐</Text>
              ) : (
                userPlans.map((p) => <UserPlanRow key={p.user_plan_id} uid={uid} p={p} onChanged={invalidate} />)
              )}
            </Card>
          </Box>
        </Flex>
        <Card>
          <Text size="2" weight="medium" as="div" mb="2">工单历史</Text>
          {(tickets.data?.tickets ?? []).length === 0 && !tickets.isLoading && (
            <Text size="1" color="gray">没有工单</Text>
          )}
          {(tickets.data?.tickets ?? []).map((t: TicketRow) => (
            <Flex key={t.id} justify="between" align="center" gap="2" py="2" style={{ borderBottom: "1px solid var(--gray-4)" }}>
              <Box style={{ minWidth: 0 }}>
                <Text size="2" weight="medium" as="div" truncate>
                  {t.ticket_no_label ? `${t.ticket_no_label} · ` : ""}{t.title}
                </Text>
                <Text size="1" color="gray">{new Date(t.created_at).toLocaleString()}</Text>
              </Box>
              <Flex gap="2" align="center">
                {t.unread && <Badge size="1" color="red" variant="soft">未读</Badge>}
                <Badge size="1" variant="soft">{t.status}</Badge>
                <Button asChild size="1" variant="soft">
                  <Link to={`/cs/tickets?id=${encodeURIComponent(t.id)}&user=${encodeURIComponent(uid)}`}>打开</Link>
                </Button>
              </Flex>
            </Flex>
          ))}
        </Card>
      </Flex>

      <AdjustBalanceDialog open={adjustOpen} onClose={() => setAdjustOpen(false)} uid={uid} onDone={invalidate} />
      <ResetPasswordDialog open={passwordOpen} onClose={() => setPasswordOpen(false)} uid={uid} />
    </Card>
  );
}

function UserPlanRow({ uid, p, onChanged }: { uid: string; p: any; onChanged: () => void }) {
  const cancel = useMutation({ mutationFn: () => csApi.cancelUserPlan(uid, p.user_plan_id), onSuccess: onChanged });
  const [days, setDays] = useState<string>("");
  const adjustDays = useMutation({
    mutationFn: (deltaDays: number) => csApi.extendUserPlan(uid, p.user_plan_id, deltaDays),
    onSuccess: () => { setDays(""); onChanged(); },
  });
  const parsedDays = parseInt(days || "0", 10);
  const canAdjustPlan = p.status === "active";
  const canAdjustDays = canAdjustPlan && Number.isFinite(parsedDays) && parsedDays > 0 && !adjustDays.isPending;
  const submitDayAdjustment = (direction: "extend" | "shorten") => {
    if (!canAdjustDays) return;
    adjustDays.mutate(direction === "extend" ? parsedDays : -parsedDays);
  };
  return (
    <Box mb="2" style={{ paddingBottom: 8, borderBottom: "1px solid var(--gray-4)" }}>
      <Flex justify="between" align="center">
        <Text size="2">{p.plan_name || p.plan_id.slice(0, 8)}</Text>
        <Badge size="1" color={p.status === "active" ? "green" : "gray"} variant="soft">{p.status}</Badge>
      </Flex>
      <Text size="1" color="gray">到期 {new Date(p.expire_at).toLocaleString()}</Text>
      <Flex gap="2" mt="2" align="center">
        <ClearableTextField
          style={{ width: 80 }}
          placeholder="天数"
          type="number"
          min="1"
          step="1"
          value={days}
          disabled={!canAdjustPlan}
          onChange={(e) => setDays(e.target.value)}
        />
        <Button size="1" color="green" variant="soft" disabled={!canAdjustDays} onClick={() => submitDayAdjustment("extend")}>延长</Button>
        <Button size="1" color="amber" variant="soft" disabled={!canAdjustDays} onClick={() => submitDayAdjustment("shorten")}>缩短</Button>
        {p.status === "active" && (
          <Button size="1" color="red" variant="soft" onClick={() => cancel.mutate()}>取消订阅</Button>
        )}
      </Flex>
    </Box>
  );
}

function AdjustBalanceDialog({ open, onClose, uid, onDone }: { open: boolean; onClose: () => void; uid: string; onDone: () => void }) {
  const [amount, setAmount] = useState("");
  const [kind, setKind] = useState<"topup" | "refund" | "deduct" | "adjust">("topup");
  const [note, setNote] = useState("");
  useEffect(() => {
    if (open) setKind("topup");
  }, [open]);
  const m = useMutation({
    mutationFn: () => csApi.adjustBalance(uid, { amount, note: note || undefined, kind }),
    onSuccess: () => { setAmount(""); setNote(""); onDone(); onClose(); },
  });
  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="460px">
        <Dialog.Title>调整余额</Dialog.Title>
        <Dialog.Description size="2" color="gray">充值/退款会增加，扣费会扣减；“调整”按金额正负直接变更。所有调整记入 balance_txes 与审计日志。</Dialog.Description>
        <Flex direction="column" gap="3" mt="3">
          <Box className="jc-adjust-balance-row">
            <Box style={{ flex: 1 }}>
              <Text as="label" size="2" weight="medium">金额（积分）</Text>
              <ClearableTextField value={amount} onChange={(e) => setAmount(e.target.value)} placeholder={kind === "adjust" ? "100 或 -50" : "100"} />
            </Box>
            <Box>
              <Text as="label" size="2" weight="medium">类型</Text>
              <Select.Root value={kind} onValueChange={(v) => setKind(v as any)}>
                <Select.Trigger />
                <Select.Content>
                  <Select.Item value="topup">充值</Select.Item>
                  <Select.Item value="adjust">调整</Select.Item>
                  <Select.Item value="refund">退款</Select.Item>
                  <Select.Item value="deduct">扣费</Select.Item>
                </Select.Content>
              </Select.Root>
            </Box>
          </Box>
          <Box>
            <Text as="label" size="2" weight="medium">备注</Text>
            <ClearableTextField value={note} onChange={(e) => setNote(e.target.value)} placeholder="例：工单 #1234 协商补偿" />
          </Box>
          <Flex gap="2" justify="end">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button disabled={!amount || m.isPending} onClick={() => m.mutate()}>提交</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function ResetPasswordDialog({ open, onClose, uid }: { open: boolean; onClose: () => void; uid: string }) {
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const m = useMutation({
    mutationFn: () => csApi.resetUserPassword(uid, newPassword),
    onSuccess: () => { setNewPassword(""); setConfirmPassword(""); onClose(); },
  });
  const canSubmit = newPassword.length >= 8 && newPassword === confirmPassword && !m.isPending;

  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="420px">
        <Dialog.Title>重置用户密码</Dialog.Title>
        <Dialog.Description size="2" color="gray">设置新密码后，该用户当前登录会话会全部失效。</Dialog.Description>
        <Flex direction="column" gap="3" mt="3">
          <Box>
            <Text as="label" size="2" weight="medium">新密码（至少 8 位）</Text>
            <ClearableTextField type="password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} />
          </Box>
          <Box>
            <Text as="label" size="2" weight="medium">重复新密码</Text>
            <ClearableTextField type="password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} />
            {confirmPassword !== "" && newPassword !== confirmPassword && (
              <Text size="1" color="red" mt="1" as="div">两次输入不一致</Text>
            )}
          </Box>
          {m.isError && <Text size="2" color="red">{errMsg(m.error)}</Text>}
          <Flex gap="2" justify="end">
            <Button variant="soft" color="gray" onClick={onClose}>取消</Button>
            <Button color="red" disabled={!canSubmit} loading={m.isPending} onClick={() => m.mutate()}>重置密码</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
