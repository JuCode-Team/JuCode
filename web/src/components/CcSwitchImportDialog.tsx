import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { AlertDialog, Box, Button, Code, Dialog, Flex, Link as ThemeLink, Select, Text } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import ClaudeCode from "@lobehub/icons/es/ClaudeCode/components/Color";
import Claude from "@lobehub/icons/es/Claude/components/Color";
import Codex from "@lobehub/icons/es/Codex/components/Color";
import OpenCode from "@lobehub/icons/es/OpenCode/components/Mono";
import Grok from "@lobehub/icons/es/Grok/components/Mono";
import { Check, Copy, Download, Monitor, Terminal } from "lucide-react";
import { useTranslation } from "react-i18next";
import { meApi } from "../api/endpoints";
import { publicApi } from "../api/public";
import { errMsg } from "../api/client";
import { copyText } from "../lib/copy";
import { ClaudeDesktopGuide } from "./ClaudeDesktopGuide";
import {
  buildCcSwitchLink,
  ccSwitchEndpointFor,
  CC_SWITCH_SITE,
  CODEX_DEFAULT_MODEL,
  GROK_DEFAULT_MODEL,
  OPENCODE_DEFAULT_MODEL,
  manualImport,
  resolveEndpoints,
  type CcSwitchApp,
} from "../lib/ccswitch";
import { deployment } from "../config/env";

// Which key the dialog is importing. The plaintext secret is either
// already in hand (right after creation) or has to be fetched by id.
export interface CcSwitchTarget {
  keyId?: string;
  keyName?: string;
  secret?: string | null;
}

// hint === null means the localized "manual setup" label.
const APPS: { id: CcSwitchApp; title: string; hint: string | null }[] = [
  { id: "claude", title: "Claude Code", hint: "Anthropic" },
  { id: "claude-desktop", title: "Claude Desktop", hint: null },
  { id: "codex", title: "Codex", hint: "Responses" },
  { id: "opencode", title: "OpenCode", hint: "Chat" },
  { id: "grokbuild", title: "Grok", hint: "Responses" },
  { id: "pi", title: "Pi", hint: "Responses" },
];

function modelOptions(names: string[], fallback: string): string[] {
  return names.includes(fallback) ? names : [fallback, ...names];
}

const ICON = 26;

function PiMark({ size }: { size: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 800 800" aria-hidden>
      <path fill="currentColor" fillRule="evenodd" d="M165.29 165.29H517.36V400H400V517.36H282.65V634.72H165.29ZM282.65 282.65V400H400V282.65Z" />
      <path fill="currentColor" d="M517.36 400H634.72V634.72H517.36Z" />
    </svg>
  );
}

function AgentMark({ id }: { id: CcSwitchApp }) {
  const badge = id === "claude"
    ? <Terminal size={9} strokeWidth={2.6} />
    : id === "claude-desktop"
      ? <Monitor size={9} strokeWidth={2.6} />
      : null;
  return (
    <span className="jc-cc-mark" aria-hidden>
      {id === "claude" && <ClaudeCode size={ICON} />}
      {id === "claude-desktop" && <Claude size={ICON} />}
      {id === "codex" && <Codex size={ICON} />}
      {id === "opencode" && <OpenCode size={ICON} />}
      {id === "grokbuild" && <Grok size={ICON} />}
      {id === "pi" && <PiMark size={ICON} />}
      {badge && <span className="jc-cc-badge">{badge}</span>}
    </span>
  );
}

export default function CcSwitchImportDialog({
  target,
  onClose,
}: {
  target: CcSwitchTarget | null;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const open = !!target;
  const { data: site } = useQuery({
    queryKey: ["public", "site-info"],
    queryFn: publicApi.siteInfo,
    enabled: open,
  });
  const { data: models } = useQuery({
    queryKey: ["public", "models"],
    queryFn: publicApi.models,
    enabled: open,
  });

  const needsReveal = open && !target?.secret && !!target?.keyId;
  const revealQuery = useQuery({
    queryKey: ["apikey-reveal", target?.keyId],
    queryFn: () => meApi.revealApiKey(target!.keyId!),
    enabled: needsReveal,
    gcTime: 0,
    staleTime: 0,
  });

  const secret = target?.secret ?? revealQuery.data ?? "";
  const ep = resolveEndpoints(site);
  const siteName = site?.site_name?.trim() || deployment.siteName;
  const providerName = target?.keyName?.trim()
    ? `${siteName} · ${target.keyName.trim()}`
    : siteName;

  const modelNames = useMemo(() => (models ?? []).map((m) => m.name), [models]);
  const codexOptions = useMemo(() => modelOptions(modelNames, CODEX_DEFAULT_MODEL), [modelNames]);
  const grokOptions = useMemo(() => modelOptions(modelNames, GROK_DEFAULT_MODEL), [modelNames]);
  const opencodeOptions = useMemo(() => modelOptions(modelNames, OPENCODE_DEFAULT_MODEL), [modelNames]);

  const [app, setApp] = useState<CcSwitchApp>("claude");
  const [codexModel, setCodexModel] = useState(CODEX_DEFAULT_MODEL);
  const [grokModel, setGrokModel] = useState(GROK_DEFAULT_MODEL);
  const [opencodeModel, setOpencodeModel] = useState(OPENCODE_DEFAULT_MODEL);
  useEffect(() => {
    if (!open) {
      setApp("claude");
      setCodexModel(CODEX_DEFAULT_MODEL);
      setGrokModel(GROK_DEFAULT_MODEL);
      setOpencodeModel(OPENCODE_DEFAULT_MODEL);
    }
  }, [open]);

  const model = app === "codex" ? codexModel : app === "grokbuild" ? grokModel : app === "opencode" ? opencodeModel : undefined;
  const endpoint = ccSwitchEndpointFor(app, ep);
  const manual = manualImport(app, { endpoint, apiKey: secret, model, siteName });
  const link = !manual.deepLink || !endpoint || !secret
    ? ""
    : buildCcSwitchLink({
        app,
        name: providerName,
        endpoint,
        apiKey: secret,
        homepage: site?.site_url || undefined,
        model: model || undefined,
      });

  const revealError = revealQuery.isError ? errMsg(revealQuery.error as Error) : "";
  const loadingSecret = needsReveal && revealQuery.isPending;

  return (
    <Dialog.Root open={open} onOpenChange={(o) => { if (!o) onClose(); }}>
      <Dialog.Content className="jc-cc-dialog" style={{ maxWidth: app === "claude-desktop" ? 720 : 640 }}>
        <Dialog.Title>{t("keys.ccSwitch.title")}</Dialog.Title>
        <Dialog.Description size="2" color="gray">
          {app === "claude-desktop"
            ? t("keys.ccSwitch.descDesktop")
            : t("keys.ccSwitch.desc")}
        </Dialog.Description>

        {loadingSecret && <Text size="2" color="gray" as="p" mt="4">{t("keys.ccSwitch.readingKey")}</Text>}
        {revealError && <Text size="2" color="red" as="p" mt="4">{t("keys.ccSwitch.readKeyFailed", { error: revealError })}</Text>}

        {!loadingSecret && !revealError && (
          <div className="jc-cc-body">
            <div className="jc-cc-grid" role="listbox" aria-label={t("keys.ccSwitch.clients")}>
              {APPS.map((item) => (
                <button
                  key={item.id}
                  type="button"
                  role="option"
                  aria-selected={app === item.id}
                  className={`jc-cc-app ${app === item.id ? "is-active" : ""}`}
                  onClick={() => setApp(item.id)}
                >
                  <AgentMark id={item.id} />
                  <span className="jc-cc-app-text">
                    <span className="jc-cc-app-title">{item.title}</span>
                    <span className="jc-cc-app-hint">{item.hint ?? t("keys.ccSwitch.manualGuide")}</span>
                  </span>
                </button>
              ))}
            </div>

            <div className="jc-cc-detail">
              {app === "claude-desktop" ? (
                <ClaudeDesktopGuide
                  providerName={providerName}
                  endpoint={endpoint}
                  apiKey={secret}
                />
              ) : (
                <>
                  <Text size="1" color="gray" style={{ wordBreak: "break-all" }}>
                    {endpoint || t("keys.ccSwitch.endpointMissing")}
                  </Text>
                  {app === "codex" && (
                    <ModelField label={t("keys.ccSwitch.defaultModel")} value={codexModel} options={codexOptions} onChange={setCodexModel} />
                  )}
                  {app === "grokbuild" && (
                    <ModelField label={t("keys.ccSwitch.defaultModel")} value={grokModel} options={grokOptions} onChange={setGrokModel} />
                  )}
                  {app === "opencode" && (
                    <ModelField label={t("keys.ccSwitch.defaultModel")} value={opencodeModel} options={opencodeOptions} onChange={setOpencodeModel} />
                  )}
                  <Text size="1" color="gray" as="p">{manual.note}</Text>
                  <Snippet filename={manual.filename} body={manual.body} />
                  <Flex gap="2" justify="end" wrap="wrap">
                    {manual.deepLink ? (
                      <>
                        <CopyLinkButton link={link} />
                        <Button disabled={!link} onClick={() => { window.location.href = link; }}>
                          <Download size={14} /> {t("keys.ccSwitch.import")}
                        </Button>
                      </>
                    ) : (
                      <Button asChild>
                        <a href={CC_SWITCH_SITE} target="_blank" rel="noreferrer">{t("keys.ccSwitch.install")}</a>
                      </Button>
                    )}
                  </Flex>
                </>
              )}
            </div>
          </div>
        )}

        <Flex justify="between" align="center" mt="4" gap="3" wrap="wrap">
          <Text size="1" color="gray">
            {t("keys.ccSwitch.noCcSwitch")}{" "}
            <ThemeLink href={CC_SWITCH_SITE} target="_blank" rel="noreferrer" underline="always">{t("keys.ccSwitch.website")}</ThemeLink>
          </Text>
          <Dialog.Close><Button variant="soft" color="gray">{t("keys.shared.close")}</Button></Dialog.Close>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}

function ModelField({
  label, value, options, onChange,
}: {
  label: string;
  value: string;
  options: string[];
  onChange: (value: string) => void;
}) {
  return (
    <Flex align="center" gap="2">
      <Text size="1" color="gray">{label}</Text>
      <Select.Root value={value} onValueChange={onChange} size="1">
        <Select.Trigger />
        <Select.Content>
          {options.map((n) => (
            <Select.Item key={n} value={n}>{n}</Select.Item>
          ))}
        </Select.Content>
      </Select.Root>
    </Flex>
  );
}

function Snippet({ filename, body }: { filename: string; body: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [copyErr, setCopyErr] = useState(false);
  useEffect(() => {
    setCopied(false);
    setCopyErr(false);
  }, [body]);
  const onCopy = async () => {
    const ok = await copyText(body);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } else {
      setCopyErr(true);
      window.setTimeout(() => setCopyErr(false), 1800);
    }
  };
  return (
    <div className="jc-cc-snippet">
      <div className="jc-cc-snippet-head">
        <span>{filename}</span>
        <button type="button" className="jc-cc-snippet-copy" onClick={onCopy}>
          {copied ? <Check size={12} /> : <Copy size={12} />}
          {copyErr ? t("keys.shared.copyFailed") : copied ? t("keys.shared.copied") : t("keys.ccSwitch.copyConfig")}
        </button>
      </div>
      <pre>{body}</pre>
    </div>
  );
}

function CopyLinkButton({ link }: { link: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [copyErr, setCopyErr] = useState(false);
  const onCopy = async () => {
    const ok = await copyText(link);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } else {
      setCopyErr(true);
      window.setTimeout(() => setCopyErr(false), 1800);
    }
  };
  return (
    <Button size="2" variant="soft" color={copyErr ? "red" : copied ? "green" : "gray"} disabled={!link} onClick={onCopy}>
      {copied ? <Check size={14} /> : <Copy size={14} />}
      {copyErr ? t("keys.shared.copyFailed") : copied ? t("keys.shared.copied") : t("keys.ccSwitch.copyLink")}
    </Button>
  );
}

// Shown right after a key is created. Teaches the two ways to put it
// into a client: install CC Switch, or paste the manual config.
export function CreatedKeyGuide({
  secret,
  onImport,
  title,
  description,
}: {
  secret: string;
  onImport: () => void;
  title?: string;
  description?: string;
}) {
  const { t } = useTranslation();
  const { data: site } = useQuery({ queryKey: ["public", "site-info"], queryFn: publicApi.siteInfo });
  const ep = resolveEndpoints(site);
  const [copied, setCopied] = useState(false);

  const onCopy = async () => {
    const ok = await copyText(secret);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    }
  };

  return (
    <AlertDialog.Content className="jc-key-guide" style={{ maxWidth: 640 }}>
      <AlertDialog.Title>{title ?? t("keys.apiKeys.createdTitle")}</AlertDialog.Title>
      <AlertDialog.Description size="2" color="gray">
        {description ?? t("keys.apiKeys.createdDesc")}
      </AlertDialog.Description>

      <Flex align="center" gap="2" my="3">
        <Box className="jc-key-guide-secret">
          <Code style={{ wordBreak: "break-all" }}>{secret}</Code>
        </Box>
        <Button size="1" variant="soft" color={copied ? "green" : "gray"} onClick={onCopy}>
          {copied ? <Check size={12} /> : <Copy size={12} />}
          {copied ? t("keys.shared.copied") : t("keys.shared.copy")}
        </Button>
      </Flex>

      <div className="jc-key-steps">
        <section className="jc-key-step">
          <div className="jc-key-step-title">{t("keys.ccSwitch.guide.step1Title")}</div>
          <Text size="2" color="gray" as="p">
            {t("keys.ccSwitch.guide.step1Body")}
          </Text>
          <Flex gap="2" mt="2" wrap="wrap">
            <Button variant="soft" asChild>
              <a href={CC_SWITCH_SITE} target="_blank" rel="noreferrer">{t("keys.ccSwitch.guide.openSite")}</a>
            </Button>
            <AlertDialog.Cancel>
              <Button onClick={onImport}><Download size={14} /> {t("keys.ccSwitch.guide.importWith")}</Button>
            </AlertDialog.Cancel>
          </Flex>
        </section>
        <section className="jc-key-step">
          <div className="jc-key-step-title">{t("keys.ccSwitch.guide.step2Title")}</div>
          <Text size="2" color="gray" as="p">
            {t("keys.ccSwitch.guide.step2Body")}
          </Text>
          <dl className="jc-key-endpoints">
            <div>
              <dt>Claude Code / Desktop</dt>
              <dd>{ep.anthropic || t("keys.shared.notConfigured")}</dd>
            </div>
            <div>
              <dt>Codex / OpenCode / Grok / Pi</dt>
              <dd>{ep.openaiResponses || t("keys.shared.notConfigured")}</dd>
            </div>
          </dl>
          <Button variant="soft" mt="2" asChild>
            <Link to="/endpoints">{t("keys.ccSwitch.guide.viewEndpoints")}</Link>
          </Button>
        </section>
      </div>

      <Flex justify="end" mt="4">
        <AlertDialog.Action><Button variant="soft" color="gray">{t("keys.ccSwitch.guide.saved")}</Button></AlertDialog.Action>
      </Flex>
    </AlertDialog.Content>
  );
}
