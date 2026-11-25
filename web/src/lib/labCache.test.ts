import { describe, expect, it } from "vitest";

import {
  chatTitle,
  forkTitle,
  deleteChat,
  fitChatCache,
  fitImageCache,
  loadChatCache,
  loadImageCache,
  saveChatCache,
  saveImageCache,
  upsertChat,
  type StoredChat,
  type StoredImageResult,
} from "./labCache";

function memoryStorage(max = Infinity): Storage {
  const data = new Map<string, string>();
  return {
    get length() { return data.size; },
    clear: () => data.clear(),
    getItem: (key) => data.get(key) ?? null,
    key: (index) => [...data.keys()][index] ?? null,
    removeItem: (key) => { data.delete(key); },
    setItem: (key, value) => {
      const next = new Map(data);
      next.set(key, value);
      let size = 0;
      for (const v of next.values()) size += v.length;
      if (size > max) {
        const err = new Error("quota");
        err.name = "QuotaExceededError";
        throw err;
      }
      data.set(key, value);
    },
  };
}

function chat(id: string, content: string, extra?: Partial<StoredChat>): StoredChat {
  return {
    title: "",
    updatedAt: 1,
    model: "m",
    groupId: "_auto",
    effort: "default",
    customEffort: "",
    ...extra,
    id,
    messages: extra?.messages ?? [{ role: "user", content }],
  };
}

describe("lab chat cache", () => {
  it("names a thread from the first user line", () => {
    expect(chatTitle([{ role: "user", content: "  你好\n世界  " }])).toBe("你好 世界");
    expect(chatTitle([{ role: "user", content: "", images: ["data:image/png;base64,aa"] }])).toBe("图片对话");
    expect(forkTitle("雨夜霓虹街道", "branch")).toBe("雨夜霓虹街道 分支");
    expect(forkTitle("雨夜霓虹街道 副本", "branch")).toBe("雨夜霓虹街道 分支");
  });

  it("keeps a fork title instead of renaming from the first line", () => {
    const storage = memoryStorage();
    const saved = upsertChat(storage, "u1", chat("a", "第一段", { title: "第一段 副本", titleFrozen: true }));
    expect(saved.chats[0].title).toBe("第一段 副本");
    expect(saved.chats[0].titleFrozen).toBe(true);
    const rewritten = saveChatCache(storage, "u1", saved);
    expect(rewritten.chats[0].title).toBe("第一段 副本");
  });

  it("renames a thread whose title only looks like a fork", () => {
    const storage = memoryStorage();
    const saved = upsertChat(storage, "u1", chat("a", "改过的第一句", { title: "第一段 副本" }));
    expect(saved.chats[0].title).toBe("改过的第一句");
  });

  it("keeps prior threads when a new one is saved", () => {
    const storage = memoryStorage();
    upsertChat(storage, "u1", chat("a", "第一段"));
    const saved = upsertChat(storage, "u1", chat("b", "第二段"));
    expect(saved.activeId).toBe("b");
    expect(saved.chats.map((c) => c.id)).toEqual(["b", "a"]);
    expect(loadChatCache(storage, "u1").chats[1].title).toBe("第一段");
  });

  it("does not store an empty draft as a thread", () => {
    const storage = memoryStorage();
    upsertChat(storage, "u1", chat("a", "留下"));
    const saved = upsertChat(storage, "u1", chat("b", "   "));
    expect(saved.chats.map((c) => c.id)).toEqual(["a"]);
    expect(saved.activeId).toBe("b");
  });

  it("restores the previous cache when the new payload still does not fit", () => {
    const storage = memoryStorage();
    upsertChat(storage, "u1", chat("a", "留下"));
    const before = storage.getItem("jucode-lab:v1:u1:chats");
    const flaky: Storage = {
      ...storage,
      setItem: (key, value) => {
        if (value.includes("放不下")) {
          const err = new Error("quota");
          err.name = "QuotaExceededError";
          throw err;
        }
        storage.setItem(key, value);
      },
    };
    upsertChat(flaky, "u1", chat("b", "放不下"));
    expect(storage.getItem("jucode-lab:v1:u1:chats")).toBe(before);
    expect(loadChatCache(storage, "u1").chats.map((c) => c.id)).toEqual(["a"]);
  });

  it("deletes one thread and moves the active pointer", () => {
    const storage = memoryStorage();
    upsertChat(storage, "u1", chat("a", "甲"));
    upsertChat(storage, "u1", chat("b", "乙"));
    const left = deleteChat(storage, "u1", "b");
    expect(left.chats.map((c) => c.id)).toEqual(["a"]);
    expect(left.activeId).toBe("a");
  });

  it("drops an older thread before forgetting its pictures", () => {
    const fat = "x".repeat(400);
    const input = {
      activeId: "new",
      chats: [
        chat("new", "最新", { updatedAt: 3, messages: [{ role: "user", content: "最新", images: [fat] }] }),
        chat("old", "旧的文字", { updatedAt: 1, messages: [{ role: "user", content: "旧的文字", images: [fat] }] }),
      ],
    };
    const kept = { activeId: "new", chats: [input.chats[0]] };
    const budget = new TextEncoder().encode(JSON.stringify(kept)).length + 16;
    const fitted = fitChatCache(input, budget);
    expect(fitted.chats.map((item) => item.id)).toEqual(["new"]);
    expect(fitted.chats[0].messages[0].images?.[0]).toBe(fat);
  });

  it("does not forget picture ids to make a chat fit", () => {
    const storage = memoryStorage(500);
    upsertChat(storage, "u1", chat("a", "短", { messages: [{ role: "user", content: "短", images: ["asset-a"] }] }));
    const before = storage.getItem("jucode-lab:v1:u1:chats");
    upsertChat(storage, "u1", chat("a", "x".repeat(2000), { messages: [{ role: "user", content: "x".repeat(2000), images: ["asset-a"] }] }));
    expect(storage.getItem("jucode-lab:v1:u1:chats")).toBe(before);
    expect(loadChatCache(storage, "u1").chats[0]?.messages[0].images).toEqual(["asset-a"]);
  });
});

describe("lab image cache", () => {
  it("drops the oldest batch when the payload is over budget", () => {
    const img = (id: string, createdAt: number): StoredImageResult => ({
      id, prompt: id, model: "m", images: ["y".repeat(400)], mode: "edit", ms: 1, createdAt,
    });
    const two = {
      activeId: "b",
      sessions: [
        { id: "a", title: "a", updatedAt: 1, model: "m", groupId: "", size: "1024x1024", count: "1", prompt: "a", sources: [], editTarget: "", results: [img("a", 1)] },
        { id: "b", title: "b", updatedAt: 2, model: "m", groupId: "", size: "1024x1024", count: "1", prompt: "b", sources: [], editTarget: "", results: [img("b", 2)] },
      ],
    };
    const one = { activeId: "b", sessions: [two.sessions[1]] };
    const oneBytes = new TextEncoder().encode(JSON.stringify(one)).length;
    const fitted = fitImageCache(two, oneBytes + 80);
    expect(fitted.sessions.map((item) => item.id)).toEqual(["b"]);
  });

  it("keeps the previous record when a write does not fit", () => {
    const session = {
      id: "a", title: "a", updatedAt: 1, model: "m", groupId: "", size: "1024x1024", count: "1", prompt: "短", sources: ["asset-a"], editTarget: "edit-1", titleFrozen: true, results: [],
    };
    const storage = memoryStorage(JSON.stringify({ activeId: "a", sessions: [session] }).length + 40);
    saveImageCache(storage, "u1", { activeId: "a", sessions: [session] });
    const before = storage.getItem("jucode-lab:v1:u1:images");
    saveImageCache(storage, "u1", { activeId: "a", sessions: [{ ...session, prompt: "x".repeat(400), sources: ["asset-a"] }] });
    expect(storage.getItem("jucode-lab:v1:u1:images")).toBe(before);
    expect(loadImageCache(storage, "u1").sessions[0]?.sources).toEqual(["asset-a"]);
    expect(loadImageCache(storage, "u1").sessions[0]?.editTarget).toBe("edit-1");
    expect(loadImageCache(storage, "u1").sessions[0]?.titleFrozen).toBe(true);
  });
});
