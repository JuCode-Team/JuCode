import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Code2, Mail } from "lucide-react";
import { useSite } from "../store/site";
import type { Translations } from "../i18n/zh";
import { docsUrl } from "../lib/siblingSite";
import { deployment } from "../config/env";

interface LinkItem { labelKey: FooterKey; to: string; external?: boolean }
interface LinkGroup { titleKey: FooterKey; items: LinkItem[] }

type FooterKey = `footer.${keyof Translations["footer"]}`;

const linkGroups: LinkGroup[] = [
  {
    titleKey: "footer.productTitle",
    items: [
      { labelKey: "footer.home", to: "/" },
      { labelKey: "footer.models", to: "/models" },
      ...(docsUrl() ? [{ labelKey: "footer.endpoints" as const, to: docsUrl(), external: true }] : []),
      { labelKey: "footer.console", to: "/me" },
      ...(deployment.webAppUrl ? [{ labelKey: "footer.webApp" as const, to: deployment.webAppUrl, external: true }] : []),
    ],
  },
  {
    titleKey: "footer.resourcesTitle",
    items: [
      { labelKey: "footer.apiKeys", to: "/me/apikeys" },
      { labelKey: "footer.plans", to: "/me/plans" },
      { labelKey: "footer.usage", to: "/me/usage" },
    ],
  },
  {
    titleKey: "footer.accountTitle",
    items: [
      { labelKey: "footer.login", to: "/login" },
      { labelKey: "footer.register", to: "/register" },
    ],
  },
];

export default function Footer() {
  const { t } = useTranslation();
  const site = useSite();
  const year = new Date().getFullYear();
  const brand = site.site_name || deployment.siteName;

  return (
    <footer className="border-t border-outline-variant bg-surface pt-16 pb-8">
      <div className="max-w-container-max mx-auto px-gutter grid grid-cols-2 md:grid-cols-5 gap-8 mb-16">
        {/* Brand column */}
        <div className="col-span-2 space-y-5">
          <div className="flex items-center">
            <span className="text-[18px] font-bold tracking-tight-custom text-on-surface">{brand}</span>
          </div>
          {site.site_description && (
            <p className="text-on-surface-variant text-[13px] leading-relaxed max-w-xs">
              {site.site_description}
            </p>
          )}
          <div className="flex gap-3">
            {site.site_url && (
              <a
                href={site.site_url}
                target="_blank"
                rel="noreferrer"
                aria-label={t("footer.website")}
                className="w-8 h-8 rounded-lg bg-[color:var(--c-card-faint-bg)] flex items-center justify-center text-on-surface-variant hover:text-on-surface transition-colors border border-outline-variant"
              >
                <Code2 size={16} strokeWidth={1.8} />
              </a>
            )}
            {site.contact_email && (
              <a
                href={`mailto:${site.contact_email}`}
                aria-label={t("footer.contactEmail")}
                className="w-8 h-8 rounded-lg bg-[color:var(--c-card-faint-bg)] flex items-center justify-center text-on-surface-variant hover:text-on-surface transition-colors border border-outline-variant"
              >
                <Mail size={16} strokeWidth={1.8} />
              </a>
            )}
          </div>
        </div>

        {/* Link columns */}
        {linkGroups.map((group) => (
          <div key={group.titleKey} className="space-y-4">
            <h2 className="text-on-surface font-semibold text-[14px]">{t(group.titleKey)}</h2>
            <ul className="space-y-3">
              {group.items.map((item) => (
                <li key={item.labelKey}>
                  {item.external ? (
                    <a
                      href={item.to}
                      target="_blank"
                      rel="noreferrer"
                      className="text-on-surface-variant hover:text-on-surface transition-colors text-[13px]"
                    >
                      {t(item.labelKey)}
                    </a>
                  ) : (
                    <Link
                      to={item.to}
                      className="text-on-surface-variant hover:text-on-surface transition-colors text-[13px]"
                    >
                      {t(item.labelKey)}
                    </Link>
                  )}
                </li>
              ))}
            </ul>
          </div>
        ))}
      </div>

      <div className="max-w-container-max mx-auto px-gutter flex flex-col md:flex-row justify-between items-center pt-8 border-t border-[color:var(--c-hairline)] text-[12px] text-on-surface-variant gap-3">
        <p>{t("footer.rights", { year, site: brand })}</p>
        {site.icp_number && (
          <a
            href="https://beian.miit.gov.cn/"
            target="_blank"
            rel="noreferrer"
            className="hover:text-on-surface transition-colors"
          >
            {site.icp_number}
          </a>
        )}
      </div>
    </footer>
  );
}
