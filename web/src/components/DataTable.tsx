// DataTable is the project-wide list shell:
//
//   ┌──────────────────────────────────────────────────────────┐
//   │  [search box]  filters...                      [refresh] │
//   ├──────────────────────────────────────────────────────────┤
//   │  col1     col2     col3 …                                │  ← sticky header
//   │  ────────────────────────────────────────────────────────│
//   │  row…                                                    │  ← virtualized rows
//   │  row…                                                    │
//   │  …                                                       │
//   ├──────────────────────────────────────────────────────────┤
//   │   1–50 / 1234        ◀  page 3 / 25  ▶                   │
//   └──────────────────────────────────────────────────────────┘
//
// It pairs with `usePagedQuery` — pass the hook's state in via props.
// The component is presentation-only; data fetching stays in the page.
//
// Virtualization choice: react-virtuoso's TableVirtuoso. Unlike
// react-window, Virtuoso supports a real <table>+<thead>+<tbody>
// structure with dynamic row heights and sticky headers out of the
// box. react-window would force a div-based layout and complicate
// every cell that uses Radix Theme tokens.

import type { CSSProperties, ReactNode } from "react";
import { Box, Card, Flex, IconButton, Skeleton, Text, TextField, Tooltip } from "@radix-ui/themes";
import { RotateCcw, Search } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ListPager } from "./list/ListPager";
import { TableVirtuoso } from "react-virtuoso";
import { ClearableTextField } from "./ClearableTextField";


export interface Column<T> {
  /** Column header — string or arbitrary React node */
  header: ReactNode;
  /** Cell renderer */
  cell: (row: T, index: number) => ReactNode;
  /** CSS width (e.g. "120px", "20%"). Optional — when omitted the
   * column sizes to content. */
  width?: string;
  /** Min width to keep header readable when content is short.
   * Defaults to "60px" for unspecified cols. */
  minWidth?: string;
  /** Right-align numeric / action columns. Defaults to "right" when
   * `type` is "number" or "actions". */
  align?: "left" | "right" | "center";
  /** Semantic column type. "number" right-aligns the column and turns on
   * tabular figures (via .jc-cell-num) so digits line up across rows.
   * "actions" right-aligns the row's action cluster (.jc-cell-actions). */
  type?: "text" | "number" | "actions";
  /** Hide column on narrow screens */
  hideOn?: "sm" | "md";
  /** Allow this cell's content to wrap onto multiple lines. Default
   * false → single-line truncate. Set true on rare prose columns. */
  wrap?: boolean;
  /** Optional key for React list reconciliation. Defaults to header */
  key?: string;
}

export interface DataTableProps<T> {
  // From usePagedQuery
  items: T[];
  total: number;
  page: number;
  setPage: (n: number) => void;
  pageSize: number;
  hasPrev: boolean;
  hasNext: boolean;
  rawQ: string;
  setRawQ: (s: string) => void;
  isLoading?: boolean;
  isFetching?: boolean;
  refetch?: () => void;

  // Table content
  columns: Column<T>[];
  rowKey: (row: T, index: number) => string | number;
  emptyText?: ReactNode;

  // Header extras — arbitrary controls between search and refresh
  filters?: ReactNode;
  // Search placeholder (omit to hide the search input entirely)
  searchPlaceholder?: string;
  // When true (default), wrap the whole thing in a Card. Pass false to
  // embed in a custom layout.
  card?: boolean;
  // Approximate row height in px for the virtualizer's initial layout.
  // Doesn't have to be exact (Virtuoso measures live).
  rowHeight?: number;
  // Visible vertical area in px. The virtualizer needs a bounded
  // scroll container. Default 600.
  height?: number | string;
}

// Header / body cell props are shared by the live virtualized table and
// the loading skeleton so the two render the exact same shell — same
// .jc-table cell grammar and column widths — and the skeleton→data swap
// does not jump. Alignment falls out of the column type unless the caller
// overrides it.
export function cellAlign<T>(c: Column<T>): "left" | "right" | "center" {
  return c.align ?? (c.type === "number" || c.type === "actions" ? "right" : "left");
}
export function cellClass<T>(c: Column<T>, ...extra: (string | undefined)[]): string | undefined {
  const cls = [
    c.hideOn ? `jc-hide-${c.hideOn}` : undefined,
    c.type === "number" ? "jc-cell-num" : undefined,
    c.type === "actions" ? "jc-cell-actions" : undefined,
    ...extra,
  ].filter(Boolean);
  return cls.length ? cls.join(" ") : undefined;
}
function headerCellProps<T>(c: Column<T>): { className?: string; style: CSSProperties } {
  return {
    className: cellClass(c),
    style: { textAlign: cellAlign(c), width: c.width, minWidth: c.minWidth },
  };
}
function bodyCellProps<T>(c: Column<T>): { className?: string; style: CSSProperties } {
  return {
    className: cellClass(c, c.wrap ? "jc-cell-wrap" : "jc-cell-truncate"),
    style: { textAlign: cellAlign(c), maxWidth: c.width },
  };
}

const SKELETON_ROWS = 10;

export function DataTable<T>(props: DataTableProps<T>) {
  const { t } = useTranslation();
  const {
    items, total, page, setPage, pageSize, hasPrev, hasNext,
    rawQ, setRawQ, isLoading, isFetching, refetch,
    columns, rowKey, emptyText: emptyTextProp,
    filters, searchPlaceholder, card = true, height = 600,
  } = props;
  const emptyText = emptyTextProp ?? t("keys.shared.noMatches");

  const toolbar = (
    <Flex align="start" mb="2" wrap="wrap" gap="2" className="jc-data-table-toolbar">
      <Flex gap="2" align="start" wrap="wrap" className="jc-data-table-controls" style={{ flex: 1, minWidth: 0 }}>
        {searchPlaceholder !== undefined && (
          <Box className="jc-data-table-search" style={{ width: 240, maxWidth: 360 }}>
            <ClearableTextField
              placeholder={searchPlaceholder}
              value={rawQ}
              onChange={(e) => setRawQ(e.target.value)}
            >
              <TextField.Slot><Search size={14} /></TextField.Slot>
            </ClearableTextField>
          </Box>
        )}
        {filters}
        {refetch && (
          <Box className="jc-data-table-refresh">
            <Tooltip content={t("keys.shared.refresh")}>
              <IconButton size="2" variant="soft" color="gray" aria-label={t("keys.shared.refresh")} onClick={refetch}>
                <RotateCcw size={14} />
              </IconButton>
            </Tooltip>
          </Box>
        )}
      </Flex>
    </Flex>
  );

  const pager = (
    <ListPager
      page={page}
      pageSize={pageSize}
      total={total}
      loaded={items.length}
      hasPrev={hasPrev}
      hasNext={hasNext}
      onPage={setPage}
      disabled={!!isFetching}
    />
  );

  const body = (
    <Box className="jc-data-table-viewport" style={{ height, position: "relative" }}>
      {items.length === 0 && isLoading ? (
        <table className="jc-table" aria-busy>
          <thead>
            <tr>
              {columns.map((c, i) => (
                <th key={c.key ?? `c${i}`} {...headerCellProps(c)}>
                  {c.header}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {Array.from({ length: Math.min(SKELETON_ROWS, pageSize) }, (_, r) => (
              <tr key={r}>
                {columns.map((c, i) => (
                  <td key={c.key ?? `c${i}`} {...bodyCellProps(c)}>
                    <Skeleton width={i === 0 ? "72%" : i === columns.length - 1 ? "44%" : `${64 + ((r + i) % 3) * 12}%`} height="14px" style={{ borderRadius: 4 }} />
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      ) : items.length === 0 ? (
        <Flex align="center" justify="center" style={{ height: "100%" }}>
          <Text color="gray" size="2">{emptyText}</Text>
        </Flex>
      ) : (
        <TableVirtuoso
          style={{ height: "100%" }}
          data={items}
          components={virtuosoComponents}
          fixedHeaderContent={() => (
            <tr>
              {columns.map((c, i) => (
                <th
                  key={c.key ?? `c${i}`}
                  {...headerCellProps(c)}
                >
                  {c.header}
                </th>
              ))}
            </tr>
          )}
          itemContent={(index, row) => (
            <>
              {columns.map((c, i) => (
                <td
                  key={c.key ?? `c${i}`}
                  {...bodyCellProps(c)}
                >
                  {c.cell(row, index)}
                </td>
              ))}
            </>
          )}
          computeItemKey={(index) => rowKey(items[index], index)}
        />
      )}
    </Box>
  );

  if (!card) {
    return (
      <Box>
        {toolbar}
        {body}
        {pager}
      </Box>
    );
  }

  return (
    <Card className="pn-card-flat jc-data-table-card">
      {toolbar}
      {body}
      {pager}
    </Card>
  );
}

// virtuosoComponents wires our Radix-friendly elements. Typography, row
// height and separators come from the shared .jc-table grammar in
// console-radix.css, the same one Radix Table (SimpleDataTable) uses, so both
// shells render identical cells.
//
// table-layout: auto so columns size to content rather than dividing
// the available width evenly — fixed layout was making long-header
// columns (e.g. "tokens(in/cached/cw/out)") render vertical when the
// row had short content. With auto, the browser handles widths
// sensibly and explicit `width: ...` in column defs still wins.
const virtuosoComponents = {
  Table: (props: React.HTMLAttributes<HTMLTableElement>) => (
    <table {...props} className={["jc-table", props.className].filter(Boolean).join(" ")} />
  ),
  TableHead: (props: React.HTMLAttributes<HTMLTableSectionElement>) => (
    <thead {...props} />
  ),
  TableRow: (props: React.HTMLAttributes<HTMLTableRowElement>) => (
    <tr {...props} />
  ),
};
