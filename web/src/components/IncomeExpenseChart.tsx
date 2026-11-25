import { SegmentedControl, Text } from "@radix-ui/themes";
import { useTranslation } from "react-i18next";
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { chartAxis, chartBarMaxSize, chartColors, chartGrid, chartLegendStyle, chartTooltipProps } from "../lib/chartTheme";
import { formatAmount } from "../lib/currency";

export type ChartKind = "bar" | "line";

export interface IncomeExpenseDay {
  date: string;
  income: string;
  expense: string;
}

// Daily income vs expense. Bars by default; the bar/line switch renders only
// when the host passes `onKindChange` — the personal billing page omits it,
// since a line said the same thing there.
export function IncomeExpenseChart({
  days,
  height = 260,
  kind = "bar",
  onKindChange,
}: {
  days: IncomeExpenseDay[];
  height?: number;
  kind?: ChartKind;
  onKindChange?: (k: ChartKind) => void;
}) {
  const { t } = useTranslation();
  const data = days.map((d) => ({
    date: d.date.slice(5),
    full: d.date,
    income: Number(d.income) || 0,
    expense: Number(d.expense) || 0,
  }));

  const toggle = onKindChange ? (
    <div style={{ display: "flex", justifyContent: "flex-end", marginBottom: 8 }}>
      <SegmentedControl.Root value={kind} onValueChange={(v) => onKindChange(v as ChartKind)} size="1">
        <SegmentedControl.Item value="bar">{t("usage.incomeExpense.bar")}</SegmentedControl.Item>
        <SegmentedControl.Item value="line">{t("usage.incomeExpense.line")}</SegmentedControl.Item>
      </SegmentedControl.Root>
    </div>
  ) : null;

  if (data.length === 0) {
    return (
      <div>
        {toggle}
        <div style={{ height, display: "grid", placeItems: "center" }}>
          <Text size="2" color="gray">{t("usage.incomeExpense.empty")}</Text>
        </div>
      </div>
    );
  }

  return (
    <div>
    {toggle}
    <ResponsiveContainer width="100%" height={height}>
      <ComposedChart data={data} barCategoryGap="24%" margin={{ top: 4, right: 0, bottom: 0, left: 0 }}>
        <CartesianGrid {...chartGrid} vertical={false} />
        <XAxis dataKey="date" {...chartAxis} interval="preserveStartEnd" minTickGap={24} />
        <YAxis
          {...chartAxis}
          width={56}
          tickFormatter={(v: number) => formatAmount(v, { decimals: 0 })}
        />
        <Tooltip
          {...chartTooltipProps}
          labelFormatter={(_, payload) => payload?.[0]?.payload?.full ?? ""}
          formatter={(value, name) => [
            t("usage.common.pointsValue", { value: formatAmount(typeof value === "number" ? value : Number(value), { decimals: 2 }) }),
            name === "income" ? t("usage.incomeExpense.income") : t("usage.incomeExpense.expense"),
          ]}
        />
        <Legend wrapperStyle={chartLegendStyle} formatter={(v) => (v === "income" ? t("usage.incomeExpense.income") : t("usage.incomeExpense.expense"))} />
        {kind === "bar" ? (
          <>
            <Bar dataKey="income" name="income" fill={chartColors.success} maxBarSize={chartBarMaxSize} radius={[3, 3, 0, 0]} />
            <Bar dataKey="expense" name="expense" fill={chartColors.primary} maxBarSize={chartBarMaxSize} radius={[3, 3, 0, 0]} />
          </>
        ) : (
          <>
            <Line type="monotone" dataKey="income" name="income" stroke={chartColors.success} strokeWidth={2} dot={false} />
            <Line type="monotone" dataKey="expense" name="expense" stroke={chartColors.primary} strokeWidth={2} dot={false} />
          </>
        )}
      </ComposedChart>
    </ResponsiveContainer>
    </div>
  );
}
