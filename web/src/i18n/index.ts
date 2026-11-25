import i18n from "i18next";
import { initReactI18next } from "react-i18next";

import { en } from "./en";
import { zh } from "./zh";

export const languages = ["zh", "en"] as const;
export type Language = (typeof languages)[number];

// Chinese stays the default: an unset preference (first visit, cleared
// storage) must land on zh, not on the browser's Accept-Language.
export const DEFAULT_LANGUAGE: Language = "zh";
const STORAGE_KEY = "jucode-lang";

function readInitial(): Language {
  if (typeof window === "undefined") return DEFAULT_LANGUAGE;
  return window.localStorage?.getItem(STORAGE_KEY) === "en" ? "en" : DEFAULT_LANGUAGE;
}

function applyToDom(language: Language) {
  if (typeof document === "undefined") return;
  document.documentElement.lang = language === "en" ? "en" : "zh-CN";
}

void i18n.use(initReactI18next).init({
  resources: {
    zh: { translation: zh },
    en: { translation: en },
  },
  lng: readInitial(),
  fallbackLng: DEFAULT_LANGUAGE,
  supportedLngs: languages,
  // React escapes for us; i18next's own escaping would double-encode.
  interpolation: { escapeValue: false },
});

applyToDom(i18n.language as Language);

export function setLanguage(language: Language) {
  window.localStorage.setItem(STORAGE_KEY, language);
  applyToDom(language);
  void i18n.changeLanguage(language);
}

export function currentLanguage(): Language {
  return i18n.language === "en" ? "en" : "zh";
}

export default i18n;

// BCP 47 tag for Intl / toLocaleString, following the UI language.
export function localeTag(): string {
  return currentLanguage() === "en" ? "en-US" : "zh-CN";
}
