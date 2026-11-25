import { create } from "zustand";
import { persist } from "zustand/middleware";
import { queryClient } from "../lib/queryClient";

// Account context for the console: null = personal, otherwise the id of
// the organization the user switched to. Every /v1/me request carries it
// as X-Organization-Id (see api/client.ts), and every org-aware queryKey
// embeds the scope string so switching can never bleed one context's
// cached data into the other.
//
// Kept intentionally free of api-layer imports: api/client.ts reads this
// store from its request interceptor, so importing endpoints here would
// create a module cycle.

const STORAGE_KEY = "jucode-org-context";

interface OrgContextState {
  activeOrgId: string | null;
  setActiveOrg: (id: string | null) => void;
}

export const useOrgContextStore = create<OrgContextState>()(
  persist(
    (set, get) => ({
      activeOrgId: null,
      setActiveOrg(id) {
        if (get().activeOrgId === id) return;
        set({ activeOrgId: id });
        // Belt-and-braces: scoped queryKeys are the real isolation, this
        // just drops the previous context's data instead of letting it
        // linger in memory until GC.
        queryClient.removeQueries({ queryKey: ["me-scoped"] });
        queryClient.removeQueries({ queryKey: ["apikeys"] });
      },
    }),
    { name: STORAGE_KEY },
  ),
);

// resetOrgContext drops back to personal without touching caches beyond
// the scoped ones. Called on logout, on auth invalidation, and when the
// backend rejects the current org header (org_context_invalid — e.g. the
// user was removed from the organization).
export function resetOrgContext(): void {
  useOrgContextStore.getState().setActiveOrg(null);
}

// useOrgScope returns the cache-key segment for the current context:
// "personal" or the active organization id. Include it in every queryKey
// whose response depends on the account context.
export function useOrgScope(): string {
  return useOrgContextStore((s) => s.activeOrgId ?? "personal");
}

// Cross-tab sync: zustand/persist writes localStorage but does not listen
// for changes made by other tabs. Rehydrate on `storage` events so
// switching context in one tab switches every tab (they share the same
// backend context anyway once the header changes).
if (typeof window !== "undefined") {
  window.addEventListener("storage", (e) => {
    if (e.key !== STORAGE_KEY) return;
    void useOrgContextStore.persist.rehydrate();
  });
}
