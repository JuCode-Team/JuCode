function groupRate(rate: number | null | undefined): number {
  return typeof rate === "number" && Number.isFinite(rate) && rate > 0 ? rate : 1;
}

export function formatMultiplier(rate: number | null | undefined): string {
  return `${groupRate(rate).toString()}×`;
}
