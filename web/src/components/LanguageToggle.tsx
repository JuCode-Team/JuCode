import { useTranslation } from "react-i18next";
import { currentLanguage, setLanguage } from "../i18n";

// Two languages only, so a toggle beats a dropdown: the button shows the
// language you'd get by pressing it. Callers pass the class list because
// the marketing shell (Tailwind) and the auth shell (pn-* CSS) style their
// controls differently.
export default function LanguageToggle({ className }: { className?: string }) {
  const { t } = useTranslation();
  const next = currentLanguage() === "zh" ? "en" : "zh";
  return (
    <button
      type="button"
      onClick={() => setLanguage(next)}
      aria-label={t("lang.switchTo")}
      title={t("lang.switchTo")}
      className={className}
    >
      {next === "en" ? "EN" : "中"}
    </button>
  );
}
