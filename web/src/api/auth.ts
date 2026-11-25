import { http } from "./client";
import type { Tokens, User } from "./endpoints";

export const authApi = {
  sendRegisterVerifyCode: (body: { email: string }) =>
    http.post<{ ok: boolean; cooldown_seconds: number }>("/v1/auth/send-register-verify-code", body).then((response) => response.data),
  register: (body: { email: string; password: string; nickname?: string; verify_code?: string; invite_code?: string }) =>
    http.post<{ user: User; tokens?: Tokens; pending_approval?: boolean; pending_verification?: boolean; message?: string }>(
      "/v1/auth/register",
      body,
    ).then((response) => response.data),
  // Pre-flight for the invite field. Always 200 — an unusable code is an
  // answer, not a failed request — so the register form can call this on
  // blur without special-casing. Registration re-checks server-side.
  checkInviteCode: (code: string) =>
    http.get<{ valid: boolean; reason?: "malformed" | "unknown" | "unavailable" }>(
      "/v1/auth/invite-code/check",
      { params: { code } },
    ).then((response) => response.data),
  // TOTP 账号密码正确时只返回 mfa_required + mfa_token,需再调 loginTotp。
  login: (body: { email: string; password: string }) =>
    http.post<{
      user?: User;
      tokens?: Tokens;
      mfa_required?: boolean;
      mfa_token?: string;
      totp_setup_required?: boolean;
    }>("/v1/auth/login", body).then((response) => response.data),
  loginTotp: (body: { mfa_token: string; code?: string; recovery_code?: string }) =>
    http.post<{ user: User; tokens: Tokens }>("/v1/auth/totp", body).then((response) => response.data),
  refresh: (refreshToken: string) =>
    http.post<{ tokens: Tokens }>("/v1/auth/refresh", { refresh_token: refreshToken }).then((response) => response.data),
  logout: (refreshToken: string) => http.post("/v1/auth/logout", { refresh_token: refreshToken }),
  verifyEmail: (body: { code: string }) =>
    http.post<{ ok: boolean; user: User; kind: "register" | "change_email" }>("/v1/auth/verify-email", body).then((response) => response.data),
  forgotPassword: (body: { email: string }) =>
    http.post<void>("/v1/auth/forgot-password", body).then((response) => response.data),
  resetPassword: (body: { code: string; new_password: string }) =>
    http.post<void>("/v1/auth/reset-password", body).then((response) => response.data),
};

export const oauthApi = {
  authorize: (params: URLSearchParams) =>
    http.post<{ redirect_to: string; expires_in: number }>(
      `/v1/oauth/authorize?${params.toString()}`,
    ).then((response) => response.data),
};
