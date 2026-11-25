import i18n from "../i18n";

// Playground history kept in this browser only. Text survives a quota
// error; picture ids stay with the thread, and a failed write leaves the
// previous record in place.

export interface StoredToolCall {
  id: string;
  name: "web_search" | "web_fetch" | "generate_image";
  detail: string;
  status: "running" | "done" | "error";
  error?: string;
  /** Short result line, e.g. "5 条结果" or the page title. */
  summary?: string;
  /** Search hits shown when the step is expanded. */
  results?: Array<{ title: string; url: string }>;
}

export interface StoredChatMessage {
  role: "user" | "assistant";
  content: string;
  images?: string[];
  reasoning?: string;
  model?: string;
  error?: boolean;
  tools?: StoredToolCall[];
}

export interface StoredChat {
  id: string;
  title: string;
  updatedAt: number;
  model: string;
  groupId: string;
  effort: string;
  customEffort: string;
  /** Set when the user copied or branched this thread. The title then stays put. */
  titleFrozen?: boolean;
  messages: StoredChatMessage[];
}

export interface ChatCache {
  activeId: string;
  chats: StoredChat[];
}

export interface StoredImageResult {
  id: string;
  prompt: string;
  model: string;
  images: string[];
  sources?: string[];
  mode: "generate" | "edit";
  ms: number;
  createdAt: number;
}

export interface ImageSession {
  id: string;
  title: string;
  updatedAt: number;
  model: string;
  groupId: string;
  size: string;
  count: string;
  prompt: string;
  sources: string[];
  /** The one picture edit mode changes. Empty while the session has no edit target. */
  editTarget: string;
  /** Set when the user copied or branched this session. The title then stays put. */
  titleFrozen?: boolean;
  results: StoredImageResult[];
}

export interface ImageCache {
  activeId: string;
  sessions: ImageSession[];
}

export type LabTab = "chat" | "image" | "tools";

export interface LabPrefs {
  /** Off: balance groups only. On: plan groups only. */
  planOnly: boolean;
  search: boolean;
  research: boolean;
}

const CHAT_BUDGET = 1_800_000;
const IMAGE_BUDGET = 1_800_000;
const MAX_CHATS = 30;

type Store = Pick<Storage, "getItem" | "setItem" | "removeItem">;

function chatKey(userId: string) {
  return `jucode-lab:v1:${userId}:chats`;
}
function imageKey(userId: string) {
  return `jucode-lab:v1:${userId}:images`;
}
function tabKey(userId: string) {
  return `jucode-lab:v1:${userId}:tab`;
}
function prefsKey(userId: string) {
  return `jucode-lab:v1:${userId}:prefs`;
}

function byteSize(value: unknown): number {
  return new TextEncoder().encode(JSON.stringify(value)).length;
}

function isQuotaError(err: unknown): boolean {
  return err instanceof DOMException
    ? err.name === "QuotaExceededError" || err.code === 22
    : err instanceof Error && err.name === "QuotaExceededError";
}

function readJSON<T>(storage: Store, key: string): T | null {
  try {
    const raw = storage.getItem(key);
    if (!raw) return null;
    return JSON.parse(raw) as T;
  } catch {
    return null;
  }
}

function writeJSON(storage: Store, key: string, value: unknown): boolean {
  const raw = JSON.stringify(value);
  const previous = storage.getItem(key);
  try {
    storage.setItem(key, raw);
    return true;
  } catch (err) {
    if (!isQuotaError(err)) return false;
    try {
      storage.removeItem(key);
      storage.setItem(key, raw);
      return true;
    } catch {
      if (previous != null) {
        try { storage.setItem(key, previous); } catch { /* previous value could not be restored */ }
      }
      return false;
    }
  }
}

// Matches the suffix in either language, so titles made before a language switch still strip cleanly.
const forkSuffix = / (副本|分支|copy|branch)$/;

export function forkTitle(base: string, kind: "copy" | "branch"): string {
  const clean = base.replace(/…$/, "").trim().replace(forkSuffix, "").slice(0, 24) || i18n.t("bench.cache.untitled");
  return `${clean} ${i18n.t(`bench.cache.${kind}`)}`;
}

export function chatTitle(messages: StoredChatMessage[]): string {
  const text = messages.find((m) => m.role === "user" && m.content.trim())?.content ?? "";
  const flat = text.trim().replace(/\s+/g, " ");
  if (flat) return flat.length > 28 ? `${flat.slice(0, 28)}…` : flat;
  if (messages.some((m) => (m.images?.length ?? 0) > 0)) return i18n.t("bench.cache.imageChat");
  return i18n.t("bench.cache.untitledChat");
}

export function resolveChatTitle(chat: Pick<StoredChat, "title" | "messages" | "titleFrozen">): string {
  if (chat.titleFrozen && chat.title.trim()) return chat.title.trim().slice(0, 32);
  return chatTitle(chat.messages);
}

function meaningful(messages: StoredChatMessage[]): boolean {
  return messages.some((m) => m.content.trim() || m.reasoning || m.error || (m.images?.length ?? 0) > 0 || (m.tools?.length ?? 0) > 0);
}

function emptyChatCache(): ChatCache {
  return { activeId: "", chats: [] };
}

/** Drop older threads, then reasoning, until the JSON fits. Picture ids stay. */
export function fitChatCache(cache: ChatCache, budget = CHAT_BUDGET): ChatCache {
  const chats = [...cache.chats]
    .sort((a, b) => b.updatedAt - a.updatedAt)
    .slice(0, MAX_CHATS)
    .map((c) => ({ ...c, messages: c.messages.map((m) => ({ ...m })) }));

  const packed = () => ({ activeId: cache.activeId, chats });
  if (byteSize(packed()) <= budget) return packed();

  while (chats.length > 1 && byteSize(packed()) > budget) {
    const dropAt = chats.findLastIndex((c) => c.id !== cache.activeId);
    chats.splice(dropAt >= 0 ? dropAt : chats.length - 1, 1);
  }
  if (byteSize(packed()) > budget && chats[0]) {
    chats[0] = {
      ...chats[0],
      messages: chats[0].messages.map((m) => ({ ...m, reasoning: undefined })),
    };
  }
  return packed();
}

export function loadChatCache(storage: Store, userId: string): ChatCache {
  const parsed = readJSON<ChatCache>(storage, chatKey(userId));
  if (!parsed || !Array.isArray(parsed.chats)) return emptyChatCache();
  return {
    activeId: typeof parsed.activeId === "string" ? parsed.activeId : "",
    chats: parsed.chats.filter((c) => c && typeof c.id === "string" && Array.isArray(c.messages)),
  };
}

export function upsertChat(storage: Store, userId: string, chat: StoredChat): ChatCache {
  const cur = loadChatCache(storage, userId);
  const rest = cur.chats.filter((c) => c.id !== chat.id);
  const chats = meaningful(chat.messages)
    ? [{ ...chat, title: resolveChatTitle(chat), updatedAt: Date.now() }, ...rest]
    : rest;
  const next = fitChatCache({ activeId: chat.id, chats });
  if (!writeJSON(storage, chatKey(userId), next)) return cur;
  return next;
}

export function deleteChat(storage: Store, userId: string, id: string): ChatCache {
  const cur = loadChatCache(storage, userId);
  const chats = cur.chats.filter((c) => c.id !== id);
  const activeId = cur.activeId === id ? (chats[0]?.id ?? "") : cur.activeId;
  const next = fitChatCache({ activeId, chats });
  writeJSON(storage, chatKey(userId), next);
  return next;
}

function emptyImageCache(): ImageCache {
  return { activeId: "", sessions: [] };
}

function imageSession(partial: Partial<ImageSession> & { id: string }): ImageSession {
  return {
    id: partial.id,
    title: partial.title || partial.prompt?.trim().slice(0, 28) || i18n.t("bench.cache.newImage"),
    updatedAt: partial.updatedAt || Date.now(),
    model: partial.model ?? "",
    groupId: partial.groupId || "",
    size: partial.size || "1024x1024",
    count: partial.count || "1",
    prompt: partial.prompt ?? "",
    sources: Array.isArray(partial.sources) ? partial.sources.filter((id) => typeof id === "string") : [],
    editTarget: typeof partial.editTarget === "string" ? partial.editTarget : "",
    titleFrozen: partial.titleFrozen === true,
    results: Array.isArray(partial.results) ? partial.results.filter((item) => item && typeof item.id === "string" && Array.isArray(item.images)) : [],
  };
}

export function fitImageCache(cache: ImageCache, budget = IMAGE_BUDGET): ImageCache {
  const sessions = [...cache.sessions]
    .sort((a, b) => b.updatedAt - a.updatedAt)
    .slice(0, MAX_CHATS);
  const packed = (): ImageCache => ({ activeId: cache.activeId, sessions });
  if (byteSize(packed()) <= budget) return packed();
  while (sessions.length > 1 && byteSize(packed()) > budget) {
    const dropAt = sessions.findLastIndex((item) => item.id !== cache.activeId);
    sessions.splice(dropAt >= 0 ? dropAt : sessions.length - 1, 1);
  }
  return packed();
}

export function loadImageCache(storage: Store, userId: string): ImageCache {
  const parsed = readJSON<ImageCache & { model?: string; groupId?: string; size?: string; count?: string; prompt?: string; results?: StoredImageResult[] }>(storage, imageKey(userId));
  if (!parsed) return emptyImageCache();
  if (Array.isArray(parsed.sessions)) {
    return {
      activeId: typeof parsed.activeId === "string" ? parsed.activeId : "",
      sessions: parsed.sessions.filter((item) => item && typeof item.id === "string").map((item) => imageSession(item)),
    };
  }
  if (!Array.isArray(parsed.results)) return emptyImageCache();
  const legacy = imageSession({
    id: "legacy",
    model: parsed.model,
    groupId: parsed.groupId,
    size: parsed.size,
    count: parsed.count,
    prompt: parsed.prompt,
    results: parsed.results,
    updatedAt: parsed.results[0]?.createdAt,
  });
  if (!legacy.prompt && legacy.results.length === 0) return emptyImageCache();
  return { activeId: legacy.id, sessions: [legacy] };
}

export function saveChatCache(storage: Store, userId: string, cache: ChatCache): ChatCache {
  const next = fitChatCache({
    activeId: cache.activeId,
    chats: cache.chats.map((chat) => ({ ...chat, title: resolveChatTitle(chat) })),
  });
  if (!writeJSON(storage, chatKey(userId), next)) return loadChatCache(storage, userId);
  return next;
}

export function saveImageCache(storage: Store, userId: string, cache: ImageCache): ImageCache {
  const next = fitImageCache(cache);
  // A failed write must leave the previous record alone. Replacing it with an
  // empty image list drops the only copy of a picture that is still a data URL.
  if (!writeJSON(storage, imageKey(userId), next)) return loadImageCache(storage, userId);
  return next;
}

export function loadLabTab(storage: Store, userId: string): LabTab {
  const raw = storage.getItem(tabKey(userId));
  if (raw === "image" || raw === "edit") return "image";
  if (raw === "tools") return "tools";
  return "chat";
}

export function saveLabTab(storage: Store, userId: string, tab: LabTab) {
  try { storage.setItem(tabKey(userId), tab); } catch { /* ignore quota */ }
}

export function loadLabPrefs(storage: Store, userId: string): LabPrefs {
  const parsed = readJSON<Partial<LabPrefs>>(storage, prefsKey(userId));
  return {
    planOnly: parsed?.planOnly === true,
    search: parsed?.search !== false,
    research: parsed?.research === true,
  };
}

export function saveLabPrefs(storage: Store, userId: string, prefs: LabPrefs) {
  writeJSON(storage, prefsKey(userId), prefs);
}
