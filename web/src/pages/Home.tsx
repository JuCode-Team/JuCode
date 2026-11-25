import { useEffect, useMemo, useRef, type CSSProperties } from "react";
import { useTranslation } from "react-i18next";
import i18n from "../i18n";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import SignalRaster from "../components/SignalRaster";
import PremiumButton from "../components/PremiumButton";
import ContactDock, { openContactDock } from "../components/ContactDock";
import { PointAmount } from "../components/PointAmount";
import { publicApi } from "../api/public";
import type { Plan, PublicStatusGroup } from "../api/endpoints";
import { useAuthStore } from "../store/auth";
import { useSite } from "../store/site";
import { resolveEndpoints } from "../lib/ccswitch";
import Codex from "@lobehub/icons/es/Codex/components/Mono";
import ClaudeCode from "@lobehub/icons/es/ClaudeCode/components/Mono";
import Cursor from "@lobehub/icons/es/Cursor/components/Mono";
import Cline from "@lobehub/icons/es/Cline/components/Mono";
import RooCode from "@lobehub/icons/es/RooCode/components/Mono";
import GeminiCLI from "@lobehub/icons/es/GeminiCLI/components/Mono";
import Windsurf from "@lobehub/icons/es/Windsurf/components/Mono";
import OpenCode from "@lobehub/icons/es/OpenCode/components/Mono";

const CODING_TOOLS = [
  { tool: "Codex", icon: Codex },
  { tool: "Claude Code", icon: ClaudeCode },
  { tool: "Cursor", icon: Cursor },
  { tool: "Cline", icon: Cline },
  { tool: "Roo Code", icon: RooCode },
  { tool: "Gemini CLI", icon: GeminiCLI },
  { tool: "Windsurf", icon: Windsurf },
  { tool: "OpenCode", icon: OpenCode },
] as const;

// 标题第二行的滚动工具名。只播一个信号：不再向 Signal Raster 发脉冲。
// 循环靠末尾复制的首项做无缝衔接，跳回首项放在 transitionend 里而不是定时器。
function HeroToolRoller() {
  const { t } = useTranslation();
  const trackRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

    const track = trackRef.current;
    if (!track) return;

    let index = 0;
    const itemHeight = () => track.parentElement?.clientHeight ?? 62;
    const onEnd = () => {
      if (index !== CODING_TOOLS.length) return;
      track.style.transition = "none";
      track.style.transform = "translate3d(0, 0, 0)";
      index = 0;
    };
    track.addEventListener("transitionend", onEnd);
    const interval = window.setInterval(() => {
      index += 1;
      track.style.transition = "transform 560ms cubic-bezier(.22,.8,.28,1)";
      track.style.transform = `translate3d(0, -${index * itemHeight()}px, 0)`;
    }, 2600);

    return () => {
      window.clearInterval(interval);
      track.removeEventListener("transitionend", onEnd);
    };
  }, []);

  return (
    <span
      className="trace-home-tool-roller"
      role="img"
      aria-label={t("pub.home.toolRollerLabel")}
    >
      <span ref={trackRef} className="trace-home-tool-roll-track" aria-hidden="true">
        {[...CODING_TOOLS, CODING_TOOLS[0]].map((item, index) => {
          const ToolIcon = item.icon;
          return (
            <span className="trace-home-tool-roll-item" key={`${item.tool}-${index}`}>
              <ToolIcon size={30} />
              <span>{item.tool}</span>
            </span>
          );
        })}
      </span>
    </span>
  );
}

// 首页按分组计数而不是取最差态：只要多数分组正常，就如实写「9/10 分组运行正常」。
// 没有流量的 unknown 分组不进分母；没有任何可判定分组时返回 null，卡片退回中性文案。
function gatewaySummary(groups: PublicStatusGroup[]) {
  const known = groups.filter((group) => group.state !== "unknown");
  if (known.length === 0) return null;
  const healthy = known.filter((group) => group.state === "healthy").length;
  const majorityDown = healthy * 2 < known.length;
  return {
    label: healthy === known.length
      ? i18n.t("pub.home.allGroupsHealthy")
      : i18n.t("pub.home.groupsHealthy", { healthy, total: known.length }),
    color: majorityDown ? "var(--c-status-critical)" : healthy === known.length ? "var(--c-status-ok)" : "inherit",
  };
}

function HomePlanSkeleton() {
  return (
    <article className="trace-home-plan" aria-hidden="true">
      <span className="sk" style={{ width: "42%", height: 14 }} />
      <span className="sk" style={{ width: "68%", height: 32, margin: "26px 0 14px" }} />
      <span className="sk" style={{ width: "100%", height: 10 }} />
      <span className="sk" style={{ width: "90%", height: 10, marginTop: 10 }} />
      <span className="sk" style={{ width: "52%", height: 12, marginTop: "auto" }} />
    </article>
  );
}

type PeriodUnit = "week" | "month" | "year" | "days";

function periodUnit(days: number): PeriodUnit {
  if (days === 7) return "week";
  if (days === 30 || days === 31) return "month";
  if (days === 365) return "year";
  return "days";
}

function HomePlan({ plan, featured, index }: { plan: Plan; featured: boolean; index: number }) {
  const { t } = useTranslation();
  const isUsage = plan.type === "pay_as_you_go";
  const unit = periodUnit(plan.period_days);
  const days = plan.period_days;

  return (
    <article
      className={`trace-home-plan ${featured ? "is-featured" : ""}`}
      data-scroll-reveal
      style={{ "--reveal-i": index } as CSSProperties}
    >
      <header>
        <span>{plan.name}</span>
        <small>{isUsage ? t("pub.home.plan.usage") : t("pub.home.plan.subscription")}</small>
      </header>
      <strong>
        <PointAmount amount={plan.price} iconSize={18} />
        <small>/ {isUsage ? t("pub.home.plan.oneTime") : t(`pub.home.plan.per.${unit}`, { count: days })}</small>
      </strong>
      {plan.description && <p>{plan.description}</p>}
      <ul>
        <li>{isUsage ? t("pub.home.plan.billedPerCall") : t(`pub.home.plan.quotaEvery.${unit}`, { count: days })}</li>
        <li>{isUsage ? t("pub.home.plan.balanceNoExpiry") : t("pub.home.plan.planModels")}</li>
      </ul>
      <Link to="/me/plans">
        <span>{featured ? t("pub.home.plan.chooseRecommended") : t("pub.home.plan.choose")}</span>
        <span aria-hidden="true">→</span>
      </Link>
    </article>
  );
}

export default function Home() {
  const { t } = useTranslation();
  const pageRef = useRef<HTMLDivElement | null>(null);
  const user = useAuthStore((state) => state.user);
  const site = useSite();

  const { data: plans, isError: plansError } = useQuery({
    queryKey: ["public", "plans"],
    queryFn: publicApi.plans,
    staleTime: 60_000,
  });

  // 首屏的三个数字来自模型目录；接口未返回时整行不渲染，不用占位数。
  const { data: models } = useQuery({
    queryKey: ["public", "models"],
    queryFn: publicApi.models,
    staleTime: 60_000,
    retry: 0,
  });

  const { data: status } = useQuery({
    queryKey: ["public", "status"],
    queryFn: publicApi.publicStatus,
    staleTime: 60_000,
    retry: 0,
  });

  const visiblePlans = useMemo(
    () => [...(plans ?? [])]
      .sort((a, b) => Number(a.price) - Number(b.price))
      .slice(0, 3),
    [plans],
  );

  const proof = useMemo(() => {
    if (!models || models.length === 0) return null;
    let minRate = Number.POSITIVE_INFINITY;
    for (const model of models) {
      for (const group of model.groups ?? []) {
        if (group.rate_multiplier > 0 && group.rate_multiplier < minRate) minRate = group.rate_multiplier;
      }
    }
    return { count: models.length, minRate: Number.isFinite(minRate) ? minRate : null };
  }, [models]);

  const endpoints = resolveEndpoints(site);
  const chatEndpoint = endpoints.openaiCompat
    ? `${endpoints.openaiCompat.replace(/\/+$/, "")}/chat/completions`
    : "";
  const gateway = gatewaySummary(status?.groups ?? []);

  const primaryTo = user ? (user.role === "admin" ? "/admin/overview" : "/me/apikeys") : "/register";

  useEffect(() => {
    const root = pageRef.current;
    if (!root) return;

    const targets = root.querySelectorAll<HTMLElement>("[data-scroll-reveal]");
    const showAll = () => targets.forEach((target) => target.classList.add("is-visible"));

    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches || !("IntersectionObserver" in window)) {
      showAll();
      return;
    }

    const observer = new IntersectionObserver((entries) => {
      entries.forEach((entry) => {
        if (!entry.isIntersecting) return;
        entry.target.classList.add("is-visible");
        observer.unobserve(entry.target);
      });
    }, { rootMargin: "0px 0px -9%", threshold: 0.08 });

    targets.forEach((target) => observer.observe(target));
    return () => observer.disconnect();
  }, [visiblePlans.length]);

  return (
    <div ref={pageRef} className="trace-home">
      <header className="trace-home-hero">
        <div className="trace-home-hero-raster">
          <SignalRaster />
        </div>
        <div className="trace-home-shell trace-home-hero-grid">
          <div className="trace-home-copy">
            <span className="trace-home-kicker">{t("pub.home.kicker")}</span>
            <h1>
              {t("pub.home.titleLine1")}
              <br />
              <span className="trace-home-h1-line">{t("pub.home.titleLine2Prefix")} <HeroToolRoller /></span>
            </h1>
            <p>{t("pub.home.lead")}</p>
            <div className="trace-home-actions">
              <PremiumButton to="/register" size="lg">{t("pub.home.signUp")}</PremiumButton>
              <PremiumButton to="/models" variant="secondary" size="lg">{t("pub.home.browseModels")}</PremiumButton>
            </div>
            {proof && (
              <dl className="trace-home-proof" aria-label={t("pub.home.proofLabel")}>
                <div><dd>{proof.count}</dd><dt>{t("pub.home.proofModels")}</dt></div>
                {proof.minRate !== null && (
                  <div><dd>{proof.minRate}×</dd><dt>{t("pub.home.proofRate")}</dt></div>
                )}
                <div><dd>{CODING_TOOLS.length}</dd><dt>{t("pub.home.proofTools")}</dt></div>
              </dl>
            )}
          </div>
        </div>
        <div className="trace-home-shell trace-home-tool-band" role="list" aria-label={t("pub.home.toolsLabel")}>
          {CODING_TOOLS.map((item) => {
            const ToolIcon = item.icon;
            return (
              <div key={item.tool} role="listitem">
                <ToolIcon size={22} />
                <span>{item.tool}</span>
              </div>
            );
          })}
        </div>
      </header>

      <section className="trace-home-section trace-home-shell" id="infrastructure">
        <div className="trace-home-section-head" data-scroll-reveal>
          <span>{t("pub.home.infra.kicker")}</span>
          <h2>{t("pub.home.infra.titleLine1")}<br />{t("pub.home.infra.titleLine2")}</h2>
        </div>
        <div className="trace-home-infra">
          <article data-scroll-reveal style={{ "--reveal-i": 0 } as CSSProperties}>
            <small>{t("pub.home.infra.routing")}</small><b>{t("pub.home.infra.routingTitle")}</b><p>{t("pub.home.infra.routingBody")}</p>
            <Link to="/status">{t("pub.home.infra.viewStatus")}</Link>
          </article>
          <article data-scroll-reveal style={{ "--reveal-i": 1 } as CSSProperties}>
            <small>{t("pub.home.infra.compat")}</small><b>{t("pub.home.infra.compatTitle")}</b><p>{t("pub.home.infra.compatBody")}</p>
            <i>{t("pub.home.infra.toolCount", { count: CODING_TOOLS.length })}</i>
          </article>
          <article data-scroll-reveal style={{ "--reveal-i": 2 } as CSSProperties}>
            <small>{t("pub.home.infra.usage")}</small><b>{t("pub.home.infra.usageTitle")}</b><p>{t("pub.home.infra.usageBody")}</p><i>{t("pub.home.infra.usageTag")}</i>
          </article>
        </div>
      </section>

      <section className="trace-home-pricing" id="plans">
        <div className="trace-home-shell">
          <div className="trace-home-pricing-head" data-scroll-reveal>
            <div><span>{t("pub.home.pricing.kicker")}</span><h2>{t("pub.home.pricing.titleLine1")}<br />{t("pub.home.pricing.titleLine2")}</h2></div>
            <Link className="trace-home-all-plans" to="/me/plans">{t("pub.home.pricing.allPlans")}</Link>
          </div>
          {visiblePlans.length > 0 ? (
            <div className="trace-home-plan-grid">
              {visiblePlans.map((plan, index) => (
                <HomePlan key={plan.id} plan={plan} featured={index === 0} index={index} />
              ))}
            </div>
          ) : plansError ? (
            <div className="trace-home-empty" data-scroll-reveal>
              <p>{t("pub.home.pricing.loadError")}</p>
              <PremiumButton to="/me/plans" variant="secondary">{t("pub.home.pricing.openConsole")}</PremiumButton>
            </div>
          ) : (
            <div className="trace-home-plan-grid" aria-busy="true" aria-label={t("pub.home.pricing.loading")}>
              {[0, 1, 2].map((i) => <HomePlanSkeleton key={i} />)}
            </div>
          )}
          <p className="trace-home-pricing-note" data-scroll-reveal>
            {t("pub.home.pricing.note")}
          </p>
        </div>
      </section>

      <section className="trace-home-start">
        <div className="trace-home-shell">
          <div className="trace-home-start-grid">
            <div data-scroll-reveal>
              <span>{t("pub.home.start.kicker")}</span>
              <h2>{t("pub.home.start.titleLine1")}<br /><em>{t("pub.home.start.titleLine2")}</em></h2>
              <p>{t("pub.home.start.lead")}</p>
              <div className="trace-home-actions">
                <PremiumButton to={primaryTo} size="lg">{user ? t("pub.home.start.createKey") : t("pub.home.start.tryNow")}</PremiumButton>
                <Link className="trace-home-text-link" to="/models">{t("pub.home.start.viewModels")}</Link>
              </div>
              {site.contact_dock_enabled && (
                <p className="trace-home-support">
                  {t("pub.home.start.needHelp")}
                  <button type="button" onClick={openContactDock}>{t("pub.home.start.joinGroup", { label: site.contact_group_label || t("pub.contact.group") })}</button>
                </p>
              )}
            </div>
            <div className="trace-home-endpoint" data-scroll-reveal>
              <header><span>{t("pub.home.endpoint.title")}</span><i>Chat Completions</i></header>
              <code>
                {chatEndpoint ? <><b>POST</b> {chatEndpoint}</> : t("pub.home.endpoint.notConfigured")}
              </code>
              <dl><div><dt>{t("pub.home.endpoint.auth")}</dt><dd>Bearer API_KEY</dd></div><div><dt>{t("pub.home.endpoint.format")}</dt><dd>{t("pub.home.endpoint.formatValue")}</dd></div><div><dt>{t("pub.home.endpoint.routing")}</dt><dd>{t("pub.home.endpoint.routingValue")}</dd></div></dl>
              <footer>
                <span>{gateway ? t("pub.home.endpoint.gatewayStatus") : t("pub.home.endpoint.apiInfo")}</span>
                <b>
                  <Link to="/status" style={{ color: gateway?.color ?? "inherit" }}>
                    {gateway ? gateway.label : t("pub.home.endpoint.viewStatus")}
                  </Link>
                </b>
              </footer>
            </div>
          </div>
          <div className="trace-home-enterprise" data-scroll-reveal>
            <h3>{t("pub.home.enterprise.title")}</h3>
            <p>{t("pub.home.enterprise.body")}</p>
            {site.contact_email ? <a href={`mailto:${site.contact_email}`}>{t("pub.home.enterprise.contact")}</a> : <Link to="/endpoints">{t("pub.home.enterprise.docs")}</Link>}
          </div>
        </div>
      </section>

      <ContactDock />
    </div>
  );
}
