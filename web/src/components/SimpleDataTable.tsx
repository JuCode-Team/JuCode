// SimpleDataTable is the non-virtualized counterpart of DataTable.
//
// Why both exist:
//   - DataTable wraps react-virtuoso, which renders a fixed-height
//     scroll viewport with absolutely-positioned rows. That layout
//     forces a `table-layout: auto` table inside a scroll container
//     of constrained width — column auto-sizing works in theory but
//     in practice the virtualizer's measurement pass sees each row in
//     isolation and produces inconsistent column widths.
//   - For lists that fit on one or two pages (50/page in our setup),
//     a plain HTML table sizes columns the way browsers have done it
//     for 30 years: content-driven, fast, predictable.
//
// API mirrors DataTable's exactly (Column<T>, props), so swapping
// the import is the only change a page needs.

import { Fragment, type MouseEvent, type ReactNode } from "react";
import { Badge, Box, Button, Card, Checkbox, Flex, IconButton, Table, TextField, Tooltip } from "@radix-ui/themes";
import { ListPager } from "./list/ListPager";
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import { restrictToParentElement, restrictToVerticalAxis } from "@dnd-kit/modifiers";
import {
  SortableContext,
  arrayMove,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { GripVertical, Inbox, RotateCcw, Search } from "lucide-react";
import { useTranslation } from "react-i18next";
import { ClearableTextField } from "./ClearableTextField";
import { TableSkeletonRows } from "./ConsoleSkeleton";
import { cellAlign, cellClass } from "./DataTable";


export interface Column<T> {
  header: ReactNode;
  cell: (row: T, index: number) => ReactNode;
  /** Optional explicit width (e.g. "120px") */
  width?: string;
  /** Optional min-width so very short content doesn't collapse the column */
  minWidth?: string;
  /** Defaults to "right" when `type` is "number" or "actions". */
  align?: "left" | "right" | "center";
  /** "number" right-aligns and enables tabular figures (.jc-cell-num);
   *  "actions" right-aligns the row's action cluster (.jc-cell-actions). */
  type?: "text" | "number" | "actions";
  hideOn?: "sm" | "md";
  /** Allow this cell's content to wrap (default: nowrap with ellipsis) */
  wrap?: boolean;
  key?: string;
}

export interface DataTableProps<T> {
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

  columns: Column<T>[];
  rowKey: (row: T, index: number) => string | number;
  emptyText?: ReactNode;
  /** Second line under the empty-state title. */
  emptyHint?: ReactNode;
  /** Optional action in the empty state, e.g. 「清空筛选」. */
  emptyAction?: ReactNode;
  /** Rendered under the toolbar — the applied-filter chips row. */
  toolbarFooter?: ReactNode;
  /** Optional expandable detail panel shown under each row. */
  renderExpandedRow?: (row: T, index: number) => ReactNode;
  /** Controls which rows are expanded when `renderExpandedRow` is set. */
  isRowExpanded?: (row: T, index: number) => boolean;
  /** Optional click handler for the primary data row. */
  onRowClick?: (row: T, index: number, event: MouseEvent<HTMLTableRowElement>) => void;

  filters?: ReactNode;
  actions?: ReactNode;
  /** Optional Radix size for search, refresh, and pager controls. */
  controlSize?: "1" | "2" | "3";
  selectedRowKeys?: Set<string | number>;
  onSelectedRowKeysChange?: (keys: Set<string | number>) => void;
  bulkActions?: ReactNode;
  searchPlaceholder?: string;
  card?: boolean;
  /** Enables a drag handle column. Ignored when `renderExpandedRow` is set:
   *  a detail row is a separate <tr> that would not travel with its parent. */
  reorderable?: boolean;
  /** Receives the page's row keys in their new order. Only the current page is
   *  reported, so the backend must splice rather than renumber from zero. */
  onReorder?: (orderedKeys: string[]) => void;
  /** Unused — kept so the prop bag from usePagedQuery + DataTable
   * mirrors SimpleDataTable's; the virtualization flag is implicit
   * in which component you import. */
  rowHeight?: number;
  height?: number | string;
}

export function SimpleDataTable<T>(props: DataTableProps<T>) {
  const { t } = useTranslation();
  const {
    items, total, page, setPage, pageSize, hasPrev, hasNext,
    rawQ, setRawQ, isLoading, isFetching, refetch,
    columns, rowKey, emptyText: emptyTextProp, emptyHint, emptyAction, toolbarFooter,
    renderExpandedRow, isRowExpanded, onRowClick,
    filters, actions, controlSize, selectedRowKeys, onSelectedRowKeysChange, bulkActions,
    searchPlaceholder, card = true, reorderable, onReorder,
  } = props;
  const emptyText = emptyTextProp ?? t("keys.shared.noMatches");

  const canReorder = !!reorderable && !!onReorder && !renderExpandedRow && items.length > 0;
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const hasColumnSizing = columns.some((c) => c.width || c.minWidth);
  const selectable = !!selectedRowKeys && !!onSelectedRowKeysChange;
  const pageKeys = items.map((row, idx) => rowKey(row, idx));
  const selectedOnPage = selectable ? pageKeys.filter((key) => selectedRowKeys.has(key)).length : 0;
  const allPageSelected = selectable && pageKeys.length > 0 && selectedOnPage === pageKeys.length;
  const somePageSelected = selectable && selectedOnPage > 0 && !allPageSelected;

  const setPageSelected = (checked: boolean) => {
    if (!selectedRowKeys || !onSelectedRowKeysChange) return;
    const next = new Set(selectedRowKeys);
    pageKeys.forEach((key) => {
      if (checked) next.add(key);
      else next.delete(key);
    });
    onSelectedRowKeysChange(next);
  };

  const handleDragEnd = (event: DragEndEvent) => {
    const { active, over } = event;
    if (!over || active.id === over.id || !onReorder) return;
    const keys = pageKeys.map(String);
    const from = keys.indexOf(String(active.id));
    const to = keys.indexOf(String(over.id));
    if (from < 0 || to < 0) return;
    onReorder(arrayMove(keys, from, to));
  };

  const setRowSelected = (key: string | number, checked: boolean) => {
    if (!selectedRowKeys || !onSelectedRowKeysChange) return;
    const next = new Set(selectedRowKeys);
    if (checked) next.add(key);
    else next.delete(key);
    onSelectedRowKeysChange(next);
  };

  const toolbar = (
    <Flex align="start" justify="between" mb="2" wrap="wrap" gap="2" className="jc-data-table-toolbar">
      <Flex gap="2" align="start" wrap="wrap" className="jc-data-table-controls" style={{ flex: 1, minWidth: 0 }}>
        {searchPlaceholder !== undefined && (
          <Box className="jc-data-table-search" style={{ width: 240, maxWidth: 360 }}>
            <ClearableTextField
              size={controlSize}
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
              <IconButton size={controlSize ?? "2"} variant="soft" color="gray" aria-label={t("keys.shared.refresh")} onClick={refetch}>
                <RotateCcw size={14} />
              </IconButton>
            </Tooltip>
          </Box>
        )}
      </Flex>
      {actions && (
        <Flex gap="2" align="center" justify="end" wrap="wrap" className="jc-data-table-actions">
          {actions}
        </Flex>
      )}
    </Flex>
  );

  const selectionBar = selectable && selectedRowKeys.size > 0 ? (
    <Flex align="center" justify="between" gap="2" wrap="wrap" mb="2" className="jc-data-table-bulkbar">
      <Flex align="center" gap="2">
        <Badge color="iris" variant="soft">{t("keys.shared.selectedCount", { n: selectedRowKeys.size })}</Badge>
        <Button size="1" variant="ghost" color="gray" onClick={() => onSelectedRowKeysChange(new Set())}>
          {t("keys.shared.clear")}
        </Button>
      </Flex>
      {bulkActions && <Flex gap="2" justify="end" wrap="wrap">{bulkActions}</Flex>}
    </Flex>
  ) : null;

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
      size={controlSize === "2" || controlSize === "3" ? "2" : "1"}
    />
  );

  const body = (
    <Box className="jc-data-table-scroll">
      <Table.Root size="2" variant="surface">
        {hasColumnSizing && (
          <colgroup>
            {/* <col> maps to cells positionally, so the leading handle and
                checkbox cells need their own entries or every width below
                shifts one column left. */}
            {canReorder && <col style={{ width: 34 }} />}
            {selectable && <col style={{ width: 42 }} />}
            {columns.map((c, i) => (
              <col
                key={c.key ?? `c${i}`}
                className={c.hideOn ? `jc-hide-${c.hideOn}` : undefined}
                style={{
                  width: c.width,
                  minWidth: c.minWidth,
                }}
              />
            ))}
          </colgroup>
        )}
        <Table.Header>
          <Table.Row>
            {canReorder && <Table.ColumnHeaderCell style={{ width: 34 }} aria-label={t("keys.shared.sort")} />}
            {selectable && (
              <Table.ColumnHeaderCell style={{ width: 42 }}>
                <Checkbox
                  checked={allPageSelected ? true : somePageSelected ? "indeterminate" : false}
                  onCheckedChange={(checked) => setPageSelected(checked === true)}
                />
              </Table.ColumnHeaderCell>
            )}
            {columns.map((c, i) => (
              <Table.ColumnHeaderCell
                key={c.key ?? `c${i}`}
                className={cellClass(c, "jc-cell-truncate")}
                style={{ textAlign: cellAlign(c), width: c.width, minWidth: c.minWidth }}
              >
                {c.header}
              </Table.ColumnHeaderCell>
            ))}
          </Table.Row>
        </Table.Header>
        <Table.Body>
          {canReorder && (
            <DndContext
              sensors={sensors}
              collisionDetection={closestCenter}
              modifiers={[restrictToVerticalAxis, restrictToParentElement]}
              onDragEnd={handleDragEnd}
            >
              <SortableContext items={pageKeys.map(String)} strategy={verticalListSortingStrategy}>
                {renderRows()}
              </SortableContext>
            </DndContext>
          )}
          {!canReorder && renderRows()}
        </Table.Body>
      </Table.Root>
    </Box>
  );

  function renderRows() {
    const colCount = columns.length + (selectable ? 1 : 0) + (canReorder ? 1 : 0);
    if (isLoading && items.length === 0) {
      return <TableSkeletonRows cols={colCount} rows={8} />;
    }
    return (
      <>
          {items.length === 0 ? (
            <Table.Row className="jc-list-detail-row">
              <Table.Cell colSpan={colCount}>
                <div className="jc-list-empty">
                  <Inbox size={22} strokeWidth={1.6} aria-hidden />
                  <div className="jc-list-empty-title">{emptyText}</div>
                  {emptyHint && <div className="jc-list-empty-hint">{emptyHint}</div>}
                  {emptyAction && <Box mt="2">{emptyAction}</Box>}
                </div>
              </Table.Cell>
            </Table.Row>
          ) : (
            items.map((row, idx) => {
              const key = rowKey(row, idx);
              const expanded = !!renderExpandedRow && !!isRowExpanded?.(row, idx);
              const cells = (
                <>
                  {selectable && (
                      <Table.Cell style={{ width: 42 }}>
                        <Checkbox
                          checked={selectedRowKeys.has(key)}
                          onClick={(event) => event.stopPropagation()}
                          onCheckedChange={(checked) => setRowSelected(key, checked === true)}
                        />
                      </Table.Cell>
                    )}
                    {columns.map((c, i) => (
                      <Table.Cell
                        key={c.key ?? `c${i}`}
                        className={cellClass(c, c.wrap ? "jc-cell-wrap" : "jc-cell-truncate")}
                        style={{ textAlign: cellAlign(c), width: c.width, minWidth: c.minWidth, maxWidth: c.width }}
                      >
                        {c.cell(row, idx)}
                      </Table.Cell>
                    ))}
                </>
              );
              return (
                <Fragment key={key}>
                  {canReorder ? (
                    <SortableTableRow
                      id={String(key)}
                      onClick={onRowClick ? (event) => onRowClick(row, idx, event) : undefined}
                    >
                      {cells}
                    </SortableTableRow>
                  ) : (
                    <Table.Row
                      onClick={onRowClick ? (event) => onRowClick(row, idx, event) : undefined}
                      style={onRowClick ? { cursor: "pointer" } : undefined}
                    >
                      {cells}
                    </Table.Row>
                  )}
                  {expanded && renderExpandedRow && (
                    <Table.Row
                      className="jc-data-table-expanded-row jc-list-detail-row"
                      style={{ boxShadow: "none", background: "transparent" }}
                    >
                      <Table.Cell
                        colSpan={columns.length + (selectable ? 1 : 0) + (canReorder ? 1 : 0)}
                        className="jc-data-table-expanded-cell"
                        style={{ padding: 0, verticalAlign: "top" }}
                      >
                        {renderExpandedRow(row, idx)}
                      </Table.Cell>
                    </Table.Row>
                  )}
                </Fragment>
              );
            })
          )}
      </>
    );
  }

  if (!card) {
    return (
      <Box>
        {toolbar}
        {toolbarFooter}
        {selectionBar}
        {body}
        {pager}
      </Box>
    );
  }

  return (
    <Card className="pn-card-flat jc-data-table-card">
      {toolbar}
      {toolbarFooter}
      {selectionBar}
      {body}
      {pager}
    </Card>
  );
}

// Convenience re-export so pages can keep the familiar `DataTable`
// name while internally we use the non-virtualized renderer. This
// makes the swap a single-line import change.
export { SimpleDataTable as DataTable };

// A <tr> that participates in dnd-kit sorting. Rows are transformed in place
// rather than lifted into a DragOverlay: an overlay copy of a table row loses
// the parent table's column widths and collapses to its content.
//
// The handle lives here rather than in the caller's column list so a page only
// has to pass `reorderable` — and so the handle can never be placed after a
// cell that swallows the pointer.
function SortableTableRow({
  id,
  onClick,
  children,
}: {
  id: string;
  onClick?: (event: MouseEvent<HTMLTableRowElement>) => void;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id });
  return (
    <Table.Row
      ref={setNodeRef}
      onClick={onClick}
      style={{
        transform: CSS.Transform.toString(transform),
        transition,
        opacity: isDragging ? 0.85 : undefined,
        position: isDragging ? "relative" : undefined,
        zIndex: isDragging ? 1 : undefined,
        cursor: onClick ? "pointer" : undefined,
      }}
    >
      <Table.Cell style={{ width: 34, verticalAlign: "middle" }}>
        <button type="button" className="sortable-handle" aria-label={t("keys.shared.dragSort")} {...attributes} {...listeners}>
          <GripVertical size={14} aria-hidden />
        </button>
      </Table.Cell>
      {children}
    </Table.Row>
  );
}
