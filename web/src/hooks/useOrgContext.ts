import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { accountApi } from "../api/account";
import type { Organization } from "../api/endpoints";
import { queryKeys } from "../lib/queryClient";
import { useAuthStore } from "../store/auth";
import { resetOrgContext, useOrgContextStore } from "../store/orgContext";

// useMyOrganizations shares one query for the org list (with the caller's
// role per org) across the switcher, tab filtering, and the org pages.
export function useMyOrganizations() {
  const user = useAuthStore((s) => s.user);
  return useQuery({
    queryKey: queryKeys.me.organizations,
    queryFn: accountApi.organizations,
    enabled: !!user,
  });
}

// useActiveOrg resolves the persisted activeOrgId against the live org
// list. Returns null in personal context. While the list is still
// loading, an activeOrgId resolves to a stub with role undefined so
// consumers can render a loading state instead of flashing personal data.
export interface ActiveOrg {
  id: string;
  org?: Organization;
  role?: Organization["role"];
  isManager: boolean;
}

export function useActiveOrg(): ActiveOrg | null {
  const activeOrgId = useOrgContextStore((s) => s.activeOrgId);
  const { data: organizations } = useMyOrganizations();
  if (!activeOrgId) return null;
  const org = organizations?.find((o) => o.id === activeOrgId);
  const role = org?.role;
  return { id: activeOrgId, org, role, isManager: role === "owner" || role === "admin" };
}

// useOrgContextGuard drops back to personal when the persisted org is no
// longer in the user's list (removed from the org, switched accounts).
// Mount once in ConsoleLayout.
export function useOrgContextGuard(): void {
  const activeOrgId = useOrgContextStore((s) => s.activeOrgId);
  const { data: organizations, isSuccess } = useMyOrganizations();
  useEffect(() => {
    if (!activeOrgId || !isSuccess) return;
    if (!organizations.some((o) => o.id === activeOrgId)) {
      resetOrgContext();
    }
  }, [activeOrgId, organizations, isSuccess]);
}
