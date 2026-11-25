// Shared recharts visual config so admin/user pages render consistently in
// both dark and light mode. Colors resolve through CSS vars defined in
// index.css under the `--c-chart-*` family, so swapping the html class
// automatically retints all charts.

export const chartGrid = {
  stroke: "var(--c-chart-grid)",
  strokeDasharray: "3 3",
} as const;

export const chartAxis = {
  stroke: "var(--c-chart-axis)",
  tick: { fill: "var(--c-chart-axis)", fontSize: 12 },
  tickLine: { stroke: "var(--c-chart-axis)" },
  axisLine: { stroke: "var(--c-chart-grid)" },
} as const;

export const chartTooltipProps = {
  contentStyle: {
    background: "var(--c-chart-tooltip-bg)",
    border: "1px solid var(--c-chart-tooltip-border)",
    borderRadius: 8,
    color: "var(--c-chart-tooltip-text)",
    fontSize: 12,
    boxShadow: "0 4px 16px rgba(0,0,0,0.18)",
  },
  labelStyle: { color: "var(--c-chart-axis)", marginBottom: 4 },
  itemStyle: { color: "var(--c-chart-tooltip-text)" },
  cursor: { fill: "var(--c-chart-cursor)" },
} as const;

export const chartLegendStyle = { color: "var(--c-chart-axis)", fontSize: 12 } as const;

// Max width (px) a single bar / stack column can grow to. Without this
// cap, a chart with 2-3 data points on a wide container ends up with
// columns half the chart width — visually awful. 48px is roughly the
// width of two text characters; wide enough to still read as a bar,
// narrow enough to leave breathing room between columns.
export const chartBarMaxSize = 48;

// Bar palette — kept perceptually distinct in both modes. Use these instead
// of literal hex throughout the app so the palette is consistent.
export const chartColors = {
  primary: "#8154e5",   // purple (brand)
  success: "#22c55e",   // green
  danger:  "#ef4444",   // red
  amber:   "#f59e0b",   // amber (cost)
  cyan:    "#06b6d4",   // cyan (cached)
  indigo:  "#6366f1",   // indigo (requests)
  violet:  "#a78bfa",   // violet (alt input)
  orange:  "#f97316",   // orange (high severity)
  slate:   "#64748b",   // slate (neutral fallback)
} as const;
