import { Box, Dialog, DropdownMenu, Flex, Text } from "@radix-ui/themes";
import PremiumButton from "./PremiumButton";
import LanguageToggle from "./LanguageToggle";
import { Link, NavLink, useLocation, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Megaphone, Menu, Moon, Sun, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useAuthStore } from "../store/auth";
import { useSite } from "../store/site";
import { useThemeStore } from "../store/theme";
import { activeAnnouncements } from "./AnnouncementDialog";
import { docsUrl } from "../lib/siblingSite";
import { deployment } from "../config/env";

function NavItem({ to, children, onClick }: { to: string; children: React.ReactNode; onClick?: () => void }) {
  return (
    <NavLink
      to={to}
      end
      onClick={onClick}
      className={({ isActive }) =>
        [
          "pn-nav-link whitespace-nowrap",
          isActive
            ? "text-on-surface"
            : "text-on-surface-variant hover:text-on-surface",
        ].join(" ")
      }
    >
      {children}
    </NavLink>
  );
}

function MobileNavItem({ to, children, onClick }: { to: string; children: React.ReactNode; onClick?: () => void }) {
  return (
    <NavLink
      to={to}
      end
      onClick={onClick}
      className={({ isActive }) =>
        [
          "block px-3 py-2.5 rounded-md text-[15px] font-medium transition-colors",
          isActive
            ? "text-on-surface bg-[color:var(--c-card-faint-bg)]"
            : "text-on-surface-variant hover:text-on-surface hover:bg-[color:var(--c-card-faint-bg)]",
        ].join(" ")
      }
    >
      {children}
    </NavLink>
  );
}

// The web app (chat, remote computers) lives on its own host: the only one
// its OAuth client may return to, whatever domain this site is served from.
const WEB_APP_URL = deployment.webAppUrl;
const DOCS_URL = docsUrl();

function ThemeToggle() {
  const { t } = useTranslation();
  const appearance = useThemeStore((s) => s.appearance);
  const toggle = useThemeStore((s) => s.toggle);
  const isDark = appearance === "dark";
  const label = isDark ? t("nav.toLight") : t("nav.toDark");
  return (
    <button
      type="button"
      onClick={toggle}
      aria-label={label}
      title={label}
      className="w-8 h-8 inline-flex items-center justify-center rounded-full bg-[color:var(--c-card-faint-bg)] text-on-surface-variant hover:text-on-surface hover:bg-[color:var(--c-btn-secondary-bg-hover)] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-bright"
    >
      {isDark ? <Sun size={18} strokeWidth={1.8} /> : <Moon size={18} strokeWidth={1.8} />}
    </button>
  );
}

export function AnnouncementNavButton({ className = "pn-nav-icon-button" }: { className?: string }) {
  const { t } = useTranslation();
  const site = useSite();
  const user = useAuthStore((s) => s.user);
  const [open, setOpen] = useState(false);
  const announcements = useMemo(() => activeAnnouncements(site, !!user), [site, user]);

  if (announcements.length === 0) return null;

  return (
    <Dialog.Root open={open} onOpenChange={setOpen}>
      <Dialog.Trigger>
        <button
          type="button"
          aria-label={t("nav.announcements")}
          title={t("nav.announcements")}
          className={className}
        >
          <Megaphone size={18} strokeWidth={1.8} />
        </button>
      </Dialog.Trigger>
      <Dialog.Content style={{ maxWidth: 620 }}>
        <Flex align="center" gap="3" mb="3">
          <Flex
            align="center"
            justify="center"
            style={{
              width: 36,
              height: 36,
              borderRadius: 8,
              background: "var(--accent-a3)",
              color: "var(--accent-11)",
              flexShrink: 0,
            }}
          >
            <Megaphone size={18} strokeWidth={1.8} />
          </Flex>
          <Box>
            <Dialog.Title mb="0">{t("nav.announcementsTitle")}</Dialog.Title>
            <Text size="1" color="gray">{t("nav.announcementCount", { count: announcements.length })}</Text>
          </Box>
        </Flex>

        <Flex direction="column" gap="3">
          {announcements.map((announcement, idx) => (
            <Box
              key={announcement.id || idx}
              p="3"
              style={{
                border: "1px solid var(--gray-a4)",
                borderRadius: 8,
                background: "var(--gray-a2)",
              }}
            >
              <Text size="2" weight="medium" as="div">
                {announcement.title.trim() || t("nav.announcementFallback")}
              </Text>
              {announcement.content.trim() && (
                <Text
                  size="2"
                  color="gray"
                  as="div"
                  mt="2"
                  style={{ whiteSpace: "pre-wrap", lineHeight: 1.7, wordBreak: "break-word" }}
                >
                  {announcement.content.trim()}
                </Text>
              )}
            </Box>
          ))}
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

export default function Navbar() {
  const { t } = useTranslation();
  const user = useAuthStore((s) => s.user);
  const logout = useAuthStore((s) => s.logout);
  const site = useSite();
  const nav = useNavigate();
  const { pathname } = useLocation();
  const [mobileOpen, setMobileOpen] = useState(false);
  const [mobileMenuMounted, setMobileMenuMounted] = useState(false);
  const [mobileMenuExpanded, setMobileMenuExpanded] = useState(false);
  const [mobileMenuHeight, setMobileMenuHeight] = useState(0);
  const navRootRef = useRef<HTMLElement | null>(null);
  const mobileMenuContentRef = useRef<HTMLElement | null>(null);
  const mobileMenuFrame = useRef<number | null>(null);
  const mobileMenuCloseTimer = useRef<number | null>(null);

  useEffect(() => {
    const onResize = () => { if (window.innerWidth > 900) setMobileOpen(false); };
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);

  useEffect(() => {
    if (mobileOpen) {
      if (mobileMenuCloseTimer.current != null) {
        window.clearTimeout(mobileMenuCloseTimer.current);
        mobileMenuCloseTimer.current = null;
      }
      setMobileMenuMounted(true);
      return;
    }

    if (mobileMenuFrame.current != null) {
      window.cancelAnimationFrame(mobileMenuFrame.current);
      mobileMenuFrame.current = null;
    }
    setMobileMenuExpanded(false);
    setMobileMenuHeight(0);

    if (mobileMenuMounted) {
      mobileMenuCloseTimer.current = window.setTimeout(() => {
        setMobileMenuMounted(false);
        mobileMenuCloseTimer.current = null;
      }, 260);
    }
  }, [mobileOpen, mobileMenuMounted]);

  useEffect(() => {
    if (!mobileMenuMounted || !mobileOpen) return;

    const content = mobileMenuContentRef.current;
    if (!content) return;

    if (mobileMenuFrame.current != null) {
      window.cancelAnimationFrame(mobileMenuFrame.current);
    }

    setMobileMenuExpanded(false);
    setMobileMenuHeight(0);
    mobileMenuFrame.current = window.requestAnimationFrame(() => {
      mobileMenuFrame.current = window.requestAnimationFrame(() => {
        setMobileMenuHeight(content.scrollHeight);
        setMobileMenuExpanded(true);
        mobileMenuFrame.current = null;
      });
    });

    return () => {
      if (mobileMenuFrame.current != null) {
        window.cancelAnimationFrame(mobileMenuFrame.current);
        mobileMenuFrame.current = null;
      }
    };
  }, [mobileMenuMounted, mobileOpen, user]);

  useEffect(() => {
    if (!mobileOpen) return;

    const onPointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (navRootRef.current?.contains(target)) return;
      setMobileOpen(false);
    };

    document.addEventListener("pointerdown", onPointerDown, true);
    return () => document.removeEventListener("pointerdown", onPointerDown, true);
  }, [mobileOpen]);

  useEffect(() => {
    return () => {
      if (mobileMenuFrame.current != null) {
        window.cancelAnimationFrame(mobileMenuFrame.current);
      }
      if (mobileMenuCloseTimer.current != null) {
        window.clearTimeout(mobileMenuCloseTimer.current);
      }
    };
  }, []);

  const isConsole = pathname === "/me" || pathname.startsWith("/me/") ||
                    pathname === "/admin" || pathname.startsWith("/admin/") ||
                    pathname === "/cs" || pathname.startsWith("/cs/") ||
                    pathname === "/agent" || pathname.startsWith("/agent/");

  const onLogout = async () => {
    await logout();
    nav("/login");
  };

  const closeMobile = () => setMobileOpen(false);
  const onMobileMenuTransitionEnd = (event: React.TransitionEvent<HTMLDivElement>) => {
    if (event.target !== event.currentTarget || event.propertyName !== "height") return;
    if (!mobileOpen) setMobileMenuMounted(false);
  };

  // Hover-driven user menu. Radix DropdownMenu is click-only by
  // design (a11y reasons), so we drive it as a controlled component:
  // mouseenter opens, mouseleave closes after a short grace period so
  // the cursor can travel from the trigger to the menu body without
  // dismissing it. Click also still toggles via onOpenChange.
  const [menuOpen, setMenuOpen] = useState(false);
  const closeTimer = useRef<number | null>(null);
  const cancelClose = () => {
    if (closeTimer.current != null) {
      window.clearTimeout(closeTimer.current);
      closeTimer.current = null;
    }
  };
  const scheduleClose = () => {
    cancelClose();
    closeTimer.current = window.setTimeout(() => setMenuOpen(false), 150);
  };
  useEffect(() => () => cancelClose(), []);

  return (
    <header
      ref={navRootRef}
      className="pn-nav"
      data-console={isConsole ? "true" : undefined}
    >
      <div className="pn-nav-inner flex justify-between items-center px-gutter py-3 max-w-container-max mx-auto">
        {/* Left: logo + desktop nav */}
        <div className="pn-nav-primary flex items-center gap-10">
          <Link to="/" className="pn-brand" onClick={closeMobile}>
            <span className="text-[18px] font-semibold tracking-tight-custom text-on-surface">
              {site.site_name || deployment.siteName}
            </span>
          </Link>
          <nav className="hidden min-[901px]:flex items-center gap-6">
            <NavItem to="/">{t("nav.home")}</NavItem>
            {WEB_APP_URL && (
              <a
                href={WEB_APP_URL}
                target="_blank"
                rel="noreferrer"
                className="pn-nav-link whitespace-nowrap text-on-surface-variant hover:text-on-surface"
              >
                {t("nav.webApp")}
              </a>
            )}
            <NavItem to="/models">{t("nav.models")}</NavItem>
            <NavItem to="/status">{t("nav.status")}</NavItem>
          </nav>
        </div>

        {/* Right: announcements + theme toggle + auth */}
        <div className="flex items-center gap-3">
          <AnnouncementNavButton />
          <LanguageToggle className="w-8 h-8 inline-flex items-center justify-center rounded-full text-[12px] font-semibold bg-[color:var(--c-card-faint-bg)] text-on-surface-variant hover:text-on-surface hover:bg-[color:var(--c-btn-secondary-bg-hover)] transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-bright" />
          <ThemeToggle />
          {/* Mobile toggle. Console pages use ConsoleLayout's sidebar button. */}
          {!isConsole && (
            <button
              type="button"
              className="min-[901px]:hidden p-1.5 rounded-md text-on-surface-variant hover:text-on-surface transition-colors"
              onClick={() => setMobileOpen((v) => !v)}
              aria-label={mobileOpen ? t("nav.closeMenu") : t("nav.openMenu")}
            >
              {mobileOpen ? <X size={20} /> : <Menu size={20} />}
            </button>
          )}
          {/* Plain links, as 登录 beside them: one filled button (注册) in the row. */}
          {DOCS_URL && (
            <div className="hidden min-[901px]:flex items-center gap-1">
              <a
                href={DOCS_URL}
                target="_blank"
                rel="noreferrer"
                className="text-on-surface-variant hover:text-on-surface transition-colors font-medium text-[15px] px-2 whitespace-nowrap"
              >
                {t("nav.docs")}
              </a>
            </div>
          )}
          {!user ? (
            <div className="hidden min-[901px]:flex items-center gap-2">
              <Link
                to="/login"
                className="text-on-surface-variant hover:text-on-surface transition-colors font-medium text-[15px] px-2"
              >
                {t("nav.login")}
              </Link>
              <PremiumButton to="/register" variant="primary" size="md">
                {t("nav.register")}
              </PremiumButton>
            </div>
          ) : (
            <DropdownMenu.Root open={menuOpen} onOpenChange={setMenuOpen} modal={false}>
              <DropdownMenu.Trigger>
                <button
                  type="button"
                  onMouseEnter={() => { cancelClose(); setMenuOpen(true); }}
                  onMouseLeave={scheduleClose}
                  className="pn-user-trigger"
                >
                  <span className="pn-user-avatar">
                    {(user.nickname || user.email)[0]?.toUpperCase()}
                  </span>
                  <span className="pn-user-name">
                    {user.nickname || user.email.split("@")[0]}
                  </span>
                </button>
              </DropdownMenu.Trigger>
              <DropdownMenu.Content
                align="end"
                size="2"
                style={{ minWidth: 248 }}
                onMouseEnter={cancelClose}
                onMouseLeave={scheduleClose}
              >
                <div className="flex items-center gap-3 px-2 py-2">
                  <span
                    className="w-9 h-9 rounded-full flex items-center justify-center text-white text-[15px] font-bold flex-shrink-0"
                    style={{ background: "var(--c-primary)" }}
                  >
                    {user.email[0]?.toUpperCase()}
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-1.5">
                      <span className="text-[14px] font-semibold truncate">
                        {user.nickname || user.email.split("@")[0]}
                      </span>
                      {user.role === "admin" && (
                        <span
                          className="text-caption leading-none px-1.5 py-0.5 rounded font-medium flex-shrink-0"
                          style={{
                            background: "color-mix(in oklab, var(--c-primary) 18%, transparent)",
                            color: "var(--c-primary-bright)",
                          }}
                        >
                          {t("nav.adminBadge")}
                        </span>
                      )}
                    </div>
                    <div className="text-[12px] opacity-70 truncate mt-0.5">{user.email}</div>
                  </div>
                </div>
                <DropdownMenu.Separator />
                <DropdownMenu.Item onSelect={() => nav("/me")}>{t("nav.menuOverview")}</DropdownMenu.Item>
                <DropdownMenu.Item onSelect={() => nav("/me/apikeys")}>{t("nav.menuApiKeys")}</DropdownMenu.Item>
                <DropdownMenu.Item onSelect={() => nav("/me/usage")}>{t("nav.menuUsage")}</DropdownMenu.Item>
                {user.role === "admin" && (
                  <DropdownMenu.Item onSelect={() => nav("/admin/overview")}>{t("nav.menuAdmin")}</DropdownMenu.Item>
                )}
                <DropdownMenu.Separator />
                <DropdownMenu.Item color="red" onSelect={onLogout}>{t("nav.logout")}</DropdownMenu.Item>
              </DropdownMenu.Content>
            </DropdownMenu.Root>
          )}
        </div>
      </div>

      {/* Mobile menu */}
      {!isConsole && mobileMenuMounted && (
        <div
          className={[
            "pn-mobile-menu min-[901px]:hidden border-t border-outline-variant",
            mobileMenuExpanded ? "is-open" : "",
          ].join(" ")}
          style={{ "--pn-mobile-menu-height": `${mobileMenuHeight}px` } as React.CSSProperties}
          aria-hidden={!mobileOpen}
          onTransitionEnd={onMobileMenuTransitionEnd}
        >
          <nav ref={mobileMenuContentRef} className="flex flex-col gap-1 px-4 py-3 max-w-container-max mx-auto">
            <MobileNavItem to="/" onClick={closeMobile}>{t("nav.home")}</MobileNavItem>
            {DOCS_URL && (
              <a
                href={DOCS_URL}
                target="_blank"
                rel="noreferrer"
                className="block px-3 py-2.5 rounded-md text-[15px] font-medium text-on-surface bg-[color:var(--c-card-faint-bg)] hover:bg-[color:var(--c-btn-secondary-bg-hover)]"
                onClick={closeMobile}
              >
                {t("nav.docs")}
              </a>
            )}
            {WEB_APP_URL && (
              <a
                href={WEB_APP_URL}
                target="_blank"
                rel="noreferrer"
                className="block px-3 py-2.5 rounded-md text-[15px] font-medium text-on-surface-variant hover:text-on-surface hover:bg-[color:var(--c-card-faint-bg)]"
                onClick={closeMobile}
              >
                {t("nav.webApp")}
              </a>
            )}
            <MobileNavItem to="/models" onClick={closeMobile}>{t("nav.models")}</MobileNavItem>
            <MobileNavItem to="/status" onClick={closeMobile}>{t("nav.status")}</MobileNavItem>
            {!user && (
              <>
                <div className="h-px bg-outline-variant my-2" />
                <MobileNavItem to="/login" onClick={closeMobile}>{t("nav.login")}</MobileNavItem>
                <PremiumButton to="/register" onClick={closeMobile} variant="primary" size="md" className="mt-1">
                  {t("nav.register")}
                </PremiumButton>
              </>
            )}
            {user && (
              <>
                <div className="h-px bg-outline-variant my-2" />
                <button
                  type="button"
                  onClick={() => { closeMobile(); onLogout(); }}
                  className="text-left px-3 py-2.5 rounded-md text-[15px] font-medium text-on-surface-variant hover:text-on-surface hover:bg-[color:var(--c-card-faint-bg)] transition-colors"
                >
                  {t("nav.logout")}
                </button>
              </>
            )}
          </nav>
        </div>
      )}
    </header>
  );
}
