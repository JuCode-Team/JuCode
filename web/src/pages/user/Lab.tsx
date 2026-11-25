// react-hooks/purity flags Date.now()/performance.now() in closures the lab
// calls from event handlers and effects; nothing here reads the clock while
// rendering.
/* eslint-disable react-hooks/purity */
import "../../personal-responsive.css";
import { useEffect, useMemo, useRef, useState, type ChangeEvent, type KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import {
  Box, Button, Flex, IconButton, SegmentedControl, Select, Text, Tooltip,
} from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import type { Components } from "react-markdown";
import {
  ArrowUp, Check, ChevronDown, ChevronRight, Copy, Download, GitBranch, Globe, Image as ImageIcon, ImagePlus, Paperclip, Pencil, Plus, RotateCcw, Sparkles, Square, Telescope, Trash2, X,
} from "lucide-react";
import { meApi } from "../../api/endpoints";
import { apiBaseURL, errMsg } from "../../api/client";
import { publicApi, type PublicModel, type PublicModelGroup } from "../../api/public";
import { labWebToolsApi } from "../../api/webTools";
import i18n, { localeTag } from "../../i18n";
import { copyText } from "../../lib/copy";
import {
  chatTitle, deleteChat, forkTitle, loadChatCache, loadImageCache, loadLabPrefs, loadLabTab, saveChatCache, saveImageCache, saveLabPrefs, saveLabTab, upsertChat,
  type ImageSession, type LabPrefs, type LabTab, type StoredChat, type StoredImageResult, type StoredToolCall,
} from "../../lib/labCache";
import {
  appendToolTurn, effortOptions, foldAnthropicBlocks, foldLabTool, GENERATE_IMAGE_TOOL, imageEditLimit, imageQualityOptions, imageReferenceLimit, imageSizeError, imageSizePolicy, imageSupportsFidelity, imageToolModel, labConsolePath, labHeaders, labRequestBody, labSurface, labToolDefs, labToolDetail, readLabDelta, readLabTools, supportsImageTool, supportsWebSearch,
  type AnthropicBlock, type LabToolAcc, type ToolRunResult,
} from "../../lib/labProtocol";
import { RESEARCH_TOOL_ROUNDS, runWebTool, TOOL_ROUNDS, webToolPrompt } from "../../lib/labWebTools";
import { assetBlob, blobFromImagePayload, blobToDataURL, deleteAssets, isInlineImage, saveAsset } from "../../lib/labAssets";
import { candidateGroups, groupsForModel, LAB_AUTO_GROUP, labBillingSource, modelsForGroups, reconcileLabPick, type LabBillingSource } from "../../lib/labPick";
import { useOrgContextStore, useOrgScope } from "../../store/orgContext";
import { useAuthStore } from "../../store/auth";
import { MaskPainter, type MaskPainterHandle } from "../../components/MaskPainter";
import ModelBrandIcon from "../../components/ModelBrandIcon";
import { RouteSkeleton } from "../../components/ConsoleSkeleton";
import { PageHeader } from "../../components/PageHeader";
import { LabMarkdown } from "./lab/LabMarkdown";
import { ModelPicker, PlanOnlySwitch } from "./lab/ModelPicker";
import { ToolSteps } from "./lab/ToolSteps";
import { WebToolsLab } from "./lab/WebToolsLab";

// 实验室 — in-console playground. Calls go through the signed-in session.
// The 仅套餐 switch picks the billing source (balance groups by default, plan
// groups when on) and every call sends it, so automatic routing stays inside
// that source. Web tools bill the balance per call. The lab key itself is not
// a client credential.

type Effort = string;

export interface ChatMsg {
  role: "user" | "assistant";
  content: string;
  images?: string[]; // asset ids; legacy data URLs are migrated on load
  reasoning?: string; // streamed reasoning_content
  model?: string; // assistant: which model produced it
  error?: boolean;
  tools?: StoredToolCall[];
}

interface ChatUsage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
}

const AUTO_GROUP = LAB_AUTO_GROUP;
const MAX_IMAGES = 4;
const MAX_IMAGE_BYTES = 5 * 1024 * 1024;

const SUGGESTIONS = ["intro", "caching", "poem"] as const;

function toApiMessages(msgs: ChatMsg[]) {
  return msgs.map((m) => {
    if (m.role === "user" && (m.images?.length ?? 0) > 0) {
      return {
        role: m.role,
        content: [
          ...(m.content ? [{ type: "text", text: m.content }] : []),
          ...(m.images ?? []).map((url) => ({ type: "image_url", image_url: { url } })),
        ],
      };
    }
    return { role: m.role, content: m.content };
  });
}

async function dataURLToBlob(url: string): Promise<Blob> {
  const res = await fetch(url);
  return res.blob();
}

async function imageSourceBlob(src: string): Promise<Blob> {
  if (!isInlineImage(src)) {
    const blob = await assetBlob(src);
    if (!blob) throw new Error(i18n.t("bench.console.imageNotFound"));
    return blob;
  }
  return dataURLToBlob(src);
}

async function resolveImageUrls(ids: string[]): Promise<string[]> {
  const urls: string[] = [];
  for (const id of ids) {
    if (isInlineImage(id)) {
      urls.push(id);
      continue;
    }
    const blob = await assetBlob(id);
    if (blob) urls.push(await blobToDataURL(blob));
  }
  return urls;
}

function labRequestHeaders(group: string, billingSource: LabBillingSource, json = true): Record<string, string> {
  return labHeaders({
    token: useAuthStore.getState().tokens?.access_token,
    orgId: useOrgContextStore.getState().activeOrgId ?? undefined,
    group,
    billingSource,
    json,
  });
}

async function drawWithImagesAPI(opts: {
  baseURL: string;
  groupId: string;
  billingSource: LabBillingSource;
  model: string;
  prompt: string;
  sources: string[];
  signal: AbortSignal;
}): Promise<string[]> {
  const headers = labRequestHeaders(opts.groupId, opts.billingSource, false);
  const gptImage = /^gpt-image-/i.test(opts.model);
  const size = gptImage ? "auto" : "1024x1024";
  let res: Response;
  if (opts.sources.length > 0) {
    const body = new FormData();
    body.append("model", opts.model);
    body.append("prompt", opts.prompt || "edit this image");
    body.append("n", "1");
    body.append("size", size);
    if (!gptImage) body.append("response_format", "b64_json");
    const imageField = opts.sources.length > 1 ? "image[]" : "image";
    for (const [i, src] of opts.sources.entries()) {
      const blob = await imageSourceBlob(src);
      body.append(imageField, blob, `image-${i}.png`);
    }
    res = await fetch(`${opts.baseURL}/v1/me/lab/images/edits`, { method: "POST", headers, body, signal: opts.signal });
  } else {
    res = await fetch(`${opts.baseURL}/v1/me/lab/images/generations`, {
      method: "POST",
      headers: { ...headers, "Content-Type": "application/json" },
      signal: opts.signal,
      body: JSON.stringify({
        model: opts.model,
        prompt: opts.prompt,
        n: 1,
        size,
        ...(gptImage ? {} : { response_format: "b64_json" }),
      }),
    });
  }
  if (!res.ok) throw new Error(await readGatewayError(res));
  const j = await res.json();
  return ((j.data ?? []) as Array<{ b64_json?: string; url?: string }>)
    .map((d) => (d.b64_json ? `data:image/png;base64,${d.b64_json}` : d.url))
    .filter((s): s is string => !!s);
}

function pinnedAssetIds(userId: string): string[] {
  const sessions = loadImageCache(localStorage, userId).sessions.flatMap((item) => [
    ...item.sources,
    ...(item.editTarget ? [item.editTarget] : []),
    ...item.results.flatMap((result) => [...result.images, ...(result.sources ?? [])]),
  ]);
  const chats = loadChatCache(localStorage, userId).chats.flatMap((chat) => chat.messages.flatMap((message) => message.images ?? []));
  return [...sessions, ...chats].filter((id) => !isInlineImage(id));
}

async function materializeImage(userId: string, value: string, prompt: string, model: string, keepIds: readonly string[] = []): Promise<string> {
  if (!isInlineImage(value)) return value;
  const blob = await blobFromImagePayload(value);
  const saved = await saveAsset({ userId, blob, prompt, model, keepIds });
  return saved.id;
}

function StoredImage({ id, fit = "thumb", alt = "" }: { id: string; fit?: "thumb" | "natural"; alt?: string }) {
  const { t } = useTranslation();
  const [url, setUrl] = useState(isInlineImage(id) ? id : "");
  const [open, setOpen] = useState(false);
  const dialogRef = useRef<HTMLDivElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (isInlineImage(id) || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id)) {
      setUrl(id);
      return;
    }
    let dead = false;
    let objectUrl = "";
    assetBlob(id).then((blob) => {
      if (!blob || dead) return;
      objectUrl = URL.createObjectURL(blob);
      setUrl(objectUrl);
    }).catch(() => {});
    return () => {
      dead = true;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [id]);
  useEffect(() => {
    if (!open) return;
    const previouslyFocused = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    closeRef.current?.focus();
    const onKey = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        setOpen(false);
        return;
      }
      if (event.key !== "Tab") return;
      const root = dialogRef.current;
      if (!root) return;
      const focusable = [...root.querySelectorAll<HTMLElement>("button, [href], input, select, textarea, [tabindex]:not([tabindex='-1'])")];
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      const active = document.activeElement;
      if (event.shiftKey && (active === first || !root.contains(active))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (active === last || !root.contains(active))) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
      previouslyFocused?.focus();
    };
  }, [open]);
  if (!url) return null;
  return (
    <>
      <button type="button" className={fit === "natural" ? "jc-lab-msg-img-btn is-natural" : "jc-lab-msg-img-btn"} aria-label={alt ? t("bench.console.viewLarge", { alt }) : t("bench.console.viewLargeBare")} onClick={() => setOpen(true)}>
        <img src={url} alt={alt} className={fit === "natural" ? "jc-lab-msg-img is-natural" : "jc-lab-msg-img"} />
      </button>
      {open && createPortal(
        <div ref={dialogRef} className="jc-lab-lightbox" role="dialog" aria-modal="true" aria-label={t("bench.console.imagePreview")} onClick={() => setOpen(false)}>
          <button ref={closeRef} type="button" className="jc-lab-lightbox-close" aria-label={t("bench.console.close")} onClick={() => setOpen(false)}>
            <X size={16} />
          </button>
          <img src={url} alt={alt} onClick={(event) => event.stopPropagation()} />
        </div>,
        document.body,
      )}
    </>
  );
}

export async function readGatewayError(res: Response): Promise<string> {
  const text = await res.text().catch(() => "");
  try {
    const j = JSON.parse(text);
    return j?.error?.message || j?.error || text || `HTTP ${res.status}`;
  } catch {
    return text || `HTTP ${res.status}`;
  }
}

/* ============ shared config selectors ============ */

function useLabPick(
  models: PublicModel[],
  groups: PublicModelGroup[],
  model: string,
  setModel: (value: string) => void,
  groupId: string,
  setGroupId: (value: string) => void,
) {
  const view = useMemo(() => reconcileLabPick(models, groups, groupId, model), [models, groups, groupId, model]);
  const modelOptions = useMemo(() => modelsForGroups(models, groups), [models, groups]);
  return {
    note: view.note,
    modelOptions,
    activeModel: view.model,
    activeGroupId: view.groupId,
    pickModel: (next: string) => {
      setModel(next);
      const carried = groupsForModel(models.find((item) => item.name === next), groups);
      if (groupId !== AUTO_GROUP && !carried.some((group) => group.id === groupId)) setGroupId(AUTO_GROUP);
    },
    pickGroup: (next: string) => setGroupId(next),
  };
}

/** The group to pin for a model: the chosen one when it carries the model, otherwise automatic. */
function groupFor(models: PublicModel[], groups: PublicModelGroup[], model: string, groupId: string): string {
  if (groupId === AUTO_GROUP) return AUTO_GROUP;
  const carried = groupsForModel(models.find((item) => item.name === model), groups);
  return carried.some((group) => group.id === groupId) ? groupId : AUTO_GROUP;
}

/* ============ page ============ */

interface LabPreview {
  userId: string;
  models: PublicModel[];
  planName?: string;
}

export default function Lab({ preview }: { preview?: LabPreview } = {}) {
  const { t } = useTranslation();
  const user = useAuthStore((s) => s.user);
  const userId = preview?.userId || user?.id || "local";
  const [tab, setTab] = useState<LabTab>(() => loadLabTab(localStorage, userId));
  useEffect(() => { saveLabTab(localStorage, userId, tab); }, [userId, tab]);
  const [prefs, setPrefsState] = useState<LabPrefs>(() => loadLabPrefs(localStorage, userId));
  const setPrefs = (patch: Partial<LabPrefs>) => setPrefsState((cur) => {
    const next = { ...cur, ...patch };
    saveLabPrefs(localStorage, userId, next);
    return next;
  });

  const scope = useOrgScope();
  const walletLabel = scope === "personal" ? t("bench.console.wallet") : t("bench.console.orgQuota");
  const { data: usage, isLoading: usageLoading } = useQuery({
    queryKey: ["me-usage", scope],
    queryFn: meApi.usage,
    enabled: !preview,
  });
  const { data: accessibleGroups, isLoading: groupsLoading } = useQuery({
    queryKey: ["available-groups", scope],
    queryFn: meApi.availableGroups,
    enabled: !preview,
  });
  const { data: liveModels = [], isLoading: modelsLoading } = useQuery({
    queryKey: ["public-models"],
    queryFn: publicApi.models,
    enabled: !preview,
  });
  const models = preview?.models ?? liveModels;
  const planName = preview ? preview.planName : (usage?.has_active_plan ? usage.plan_name : undefined);
  const hasPlan = !!planName;
  const billingSource = labBillingSource(prefs.planOnly && hasPlan);

  const groups = useMemo(() => {
    const accessible = preview ? null : new Set((accessibleGroups ?? []).map((group) => group.id));
    return candidateGroups(models, billingSource, accessible);
  }, [models, billingSource, accessibleGroups, preview]);

  if (!preview && (modelsLoading || usageLoading || groupsLoading)) {
    return <RouteSkeleton />;
  }

  const tabProps: LabTabProps = {
    models, groups, userId, hasPlan, walletLabel, billingSource, prefs, setPrefs,
  };

  return (
    <Box className="jc-lab-page">
      <PageHeader
        title={t("bench.console.title")}
        description={t("bench.console.description", { wallet: walletLabel })}
        actions={
          <SegmentedControl.Root value={tab} onValueChange={(v) => setTab(v as LabTab)} size="2">
            <SegmentedControl.Item value="chat">{t("bench.console.tabs.chat")}</SegmentedControl.Item>
            <SegmentedControl.Item value="image">{t("bench.console.tabs.image")}</SegmentedControl.Item>
            <SegmentedControl.Item value="tools">{t("bench.console.tabs.tools")}</SegmentedControl.Item>
          </SegmentedControl.Root>
        }
      />

      {tab === "chat" ? <ChatLab {...tabProps} /> : tab === "image" ? <ImageLab {...tabProps} /> : <WebToolsLab enabled={!preview} />}
    </Box>
  );
}

interface LabTabProps {
  models: PublicModel[];
  groups: PublicModelGroup[];
  userId: string;
  hasPlan: boolean;
  walletLabel: string;
  billingSource: LabBillingSource;
  prefs: LabPrefs;
  setPrefs: (patch: Partial<LabPrefs>) => void;
}

/* ============ 对话（chatbox） ============ */

function ReasoningBlock({ text, streaming }: { text: string; streaming: boolean }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(true);
  // Auto-collapse once the answer starts arriving; user can reopen.
  useEffect(() => { if (!streaming) setOpen(false); }, [streaming]);
  return (
    <div className="jc-chatbox-reasoning">
      <button type="button" onClick={() => setOpen(!open)} aria-expanded={open}>
        {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
        <Sparkles size={12} /> {t("bench.console.reasoning")}
      </button>
      {open && <div className="jc-chatbox-reasoning-body">{text}</div>}
    </div>
  );
}

function CopyButton({ text }: { text: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  return (
    <Tooltip content={copied ? t("bench.console.copied") : t("bench.console.copy")}>
      <IconButton
        size="1"
        variant="ghost"
        color="gray"
        onClick={async () => {
          if (await copyText(text)) {
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1400);
          }
        }}
      >
        {copied ? <Check size={13} /> : <Copy size={13} />}
      </IconButton>
    </Tooltip>
  );
}

function chatMessageVisible(message: ChatMsg): boolean {
  return Boolean(message.content.trim() || message.reasoning || message.error || (message.images?.length ?? 0) > 0 || (message.tools?.length ?? 0) > 0);
}

export async function consumeSSE(res: Response, onPayload: (payload: Record<string, unknown>) => Promise<void>): Promise<void> {
  if (!res.body) return;
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    const events = buf.split("\n\n");
    buf = events.pop() ?? "";
    for (const evt of events) {
      for (const line of evt.split("\n")) {
        if (!line.startsWith("data:")) continue;
        const data = line.slice(5).trim();
        if (!data || data === "[DONE]") continue;
        let payload: Record<string, unknown>;
        try {
          payload = JSON.parse(data) as Record<string, unknown>;
        } catch {
          continue;
        }
        await onPayload(payload);
      }
    }
  }
}

const markdownImage: Components["img"] = ({ src, alt }) => (
  typeof src === "string" && src ? <StoredImage id={src} fit="natural" alt={alt || ""} /> : null
);

export function AssistantMessage({ m, streaming }: { m: ChatMsg; streaming: boolean }) {
  return (
    <div className="jc-chatbox-row">
      <div className="jc-chatbox-avatar">
        <ModelBrandIcon model={m.model || "assistant"} size={17} />
      </div>
      <div className="jc-chatbox-assistant-body">
        {m.reasoning && <ReasoningBlock text={m.reasoning} streaming={streaming && !m.content} />}
        {(m.tools?.length ?? 0) > 0 && <ToolSteps tools={m.tools!} streaming={streaming} />}
        {m.error ? (
          <Text size="2" color="red">{m.content}</Text>
        ) : (m.content || streaming) ? (
          <div className="jc-chatbox-md">
            <LabMarkdown img={markdownImage}>{m.content}</LabMarkdown>
            {streaming && <span className="jc-chatbox-cursor" />}
          </div>
        ) : null}
        {(m.images?.length ?? 0) > 0 && (
          <Flex gap="2" mt="2" wrap="wrap">
            {m.images!.map((src, j) => (
              <StoredImage key={j} id={src} />
            ))}
          </Flex>
        )}
        {!streaming && m.content && !m.error && (
          <div className="jc-chatbox-actions">
            <CopyButton text={m.content} />
          </div>
        )}
      </div>
    </div>
  );
}

function newChatId() {
  return crypto.randomUUID();
}

function formatWhen(ts: number): string {
  const delta = Date.now() - ts;
  if (delta < 60_000) return i18n.t("bench.console.justNow");
  if (delta < 3_600_000) return i18n.t("bench.console.minutesAgo", { count: Math.floor(delta / 60_000) });
  if (delta < 86_400_000) return i18n.t("bench.console.hoursAgo", { count: Math.floor(delta / 3_600_000) });
  return new Date(ts).toLocaleDateString(localeTag());
}

function ChatLab({ models, groups, userId, hasPlan, walletLabel, billingSource, prefs, setPrefs }: LabTabProps) {
  const { t } = useTranslation();
  const loaded = useRef(loadChatCache(localStorage, userId)).current;
  const restored = loaded.chats.find((c) => c.id === loaded.activeId);
  const [chatId, setChatId] = useState(restored?.id || loaded.activeId || newChatId());
  const [chats, setChats] = useState<StoredChat[]>(loaded.chats);
  const chatsRef = useRef(chats);
  chatsRef.current = chats;
  const [model, setModel] = useState(restored?.model ?? "");
  const [groupId, setGroupId] = useState(restored?.groupId || AUTO_GROUP);
  const [effort, setEffort] = useState<Effort>((restored?.effort as Effort) || "default");
  const [customEffort, setCustomEffort] = useState(restored?.customEffort ?? "");
  const [msgs, setMsgs] = useState<ChatMsg[]>(restored?.messages ?? []);
  const [draft, setDraft] = useState("");
  const [editing, setEditing] = useState<number | null>(null);
  const [editDraft, setEditDraft] = useState("");
  const [attachments, setAttachments] = useState<string[]>([]);
  const useSearch = prefs.search;
  const useResearch = prefs.research;
  const [useImage, setUseImage] = useState(true);
  const [busy, setBusy] = useState(false);
  const [usage, setUsage] = useState<ChatUsage | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const snapshotRef = useRef({ chatId, msgs, model, groupId, effort, customEffort });
  snapshotRef.current = { chatId, msgs, model, groupId, effort, customEffort };
  useEffect(() => {
    let dead = false;
    let timer = 0;
    const failed = new Map<string, number>();
    const run = async () => {
      const cache = loadChatCache(localStorage, userId);
      const live = snapshotRef.current;
      const byId = new Map(cache.chats.map((chat) => [chat.id, chat]));
      if (live.msgs.some(chatMessageVisible)) {
        const prev = byId.get(live.chatId);
        byId.set(live.chatId, {
          id: live.chatId,
          title: prev?.title ?? "",
          titleFrozen: prev?.titleFrozen === true,
          updatedAt: Date.now(),
          model: live.model,
          groupId: live.groupId,
          effort: live.effort,
          customEffort: live.customEffort,
          messages: live.msgs,
        });
      }
      const threads = [...byId.values()];
      const inline = [...new Set(threads.flatMap((chat) => chat.messages.flatMap((message) => message.images ?? [])).filter(isInlineImage))];
      if (inline.length === 0 || dead) return;
      const keepIds = [
        ...threads.flatMap((chat) => chat.messages.flatMap((message) => message.images ?? [])),
        ...loadImageCache(localStorage, userId).sessions.flatMap((item) => [
          ...item.sources,
          ...(item.editTarget ? [item.editTarget] : []),
          ...item.results.flatMap((result) => [...result.images, ...(result.sources ?? [])]),
        ]),
      ].filter((id) => !isInlineImage(id));
      const mapped = new Map<string, string>();
      const drop = new Set<string>();
      for (const value of inline) {
        try {
          const savedId = await materializeImage(userId, value, "", "", keepIds);
          mapped.set(value, savedId);
          keepIds.push(savedId);
        } catch {
          const tries = (failed.get(value) ?? 0) + 1;
          failed.set(value, tries);
          if (tries >= 3) drop.add(value);
        }
      }
      if (dead) return;
      if (mapped.size === 0 && drop.size === 0) {
        timer = window.setTimeout(() => void run(), 2000);
        return;
      }
      const rewrite = (id: string) => (drop.has(id) ? "" : (mapped.get(id) ?? id));
      const nextChats = threads.map((chat) => ({
        ...chat,
        messages: chat.messages.map((message) => (
          message.images?.some((image) => mapped.has(image) || drop.has(image))
            ? { ...message, images: message.images.map(rewrite).filter((image) => image.length > 0) }
            : message
        )),
      }));
      const saved = saveChatCache(localStorage, userId, { activeId: live.chatId, chats: nextChats });
      const hasInline = (items: StoredChat[]) => items.some((chat) => chat.messages.some((message) => message.images?.some(isInlineImage)));
      const applied = hasInline(saved.chats) ? nextChats : saved.chats;
      setChats(applied);
      const current = applied.find((chat) => chat.id === snapshotRef.current.chatId);
      if (current) setMsgs(current.messages);
      if (hasInline(nextChats) && !dead) timer = window.setTimeout(() => void run(), 2000);
    };
    void run();
    return () => {
      dead = true;
      window.clearTimeout(timer);
    };
  }, [userId]);

  const { note: selectionNote, modelOptions, activeModel, activeGroupId, pickModel, pickGroup } = useLabPick(models, groups, model, setModel, groupId, setGroupId);
  const efforts = effortOptions(model);
  const drawModel = imageToolModel(
    models.map((item) => item.name),
    modelOptions.map((item) => item.name),
  );
  const canSearch = supportsWebSearch(activeModel);
  const searchOn = canSearch && (useSearch || useResearch);
  const researchOn = canSearch && useResearch;
  const canDraw = supportsImageTool(activeModel) && !!drawModel;
  const modelReady = !!activeModel && modelOptions.some((item) => item.name === activeModel);
  useEffect(() => {
    if (effort !== "default" && !efforts.some((option) => option.value === effort)) setEffort("default");
  }, [effort, efforts]);

  const heldTitle = (id: string) => {
    const chat = chatsRef.current.find((item) => item.id === id);
    return chat?.titleFrozen ? chat.title : "";
  };

  const chatIsFrozen = (id: string) => chatsRef.current.find((item) => item.id === id)?.titleFrozen === true;

  const saveLiveChat = () => {
    const s = snapshotRef.current;
    if (s.msgs.some((message) => message.images?.some(isInlineImage))) return;
    upsertChat(localStorage, userId, {
      id: s.chatId,
      title: heldTitle(s.chatId),
      titleFrozen: chatIsFrozen(s.chatId),
      updatedAt: Date.now(),
      model: s.model,
      groupId: s.groupId,
      effort: s.effort,
      customEffort: s.customEffort,
      messages: s.msgs,
    });
  };

  const persistNow = () => {
    const s = snapshotRef.current;
    if (s.msgs.some((message) => message.images?.some(isInlineImage))) return;
    const next = upsertChat(localStorage, userId, {
      id: s.chatId,
      title: heldTitle(s.chatId),
      titleFrozen: chatIsFrozen(s.chatId),
      updatedAt: Date.now(),
      model: s.model,
      groupId: s.groupId,
      effort: s.effort,
      customEffort: s.customEffort,
      messages: s.msgs,
    });
    setChats((cur) => {
      const mem = new Map(cur.map((c) => [c.id, c]));
      if (s.msgs.some(chatMessageVisible)) {
        mem.set(s.chatId, {
          id: s.chatId,
          title: "",
          updatedAt: Date.now(),
          model: s.model,
          groupId: s.groupId,
          effort: s.effort,
          customEffort: s.customEffort,
          messages: s.msgs,
        });
      }
      return next.chats.map((saved) => {
        const live = mem.get(saved.id);
        const liveHasImages = live?.messages.some((m) => (m.images?.length ?? 0) > 0);
        const savedHasImages = saved.messages.some((m) => (m.images?.length ?? 0) > 0);
        if (live && liveHasImages && !savedHasImages) return { ...saved, messages: live.messages };
        return saved;
      });
    });
    return next;
  };

  useEffect(() => {
    const t = window.setTimeout(persistNow, 400);
    return () => window.clearTimeout(t);
    // persistNow closes over the latest snapshot ref.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chatId, msgs, model, groupId, effort, customEffort, userId]);

  useEffect(() => {
    const flush = () => { persistNow(); };
    window.addEventListener("pagehide", flush);
    return () => {
      window.removeEventListener("pagehide", flush);
      flush();
    };
    // Flush the latest snapshot when leaving the page or the chat tab.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [userId]);

  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [msgs]);

  // Auto-grow the composer textarea like ChatGPT's input.
  const autoGrow = () => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  };
  useEffect(autoGrow, [draft]);

  const appendToAssistant = (content: string, reasoning: string, images: string[] = []) => {
    setMsgs((cur) => {
      const next = [...cur];
      const last = next[next.length - 1];
      if (!last || last.role !== "assistant") return cur;
      next[next.length - 1] = {
        ...last,
        content: last.content + content,
        reasoning: (last.reasoning ?? "") + reasoning || undefined,
        images: images.length ? [...(last.images ?? []), ...images] : last.images,
      };
      return next;
    });
  };

  const run = async (history: ChatMsg[]) => {
    const clean = history.filter((m) => !m.error);
    const lastUser = [...clean].reverse().find((m) => m.role === "user");
    if (busy || !modelReady || !lastUser) return;
    const tools = { search: searchOn, image: useImage && canDraw };
    const surface = labSurface(activeModel, tools);
    const defs = labToolDefs(surface, tools);
    const system = tools.search ? webToolPrompt({ research: researchOn, today: new Date().toLocaleDateString("zh-CN") }) : "";
    const toolRounds = researchOn ? RESEARCH_TOOL_ROUNDS : TOOL_ROUNDS;
    const group = activeGroupId;
    const drawGroup = groupFor(models, groups, drawModel, activeGroupId);
    setMsgs([...clean, { role: "assistant", content: "", model: activeModel }]);
    setEditing(null);
    setDraft("");
    setAttachments([]);
    setUsage(null);
    setBusy(true);
    const ac = new AbortController();
    abortRef.current = ac;
    const steps = new Map<string, StoredToolCall>();
    const publish = () => {
      const view = [...steps.values()];
      setMsgs((cur) => {
        const next = [...cur];
        const last = next[next.length - 1];
        if (!last || last.role !== "assistant") return cur;
        next[next.length - 1] = { ...last, tools: view };
        return next;
      });
    };
    const drawImage = async (tool: LabToolAcc): Promise<ToolRunResult> => {
      const prompt = labToolDetail(tool.input);
      const base = { id: tool.id, callId: tool.callId, name: tool.name, arguments: tool.input };
      const fail = (error: string): ToolRunResult => {
        steps.set(tool.id, { id: tool.id, name: tool.name, detail: prompt, status: "error", error });
        return { ...base, output: t("bench.console.drawFailed", { error }), isError: true };
      };
      if (!prompt) return fail(t("bench.console.noDrawPrompt"));
      if (!drawModel) return fail(t("bench.console.noDrawModel"));
      try {
        const drawn = await drawWithImagesAPI({
          baseURL: apiBaseURL,
          groupId: drawGroup,
          billingSource,
          model: drawModel,
          prompt,
          sources: lastUser.images ?? [],
          signal: ac.signal,
        });
        if (drawn.length === 0) throw new Error(t("bench.console.noImageData"));
        const drawnIds: string[] = [];
        const keep = pinnedAssetIds(userId);
        for (const image of drawn) {
          const blob = await blobFromImagePayload(image);
          const saved = await saveAsset({ userId, blob, prompt, model: drawModel, keepIds: keep });
          keep.push(saved.id);
          drawnIds.push(saved.id);
        }
        appendToAssistant("", "", drawnIds);
        steps.set(tool.id, { id: tool.id, name: tool.name, detail: prompt, status: "done" });
        return { ...base, output: t("bench.console.drawDone") };
      } catch (err) {
        if (ac.signal.aborted) throw err;
        return fail(err instanceof Error ? err.message : errMsg(err));
      }
    };
    try {
      const effortField = effort !== "default" ? effort : undefined;
      const apiMsgs = await Promise.all(clean.map(async (message) => ({
        ...message,
        images: message.images ? await resolveImageUrls(message.images) : undefined,
      })));
      let input: Array<Record<string, unknown>> = toApiMessages(apiMsgs);
      let shown = "";
      for (let used = 0; ; used++) {
        const forceAnswer = used >= toolRounds;
        const res = await fetch(`${apiBaseURL}${labConsolePath(surface)}`, {
          method: "POST",
          headers: labRequestHeaders(group, billingSource),
          body: JSON.stringify(labRequestBody(surface, { model: activeModel, input, tools: defs, effort: effortField, system, forceAnswer })),
          signal: ac.signal,
        });
        if (!res.ok || !res.body) throw new Error(await readGatewayError(res));
        let acc: LabToolAcc[] = [];
        let blocks: AnthropicBlock[] = [];
        let text = "";
        let reasoning = "";
        await consumeSSE(res, async (j) => {
          const usage = j.usage as ChatUsage | undefined;
          if (usage) setUsage(usage);
          const delta = readLabDelta(j);
          let images = delta.images;
          if (images.some(isInlineImage)) {
            const stored: string[] = [];
            const keep = pinnedAssetIds(userId);
            for (const image of images) {
              const savedId = await materializeImage(userId, image, "", activeModel, keep);
              keep.push(savedId);
              stored.push(savedId);
            }
            images = stored;
          }
          let piece = delta.text;
          if (piece) {
            // A new round continues the answer as a new paragraph.
            if (!text && shown.trim()) piece = `\n\n${piece}`;
            text += delta.text;
            shown += piece;
          }
          reasoning += delta.reasoning;
          if (piece || delta.reasoning || images.length) appendToAssistant(piece, delta.reasoning, images);
          if (surface === "anthropic") blocks = foldAnthropicBlocks(blocks, j);
          const activity = readLabTools(j);
          if (activity.patches.length) {
            acc = activity.patches.reduce(foldLabTool, acc);
            for (const tool of acc) {
              steps.set(tool.id, { id: tool.id, name: tool.name, detail: labToolDetail(tool.input), status: "running" });
            }
            publish();
          }
        });
        if (acc.length === 0 || forceAnswer) break;
        const results = await Promise.all(acc.map(async (tool): Promise<ToolRunResult> => {
          if (tool.name === GENERATE_IMAGE_TOOL) return drawImage(tool);
          const done = await runWebTool(labWebToolsApi, { id: tool.id, name: tool.name, arguments: tool.input }, ac.signal);
          steps.set(tool.id, done.step);
          publish();
          return { id: tool.id, callId: tool.callId, name: tool.name, arguments: tool.input, output: done.output, isError: done.isError };
        }));
        if (ac.signal.aborted) throw new DOMException("aborted", "AbortError");
        publish();
        input = appendToolTurn(surface, input, { text, reasoning, blocks: surface === "anthropic" ? blocks : undefined, calls: results });
      }
    } catch (e) {
      if (!ac.signal.aborted) {
        const message = e instanceof Error ? e.message : errMsg(e);
        setMsgs((cur) => {
          const next = [...cur];
          const last = next[next.length - 1];
          if (last?.role === "assistant" && !last.content.trim() && !last.reasoning && !(last.images?.length) && !(last.tools?.length)) {
            next[next.length - 1] = { role: "assistant", content: t("bench.console.requestFailed", { message }), error: true, model: activeModel };
          } else if (last?.role === "assistant") {
            next[next.length - 1] = { ...last, content: `${last.content}${last.content.trim() ? "\n\n" : ""}> ${t("bench.console.requestInterrupted", { message })}` };
          }
          return next;
        });
      }
    } finally {
      for (const [id, step] of steps) {
        if (step.status === "running") steps.set(id, { ...step, status: "error", error: ac.signal.aborted ? t("bench.console.stopped") : t("bench.console.incomplete") });
      }
      if (steps.size > 0) publish();
      setBusy(false);
      abortRef.current = null;
    }
  };

  const send = () => {
    const text = draft.trim();
    if (!text && attachments.length === 0) return;
    const userMsg: ChatMsg = { role: "user", content: text, images: attachments.length ? attachments : undefined };
    void run([...msgs, userMsg]);
  };

  const lastSentIndex = msgs.reduce((found, message, index) => (message.role === "user" ? index : found), -1);

  const retryAt = (index: number) => {
    if (index !== lastSentIndex) return;
    void run(msgs.slice(0, index + 1));
  };

  const deletePart = (index: number) => {
    if (busy || index !== lastSentIndex) return;
    if (editing === index) setEditing(null);
    let end = index + 1;
    while (end < msgs.length && msgs[end].role === "assistant") end += 1;
    setMsgs([...msgs.slice(0, index), ...msgs.slice(end)]);
  };

  const saveEdit = (index: number) => {
    const target = msgs[index];
    const text = editDraft.trim();
    if (index !== lastSentIndex || !target || target.role !== "user" || !text) return;
    void run([...msgs.slice(0, index), { ...target, content: text }]);
  };

  const stop = () => abortRef.current?.abort();

  const pickImages = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? [])
      .filter((file) => file.type.startsWith("image/") && file.size <= MAX_IMAGE_BYTES)
      .slice(0, MAX_IMAGES);
    event.target.value = "";
    const ids: string[] = [];
    const keep = pinnedAssetIds(userId);
    for (const file of files) {
      const saved = await saveAsset({ userId, blob: file, prompt: file.name, model, keepIds: keep });
      keep.push(saved.id);
      ids.push(saved.id);
    }
    setAttachments((cur) => [...cur, ...ids].slice(0, MAX_IMAGES));
  };

  const onComposerKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    send();
  };

  const canSend = modelReady && (!!draft.trim() || attachments.length > 0);
  const empty = msgs.length === 0;

  const openStored = (stored: StoredChat) => {
    if (busy || stored.id === chatId) return;
    abortRef.current?.abort();
    setChatId(stored.id);
    setMsgs(stored.messages);
    setModel(stored.model);
    setGroupId(stored.groupId || AUTO_GROUP);
    setEffort((stored.effort as Effort) || "default");
    setCustomEffort(stored.customEffort ?? "");
    setUsage(null);
    setDraft("");
    setEditing(null);
    setAttachments([]);
  };

  const resetComposer = () => {
    abortRef.current?.abort();
    setChatId(newChatId());
    setMsgs([]);
    setUsage(null);
    setDraft("");
    setEditing(null);
    setAttachments([]);
  };

  const removeStored = (id: string) => {
    const next = deleteChat(localStorage, userId, id);
    setChats((cur) => {
      const mem = new Map(cur.map((c) => [c.id, c]));
      return next.chats.map((saved) => {
        const live = mem.get(saved.id);
        const liveHasImages = live?.messages.some((m) => (m.images?.length ?? 0) > 0);
        const savedHasImages = saved.messages.some((m) => (m.images?.length ?? 0) > 0);
        if (live && liveHasImages && !savedHasImages) return { ...saved, messages: live.messages };
        return saved;
      });
    });
    if (id !== chatId) return;
    const fallback = next.chats.find((x) => x.id === next.activeId);
    if (fallback) openStored(fallback);
    else resetComposer();
  };

  const blankChat = () => {
    if (busy || msgs.length === 0) return;
    persistNow();
    resetComposer();
  };

  const storedFromLive = (stored: StoredChat): StoredChat => {
    if (stored.id !== chatId) return stored;
    const s = snapshotRef.current;
    return { ...stored, model: s.model, groupId: s.groupId, effort: s.effort, customEffort: s.customEffort, messages: s.msgs };
  };

  const openFork = (fork: StoredChat) => {
    const saved = upsertChat(localStorage, userId, fork);
    setChats(saved.chats);
    const opened = saved.chats.find((chat) => chat.id === fork.id) ?? fork;
    openStored(opened);
  };

  const duplicateChat = (stored: StoredChat) => {
    if (busy) return;
    saveLiveChat();
    const source = storedFromLive(stored);
    if (!source.messages.some((message) => message.content.trim() || message.reasoning || (message.images?.length ?? 0) > 0)) return;
    openFork({
      ...source,
      id: newChatId(),
      title: forkTitle(source.title || chatTitle(source.messages), "copy"),
      titleFrozen: true,
      updatedAt: Date.now(),
    });
  };

  const branchAt = (index: number) => {
    if (busy || msgs[index]?.role !== "user") return;
    let end = index + 1;
    while (end < msgs.length && msgs[end].role === "assistant") end += 1;
    saveLiveChat();
    const messages = msgs.slice(0, end);
    openFork({
      id: newChatId(),
      title: forkTitle(chatTitle(messages), "branch"),
      titleFrozen: true,
      updatedAt: Date.now(),
      model,
      groupId,
      effort,
      customEffort,
      messages,
    });
  };

  const composer = (
    <div className="jc-chatbox-composer-outer">
      <div className="jc-lab-composer-bar">
        <ModelPicker
          models={modelOptions}
          groups={groups}
          model={activeModel}
          groupId={activeGroupId}
          onModel={pickModel}
          onGroup={pickGroup}
          placeholder={t("bench.console.selectModel")}
          side={empty ? "bottom" : "top"}
        />
        <span className="jc-chatbox-toolspacer" />
        <PlanOnlySwitch planOnly={prefs.planOnly} hasPlan={hasPlan} walletLabel={walletLabel} onChange={(planOnly) => setPrefs({ planOnly })} />
      </div>
      {selectionNote && <p className="jc-lab-selection-note">{selectionNote}</p>}
      {attachments.length > 0 && (
        <Flex gap="2" mb="2" wrap="wrap">
          {attachments.map((src, i) => (
            <div key={i} className="jc-lab-attach-thumb">
              <StoredImage id={src} />
              <button type="button" className="jc-lab-attach-remove" aria-label={t("bench.console.removeImage")} onClick={() => setAttachments(attachments.filter((_, j) => j !== i))}>
                <X size={11} />
              </button>
            </div>
          ))}
        </Flex>
      )}
      <div className="jc-chatbox-composer">
        <div className="jc-chatbox-composer-main">
          <Tooltip content={t("bench.console.uploadTip", { max: MAX_IMAGES })}>
            <label className="jc-chatbox-attach" aria-label={t("bench.console.uploadImage")}>
              <Paperclip size={17} />
              <input type="file" accept="image/*" multiple hidden onChange={pickImages} />
            </label>
          </Tooltip>
          <textarea
            ref={textareaRef}
            className="jc-chatbox-input"
            rows={1}
            placeholder={activeModel ? (researchOn ? t("bench.console.placeholderResearch") : t("bench.console.placeholder")) : t("bench.console.placeholderNoModel")}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={onComposerKeyDown}
          />
        </div>
        <div className="jc-chatbox-toolrow">
          {canSearch && (
            <>
              <Tooltip content={searchOn ? t("bench.console.searchOnTip") : t("bench.console.searchOffTip")}>
                <button
                  type="button"
                  className={`jc-lab-tool is-labeled${searchOn ? " is-on" : ""}`}
                  aria-pressed={searchOn}
                  disabled={useResearch}
                  onClick={() => setPrefs({ search: !useSearch })}
                >
                  <Globe size={15} /> {t("bench.console.search")}
                </button>
              </Tooltip>
              <Tooltip content={useResearch ? t("bench.console.researchOnTip") : t("bench.console.researchOffTip")}>
                <button
                  type="button"
                  className={`jc-lab-tool is-labeled${useResearch ? " is-on" : ""}`}
                  aria-pressed={useResearch}
                  onClick={() => setPrefs({ research: !useResearch })}
                >
                  <Telescope size={15} /> Research
                </button>
              </Tooltip>
            </>
          )}
          {canDraw && (
            <Tooltip content={useImage ? t("bench.console.imageOnTip", { model: drawModel }) : t("bench.console.imageOffTip")}>
              <button type="button" className={`jc-lab-tool${useImage ? " is-on" : ""}`} aria-pressed={useImage} aria-label={t("bench.console.imageGen")} title={drawModel} onClick={() => setUseImage((v) => !v)}>
                <ImageIcon size={16} />
              </button>
            </Tooltip>
          )}
          <span className="jc-chatbox-toolspacer" />
          {efforts.length > 0 && (
            <Select.Root value={efforts.some((option) => option.value === effort) ? effort : "default"} onValueChange={setEffort} size="1">
              <Select.Trigger variant="ghost" className="jc-chatbox-effort" aria-label={t("bench.console.effort")} />
              <Select.Content position="popper">
                {efforts.map((option) => (
                  <Select.Item key={option.value} value={option.value}>{option.label}</Select.Item>
                ))}
              </Select.Content>
            </Select.Root>
          )}
          {busy ? (
            <Tooltip content={t("bench.console.stopTip")}>
              <button type="button" className="jc-chatbox-send is-stop" onClick={stop} aria-label={t("bench.console.stop")}>
                <Square size={14} />
              </button>
            </Tooltip>
          ) : (
            <button type="button" className="jc-chatbox-send" disabled={!canSend} onClick={() => send()} aria-label={t("bench.console.send")}>
              <ArrowUp size={17} strokeWidth={2.4} />
            </button>
          )}
        </div>
      </div>
      <div className="jc-chatbox-hint">
        {t("bench.console.hint")}
        {usage && (
          <span className="jc-num">
            {" "}· {t("bench.console.lastUsage", {
              input: usage.prompt_tokens?.toLocaleString(localeTag()) ?? "—",
              output: usage.completion_tokens?.toLocaleString(localeTag()) ?? "—",
            })}
          </span>
        )}
      </div>
    </div>
  );

  return (
    <div className="jc-lab-chat-shell">
      <aside className="jc-lab-sessions">
        <button type="button" className="jc-lab-newchat" disabled={empty || busy} onClick={blankChat}>
          <Plus size={14} /> {t("bench.console.newChat")}
        </button>
        <div className="jc-lab-session-list">
          {chats.length === 0 && <div className="jc-lab-session-empty">{t("bench.console.noChats")}</div>}
          {chats.map((c) => (
            <div key={c.id} className={`jc-lab-session${c.id === chatId ? " is-active" : ""}`}>
              <button type="button" className="jc-lab-session-open" onClick={() => { persistNow(); openStored(c); }}>
                <span>{c.title || t("bench.console.untitledChat")}</span>
                <small>{formatWhen(c.updatedAt)}</small>
              </button>
              <button type="button" className="jc-lab-history-copy" aria-label={t("bench.console.duplicateChatAria")} title={t("bench.console.duplicateChatTitle")} disabled={busy} onClick={() => duplicateChat(c)}>
                <Copy size={12} />
              </button>
              <button type="button" className="jc-lab-history-del" aria-label={t("bench.console.deleteChatAria")} onClick={() => removeStored(c.id)}>
                <Trash2 size={12} />
              </button>
            </div>
          ))}
        </div>
      </aside>
      <div className="jc-chatbox">

      {empty ? (
        <div className="jc-chatbox-emptywrap">
          <div className="jc-chatbox-col">
            <div className="jc-chatbox-greeting">{t("bench.console.greeting")}</div>
            {composer}
            <div className="jc-chatbox-suggestions">
              {SUGGESTIONS.map((key) => {
                const s = t(`bench.console.suggestions.${key}`);
                return (
                  <button key={key} type="button" onClick={() => { setDraft(s); textareaRef.current?.focus(); }}>
                    {s}
                  </button>
                );
              })}
            </div>
          </div>
        </div>
      ) : (
        <>
          <div className="jc-chatbox-scroll" ref={scrollRef}>
            <div className="jc-chatbox-col">
              {msgs.map((m, i) =>
                m.role === "user" ? (
                  <div key={i} className="jc-chatbox-row is-user">
                    <div className="jc-chatbox-user-col">
                      {editing === i ? (
                        <div className="jc-chatbox-edit">
                          <textarea
                            className="jc-chatbox-input"
                            rows={3}
                            value={editDraft}
                            onChange={(e) => setEditDraft(e.target.value)}
                            onKeyDown={(event) => {
                              if (event.key === "Escape") setEditing(null);
                              if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
                              event.preventDefault();
                              saveEdit(i);
                            }}
                          />
                          <Flex gap="2" justify="end">
                            <Button size="1" variant="soft" color="gray" onClick={() => setEditing(null)}>{t("bench.console.cancel")}</Button>
                            <Button size="1" disabled={!editDraft.trim() || busy} onClick={() => saveEdit(i)}>{t("bench.console.saveRegenerate")}</Button>
                          </Flex>
                        </div>
                      ) : (
                        <div className="jc-chatbox-user-bubble">
                          {m.content && <div style={{ whiteSpace: "pre-wrap" }}>{m.content}</div>}
                          {(m.images?.length ?? 0) > 0 && (
                            <Flex gap="2" mt={m.content ? "2" : "0"} wrap="wrap" justify="end">
                              {m.images!.map((src, j) => (
                                <StoredImage key={j} id={src} />
                              ))}
                            </Flex>
                          )}
                        </div>
                      )}
                      {!busy && editing !== i && (
                        <div className="jc-chatbox-actions">
                          {i === lastSentIndex && (
                            <>
                              <Tooltip content={t("bench.console.editRetry")}>
                                <IconButton size="1" variant="ghost" color="gray" aria-label={t("bench.console.editRetry")} onClick={() => { setEditing(i); setEditDraft(m.content); }}>
                                  <Pencil size={13} />
                                </IconButton>
                              </Tooltip>
                              <Tooltip content={t("bench.console.resend")}>
                                <IconButton size="1" variant="ghost" color="gray" aria-label={t("bench.console.resend")} onClick={() => retryAt(i)}>
                                  <RotateCcw size={13} />
                                </IconButton>
                              </Tooltip>
                              <Tooltip content={t("bench.console.deletePart")}>
                                <IconButton size="1" variant="ghost" color="gray" aria-label={t("bench.console.deletePart")} onClick={() => deletePart(i)}>
                                  <Trash2 size={13} />
                                </IconButton>
                              </Tooltip>
                            </>
                          )}
                          <Tooltip content={t("bench.console.branchTip")}>
                            <IconButton size="1" variant="ghost" color="gray" aria-label={t("bench.console.branchAria")} onClick={() => branchAt(i)}>
                              <GitBranch size={13} />
                            </IconButton>
                          </Tooltip>
                        </div>
                      )}
                    </div>
                  </div>
                ) : (
                  <AssistantMessage key={i} m={m} streaming={busy && i === msgs.length - 1} />
                ),
              )}
            </div>
          </div>
          <div className="jc-chatbox-footer">
            <div className="jc-chatbox-col">{composer}</div>
          </div>
        </>
      )}
    </div>
    </div>
  );
}

/* ============ 文生图 / 图生图 ============ */

function useAssetThumbs(ids: string[]) {
  const [urls, setUrls] = useState<Record<string, string>>({});
  const key = ids.join("|");
  useEffect(() => {
    let dead = false;
    const created: string[] = [];
    (async () => {
      const next: Record<string, string> = {};
      for (const id of ids) {
        const blob = await assetBlob(id, "thumb");
        if (!blob) continue;
        const url = URL.createObjectURL(blob);
        created.push(url);
        next[id] = url;
      }
      if (!dead) setUrls(next);
    })();
    return () => {
      dead = true;
      for (const url of created) URL.revokeObjectURL(url);
    };
  }, [key]);
  return urls;
}

function customEdges(size: string): [string, string] {
  const match = /^(\d*)x(\d*)$/.exec(size);
  return match ? [match[1], match[2]] : ["", ""];
}

function ImageLab({ models, groups, userId, hasPlan, walletLabel, billingSource, prefs, setPrefs }: LabTabProps) {
  const { t } = useTranslation();
  const loaded = useRef(loadImageCache(localStorage, userId)).current;
  const initial = loaded.sessions.find((item) => item.id === loaded.activeId) ?? loaded.sessions[0];
  const [sessionId, setSessionId] = useState(initial?.id || newChatId());
  const [sessions, setSessions] = useState<ImageSession[]>(loaded.sessions.length ? loaded.sessions : []);
  const sessionsRef = useRef(sessions);
  const sessionIdRef = useRef(sessionId);
  sessionsRef.current = sessions;
  sessionIdRef.current = sessionId;
  const [model, setModel] = useState(initial?.model ?? "");
  const [groupId, setGroupId] = useState(initial?.groupId || AUTO_GROUP);
  const [size, setSize] = useState(initial?.size || "1024x1024");
  const [count, setCount] = useState(initial?.count || "1");
  const [prompt, setPrompt] = useState(initial?.prompt ?? "");
  const [customW, setCustomW] = useState(() => customEdges(initial?.size || "")[0]);
  const [customH, setCustomH] = useState(() => customEdges(initial?.size || "")[1]);
  const [customMode, setCustomMode] = useState(() => {
    const initialSize = initial?.size || "";
    return !!initialSize && !imageSizePolicy(initial?.model || "").presets.some((item) => item.size === initialSize);
  });
  const [pictureIntent, setPictureIntent] = useState<"generate" | "edit">("generate");
  const [maskOpen, setMaskOpen] = useState(false);
  const [sources, setSources] = useState<string[]>(initial?.sources ?? []);
  const [editId, setEditId] = useState(initial?.editTarget ?? "");
  const [sessionTitle, setSessionTitle] = useState(initial?.title ?? "");
  const [titleFrozen, setTitleFrozen] = useState(initial?.titleFrozen === true);
  const maskRef = useRef<MaskPainterHandle>(null);
  const maskDirtyRef = useRef(false);
  const [quality, setQuality] = useState("auto");
  const [background, setBackground] = useState("auto");
  const [outputFormat, setOutputFormat] = useState("png");
  const [fidelity, setFidelity] = useState("high");
  const initialFocus = initial?.results[0]?.images[0] || initial?.sources[0] || "";
  const [focusId, setFocusId] = useState(initialFocus);
  const [stageFrom, setStageFrom] = useState<"result" | "source" | "edit">(
    initial?.results.some((item) => item.images.includes(initialFocus)) ? "result" : "source",
  );
  const [maskDirty, setMaskDirty] = useState(false);
  const markMask = (dirty: boolean) => {
    maskDirtyRef.current = dirty;
    setMaskDirty(dirty);
  };
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [results, setResults] = useState<StoredImageResult[]>(initial?.results ?? []);

  const { note: selectionNote, modelOptions, activeModel, activeGroupId, pickModel, pickGroup } = useLabPick(models, groups, model, setModel, groupId, setGroupId);
  const modelReady = !!activeModel && modelOptions.some((item) => item.name === activeModel);
  const sizePolicy = imageSizePolicy(model);
  useEffect(() => {
    if (!model || imageSizePolicy(model).flexible || !imageSizeError(model, size)) return;
    setCustomMode(false);
    setSize(imageSizePolicy(model).presets[0]?.size || "1024x1024");
  }, [model, size]);
  useEffect(() => {
    const match = /^(\d*)x(\d*)$/.exec(size);
    if (!match) return;
    setCustomW(match[1]);
    setCustomH(match[2]);
  }, [size]);
  const qualities = imageQualityOptions(model);
  useEffect(() => {
    if (!qualities.includes(quality)) setQuality("auto");
  }, [qualities, quality]);
  const resultIds = results.flatMap((item) => item.images);
  const thumbs = useAssetThumbs([...new Set([...sources, ...resultIds, ...(editId ? [editId] : [])])]);
  const preview = (id: string) => (id.startsWith("data:") || id.startsWith("http") ? id : thumbs[id]);
  const editTargetId = pictureIntent === "edit" ? editId : "";
  const [editUrl, setEditUrl] = useState("");
  const [stageUrl, setStageUrl] = useState("");
  useEffect(() => {
    if (!focusId) {
      setStageUrl("");
      return;
    }
    if (focusId.startsWith("data:") || focusId.startsWith("http")) {
      setStageUrl(focusId);
      return;
    }
    let dead = false;
    let url = "";
    assetBlob(focusId).then((blob) => {
      if (!blob || dead) return;
      url = URL.createObjectURL(blob);
      setStageUrl(url);
    });
    return () => {
      dead = true;
      if (url) URL.revokeObjectURL(url);
    };
  }, [focusId]);
  useEffect(() => {
    if (!editTargetId) {
      setEditUrl("");
      return;
    }
    if (isInlineImage(editTargetId)) {
      setEditUrl(editTargetId);
      return;
    }
    let dead = false;
    let url = "";
    assetBlob(editTargetId).then((blob) => {
      if (!blob || dead) return;
      url = URL.createObjectURL(blob);
      setEditUrl(url);
    });
    return () => {
      dead = true;
      if (url) URL.revokeObjectURL(url);
    };
  }, [editTargetId]);

  const draftRef = useRef({ sessionId, model, groupId, size, count, prompt, sources, results, sessionTitle, editId, titleFrozen });
  draftRef.current = { sessionId, model, groupId, size, count, prompt, sources, results, sessionTitle, editId, titleFrozen };
  const referencedAssetIds = () => new Set([
    ...sessionsRef.current.flatMap((item) => [...item.sources, ...(item.editTarget ? [item.editTarget] : []), ...item.results.flatMap((result) => [...result.images, ...(result.sources ?? [])])]),
    ...loadChatCache(localStorage, userId).chats.flatMap((chat) => chat.messages.flatMap((message) => message.images ?? [])),
    ...(editId && !isInlineImage(editId) ? [editId] : []),
  ].filter((id) => !isInlineImage(id)));

  const displayTitle = (text: string, current: string, frozen: boolean) => (
    frozen ? (current.trim().slice(0, 32) || t("bench.console.untitled")) : (text.trim().slice(0, 28) || t("bench.console.newImage"))
  );

  const sessionSnapshot = (): ImageSession => ({
    id: sessionId,
    title: displayTitle(prompt, sessionTitle, titleFrozen),
    titleFrozen,
    updatedAt: Date.now(),
    model,
    groupId: groupId === AUTO_GROUP ? "" : groupId,
    size,
    count,
    prompt,
    sources,
    editTarget: editId,
    results,
  });

  const sessionImages = (item: ImageSession) => [
    ...item.sources,
    ...(item.editTarget ? [item.editTarget] : []),
    ...item.results.flatMap((result) => [...result.images, ...(result.sources ?? [])]),
  ];

  const persistSessions = (activeId: string, next: ImageSession[]) => {
    const inline = (item: ImageSession) => sessionImages(item).some(isInlineImage);
    if (!next.some(inline)) {
      saveImageCache(localStorage, userId, { activeId, sessions: next });
      return;
    }
    const previous = loadImageCache(localStorage, userId);
    const merged = next.flatMap((item) => {
      if (!inline(item)) return [item];
      const saved = previous.sessions.find((entry) => entry.id === item.id);
      return saved ? [saved] : [];
    });
    if (merged.length === 0) return;
    const active = merged.some((item) => item.id === activeId) ? activeId : merged[0].id;
    saveImageCache(localStorage, userId, { activeId: active, sessions: merged });
  };

  const remember = (commit = true) => {
    const draft = draftRef.current;
    const current: ImageSession = {
      id: draft.sessionId,
      title: displayTitle(draft.prompt, draft.sessionTitle, draft.titleFrozen),
      titleFrozen: draft.titleFrozen,
      updatedAt: Date.now(),
      model: draft.model,
      groupId: draft.groupId === AUTO_GROUP ? "" : draft.groupId,
      size: draft.size,
      count: draft.count,
      prompt: draft.prompt,
      sources: draft.sources,
      editTarget: draft.editId,
      results: draft.results,
    };
    const next = [current, ...sessionsRef.current.filter((item) => item.id !== current.id)];
    sessionsRef.current = next;
    if (commit) setSessions(next);
    persistSessions(draft.sessionId, next);
  };

  useEffect(() => {
    const t = window.setTimeout(() => remember(), 400);
    return () => window.clearTimeout(t);
  }, [userId, sessionId, model, groupId, size, count, prompt, sources, results, sessionTitle, editId, titleFrozen]);

  useEffect(() => {
    const flush = () => remember(false);
    window.addEventListener("pagehide", flush);
    return () => {
      window.removeEventListener("pagehide", flush);
      flush();
    };
    // remember reads the latest draft ref, including when the image tab unmounts.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [userId]);

  useEffect(() => {
    let dead = false;
    let timer = 0;
    const failed = new Map<string, number>();
    const run = async () => {
      const collect = (items: ImageSession[]) => items.flatMap(sessionImages);
      const pending = [...new Set(collect(sessionsRef.current).filter(isInlineImage))];
      if (pending.length === 0 || dead) return;
      const keepIds = collect(sessionsRef.current).filter((id) => !isInlineImage(id));
      const mapped = new Map<string, string>();
      const drop = new Set<string>();
      for (const value of pending) {
        try {
          const savedId = await materializeImage(userId, value, "", "", keepIds);
          mapped.set(value, savedId);
          keepIds.push(savedId);
        } catch {
          const tries = (failed.get(value) ?? 0) + 1;
          failed.set(value, tries);
          if (tries >= 3) drop.add(value);
        }
      }
      if (dead) return;
      if (mapped.size === 0 && drop.size === 0) {
        timer = window.setTimeout(() => void run(), 2000);
        return;
      }
      const rewrite = (id: string) => (drop.has(id) ? "" : (mapped.get(id) ?? id));
      const clean = (ids: string[]) => ids.map(rewrite).filter((id) => id.length > 0);
      const next = sessionsRef.current.map((item) => ({
        ...item,
        sources: clean(item.sources),
        editTarget: item.editTarget ? rewrite(item.editTarget) : "",
        results: item.results.map((result) => ({
          ...result,
          images: clean(result.images),
          sources: result.sources ? clean(result.sources) : result.sources,
        })),
      }));
      sessionsRef.current = next;
      setSessions(next);
      const current = next.find((item) => item.id === sessionIdRef.current);
      if (current) {
        setSources(current.sources);
        setResults(current.results);
        setEditId(current.editTarget || "");
        setFocusId((id) => {
          if (drop.has(id)) return current.results[0]?.images[0] || current.sources[0] || current.editTarget || "";
          return mapped.get(id) ?? id;
        });
      }
      persistSessions(sessionIdRef.current, next);
      if (drop.size > 0) setError(i18n.t("bench.console.assetDropped"));
      if (collect(next).some(isInlineImage) && !dead) timer = window.setTimeout(() => void run(), 2000);
    };
    void run();
    return () => {
      dead = true;
      window.clearTimeout(timer);
    };
  }, [userId]);

  const applySession = (item: ImageSession) => {
    setSessionId(item.id);
    setModel(item.model);
    setGroupId(item.groupId || AUTO_GROUP);
    setSize(item.size || "1024x1024");
    setCustomMode(!imageSizePolicy(item.model).presets.some((preset) => preset.size === (item.size || "1024x1024")));
    setCount(item.count || "1");
    setPrompt(item.prompt);
    setSources(item.sources);
    setResults(item.results);
    setSessionTitle(item.title);
    setTitleFrozen(item.titleFrozen === true);
    setEditId(item.editTarget || "");
    setPictureIntent("generate");
    const nextFocus = item.results[0]?.images[0] || item.sources[0] || "";
    setFocusId(nextFocus);
    setStageFrom(item.results.some((result) => result.images.includes(nextFocus)) ? "result" : "source");
    setMaskOpen(false);
    markMask(false);
    maskRef.current?.clear();
    setError("");
  };

  const insertSession = (fresh: ImageSession) => {
    const current = sessionSnapshot();
    const next = [fresh, current, ...sessionsRef.current.filter((entry) => entry.id !== sessionId && entry.id !== fresh.id)];
    sessionsRef.current = next;
    setSessions(next);
    persistSessions(fresh.id, next);
    applySession(fresh);
  };

  const blankSession = () => {
    if (busy) return;
    const id = newChatId();
    const fresh: ImageSession = {
      id, title: t("bench.console.newImage"), updatedAt: Date.now(), model, groupId: groupId === AUTO_GROUP ? "" : groupId,
      size, count, prompt: "", sources: [], editTarget: "", results: [],
    };
    const next = [fresh, sessionSnapshot(), ...sessionsRef.current.filter((item) => item.id !== sessionId && item.id !== id)];
    sessionsRef.current = next;
    setSessions(next);
    persistSessions(id, next);
    applySession(fresh);
  };

  const openSession = (item: ImageSession) => {
    if (item.id === sessionId || busy) return;
    const next = [sessionSnapshot(), ...sessionsRef.current.filter((entry) => entry.id !== sessionId)];
    sessionsRef.current = next;
    setSessions(next);
    persistSessions(item.id, next);
    applySession(item);
  };

  const removeSession = (id: string) => {
    const prev = sessionsRef.current;
    const removed = prev.find((item) => item.id === id);
    let next = prev.filter((item) => item.id !== id);
    let active = id === sessionId ? (next[0]?.id ?? "") : sessionId;
    if (id === sessionId && next.length === 0) {
      const fresh: ImageSession = {
        id: newChatId(), title: t("bench.console.newImage"), updatedAt: Date.now(), model, groupId: "",
        size, count, prompt: "", sources: [], editTarget: "", results: [],
      };
      next = [fresh];
      active = fresh.id;
    }
    sessionsRef.current = next;
    setSessions(next);
    persistSessions(active, next);
    if (removed) {
      const keep = new Set([
        ...next.flatMap((item) => sessionImages(item)),
        ...loadChatCache(localStorage, userId).chats.flatMap((chat) => chat.messages.flatMap((message) => message.images ?? [])),
      ]);
      const drop = sessionImages(removed).filter((imageId) => !keep.has(imageId) && !isInlineImage(imageId));
      void deleteAssets(drop);
    }
    if (id !== sessionId) return;
    const target = next.find((item) => item.id === active);
    if (target) applySession(target);
  };

  const hasSessionWork = (item: ImageSession) => item.prompt.trim() || item.sources.length > 0 || item.results.length > 0 || !!item.editTarget;

  const copyImageSession = (item: ImageSession) => {
    if (busy) return;
    const source = item.id === sessionId ? sessionSnapshot() : item;
    if (!hasSessionWork(source)) return;
    insertSession({
      ...source,
      id: newChatId(),
      title: forkTitle(source.prompt.trim() || source.title, "copy"),
      titleFrozen: true,
      updatedAt: Date.now(),
    });
  };

  const branchImageSession = (item: ImageSession) => {
    if (busy) return;
    const source = item.id === sessionId ? sessionSnapshot() : item;
    if (!source.prompt.trim() && source.sources.length === 0 && !source.editTarget) return;
    insertSession({
      ...source,
      id: newChatId(),
      title: forkTitle(source.prompt.trim() || source.title, "branch"),
      titleFrozen: true,
      updatedAt: Date.now(),
      results: [],
    });
  };

  const refLimit = imageReferenceLimit(model);
  const editLimit = imageEditLimit(model);

  const keepReference = (id: string) => {
    if (refLimit === 0) {
      setError(t("bench.console.noInputImages"));
      return;
    }
    if (sources.includes(id)) return;
    if (sources.length >= refLimit) {
      setError(t("bench.console.refsFull"));
      return;
    }
    setError("");
    setSources((cur) => cur.includes(id) ? cur : [...cur, id]);
  };

  const adoptEditTarget = (id: string) => {
    if (id !== editId) {
      markMask(false);
      maskRef.current?.clear();
    }
    setEditId(id);
    setFocusId(id);
    setStageFrom("edit");
    setMaskOpen(false);
  };

  const makeEditTarget = (id: string) => {
    if (editLimit === 0) {
      setError(t("bench.console.noEdit"));
      return;
    }
    setError("");
    adoptEditTarget(id);
  };

  const continueFromResult = (id: string) => {
    const batch = results.find((item) => item.images.includes(id));
    if (!batch) return;
    if (refLimit === 0) {
      setError(t("bench.console.noInputImages"));
      return;
    }
    if (!sources.includes(id) && sources.length >= refLimit) {
      setError(t("bench.console.refsFull"));
      return;
    }
    setError("");
    setPictureIntent("generate");
    setMaskOpen(false);
    setPrompt(batch.prompt);
    setSources((cur) => cur.includes(id) ? cur : [...cur, id]);
    setFocusId(id);
    setStageFrom("source");
  };

  const branchFromResult = (id: string) => {
    if (busy) return;
    if (refLimit === 0) {
      setError(t("bench.console.noInputImages"));
      return;
    }
    const batch = results.find((item) => item.images.includes(id));
    const text = batch?.prompt || prompt;
    insertSession({
      id: newChatId(),
      title: forkTitle(text.trim() || t("bench.console.imageFallbackTitle"), "branch"),
      titleFrozen: true,
      updatedAt: Date.now(),
      model,
      groupId: groupId === AUTO_GROUP ? "" : groupId,
      size,
      count,
      prompt: text,
      sources: [id],
      editTarget: "",
      results: [],
    });
  };

  const pickSources = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? [])
      .filter((file) => file.type.startsWith("image/") && file.size <= MAX_IMAGE_BYTES)
      .slice(0, refLimit || 1);
    event.target.value = "";
    if (pictureIntent === "edit" ? editLimit === 0 : refLimit === 0) {
      setError(pictureIntent === "edit" ? t("bench.console.noEdit") : t("bench.console.noInputImages"));
      return;
    }
    if (pictureIntent !== "edit" && sources.length >= refLimit) {
      setError(t("bench.console.refsFull"));
      return;
    }
    const picked = pictureIntent === "edit" ? files.slice(0, 1) : files;
    const keepIds = [...referencedAssetIds()];
    for (const file of picked) {
      const saved = await saveAsset({ userId, blob: file, prompt: file.name, model, keepIds });
      keepIds.push(saved.id);
      if (pictureIntent === "edit") {
        adoptEditTarget(saved.id);
        continue;
      }
      setSources((cur) => {
        if (cur.includes(saved.id) || refLimit === 0 || cur.length >= refLimit) return cur;
        return [...cur, saved.id];
      });
      setFocusId(saved.id);
      setStageFrom("source");
    }
  };

  const runImage = async (request: { prompt: string; images: string[]; mode: "generate" | "edit"; mask: boolean }) => {
    const text = request.prompt.trim();
    const inputImages = request.mode === "edit" ? request.images.slice(0, 1) : request.images;
    if (busy || !text) return;
    if (!modelReady) {
      setError(t("bench.console.pickUsableModel"));
      return;
    }
    const sizeError = imageSizeError(model, size);
    if (sizeError) {
      setError(sizeError);
      return;
    }
    if (request.mode === "edit" && inputImages.length === 0) {
      setError(t("bench.console.pickEditImage"));
      return;
    }
    if (inputImages.length > 0 && refLimit === 0) {
      setError(t("bench.console.noInputImages"));
      return;
    }
    setBusy(true);
    setError("");
    const started = performance.now();
    const sessionAtStart = sessionId;
    const resultsAtStart = draftRef.current.results;
    try {
      const headers = labRequestHeaders(activeGroupId, billingSource, false);
      const gptImage = /^gpt-image-/i.test(activeModel);
      const sendFormat = background === "transparent" && outputFormat === "jpeg" ? "png" : outputFormat;
      const sendBackground = sendFormat === "jpeg" && background === "transparent" ? "opaque" : background;
      const extras: Record<string, string> = {};
      if (quality !== "auto") extras.quality = quality;
      if (gptImage && sendBackground !== "auto") extras.background = sendBackground;
      if (gptImage && sendFormat !== "png") extras.output_format = sendFormat;
      if (request.mode === "edit" && imageSupportsFidelity(activeModel)) extras.input_fidelity = fidelity;
      let res: Response;
      if (inputImages.length > 0) {
        const body = new FormData();
        body.append("model", activeModel);
        body.append("prompt", text);
        body.append("n", String(Number(count) || 1));
        body.append("size", size);
        if (!gptImage) body.append("response_format", "b64_json");
        for (const [key, value] of Object.entries(extras)) body.append(key, value);
        const imageField = inputImages.length > 1 ? "image[]" : "image";
        for (const [i, id] of inputImages.entries()) {
          const blob = await imageSourceBlob(id);
          body.append(imageField, blob, `reference-${i}.png`);
        }
        if (request.mask && maskDirtyRef.current) {
          const mask = await maskRef.current?.maskBlob();
          if (mask) body.append("mask", mask, "mask.png");
        }
        res = await fetch(`${apiBaseURL}/v1/me/lab/images/edits`, { method: "POST", headers, body });
      } else {
        res = await fetch(`${apiBaseURL}/v1/me/lab/images/generations`, {
          method: "POST",
          headers: { ...headers, "Content-Type": "application/json" },
          body: JSON.stringify({
            model: activeModel,
            prompt: text,
            n: Number(count) || 1,
            size,
            ...extras,
            ...(gptImage ? {} : { response_format: "b64_json" }),
          }),
        });
      }
      if (!res.ok) throw new Error(await readGatewayError(res));
      const j = await res.json();
      const images = ((j.data ?? []) as Array<{ b64_json?: string; url?: string }>)
        .map((d) => (d.b64_json ? `data:image/png;base64,${d.b64_json}` : d.url))
        .filter((s): s is string => !!s);
      if (images.length === 0) throw new Error(t("bench.console.noImageData"));
      const savedIds: string[] = [];
      for (const image of images) {
        const blob = await blobFromImagePayload(image);
        const saved = await saveAsset({ userId, blob, prompt: text, model: activeModel, keepIds: [...referencedAssetIds()] });
        savedIds.push(saved.id);
      }
      const batch: StoredImageResult = {
        id: newChatId(),
        prompt: text,
        model: activeModel,
        images: savedIds,
        sources: inputImages.length ? inputImages : undefined,
        mode: request.mode,
        ms: Math.round(performance.now() - started),
        createdAt: Date.now(),
      };
      if (!sessionsRef.current.some((item) => item.id === sessionAtStart)) {
        const keep = referencedAssetIds();
        const orphan = savedIds.filter((id) => !keep.has(id));
        if (orphan.length > 0) void deleteAssets(orphan);
        return;
      }
      const draft = draftRef.current;
      const next = sessionsRef.current.map((item) => {
        if (item.id !== sessionAtStart) return item;
        const same = item.id === draft.sessionId;
        const frozen = same ? draft.titleFrozen : item.titleFrozen === true;
        const titleSource = same ? draft.sessionTitle : item.title;
        const promptSource = same ? draft.prompt : item.prompt;
        return {
          ...item,
          results: [batch, ...resultsAtStart],
          updatedAt: Date.now(),
          title: frozen
            ? (titleSource.trim().slice(0, 32) || item.title)
            : (promptSource.trim().slice(0, 28) || item.title),
        };
      });
      sessionsRef.current = next;
      setSessions(next);
      persistSessions(sessionIdRef.current, next);
      if (sessionIdRef.current === sessionAtStart) {
        setResults((cur) => [batch, ...cur]);
        setFocusId(savedIds[0] ?? "");
        setStageFrom("result");
        setMaskOpen(false);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const generate = () => {
    const images = pictureIntent === "edit" ? (editId ? [editId] : []) : sources;
    void runImage({ prompt, images, mode: pictureIntent, mask: pictureIntent === "edit" });
  };

  const replayResult = (id: string) => {
    const batch = results.find((item) => item.images.includes(id));
    if (!batch?.prompt.trim()) {
      setError(t("bench.console.noPromptRecorded"));
      return;
    }
    const images = batch.mode === "edit" ? (batch.sources ?? []).slice(0, 1) : (batch.sources ?? []);
    if (batch.mode === "edit" && images.length === 0) {
      setError(t("bench.console.noSourceRecorded"));
      return;
    }
    void runImage({ prompt: batch.prompt, images, mode: batch.mode, mask: false });
  };

  const onPromptKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    generate();
  };

  const customSize = `${customW}x${customH}`;
  const usingCustom = sizePolicy.flexible
    ? customMode || !sizePolicy.presets.some((item) => item.size === size)
    : !sizePolicy.presets.some((item) => item.size === size) && (sizePolicy.flexible || !model);
  const sizeHint = usingCustom ? imageSizeError(model, size) : "";
  const showMask = pictureIntent === "edit" && maskOpen && !!editUrl;
  const viewingResult = stageFrom === "result" && resultIds.includes(focusId);
  const viewingSource = stageFrom === "source" && sources.includes(focusId);
  const viewingEdit = stageFrom === "edit" && !!editId && focusId === editId;
  const resultPos = resultIds.indexOf(focusId);
  const stageCaption = showMask
    ? t("bench.console.captionMask")
    : viewingEdit && pictureIntent === "edit"
      ? t("bench.console.captionEditing")
      : viewingSource
        ? t("bench.console.captionReference")
        : viewingResult && pictureIntent === "edit" && focusId === editId
          ? t("bench.console.captionResultEdit", { pos: resultPos + 1, total: resultIds.length })
          : viewingResult
            ? t("bench.console.captionResult", { pos: resultPos + 1, total: resultIds.length })
            : "";
  useEffect(() => {
    if (focusId || results.length === 0) return;
    setFocusId(results[0]?.images[0] ?? "");
    setStageFrom("result");
  }, [focusId, results]);

  return (
    <div className="jc-lab-chat-shell">
      <aside className="jc-lab-sessions">
        <button type="button" className="jc-lab-newchat" onClick={blankSession}>
          <Plus size={14} /> {t("bench.console.newImage")}
        </button>
        <div className="jc-lab-session-list">
          {sessions.length === 0 && <div className="jc-lab-session-empty">{t("bench.console.noImages")}</div>}
          {sessions.map((item) => (
            <div key={item.id} className={`jc-lab-session${item.id === sessionId ? " is-active" : ""}`}>
              <button type="button" className="jc-lab-session-open" onClick={() => openSession(item)}>
                <span>{item.title || t("bench.console.newImage")}</span>
                <small>{formatWhen(item.updatedAt)}</small>
              </button>
              <button type="button" className="jc-lab-history-copy" aria-label={t("bench.console.duplicateImageAria")} title={t("bench.console.duplicateImageTitle")} disabled={busy || !(item.id === sessionId ? hasSessionWork(sessionSnapshot()) : hasSessionWork(item))} onClick={() => copyImageSession(item)}>
                <Copy size={12} />
              </button>
              <button type="button" className="jc-lab-history-copy" aria-label={t("bench.console.branchImageAria")} title={t("bench.console.branchImageTitle")} disabled={busy || !(item.id === sessionId ? prompt.trim() || sources.length > 0 || !!editId : item.prompt.trim() || item.sources.length > 0 || !!item.editTarget)} onClick={() => branchImageSession(item)}>
                <GitBranch size={12} />
              </button>
              <button type="button" className="jc-lab-history-del" aria-label={t("bench.console.deleteImageAria")} onClick={() => removeSession(item.id)}>
                <Trash2 size={12} />
              </button>
            </div>
          ))}
        </div>
      </aside>
      <div className="jc-imagelab">
      <header className="jc-imagelab-top">
        <div className="jc-imagelab-bar">
          <ModelPicker
            models={modelOptions}
            groups={groups}
            model={activeModel}
            groupId={activeGroupId}
            onModel={pickModel}
            onGroup={pickGroup}
            placeholder={t("bench.console.selectImageModel")}
          />
          <PlanOnlySwitch planOnly={prefs.planOnly} hasPlan={hasPlan} walletLabel={walletLabel} onChange={(planOnly) => setPrefs({ planOnly })} />
        </div>
        <div className="jc-imagelab-top-end">
          <Select.Root value={count} onValueChange={setCount} size="2">
            <Select.Trigger />
            <Select.Content position="popper">
              {["1", "2", "4"].map((n) => <Select.Item key={n} value={n}>{t("bench.console.imageCount", { count: Number(n) })}</Select.Item>)}
            </Select.Content>
          </Select.Root>
          <Button
            size="2"
            disabled={!modelReady || !prompt.trim() || !!imageSizeError(model, size) || (pictureIntent === "edit" && !editId)}
            loading={busy}
            onClick={generate}
          >
            <Sparkles size={14} /> {pictureIntent === "edit" ? t("bench.console.edit") : t("bench.console.generate")}
          </Button>
        </div>
        {selectionNote && <p className="jc-lab-selection-note">{selectionNote}</p>}
      </header>

      <div className={`jc-imagelab-stage${showMask ? " is-editing" : ""}`}>
        {busy && <div className="jc-imagelab-skeleton" />}
        {showMask && stageCaption && <div className="jc-imagelab-caption">{stageCaption}</div>}
        {pictureIntent === "edit" && editUrl && (
          <div className={showMask ? "jc-mask-host" : "jc-mask-host is-off"} aria-hidden={showMask ? undefined : true} inert={showMask ? undefined : true}>
            <MaskPainter ref={maskRef} url={editUrl} disabled={busy || !showMask} onEditsChange={markMask} />
          </div>
        )}
        {!busy && !showMask && stageUrl && (
          <div className="jc-imagelab-preview">
            <img src={stageUrl} alt={stageCaption || t("bench.console.imagePreview")} />
            <div className="jc-imagelab-preview-bar">
              {stageCaption ? <div className="jc-imagelab-caption">{stageCaption}</div> : <span />}
              <div className="jc-imagelab-preview-actions">
                {viewingResult && (
                  <>
                    <button type="button" className="jc-imagelab-keep" disabled={busy || !modelReady} onClick={() => replayResult(focusId)}>{t("bench.console.regenerate")}</button>
                    <button type="button" className="jc-imagelab-keep" disabled={busy} onClick={() => continueFromResult(focusId)}>{t("bench.console.continueFrom")}</button>
                    <button type="button" className="jc-imagelab-keep" disabled={busy} onClick={() => branchFromResult(focusId)}>{t("bench.console.branch")}</button>
                  </>
                )}
                {viewingResult && pictureIntent === "generate" && (
                  sources.includes(focusId)
                    ? <span className="jc-imagelab-keep">{t("bench.console.inReferences")}</span>
                    : <button type="button" className="jc-imagelab-keep" onClick={() => keepReference(focusId)}>{t("bench.console.useAsReference")}</button>
                )}
                {pictureIntent === "edit" && focusId !== editId && (
                  <button type="button" className="jc-imagelab-keep" onClick={() => makeEditTarget(focusId)}>{t("bench.console.editThis")}</button>
                )}
                <a className="jc-imagelab-download" href={stageUrl} download="jucode-image.png" aria-label={t("bench.console.download")}>
                  <Download size={14} />
                </a>
              </div>
            </div>
          </div>
        )}
        {!busy && !stageUrl && !showMask && (
          <Text size="2" color="gray">
            {pictureIntent === "edit" ? t("bench.console.stageEmptyEdit") : t("bench.console.stageEmpty")}
          </Text>
        )}
      </div>

      <footer className="jc-imagelab-dock">
        {sizeHint && sizeHint !== error && <Text size="2" color="red">{sizeHint}</Text>}
        {error && <Text size="2" color="red">{error}</Text>}
        <textarea
          className="jc-imagelab-prompt"
          rows={2}
          placeholder={pictureIntent === "edit" ? t("bench.console.promptEdit") : sources.length > 0 ? t("bench.console.promptWithRefs") : t("bench.console.promptGenerate")}
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          onKeyDown={onPromptKeyDown}
        />
        <div className="jc-imagelab-tools">
          <SegmentedControl.Root size="2" value={pictureIntent} onValueChange={(value) => {
            const next = value as "generate" | "edit";
            setPictureIntent(next);
            setMaskOpen(false);
            if (next !== "edit") {
              markMask(false);
              maskRef.current?.clear();
              if (stageFrom === "edit") {
                if (editId && sources.includes(editId)) setStageFrom("source");
                else if (editId && results.some((item) => item.images.includes(editId))) setStageFrom("result");
                else {
                  setFocusId(sources[0] || resultIds[0] || "");
                  setStageFrom(sources[0] ? "source" : "result");
                }
              }
            }
            if (next === "edit") {
              const target = editId || focusId || sources[0] || "";
              if (target) adoptEditTarget(target);
            }
          }}>
            <SegmentedControl.Item value="generate">{t("bench.console.intentGenerate")}</SegmentedControl.Item>
            <SegmentedControl.Item value="edit">{t("bench.console.intentEdit")}</SegmentedControl.Item>
          </SegmentedControl.Root>
          <Select.Root
            size="2"
            value={usingCustom ? "custom" : size}
            onValueChange={(value) => {
              if (value === "custom") {
                setCustomMode(true);
                if (!sizePolicy.presets.some((item) => item.size === customSize)) setSize(customSize);
                return;
              }
              setCustomMode(false);
              setSize(value);
            }}
          >
            <Select.Trigger />
            <Select.Content position="popper">
              {sizePolicy.presets.map((item) => (
                <Select.Item key={item.size} value={item.size}>{item.size === "auto" ? t("bench.console.sizeAuto") : t("bench.console.sizeLabel", { label: item.label })}</Select.Item>
              ))}
              {(sizePolicy.flexible || usingCustom) && <Select.Item value="custom">{t("bench.console.sizeCustom")}</Select.Item>}
            </Select.Content>
          </Select.Root>
          {usingCustom && (
            <div className="jc-imagelab-custom">
              <input inputMode="numeric" aria-label={t("bench.console.width")} value={customW} onChange={(e) => { const next = e.target.value.replace(/\D/g, ""); setCustomW(next); setSize(`${next}x${customH}`); }} />
              <span>×</span>
              <input inputMode="numeric" aria-label={t("bench.console.height")} value={customH} onChange={(e) => { const next = e.target.value.replace(/\D/g, ""); setCustomH(next); setSize(`${customW}x${next}`); }} />
            </div>
          )}
          <Select.Root value={quality} onValueChange={setQuality} size="2">
            <Select.Trigger />
            <Select.Content position="popper">
              {imageQualityOptions(model).map((item) => <Select.Item key={item} value={item}>{t("bench.console.quality", { value: item })}</Select.Item>)}
            </Select.Content>
          </Select.Root>
          <Select.Root value={background} onValueChange={(value) => {
            setBackground(value);
            if (value === "transparent" && outputFormat === "jpeg") setOutputFormat("png");
          }} size="2">
            <Select.Trigger />
            <Select.Content position="popper">
              <Select.Item value="auto">{t("bench.console.bgAuto")}</Select.Item>
              <Select.Item value="opaque">{t("bench.console.bgOpaque")}</Select.Item>
              <Select.Item value="transparent">{t("bench.console.bgTransparent")}</Select.Item>
            </Select.Content>
          </Select.Root>
          <Select.Root value={outputFormat} onValueChange={(value) => {
            setOutputFormat(value);
            if (value === "jpeg" && background === "transparent") setBackground("opaque");
          }} size="2">
            <Select.Trigger />
            <Select.Content position="popper">
              <Select.Item value="png">PNG</Select.Item>
              <Select.Item value="jpeg">JPEG</Select.Item>
              <Select.Item value="webp">WebP</Select.Item>
            </Select.Content>
          </Select.Root>
          {pictureIntent === "edit" && imageSupportsFidelity(model) && (
            <Select.Root value={fidelity} onValueChange={setFidelity} size="2">
              <Select.Trigger />
              <Select.Content position="popper">
                <Select.Item value="high">{t("bench.console.fidelityHigh")}</Select.Item>
                <Select.Item value="low">{t("bench.console.fidelityLow")}</Select.Item>
              </Select.Content>
            </Select.Root>
          )}
          {pictureIntent === "edit" && (
            <button
              type="button"
              className={maskOpen || maskDirty ? "is-on" : ""}
              disabled={!editId}
              onClick={() => {
                if (!editId) return;
                setFocusId(editId);
                setStageFrom("edit");
                setMaskOpen((open) => !open);
              }}
            >{maskDirty ? t("bench.console.maskActive") : t("bench.console.mask")}</button>
          )}
          {pictureIntent === "edit" && maskDirty && !maskOpen && (
            <button type="button" onClick={() => maskRef.current?.clear()}>{t("bench.console.clearMask")}</button>
          )}
        </div>
        <div className="jc-imagelab-lists">
          <section>
            {pictureIntent === "edit" ? (
              <>
                <div className="jc-imagelab-list-label">
                  <span>{t("bench.console.editTarget")}</span>
                  <span className="jc-imagelab-list-hint">{t("bench.console.editTargetHint")}</span>
                </div>
                <div className="jc-imagelab-strip">
                  <label className="jc-imagelab-thumb jc-imagelab-add" aria-label={t("bench.console.uploadEditTarget")}>
                    <ImagePlus size={16} />
                    <input type="file" accept="image/*" hidden onChange={pickSources} />
                  </label>
                  {editId && (
                    <span className="jc-imagelab-refitem">
                      <button type="button" className={`jc-imagelab-thumb${viewingEdit ? " is-on" : ""}`} onClick={() => { setStageFrom("edit"); setFocusId(editId); }} aria-label={t("bench.console.viewEditTarget")}>
                        {preview(editId) && <img src={preview(editId)} alt="" />}
                        <span className="jc-imagelab-badge">{t("bench.console.edit")}</span>
                      </button>
                      <button type="button" className="jc-imagelab-refx" aria-label={t("bench.console.removeEditTarget")} onClick={() => {
                        markMask(false);
                        maskRef.current?.clear();
                        setEditId("");
                        setMaskOpen(false);
                        if (stageFrom === "edit" && focusId === editId) {
                          setFocusId(resultIds[0] || sources[0] || "");
                          setStageFrom(resultIds[0] ? "result" : sources[0] ? "source" : "edit");
                        }
                      }}>
                        <X size={12} />
                      </button>
                    </span>
                  )}
                </div>
              </>
            ) : (
              <>
                <div className="jc-imagelab-list-label">
                  <span>{t("bench.console.references", { count: sources.length, limit: refLimit || 0 })}</span>
                  <span className="jc-imagelab-list-hint">{t("bench.console.referencesHint")}</span>
                </div>
                <div className="jc-imagelab-strip">
                  <label className="jc-imagelab-thumb jc-imagelab-add" aria-label={t("bench.console.uploadReference")}>
                    <ImagePlus size={16} />
                    <input type="file" accept="image/*" multiple hidden onChange={pickSources} />
                  </label>
                  {sources.map((id) => (
                    <span key={id} className="jc-imagelab-refitem">
                      <button type="button" className={`jc-imagelab-thumb${viewingSource && focusId === id ? " is-on" : ""}`} onClick={() => { setStageFrom("source"); setFocusId(id); }} aria-label={t("bench.console.viewReference")}>
                        {preview(id) && <img src={preview(id)} alt="" />}
                      </button>
                      <button type="button" className="jc-imagelab-refx" aria-label={t("bench.console.removeReference")} onClick={() => {
                        const nextSources = sources.filter((item) => item !== id);
                        setSources(nextSources);
                        if (stageFrom === "source" && focusId === id) {
                          setFocusId(nextSources[0] || resultIds[0] || "");
                          setStageFrom(nextSources[0] ? "source" : "result");
                        }
                      }}>
                        <X size={12} />
                      </button>
                    </span>
                  ))}
                </div>
              </>
            )}
          </section>
          <section>
            <div className="jc-imagelab-list-label">
              <span>{t("bench.console.results")}</span>
              <span className="jc-imagelab-list-hint">{t("bench.console.resultsHint")}</span>
            </div>
            <div className="jc-imagelab-strip">
              {resultIds.length === 0 && <span className="jc-imagelab-list-empty">{t("bench.console.noResults")}</span>}
              {resultIds.map((id, index) => (
                <button
                  key={`${id}-${index}`}
                  type="button"
                  className={`jc-imagelab-thumb${viewingResult && focusId === id ? " is-on" : ""}`}
                  onClick={() => { setStageFrom("result"); setFocusId(id); setMaskOpen(false); }}
                  aria-label={t("bench.console.viewResult", { n: index + 1 })}
                >
                  {preview(id) && <img src={preview(id)} alt="" />}
                </button>
              ))}
            </div>
          </section>
        </div>
      </footer>
    </div>
    </div>
  );
}
