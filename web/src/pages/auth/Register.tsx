import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { CheckCircle2, Send, UserPlus } from "lucide-react";
import { useAuthStore } from "../../store/auth";
import { useSite } from "../../store/site";
import { authApi } from "../../api/auth";
import { errCode, errMsg } from "../../api/client";
import PremiumButton from "../../components/PremiumButton";
import PasswordInput from "./PasswordInput";

// The register form shows the verdict of the last check against the code the
// visitor typed. "checking" is transient; the rest are terminal until they
// edit the field again.
type InviteStatus = "idle" | "checking" | "valid" | "malformed" | "unknown";

export default function Register() {
  const { t } = useTranslation();
  const register = useAuthStore((s) => s.register);
  const site = useSite();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [nickname, setNickname] = useState("");
  const [searchParams] = useSearchParams();
  const [inviteCode, setInviteCode] = useState(
    () => searchParams.get("code") || searchParams.get("invite") || "",
  );
  const [inviteStatus, setInviteStatus] = useState<InviteStatus>("idle");
  // Mirrors inviteCode so an in-flight check can tell whether its verdict is
  // still about what the visitor sees, or about a code they already edited.
  const inviteCodeRef = useRef(inviteCode);
  inviteCodeRef.current = inviteCode;
  const [verifyCode, setVerifyCode] = useState("");
  const [codeSent, setCodeSent] = useState(false);
  const [codeLoading, setCodeLoading] = useState(false);
  const [cooldown, setCooldown] = useState(0);
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [pending, setPending] = useState<{ message: string } | null>(null);
  const nav = useNavigate();
  const requireVerify = site.require_register_verification;

  useEffect(() => {
    if (cooldown <= 0) return;
    const timer = window.setTimeout(() => setCooldown((v) => Math.max(0, v - 1)), 1000);
    return () => window.clearTimeout(timer);
  }, [cooldown]);

  const sendCode = async () => {
    setErr(null);
    setCodeSent(false);
    setCodeLoading(true);
    try {
      const result = await authApi.sendRegisterVerifyCode({ email });
      setCooldown(result.cooldown_seconds || 60);
      setCodeSent(true);
    } catch (e) {
      setErr(errMsg(e));
    } finally {
      setCodeLoading(false);
    }
  };

  // Checks on blur rather than per keystroke: a half-typed code is always
  // "unknown", and flagging it mid-word is just noise. A failed request
  // leaves the field neutral — registration re-validates server-side and
  // reports the real reason, so a flaky check must not block the form.
  const verifyInviteCode = async () => {
    const code = inviteCode.trim();
    if (!code) {
      setInviteStatus("idle");
      return;
    }
    setInviteStatus("checking");
    try {
      const { valid, reason } = await authApi.checkInviteCode(code);
      if (inviteCodeRef.current.trim() !== code) return; // superseded
      setInviteStatus(valid ? "valid" : reason === "malformed" ? "malformed" : "unknown");
    } catch {
      if (inviteCodeRef.current.trim() === code) setInviteStatus("idle");
    }
  };

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(null);
    setLoading(true);
    try {
      const result = await register(
        email,
        password,
        nickname || undefined,
        requireVerify ? verifyCode : undefined,
        inviteCode.trim() || undefined,
      );
      if (result.pending) {
        setPending({ message: result.message || t("auth.pendingBody") });
        return;
      }
      nav("/me");
    } catch (e) {
      // An invite-code rejection belongs on the field itself; anything else
      // is a whole-form problem.
      const code = errCode(e);
      if (code === "invite_code_malformed" || code === "invite_code_unknown") {
        setInviteStatus(code === "invite_code_malformed" ? "malformed" : "unknown");
      } else {
        setErr(errMsg(e));
      }
    } finally {
      setLoading(false);
    }
  };

  if (pending) {
    return (
      <div className="pn-auth-pending">
        <div className="pn-auth-pending-title">
          <CheckCircle2 size={18} style={{ verticalAlign: "-3px", marginRight: 8 }} />
          {t("auth.pendingTitle")}
        </div>
        <div className="pn-auth-pending-body">{pending.message}</div>
        <div className="pn-auth-pending-email">
          <span style={{ opacity: 0.6, marginRight: 6 }}>{t("auth.email")}</span>
          {email}
        </div>
        <p className="pn-auth-meta">
          <Link to="/login">{t("auth.backToLogin")}</Link>
        </p>
      </div>
    );
  }

  if (!site.registration_enabled) {
    return (
      <div>
        <h1 className="pn-auth-heading">{t("pub.register.closedTitle")}</h1>
        <p className="pn-auth-copy">{t("pub.register.closedCopy")}</p>
        <p className="pn-auth-meta">
          <Link to="/login">{t("auth.backToLogin")}</Link>
        </p>
      </div>
    );
  }

  return (
    <form onSubmit={onSubmit} autoComplete="on">
      <h1 className="pn-auth-heading">{t("auth.registerHeading")}</h1>
      <p className="pn-auth-copy">{t("auth.registerCopy")}</p>
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">{t("auth.email")}</span>
        <input
          type="email"
          className="pn-auth-input"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
          autoComplete="email"
          placeholder="you@example.com"
        />
      </label>
      {requireVerify && (
        <div className="pn-auth-field">
          <span className="pn-auth-field-label">{t("auth.verifyCode")}</span>
          <div className="pn-auth-code-row">
            <input
              type="text"
              inputMode="numeric"
              className="pn-auth-input"
              value={verifyCode}
              onChange={(e) => setVerifyCode(e.target.value)}
              required
              maxLength={6}
              autoComplete="one-time-code"
              placeholder={t("auth.verifyCodePlaceholder")}
            />
            <button
              type="button"
              className="pn-auth-code-button"
              disabled={codeLoading || cooldown > 0 || !email}
              onClick={sendCode}
            >
              {codeLoading ? t("auth.sendingCode") : cooldown > 0 ? `${cooldown}s` : (<><Send size={15} /> {t("auth.sendCode")}</>)}
            </button>
          </div>
          {codeSent && <div className="pn-auth-hint">{t("auth.codeSent")}</div>}
        </div>
      )}
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">{t("auth.password")}</span>
        <PasswordInput
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          minLength={8}
          autoComplete="new-password"
          placeholder="••••••••"
        />
        <div className="pn-auth-hint is-muted">{t("auth.passwordHint")}</div>
      </label>
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">
          {t("auth.nickname")}
          <span className="pn-auth-field-aside">{t("auth.optional")}</span>
        </span>
        <input
          type="text"
          className="pn-auth-input"
          value={nickname}
          onChange={(e) => setNickname(e.target.value)}
          placeholder={t("auth.nicknamePlaceholder")}
        />
      </label>
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">
          {t("auth.inviteCode")}
          <span className="pn-auth-field-aside">{t("auth.optional")}</span>
        </span>
        <input
          type="text"
          className="pn-auth-input pn-auth-invite-input"
          value={inviteCode}
          onChange={(e) => {
            setInviteCode(e.target.value);
            // The previous verdict was about a different string.
            setInviteStatus("idle");
          }}
          onBlur={verifyInviteCode}
          autoComplete="off"
          spellCheck={false}
          placeholder={t("auth.inviteCodePlaceholder")}
        />
        {inviteStatus !== "idle" && (
          <div
            className={
              "pn-auth-hint" +
              (inviteStatus === "malformed" || inviteStatus === "unknown" ? " is-error" : "")
            }
          >
            {inviteStatus === "checking" && t("auth.inviteCodeChecking")}
            {inviteStatus === "valid" && t("auth.inviteCodeValid")}
            {inviteStatus === "malformed" && t("auth.inviteCodeMalformed")}
            {inviteStatus === "unknown" && t("auth.inviteCodeUnknown")}
          </div>
        )}
      </label>

      {err && <div className="pn-auth-error">{err}</div>}

      <PremiumButton type="submit" size="block" disabled={loading}>
        {loading ? t("auth.registerPending") : (<><UserPlus size={16} /> {t("auth.registerSubmit")}</>)}
      </PremiumButton>

      <div className="pn-auth-alt">
        <span>{t("auth.hasAccount")}</span>
        <PremiumButton to="/login" variant="secondary" size="block">
          {t("auth.goLogin")}
        </PremiumButton>
      </div>
    </form>
  );
}
