import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { DropdownMenu } from "@radix-ui/themes";
import { BookOpen, ChevronDown, ChevronsUpDown, LayoutGrid, Menu, Moon, ShieldAlert, Sun, X, type LucideProps } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { meApi } from "../api/endpoints";
import { useAuthStore, useHasRole } from "../store/auth";
import { useSite } from "../store/site";
import { useThemeStore } from "../store/theme";
import { AccountSwitcher } from "../components/AccountSwitcher";
import { AnnouncementNavButton } from "../components/Navbar";
import LanguageToggle from "../components/LanguageToggle";
import { docsUrl } from "../lib/siblingSite";
import { useActiveOrg, useMyOrganizations, useOrgContextGuard } from "../hooks/useOrgContext";
import {
  consoleNavGroups,
  consoleRoutes,
  getConsoleRoute,
  personalNavGroups,
  type ConsoleGroup,
  type ConsoleRoute,
  type PersonalGroup,
} from "../config/consoleRoutes";
import { deployment } from "../config/env";

function NavItem({
  to,
  label,
  Icon,
  exact,
}: {
  to: string;
  label: string;
  Icon: React.ComponentType<LucideProps>;
  // `exact` flips NavLink's `end` flag on. Needed whenever the link
  // shares a path prefix with one of its siblings — otherwise NavLink's
  // default prefix-match marks BOTH as active. e.g. /agent vs
  // /agent/settlements would highlight "代理看板" together with whatever
  // sub-page the user is on. Default off so deep routes
  // (/admin/users/:id) still highlight their parent sidebar entry.
  exact?: boolean;
}) {
  return (
    <NavLink to={to} end={exact} className={({ isActive }) => `pn-side-link ${isActive ? "is-active" : ""}`}>
      <Icon size={16} strokeWidth={1.75} aria-hidden />
      <span>{label}</span>
    </NavLink>
  );
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return <div className="pn-side-section">{children}</div>;
}

function pathMatchesItem(pathname: string, item: ConsoleRoute): boolean {
  return item.exact ? pathname === item.path : pathname === item.path || pathname.startsWith(`${item.path}/`);
}

function NavGroup({
  title,
  items,
  pathname,
  defaultOpen = false,
}: {
  title: string;
  items: readonly ConsoleRoute[];
  pathname: string;
  defaultOpen?: boolean;
}) {
  const { t } = useTranslation();
  const active = items.some((item) => pathMatchesItem(pathname, item));
  const [open, setOpen] = useState(defaultOpen);
  const expanded = open || active;

  return (
    <div className={`pn-side-group ${expanded ? "is-open" : ""} ${active ? "has-active" : ""}`}>
      <button type="button" className="pn-side-group-trigger" onClick={() => setOpen((value) => !value)} aria-expanded={expanded}>
        <span>{title}</span>
        <ChevronDown size={14} strokeWidth={1.75} aria-hidden />
      </button>
      {expanded && (
        <div className="pn-side-group-items">
          {items.map((item) => item.icon && (
            <NavItem key={item.path} to={item.path} label={t(item.labelKey)} Icon={item.icon} exact={item.exact} />
          ))}
        </div>
      )}
    </div>
  );
}

const routesInGroup = (group: ConsoleGroup) => consoleRoutes.filter((route) => route.nav && route.group === group);

// TotpNudge: one-line chip in the toolbar for privileged accounts that
// haven't bound TOTP yet. Hidden on /me itself, where SecurityCard already
// carries the same call to action; the link opens the dialog there.
function TotpNudge({ pathname }: { pathname: string }) {
  const { t } = useTranslation();
  const { data: me } = useQuery({ queryKey: ["me"], queryFn: meApi.me, staleTime: 60_000 });
  if (!me?.totp_setup_required || pathname === "/me") return null;
  return (
    <Link to="/me?totp=1" className="jc-topbar-nudge" title={t("console.totpNudge")}>
      <ShieldAlert size={14} strokeWidth={1.8} aria-hidden />
      <span className="jc-topbar-nudge-text">{t("console.totpNudge")}</span>
      <strong>{t("console.totpNudgeAction")}</strong>
    </Link>
  );
}

function ThemeToggle() {
  const { t } = useTranslation();
  const appearance = useThemeStore((s) => s.appearance);
  const toggle = useThemeStore((s) => s.toggle);
  const isDark = appearance === "dark";
  const label = isDark ? t("nav.toLight") : t("nav.toDark");
  return (
    <button type="button" onClick={toggle} aria-label={label} title={label} className="jc-topbar-btn">
      {isDark ? <Sun size={16} strokeWidth={1.75} /> : <Moon size={16} strokeWidth={1.75} />}
    </button>
  );
}

// Sidebar footer: who is signed in, with the account actions that used to
// live in the marketing navbar's hover menu.
function SidebarUser() {
  const { t } = useTranslation();
  const user = useAuthStore((s) => s.user);
  const logout = useAuthStore((s) => s.logout);
  const navigate = useNavigate();
  if (!user) return null;
  const name = user.nickname || user.email.split("@")[0];
  const onLogout = async () => {
    await logout();
    navigate("/login");
  };
  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger>
        <button type="button" className="jc-side-user" aria-label={t("console.userMenu")}>
          <span className="jc-side-user-avatar" aria-hidden>{name[0]?.toUpperCase()}</span>
          <span className="jc-side-user-text">
            <span className="jc-side-user-name">{name}</span>
            <span className="jc-side-user-mail">{user.email}</span>
          </span>
          <ChevronsUpDown size={14} strokeWidth={1.75} aria-hidden />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Content align="start" side="top" size="2" style={{ minWidth: 224 }}>
        <DropdownMenu.Item onSelect={() => navigate("/me")}>{t("nav.menuOverview")}</DropdownMenu.Item>
        <DropdownMenu.Item onSelect={() => navigate("/me/apikeys")}>{t("nav.menuApiKeys")}</DropdownMenu.Item>
        <DropdownMenu.Item onSelect={() => navigate("/me/usage")}>{t("nav.menuUsage")}</DropdownMenu.Item>
        {user.role === "admin" && (
          <DropdownMenu.Item onSelect={() => navigate("/admin/overview")}>{t("nav.menuAdmin")}</DropdownMenu.Item>
        )}
        <DropdownMenu.Separator />
        <DropdownMenu.Item onSelect={() => navigate("/")}>{t("console.website")}</DropdownMenu.Item>
        <DropdownMenu.Item color="red" onSelect={onLogout}>{t("nav.logout")}</DropdownMenu.Item>
      </DropdownMenu.Content>
    </DropdownMenu.Root>
  );
}

export default function ConsoleLayout() {
  const { t } = useTranslation();
  const site = useSite();
  const user = useAuthStore((s) => s.user);
  const isCS = useHasRole("cs");
  const isAgent = useHasRole("agent");
  const { data: myOrganizations } = useMyOrganizations();
  const showUserOrganizations = (myOrganizations?.length ?? 0) > 0;
  const activeOrg = useActiveOrg();
  // Drop back to personal if the persisted org vanished from the list
  // (removed from the org / org deleted).
  useOrgContextGuard();
  const location = useLocation();
  const currentRoute = getConsoleRoute(location.pathname);
  const areaTitle =
    currentRoute?.area === "personal" && activeOrg
      ? activeOrg.org?.name ?? t("common.organization")
      : t(`console.area.${currentRoute?.area ?? "personal"}`);
  // Breadcrumb in the toolbar names the area and section only; the page
  // itself is the H1 right below, so it is not repeated here.
  const adminGroup = (consoleNavGroups as readonly string[]).includes(currentRoute?.group ?? "")
    ? (currentRoute?.group as (typeof consoleNavGroups)[number])
    : undefined;
  const groupKey: PersonalGroup | (typeof consoleNavGroups)[number] | undefined = currentRoute?.personalGroup ?? adminGroup;
  const groupTitle = groupKey ? t(`console.group.${groupKey}`) : null;
  const [mobileOpen, setMobileOpen] = useState(false);

  useEffect(() => {
    const desktopQuery = window.matchMedia("(min-width: 901px)");
    const closeMobileSidebar = (event: MediaQueryListEvent | MediaQueryList) => {
      if (event.matches) setMobileOpen(false);
    };

    closeMobileSidebar(desktopQuery);
    desktopQuery.addEventListener("change", closeMobileSidebar);
    return () => desktopQuery.removeEventListener("change", closeMobileSidebar);
  }, []);

  // Close the drawer on navigation and lock body scroll while it is open.
  useEffect(() => { setMobileOpen(false); }, [location.pathname]);
  useEffect(() => {
    if (!mobileOpen) return;
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const onKey = (event: KeyboardEvent) => { if (event.key === "Escape") setMobileOpen(false); };
    window.addEventListener("keydown", onKey);
    return () => {
      document.body.style.overflow = previous;
      window.removeEventListener("keydown", onKey);
    };
  }, [mobileOpen]);

  // The personal group is context-sensitive: each route declares which
  // account context (personal / organization / both) it belongs to, and
  // manager-only tabs additionally require the owner/admin role in the
  // active organization.
  const matchesContext = (route: ConsoleRoute): boolean => {
    if (route.group !== "personal") return true;
    const context = route.context ?? "both";
    if (activeOrg) {
      if (context === "personal") return false;
      if (route.requiresOrgManager && !activeOrg.isManager) return false;
      return true;
    }
    return context !== "organization";
  };

  const visibleRoutes = (group: ConsoleGroup) => routesInGroup(group)
    .filter((route) => !route.requiresOrganization || showUserOrganizations)
    .filter(matchesContext);

  const renderItems = (routes: readonly ConsoleRoute[]) => routes.map((route) => route.icon && (
    <NavItem key={route.path} to={route.path} label={t(route.labelKey)} Icon={route.icon} exact={route.exact} />
  ));

  const isAdmin = user?.role === "admin";
  const primaryRole = isAdmin ? "admin" : isCS ? "cs" : isAgent ? "agent" : "user";

  // Personal area: three task sections (account / access / support). The
  // active organization is named by the switcher below and the toolbar
  // breadcrumb, so no extra area label here.
  const personalRoutes = visibleRoutes("personal");
  const personalSection = (
    <div key="personal" className="pn-side-block">
      {personalNavGroups.map((group) => {
        const items = personalRoutes.filter((route) => route.personalGroup === group);
        if (items.length === 0) return null;
        return (
          <div key={group} className="pn-side-subsection">
            <SectionLabel>{t(`console.group.${group}`)}</SectionLabel>
            {renderItems(items)}
          </div>
        );
      })}
    </div>
  );

  const adminSection = isAdmin ? (
    <div key="admin" className="pn-side-block">
      <SectionLabel>{t("console.section.admin")}</SectionLabel>
      {renderItems(visibleRoutes("admin-primary"))}
      {consoleNavGroups.map((group, index) => (
        <NavGroup
          key={group}
          title={t(`console.group.${group}`)}
          items={routesInGroup(group)}
          pathname={location.pathname}
          defaultOpen={index < 2}
        />
      ))}
    </div>
  ) : null;

  const roleSection = (key: "cs" | "agent", title: string, group: ConsoleGroup) => (
    primaryRole === key ? (
      <div key={key} className="pn-side-block">
        <SectionLabel>{title}</SectionLabel>
        {renderItems(visibleRoutes(group))}
      </div>
    ) : (
      <div key={key} className="pn-side-block">
        <NavGroup title={title} items={routesInGroup(group)} pathname={location.pathname} defaultOpen={false} />
      </div>
    )
  );
  const csSection = isCS ? roleSection("cs", t("console.section.support"), "support") : null;
  const agentSection = isAgent ? roleSection("agent", t("console.section.agent"), "agent") : null;

  // 个人 always leads; the user's primary role section follows, and
  // remaining role sections sit collapsed at the bottom.
  const ordered =
    primaryRole === "admin" ? [personalSection, adminSection, csSection, agentSection]
    : primaryRole === "cs" ? [personalSection, csSection, agentSection]
    : primaryRole === "agent" ? [personalSection, agentSection, csSection]
    : [personalSection];

  return (
    <div className="jc-console pn-console">
      <aside className={`jc-console-sidebar ${mobileOpen ? "is-open" : ""}`}>
        <div className="jc-side-brand">
          <Link to="/" className="jc-side-wordmark">{site.site_name || deployment.siteName}</Link>
          <button
            type="button"
            className="jc-topbar-btn jc-side-close"
            onClick={() => setMobileOpen(false)}
            aria-label={t("console.closeSidebar")}
          >
            <X size={18} strokeWidth={1.75} />
          </button>
        </div>
        <nav className="jc-side-nav" aria-label={t("common.console")}>
          {ordered}
        </nav>
        <div className="jc-side-foot">
          <AccountSwitcher />
          <SidebarUser />
        </div>
      </aside>

      {mobileOpen && <div className="jc-console-overlay" onClick={() => setMobileOpen(false)} />}

      <div className="jc-console-body">
        <header className="jc-console-topbar">
          <button
            type="button"
            className="jc-topbar-btn jc-topbar-menu"
            onClick={() => setMobileOpen((v) => !v)}
            aria-label={mobileOpen ? t("console.closeSidebar") : t("console.openSidebar")}
            aria-expanded={mobileOpen}
          >
            <Menu size={18} strokeWidth={1.75} />
          </button>
          <nav className="jc-topbar-crumb" aria-label={t("console.breadcrumb")}>
            <span>{areaTitle}</span>
            {groupTitle && (
              <>
                <i aria-hidden>/</i>
                <span>{groupTitle}</span>
              </>
            )}
          </nav>
          <Link to="/me" className="jc-topbar-mobile-title">{site.site_name || deployment.siteName}</Link>
          <div className="jc-topbar-actions">
            <TotpNudge pathname={location.pathname} />
            <Link to="/models" className="jc-topbar-link">
              <LayoutGrid size={15} strokeWidth={1.75} aria-hidden />
              <span>{t("nav.models")}</span>
            </Link>
            {docsUrl() && (
              <a href={docsUrl()} target="_blank" rel="noreferrer" className="jc-topbar-link">
                <BookOpen size={15} strokeWidth={1.75} aria-hidden />
                <span>{t("nav.docs")}</span>
              </a>
            )}
            <AnnouncementNavButton className="jc-topbar-btn" />
            <LanguageToggle className="jc-topbar-btn jc-topbar-lang" />
            <ThemeToggle />
          </div>
        </header>

        <main className="jc-console-main">
          <div key={location.pathname} className="pn-page-enter jc-console-page">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
