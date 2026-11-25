import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Trans, useTranslation } from "react-i18next";
import { BookOpen, Check, Copy, KeyRound, Terminal } from "lucide-react";
import { publicApi } from "../api/public";
import { copyText } from "../lib/copy";
import { apiKeyEnv, providerId, resolveEndpoints } from "../lib/ccswitch";
import { deployment } from "../config/env";
import PremiumCard from "../components/PremiumCard";
import { docsUrl } from "../lib/siblingSite";

type EndpointRow = {
  key: string;
  label: string;
  value: string;
  hint: string;
};
type EndpointGroup = {
  key: string;
  title: string;
  accent: "purple" | "amber";
  rows: EndpointRow[];
};

const TABS = [
  { id: "codex", label: "Codex CLI", short: "codex" },
  { id: "claude-code", label: "Claude Code", short: "claude" },
  { id: "grok-build", label: "Grok Build", short: "grok" },
  { id: "openai", label: "openai (Python)", short: "openai" },
] as const;

type TabId = (typeof TABS)[number]["id"];

const DOCS_URL = docsUrl();
const DOCS_HOST = DOCS_URL ? new URL(DOCS_URL).host : "";

export default function Endpoints() {
  const { t } = useTranslation();
  const { data } = useQuery({ queryKey: ["public", "site-info"], queryFn: publicApi.siteInfo });
  const ep = resolveEndpoints(data);
  const siteName = data?.site_name?.trim() || deployment.siteName;
  const id = providerId(siteName);
  const keyEnv = apiKeyEnv(siteName);
  const [tab, setTab] = useState<TabId>("codex");
  const notSet = t("pub.endpoints.notSet");

  const groups: EndpointGroup[] = useMemo(
    () => [
      {
        key: "openai",
        title: "OpenAI",
        accent: "purple",
        rows: [
          {
            key: "openai-responses",
            label: "Responses",
            value: ep.openaiResponses,
            hint: t("pub.endpoints.hintResponses"),
          },
          {
            key: "openai-compat",
            label: "Chat Completions",
            value: ep.openaiCompat,
            hint: t("pub.endpoints.hintChat"),
          },
          {
            key: "grok-build",
            label: "Grok Build",
            value: ep.openaiResponses,
            hint: t("pub.endpoints.hintGrok"),
          },
        ],
      },
      {
        key: "anthropic",
        title: "Anthropic",
        accent: "amber",
        rows: [
          {
            key: "anthropic-messages",
            label: "Messages",
            value: ep.anthropic,
            hint: t("pub.endpoints.hintMessages"),
          },
        ],
      },
    ],
    [ep.openaiResponses, ep.openaiCompat, ep.anthropic, t],
  );

  return (
    <div className="ep-shell">
      <div className="ep-container">
        <header className="ep-hero">
          <span className="trace-page-kicker">{t("pub.endpoints.kicker")}</span>
          <h1 className="ep-title">{t("pub.endpoints.title")}</h1>
          <p className="ep-sub">
            <Trans i18nKey="pub.endpoints.lead" components={{ c: <code className="ep-inline" /> }} />
            <Link to="/me/apikeys" className="ep-link"> {t("pub.endpoints.toKeys")}</Link>
          </p>
        </header>

        <section className="ep-grid">
          {groups.map((g) => (
            <EndpointGroupCard key={g.key} group={g} />
          ))}
        </section>

        <section className="ep-snippet">
          <div className="ep-snippet-head">
            <div className="ep-snippet-title">
              <Terminal size={16} /> {t("pub.endpoints.examples")}
            </div>
            <div className="ep-tabbar" role="tablist">
              {TABS.map((item) => (
                <button
                  key={item.id}
                  type="button"
                  role="tab"
                  aria-selected={item.id === tab}
                  className={`ep-tab ${item.id === tab ? "is-active" : ""}`}
                  onClick={() => setTab(item.id)}
                >
                  {item.label}
                </button>
              ))}
            </div>
          </div>

          <SnippetTerminal>
            {tab === "codex" && (
              <CodeBlock
                title="~/.codex/config.toml"
                lang="toml"
                code={`model_provider = "${id}"
model = "gpt-5.5"

[model_providers.${id}]
name = "${id}"
base_url = "${ep.openaiResponses || notSet}"
wire_api = "responses"
experimental_bearer_token = "key..."`}
                footer={t("pub.endpoints.codexFooter", { site: siteName })}
              />
            )}
            {tab === "openai" && (
              <CodeBlock
                title="openai_demo.py"
                lang="python"
                code={`from openai import OpenAI

client = OpenAI(
    base_url="${ep.openaiCompat || notSet}",
    api_key="sk-juc-...",
)

resp = client.chat.completions.create(
    model="gpt-5.3-codex",
    messages=[{"role": "user", "content": "Hello"}],
)
print(resp.choices[0].message.content)`}
              />
            )}
            {tab === "claude-code" && (
              <CodeBlock
                title="shell"
                lang="bash"
                code={`# ${t("pub.endpoints.claudeComment1", { site: siteName })}
export ANTHROPIC_BASE_URL=${ep.anthropic || notSet}
export ANTHROPIC_API_KEY=sk-juc-...

# ${t("pub.endpoints.claudeComment2")}`}
              />
            )}
            {tab === "grok-build" && (
              <CodeBlock
                title="~/.grok/config.toml"
                lang="toml"
                code={`[model.${id}]
model = "grok-4.7"
base_url = "${ep.openaiResponses || notSet}"
name = "${siteName}"
api_backend = "responses"
env_key = "${keyEnv}"

[model.grok-4.7]
base_url = "${ep.openaiResponses || notSet}"
api_backend = "responses"
env_key = "${keyEnv}"

[model.grok-4.6]
base_url = "${ep.openaiResponses || notSet}"
api_backend = "responses"
env_key = "${keyEnv}"

[models]
default = "${id}"`}
                footer={`# export ${keyEnv}=sk-juc-...
# grok -m ${id} -p "Hello"
# ${t("pub.endpoints.grokComment")}`}
              />
            )}
          </SnippetTerminal>
        </section>

        <section className="ep-tips">
          <div className="ep-tip-card">
            <div className="ep-tip-icon"><KeyRound size={14} /></div>
            <div>
              <div className="ep-tip-title">API Key</div>
              <div className="ep-tip-body">
                {t("pub.endpoints.keyTip")}
                <Link to="/me/apikeys" className="ep-link">{t("pub.endpoints.manage")}</Link>
              </div>
            </div>
          </div>
          {DOCS_URL && <div className="ep-tip-card">
            <div className="ep-tip-icon"><BookOpen size={14} /></div>
            <div>
              <div className="ep-tip-title">{t("pub.endpoints.docsTitle")}</div>
              <div className="ep-tip-body">
                {t("pub.endpoints.docsTip")}
                <a href={DOCS_URL} target="_blank" rel="noreferrer" className="ep-link">{DOCS_HOST} →</a>
              </div>
            </div>
          </div>}
        </section>
      </div>
    </div>
  );
}

function EndpointGroupCard({ group }: { group: EndpointGroup }) {
  return (
    <PremiumCard interactive={false} className={`ep-card ep-card-${group.accent}`}>
      <div className="ep-card-title">{group.title}</div>
      <div className="ep-card-rows">
        {group.rows.map((r) => (
          <EndpointRowItem key={r.key} row={r} />
        ))}
      </div>
    </PremiumCard>
  );
}

function EndpointRowItem({ row }: { row: EndpointRow }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState(false);
  const onCopy = async () => {
    if (!row.value) return;
    const ok = await copyText(row.value);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } else {
      setError(true);
      window.setTimeout(() => setError(false), 1800);
    }
  };
  return (
    <div className="ep-row">
      <div className="ep-row-label">{row.label}</div>
      <button type="button" className="ep-card-url" onClick={onCopy} title={t("pub.endpoints.clickToCopy")}>
        <code>{row.value || <span className="ep-card-url-empty">{t("pub.endpoints.notConfigured")}</span>}</code>
        <span className={`ep-card-copy ${copied ? "is-copied" : ""} ${error ? "is-error" : ""}`}>
          {copied ? <Check size={14} /> : <Copy size={14} />}
          <span>{error ? t("pub.common.copyFailed") : copied ? t("pub.common.copied") : t("pub.common.copy")}</span>
        </span>
      </button>
      <div className="ep-card-hint">{row.hint}</div>
    </div>
  );
}

function SnippetTerminal({ children }: { children: React.ReactNode }) {
  return (
    <div className="ep-terminal">
      <div className="ep-terminal-body">{children}</div>
    </div>
  );
}

function CodeBlock({
  title, lang, code, footer,
}: { title: string; lang: string; code: string; footer?: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState(false);
  const onCopy = async () => {
    const full = footer ? `${code}\n\n${footer}` : code;
    const ok = await copyText(full);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } else {
      setError(true);
      window.setTimeout(() => setError(false), 1800);
    }
  };
  // Re-render with fresh state on tab change.
  useEffect(() => {
    setCopied(false);
    setError(false);
  }, [code]);
  return (
    <div className="ep-codeblock">
      <div className="ep-codeblock-head">
        <span className="ep-codeblock-file">
          <span className="ep-codeblock-lang">{lang}</span>
          <span className="ep-codeblock-title">{title}</span>
        </span>
        <button type="button" className={`ep-codeblock-copy ${copied ? "is-copied" : ""} ${error ? "is-error" : ""}`} onClick={onCopy}>
          {copied ? <Check size={13} /> : <Copy size={13} />}
          <span>{error ? t("pub.common.copyFailed") : copied ? t("pub.common.copied") : t("pub.common.copy")}</span>
        </button>
      </div>
      <pre className="ep-codeblock-pre">
        <code>{code}</code>
        {footer && (
          <>
            {"\n\n"}
            <span style={{ color: "var(--c-on-surface-variant)" }}>{footer}</span>
          </>
        )}
      </pre>
    </div>
  );
}
