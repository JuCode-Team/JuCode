import { MutationCache, QueryClient } from "@tanstack/react-query";
import axios from "axios";
import { errMsg } from "../api/client";
import { showToast } from "./appToast";
import i18n from "../i18n";

// Mutations are the write path: a silent failure there leaves the operator
// believing a save landed when it did not. Every mutation error therefore
// raises a toast, on top of whatever inline message the page shows.
//
// Two exceptions:
//   - 401, which the axios interceptor already turns into a refresh or a
//     bounce to /login. A toast on top of a redirect is noise.
//   - mutations that opt out via meta.suppressErrorToast, for the handful of
//     flows where the "error" is an expected branch (e.g. the refund ticket
//     that already exists and is navigated to instead).
const mutationCache = new MutationCache({
  onError: (error, _vars, _ctx, mutation) => {
    if (mutation.meta?.suppressErrorToast) return;
    if (axios.isAxiosError(error) && error.response?.status === 401) return;
    showToast({ title: i18n.t("keys.shared.operationFailed"), message: errMsg(error) });
  },
});

export const queryClient = new QueryClient({
  mutationCache,
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
      staleTime: 30_000,
    },
    mutations: {
      retry: 0,
    },
  },
});

export const queryKeys = {
  public: {
    root: ["public"] as const,
    siteInfo: ["public", "site-info"] as const,
  },
  me: {
    root: ["me"] as const,
    profile: ["me"] as const,
    organizations: ["me", "organizations"] as const,
  },
  admin: {
    root: ["admin"] as const,
  },
  cs: {
    root: ["cs"] as const,
  },
  agent: {
    root: ["agent"] as const,
  },
} as const;
