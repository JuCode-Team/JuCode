import { useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { LogIn, ShieldCheck } from "lucide-react";
import { useAuthStore } from "../../store/auth";
import { errMsg } from "../../api/client";
import PremiumButton from "../../components/PremiumButton";
import PasswordInput from "./PasswordInput";

// Only honor next= when it's an in-app absolute path. Strip absolute URLs
// and protocol-relative URLs so a crafted ?next=https://evil.example
// can't redirect a freshly-logged-in user off-site.
function safeNext(raw: string | null): string {
  if (!raw) return "/me";
  if (!raw.startsWith("/") || raw.startsWith("//")) return "/me";
  return raw;
}

export default function Login() {
  const { t } = useTranslation();
  const login = useAuthStore((s) => s.login);
  const loginTotp = useAuthStore((s) => s.loginTotp);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  // "remember" defaults to false: closing the browser drops the session
  // unless the user explicitly opted into the 7-day window. Avoids the
  // shared-computer footgun where a tick-by-default checkbox would
  // persist someone else's login.
  const [remember, setRemember] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  // TOTP second step: non-null mfaToken switches the form to code entry.
  const [mfaToken, setMfaToken] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [useRecovery, setUseRecovery] = useState(false);
  const nav = useNavigate();
  const [params] = useSearchParams();
  const next = safeNext(params.get("next"));

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(null);
    setLoading(true);
    try {
      const r = await login(email, password, remember);
      if (r.mfaToken) {
        setMfaToken(r.mfaToken);
        return;
      }
      nav(next, { replace: true });
    } catch (e) {
      setErr(errMsg(e));
    } finally {
      setLoading(false);
    }
  };

  const onSubmitCode = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!mfaToken) return;
    setErr(null);
    setLoading(true);
    try {
      await loginTotp(
        mfaToken,
        useRecovery ? { recoveryCode: code.trim() } : { code: code.trim() },
        remember,
      );
      nav(next, { replace: true });
    } catch (e) {
      const msg = errMsg(e);
      // Expired / burned ticket → back to the password step.
      if (msg.includes("start over") || msg.includes("expired")) {
        setMfaToken(null);
        setCode("");
        setErr(t("auth.totpExpired"));
      } else {
        setErr(msg);
      }
    } finally {
      setLoading(false);
    }
  };

  if (mfaToken) {
    return (
      <form onSubmit={onSubmitCode}>
        <h1 className="pn-auth-heading">{t("auth.totpHeading")}</h1>
        <p className="pn-auth-copy">
          {useRecovery ? t("auth.totpCopyRecovery") : t("auth.totpCopyApp")}
        </p>
        <label className="pn-auth-field">
          <span className="pn-auth-field-label">
            {useRecovery ? t("auth.totpRecoveryLabel") : t("auth.totpCodeLabel")}
          </span>
          <input
            type="text"
            className="pn-auth-input"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            required
            autoFocus
            autoComplete="one-time-code"
            inputMode={useRecovery ? "text" : "numeric"}
            placeholder={useRecovery ? "XXXXX-XXXXX" : "123456"}
            style={{ fontFamily: "ui-monospace, monospace", letterSpacing: 2 }}
          />
        </label>

        {err && <div className="pn-auth-error">{err}</div>}

        <PremiumButton type="submit" size="block" disabled={loading || !code.trim()}>
          {loading ? t("auth.totpPending") : (<><ShieldCheck size={16} /> {t("auth.totpSubmit")}</>)}
        </PremiumButton>

        <p className="pn-auth-meta">
          <a
            href="#"
            onClick={(e) => {
              e.preventDefault();
              setUseRecovery((v) => !v);
              setCode("");
              setErr(null);
            }}
          >
            {useRecovery ? t("auth.totpSwitchToApp") : t("auth.totpSwitchToRecovery")}
          </a>
          {" · "}
          <a
            href="#"
            onClick={(e) => {
              e.preventDefault();
              setMfaToken(null);
              setCode("");
              setErr(null);
            }}
          >
            {t("auth.totpBack")}
          </a>
        </p>
      </form>
    );
  }

  return (
    <form onSubmit={onSubmit} autoComplete="on">
      <h1 className="pn-auth-heading">{t("auth.loginHeading")}</h1>
      <p className="pn-auth-copy">{t(next.startsWith("/cli/oauth") ? "auth.loginCopyApp" : "auth.loginCopy")}</p>
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">{t("auth.identifier")}</span>
        <input
          type="text"
          className="pn-auth-input"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
          autoComplete="username"
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder={t("auth.identifierPlaceholder")}
        />
      </label>
      {/* div, not label: the reset link must not become part of the
          password field's accessible name. */}
      <div className="pn-auth-field">
        <div className="pn-auth-field-label">
          <label htmlFor="login-password">{t("auth.password")}</label>
          <Link to="/forgot-password" className="pn-auth-field-aside">{t("auth.forgotPassword")}</Link>
        </div>
        <PasswordInput
          id="login-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          autoComplete="current-password"
          placeholder="••••••••"
        />
      </div>

      <label className="pn-auth-check">
        <input
          type="checkbox"
          checked={remember}
          onChange={(e) => setRemember(e.target.checked)}
        />
        {t("auth.remember")}
      </label>

      {err && <div className="pn-auth-error">{err}</div>}

      <PremiumButton type="submit" size="block" disabled={loading}>
        {loading ? t("auth.loginPending") : (<><LogIn size={16} /> {t("auth.loginSubmit")}</>)}
      </PremiumButton>

      <div className="pn-auth-alt">
        <span>{t("auth.noAccount")}</span>
        <PremiumButton to="/register" variant="secondary" size="block">
          {t("auth.registerSubmit")}
        </PremiumButton>
      </div>
    </form>
  );
}
