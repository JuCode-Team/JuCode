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
import { GripVertical } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { ReactNode } from "react";

// Shared vertical drag-to-reorder list. Ordering is expressed as an array of
// ids rather than index arithmetic at each call site, so a page only has to say
// "this is the new order" and persist it.
//
// Dragging is bound to an explicit handle, never the whole row: these lists sit
// inside cards with their own buttons, links and expand toggles, and a
// whole-row drag surface swallows those interactions on touch.

/** Distance the pointer must travel before a press becomes a drag. Below this a
 *  press is still a click, so handles remain focusable and tappable. */
const DRAG_ACTIVATION_DISTANCE = 6;

export function SortableList<T>({
  items,
  getId,
  onReorder,
  renderItem,
}: {
  items: T[];
  getId: (item: T) => string;
  /** Called with the full id list in its new order. Not called for no-op drags. */
  onReorder: (orderedIds: string[]) => void;
  renderItem: (item: T, index: number) => ReactNode;
}) {
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: DRAG_ACTIVATION_DISTANCE } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );
  const ids = items.map(getId);

  function handleDragEnd(event: DragEndEvent) {
    const { active, over } = event;
    if (!over || active.id === over.id) return;
    const from = ids.indexOf(String(active.id));
    const to = ids.indexOf(String(over.id));
    if (from < 0 || to < 0) return;
    onReorder(arrayMove(ids, from, to));
  }

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      modifiers={[restrictToVerticalAxis, restrictToParentElement]}
      onDragEnd={handleDragEnd}
    >
      <SortableContext items={ids} strategy={verticalListSortingStrategy}>
        {items.map((item, index) => renderItem(item, index))}
      </SortableContext>
    </DndContext>
  );
}

/** One draggable row. `children` receives the handle to place wherever the row
 *  layout wants it. */
export function SortableItem({
  id,
  children,
}: {
  id: string;
  children: (handle: ReactNode) => ReactNode;
}) {
  const { t } = useTranslation();
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id });
  const handle = (
    <button
      type="button"
      className="sortable-handle"
      aria-label={t("keys.shared.dragSort")}
      {...attributes}
      {...listeners}
    >
      <GripVertical size={14} aria-hidden />
    </button>
  );
  return (
    <div
      ref={setNodeRef}
      className={isDragging ? "sortable-row sortable-row-dragging" : "sortable-row"}
      style={{
        transform: CSS.Transform.toString(transform),
        transition,
        // Lift the dragged row above its neighbours; without this the rows it
        // slides past paint on top of it.
        zIndex: isDragging ? 1 : undefined,
        position: isDragging ? "relative" : undefined,
      }}
    >
      {children(handle)}
    </div>
  );
}
