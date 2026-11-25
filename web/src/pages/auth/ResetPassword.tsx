import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Link, useSearchParams } from "react-router-dom";
import { authApi } from "../../api/auth";
import { errMsg } from "../../api/client";
import PremiumButton from "../../components/PremiumButton";

export default function ResetPassword() {
  const { t } = useTranslation();
  const [params] = useSearchParams();
  const code = params.get("code") ?? "";
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const mutation = useMutation({
    mutationFn: () => authApi.resetPassword({ code, new_password: newPassword }),
  });

  if (!code) {
    return (
      <div className="pn-auth-state">
        <h1 className="pn-auth-heading">{t("pub.reset.invalidTitle")}</h1>
        <p className="pn-auth-copy">{t("pub.reset.invalidCopy")}</p>
        <PremiumButton size="block" to="/forgot-password">{t("pub.reset.newLink")}</PremiumButton>
      </div>
    );
  }

  if (mutation.isSuccess) {
    return (
      <div className="pn-auth-state">
        <h1 className="pn-auth-heading">{t("pub.reset.doneTitle")}</h1>
        <p className="pn-auth-copy">{t("pub.reset.doneCopy")}</p>
        <PremiumButton size="block" to="/login">{t("pub.reset.toLogin")}</PremiumButton>
      </div>
    );
  }

  const mismatch = confirmPassword !== "" && confirmPassword !== newPassword;
  return (
    <form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
      <h1 className="pn-auth-heading">{t("pub.reset.title")}</h1>
      <p className="pn-auth-copy">{t("pub.reset.copy")}</p>
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">{t("pub.reset.newPassword")}</span>
        <input type="password" className="pn-auth-input" value={newPassword} onChange={(event) => setNewPassword(event.target.value)} autoComplete="new-password" required minLength={8} />
      </label>
      <label className="pn-auth-field">
        <span className="pn-auth-field-label">{t("pub.reset.confirmPassword")}</span>
        <input type="password" className="pn-auth-input" value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} autoComplete="new-password" required minLength={8} />
      </label>
      {mismatch && <div className="pn-auth-error">{t("pub.reset.mismatch")}</div>}
      {mutation.isError && <div className="pn-auth-error">{errMsg(mutation.error)}</div>}
      <PremiumButton type="submit" size="block" disabled={newPassword.length < 8 || mismatch || mutation.isPending}>
        {mutation.isPending ? t("pub.reset.pending") : t("pub.reset.submit")}
      </PremiumButton>
      <p className="pn-auth-meta"><Link to="/login">{t("pub.reset.cancel")}</Link></p>
    </form>
  );
}
