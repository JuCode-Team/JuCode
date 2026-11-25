import { ArrowUpRight } from "lucide-react";
import { Link } from "react-router-dom";
import type { MarketingBannerConfig } from "../api/endpoints";

export function MarketingBanner({ config }: { config: MarketingBannerConfig }) {
  const text = config.text.trim();
  const linkUrl = config.link_url.trim();
  const content = (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      <span className="truncate">{text}</span>
      {linkUrl && <ArrowUpRight className="shrink-0" size={13} strokeWidth={1.8} aria-hidden />}
    </span>
  );

  return (
    <div className="fixed inset-x-0 top-0 z-[92] flex h-9 items-center justify-center border-b border-outline-variant bg-surface-container-lowest px-gutter text-center font-body text-body-sm text-on-surface">
      {linkUrl.startsWith("/") ? (
        <Link
          to={linkUrl}
          className="min-w-0 transition-colors hover:text-primary-bright focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-bright"
          title={text}
        >
          {content}
        </Link>
      ) : linkUrl ? (
        <a
          href={linkUrl}
          target="_blank"
          rel="noreferrer"
          className="min-w-0 transition-colors hover:text-primary-bright focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary-bright"
          title={text}
        >
          {content}
        </a>
      ) : (
        <span className="min-w-0" title={text}>{content}</span>
      )}
    </div>
  );
}
