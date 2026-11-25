// Real-user monitoring: buffer web-vitals metric emissions for this
// page-load and ship them to /v1/public/web-vitals via sendBeacon at
// pagehide / visibilitychange. Batching means at most one network
// request per page-load regardless of how many metrics fired.
//
// We intentionally do NOT use fetch() — sendBeacon survives the page
// unload that LCP / CLS reports often coincide with.

import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from "web-vitals";

type Sample = {
  name: string;
  value: number;
  rating: string;
  nav_type: string;
};

const buffer: Sample[] = [];

// Per-load session ID — randomised on bootstrap, opaque to backend.
// Lets us correlate "the LCP and CLS from the same visit" without
// holding any user identity.
const sessionId =
  typeof crypto !== "undefined" && crypto.randomUUID
    ? crypto.randomUUID()
    : Math.random().toString(36).slice(2);

function record(metric: Metric) {
  buffer.push({
    name: metric.name,
    value: metric.value,
    rating: metric.rating,
    nav_type: metric.navigationType,
  });
}

function flush() {
  if (buffer.length === 0) return;
  const payload = {
    session: sessionId,
    path: window.location.pathname,
    metrics: buffer.splice(0, buffer.length),
  };
  const body = JSON.stringify(payload);
  // sendBeacon when available; falls back to fetch keepalive.
  if (navigator.sendBeacon) {
    const blob = new Blob([body], { type: "application/json" });
    navigator.sendBeacon("/v1/public/web-vitals", blob);
    return;
  }
  // Fallback for older Safari.
  fetch("/v1/public/web-vitals", {
    method: "POST",
    body,
    headers: { "Content-Type": "application/json" },
    keepalive: true,
  }).catch(() => { /* swallow — RUM must never throw */ });
}

export function initRUM(): void {
  if (typeof window === "undefined") return;
  try {
    onLCP(record);
    onCLS(record);
    onINP(record);
    onFCP(record);
    onTTFB(record);
    // Most reliable flush time: page becomes hidden (covers tab-switch,
    // back-button, true close). Some metrics (CLS, INP) finalise at
    // this point so we must wait for it.
    window.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "hidden") flush();
    });
    // Belt-and-braces for browsers that don't fire visibilitychange
    // on close (e.g. some Safari versions).
    window.addEventListener("pagehide", flush);
  } catch {
    // Library mis-init shouldn't crash the page.
  }
}
