import axios, { AxiosError, AxiosHeaders, type AxiosRequestConfig, type RawAxiosHeaders } from "axios";
import { useAuthStore } from "../store/auth";
import { resetOrgContext, useOrgContextStore } from "../store/orgContext";
import { requestStepUpCode, stepUpPromptAvailable } from "./stepup";

// Dev: hit jucode on its own port (Vite serves frontend on 5173).
// Prod: VITE_API_BASE may be set explicitly; otherwise use same-origin
// (Caddy / nginx forwards /v1/* and /healthz/* to the backend).
const baseURL =
  import.meta.env.VITE_API_BASE ??
  (import.meta.env.DEV ? "http://localhost:8090" : "");

// Exposed for callers that need raw fetch against the same API host —
// e.g. the lab page streams SSE from the LLM gateway with an sk-juc-*
// key instead of the console JWT, so it can't go through `http`.
export const apiBaseURL = baseURL;

// Site-relative asset paths the backend hands out (e.g. an uploaded
// /v1/public/site-assets/<name>) live on the API host, not the page origin.
export function apiAssetURL(url: string): string {
  return url.startsWith("/") ? `${baseURL}${url}` : url;
}

export const http = axios.create({ baseURL });

// Inject access_token into outgoing requests, plus the account context
// for /v1/me/* reads: when the user switched the console to an
// organization, X-Organization-Id selects the org scope server-side. The
// org-management endpoints (/v1/me/organizations/:id/...) key off their
// route param and simply ignore the header, so a blanket prefix match is
// safe.
http.interceptors.request.use((cfg) => {
  const tok = useAuthStore.getState().tokens?.access_token;
  if (tok && !cfg.headers?.Authorization) {
    cfg.headers = cfg.headers ?? {};
    cfg.headers.Authorization = `Bearer ${tok}`;
  }
  const orgId = useOrgContextStore.getState().activeOrgId;
  if (orgId && cfg.url?.startsWith("/v1/me")) {
    cfg.headers = cfg.headers ?? {};
    cfg.headers["X-Organization-Id"] = orgId;
  }
  return cfg;
});

// On 401: try one refresh; if that fails (or there's no refresh token), and
// the user is currently logged in, drop the session and bounce them to
// /login. /v1/auth/* 401s are credential errors from the auth flow itself
// (wrong password, expired refresh token during a refresh call) — those
// should surface to the caller, not trigger a session-expired redirect loop.
let refreshing: Promise<void> | null = null;

function isAuthRequest(url?: string): boolean {
  if (!url) return false;
  return url.includes("/v1/auth/");
}

function bounceToLogin() {
  // Already on the login page? Don't loop.
  if (window.location.pathname.startsWith("/login")) return;
  // Preserve where the user was so Login can send them back after re-auth.
  const next = window.location.pathname + window.location.search;
  window.location.href = `/login?next=${encodeURIComponent(next)}`;
}

http.interceptors.response.use(
  (r) => r,
  async (err: AxiosError) => {
    const original = err.config as AxiosRequestConfig & { _retry?: boolean };
    // The backend rejects the org header when the caller is no longer a
    // member (removed from the org, org deleted). Fall back to personal —
    // the guard in ConsoleLayout will also catch this via the org list,
    // but the request path must self-heal for direct API usage too.
    if (
      err.response?.status === 403 &&
      (err.response.data as { code?: string } | undefined)?.code === "org_context_invalid"
    ) {
      resetOrgContext();
      return Promise.reject(err);
    }
    if (err.response?.status !== 401 || !original || isAuthRequest(original.url)) {
      return Promise.reject(err);
    }

    // Two-factor step-up. This 401 says "prove it's you", NOT "your session
    // died" — it must be handled before the refresh path below, which would
    // otherwise spend a refresh replaying the same code-less request and, if
    // the refresh failed, log the user out over a prompt they never saw.
    const code = (err.response.data as { code?: string } | undefined)?.code;
    if (code === "totp_stepup_required" || code === "totp_invalid_code") {
      const stepUp = original as typeof original & { _stepUpTries?: number };
      const tries = stepUp._stepUpTries ?? 0;
      // Three chances, then give up so a mistyped secret cannot loop forever.
      if (!stepUpPromptAvailable() || tries >= 3) {
        return Promise.reject(err);
      }
      const entered = await requestStepUpCode(code === "totp_invalid_code" || tries > 0);
      if (!entered) {
        return Promise.reject(err);
      }
      stepUp._stepUpTries = tries + 1;
      stepUp.headers = stepUp.headers instanceof AxiosHeaders
        ? stepUp.headers
        : AxiosHeaders.from(stepUp.headers as RawAxiosHeaders | undefined);
      stepUp.headers.set("X-TOTP-Code", entered);
      return http(stepUp);
    }

    const auth = useAuthStore.getState();
    const hasRT = !!auth.tokens?.refresh_token;

    // A 401 that arrives on a request we already retried with a freshly
    // refreshed token is not an expired session — the refresh proved the
    // credentials are good, so the endpoint itself is rejecting us. Tearing
    // down a working session because one endpoint misbehaves is the wrong
    // response: it turns a broken widget into a forced logout for the whole
    // app. Surface the error to the caller and leave the session alone.
    //
    // (Genuine loss of access does not land here: the backend answers a
    // disabled/banned account with 403 account_disabled, not 401.)
    if (original._retry) {
      if (import.meta.env.DEV) {
        console.warn(
          `[auth] ${original.url} still 401 after a successful token refresh — ` +
            `treating as an endpoint fault, not a session expiry`,
        );
      }
      return Promise.reject(err);
    }

    // First 401 on a normal request → try a single refresh, then retry.
    if (hasRT) {
      original._retry = true;
      try {
        if (!refreshing) {
          refreshing = auth.refresh().finally(() => { refreshing = null; });
        }
        await refreshing;
        const tok = useAuthStore.getState().tokens?.access_token;
        if (tok) {
          original.headers = original.headers instanceof AxiosHeaders
            ? original.headers
            : AxiosHeaders.from(original.headers as RawAxiosHeaders | undefined);
          original.headers.set("Authorization", `Bearer ${tok}`);
          return http(original);
        }
      } catch {
        // refresh itself failed (e.g. refresh_token rotated/expired) →
        // fall through to the session-expired branch below.
      }
    }

    // No refresh token, or the refresh call itself failed. This is the only
    // remaining evidence of an actually unrecoverable session, so drop local
    // state and bounce — but only if the user was logged in, so cold-start
    // 401s on public-but-token-stale tabs don't yank anonymous traffic.
    if (auth.user || auth.tokens) {
      await auth.logout().catch(() => {});
      bounceToLogin();
    }
    return Promise.reject(err);
  },
);

export function errMsg(e: unknown): string {
  if (e == null) return ""; // no error → empty (callers gate display on this)
  if (axios.isAxiosError(e)) {
    const data: unknown = e.response?.data;
    if (typeof data === "object" && data !== null && "error" in data && typeof data.error === "string") {
      return data.error;
    }
    return e.message;
  }
  if (e instanceof Error) return e.message;
  return String(e);
}

// errCode reads the machine-readable `code` some endpoints return alongside
// `error` (registration_disabled, invite_code_unknown, …). Callers use it to
// pick a localized message instead of showing the backend's English string.
export function errCode(e: unknown): string | undefined {
  if (!axios.isAxiosError(e)) return undefined;
  const data: unknown = e.response?.data;
  if (typeof data === "object" && data !== null && "code" in data) {
    const code = (data as { code?: unknown }).code;
    if (typeof code === "string") return code;
  }
  return undefined;
}
