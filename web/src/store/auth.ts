import { create } from "zustand";
import { persist } from "zustand/middleware";
import { authApi } from "../api/auth";
import type { Tokens, User } from "../api/endpoints";
import { queryClient } from "../lib/queryClient";
import { resetOrgContext } from "./orgContext";

// "Remember me" key stored alongside the persisted auth blob. When set,
// it carries an absolute ms timestamp at which the auto-login window
// expires (default 7 days from sign-in). When unset, the session is
// "session-only": closing the browser drops it (see initAuthLifecycle
// below for the sessionStorage handshake that enforces this).
const REMEMBER_UNTIL_KEY = "jucode-remember-until";
const SESSION_MARK_KEY = "jucode-session-active";
export const REMEMBER_DURATION_MS = 7 * 24 * 60 * 60 * 1000;

interface AuthState {
  user: User | null;
  tokens: Tokens | null;
  // login resolves to { mfaToken } (session NOT opened) when the account
  // has TOTP enabled — the caller must follow up with loginTotp.
  login: (email: string, password: string, remember?: boolean) => Promise<{ mfaToken?: string }>;
  loginTotp: (mfaToken: string, body: { code?: string; recoveryCode?: string }, remember?: boolean) => Promise<void>;
  register: (email: string, password: string, nickname?: string, verifyCode?: string, inviteCode?: string) => Promise<{ pending: boolean; message?: string }>;
  refresh: () => Promise<void>;
  logout: () => Promise<void>;
  setUser: (u: User) => void;
}

// openSession finalizes a successful sign-in: wipes the previous
// account's caches/org context, stores user + tokens, and records the
// remember-me preference. Shared by the plain and the TOTP login paths.
function openSession(
  set: (partial: Partial<AuthState>) => void,
  user: User,
  tokens: Tokens,
  remember: boolean,
): void {
  queryClient.clear();
  // A fresh sign-in must not inherit the previous account's org
  // context — the new user may not even be a member of that org.
  resetOrgContext();
  set({ user, tokens });
  // Record the "remember me" preference at the storage layer so
  // initAuthLifecycle can decide on the next page open whether to
  // honor the persisted tokens or treat them as session-only.
  if (remember) {
    localStorage.setItem(REMEMBER_UNTIL_KEY, String(Date.now() + REMEMBER_DURATION_MS));
  } else {
    localStorage.removeItem(REMEMBER_UNTIL_KEY);
  }
  // Mark the current tab/window as an active session. Stays in
  // sessionStorage so it disappears when the browser is closed —
  // that disappearance is exactly the signal we need to drop a
  // not-remembered login on the next visit.
  sessionStorage.setItem(SESSION_MARK_KEY, "1");
}

export const useAuthStore = create<AuthState>()(
  persist(
    (set, get) => ({
      user: null,
      tokens: null,
      async login(email, password, remember = false) {
        const r = await authApi.login({ email, password });
        if (r.mfa_required && r.mfa_token) {
          // Password ok but the account wants a TOTP code. No session
          // yet — the Login page shows the code step and calls loginTotp.
          return { mfaToken: r.mfa_token };
        }
        openSession(set, r.user!, r.tokens!, remember);
        return {};
      },
      async loginTotp(mfaToken, body, remember = false) {
        const r = await authApi.loginTotp({
          mfa_token: mfaToken,
          code: body.code,
          recovery_code: body.recoveryCode,
        });
        openSession(set, r.user, r.tokens, remember);
      },
      async register(email, password, nickname, verifyCode, inviteCode) {
        const r = await authApi.register({
          email,
          password,
          nickname,
          verify_code: verifyCode,
          invite_code: inviteCode,
        });
        if (r.pending_approval || !r.tokens) {
          // Approval workflow: no session is opened. The Register page is
          // responsible for showing the "thanks, awaiting review" UI.
          return { pending: true, message: r.message };
        }
        queryClient.clear();
        set({ user: r.user, tokens: r.tokens });
        sessionStorage.setItem(SESSION_MARK_KEY, "1");
        return { pending: false };
      },
      async refresh() {
        const rt = get().tokens?.refresh_token;
        if (!rt) throw new Error("no refresh token");
        const r = await authApi.refresh(rt);
        set({ tokens: r.tokens });
      },
      async logout() {
        const rt = get().tokens?.refresh_token;
        if (rt) {
          try {
            await authApi.logout(rt);
          } catch {
            // best-effort
          }
        }
        set({ user: null, tokens: null });
        queryClient.clear();
        resetOrgContext();
        localStorage.removeItem(REMEMBER_UNTIL_KEY);
        sessionStorage.removeItem(SESSION_MARK_KEY);
      },
      setUser(u) {
        set({ user: u });
      },
    }),
    { name: "jucode-auth" },
  ),
);

// initAuthLifecycle is called once at app bootstrap (main.tsx). It
// reconciles three pieces of state on disk:
//
//   localStorage  jucode-auth                — persisted user + tokens
//   localStorage  jucode-remember-until      — ms timestamp; only set when
//                                              user ticked "记住我 7 天"
//   sessionStorage jucode-session-active     — "1" while this browser
//                                              tab/window has stayed open
//
// Rules:
//   - If remember-until is in the future → keep the session. Refresh the
//     session-mark for this tab so refreshes still feel instant.
//   - If remember-until is missing or expired AND the session mark is
//     absent (meaning the browser was closed since last login) → treat
//     the persisted tokens as stale and wipe the auth store. The next
//     navigation lands on /login. This is what makes the "7 天自动登录"
//     checkbox meaningful: not ticked = closing the browser logs you out.
export function initAuthLifecycle(): void {
  const until = parseInt(localStorage.getItem(REMEMBER_UNTIL_KEY) || "0", 10);
  const hasSession = sessionStorage.getItem(SESSION_MARK_KEY) === "1";
  const now = Date.now();

  if (until > now) {
    // Remember-me still valid — promote into this tab as an active session.
    sessionStorage.setItem(SESSION_MARK_KEY, "1");
    return;
  }
  if (until > 0 && until <= now) {
    // Remember-me window has elapsed → drop everything.
    localStorage.removeItem(REMEMBER_UNTIL_KEY);
  }
  if (!hasSession) {
    // Browser was closed since the last sign-in (sessionStorage cleared)
    // AND the user did not opt into 7-day remember-me → invalidate.
    useAuthStore.setState({ user: null, tokens: null });
    queryClient.clear();
    resetOrgContext();
    // Best-effort cleanup; ignore failures (the slot may already be gone).
    try { localStorage.removeItem("jucode-auth"); } catch { /* ignore */ }
  }
}

export function isAdmin(): boolean {
  return useAuthStore.getState().user?.role === "admin";
}

// useHasRole is the reactive variant: returns true when any of the
// listed roles is granted to the signed-in user (admin always
// passes). Use inside components so sidebar/nav repaints when the
// /me payload arrives.
export function useHasRole(role: "cs" | "agent"): boolean {
  return useAuthStore((s) => {
    const u = s.user;
    if (!u) return false;
    if (u.role === "admin") return true;
    return (u.roles ?? []).includes(role);
  });
}
