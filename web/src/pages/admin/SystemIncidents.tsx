import { useEffect, useState } from "react";
import {
  Box,
  Button,
  Callout,
  Flex,
  SegmentedControl,
  Select,
  Text,
  TextArea,
} from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertCircle } from "lucide-react";
import { Link } from "react-router-dom";
import {
  adminApi,
  type SystemIncident,
  type SystemIncidentResolution,
  type SystemIncidentSeverity,
  type SystemIncidentStatusFilter,
} from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { AdminPage } from "../../components/AdminPage";
import { DataTable, type Column } from "../../components/SimpleDataTable";
import { DateRangeInput, rangeToIso } from "../../components/UsageLogFilters";
import { stateTone } from "../../components/AvailabilityDots";
import { usePagedQuery } from "../../lib/paged";

const SUMMARY_LIMIT = 200;
const PAGE_SIZE = 50;

export default function SystemIncidentsPage() {
  const queryClient = useQueryClient();
  const [status, setStatus] = useState<SystemIncidentStatusFilter>("open");
  const [groupID, setGroupID] = useState("");
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");
  const range = rangeToIso(start, end);

  const paged = usePagedQuery<SystemIncident>({
    queryKey: ["admin", "system-incidents"],
    queryFn: (page) =>
      adminApi
        .systemIncidents({
          status,
          group_id: groupID || undefined,
          start: range.from,
          end: range.to,
          limit: page.limit,
          offset: page.offset,
        })
        .then((response) => ({
          items: response.incidents ?? [],
          total: response.total ?? 0,
          limit: response.limit ?? page.limit,
          offset: response.offset ?? page.offset,
        })),
    extras: [status, groupID, range.from ?? "", range.to ?? ""],
    pageSize: PAGE_SIZE,
    refetchInterval: 30_000,
  });

  const openSummary = useQuery({
    queryKey: ["admin", "system-incidents", "open-summary"],
    queryFn: () => adminApi.systemIncidents({ status: "open", limit: SUMMARY_LIMIT, offset: 0 }),
    refetchInterval: 30_000,
  });
  const groups = useQuery({
    queryKey: ["admin", "groups", "system-incident-filter"],
    queryFn: adminApi.groups,
    staleTime: 60_000,
  });

  const updateNote = useMutation({
    mutationFn: ({ id, note }: { id: string; note: string }) =>
      adminApi.updateSystemIncidentNote(id, note),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["admin", "system-incidents"] }),
  });

  const openIncidents = openSummary.data?.incidents ?? [];
  const openCount = openSummary.data?.total;
  const worstOpenSeverity = findWorstSeverity(openIncidents);
  const apiError = paged.query.error ?? openSummary.error;

  const columns: Column<SystemIncident>[] = [
    {
      header: "分组",
      width: "180px",
      cell: (incident) => (
        <Box>
          <Text size="2" weight="medium" as="div">{incident.group_name}</Text>
          {incident.model && (
            <Text size="1" color="gray" as="div" className="jc-mono">{incident.model}</Text>
          )}
        </Box>
      ),
    },
    {
      header: "严重度",
      width: "116px",
      cell: (incident) => (
        <Flex direction="column" align="start" gap="1">
          <SeverityChip severity={incident.severity} />
          {incident.worst_severity !== incident.severity && (
            <Text size="1" color="gray">期间最严重：{severityLabel(incident.worst_severity)}</Text>
          )}
        </Flex>
      ),
    },
    {
      header: "开始时间",
      width: "164px",
      cell: (incident) => (
        <span className="jc-mono jc-system-incident-time">{formatTimestamp(incident.started_at)}</span>
      ),
    },
    {
      header: "持续时长",
      width: "104px",
      cell: (incident) => (
        <span className="jc-mono jc-system-incident-readout" title={`${incident.duration_seconds} 秒`}>
          {formatDuration(incident.duration_seconds)}
        </span>
      ),
    },
    {
      header: "最差业务可用率",
      width: "138px",
      cell: (incident) => (
        <Flex direction="column" align="start" gap="1">
          <span className="jc-mono jc-system-incident-readout">
            {formatAvailability(incident.worst_availability_pct)}
          </span>
          <Text size="1" color="gray">样本 {incident.sample_count.toLocaleString()}</Text>
        </Flex>
      ),
    },
    {
      header: "状态 / 结束方式",
      width: "132px",
      cell: (incident) => {
        const resolution = resolutionLabel(incident.resolution);
        return (
          <Flex direction="column" align="start" gap="1">
            <Text size="2" weight="medium">{incidentStateLabel(incident)}</Text>
            {resolution && resolution !== incidentStateLabel(incident) && (
              <Text size="1" color="gray">{resolution}</Text>
            )}
          </Flex>
        );
      },
    },
    {
      header: "备注",
      minWidth: "240px",
      wrap: true,
      cell: (incident) => (
        <IncidentNoteEditor
          note={incident.note}
          pending={updateNote.isPending && updateNote.variables?.id === incident.id}
          onSave={async (note) => {
            await updateNote.mutateAsync({ id: incident.id, note });
          }}
        />
      ),
    },
    {
      header: "查看",
      width: "132px",
      wrap: true,
      cell: (incident) => (
        <Flex direction="column" align="start" gap="1" className="jc-system-incident-links">
          <Link to={`/admin/observability?dim=group&group_id=${encodeURIComponent(incident.group_id)}`}>
            查看服务数据
          </Link>
          <Link to={`/admin/logs?anomaly=any&group_id=${encodeURIComponent(incident.group_id)}`}>
            查看异常请求
          </Link>
        </Flex>
      ),
    },
  ];

  const filters = (
    <>
      <SegmentedControl.Root
        size="1"
        value={status}
        onValueChange={(value) => setStatus(value as SystemIncidentStatusFilter)}
        aria-label="异常状态"
      >
        <SegmentedControl.Item value="open">进行中</SegmentedControl.Item>
        <SegmentedControl.Item value="resolved">已恢复</SegmentedControl.Item>
        <SegmentedControl.Item value="all">全部</SegmentedControl.Item>
      </SegmentedControl.Root>
      <Select.Root value={groupID || "__all"} onValueChange={(value) => setGroupID(value === "__all" ? "" : value)}>
        <Select.Trigger aria-label="分组" placeholder="全部分组" />
        <Select.Content>
          <Select.Item value="__all">全部分组</Select.Item>
          {(groups.data ?? []).map((group) => (
            <Select.Item key={group.id} value={group.id}>{group.name}</Select.Item>
          ))}
        </Select.Content>
      </Select.Root>
      <DateRangeInput start={start} setStart={setStart} end={end} setEnd={setEnd} />
    </>
  );

  return (
    <AdminPage
      title="系统异常"
      description="服务异常时自动开始，恢复稳定后自动结束。"
    >
      <Box className="jc-system-incidents-summary" mb="3" aria-live="polite">
        <Box className="jc-system-incidents-summary-item">
          <Text size="1" color="gray" as="div">进行中</Text>
          <span className="jc-system-incidents-summary-value">
            {openSummary.isLoading && openCount === undefined ? "—" : openCount?.toLocaleString() ?? "—"}
          </span>
          <Text size="1" color="gray" as="div">自动监测中的事项</Text>
        </Box>
        <Box className="jc-system-incidents-summary-item">
          <Text size="1" color="gray" as="div">进行中最严重级别</Text>
          <Box className="jc-system-incidents-summary-severity">
            {worstOpenSeverity ? (
              <SeverityChip severity={worstOpenSeverity} />
            ) : (
              <Text size="2" color="gray">{openCount === 0 ? "无" : "—"}</Text>
            )}
          </Box>
          <Text size="1" color="gray" as="div">按当前业务可用率判定</Text>
        </Box>
      </Box>

      {apiError && (
        <Callout.Root color="red" size="1" mb="3">
          <Callout.Icon><AlertCircle size={16} /></Callout.Icon>
          <Callout.Text>
            无法加载系统异常。请确认后端 system-incidents API 已部署并可访问；其他管理页面不受影响。
            <Text size="1" color="gray" as="div" mt="1">{errMsg(apiError)}</Text>
          </Callout.Text>
        </Callout.Root>
      )}

      {!paged.query.isError && (
        <DataTable
          {...paged}
          isLoading={paged.query.isLoading}
          isFetching={paged.query.isFetching}
          refetch={() => {
            void Promise.all([paged.query.refetch(), openSummary.refetch()]);
          }}
          columns={columns}
          rowKey={(incident) => incident.id}
          filters={filters}
          controlSize="1"
          emptyText={emptyText(status)}
        />
      )}
    </AdminPage>
  );
}

function SeverityChip({ severity }: { severity: SystemIncidentSeverity }) {
  return (
    <span className="jc-system-incident-severity" data-tone={stateTone(severity)}>
      {severityLabel(severity)}
    </span>
  );
}

function IncidentNoteEditor({
  note,
  pending,
  onSave,
}: {
  note: string;
  pending: boolean;
  onSave: (note: string) => Promise<void>;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(note);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!editing) setDraft(note);
  }, [editing, note]);

  if (!editing) {
    return (
      <button
        type="button"
        className="jc-system-incident-note-trigger"
        onClick={() => {
          setError("");
          setEditing(true);
        }}
        title={note || "添加备注"}
      >
        {note || "添加备注"}
      </button>
    );
  }

  const normalized = draft.trim();
  const dirty = normalized !== note;
  const save = async () => {
    setError("");
    try {
      await onSave(normalized);
      setEditing(false);
    } catch (cause) {
      setError(errMsg(cause) || "备注保存失败");
    }
  };

  return (
    <Flex direction="column" gap="2" className="jc-system-incident-note-editor">
      <TextArea
        value={draft}
        maxLength={2000}
        rows={2}
        aria-label="系统异常备注"
        onChange={(event) => setDraft(event.target.value)}
      />
      <Flex align="center" justify="between" gap="2">
        <Text size="1" color="gray">{draft.length} / 2000</Text>
        <Flex gap="1">
          <Button
            size="1"
            variant="ghost"
            color="gray"
            disabled={pending}
            onClick={() => {
              setDraft(note);
              setError("");
              setEditing(false);
            }}
          >
            取消
          </Button>
          <Button size="1" loading={pending} disabled={!dirty} onClick={() => void save()}>
            保存
          </Button>
        </Flex>
      </Flex>
      {error && <Text size="1" color="red">{error}</Text>}
    </Flex>
  );
}

function findWorstSeverity(incidents: SystemIncident[]): SystemIncidentSeverity | null {
  let worst: SystemIncidentSeverity | null = null;
  for (const incident of incidents) {
    if (worst === null || severityRank(incident.severity) > severityRank(worst)) {
      worst = incident.severity;
    }
  }
  return worst;
}

function severityRank(severity: SystemIncidentSeverity): number {
  if (severity === "down") return 3;
  if (severity === "serious") return 2;
  return 1;
}

function severityLabel(severity: SystemIncidentSeverity): string {
  if (severity === "down") return "不可用";
  if (severity === "serious") return "严重受损";
  return "波动";
}

function incidentStateLabel(incident: SystemIncident): "进行中" | "已恢复" | "停止监测" {
  if (!incident.resolved_at) return "进行中";
  if (incident.resolution === "stale_closed") return "停止监测";
  return "已恢复";
}

function resolutionLabel(resolution: SystemIncidentResolution): string {
  if (resolution === "auto_recovered") return "自动恢复";
  if (resolution === "stale_closed") return "停止监测";
  if (resolution === "manual") return "人工结束";
  return "";
}

function formatAvailability(value: number | null): string {
  return value === null || !Number.isFinite(value) ? "—" : `${value.toFixed(2)}%`;
}

function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  const whole = Math.floor(seconds);
  const days = Math.floor(whole / 86_400);
  const hours = Math.floor((whole % 86_400) / 3_600);
  const minutes = Math.floor((whole % 3_600) / 60);
  const remainingSeconds = whole % 60;
  if (days > 0) return `${days}d ${String(hours).padStart(2, "0")}h`;
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, "0")}m`;
  if (minutes > 0) return `${minutes}m ${String(remainingSeconds).padStart(2, "0")}s`;
  return `${remainingSeconds}s`;
}

function formatTimestamp(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString("zh-CN", { hour12: false });
}

function emptyText(status: SystemIncidentStatusFilter): string {
  if (status === "open") return "当前没有进行中的系统异常";
  if (status === "resolved") return "没有匹配的已恢复系统异常";
  return "没有匹配的系统异常";
}
