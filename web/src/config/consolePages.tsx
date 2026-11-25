import { type ComponentType } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { consoleRoutes, getConsoleRoute } from "./consoleRoutes";

// Eager glob inside this module — App.tsx loads it once via React.lazy,
// so the landing page never pays for console JS. After the first console
// visit, tab switches skip further chunk fetches.
const pageModules = import.meta.glob<{ default: ComponentType }>(
  [
    "../pages/user/*.tsx",
    "../pages/cs/*.tsx",
    "../pages/agent/*.tsx",
    "../pages/admin/*.tsx",
  ],
  { eager: true },
);

const pages: Record<string, ComponentType> = {};
for (const route of consoleRoutes) {
  const mod = pageModules[`../pages/${route.page}.tsx`];
  if (!mod) throw new Error(`Missing console page module: ${route.page}`);
  pages[route.path] = mod.default;
}

export default function ConsolePages() {
  const route = getConsoleRoute(useLocation().pathname);
  if (!route) return <Navigate to="/me" replace />;
  const Page = pages[route.path];
  if (!Page) return <Navigate to="/me" replace />;
  return <Page />;
}
