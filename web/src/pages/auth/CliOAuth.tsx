import { useState, type ComponentType } from "react";
import { useTranslation } from "react-i18next";
import { Globe, Terminal, Monitor, Cpu, BarChart3 } from "lucide-react";
import { oauthApi } from "../../api/auth";
import { errMsg } from "../../api/client";
import { useAuthStore } from "../../store/auth";
import PremiumButton from "../../components/PremiumButton";

function requiredParam(params: URLSearchParams, key: string): string {
  return params.get(key)?.trim() || "";
}

function Permission({ Icon, title, desc }: { Icon: ComponentType<{ size?: number }>; title: string; desc: string }) {
  return (
    <div style={{ display: "flex", gap: 12, alignItems: "flex-start", padding: "10px 0" }}>
      <span style={{ color: "var(--c-primary-bright, var(--c-primary))", marginTop: 1, flex: "none" }}>
        <Icon size={18} />
      </span>
      <div>
        <div style={{ fontSize: 14, fontWeight: 600, color: "var(--c-on-surface)" }}>{title}</div>
        <div style={{ fontSize: 12, color: "var(--c-on-surface-variant)", marginTop: 2 }}>{desc}</div>
      </div>
    </div>
  );
}

export default function CliOAuth() {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // Browsers may refuse to open an app (jucode://) from a script after an
  // async request; a link the user taps always works.
  const [redirectTo, setRedirectTo] = useState<string | null>(null);
  const user = useAuthStore((s) => s.user);
  const params = new URLSearchParams(window.location.search);
  const clientID = requiredParam(params, "client_id");
  const redirectURI = requiredParam(params, "redirect_uri");
  const clientMeta: Record<string, { name: string; Icon: ComponentType<{ size?: number }> }> = {
    "jucode-desktop": { name: "JuCode Desktop", Icon: Monitor },
    "jucode-cli": { name: "JuCode CLI", Icon: Terminal },
    "jucode-web": { name: t("pub.oauth.client.web"), Icon: Globe },
  };
  // Unknown clients show their id; the backend decides whether it is registered.
  const meta = clientMeta[clientID] ?? { name: clientID, Icon: Terminal };
  const clientName = meta.name;
  const ClientIcon = meta.Icon;
  const valid =
    requiredParam(params, "response_type") === "code" &&
    clientID &&
    redirectURI &&
    requiredParam(params, "code_challenge") &&
    requiredParam(params, "code_challenge_method") === "S256" &&
    requiredParam(params, "state");

  const authorize = async () => {
    if (!valid) return;
    setErr(null);
    setLoading(true);
    try {
      const res = await oauthApi.authorize(params);
      setRedirectTo(res.redirect_to);
      window.location.assign(res.redirect_to);
    } catch (e) {
      setErr(errMsg(e));
      setLoading(false);
    }
  };

  return (
    <div>
      <div className="pn-auth-pending" style={{ marginBottom: 16 }}>
        <div className="pn-auth-pending-title">
          <ClientIcon size={16} /> {t("pub.oauth.title", { client: clientName })}
        </div>
        <div className="pn-auth-pending-body">
          {t("pub.oauth.body", { client: clientName })}
        </div>
        {user?.email && (
          <div className="pn-auth-pending-email">{t("pub.oauth.signedInAs", { who: user.nickname ? `${user.nickname} · ${user.email}` : user.email })}</div>
        )}
      </div>

      <div style={{ marginBottom: 18, borderTop: "1px solid var(--c-outline-variant, rgba(128,128,128,0.2))" }}>
        <Permission Icon={Cpu} title={t("pub.oauth.permModels")} desc={t("pub.oauth.permModelsDesc")} />
        <Permission Icon={BarChart3} title={t("pub.oauth.permAccount")} desc={t("pub.oauth.permAccountDesc")} />
      </div>

      {redirectURI && (
        <div className="pn-auth-pending-email" style={{ wordBreak: "break-all", marginBottom: 14 }}>
          {t("pub.oauth.redirect", { uri: redirectURI })}
        </div>
      )}

      {!valid && <div className="pn-auth-error">{t("pub.oauth.invalid")}</div>}
      {err && <div className="pn-auth-error">{err}</div>}

      {redirectTo ? (
        <PremiumButton href={redirectTo} size="block">{t("pub.oauth.authorized", { client: clientName })}</PremiumButton>
      ) : (
        <PremiumButton type="button" size="block" disabled={!valid || loading} onClick={authorize}>
          {loading ? t("pub.oauth.pending") : t("pub.oauth.authorize", { client: clientName })}
        </PremiumButton>
      )}
    </div>
  );
}
