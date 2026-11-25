import { useEffect, useState } from "react";
import { useSite } from "../store/site";
import { deployment } from "../config/env";

/**
 * Full-screen brand splash shown on the very first session visit. It only
 * covers the font swap: it dismisses as soon as document.fonts is ready,
 * and never later than 300ms, then fades over --dur-base (180ms). Subsequent
 * navigations skip it via sessionStorage.
 */
const FONT_WAIT_CAP_MS = 300;
const FADE_MS = 180;
export default function LoadingSplash() {
  const site = useSite();
  const [show, setShow] = useState(() => {
    if (typeof window === "undefined") return false;
    return window.sessionStorage.getItem("jc-splash-done") !== "1";
  });
  const [fade, setFade] = useState(false);

  useEffect(() => {
    if (!show) return;
    let cancelled = false;
    const cap = new Promise<void>((r) => window.setTimeout(r, FONT_WAIT_CAP_MS));
    const fontsReady = (document as Document & { fonts?: { ready: Promise<unknown> } }).fonts?.ready
      ?? Promise.resolve();
    Promise.race([cap, fontsReady]).then(() => {
      if (cancelled) return;
      setFade(true);
      window.setTimeout(() => {
        if (cancelled) return;
        setShow(false);
        try { window.sessionStorage.setItem("jc-splash-done", "1"); } catch { /* ignore */ }
      }, FADE_MS);
    });
    return () => { cancelled = true; };
  }, [show]);

  if (!show) return null;
  const brand = site.site_name || deployment.siteName;

  return (
    <div className={`pn-splash ${fade ? "is-fade" : ""}`} aria-hidden>
      <div className="pn-splash-brand">{brand}</div>
      <div className="pn-splash-bar"><span /></div>
    </div>
  );
}
