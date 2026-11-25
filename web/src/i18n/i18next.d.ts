import "i18next";
import type { Translations } from "./zh";

// Makes t("console.nav.overview") a compile-time checked key instead of a
// free-form string, so renamed or missing keys fail the build.
declare module "i18next" {
  interface CustomTypeOptions {
    defaultNS: "translation";
    resources: { translation: Translations };
  }
}
