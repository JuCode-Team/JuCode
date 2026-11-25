import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { authApi } from "../../api/auth";
import { errMsg } from "../../api/client";
import PremiumButton from "../../components/PremiumButton";

export default function ForgotPassword() {
  const { t } = useTranslation();
  const [email, setEmail] = useState("");
  const mutation = useMutation({
    mutationFn: () => authApi.forgotPassword({ email: email.trim() }),
  });

  if (mutation.isSuccess) {
    return (
      <div className="pn-auth-state">
        <h1 className="pn-auth-heading">{t("pub.forgot.sentTitle")}</h1>
        <p className="pn-auth-copy">{t("pub.forgot.sentCopy")}</p>
        <PremiumButton size="block" to="/login">{t("pub.forgot.back")}</PremiumButton>
      </div>
    );
  }

  return (
    <form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
      <h1 className="pn-auth-heading">{t("pub.forgot.title")}</h1>
      <p className="pn-auth-copy">{t("pub.forgot.copy")}</p>
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">{t("pub.forgot.email")}</span>
        <input
          type="email"
          className="pn-auth-input"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          autoComplete="email"
          placeholder="you@example.com"
          required
        />
      </label>
      {mutation.isError && <div className="pn-auth-error">{t("pub.forgot.failed", { error: errMsg(mutation.error) })}</div>}
      <PremiumButton type="submit" size="block" disabled={!email.includes("@") || mutation.isPending}>
        {mutation.isPending ? t("pub.forgot.pending") : t("pub.forgot.submit")}
      </PremiumButton>
      <p className="pn-auth-meta"><Link to="/login">{t("pub.forgot.back")}</Link></p>
    </form>
  );
}
