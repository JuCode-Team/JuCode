import { Box, Flex, SegmentedControl, Switch, Text } from "@radix-ui/themes";
import { useTranslation } from "react-i18next";
import { ClearableTextField } from "./ClearableTextField";
import { keptPins, overlappingModels, type AccessibleGroup } from "../lib/groupAccess";

export type RouteMode = "price" | "stable" | "fast";
export type ModelPinValue = { model: string; group_id: string };

export function AutoRouteFields({
  checked,
  onCheckedChange,
  maxRate,
  onMaxRateChange,
  mode,
  onModeChange,
  groups,
  selectedIds,
  pins,
  onPinsChange,
}: {
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  maxRate: string;
  onMaxRateChange: (value: string) => void;
  mode: RouteMode;
  onModeChange: (mode: RouteMode) => void;
  groups: AccessibleGroup[];
  selectedIds: string[];
  pins: ModelPinValue[];
  onPinsChange: (pins: ModelPinValue[]) => void;
}) {
  const { t } = useTranslation();
  const overlaps = overlappingModels(groups, selectedIds);
  const visiblePins = keptPins(pins, overlaps);
  const setPin = (model: string, groupId: string) => {
    const rest = visiblePins.filter((pin) => pin.model !== model);
    onPinsChange(groupId ? [...rest, { model, group_id: groupId }] : rest);
  };
  return (
    <div className="jc-field jc-route">
      <label className="jc-field-inline">
        <span className="jc-route-head">
          <span className="jc-field-label">{t("keys.formExtras.autoRoute")}</span>
          <span className="jc-field-hint">{t("keys.formExtras.autoRouteHint")}</span>
        </span>
        <Switch checked={checked} onCheckedChange={onCheckedChange} />
      </label>
      {checked && (
        <Flex direction="column" gap="2">
          <SegmentedControl.Root value={mode} onValueChange={(value) => onModeChange(value as RouteMode)} size="1">
            <SegmentedControl.Item value="price">{t("keys.apiKeys.route.price")}</SegmentedControl.Item>
            <SegmentedControl.Item value="stable">{t("keys.apiKeys.route.stable")}</SegmentedControl.Item>
            <SegmentedControl.Item value="fast">{t("keys.apiKeys.route.fast")}</SegmentedControl.Item>
          </SegmentedControl.Root>
          <Text size="1" color="gray" as="p">
            {mode === "stable"
              ? t("keys.formExtras.stableHint")
              : mode === "fast"
                ? t("keys.formExtras.fastHint")
                : t("keys.formExtras.priceHint")}
          </Text>
          {mode === "price" && (
            <Flex align="center" gap="2">
              <Text size="1" color="gray" as="label" htmlFor="jc-route-max-rate">{t("keys.formExtras.maxRate")}</Text>
              <Box style={{ width: 120 }}>
                <ClearableTextField
                  id="jc-route-max-rate"
                  type="number"
                  step="0.1"
                  min="0"
                  value={maxRate}
                  onChange={(event) => onMaxRateChange(event.target.value)}
                />
              </Box>
            </Flex>
          )}
          {selectedIds.length === 0 ? (
            <Text size="1" color="gray" as="p">{t("keys.formExtras.pinNoSelection")}</Text>
          ) : overlaps.length === 0 ? (
            <Text size="1" color="gray" as="p">{t("keys.formExtras.pinNoOverlap")}</Text>
          ) : (
            <div className="jc-route-models">
              <Text size="1" color="gray" as="p">{t("keys.formExtras.pinIntro")}</Text>
              {overlaps.map((row) => (
                <label key={row.model} className="jc-route-model">
                  <span>{row.model}</span>
                  <select
                    value={visiblePins.find((pin) => pin.model === row.model)?.group_id ?? ""}
                    onChange={(event) => setPin(row.model, event.target.value)}
                  >
                    <option value="">{t("keys.formExtras.auto")}</option>
                    {row.groups.map((group) => (
                      <option key={group.id} value={group.id}>{group.name}</option>
                    ))}
                  </select>
                </label>
              ))}
            </div>
          )}
        </Flex>
      )}
    </div>
  );
}

export type CostLimitValues = { daily: string; monthly: string; total: string };
export const emptyCostLimits: CostLimitValues = { daily: "", monthly: "", total: "" };
export const validCostLimits = (values: CostLimitValues) => Object.values(values).every((value) => value.trim() === "" || (Number.isFinite(Number(value)) && Number(value) >= 0));
export const costLimitPayload = (values: CostLimitValues) => ({
  daily_cost_limit: values.daily.trim() || null,
  monthly_cost_limit: values.monthly.trim() || null,
  total_cost_limit: values.total.trim() || null,
});

export function CostLimitFields({ values, onChange }: { values: CostLimitValues; onChange: (values: CostLimitValues) => void }) {
  const { t } = useTranslation();
  return (
    <Box>
      <Text size="1" color="gray" as="p" mb="1">{t("keys.apiKeys.form.costLimit")}</Text>
      <Flex gap="2" wrap="wrap">
        {(["daily", "monthly", "total"] as const).map((key) => ({ key, label: t(`keys.apiKeys.period.${key}`) })).map(({ key, label }) => (
          <Box key={key} style={{ flex: "1 1 110px" }}>
            <ClearableTextField type="number" min="0" step="any" placeholder={label} value={values[key]} onChange={(event) => onChange({ ...values, [key]: event.target.value })} />
          </Box>
        ))}
      </Flex>
    </Box>
  );
}
