import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Check, Copy, MessageCircle, X } from "lucide-react";
import { apiAssetURL } from "../api/client";
import { useSite } from "../store/site";

// 全站共用的「打开联系卡」事件：首页「开始使用」区的文字入口通过它唤起浮层，
// 避免把状态提升到 Home 再层层透传。
export const CONTACT_OPEN_EVENT = "jucode:contact-open";
export function openContactDock() {
  window.dispatchEvent(new Event(CONTACT_OPEN_EVENT));
}

function useCopy(text: string) {
  const [copied, setCopied] = useState(false);
  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      // 剪贴板不可用（非安全上下文）时保持静默，群号本身仍可手动选中复制。
    }
  }, [text]);
  return { copied, copy };
}

/**
 * 右下角常驻的联系入口。群号、二维码与是否显示由后台「系统设置」配置；
 * 滚动到页脚时自动隐藏（页脚本身承载联系方式），Escape / 点击外部关闭。
 */
export default function ContactDock() {
  const site = useSite();
  return site.contact_dock_enabled ? <ContactDockPanel /> : null;
}

function ContactDockPanel() {
  const { t } = useTranslation();
  const site = useSite();
  const group = site.contact_group;
  const groupQR = site.contact_group_qr_url;
  const label = site.contact_group_label || t("pub.contact.group");
  const [open, setOpen] = useState(false);
  const [nearFooter, setNearFooter] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const panelId = useId();
  const { copied, copy } = useCopy(group);

  useEffect(() => {
    const footer = document.querySelector("footer");
    if (!footer || !("IntersectionObserver" in window)) return;
    const observer = new IntersectionObserver(
      ([entry]) => setNearFooter(entry.isIntersecting),
      { threshold: 0.12 },
    );
    observer.observe(footer);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    const onOpen = () => setOpen(true);
    window.addEventListener(CONTACT_OPEN_EVENT, onOpen);
    return () => window.removeEventListener(CONTACT_OPEN_EVENT, onOpen);
  }, []);

  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    const onPointer = (event: PointerEvent) => {
      if (rootRef.current && !rootRef.current.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onPointer);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointer);
    };
  }, [open]);

  const hidden = nearFooter && !open;

  return (
    <div ref={rootRef} className={`jc-contact-dock ${hidden ? "is-hidden" : ""} ${open ? "is-open" : ""}`}>
      <section
        id={panelId}
        className="jc-contact-card"
        role="dialog"
        aria-label={t("pub.contact.title")}
        aria-hidden={!open}
      >
        <header>
          <b>{t("pub.contact.title")}</b>
          <button type="button" onClick={() => setOpen(false)} aria-label={t("pub.contact.close")}>
            <X size={16} strokeWidth={1.8} />
          </button>
        </header>

        <div className="jc-contact-group">
          <div className="jc-contact-qr" aria-label={t("pub.contact.qr", { label })}>
            {groupQR ? <img src={apiAssetURL(groupQR)} alt={t("pub.contact.qr", { label })} /> : <span>{t("pub.contact.qrPending")}</span>}
          </div>
          <dl>
            {group && (
              <>
                <dt>{label}</dt>
                <dd>
                  <code>{group}</code>
                  <button type="button" onClick={copy} aria-label={t("pub.contact.copyNumber")}>
                    {copied ? <Check size={14} strokeWidth={2} /> : <Copy size={14} strokeWidth={1.8} />}
                    <span>{copied ? t("pub.common.copied") : t("pub.common.copy")}</span>
                  </button>
                </dd>
              </>
            )}
            <dd className="jc-contact-hint">{t("pub.contact.hint")}</dd>
          </dl>
        </div>

        <ul className="jc-contact-links">
          <li>
            <span>{t("pub.contact.email")}</span>
            {site.contact_email
              ? <a href={`mailto:${site.contact_email}`}>{site.contact_email}</a>
              : <em>{t("pub.contact.notSet")}</em>}
          </li>
          <li>
            <span>{t("pub.contact.ticket")}</span>
            <Link to="/me/tickets" onClick={() => setOpen(false)}>{t("pub.contact.submitTicket")}</Link>
          </li>
        </ul>
      </section>

      <button
        type="button"
        className="jc-contact-fab"
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setOpen((value) => !value)}
      >
        <MessageCircle size={16} strokeWidth={1.8} />
        <span>{t("pub.contact.fab")}</span>
      </button>
    </div>
  );
}
