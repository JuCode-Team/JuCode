import { useTranslation } from "react-i18next";
import type { PublicStatusBucket, PublicStatusState } from "../api/endpoints";
import i18n, { localeTag } from "../i18n";

export type AvailabilityTone = "green" | "amber" | "orange" | "red" | "gray";

// The tier table, in one place, so the legend can never drift from the marks it
// explains. Keyed by the state the backend already graded, so the client no
// longer owns any thresholds: it only chooses the colour and the wording.
//
// The four hues were picked by enumerating Radix's green/warm scales against the
// palette validator, not by eye. Worst adjacent pair (amber↔orange) lands at CVD
// ΔE 15.8 and normal-vision ΔE 20.3, clearing both floors in light and dark.
// Two things that look reasonable and are NOT: keeping red-9 (#e5484d) as the
// bottom tier, which sits at normal ΔE 9.8 from orange-9 — indistinguishable, so
// the bottom tier is the deeper #b02a37; and Radix step 11, which inverts to a
// pale tint in dark mode and would render "critical" as pink.
//
// Ordered worst-last, so severity() below can read the rank straight off the
// index instead of keeping a second table in step with this one.
export const AVAILABILITY_TIERS: {
  state: PublicStatusState;
  tone: AvailabilityTone;
}[] = [
  { state: "unknown", tone: "gray" },
  { state: "healthy", tone: "green" },
  { state: "degraded", tone: "amber" },
  { state: "serious", tone: "orange" },
  { state: "down", tone: "red" },
];

// The legend reads worst-first, matching how the rest of the page ramps from
// healthy down. Only the display order differs from the table above.
const LEGEND_ORDER: PublicStatusState[] = ["healthy", "degraded", "serious", "down", "unknown"];

const TIER_BY_STATE = new Map(AVAILABILITY_TIERS.map((tier) => [tier.state, tier]));
const RANK_BY_STATE = new Map(AVAILABILITY_TIERS.map((tier, index) => [tier.state, index]));

export function stateTone(state: PublicStatusState): AvailabilityTone {
  return TIER_BY_STATE.get(state)?.tone ?? "gray";
}

// Wording lives in pub.avail.{label,hint}.<state>; unrecognised states read as unknown.
function knownState(state: PublicStatusState): PublicStatusState {
  return TIER_BY_STATE.has(state) ? state : "unknown";
}

export function stateLabel(state: PublicStatusState): string {
  return i18n.t(`pub.avail.label.${knownState(state)}`);
}

export function stateHint(state: PublicStatusState): string {
  return i18n.t(`pub.avail.hint.${knownState(state)}`);
}

// unknown ranks below healthy, so folding several buckets into one dot drops it
// the moment any neighbour carries a real verdict — a dot only reads "数据不足"
// when every bucket behind it did.
function severity(state: PublicStatusState): number {
  return RANK_BY_STATE.get(state) ?? 0;
}

function worstState(states: PublicStatusState[]): PublicStatusState {
  let worst: PublicStatusState = "unknown";
  for (const state of states) {
    if (severity(state) > severity(worst)) worst = state;
  }
  return worst;
}

interface Slot {
  state: PublicStatusState;
  from: Date | null;
}

// Condense the backend's fixed bucket grid (48) down to however many dots fit.
// Slot boundaries are computed from the ratio rather than a fixed chunk size,
// so the last dot covers the same span as the rest instead of a short tail.
// Prefer a column count that divides the grid evenly (24 → two buckets per dot);
// an indivisible count still works but gives neighbouring dots unequal spans,
// which makes the strip lie slightly about time.
//
// Merging takes the worst state rather than averaging: a dot standing for half
// an hour of downtime must not be diluted to green by the healthy bucket beside
// it, because the strip exists to show *when* things broke.
function condense(buckets: PublicStatusBucket[], columns: number): Slot[] {
  const n = buckets.length;
  if (n === 0) return [];
  const slots: Slot[] = [];
  for (let i = 0; i < columns; i++) {
    const start = Math.floor((i * n) / columns);
    const end = Math.max(start + 1, Math.floor(((i + 1) * n) / columns));
    const slot: Slot = { state: "unknown", from: null };
    for (let j = start; j < end && j < n; j++) {
      if (severity(buckets[j].state) > severity(slot.state)) slot.state = buckets[j].state;
      if (slot.from == null) slot.from = new Date(buckets[j].time);
    }
    slots.push(slot);
  }
  return slots;
}

function slotLabel(slot: Slot): string {
  const when = slot.from ? slot.from.toLocaleString(localeTag()) : "";
  return when ? `${when} · ${stateLabel(slot.state)}` : stateLabel(slot.state);
}

/* Names every tone the bars and dots can take. Required rather than decorative:
   a status colour must never be the only carrier of its meaning, and with
   severity capped at three bands the legend is where a reader learns that gray
   means "not enough traffic to say" rather than "no traffic". */
export function AvailabilityLegend() {
  useTranslation();
  return (
    <ul className="avail-legend">
      {LEGEND_ORDER.map((state) => TIER_BY_STATE.get(state)!).map((tier) => (
        <li key={tier.tone} className="avail-legend-item" title={stateHint(tier.state)}>
          <span className="avail-legend-swatch" data-tone={tier.tone} aria-hidden />
          <span className="avail-legend-label">{stateLabel(tier.state)}</span>
        </li>
      ))}
    </ul>
  );
}

/* A dense strip of small dots — one per time slot, colored by that slot's
   success rate. It is deliberately supplementary: the state icon, label and
   percentage sit beside it and carry the meaning on their own, so a
   red-green colorblind reader never depends on the hue here. */
export default function AvailabilityDots({
  buckets,
  columns = 24,
  size = 5,
}: {
  buckets: PublicStatusBucket[];
  columns?: number;
  size?: number;
}) {
  const { t } = useTranslation();
  const slots = condense(buckets ?? [], columns);
  if (slots.length === 0) return null;
  // A screen reader cannot scan 24 dots, so the strip announces the one thing
  // the shape is there to convey: how bad it got inside the window.
  const worst = worstState(slots.map((s) => s.state));
  const summary = worst === "unknown"
    ? t("pub.avail.summaryUnknown", { count: slots.length })
    : t("pub.avail.summary", { count: slots.length, worst: stateLabel(worst) });
  return (
    <span
      className="avail-dots"
      role="img"
      aria-label={summary}
      style={{ ["--avail-dot" as string]: `${size}px` }}
    >
      {slots.map((slot, index) => (
        <span
          key={index}
          className="avail-dot"
          data-tone={stateTone(slot.state)}
          title={slotLabel(slot)}
        />
      ))}
    </span>
  );
}
