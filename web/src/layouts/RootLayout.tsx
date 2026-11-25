import { useEffect } from "react";
import { Outlet, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import Navbar from "../components/Navbar";
import Footer from "../components/Footer";
import { AnnouncementDialog } from "../components/AnnouncementDialog";
import { MarketingBanner } from "../components/MarketingBanner";
import { useSiteStore } from "../store/site";
import { accountApi } from "../api/account";
import { useAuthStore } from "../store/auth";
import { queryKeys } from "../lib/queryClient";

export default function RootLayout() {
  const load = useSiteStore((s) => s.load);
  const marketingBanner = useSiteStore((s) => s.site?.marketing_banner);
  useEffect(() => { load(); }, [load]);

  const { pathname } = useLocation();
  const showMarketingBanner = marketingBanner?.enabled === true && marketingBanner.text.trim() !== "";
  const user = useAuthStore((s) => s.user);
  const setUser = useAuthStore((s) => s.setUser);
  // Sync the auth-store user with the latest /me response so functional
  // roles (cs / agent) granted after the JWT was issued become visible
  // without a re-login. /me returns the canonical role list; we patch
  // the persisted user record from it on a long-cache refetch.
  const meQ = useQuery({
    queryKey: queryKeys.me.profile,
    queryFn: accountApi.profile,
    enabled: !!user,
    staleTime: 30_000,
    refetchOnWindowFocus: true,
  });
  useEffect(() => {
    if (!meQ.data || !user) return;
    const want = (meQ.data.roles ?? []).slice().sort().join(",");
    const have = (user.roles ?? []).slice().sort().join(",");
    if (want !== have || meQ.data.role !== user.role) {
      setUser({ ...user, role: meQ.data.role, roles: meQ.data.roles });
    }
  }, [meQ.data, user, setUser]);

  // Console routes are work surfaces — skip the marketing footer.
  const isConsole = pathname === "/me" || pathname.startsWith("/me/") ||
                    pathname === "/admin" || pathname.startsWith("/admin/") ||
                    pathname === "/cs" || pathname.startsWith("/cs/") ||
                    pathname === "/agent" || pathname.startsWith("/agent/");

  // Console routes render their own 48px toolbar inside ConsoleLayout;
  // the marketing navbar, banner and footer stay on public pages only.
  if (isConsole) {
    return (
      <div className="jc-root">
        <Outlet />
        <AnnouncementDialog />
      </div>
    );
  }

  return (
    <div className={showMarketingBanner ? "jc-root [&_.pn-nav]:!top-9" : "jc-root"}>
      {showMarketingBanner && marketingBanner && <MarketingBanner config={marketingBanner} />}
      <Navbar />
      <main className={`jc-root-main ${showMarketingBanner ? "pt-24" : "pt-[60px]"}`}>
        <Outlet />
      </main>
      <Footer />
      <AnnouncementDialog />
    </div>
  );
}
