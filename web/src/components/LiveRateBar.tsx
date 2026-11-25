import { Badge, Card, Flex, Text } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { Activity, Gauge } from "lucide-react";
import { useTranslation } from "react-i18next";

// Live RPM / TPM readout, shown above a usage-log table.
//
// The backend estimates both over a trailing 60 seconds by blending the
// current minute bucket with the tail of the previous one, so the numbers move
// smoothly instead of resetting on every minute boundary. Polling faster than
// a few seconds would only re-render the same blend, so 5s is the floor here.

type Rate = { rpm: number; tpm: number; rpm_limit: number; tpm_limit: number };

function formatCount(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(n >= 10_000 ? 0 : 1) + "k";
  return String(n);
}

function Metric({
  icon,
  label,
  value,
  limit,
  unit,
}: {
  icon: React.ReactNode;
  label: string;
  value: number;
  limit: number;
  unit: string;
}) {
  // 0 means "no cap configured" everywhere in the backend, so it must render
  // as unlimited rather than as a limit of zero.
  const capped = limit > 0;
  const ratio = capped ? value / limit : 0;
  const color = !capped ? "gray" : ratio >= 1 ? "red" : ratio >= 0.8 ? "orange" : "green";

  return (
    <Flex align="center" gap="2">
      <Flex align="center" gap="1" style={{ color: "var(--gray-10)" }}>
        {icon}
        <Text size="1" color="gray">
          {label}
        </Text>
      </Flex>
      <Text size="3" weight="bold" style={{ fontVariantNumeric: "tabular-nums" }}>
        {formatCount(value)}
      </Text>
      {capped ? (
        <Badge color={color} variant="soft" size="1">
          / {formatCount(limit)} {unit}
        </Badge>
      ) : (
        <Text size="1" color="gray">
          {unit}
        </Text>
      )}
    </Flex>
  );
}

export default function LiveRateBar({
  queryKey,
  fetcher,
  title,
  variant = "card",
}: {
  queryKey: readonly unknown[];
  fetcher: () => Promise<Rate>;
  title?: string;
  // "inline" renders one line of caption text for a table toolbar and stays
  // empty while nothing is flowing and no cap is configured — a personal
  // account sits at 0 req/min almost all the time.
  variant?: "card" | "inline";
}) {
  const { t } = useTranslation();
  const { data } = useQuery({
    queryKey: [...queryKey, "live-rate"],
    queryFn: fetcher,
    refetchInterval: 5_000,
    placeholderData: (prev) => prev,
  });

  const rate = data ?? { rpm: 0, tpm: 0, rpm_limit: 0, tpm_limit: 0 };

  if (variant === "inline") {
    const capped = rate.rpm_limit > 0 || rate.tpm_limit > 0;
    if (!capped && rate.rpm === 0 && rate.tpm === 0) return null;
    const part = (v: number, limit: number, unit: string) =>
      limit > 0 ? `${formatCount(v)} / ${formatCount(limit)} ${unit}` : `${formatCount(v)} ${unit}`;
    return (
      <Text size="1" color="gray" className="jc-num" style={{ whiteSpace: "nowrap" }}>
        {t("keys.shared.liveRate.window")} · RPM {part(rate.rpm, rate.rpm_limit, "req/min")} · TPM {part(rate.tpm, rate.tpm_limit, "tok/min")}
      </Text>
    );
  }

  return (
    <Card mb="3">
      <Flex align="center" gap="4" wrap="wrap">
        <Text size="1" color="gray">
          {title ?? t("keys.shared.liveRate.title")}
        </Text>
        <Metric
          icon={<Activity size={13} />}
          label="RPM"
          value={rate.rpm}
          limit={rate.rpm_limit}
          unit="req/min"
        />
        <Metric
          icon={<Gauge size={13} />}
          label="TPM"
          value={rate.tpm}
          limit={rate.tpm_limit}
          unit="tok/min"
        />
        <Text size="1" color="gray">
          {t("keys.shared.liveRate.window")}
        </Text>
      </Flex>
    </Card>
  );
}
