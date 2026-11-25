import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { lazy, Suspense } from "react";
import type { JSX, ReactNode } from "react";

// Layouts + shell stay eager — every route mounts inside one of these.
import AuthLayout from "./layouts/AuthLayout";
import RootLayout from "./layouts/RootLayout";
import ConsoleLayout from "./layouts/ConsoleLayout";
import ScrollToTop from "./components/ScrollToTop";
import LoadingSplash from "./components/LoadingSplash";
import { AppErrorBoundary, GlobalErrorToast } from "./components/AppErrorBoundary";
import { RouteSkeleton } from "./components/ConsoleSkeleton";

// Only Home + the auth pages stay eager. Home is the canonical landing
// route, auth pages are tiny and we don't want a Suspense flash on the
// login→register hop. Everything else — including the marketing
// /models, /endpoints, /status surfaces — lazy-loads so the main
// chunk stays small for the most-common "land on /" visitor.
import Home from "./pages/Home";
import Login from "./pages/auth/Login";
import Register from "./pages/auth/Register";
import VerifyEmail from "./pages/auth/VerifyEmail";
import ForgotPassword from "./pages/auth/ForgotPassword";
import ResetPassword from "./pages/auth/ResetPassword";
import CliOAuth from "./pages/auth/CliOAuth";

const PublicModels = lazy(() => import("./pages/PublicModels"));
const Endpoints = lazy(() => import("./pages/Endpoints"));
const Status = lazy(() => import("./pages/Status"));

// All console pages live in this one async module. First visit to /me or
// /admin downloads them together; later tab switches skip JS fetches.
const ConsolePages = lazy(() => import("./config/consolePages"));

import { useAuthStore } from "./store/auth";
import { canAccessConsoleRoute, consoleRoutes, type ConsoleAccess } from "./config/consoleRoutes";
import TotpStepUpDialog from "./components/TotpStepUpDialog";

function RequireAuth({ children, adminOnly = false }: { children: JSX.Element; adminOnly?: boolean }) {
  const user = useAuthStore((s) => s.user);
  const tok = useAuthStore((s) => s.tokens);
  const location = useLocation();
  if (!user || !tok) {
    const next = `${location.pathname}${location.search}`;
    return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />;
  }
  if (adminOnly && user.role !== "admin") return <Navigate to="/me" replace />;
  return children;
}

function RequireConsoleAccess({ access, children }: { access: ConsoleAccess; children: JSX.Element }) {
  const user = useAuthStore((s) => s.user);
  const tok = useAuthStore((s) => s.tokens);
  if (!user || !tok) return <Navigate to="/login" replace />;
  if (!canAccessConsoleRoute(access, user.role, user.roles)) return <Navigate to="/me" replace />;
  return children;
}

function LazyView({ children, fallback = null }: { children: JSX.Element; fallback?: ReactNode }) {
  const location = useLocation();
  return (
    <AppErrorBoundary resetKey={location.pathname}>
      <Suspense fallback={fallback}>{children}</Suspense>
    </AppErrorBoundary>
  );
}

export default function App() {
  return (
    <>
    <ScrollToTop />
    <LoadingSplash />
    <GlobalErrorToast />
    <TotpStepUpDialog />
    <AppErrorBoundary>
      <Routes>
        {/* Auth pages — bare layout */}
        <Route element={<AuthLayout />}>
          <Route path="/login" element={<Login />} />
          <Route path="/register" element={<Register />} />
          <Route path="/verify-email" element={<VerifyEmail />} />
          <Route path="/forgot-password" element={<ForgotPassword />} />
          <Route path="/reset-password" element={<ResetPassword />} />
          <Route path="/cli/oauth" element={<RequireAuth><CliOAuth /></RequireAuth>} />
        </Route>

      {/* Everything else — Navbar + Footer */}
      <Route element={<RootLayout />}>
        <Route path="/" element={<Home />} />
        <Route path="/models" element={<LazyView><PublicModels /></LazyView>} />
        <Route path="/endpoints" element={<LazyView><Endpoints /></LazyView>} />
        <Route path="/status" element={<LazyView><Status /></LazyView>} />

        {/* Console (auth required) — Navbar + sidebar + Footer */}
        <Route element={<RequireAuth><ConsoleLayout /></RequireAuth>}>
          {consoleRoutes.map(({ path, access }) => (
            <Route
              key={path}
              path={path}
              element={(
                <RequireConsoleAccess access={access}>
                  <LazyView fallback={<RouteSkeleton />}>
                    <ConsolePages />
                  </LazyView>
                </RequireConsoleAccess>
              )}
            />
          ))}
        </Route>

        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
      </Routes>
    </AppErrorBoundary>
    </>
  );
}
