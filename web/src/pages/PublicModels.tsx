import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Trans, useTranslation } from "react-i18next";
import i18n from "../i18n";
import { ChevronDown, ChevronUp, RotateCw, Search } from "lucide-react";
import ModelBrandIcon from "../components/ModelBrandIcon";
import { http } from "../api/client";
import { publicApi } from "../api/public";
import { type PriceUnit, UNIT_SUFFIX, currencySymbol } from "../lib/price";
import { PointIcon } from "../components/PointAmount";
import AvailabilityDots, { stateLabel } from "../components/AvailabilityDots";
import { formatMultiplier } from "../lib/groupUsage";
import type { PublicStatusModel } from "../api/endpoints";

interface PublicModel {
  name: string;
  billing_mode: "per_request" | "per_token";
  currency: string;
  description?: string;
  price_per_request?: string;
  input_unit_price?: string;
  output_unit_price?: string;
  cache_read_input_price?: string;
  cache_write_input_price?: string;
  cache_write_1h_input_price?: string;
  image_input_unit_price?: string;
  image_output_unit_price?: string;
  audio_input_unit_price?: string;
  audio_output_unit_price?: string;
  output_image_price?: string;
  vendor?: { id: string; name: string; slug: string };
  groups?: PublicModelGroup[];
}

interface PublicModelGroup {
  id: string;
  name: string;
  billing_source: "plan_only" | "balance_only";
  rate_multiplier: number;
  description?: string;
}

type ExtraPriceKey =
  | "cache_read_input_price"
  | "cache_write_input_price"
  | "cache_write_1h_input_price"
  | "image_input_unit_price"
  | "image_output_unit_price"
  | "audio_input_unit_price"
  | "audio_output_unit_price"
  | "output_image_price";

// Secondary prices: shown collapsed so every card keeps the same silhouette
// (name / two prices / group + availability) and only grows on request.
// 标签取 pub.models.extra.<key>。
const EXTRA_PRICES: { key: ExtraPriceKey; perRequest?: boolean }[] = [
  { key: "cache_read_input_price" },
  { key: "cache_write_input_price" },
  { key: "cache_write_1h_input_price" },
  { key: "image_input_unit_price" },
  { key: "image_output_unit_price" },
  { key: "audio_input_unit_price" },
  { key: "audio_output_unit_price" },
  { key: "output_image_price", perRequest: true },
];

const ALL_VENDORS = "__all__";
// Per-section group choice: "lowest" means each card quotes whichever
// balance group gives it the smallest multiplier.
const LOWEST = "__lowest__";
const OTHER_VENDOR = "__other__";

function isSet(value: string | undefined | null): value is string {
  return value != null && value !== "";
}

function fmtNumber(value: number): string {
  const decimals = Math.abs(value) >= 100 ? 2 : Math.abs(value) >= 1 ? 4 : 6;
  return value.toFixed(decimals).replace(/\.?0+$/, "");
}

// Stored unit prices are per-1K tokens in platform points (1 积分 = 1 元);
// only the K→M scale and the group multiplier apply here, never FX.
function fmtTokenPrice(perK: string | undefined | null, unit: PriceUnit, rate = 1): string {
  if (!isSet(perK)) return "";
  const n = parseFloat(perK);
  if (!Number.isFinite(n)) return "";
  return fmtNumber((unit === "M" ? n * 1000 : n) * rate);
}

function fmtPerRequest(amount: string | undefined | null, rate = 1): string {
  if (!isSet(amount)) return "";
  const n = Number(amount);
  if (!Number.isFinite(n)) return "";
  return fmtNumber(n * rate);
}

// Plan-only groups are excluded everywhere on this page: their price is
// unreachable with balance, so a quote from them would be a price nobody
// can pay. Plan contents live on /me/plans.
function balanceGroups(m: PublicModel): PublicModelGroup[] {
  return (m.groups ?? []).filter((g) => g.billing_source !== "plan_only");
}

function lowestGroup(m: PublicModel): PublicModelGroup | undefined {
  return [...balanceGroups(m)].sort((a, b) => a.rate_multiplier - b.rate_multiplier)[0];
}

// The one number a buyer reads first. Compares the two columns numerically,
// the same way the card does; a USD-priced model saves more in real terms
// than this says, so the figure is conservative rather than inflated.
// Only the figure is set in mono; the Han label stays in the body face.
function savingBadge(rate: number): { text: ReactNode; tone: "save" | "flat" } {
  if (rate < 1) {
    const pct = ((1 - rate) * 100).toFixed(1).replace(/\.0$/, "");
    return { text: <Trans i18nKey="pub.models.save" values={{ pct }} components={{ b: <b /> }} />, tone: "save" };
  }
  if (rate === 1) return { text: i18n.t("pub.models.sameAsOfficial"), tone: "flat" };
  return { text: <Trans i18nKey="pub.models.officialTimes" values={{ rate }} components={{ b: <b /> }} />, tone: "flat" };
}

const nameCollator = new Intl.Collator("en", { numeric: true, sensitivity: "base" });

/* Reveals a section once it enters the viewport; the cards inside stagger
   off this single flag via their --i index, so one observer per vendor
   section is all the choreography needs. */
function useReveal<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [inView, setInView] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el || inView) return;
    if (typeof IntersectionObserver === "undefined") {
      setInView(true);
      return;
    }
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setInView(true);
          io.disconnect();
        }
      },
      { rootMargin: "0px 0px -8% 0px" },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [inView]);
  return { ref, inView };
}

function Struck({ children, plain }: { children: ReactNode; plain?: boolean }) {
  return plain
    ? <span className="cs-price-official is-plain">{children}</span>
    : <s className="cs-price-official">{children}</s>;
}

function PriceRow({ label, official, site, primary, muted }: {
  label: string;
  official: string;
  site: string;
  primary?: boolean;
  muted?: boolean;
}) {
  const cls = ["cs-price-row", primary ? "is-primary" : "", muted ? "is-muted" : ""].filter(Boolean).join(" ");
  return (
    <div className={cls}>
      <span className="cs-price-label">{label}</span>
      {site ? <Struck>{official || "—"}</Struck> : <Struck plain>{official || "—"}</Struck>}
      <span className="cs-price-site">
        {site ? (<><PointIcon size={12} />{site}</>) : ""}
      </span>
    </div>
  );
}

/* One model, one group. The struck official price sits beside the points
   price on every row; output is the row people compare, so it is the
   large one. */
function ShowcaseCard({ m, group, unit, health, windowHours, index, onGroupPick }: {
  m: PublicModel;
  group: PublicModelGroup | undefined;
  unit: PriceUnit;
  health: PublicStatusModel | undefined;
  windowHours: number;
  index: number;
  onGroupPick: (groupId: string) => void;
}) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const rate = group?.rate_multiplier ?? 1;
  const symbol = currencySymbol(m.currency);
  const badge = group ? savingBadge(rate) : null;
  const extras = EXTRA_PRICES.filter((e) => isSet(m[e.key]));
  const perRequest = m.billing_mode === "per_request";
  const quote = (value: string | undefined, isRequest: boolean) =>
    isRequest ? fmtPerRequest(value, rate) : fmtTokenPrice(value, unit, rate);
  const list = (value: string | undefined, isRequest: boolean) =>
    isRequest ? fmtPerRequest(value) : fmtTokenPrice(value, unit);
  const withSymbol = (text: string) => (text ? `${symbol}${text}` : "");
  // Without a balance group there is nothing to compare against, so the
  // rows show the list price alone and no site price.
  const site = (value: string | undefined, isRequest: boolean) => (group ? quote(value, isRequest) : "");

  const pct = health?.availability_pct == null ? "—" : `${health.availability_pct.toFixed(2)}%`;
  const healthTitle = health
    ? [
        group ? t("pub.models.groupName", { name: group.name }) : "",
        stateLabel(health.state),
        t("pub.models.stateWindow", { count: health.state_window_minutes }),
      ].filter(Boolean).join(" · ")
    : undefined;

  return (
    <article className="cs-card" style={{ ["--i" as string]: index }}>
      <div className="cs-card-head">
        <span className="cs-card-mark" aria-hidden><ModelBrandIcon model={m.name} size={18} mono /></span>
        <div className="cs-card-title">
          <h3 className="cs-card-name" title={m.name}>{m.name}</h3>
          <p className="cs-card-desc" title={m.description || undefined}>{m.description || ""}</p>
        </div>
        {badge && <span className="cs-badge" data-tone={badge.tone}>{badge.text}</span>}
      </div>

      <div className="cs-price">
        <div className="cs-price-head">
          <span>{perRequest ? t("pub.models.perCall") : <code>{UNIT_SUFFIX[unit]}</code>}</span>
          <span>{t("pub.models.official")}</span>
          <span>{group ? t("pub.models.ours") : ""}</span>
        </div>
        {perRequest ? (
          <PriceRow
            label={t("pub.models.perRequest")}
            official={withSymbol(list(m.price_per_request, true))}
            site={site(m.price_per_request, true)}
            primary
          />
        ) : (
          <>
            <PriceRow
              label={t("pub.models.input")}
              official={withSymbol(list(m.input_unit_price, false))}
              site={site(m.input_unit_price, false)}
            />
            <PriceRow
              label={t("pub.models.output")}
              official={withSymbol(list(m.output_unit_price, false))}
              site={site(m.output_unit_price, false)}
              primary
            />
          </>
        )}
        {expanded && extras.map((e) => (
          <PriceRow
            key={e.key}
            label={t(`pub.models.extra.${e.key}`)}
            official={withSymbol(list(m[e.key], !!e.perRequest))}
            site={site(m[e.key], !!e.perRequest)}
            muted
          />
        ))}
        {extras.length > 0 && (
          <button type="button" className="cs-more" onClick={() => setExpanded((v) => !v)} aria-expanded={expanded}>
            {expanded ? t("pub.models.collapse") : t("pub.models.morePrices", { count: extras.length })}
            {expanded ? <ChevronUp size={12} /> : <ChevronDown size={12} />}
          </button>
        )}
      </div>

      {(group || health) && (
        <div className="cs-card-foot">
          {group ? (
            <button
              type="button"
              className="cs-group-link"
              title={group.description || t("pub.models.groupName", { name: group.name })}
              onClick={() => onGroupPick(group.id)}
            >
              <span>{group.name}</span>
              <b>{formatMultiplier(rate)}</b>
            </button>
          ) : <span />}
          {health && (
            <span className="cs-avail" title={healthTitle}>
              <AvailabilityDots buckets={health.buckets ?? []} columns={24} size={4} />
              <span>{pct}</span>
            </span>
          )}
          {!health && group && <span className="cs-avail" title={t("pub.models.noAvailability", { name: group.name, hours: windowHours })} />}
        </div>
      )}
    </article>
  );
}

interface VendorSection {
  slug: string;
  name: string;
  iconModel: string;
  models: PublicModel[];
  groups: PublicModelGroup[];
}

function VendorBlock({ section, activeGroupId, onGroupChange, unit, availability, windowHours }: {
  section: VendorSection;
  activeGroupId: string;
  onGroupChange: (groupId: string) => void;
  unit: PriceUnit;
  availability: Map<string, PublicStatusModel>;
  windowHours: number;
}) {
  const { t } = useTranslation();
  const { ref, inView } = useReveal<HTMLElement>();
  const picked = activeGroupId === LOWEST ? undefined : section.groups.find((g) => g.id === activeGroupId);
  const models = picked
    ? section.models.filter((m) => balanceGroups(m).some((g) => g.id === picked.id))
    : section.models;
  // Clicking the group already shown returns the section to its default.
  const pick = (id: string) => onGroupChange(id === activeGroupId ? LOWEST : id);

  return (
    <section ref={ref} className={`cs-section ${inView ? "is-in" : ""}`} aria-labelledby={`cs-vendor-${section.slug}`}>
      <header className="cs-section-head">
        <h2 id={`cs-vendor-${section.slug}`} className="cs-section-title">
          <ModelBrandIcon model={section.iconModel} size={20} mono />
          <span>{section.name}</span>
          <span className="cs-section-count">{section.models.length}</span>
        </h2>
        {section.groups.length === 1 && (
          <div className="cs-groups">
            <span className="cs-groups-label">{t("pub.models.group")}</span>
            <span className="cs-chip is-active" title={section.groups[0].description || undefined}>
              {section.groups[0].name}<b>{formatMultiplier(section.groups[0].rate_multiplier)}</b>
            </span>
          </div>
        )}
        {section.groups.length > 1 && (
          <div className="cs-groups" role="group" aria-label={t("pub.models.vendorGroups", { name: section.name })}>
            <span className="cs-groups-label">{t("pub.models.group")}</span>
            <button
              type="button"
              className={`cs-chip ${activeGroupId === LOWEST ? "is-active" : ""}`}
              onClick={() => onGroupChange(LOWEST)}
              title={t("pub.models.lowestHint")}
            >
              {t("pub.models.lowest")}
            </button>
            {section.groups.map((g) => (
              <button
                key={g.id}
                type="button"
                className={`cs-chip ${activeGroupId === g.id ? "is-active" : ""}`}
                onClick={() => pick(g.id)}
                title={g.description || undefined}
              >
                {g.name}<b>{formatMultiplier(g.rate_multiplier)}</b>
              </button>
            ))}
          </div>
        )}
      </header>
      <div className="cs-grid">
        {models.map((m, i) => {
          const group = picked ?? lowestGroup(m);
          return (
            <ShowcaseCard
              key={m.name}
              m={m}
              group={group}
              unit={unit}
              health={group ? availability.get(`${group.id}::${m.name}`) : undefined}
              windowHours={windowHours}
              index={i}
              onGroupPick={pick}
            />
          );
        })}
      </div>
    </section>
  );
}

/* Loading state shares the card silhouette so the page does not reflow
   when data arrives. */
function SkeletonSection() {
  return (
    <section className="cs-section cs-skeleton" aria-hidden>
      <header className="cs-section-head">
        <span className="cs-skel" style={{ width: 140, height: 20 }} />
      </header>
      <div className="cs-grid">
        {Array.from({ length: 6 }).map((_, i) => (
          <div key={i} className="cs-card">
            <div className="cs-card-head">
              <span className="cs-card-mark" />
              <div className="cs-card-title" style={{ display: "grid", gap: 8 }}>
                <span className="cs-skel" style={{ width: "60%" }} />
                <span className="cs-skel" style={{ width: "85%", height: 10 }} />
              </div>
            </div>
            <div className="cs-price" style={{ gap: 10 }}>
              <span className="cs-skel" style={{ width: "100%", height: 10 }} />
              <span className="cs-skel" style={{ width: "100%" }} />
              <span className="cs-skel" style={{ width: "100%", height: 16 }} />
            </div>
            <div className="cs-card-foot">
              <span className="cs-skel" style={{ width: 90, height: 10 }} />
              <span className="cs-skel" style={{ width: 120, height: 10 }} />
            </div>
          </div>
        ))}
      </div>
    </section>
  );
}

export default function PublicModels() {
  const { t } = useTranslation();
  const [unit, setUnit] = useState<PriceUnit>("M");
  const [q, setQ] = useState("");
  const [vendorSlug, setVendorSlug] = useState(ALL_VENDORS);
  const [groupBySection, setGroupBySection] = useState<Record<string, string>>({});

  const { data, isLoading, isError, refetch } = useQuery({
    queryKey: ["public", "models"],
    queryFn: () =>
      http.get<{ models: PublicModel[] }>("/v1/public/models").then((r) => r.data.models),
  });
  // Same payload the /status page renders (server-cached 30s). Only groups
  // the operator publishes are in it, so models outside those simply show
  // no availability rather than a fabricated one.
  const { data: status } = useQuery({
    queryKey: ["public", "status"],
    queryFn: publicApi.publicStatus,
    staleTime: 30_000,
  });

  const all = useMemo(() => data ?? [], [data]);

  const availability = useMemo(() => {
    const m = new Map<string, PublicStatusModel>();
    for (const group of status?.groups ?? []) {
      for (const model of group.models ?? []) m.set(`${group.id}::${model.name}`, model);
    }
    return m;
  }, [status]);

  // Vendor sections in order of catalogue depth, models inside in natural
  // name order so versions of one family sit together.
  const sections = useMemo<VendorSection[]>(() => {
    const bySlug = new Map<string, VendorSection>();
    for (const m of all) {
      const slug = m.vendor?.slug ?? OTHER_VENDOR;
      let s = bySlug.get(slug);
      if (!s) {
        s = { slug, name: m.vendor?.name ?? t("pub.models.otherVendor"), iconModel: m.name, models: [], groups: [] };
        bySlug.set(slug, s);
      }
      s.models.push(m);
    }
    for (const s of bySlug.values()) {
      s.models.sort((a, b) => nameCollator.compare(a.name, b.name));
      const seen = new Map<string, PublicModelGroup>();
      for (const m of s.models) for (const g of balanceGroups(m)) if (!seen.has(g.id)) seen.set(g.id, g);
      s.groups = [...seen.values()].sort((a, b) => a.rate_multiplier - b.rate_multiplier || a.name.localeCompare(b.name));
    }
    return [...bySlug.values()].sort((a, b) => {
      if (a.slug === OTHER_VENDOR) return 1;
      if (b.slug === OTHER_VENDOR) return -1;
      return b.models.length - a.models.length || a.name.localeCompare(b.name);
    });
  }, [all, t]);

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return sections
      .filter((s) => vendorSlug === ALL_VENDORS || s.slug === vendorSlug)
      .map((s) => ({
        ...s,
        models: needle
          ? s.models.filter((m) =>
              m.name.toLowerCase().includes(needle) ||
              (m.description ?? "").toLowerCase().includes(needle) ||
              s.name.toLowerCase().includes(needle))
          : s.models,
      }))
      .filter((s) => s.models.length > 0);
  }, [sections, q, vendorSlug]);

  // Hero evidence: each vendor's flagship (highest official output price a
  // balance group can buy), then the three with the lowest multiplier. One
  // row per vendor so the sample never reads as one channel's discount.
  const ledger = useMemo(() => {
    const flagships: { m: PublicModel; group: PublicModelGroup; price: number }[] = [];
    for (const s of sections) {
      let best: { m: PublicModel; group: PublicModelGroup; price: number } | undefined;
      for (const m of s.models) {
        if (m.billing_mode !== "per_token" || !isSet(m.output_unit_price)) continue;
        const group = lowestGroup(m);
        if (!group) continue;
        const price = parseFloat(m.output_unit_price);
        if (!Number.isFinite(price)) continue;
        if (!best || price > best.price) best = { m, group, price };
      }
      if (best) flagships.push(best);
    }
    return flagships
      .sort((a, b) => a.group.rate_multiplier - b.group.rate_multiplier || b.price - a.price)
      .slice(0, 3);
  }, [sections]);

  const vendorCount = sections.filter((s) => s.slug !== OTHER_VENDOR).length;
  const isFiltering = q.trim() !== "" || vendorSlug !== ALL_VENDORS;
  const windowHours = status?.window_hours ?? 24;

  return (
    <div className="cs-page">
      <section className="cs-hero">
        <div className="cs-shell cs-hero-grid">
          <div>
            <p className="cs-kicker">
              <b>{t("pub.models.kicker")}</b>
              {all.length > 0 && <span><Trans i18nKey="pub.models.modelCount" count={all.length} components={{ b: <b /> }} /></span>}
              {vendorCount > 0 && <span><Trans i18nKey="pub.models.vendorCount" count={vendorCount} components={{ b: <b /> }} /></span>}
            </p>
            <h1 className="cs-title">{t("pub.models.title")}</h1>
            <p className="cs-lede">
              {t("pub.models.lede")}
            </p>
          </div>
          <div className="cs-ledger" aria-label={t("pub.models.ledgerLabel")}>
            <div className="cs-ledger-head">
              <span>{t("pub.models.ledgerCurrencies")}</span>
              <span>{t("pub.models.outputPrice")} <code>{UNIT_SUFFIX[unit]} tokens</code></span>
            </div>
            {ledger.length === 0 && (
              <p className="cs-ledger-empty">{isLoading ? t("pub.models.loadingPrices") : t("pub.models.noLedger")}</p>
            )}
            {ledger.map(({ m, group }) => {
              const official = fmtTokenPrice(m.output_unit_price, unit);
              const site = fmtTokenPrice(m.output_unit_price, unit, group.rate_multiplier);
              const badge = savingBadge(group.rate_multiplier);
              return (
                <div key={m.name} className="cs-ledger-row">
                  <span className="cs-ledger-name" title={`${m.name} · ${group.name} ${formatMultiplier(group.rate_multiplier)}`}>
                    <ModelBrandIcon model={m.name} size={16} mono />
                    <span>{m.name}</span>
                  </span>
                  <s className="cs-ledger-official">{currencySymbol(m.currency)}{official}</s>
                  <span className="cs-ledger-site"><PointIcon size={14} />{site}</span>
                  <span className="cs-badge" data-tone={badge.tone}>{badge.text}</span>
                </div>
              );
            })}
          </div>
        </div>
      </section>

      <div className="cs-shell cs-body">
        <div className="cs-toolbar">
          <label className="cs-search">
            <Search size={15} aria-hidden />
            <input
              type="search"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder={t("pub.models.search")}
              aria-label={t("pub.models.search")}
            />
          </label>
          <div className="cs-vendors" role="group" aria-label={t("pub.models.vendor")}>
            <button
              type="button"
              className={`cs-chip ${vendorSlug === ALL_VENDORS ? "is-active" : ""}`}
              onClick={() => setVendorSlug(ALL_VENDORS)}
            >
              {t("pub.models.all")}
            </button>
            {sections.map((s) => (
              <button
                key={s.slug}
                type="button"
                className={`cs-chip ${vendorSlug === s.slug ? "is-active" : ""}`}
                onClick={() => setVendorSlug(s.slug)}
              >
                {s.name}
              </button>
            ))}
          </div>
          <select
            className="cs-vendor-select"
            value={vendorSlug}
            onChange={(e) => setVendorSlug(e.target.value)}
            aria-label={t("pub.models.vendor")}
          >
            <option value={ALL_VENDORS}>{t("pub.models.allVendors")} · {all.length}</option>
            {sections.map((s) => (
              <option key={s.slug} value={s.slug}>{s.name} · {s.models.length}</option>
            ))}
          </select>
          <div className="cs-unit" role="group" aria-label={t("pub.models.priceUnit")}>
            {(["M", "K"] as const).map((u) => (
              <button
                key={u}
                type="button"
                className={unit === u ? "is-active" : ""}
                onClick={() => setUnit(u)}
                aria-pressed={unit === u}
              >
                <Trans i18nKey="pub.models.perUnit" values={{ u }} components={{ b: <b /> }} />
              </button>
            ))}
          </div>
        </div>

        {isLoading && (<><SkeletonSection /><SkeletonSection /></>)}

        {isError && (
          <div className="cs-notice" role="alert">
            <span>{t("pub.models.loadError")}</span>
            <button type="button" onClick={() => refetch()}><RotateCw size={14} />{t("pub.models.retry")}</button>
          </div>
        )}

        {!isLoading && !isError && filtered.length === 0 && (
          <div className="cs-notice">
            <span>{isFiltering ? t("pub.models.noMatch") : t("pub.models.empty")}</span>
            {isFiltering && (
              <button type="button" onClick={() => { setQ(""); setVendorSlug(ALL_VENDORS); }}>{t("pub.models.clearFilters")}</button>
            )}
          </div>
        )}

        {filtered.map((s) => (
          <VendorBlock
            key={s.slug}
            section={s}
            activeGroupId={groupBySection[s.slug] ?? LOWEST}
            onGroupChange={(id) => setGroupBySection((prev) => ({ ...prev, [s.slug]: id }))}
            unit={unit}
            availability={availability}
            windowHours={windowHours}
          />
        ))}
      </div>
    </div>
  );
}
