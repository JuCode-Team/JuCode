import { create } from "zustand";

export type Appearance = "light" | "dark";

const STORAGE_KEY = "jucode-theme";

function readInitial(): Appearance {
  if (typeof window === "undefined") return "dark";
  const saved = window.localStorage.getItem(STORAGE_KEY);
  return saved === "light" ? "light" : "dark";
}

function applyToDom(theme: Appearance) {
  const root = document.documentElement;
  root.classList.remove("light", "dark");
  root.classList.add(theme);
  root.dataset.theme = theme;
}

interface ThemeState {
  appearance: Appearance;
  setAppearance: (a: Appearance) => void;
  toggle: () => void;
}

export const useThemeStore = create<ThemeState>((set, get) => ({
  appearance: readInitial(),
  setAppearance(a) {
    applyToDom(a);
    window.localStorage.setItem(STORAGE_KEY, a);
    set({ appearance: a });
  },
  toggle() {
    const next: Appearance = get().appearance === "dark" ? "light" : "dark";
    get().setAppearance(next);
  },
}));
