import { useEffect, useMemo, useState } from "react";
import {
  Badge, Box, Button, Card, Dialog, Flex, Grid, IconButton, Select,
  Switch, Table, Text, TextArea, Tooltip,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Pencil, Trash2, Upload } from "lucide-react";
import {
  adminApi,
  type PolicyGroup,
  type PolicyGroupInput,
  type PolicyKeyword,
  type PolicyScope,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

const SEVERITIES = ["low", "medium", "high", "critical"] as const;
const ACTIONS = ["block", "log_only"] as const;

// Scope catalogue — drives the dropdown in the group dialog AND the
// per-row badge in the group sidebar. Keep this list in sync with
// PolicyScope union + backend ent enum.
const SCOPES: { value: PolicyScope; label: string; hint: string }[] = [
  { value: "input_only",         label: "仅输入",           hint: "只扫请求体 (默认)" },
  { value: "input_output",       label: "输入+输出",        hint: "再扫流式输出文本,跳过工具调用" },
  { value: "input_output_tools", label: "输入+输出+工具",   hint: "并扫流式输出 + tool_use 参数 / tool_result 内容" },
];
const scopeLabel = (s?: string) => SCOPES.find((x) => x.value === s)?.label ?? "—";

export default function PolicyPage() {
  const qc = useQueryClient();
  const [activeGroup, setActiveGroup] = useState<string | null>(null);

  const { data: stats } = useQuery({ queryKey: ["admin", "policy", "stats"], queryFn: adminApi.policyStats });
  const { data: groups } = useQuery({ queryKey: ["admin", "policy", "groups"], queryFn: adminApi.policyGroups });
  useEffect(() => {
    if (!activeGroup && groups && groups.length > 0) setActiveGroup(groups[0].id);
  }, [activeGroup, groups]);
  const inv = () => {
    qc.invalidateQueries({ queryKey: ["admin", "policy"] });
  };

  return (
    <AdminPage
      title="关键词审查"
      description="入站与流式输出同步审查，按分组管理拦截动作、严重度和扫描范围。"
    >
      <AdminMetricGrid>
        <AdminMetricCard label="分组" value={stats?.categories ?? 0} detail="policy groups" tone="iris" />
        <AdminMetricCard label="关键词" value={stats?.keywords ?? 0} detail="patterns" tone="blue" />
        <AdminMetricCard label="已激活" value={stats?.active ?? 0} detail="enabled keywords" tone="green" />
        <AdminMetricCard label="扫描引擎" value="Aho-Corasick" detail="同步匹配" />
      </AdminMetricGrid>

      <Grid columns={{ initial: "1", md: "320px 1fr" }} gap="3">
        <Card>
          <Flex justify="between" align="center" mb="2">
            <Text size="2" weight="medium">分组</Text>
            <GroupDialog mode="create" onDone={inv} />
          </Flex>
          <Flex direction="column" gap="1">
            {(groups ?? []).map((g) => (
              <GroupItem
                key={g.id}
                group={g}
                active={g.id === activeGroup}
                onClick={() => setActiveGroup(g.id)}
                onDone={inv}
              />
            ))}
            {(groups?.length ?? 0) === 0 && (
              <Text size="2" color="gray">尚未创建分组,先建一个</Text>
            )}
          </Flex>
        </Card>
        <KeywordPanel groupId={activeGroup} groups={groups ?? []} onDone={inv} />
      </Grid>
    </AdminPage>
  );
}

function GroupItem({
  group, active, onClick, onDone,
}: { group: PolicyGroup; active: boolean; onClick: () => void; onDone: () => void }) {
  return (
    <Flex
      align="center"
      justify="between"
      gap="2"
      style={{
        padding: "8px 10px",
        borderRadius: 8,
        cursor: "pointer",
        background: active ? "var(--accent-a3)" : "transparent",
        border: active ? "1px solid var(--accent-a6)" : "1px solid transparent",
      }}
      onClick={onClick}
    >
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Text size="2" weight={active ? "medium" : "regular"} as="div" truncate>{group.name}</Text>
        <Flex gap="1" mt="1" wrap="wrap">
          <Badge size="1" color={group.default_action === "block" ? "red" : "gray"} variant="soft">
            {group.default_action}
          </Badge>
          <Badge size="1" color="gray" variant="soft">{group.default_severity}</Badge>
          <Tooltip content={SCOPES.find((s) => s.value === group.scope)?.hint ?? ""}>
            <Badge size="1" color={group.scope === "input_only" ? "gray" : "blue"} variant="soft">
              {scopeLabel(group.scope)}
            </Badge>
          </Tooltip>
          {!group.enabled && <Badge size="1" color="gray" variant="soft">disabled</Badge>}
        </Flex>
      </Box>
      <GroupDialog mode="edit" initial={group} onDone={onDone} />
    </Flex>
  );
}

function GroupDialog({
  mode, initial, onDone,
}: { mode: "create" | "edit"; initial?: PolicyGroup; onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<PolicyGroupInput>(() => ({
    name: initial?.name ?? "",
    slug: initial?.slug ?? "",
    description: initial?.description ?? "",
    default_action: initial?.default_action ?? "block",
    default_severity: initial?.default_severity ?? "medium",
    scope: initial?.scope ?? "input_only",
    enabled: initial?.enabled ?? true,
    sort_order: initial?.sort_order ?? 0,
  }));
  const save = useMutation({
    mutationFn: () =>
      mode === "create"
        ? adminApi.createPolicyGroup(form)
        : adminApi.updatePolicyGroup(initial!.id, form),
    onSuccess: () => { onDone(); setOpen(false); },
  });
  const del = useMutation({
    mutationFn: () => adminApi.deletePolicyGroup(initial!.id),
    onSuccess: () => { onDone(); setOpen(false); },
  });
  return (
    <Dialog.Root open={open} onOpenChange={setOpen}>
      <Dialog.Trigger>
        {mode === "create" ? (
          <Button size="1" variant="soft"><Plus size={12} /> 新建</Button>
        ) : (
          <IconButton size="1" variant="ghost" onClick={(e) => e.stopPropagation()}>
            <Pencil size={13} />
          </IconButton>
        )}
      </Dialog.Trigger>
      <Dialog.Content style={{ maxWidth: 480 }}>
        <Dialog.Title>{mode === "create" ? "新建分组" : "编辑分组"}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Box>
            <Text size="1" color="gray" as="p" mb="1">名称</Text>
            <ClearableTextField value={form.name ?? ""} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">slug（URL 安全）</Text>
            <ClearableTextField value={form.slug ?? ""} onChange={(e) => setForm({ ...form, slug: e.target.value })} />
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">扫描范围</Text>
            <Select.Root
              value={form.scope ?? "input_only"}
              onValueChange={(v) => setForm({ ...form, scope: v as PolicyScope })}
            >
              <Select.Trigger />
              <Select.Content>
                {SCOPES.map((s) => (
                  <Select.Item key={s.value} value={s.value}>{s.label}</Select.Item>
                ))}
              </Select.Content>
            </Select.Root>
            <Text size="1" color="gray" as="p" mt="1">
              {SCOPES.find((s) => s.value === (form.scope ?? "input_only"))?.hint}
            </Text>
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">默认动作</Text>
            <Select.Root
              value={form.default_action ?? "block"}
              onValueChange={(v) => setForm({ ...form, default_action: v as "block" | "log_only" })}
            >
              <Select.Trigger />
              <Select.Content>
                {ACTIONS.map((a) => <Select.Item key={a} value={a}>{a}</Select.Item>)}
              </Select.Content>
            </Select.Root>
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">默认严重度</Text>
            <Select.Root
              value={form.default_severity ?? "medium"}
              onValueChange={(v) => setForm({ ...form, default_severity: v as typeof SEVERITIES[number] })}
            >
              <Select.Trigger />
              <Select.Content>
                {SEVERITIES.map((s) => <Select.Item key={s} value={s}>{s}</Select.Item>)}
              </Select.Content>
            </Select.Root>
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">备注</Text>
            <TextArea
              value={form.description ?? ""}
              onChange={(e) => setForm({ ...form, description: e.target.value })}
              rows={2}
            />
          </Box>
          <Flex align="center" gap="2">
            <Switch checked={form.enabled ?? true} onCheckedChange={(v) => setForm({ ...form, enabled: v })} />
            <Text size="2">启用</Text>
          </Flex>
          {save.isError && <Text size="2" color="red">{errMsg(save.error)}</Text>}
          <Flex gap="2" justify="end">
            {mode === "edit" && (
              <Button color="red" variant="soft" loading={del.isPending}
                onClick={() => { if (confirm(`删除分组「${initial!.name}」?该分组下所有关键词也会被删除。`)) del.mutate(); }}>
                <Trash2 size={13} /> 删除
              </Button>
            )}
            <Dialog.Close><Button variant="soft" color="gray">取消</Button></Dialog.Close>
            <Button loading={save.isPending} onClick={() => save.mutate()}>保存</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function KeywordPanel({
  groupId, groups, onDone,
}: { groupId: string | null; groups: PolicyGroup[]; onDone: () => void }) {
  const [q, setQ] = useState("");
  const [offset, setOffset] = useState(0);
  const limit = 50;
  const { data, isLoading } = useQuery({
    queryKey: ["admin", "policy", "kws", groupId, q, offset],
    queryFn: () => adminApi.policyKeywords({
      group_id: groupId ?? undefined,
      q: q.trim() || undefined,
      limit, offset,
    }),
    enabled: !!groupId,
    placeholderData: (prev) => prev,
  });
  const inv = () => onDone();
  useEffect(() => { setOffset(0); }, [groupId, q]);

  const del = useMutation({ mutationFn: (id: string) => adminApi.deletePolicyKeyword(id), onSuccess: inv });
  const toggle = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      adminApi.updatePolicyKeyword(id, { enabled }),
    onSuccess: inv,
  });

  const total = data?.total ?? 0;

  return (
    <Card>
      <Flex justify="between" align="center" mb="2" wrap="wrap" gap="2">
        <Flex gap="2" align="center" wrap="wrap">
          <ClearableTextField placeholder="模糊搜索 pattern…" value={q} onChange={(e) => setQ(e.target.value)} style={{ minWidth: 220 }} />
          <Text size="1" color="gray">共 {total} 条</Text>
        </Flex>
        <Flex gap="2">
          <ImportDialog groupId={groupId} onDone={inv} />
          <KeywordDialog mode="create" groupId={groupId} groups={groups} onDone={inv} />
        </Flex>
      </Flex>
      <div className="jc-local-table-scroll">
      <Table.Root size="1" variant="surface">
        <Table.Header>
          <Table.Row>
            <Table.ColumnHeaderCell>Pattern</Table.ColumnHeaderCell>
            <Table.ColumnHeaderCell>严重度</Table.ColumnHeaderCell>
            <Table.ColumnHeaderCell>启用</Table.ColumnHeaderCell>
            <Table.ColumnHeaderCell>备注</Table.ColumnHeaderCell>
            <Table.ColumnHeaderCell align="right">操作</Table.ColumnHeaderCell>
          </Table.Row>
        </Table.Header>
        <Table.Body>
          {(data?.keywords ?? []).map((k) => (
            <Table.Row key={k.id}>
              <Table.Cell><span className="jc-mono">{k.pattern}</span></Table.Cell>
              <Table.Cell>{k.severity ? <Badge size="1" variant="soft">{k.severity}</Badge> : <Text size="1" color="gray">继承</Text>}</Table.Cell>
              <Table.Cell>
                <Switch size="1" checked={k.enabled} onCheckedChange={(v) => toggle.mutate({ id: k.id, enabled: v })} />
              </Table.Cell>
              <Table.Cell><Text size="1" color="gray" style={{ maxWidth: 280, display: "inline-block" }} truncate>{k.note || "—"}</Text></Table.Cell>
              <Table.Cell align="right">
                <Flex gap="1" justify="end">
                  <KeywordDialog mode="edit" groupId={groupId} groups={groups} initial={k} onDone={inv} />
                  <Tooltip content="删除">
                    <IconButton size="1" variant="soft" color="red"
                      onClick={() => { if (confirm(`删除 ${k.pattern} ?`)) del.mutate(k.id); }}>
                      <Trash2 size={13} />
                    </IconButton>
                  </Tooltip>
                </Flex>
              </Table.Cell>
            </Table.Row>
          ))}
          {!isLoading && (data?.keywords.length ?? 0) === 0 && (
            <Table.Row><Table.Cell colSpan={5}><Text color="gray">无匹配关键词</Text></Table.Cell></Table.Row>
          )}
        </Table.Body>
      </Table.Root>
      </div>
      <Flex justify="end" align="center" gap="2" mt="2">
        <Button size="1" variant="soft" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - limit))}>上一页</Button>
        <Text size="1" color="gray">{Math.floor(offset / limit) + 1} / {Math.max(1, Math.ceil(total / limit))}</Text>
        <Button size="1" variant="soft" disabled={offset + limit >= total} onClick={() => setOffset(offset + limit)}>下一页</Button>
      </Flex>
    </Card>
  );
}

function KeywordDialog({
  mode, groupId, groups, initial, onDone,
}: {
  mode: "create" | "edit";
  groupId: string | null;
  groups: PolicyGroup[];
  initial?: PolicyKeyword;
  onDone: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState(() => ({
    group_id: initial?.group_id ?? groupId ?? "",
    pattern: initial?.pattern ?? "",
    severity: (initial?.severity ?? "") as "" | "low" | "medium" | "high" | "critical",
    note: initial?.note ?? "",
    enabled: initial?.enabled ?? true,
  }));
  useEffect(() => {
    if (open && mode === "create") {
      setForm({ group_id: groupId ?? "", pattern: "", severity: "", note: "", enabled: true });
    }
  }, [open, mode, groupId]);
  const save = useMutation({
    mutationFn: () => {
      const body = { ...form, severity: form.severity || undefined };
      return mode === "create"
        ? adminApi.createPolicyKeyword(body as unknown as Parameters<typeof adminApi.createPolicyKeyword>[0])
        : adminApi.updatePolicyKeyword(initial!.id, body as unknown as Parameters<typeof adminApi.updatePolicyKeyword>[1]);
    },
    onSuccess: () => { onDone(); setOpen(false); },
  });
  const disabled = !form.pattern.trim() || !form.group_id;
  return (
    <Dialog.Root open={open} onOpenChange={setOpen}>
      <Dialog.Trigger>
        {mode === "create" ? (
          <Button size="2" disabled={!groupId}><Plus size={14} /> 关键词</Button>
        ) : (
          <Tooltip content="编辑">
            <IconButton size="1" variant="soft"><Pencil size={13} /></IconButton>
          </Tooltip>
        )}
      </Dialog.Trigger>
      <Dialog.Content style={{ maxWidth: 440 }}>
        <Dialog.Title>{mode === "create" ? "新建关键词" : "编辑关键词"}</Dialog.Title>
        <Flex direction="column" gap="3" mt="3">
          <Box>
            <Text size="1" color="gray" as="p" mb="1">所属分组</Text>
            <Select.Root value={form.group_id} onValueChange={(v) => setForm({ ...form, group_id: v })}>
              <Select.Trigger />
              <Select.Content>
                {groups.map((g) => <Select.Item key={g.id} value={g.id}>{g.name}</Select.Item>)}
              </Select.Content>
            </Select.Root>
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">关键词 / 模式</Text>
            <ClearableTextField value={form.pattern} onChange={(e) => setForm({ ...form, pattern: e.target.value })} />
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">严重度（留空 = 继承分组）</Text>
            <Select.Root value={form.severity || "inherit"} onValueChange={(v) => setForm({ ...form, severity: v === "inherit" ? "" : (v as typeof SEVERITIES[number]) })}>
              <Select.Trigger />
              <Select.Content>
                <Select.Item value="inherit">继承</Select.Item>
                {SEVERITIES.map((s) => <Select.Item key={s} value={s}>{s}</Select.Item>)}
              </Select.Content>
            </Select.Root>
          </Box>
          <Box>
            <Text size="1" color="gray" as="p" mb="1">备注</Text>
            <ClearableTextField value={form.note} onChange={(e) => setForm({ ...form, note: e.target.value })} />
          </Box>
          <Flex align="center" gap="2">
            <Switch checked={form.enabled} onCheckedChange={(v) => setForm({ ...form, enabled: v })} />
            <Text size="2">启用</Text>
          </Flex>
          {save.isError && <Text size="2" color="red">{errMsg(save.error)}</Text>}
          <Flex gap="2" justify="end">
            <Dialog.Close><Button variant="soft" color="gray">取消</Button></Dialog.Close>
            <Button disabled={disabled} loading={save.isPending} onClick={() => save.mutate()}>保存</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function ImportDialog({ groupId, onDone }: { groupId: string | null; onDone: () => void }) {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");
  const importM = useMutation({
    mutationFn: () => adminApi.importPolicyKeywords(groupId!, text),
    onSuccess: () => onDone(),
  });
  const created = useMemo(() => (importM.data ?? []).filter((r) => r.created).length, [importM.data]);
  const dup = useMemo(() => (importM.data ?? []).filter((r) => r.error === "duplicate").length, [importM.data]);
  return (
    <Dialog.Root open={open} onOpenChange={(v) => { setOpen(v); if (!v) { setText(""); importM.reset(); } }}>
      <Dialog.Trigger>
        <Button size="2" variant="soft" disabled={!groupId}><Upload size={14} /> 批量导入</Button>
      </Dialog.Trigger>
      <Dialog.Content style={{ maxWidth: 520 }}>
        <Dialog.Title>批量导入关键词</Dialog.Title>
        <Text size="2" color="gray" as="div" mb="2">
          每行一个关键词;以 # 开头的行会被忽略。已存在的会被自动跳过。
        </Text>
        <TextArea
          rows={14}
          placeholder={"敏感词1\n敏感词2\n# 注释\nsensitive_keyword"}
          value={text}
          onChange={(e) => setText(e.target.value)}
          style={{ fontFamily: "var(--font-mono)", fontSize: 12 }}
        />
        {importM.isSuccess && (
          <Text size="2" color="gray" as="div" mt="2">
            导入完成:新建 {created} · 重复 {dup} · 总 {importM.data?.length ?? 0}
          </Text>
        )}
        {importM.isError && <Text size="2" color="red" as="div" mt="2">{errMsg(importM.error)}</Text>}
        <Flex gap="2" justify="end" mt="3">
          <Dialog.Close><Button variant="soft" color="gray">关闭</Button></Dialog.Close>
          <Button disabled={!text.trim()} loading={importM.isPending} onClick={() => importM.mutate()}>导入</Button>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
