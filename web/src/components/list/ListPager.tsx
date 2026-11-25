import { Button, IconButton, Text } from "@radix-ui/themes";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";
import { localeTag } from "../../i18n";

// ListPager is the single pagination footer for every list shell. It reads
// 「1–50 / 共 361」 when the server reports a total and 「已加载 50 · 还有下一页」
// when it does not, so a page never shows a fabricated total.
export function ListPager({
  page,
  pageSize,
  total,
  loaded,
  hasPrev,
  hasNext,
  onPage,
  disabled,
  size = "1",
}: {
  page: number;
  pageSize: number;
  /** Server total; pass null when the endpoint does not report one. */
  total: number | null;
  /** Items on the current page (used when total is null). */
  loaded: number;
  hasPrev: boolean;
  hasNext: boolean;
  onPage: (page: number) => void;
  disabled?: boolean;
  size?: "1" | "2";
}) {
  const { t } = useTranslation();
  const known = total !== null && total >= 0;
  const start1 = known ? (total === 0 ? 0 : page * pageSize + 1) : page * pageSize + 1;
  const end1 = known ? Math.min((page + 1) * pageSize, total) : page * pageSize + loaded;
  const pages = known ? Math.max(1, Math.ceil(total / pageSize)) : null;

  let summary: string;
  if (known) summary = total === 0 ? t("keys.shared.pager.zero") : t("keys.shared.pager.range", { start: start1, end: end1, total: total.toLocaleString(localeTag()) });
  else if (loaded === 0) summary = t("keys.shared.pager.zero");
  else summary = `${t("keys.shared.pager.loaded", { start: start1, end: end1 })}${hasNext ? t("keys.shared.pager.hasNext") : ""}`;

  return (
    <div className="jc-list-pager">
      <Text size="1" color="gray" className="jc-list-pager-summary jc-num">{summary}</Text>
      <div className="jc-list-pager-nav">
        <IconButton size={size} variant="soft" color="gray" aria-label={t("keys.shared.pager.prev")} disabled={!hasPrev || disabled}
          onClick={() => onPage(Math.max(0, page - 1))}>
          <ChevronLeft size={14} />
        </IconButton>
        <Button size={size} variant="ghost" color="gray" disabled className="jc-list-pager-page jc-num">
          {page + 1}{pages !== null ? ` / ${pages}` : ""}
        </Button>
        <IconButton size={size} variant="soft" color="gray" aria-label={t("keys.shared.pager.next")} disabled={!hasNext || disabled}
          onClick={() => onPage(page + 1)}>
          <ChevronRight size={14} />
        </IconButton>
      </div>
    </div>
  );
}
