import { Box, Card, Container, Flex, Heading, Separator, Text, Tooltip } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import i18n, { localeTag } from "../i18n";
import { AlertTriangle, CheckCircle2, ChevronDown, ExternalLink, HelpCircle, Info, XCircle } from "lucide-react";
import { publicApi } from "../api/public";
import AvailabilityDots, { AvailabilityLegend, stateLabel, stateTone } from "../components/AvailabilityDots";
import type { AvailabilityTone } from "../components/AvailabilityDots";
import type {
  PublicStatusBucket,
  PublicStatusGroup,
  PublicStatusIncident,
  PublicStatusModel,
  PublicStatusState,
  PublicUpstream,
  PublicUpstreamProvider,
  PublicUpstreamState,
} from "../api/endpoints";

// The public page reports observed business traffic only. Synthetic probes,
// budgets and routing diagnostics are intentionally absent from this view.
//
// Groups are the top level because a group is the unit that actually has an
// upstream fleet behind it. The per-model breakdown lives inside its group and
// is never pooled across groups: the same model name routed through two groups
// can be healthy in one and down in the other.
export default function StatusPage() {
  const { t } = useTranslation();
  const { data, isLoading, error } = useQuery({
    queryKey: ["public", "status"],
    queryFn: publicApi.publicStatus,
    refetchInterval: 30_000,
  });
  const [expanded, setExpanded] = useState<string | null>(null);

  const groups = data?.groups ?? [];
  const incidents = data?.incidents ?? [];
  const upstream = data?.upstream ?? null;
  const updated = data?.updated_at ? new Date(data.updated_at) : null;
  const windowHours = data?.window_hours ?? 24;
  // The state light's window is a backend constant that has moved before, so
  // every "最近 N 分钟" on this page reads it off the payload. Groups all carry
  // the same value; the fallback only covers the empty/loading render.
  const stateWindowMinutes = groups.find((g) => g.state_window_minutes > 0)?.state_window_minutes ?? 30;
  // Same predicate UpstreamRow uses to unfold its detail. While every vendor is
  // green this panel is reference material and belongs under the fold, but a
  // vendor outage is usually the explanation for whatever brought someone to
  // this page — leaving it at the bottom, below every group card, buries the
  // answer. So it moves to the top for exactly as long as it matters.
  const upstreamImpaired = (upstream?.providers ?? []).some(
    (p) => p.state !== "operational" && p.state !== "unknown",
  );

  return (
    <Container size="3" px="4" className="status-page">
      <Box className="status-page-head">
        <Heading className="status-page-title">{t("pub.status.title")}</Heading>
        <Text size="2" color="gray" as="div">
          {updated ? t("pub.status.updated", { time: updated.toLocaleString(localeTag()), hours: windowHours }) : t("pub.status.loading")}
        </Text>
      </Box>

      {!isLoading && groups.length > 0 && (
        <IncidentPanel incidents={incidents} stateWindowMinutes={stateWindowMinutes} />
      )}
      {!isLoading && upstream && upstreamImpaired && (
        <UpstreamPanel upstream={upstream} prominent />
      )}
      <Separator size="4" my="5" />

      {isLoading && <Text size="2" color="gray">{t("pub.status.loadingShort")}</Text>}
      {error && <Text size="2" color="red">{t("pub.status.loadError")}</Text>}
      {!isLoading && groups.length === 0 && (
        <Card className="status-empty">
          <Flex direction="column" align="center" gap="2" p="4">
            <Text size="3">{t("pub.status.emptyTitle")}</Text>
            <Text size="1" color="gray">{t("pub.status.emptyBody")}</Text>
          </Flex>
        </Card>
      )}

      {groups.length > 0 && (
        <Box>
          <Flex align="baseline" justify="between" mb="2" gap="3" wrap="wrap">
            <Text size="3" weight="medium">{t("pub.status.groupAvailability")}</Text>
            <Text size="1" color="gray">{t("pub.status.expandHint")}</Text>
          </Flex>
          <Box mb="3"><AvailabilityLegend /></Box>
          <Flex direction="column" gap="3">
            {groups.map((group) => (
              <GroupCard
                key={group.id}
                group={group}
                windowHours={windowHours}
                open={expanded === group.id}
                onToggle={() => setExpanded(expanded === group.id ? null : group.id)}
              />
            ))}
          </Flex>
        </Box>
      )}

      {!isLoading && upstream && !upstreamImpaired && <UpstreamPanel upstream={upstream} />}
    </Container>
  );
}

// The panel deliberately reports only what the traffic proves. A quiet group
// (unknown) is not an incident: no requests is not the same as failed requests,
// and surfacing it as one trains people to ignore the panel.
function IncidentPanel({ incidents, stateWindowMinutes }: { incidents: PublicStatusIncident[]; stateWindowMinutes: number }) {
  const { t } = useTranslation();
  if (incidents.length === 0) {
    return (
      <Card className="status-incidents status-incidents-clear">
        <Flex align="center" gap="3" p="3">
          <CheckCircle2 size={18} className="status-incident-icon-ok" aria-hidden />
          <Text size="4" weight="bold" as="div">{t("pub.status.allClear")}</Text>
        </Flex>
      </Card>
    );
  }
  return (
    <Card className="status-incidents">
      <Flex align="center" gap="3" p="3" className="status-incidents-head">
        <AlertTriangle size={18} className="status-incident-icon-warn" aria-hidden />
        <Text size="4" weight="bold" as="div">{t("pub.status.hasIssues")}</Text>
      </Flex>
      <Flex direction="column">
        {incidents.map((incident) => (
          <IncidentRow key={incident.group_id} incident={incident} stateWindowMinutes={stateWindowMinutes} />
        ))}
      </Flex>
    </Card>
  );
}

// The vendor mirror. This sits at the bottom, below our own numbers, because
// it answers a weaker question: not "is the service working" but "does the
// vendor think it is". The two disagree routinely — an upstream reading green
// while our success rate falls is the ordinary case — so the panel is labelled
// as self-reported and never blended into the incident list above.
//
// Healthy providers collapse to a single line. The component and incident
// detail only unfolds when something is actually wrong, which keeps ~40
// permanently-green vendor subsystems off the page.
// prominent renders the same panel above the fold, used while a vendor is
// reporting trouble. Only the spacing and the heading change: the caller decides
// placement, this component does not re-judge the state.
function UpstreamPanel({ upstream, prominent = false }: { upstream: PublicUpstream; prominent?: boolean }) {
  const { t } = useTranslation();
  if (upstream.providers.length === 0) return null;
  return (
    <Box mt={prominent ? "4" : "6"}>
      <Flex align="baseline" justify="between" mb="2" gap="3" wrap="wrap">
        <Text size="3" weight="medium">
          {prominent ? t("pub.status.upstreamTitleIssue") : t("pub.status.upstreamTitle")}
        </Text>
        <Text size="1" color="gray">{t("pub.status.upstreamNote")}</Text>
      </Flex>
      <Card className="status-incidents">
        <Flex direction="column">
          {upstream.providers.map((p) => (
            <UpstreamRow key={p.key} provider={p} />
          ))}
        </Flex>
      </Card>
    </Box>
  );
}

function UpstreamRow({ provider }: { provider: PublicUpstreamProvider }) {
  const { t } = useTranslation();
  const meta = upstreamMeta(provider.state);
  const unresolved = provider.state !== "operational" && provider.state !== "unknown";
  return (
    <Flex gap="3" p="3" className="status-incident-row">
      <span className={`status-incident-dot status-incident-dot-${meta.color}`} aria-hidden />
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Flex align="baseline" gap="2" wrap="wrap">
          <Text size="3" weight="medium" as="span">{provider.name}</Text>
          <Text size="2" color="gray" as="span">{meta.label}</Text>
          <a className="status-upstream-link" href={provider.page_url} target="_blank" rel="noopener noreferrer">
            {t("pub.status.officialPage")}
            <ExternalLink size={12} aria-hidden />
          </a>
        </Flex>

        {provider.state === "unknown" ? (
          // Deliberately not "正常". A mirror we could not refresh says
          // nothing about the vendor, and defaulting to green would mislead
          // in exactly the situation this panel exists for.
          <Text size="2" color="gray" as="div" mt="1">{t("pub.status.upstreamFetchFailed")}</Text>
        ) : (
          <>
            {provider.description && (
              <Text size="2" color="gray" as="div" mt="1">{provider.description}</Text>
            )}
            {unresolved && provider.components.length > 0 && (
              <Text size="2" color="gray" as="div" mt="1">
                {t("pub.status.affected", {
                  list: provider.components
                    .map((c) => t("pub.status.affectedItem", { name: c.name, status: componentStatusLabel(c.status) }))
                    .join(t("pub.status.listSep")),
                })}
              </Text>
            )}
            {unresolved && provider.incidents.map((inc) => (
              <Box key={inc.id} mt="2" className="status-upstream-incident">
                <Text size="2" weight="medium" as="div">
                  {inc.name}
                  <Text size="1" color="gray" as="span" ml="2">
                    {incidentStatusLabel(inc.status)} · {impactLabel(inc.impact)}
                  </Text>
                </Text>
                {inc.latest_update && (
                  <Text size="1" color="gray" as="div" mt="1">{inc.latest_update}</Text>
                )}
                <Text size="1" color="gray" as="div" mt="1">
                  {t("pub.status.incidentStarted", { time: new Date(inc.started_at).toLocaleString(localeTag()) })}
                </Text>
              </Box>
            ))}
            {provider.fetched_at && (
              <Text size="1" color="gray" as="div" mt="1">
                {t("pub.status.syncedAt", { time: new Date(provider.fetched_at).toLocaleString(localeTag()) })}
              </Text>
            )}
          </>
        )}
      </Box>
    </Flex>
  );
}

function upstreamMeta(state: PublicUpstreamState): { label: string; color: string } {
  switch (state) {
    case "operational": return { label: i18n.t("pub.status.upstream.operational"), color: "green" };
    case "degraded": return { label: i18n.t("pub.status.upstream.degraded"), color: "amber" };
    case "outage": return { label: i18n.t("pub.status.upstream.outage"), color: "red" };
    case "maintenance": return { label: i18n.t("pub.status.upstream.maintenance"), color: "gray" };
    default: return { label: i18n.t("pub.status.upstream.unknown"), color: "gray" };
  }
}

// The vendor strings pass through raw from the API so the wording lives here
// rather than in Go. Unrecognized values render as-is: a vendor adding a new
// status should show up verbatim rather than be swallowed by a default.
function componentStatusLabel(status: string): string {
  switch (status) {
    case "degraded_performance": return i18n.t("pub.status.component.degraded_performance");
    case "partial_outage": return i18n.t("pub.status.component.partial_outage");
    case "major_outage": return i18n.t("pub.status.component.major_outage");
    case "under_maintenance": return i18n.t("pub.status.component.under_maintenance");
    default: return status;
  }
}

function incidentStatusLabel(status: string): string {
  switch (status) {
    case "investigating": return i18n.t("pub.status.incident.investigating");
    case "identified": return i18n.t("pub.status.incident.identified");
    case "monitoring": return i18n.t("pub.status.incident.monitoring");
    default: return status;
  }
}

function impactLabel(impact: string): string {
  switch (impact) {
    case "critical": return i18n.t("pub.status.impact.critical");
    case "major": return i18n.t("pub.status.impact.major");
    case "minor": return i18n.t("pub.status.impact.minor");
    case "none": return i18n.t("pub.status.impact.none");
    default: return impact;
  }
}

function IncidentRow({ incident, stateWindowMinutes }: { incident: PublicStatusIncident; stateWindowMinutes: number }) {
  const { t } = useTranslation();
  const meta = stateMeta(incident.state, "group");
  const pct = incident.success_pct == null ? null : `${incident.success_pct.toFixed(1)}%`;
  return (
    <Flex gap="3" p="3" className="status-incident-row">
      <span className={`status-incident-dot status-incident-dot-${meta.color}`} aria-hidden />
      <Box style={{ flex: 1 }}>
        <Text size="3" weight="medium" as="div">
          {incident.group_name} · {meta.label}
        </Text>
        <Text size="2" color="gray" as="div" mt="1">
          {pct
            ? t("pub.status.recentRate", { minutes: stateWindowMinutes, pct })
            : t("pub.status.recentNoSamples", { minutes: stateWindowMinutes })}
        </Text>
        <Text size="1" color="gray" as="div" mt="1">
          {t("pub.status.autoDetected")} · {incidentDuration(incident.started_at)}
        </Text>
      </Box>
    </Flex>
  );
}

// started_at comes off the bucket grid, so it is only accurate to one bucket
// (window/48 — 30 minutes on the default 24h window). Round the label down to
// coarse units rather than implying a precision the source does not have.
function incidentDuration(startedAt: string | null): string {
  if (!startedAt) return i18n.t("pub.status.duration.beyondWindow");
  const minutes = Math.floor((Date.now() - new Date(startedAt).getTime()) / 60_000);
  if (!Number.isFinite(minutes) || minutes < 0) return i18n.t("pub.status.duration.justStarted");
  if (minutes < 15) return i18n.t("pub.status.duration.justStarted");
  if (minutes < 60) return i18n.t("pub.status.duration.minutes", { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return i18n.t("pub.status.duration.hours", { count: hours });
  return i18n.t("pub.status.duration.days", { count: Math.floor(hours / 24) });
}

function GroupCard({ group, open, onToggle, windowHours }: { group: PublicStatusGroup; open: boolean; onToggle: () => void; windowHours: number }) {
  const { t } = useTranslation();
  const models = group.models ?? [];
  return (
    <Card className="status-group">
      <StatusSummary item={group} windowHours={windowHours} />
      {group.description && (
        <Text size="1" color="gray" as="div" mt="1">{group.description}</Text>
      )}

      {models.length > 0 && (
        <>
          <button type="button" className="status-group-toggle" onClick={onToggle} aria-expanded={open}>
            <Text size="1" color="gray">{open ? t("pub.status.hideModels") : t("pub.status.showModels", { count: models.length })}</Text>
            <ChevronDown
              size={14}
              aria-hidden
              style={{ transform: open ? "rotate(180deg)" : undefined, transition: "transform var(--dur-fast) var(--ease-out)" }}
            />
          </button>
          {open && (
            <Flex direction="column" className="status-model-list">
              {models.map((model) => <ModelRow key={model.id} model={model} />)}
            </Flex>
          )}
        </>
      )}
    </Card>
  );
}

// One line per model: state, name, dot strip, rate. The long-form detail moves
// into the row title so scanning twenty models stays a single vertical read.
function ModelRow({ model }: { model: PublicStatusModel }) {
  const { t } = useTranslation();
  const percentage = model.availability_pct == null ? "—" : `${model.availability_pct.toFixed(2)}%`;
  const detail = [
    stateMeta(model.state).label,
    t("pub.status.stateWindow", { minutes: model.state_window_minutes }),
    model.last_observed_at ? t("pub.status.lastObserved", { time: new Date(model.last_observed_at).toLocaleString(localeTag()) }) : "",
  ].filter(Boolean).join(" · ");
  return (
    <Flex className="status-model-row" align="center" gap="3" title={detail}>
      <StateIcon state={model.state} size={13} />
      <Text size="2" className="status-model-name">{model.name}</Text>
      <AvailabilityDots buckets={model.buckets ?? []} columns={24} size={4} />
      <Text size="2" weight="medium" className="status-model-pct">{percentage}</Text>
    </Flex>
  );
}

// The icon and the percentage answer different questions over different windows —
// "is it working now" (state_window_minutes) versus "how did the window go"
// (hours). Shown unlabelled they read as a contradiction: a green tick beside
// 79% looks like the page disagreeing with itself. Each side names its own
// window, so both facts survive and neither is mistaken for the other.
function StatusSummary({ item, windowHours }: { item: PublicStatusGroup; windowHours: number }) {
  const { t } = useTranslation();
  const meta = stateMeta(item.state, "group");
  const percentage = item.availability_pct == null ? "—" : `${item.availability_pct.toFixed(2)}%`;
  return (
    <>
      <Flex justify="between" align="center" gap="3" wrap="wrap">
        <Flex align="center" gap="2">
          <StateIcon state={item.state} size={16} />
          <Text size="3" weight="medium">{item.name}</Text>
          <Text size="2" color="gray">{meta.label}</Text>
          <Tooltip content={meta.hint(item.state_window_minutes)}>
            <Info size={14} style={{ color: "var(--gray-9)", cursor: "help", flexShrink: 0 }} />
          </Tooltip>
        </Flex>
        <Flex align="baseline" gap="2">
          <Text size="3" weight="medium">{percentage}</Text>
          <Text size="2" color="gray">{t("pub.status.windowRate", { hours: windowHours })}</Text>
        </Flex>
      </Flex>

      <Box mt="3"><UptimeBar buckets={item.buckets ?? []} /></Box>

      <Text size="1" color="gray" as="div" mt="3">
        {t("pub.status.stateWindow", { minutes: item.state_window_minutes })}
      </Text>
      {item.last_observed_at && (
        <Text size="1" color="gray" as="div" mt="1">
          {t("pub.status.lastObservedLong", { time: new Date(item.last_observed_at).toLocaleString(localeTag()) })}
          {item.fresh ? ` · ${t("pub.status.fresh")}` : ""}
        </Text>
      )}
    </>
  );
}

// Shorter than it was (32px): with severity capped at three tones, the extra
// height bought no information and 96 saturated full-height cells read as an
// alarm even when the bar is mostly green. The state icon, label and percentage
// carry the verdict; the bar only has to show *when*.
function UptimeBar({ buckets }: { buckets: PublicStatusBucket[] }) {
  return (
    <Flex gap="1" align="stretch" style={{ height: 22, width: "100%" }}>
      {buckets.map((bucket, index) => <BucketCell key={index} bucket={bucket} />)}
    </Flex>
  );
}

// The cell colours straight off the state the backend graded — the client owns
// no thresholds of its own, so an orange cell can never contradict the label
// beside it.
function BucketCell({ bucket }: { bucket: PublicStatusBucket }) {
  useTranslation();
  const tone = stateTone(bucket.state);
  const cssVar = tone === "gray" ? "var(--gray-6)" : TONE_VAR[tone];
  const time = new Date(bucket.time);
  const label = `${time.toLocaleString(localeTag())} · ${stateLabel(bucket.state)}`;
  return (
    <Tooltip content={label}>
      <Box style={{ flex: 1, minWidth: 2, borderRadius: 2, background: cssVar, cursor: "default" }} aria-label={label} />
    </Tooltip>
  );
}

// Shape carries the verdict alongside hue: degraded and serious share the
// triangle but differ in colour, while healthy/down/unknown each have their own
// glyph. A reader who cannot separate amber from orange still sees "not a tick
// and not a cross" and reads the label beside it.
function StateIcon({ state, size = 16 }: { state: PublicStatusState; size?: number }) {
  const { color } = stateMeta(state);
  const common = { size, style: { color: TONE_VAR[color], flexShrink: 0 }, "aria-label": state };
  if (state === "healthy") return <CheckCircle2 {...common} />;
  if (state === "degraded" || state === "serious") return <AlertTriangle {...common} />;
  if (state === "down") return <XCircle {...common} />;
  return <HelpCircle {...common} />;
}

// Labels and hints are the secondary encoding the palette's contrast WARN
// requires: the tone never carries the verdict alone.
//
// hint takes the window rather than closing over a constant, because the state
// window is a backend setting: copy that names a number has to read it from
// state_window_minutes or it will quietly go stale the next time it moves.
const STATE_COLOR: Record<PublicStatusState, AvailabilityTone> = {
  healthy: "green",
  degraded: "amber",
  serious: "orange",
  down: "red",
  unknown: "gray",
};

function stateMeta(state: PublicStatusState, scope: "group" | "model" = "model"): { label: string; color: AvailabilityTone; hint: (minutes: number) => string } {
  const known: PublicStatusState = state in STATE_COLOR ? state : "unknown";
  return {
    label: i18n.t(`pub.status.state.${known}`),
    color: STATE_COLOR[known],
    hint: (minutes) => i18n.t(`pub.status.hint.${scope}.${known}`, { minutes }),
  };
}

// One place mapping a tone to its CSS variable, so the bar, the dots, the icons
// and the incident rows can never disagree about what "orange" looks like.
const TONE_VAR: Record<AvailabilityTone, string> = {
  green: "var(--c-status-ok)",
  amber: "var(--c-status-warn)",
  orange: "var(--c-status-serious)",
  red: "var(--c-status-critical)",
  gray: "var(--c-status-none)",
};
