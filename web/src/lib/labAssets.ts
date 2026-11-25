// Generated and uploaded lab images live in IndexedDB, not localStorage.
// The library keeps one blob per picture. Citing it again reuses that blob
// instead of asking the user to upload the same file a second time.
// Oldest pictures are dropped when the byte budget or the count cap is hit.

const DB_NAME = "jucode-lab-assets";
const DB_STORE = "assets";
export const ASSET_BUDGET = 80 * 1024 * 1024;
export const ASSET_CAP = 40;

export interface LabAssetMeta {
  id: string;
  userId: string;
  prompt: string;
  model: string;
  createdAt: number;
  bytes: number;
  mime: string;
}

interface LabAssetRecord extends LabAssetMeta {
  blob: Blob;
  thumb: Blob;
}

export function isInlineImage(value: string): boolean {
  return value.startsWith("data:") || value.startsWith("http://") || value.startsWith("https://");
}

export function trimAssetIds<T extends { id: string; createdAt: number; bytes: number }>(
  items: T[],
  budget = ASSET_BUDGET,
  cap = ASSET_CAP,
  pinned: ReadonlySet<string> = new Set(),
): string[] {
  const sorted = [...items].sort((a, b) => b.createdAt - a.createdAt);
  const drop: string[] = [];
  let used = 0;
  let kept = 0;
  for (const item of sorted) {
    if (!pinned.has(item.id) && (kept >= cap || used + item.bytes > budget)) {
      drop.push(item.id);
      continue;
    }
    kept += 1;
    used += item.bytes;
  }
  return drop;
}

export function blobToDataURL(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(blob);
  });
}

function openDb(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(DB_STORE)) {
        const store = db.createObjectStore(DB_STORE, { keyPath: "id" });
        store.createIndex("userId", "userId", { unique: false });
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function txDone(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error);
  });
}

async function makeThumb(blob: Blob): Promise<Blob> {
  try {
    const bitmap = await createImageBitmap(blob);
    const scale = Math.min(1, 240 / Math.max(bitmap.width, bitmap.height, 1));
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.round(bitmap.width * scale));
    canvas.height = Math.max(1, Math.round(bitmap.height * scale));
    canvas.getContext("2d")?.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
    bitmap.close();
    const thumb = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, "image/jpeg", 0.72));
    return thumb ?? blob;
  } catch {
    return blob;
  }
}

export async function assetBlob(id: string, kind: "blob" | "thumb" = "blob"): Promise<Blob | null> {
  const db = await openDb();
  const record = await new Promise<LabAssetRecord | undefined>((resolve, reject) => {
    const req = db.transaction(DB_STORE, "readonly").objectStore(DB_STORE).get(id);
    req.onsuccess = () => resolve(req.result as LabAssetRecord | undefined);
    req.onerror = () => reject(req.error);
  });
  db.close();
  if (!record) return null;
  return kind === "thumb" ? record.thumb : record.blob;
}

export async function deleteAssets(ids: string[]): Promise<void> {
  const unique = [...new Set(ids)].filter(Boolean);
  if (unique.length === 0) return;
  const db = await openDb();
  const tx = db.transaction(DB_STORE, "readwrite");
  const store = tx.objectStore(DB_STORE);
  for (const id of unique) store.delete(id);
  await txDone(tx);
  db.close();
}

export async function saveAsset(input: {
  userId: string;
  blob: Blob;
  prompt: string;
  model: string;
  keepIds?: readonly string[];
}): Promise<LabAssetMeta> {
  const record: LabAssetRecord = {
    id: crypto.randomUUID(),
    userId: input.userId,
    prompt: input.prompt,
    model: input.model,
    createdAt: Date.now(),
    bytes: input.blob.size,
    mime: input.blob.type || "image/png",
    blob: input.blob,
    thumb: await makeThumb(input.blob),
  };
  const db = await openDb();
  const tx = db.transaction(DB_STORE, "readwrite");
  const store = tx.objectStore(DB_STORE);
  store.put(record);
  const existing = await new Promise<LabAssetRecord[]>((resolve, reject) => {
    const req = store.index("userId").getAll(input.userId);
    req.onsuccess = () => resolve(req.result as LabAssetRecord[]);
    req.onerror = () => reject(req.error);
  });
  const pinned = new Set([record.id, ...(input.keepIds ?? [])]);
  for (const id of trimAssetIds(existing, ASSET_BUDGET, ASSET_CAP, pinned)) store.delete(id);
  await txDone(tx);
  db.close();
  const { blob: _blob, thumb: _thumb, ...meta } = record;
  return meta;
}

export function blobFromImagePayload(payload: string): Promise<Blob> {
  if (payload.startsWith("data:") || payload.startsWith("http")) {
    return fetch(payload).then((res) => res.blob());
  }
  const bytes = Uint8Array.from(atob(payload), (ch) => ch.charCodeAt(0));
  return Promise.resolve(new Blob([bytes], { type: "image/png" }));
}
