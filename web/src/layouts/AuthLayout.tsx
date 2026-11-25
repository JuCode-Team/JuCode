import { Link, Outlet } from "react-router-dom";
import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { ArrowLeft, ShieldCheck } from "lucide-react";
import { useSite, useSiteStore } from "../store/site";
import { publicApi } from "../api/public";
import SignalRaster from "../components/SignalRaster";
import LanguageToggle from "../components/LanguageToggle";
import { deployment } from "../config/env";

// AuthLayout owns the stable shell; auth pages only provide form content.
// Wide screens split into a brand panel (live platform readout over the same
// Signal Raster as the home hero) and the form panel; narrow screens keep the
// form only.
export default function AuthLayout() {
  const { t } = useTranslation();
  const site = useSite();
  const loadSite = useSiteStore((s) => s.load);

  useEffect(() => {
    loadSite();
  }, [loadSite]);

  // Same query keys as Home, so moving between the two shares the cache.
  // A failed fetch drops its row instead of showing a placeholder value.
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

  // Groups without traffic report "unknown" and stay out of the count.
  const known = (status?.groups ?? []).filter((g) => g.state !== "unknown");
  const healthy = known.filter((g) => g.state === "healthy").length;
  const gatewayTone =
    healthy === known.length ? "ok" : healthy * 2 < known.length ? "critical" : "warn";
  const siteName = site.site_name || deployment.siteName;

  return (
    <div className="pn-auth-shell">
      <aside className="pn-auth-panel" aria-label={siteName}>
        <div className="pn-auth-raster" aria-hidden="true">
          <SignalRaster />
        </div>
        <div className="pn-auth-panel-top">
          <Link to="/" className="pn-auth-wordmark">{siteName}</Link>
          <Link to="/" className="pn-auth-back">
            <ArrowLeft size={14} aria-hidden="true" />
            {t("auth.backHome")}
          </Link>
        </div>

        <div className="pn-auth-pitch">
          <h2>{t("auth.panelTitle")}</h2>
          <p>{t("auth.panelCopy")}</p>
          <dl className="pn-auth-readout">
            {known.length > 0 && (
              <div>
                <dt>{t("auth.panelGateway")}</dt>
                <dd>
                  <Link to="/status" className="pn-auth-gateway" data-tone={gatewayTone}>
                    {healthy === known.length
                      ? t("auth.gatewayAllOk")
                      : t("auth.gatewayPartial", { healthy, total: known.length })}
                  </Link>
                </dd>
              </div>
            )}
            {models && models.length > 0 && (
              <div>
                <dt>{t("auth.panelModels")}</dt>
                <dd><Link to="/models">{t("auth.modelCount", { count: models.length })}</Link></dd>
              </div>
            )}
            <div>
              <dt>{t("auth.panelProtocols")}</dt>
              <dd>OpenAI · Anthropic</dd>
            </div>
          </dl>
        </div>

        <p className="pn-auth-legal">
          {t("footer.rights", { year: new Date().getFullYear(), site: siteName })}
          {site.icp_number && <span>{site.icp_number}</span>}
        </p>
      </aside>

      <main className="pn-auth-main">
        <header className="pn-auth-main-top">
          <Link to="/" className="pn-auth-wordmark">{siteName}</Link>
          <LanguageToggle className="pn-auth-lang" />
        </header>

        <div className="pn-auth-body pn-enter">
          <Outlet />
        </div>

        <footer className="pn-auth-main-foot">
          <span>
            <ShieldCheck size={14} aria-hidden="true" />
            {t("auth.securityNote")}
          </span>
          {/* The brand panel carries the ICP number on wide screens; it is
              hidden below that, and the filing must stay visible. */}
          {site.icp_number && <span className="pn-auth-icp">{site.icp_number}</span>}
        </footer>
      </main>
    </div>
  );
}
