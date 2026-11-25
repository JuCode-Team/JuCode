import { useState, type InputHTMLAttributes } from "react";
import { useTranslation } from "react-i18next";
import { Eye, EyeOff } from "lucide-react";

// Password input with a show/hide toggle. The toggle is type="button" so it
// never submits, and tabIndex=-1 keeps Tab moving from password to the next
// field, the order password managers and keyboard users expect.
export default function PasswordInput(props: Omit<InputHTMLAttributes<HTMLInputElement>, "type">) {
  const { t } = useTranslation();
  const [visible, setVisible] = useState(false);
  const label = visible ? t("auth.hidePassword") : t("auth.showPassword");
  return (
    <span className="pn-auth-password">
      <input {...props} type={visible ? "text" : "password"} className="pn-auth-input" />
      <button
        type="button"
        tabIndex={-1}
        className="pn-auth-reveal"
        onClick={() => setVisible((v) => !v)}
        aria-label={label}
        title={label}
        aria-pressed={visible}
      >
        {visible ? <EyeOff size={16} /> : <Eye size={16} />}
      </button>
    </span>
  );
}
