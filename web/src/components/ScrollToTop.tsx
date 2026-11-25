import { useEffect } from "react";
import { useLocation } from "react-router-dom";

/**
 * Resets window scroll on every pathname change. SPA navigation does not
 * naturally reset scroll position, so a long admin page followed by a jump
 * to the home page would otherwise leave the user mid-hero-section.
 *
 * When the target carries a hash (e.g. /models#pricing) we instead scroll to
 * that element. The page may still be lazy-loading when the effect first
 * runs, so we retry across a few animation frames until the anchor mounts.
 */
export default function ScrollToTop() {
  const { pathname, hash } = useLocation();
  useEffect(() => {
    if (hash) {
      let frames = 0;
      let raf = 0;
      const tryScroll = () => {
        const el = document.querySelector(hash);
        if (el) {
          el.scrollIntoView({ behavior: "auto", block: "start" });
          return;
        }
        if (frames++ < 20) raf = requestAnimationFrame(tryScroll);
      };
      raf = requestAnimationFrame(tryScroll);
      return () => cancelAnimationFrame(raf);
    }
    window.scrollTo(0, 0);
  }, [pathname, hash]);
  return null;
}
