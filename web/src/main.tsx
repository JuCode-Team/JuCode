import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { Theme } from "@radix-ui/themes";

import App from "./App";
// Initializes i18next (zh default, English opt-in via localStorage) before
// any component calls useTranslation.
import "./i18n";
import { useThemeStore } from "./store/theme";
import { initRUM } from "./lib/rum";
import { initAuthLifecycle } from "./store/auth";
import { queryClient } from "./lib/queryClient";

// Self-hosted brand faces:
// Space Grotesk (display, latin), Noto Sans SC (body, CJK subsets are
// unicode-range split so browsers only fetch the slices they render),
// IBM Plex Mono (data/labels, latin). Replaces the render-blocking
// Google Fonts @import that used to live in index.css.
import "@fontsource/space-grotesk/latin-400.css";
import "@fontsource/space-grotesk/latin-500.css";
import "@fontsource/space-grotesk/latin-600.css";
import "@fontsource/space-grotesk/latin-700.css";
import "@fontsource/noto-sans-sc/400.css";
import "@fontsource/noto-sans-sc/500.css";
import "@fontsource/noto-sans-sc/600.css";
import "@fontsource/noto-sans-sc/700.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-500.css";
import "@fontsource/ibm-plex-mono/latin-600.css";

import "@radix-ui/themes/styles.css";
import "./index.css";
// Domain-split stylesheet modules (extracted from the former 6k-line
// index.css monolith; import order preserves the original cascade).
import "./styles/legacy-console.css";
import "./styles/components.css";
import "./styles/console-radix.css";
import "./styles/auth.css";
import "./styles/endpoints.css";
import "./styles/responsive-legacy.css";
import "./styles/migration.css";
import "./styles/landing.css";
import "./styles/catalog.css";
import "./styles/lab.css";
import "./styles/console-foundation.css";
import "./styles/console-shell.css";
import "./styles/console-overview.css";
import "./styles/ledger.css";
import "./styles/console-personal.css";
import "./styles/forms.css";
import "./styles/list-shell.css";
import "./responsive-components.css";

// Decide whether to honor a persisted login or treat it as expired.
// Runs before <App /> mounts so the first route decision sees the
// reconciled auth state. See store/auth.ts initAuthLifecycle for rules.
initAuthLifecycle();

// Wire web-vitals before React mounts so we don't miss the early
// metrics (TTFB / FCP).
initRUM();

function ThemedApp() {
  const appearance = useThemeStore((s) => s.appearance);
  return (
    <Theme accentColor="violet" grayColor="mauve" radius="medium" appearance={appearance} hasBackground={false}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </Theme>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemedApp />
    </QueryClientProvider>
  </StrictMode>,
);
