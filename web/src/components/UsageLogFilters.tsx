import { useEffect, useMemo, useState } from "react";
import { Box, Popover } from "@radix-ui/themes";
import {
  CalendarDays,
  ChevronLeft,
  ChevronRight,
  ChevronsLeft,
  ChevronsRight,
  Clock3,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { localeTag } from "../i18n";

const ENDPOINT_VALUES = [
  "chat_completions", "responses", "messages", "embeddings", "image_generation",
  "image_edit", "image_variation", "audio_speech", "audio_transcription",
  "audio_translation", "rerank", "video_generation", "web_search", "web_fetch",
];
const ENDPOINT_LABELS: Record<string, string> = {
  messages: "messages (Anthropic)",
  audio_speech: "audio speech (TTS)",
};

// Endpoint names are API identifiers and stay in English; only the "all"
// option is translated.
export const endpointOptions = (t: TFunction) => [
  { v: "", l: t("usage.filters.allEndpoints") },
  ...ENDPOINT_VALUES.map((v) => ({ v, l: ENDPOINT_LABELS[v] ?? v.replace(/_/g, " ") })),
];

export const billingSourceOptions = (t: TFunction) => [
  { v: "", l: t("usage.filters.allBillingSources") },
  { v: "plan", l: t("usage.filters.plan") },
  { v: "plan_balance", l: t("usage.filters.planBalance") },
  { v: "balance", l: t("usage.filters.balance") },
  { v: "none", l: t("usage.filters.none") },
];

export const statusOptions = (t: TFunction) => [
  { v: "", l: t("usage.filters.allStatuses") },
  ...(["ok", "upstream_error", "quota_exceeded", "banned", "unauthorized", "internal_error"] as const)
    .map((v) => ({ v, l: t(`usage.status.${v}`) })),
];

export function last7DaysRange(): { start: string; end: string } {
  const now = new Date();
  const from = addDays(startOfDay(now), -6);
  return {
    start: makeLocalDateTime(from, "00:00:00"),
    end: makeLocalDateTime(now, formatTime(now)),
  };
}

export function last30DaysRange(): { start: string; end: string } {
  const now = new Date();
  const from = addDays(startOfDay(now), -29);
  return {
    start: makeLocalDateTime(from, "00:00:00"),
    end: makeLocalDateTime(now, formatTime(now)),
  };
}

// The admin log endpoint floors an anomaly query at now-24h when no start
// is given. Pages that set the anomaly filter fill the picker with the
// same window so the visible range matches what was actually queried.
export function last24HoursStart(): string {
  const from = new Date(Date.now() - 24 * 60 * 60 * 1000);
  return makeLocalDateTime(from, formatTime(from));
}

export function last12MonthsRange(): { start: string; end: string } {
  const now = new Date();
  const from = new Date(now.getFullYear(), now.getMonth() - 11, 1);
  return {
    start: makeLocalDateTime(from, "00:00:00"),
    end: makeLocalDateTime(now, formatTime(now)),
  };
}


// Backend windows are half-open [from, to). The picker stores an inclusive
// local datetime, so we bump the end by one second before sending ISO.
export function rangeToIso(start: string, end: string): { from?: string; to?: string } {
  const from = start ? new Date(start).toISOString() : undefined;
  if (!end) return { from };
  const d = new Date(end);
  d.setSeconds(d.getSeconds() + 1);
  return { from, to: d.toISOString() };
}

export function DateRangeInput({
  start,
  setStart,
  end,
  setEnd,
  compact = false,
}: {
  start: string;
  setStart: (v: string) => void;
  end: string;
  setEnd: (v: string) => void;
  // Dates only on the trigger; the popover still edits times. For toolbars
  // where the full two-timestamp string would take a third of the row.
  compact?: boolean;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const startDate = useMemo(() => parseLocalDateTime(start), [start]);
  const endDate = useMemo(() => parseLocalDateTime(end), [end]);
  const [viewMonth, setViewMonth] = useState(() =>
    startOfMonth(startDate ?? new Date()),
  );

  useEffect(() => {
    if (open) setViewMonth(startOfMonth(startDate ?? new Date()));
  }, [open, startDate]);

  const commitStart = (next: string) => {
    setStart(next);
    const nextDate = parseLocalDateTime(next);
    if (nextDate && endDate && startOfDay(endDate) < startOfDay(nextDate)) {
      setEnd("");
    }
  };

  const commitEnd = (next: string) => {
    const nextDate = parseLocalDateTime(next);
    if (nextDate && startDate && startOfDay(nextDate) < startOfDay(startDate)) {
      setStart(makeLocalDateTime(nextDate, "00:00:00"));
    }
    setEnd(next);
  };

  const selectDay = (day: Date) => {
    const selectedDay = startOfDay(day);
    if (!startDate || endDate || selectedDay < startOfDay(startDate)) {
      setStart(makeLocalDateTime(selectedDay, "00:00:00"));
      setEnd("");
      return;
    }
    setEnd(makeLocalDateTime(selectedDay, "23:59:59"));
  };

  const applyPreset = (preset: DateRangePreset) => {
    const now = new Date();
    let from = startOfDay(now);

    if (preset === "last7") from = addDays(startOfDay(now), -6);
    if (preset === "week") from = startOfWeekMonday(now);
    if (preset === "last30") from = addDays(startOfDay(now), -29);
    if (preset === "month") from = startOfMonth(now);
    if (preset === "lastMonth") {
      const firstThisMonth = startOfMonth(now);
      from = addMonths(firstThisMonth, -1);
      setStart(makeLocalDateTime(from, "00:00:00"));
      setEnd(makeLocalDateTime(addDays(firstThisMonth, -1), "23:59:59"));
      setViewMonth(startOfMonth(from));
      return;
    }

    setStart(makeLocalDateTime(from, "00:00:00"));
    setEnd(makeLocalDateTime(now, formatTime(now)));
    setViewMonth(startOfMonth(from));
  };

  return (
    <Box className="jc-log-filter-range">
      <Popover.Root open={open} onOpenChange={setOpen}>
        <Popover.Trigger>
          <button type="button" className="jc-date-range-trigger" title={compact ? formatRangeTrigger(t, startDate, endDate) : undefined}>
            <CalendarDays size={15} />
            <span>{compact ? formatRangeTriggerCompact(t, startDate, endDate) : formatRangeTrigger(t, startDate, endDate)}</span>
          </button>
        </Popover.Trigger>
        <Popover.Content
          align="start"
          sideOffset={8}
          className="jc-date-range-popover"
        >
          <div className="jc-date-range-panel">
            <div className="jc-date-range-calendars">
              <MonthCalendar
                month={viewMonth}
                startDate={startDate}
                endDate={endDate}
                onSelect={selectDay}
                onNavigate={(months) => setViewMonth(addMonths(viewMonth, months))}
              />
              <MonthCalendar
                month={addMonths(viewMonth, 1)}
                startDate={startDate}
                endDate={endDate}
                onSelect={selectDay}
                onNavigate={(months) => setViewMonth(addMonths(viewMonth, months))}
              />
            </div>

            <div className="jc-date-range-footer">
              <DateTimeEndpoint
                value={start}
                fallbackTime="00:00:00"
                placeholder={t("usage.dateRange.startDate")}
                onChange={commitStart}
                onJump={(date) => setViewMonth(startOfMonth(date))}
              />
              <DateTimeEndpoint
                value={end}
                fallbackTime="23:59:59"
                placeholder={t("usage.dateRange.endDate")}
                onChange={commitEnd}
                onJump={(date) => setViewMonth(startOfMonth(date))}
              />
            </div>

            <div className="jc-date-range-presets">
              <button type="button" onClick={() => applyPreset("today")}>{t("usage.dateRange.today")}</button>
              <button type="button" onClick={() => applyPreset("last7")}>{t("usage.dateRange.last7")}</button>
              <button type="button" onClick={() => applyPreset("week")}>{t("usage.dateRange.week")}</button>
              <button type="button" onClick={() => applyPreset("last30")}>{t("usage.dateRange.last30")}</button>
              <button type="button" onClick={() => applyPreset("month")}>{t("usage.dateRange.month")}</button>
              <button type="button" onClick={() => applyPreset("lastMonth")}>{t("usage.dateRange.lastMonth")}</button>
            </div>
          </div>
        </Popover.Content>
      </Popover.Root>
    </Box>
  );
}

type DateRangePreset = "today" | "last7" | "week" | "last30" | "month" | "lastMonth";

function MonthCalendar({
  month,
  startDate,
  endDate,
  onSelect,
  onNavigate,
}: {
  month: Date;
  startDate: Date | null;
  endDate: Date | null;
  onSelect: (day: Date) => void;
  onNavigate: (months: number) => void;
}) {
  const { t } = useTranslation();
  const days = calendarDays(month);
  return (
    <section className="jc-date-calendar">
      <div className="jc-date-calendar-head">
        <button type="button" aria-label={t("usage.dateRange.prevYear")} onClick={() => onNavigate(-12)}>
          <ChevronsLeft size={16} />
        </button>
        <button type="button" aria-label={t("usage.dateRange.prevMonth")} onClick={() => onNavigate(-1)}>
          <ChevronLeft size={16} />
        </button>
        <div>
          {t("usage.dateRange.monthTitle", {
            year: month.getFullYear(),
            month: month.getMonth() + 1,
            monthName: month.toLocaleDateString(localeTag(), { month: "long" }),
          })}
        </div>
        <button type="button" aria-label={t("usage.dateRange.nextMonth")} onClick={() => onNavigate(1)}>
          <ChevronRight size={16} />
        </button>
        <button type="button" aria-label={t("usage.dateRange.nextYear")} onClick={() => onNavigate(12)}>
          <ChevronsRight size={16} />
        </button>
      </div>
      <div className="jc-date-weekdays">
        {t("usage.dateRange.weekdays").split(" ").map((d) => (
          <span key={d}>{d}</span>
        ))}
      </div>
      <div className="jc-date-days">
        {days.map((day) => {
          const outside = day.getMonth() !== month.getMonth();
          const selectedStart = !!startDate && isSameDay(day, startDate);
          const selectedEnd = !!endDate && isSameDay(day, endDate);
          const inRange = isWithinRange(day, startDate, endDate);
          const today = isSameDay(day, new Date());
          return (
            <button
              key={dayKey(day)}
              type="button"
              className={[
                outside ? "is-outside" : "",
                inRange ? "is-in-range" : "",
                selectedStart || selectedEnd ? "is-selected" : "",
                today ? "is-today" : "",
              ].filter(Boolean).join(" ")}
              onClick={() => onSelect(day)}
            >
              {day.getDate()}
            </button>
          );
        })}
      </div>
    </section>
  );
}

function DateTimeEndpoint({
  value,
  fallbackTime,
  placeholder,
  onChange,
  onJump,
}: {
  value: string;
  fallbackTime: string;
  placeholder: string;
  onChange: (v: string) => void;
  onJump: (date: Date) => void;
}) {
  const date = parseLocalDateTime(value);
  const timeValue = date ? formatTime(date) : fallbackTime;

  return (
    <div className="jc-date-time-endpoint">
      <button
        type="button"
        className="jc-date-time-date"
        onClick={() => date && onJump(date)}
      >
        <CalendarDays size={15} />
        <span>{date ? formatDate(date) : placeholder}</span>
      </button>
      <label className="jc-date-time-time">
        <Clock3 size={15} />
        <input
          type="time"
          step="1"
          value={timeValue}
          onChange={(e) => {
            const base = date ?? new Date();
            onChange(makeLocalDateTime(base, normalizeTime(e.target.value, fallbackTime)));
          }}
        />
      </label>
    </div>
  );
}

function parseLocalDateTime(value: string): Date | null {
  if (!value) return null;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : d;
}

function formatRangeTrigger(t: TFunction, start: Date | null, end: Date | null): string {
  const unset = t("usage.dateRange.unset");
  if (start && end) return `${formatDate(start)} ${formatTime(start)} — ${formatDate(end)} ${formatTime(end)}`;
  if (start) return `${formatDate(start)} ${formatTime(start)} — ${unset}`;
  if (end) return `${unset} — ${formatDate(end)} ${formatTime(end)}`;
  return t("usage.dateRange.pickRange");
}

function formatRangeTriggerCompact(t: TFunction, start: Date | null, end: Date | null): string {
  const unset = t("usage.dateRange.unset");
  if (start && end) {
    if (isSameDay(start, end)) return formatDate(start);
    return `${formatDate(start)} — ${formatDate(end)}`;
  }
  if (start) return `${formatDate(start)} — ${unset}`;
  if (end) return `${unset} — ${formatDate(end)}`;
  return t("usage.dateRange.pickRange");
}

function formatDate(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function formatTime(date: Date): string {
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

function makeLocalDateTime(date: Date, time: string): string {
  return `${formatDate(date)}T${normalizeTime(time, "00:00:00")}`;
}

function normalizeTime(value: string, fallback: string): string {
  if (!value) return fallback;
  const [hh = "00", mm = "00", ss = "00"] = value.split(":");
  return `${pad(Number(hh))}:${pad(Number(mm))}:${pad(Number(ss))}`;
}

function pad(n: number): string {
  return String(Number.isFinite(n) ? n : 0).padStart(2, "0");
}

function startOfDay(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), date.getDate());
}

function startOfMonth(date: Date): Date {
  return new Date(date.getFullYear(), date.getMonth(), 1);
}

function startOfWeekMonday(date: Date): Date {
  const d = startOfDay(date);
  const day = d.getDay() || 7;
  return addDays(d, 1 - day);
}

function addDays(date: Date, days: number): Date {
  const d = new Date(date);
  d.setDate(d.getDate() + days);
  return d;
}

function addMonths(date: Date, months: number): Date {
  return new Date(date.getFullYear(), date.getMonth() + months, 1);
}

function calendarDays(month: Date): Date[] {
  const first = startOfMonth(month);
  const cursor = addDays(first, -first.getDay());
  return Array.from({ length: 42 }, (_, i) => addDays(cursor, i));
}

function isSameDay(a: Date, b: Date): boolean {
  return dayKey(a) === dayKey(b);
}

function dayKey(date: Date): string {
  return formatDate(date);
}

function isWithinRange(day: Date, start: Date | null, end: Date | null): boolean {
  if (!start || !end) return false;
  const t = startOfDay(day).getTime();
  return t >= startOfDay(start).getTime() && t <= startOfDay(end).getTime();
}
