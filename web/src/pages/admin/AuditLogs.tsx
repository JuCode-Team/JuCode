import { useState } from "react";
import { Box, Button, Code, Dialog, Flex, IconButton, Text } from "@radix-ui/themes";
import { Eye } from "lucide-react";
import { adminApi, type AdminAuditLog } from "../../api/endpoints";
import { usePagedQuery } from "../../lib/paged";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

export default function AuditLogsPage() {
  const [action, setAction] = useState("");
  const [target, setTarget] = useState("");
  const [actor, setActor] = useState("");

  const paged = usePagedQuery<AdminAuditLog>({
    queryKey: ["admin", "audit"],
    queryFn: (p) =>
      adminApi
        .auditLogs({
          q: p.q || undefined,
          action: action.trim() || undefined,
          target: target.trim() || undefined,
          actor_user_id: actor.trim() || undefined,
          limit: p.limit,
          offset: p.offset,
        })
        .then((r) => ({ items: r.logs, total: r.total, limit: r.limit, offset: r.offset })),
    extras: [action.trim(), target.trim(), actor.trim()],
    pageSize: 50,
  });

  const cols: Column<AdminAuditLog>[] = [
    { header: "时间", width: "150px", cell: (r) => <span className="jc-mono" style={{ fontSize: 12 }}>{new Date(r.created_at).toLocaleString()}</span> },
    { header: "管理员", width: "100px", cell: (r) => <Code style={{ fontSize: 11 }}>{r.actor_user_id.slice(0, 8)}</Code> },
    { header: "动作", width: "180px", cell: (r) => <Code style={{ fontSize: 12 }}>{r.action}</Code> },
    { header: "目标", cell: (r) => <Text size="1" style={{ display: "block" }} truncate>{r.target || "—"}</Text> },
    { header: "IP", width: "120px", cell: (r) => <Text size="1" color="gray">{r.ip || "—"}</Text> },
    { header: "详情", width: "60px", align: "right", cell: (r) => <DetailButton r={r} /> },
  ];

  const filters = (
    <>
      <Box style={{ minWidth: 160 }}>
        <ClearableTextField placeholder="action (精确)" value={action} onChange={(e) => setAction(e.target.value)} />
      </Box>
      <Box style={{ minWidth: 160 }}>
        <ClearableTextField placeholder="target (精确)" value={target} onChange={(e) => setTarget(e.target.value)} />
      </Box>
      <Box style={{ minWidth: 180 }}>
        <ClearableTextField placeholder="管理员 UUID" value={actor} onChange={(e) => setActor(e.target.value)}
          style={{ fontFamily: "ui-monospace, monospace" }} />
      </Box>
    </>
  );

  return (
    <AdminPage
      title="后台审计"
      description="管理员所有写操作的完整记录，用于追踪创建、修改和删除。"
    >
      <AdminMetricGrid>
        <AdminMetricCard label="匹配审计" value={paged.total.toLocaleString()} detail="服务端分页" tone="iris" />
        <AdminMetricCard label="当前页" value={paged.items.length} detail="审计事件" tone="blue" />
        <AdminMetricCard label="动作过滤" value={action.trim() || "全部"} detail="action" />
        <AdminMetricCard label="管理员过滤" value={actor.trim() ? "已指定" : "全部"} detail="actor_user_id" />
      </AdminMetricGrid>
      <DataTable
        {...paged}
        isLoading={paged.query.isLoading}
        isFetching={paged.query.isFetching}
        refetch={() => paged.query.refetch()}
        columns={cols}
        rowKey={(r) => r.id}
        searchPlaceholder="搜索 action / target…"
        filters={filters}
      />
    </AdminPage>
  );
}

function DetailButton({ r }: { r: AdminAuditLog }) {
  const [open, setOpen] = useState(false);
  return (
    <Dialog.Root open={open} onOpenChange={setOpen}>
      <Dialog.Trigger><IconButton size="1" variant="soft"><Eye size={13} /></IconButton></Dialog.Trigger>
      <Dialog.Content style={{ maxWidth: 640 }}>
        <Dialog.Title>审计详情</Dialog.Title>
        <Flex direction="column" gap="2" mt="3">
          <Field k="时间" v={new Date(r.created_at).toLocaleString()} />
          <Field k="管理员" v={r.actor_user_id} mono />
          <Field k="动作" v={r.action} mono />
          <Field k="目标" v={r.target || "—"} mono />
          <Field k="IP" v={r.ip || "—"} mono />
          <Field k="UA" v={r.ua || "—"} />
          <Box>
            <Text size="1" color="gray" as="div" mb="1">payload</Text>
            <Box style={{ background: "var(--gray-3)", padding: 10, borderRadius: 8, maxHeight: 280, overflow: "auto" }}>
              <pre style={{ margin: 0, fontFamily: "var(--font-mono)", fontSize: 12 }}>
                {JSON.stringify(r.payload ?? {}, null, 2)}
              </pre>
            </Box>
          </Box>
        </Flex>
        <Flex justify="end" mt="3"><Dialog.Close><Button variant="soft">关闭</Button></Dialog.Close></Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function Field({ k, v, mono = false }: { k: string; v: string; mono?: boolean }) {
  return (
    <Flex gap="3" align="start">
      <Text size="1" color="gray" style={{ width: 64, flexShrink: 0 }}>{k}</Text>
      <Text size="2" style={mono ? { fontFamily: "var(--font-mono)", fontSize: 12, wordBreak: "break-all" } : { wordBreak: "break-all" }}>{v}</Text>
    </Flex>
  );
}
