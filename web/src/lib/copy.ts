// Cross-context copy-to-clipboard.
//
// navigator.clipboard.writeText is only available in secure contexts
// (HTTPS or localhost). Our staging deploys can run on raw IPs over
// HTTP, where the Clipboard API silently rejects writes. We fall back
// to the venerable document.execCommand('copy') + offscreen <textarea>
// trick so the copy buttons still work without a TLS certificate.
//
// Returns true on success, false on every failure path so the caller
// can show an "复制失败，请手动选择" toast.
export async function copyText(value: string): Promise<boolean> {
  if (!value) return false;

  // 1) Modern path — available on HTTPS / localhost.
  if (typeof navigator !== "undefined" && navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(value);
      return true;
    } catch {
      // fall through to the legacy fallback rather than swallow silently
    }
  }

  // 2) Legacy fallback — works on plain HTTP and inside iframes that
  // can't access the modern Clipboard API. We park an offscreen
  // textarea, select its contents, then run execCommand('copy').
  if (typeof document === "undefined") return false;
  try {
    const ta = document.createElement("textarea");
    ta.value = value;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "-9999px";
    ta.style.left = "-9999px";
    ta.style.opacity = "0";
    ta.style.pointerEvents = "none";
    document.body.appendChild(ta);
    const prevSel = document.activeElement as HTMLElement | null;
    ta.focus();
    ta.select();
    ta.setSelectionRange(0, value.length);
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    if (prevSel && typeof prevSel.focus === "function") prevSel.focus();
    return ok;
  } catch {
    return false;
  }
}
