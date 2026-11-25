import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { authApi } from "../../api/auth";
import { errMsg } from "../../api/client";
import { useAuthStore } from "../../store/auth";
import PremiumButton from "../../components/PremiumButton";

export default function VerifyEmail() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const loggedIn = useAuthStore((s) => !!s.user && !!s.tokens);
  const [params] = useSearchParams();
  const code = params.get("code") ?? "";
  const [kind, setKind] = useState<"register" | "change_email" | null>(null);
  const [state, setState] = useState<"loading" | "ok" | "error">("loading");
  const [errorText, setErrorText] = useState("");
  const request = useRef<{ code: string; promise: ReturnType<typeof authApi.verifyEmail> } | null>(null);

  useEffect(() => {
    if (!code) {
      setState("error");
      return;
    }
    let active = true;
    setState("loading");
    // StrictMode re-runs effects; reuse the request for this one-time link.
    if (!request.current || request.current.code !== code) {
      request.current = { code, promise: authApi.verifyEmail({ code }) };
    }
    request.current.promise
      .then((result) => {
        if (!active) return;
        setKind(result.kind);
        setState("ok");
        qc.invalidateQueries({ queryKey: ["me"] });
      })
      .catch((error) => {
        if (!active) return;
        setState("error");
        setErrorText(errMsg(error));
      });
    return () => { active = false; };
  }, [code, qc]);

  const title = state === "loading"
    ? t("pub.verify.loadingTitle")
    : state === "ok"
      ? (kind === "register" ? t("pub.verify.verifiedTitle") : t("pub.verify.updatedTitle"))
      : t("pub.verify.failedTitle");

  return (
    <div className="pn-auth-state">
      <h1 className="pn-auth-heading">{title}</h1>
      <p className="pn-auth-copy">
        {state === "loading" && t("pub.verify.loadingCopy")}
        {state === "ok" && (kind === "register"
          ? (loggedIn ? t("pub.verify.verifiedInvite") : t("pub.verify.verifiedLogin"))
          : t("pub.verify.updatedCopy"))}
        {state === "error" && (code ? errorText : t("pub.verify.missingCode"))}
      </p>
      {state === "loading" && <div className="pn-auth-progress"><i /></div>}
      {state !== "loading" && (
        state === "ok" && kind === "register" && loggedIn
          ? <PremiumButton size="block" to="/me/invite">{t("pub.verify.backToInvite")}</PremiumButton>
          : <PremiumButton size="block" to="/login">{t("pub.verify.toLogin")}</PremiumButton>
      )}
    </div>
  );
}
