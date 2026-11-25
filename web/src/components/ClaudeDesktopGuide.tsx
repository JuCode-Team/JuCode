import { useState } from "react";
import Claude from "@lobehub/icons/es/Claude/components/Color";
import Codex from "@lobehub/icons/es/Codex/components/Color";
import Gemini from "@lobehub/icons/es/Gemini/components/Color";
import { Check, Copy, Monitor, Plus, Terminal } from "lucide-react";
import { useTranslation } from "react-i18next";
import i18n from "../i18n";
import { copyText } from "../lib/copy";
import { deployment } from "../config/env";

// Interactive stand-in for the CC Switch window on ccswitch.io.
// Released builds reject app=claude-desktop, so this walks through the
// same clicks the desktop panel expects: tab, add, then API key and base URL.

type Step = 0 | 1 | 2 | 3;
type MockApp = "claude" | "claude-desktop" | "codex" | "gemini";

const STEPS: { id: Step; label: () => string }[] = [
  { id: 0, label: () => i18n.t("keys.desktopGuide.stepFind") },
  { id: 1, label: () => i18n.t("keys.desktopGuide.stepAdd") },
  { id: 2, label: () => i18n.t("keys.desktopGuide.stepFill") },
];

const PROMPT: Record<Step, () => string> = {
  0: () => i18n.t("keys.desktopGuide.prompt0"),
  1: () => i18n.t("keys.desktopGuide.prompt1"),
  2: () => i18n.t("keys.desktopGuide.prompt2"),
  3: () => i18n.t("keys.desktopGuide.prompt3"),
};

export function ClaudeDesktopGuide({
  providerName,
  endpoint,
  apiKey,
}: {
  providerName: string;
  endpoint: string;
  apiKey: string;
}) {
  const { t } = useTranslation();
  const [step, setStep] = useState<Step>(0);
  const [reached, setReached] = useState<Step>(0);
  const [mode, setMode] = useState<"direct" | "proxy">("direct");
  const view: MockApp = step === 0 ? "claude" : "claude-desktop";

  const go = (next: Step) => {
    setStep(next);
    setReached((current) => (next > current ? next : current));
    if (next < 2) setMode("direct");
  };

  return (
    <div className="jc-cd">
      <div className="jc-cd-rail" aria-label={t("keys.desktopGuide.steps")}>
        {STEPS.map((item) => (
          <button
            key={item.id}
            type="button"
            aria-current={step === item.id ? "step" : undefined}
            disabled={item.id > reached}
            onClick={() => go(item.id)}
          >
            {item.id + 1}. {item.label()}
          </button>
        ))}
      </div>
      <p className="jc-cd-prompt">{PROMPT[step]()}</p>

      <div className={`jc-cd-window ${step < 2 ? "has-coach" : ""}`}>
        <div className="jc-cd-bar">
          <span className="jc-cd-brand">CC Switch</span>
          <div className="jc-cd-tabs" role="tablist" aria-label={t("keys.desktopGuide.apps")}>
            <TabButton id="claude" active={view === "claude"} />
            <TabButton
              id="claude-desktop"
              active={view === "claude-desktop"}
              target={step === 0}
              onClick={() => go(1)}
            />
            <TabButton id="codex" active={false} />
            <TabButton id="gemini" active={false} />
          </div>
          <button
            type="button"
            className={`jc-cd-plus ${step === 1 ? "is-target" : ""}`}
            aria-label={t("keys.desktopGuide.addProvider")}
            disabled={step !== 1}
            onClick={() => go(2)}
          >
            <Plus size={16} />
            {step === 1 && <span className="jc-cd-tag">{t("keys.desktopGuide.clickHere")}</span>}
          </button>
        </div>

        <div className="jc-cd-body">
          {step === 2 ? (
            <AddForm
              providerName={providerName}
              endpoint={endpoint}
              apiKey={apiKey}
              mode={mode}
              onMode={setMode}
              onCancel={() => go(1)}
              onAdd={() => go(3)}
            />
          ) : (
            <ProviderList
              officialActive={step < 3}
              added={step === 3}
              providerName={providerName}
              endpoint={endpoint}
            />
          )}
        </div>
      </div>

      {step === 3 && (
        <button type="button" className="jc-cd-replay" onClick={() => go(0)}>
          {t("keys.desktopGuide.replay")}
        </button>
      )}
    </div>
  );
}

function TabButton({
  id,
  active,
  target,
  onClick,
}: {
  id: MockApp;
  active: boolean;
  target?: boolean;
  onClick?: () => void;
}) {
  const { t } = useTranslation();
  const label = id === "claude"
    ? "Claude Code"
    : id === "claude-desktop"
      ? "Claude Desktop"
      : id === "codex"
        ? "Codex"
        : "Gemini";
  return (
    <button
      type="button"
      role="tab"
      aria-label={label}
      aria-selected={active}
      className={`jc-cd-tab ${active ? "is-active" : ""} ${target ? "is-target" : ""}`}
      disabled={!onClick}
      onClick={onClick}
    >
      <TabMark id={id} />
      {target && <span className="jc-cd-tag">{t("keys.desktopGuide.clickHere")}</span>}
    </button>
  );
}

function TabMark({ id }: { id: MockApp }) {
  const badge = id === "claude"
    ? <Terminal size={8} strokeWidth={2.6} />
    : id === "claude-desktop"
      ? <Monitor size={8} strokeWidth={2.6} />
      : null;
  return (
    <span className="jc-cc-mark" aria-hidden>
      {id === "claude" && <Claude size={18} />}
      {id === "claude-desktop" && <Claude size={18} />}
      {id === "codex" && <Codex size={18} />}
      {id === "gemini" && <Gemini size={18} />}
      {badge && <span className="jc-cc-badge">{badge}</span>}
    </span>
  );
}

function ProviderList({
  officialActive,
  added,
  providerName,
  endpoint,
}: {
  officialActive: boolean;
  added: boolean;
  providerName: string;
  endpoint: string;
}) {
  const { t } = useTranslation();
  return (
    <div className="jc-cd-list">
      <article className={`jc-cd-card ${officialActive ? "is-current" : ""}`}>
        <Claude size={18} />
        <div className="jc-cd-card-text">
          <div className="jc-cd-card-name">{t("keys.desktopGuide.official")}</div>
          <div className="jc-cd-card-sub">{t("keys.desktopGuide.inAppLogin")}</div>
        </div>
        {officialActive && <span className="jc-cd-using">{t("keys.desktopGuide.inUse")}</span>}
      </article>
      {added && (
        <article className="jc-cd-card is-current">
          <Claude size={18} />
          <div className="jc-cd-card-text">
            <div className="jc-cd-card-name">{providerName}</div>
            <div className="jc-cd-card-sub">{endpoint || t("keys.shared.notConfigured")}</div>
          </div>
          <span className="jc-cd-using">{t("keys.desktopGuide.inUse")}</span>
        </article>
      )}
    </div>
  );
}

function AddForm({
  providerName,
  endpoint,
  apiKey,
  mode,
  onMode,
  onCancel,
  onAdd,
}: {
  providerName: string;
  endpoint: string;
  apiKey: string;
  mode: "direct" | "proxy";
  onMode: (mode: "direct" | "proxy") => void;
  onCancel: () => void;
  onAdd: () => void;
}) {
  const { t } = useTranslation();
  return (
    <form
      className="jc-cd-form"
      onSubmit={(event) => {
        event.preventDefault();
        if (mode === "direct") onAdd();
      }}
    >
      <div className="jc-cd-preset">{t("keys.desktopGuide.preset")}<span>{t("keys.desktopGuide.custom")}</span></div>
      <Field label={t("keys.desktopGuide.providerName")} value={providerName || deployment.siteName} />
      <Field label="API Key" value={apiKey || t("keys.desktopGuide.shownAfterCreate")} copyable={!!apiKey} highlighted />
      <Field label={t("keys.desktopGuide.baseUrl")} value={endpoint || t("keys.ccSwitch.endpointMissing")} copyable={!!endpoint} highlighted />
      <div className="jc-cd-field">
        <span>{t("keys.desktopGuide.mode")}</span>
        <div className="jc-cd-mode" role="group" aria-label={t("keys.desktopGuide.mode")}>
          <button type="button" aria-pressed={mode === "direct"} onClick={() => onMode("direct")}>{t("keys.desktopGuide.direct")}</button>
          <button type="button" aria-pressed={mode === "proxy"} onClick={() => onMode("proxy")}>{t("keys.desktopGuide.proxy")}</button>
        </div>
      </div>
      {mode === "proxy" && (
        <p className="jc-cd-warn">{t("keys.desktopGuide.proxyWarn")}</p>
      )}
      <p className="jc-cd-hint">{t("keys.desktopGuide.directHint")}</p>
      <div className="jc-cd-actions">
        <button type="button" className="jc-cd-ghost" onClick={onCancel}>{t("keys.shared.cancel")}</button>
        <button type="submit" className="jc-cd-submit" disabled={mode !== "direct"}>{t("keys.desktopGuide.add")}</button>
      </div>
    </form>
  );
}

function Field({
  label,
  value,
  copyable,
  highlighted,
}: {
  label: string;
  value: string;
  copyable?: boolean;
  highlighted?: boolean;
}) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const onCopy = async () => {
    if (!copyable) return;
    const ok = await copyText(value);
    if (!ok) return;
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className={`jc-cd-field ${highlighted ? "is-target" : ""}`}>
      <span>{label}</span>
      <div className="jc-cd-value-row">
        <code>{value}</code>
        {copyable && (
          <button type="button" onClick={onCopy} aria-label={t("keys.desktopGuide.copyField", { label })}>
            {copied ? <Check size={12} /> : <Copy size={12} />}
            {copied ? t("keys.shared.copied") : t("keys.shared.copy")}
          </button>
        )}
      </div>
    </div>
  );
}
