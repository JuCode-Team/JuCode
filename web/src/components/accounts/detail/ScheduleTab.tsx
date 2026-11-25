// ScheduleTab: 调度 — scheduling knobs (priority, weight, concurrency, pool
// mode, enabled).
import { Select } from "@radix-ui/themes";
import { type Account, type PoolMode } from "../../../api/endpoints";
import { ClearableTextField } from "../../ClearableTextField";
import { POOL_MODE_LABEL, POOL_MODE_OPTIONS } from "../AccountHealthBadges";
import type { AccountForm, SetFormField } from "../AccountFormFields";
import { Field, FieldGrid, FormSection, ToggleRow } from "../../form/FormLayout";

export function ScheduleTab({
  account, form, set,
}: {
  account: Account;
  form: AccountForm;
  set: SetFormField;
}) {
  return (
    <>
      <FormSection title="调度" description="决定这个账号在同组账号里的尝试顺序与承载量。">
        <FieldGrid columns={3}>
          <Field label="优先级" code="priority" hint="数值小者先试。">
            <ClearableTextField type="number" value={form.priority} onChange={(e) => set("priority", e.target.value)} />
          </Field>
          <Field label="权重" code="weight" hint="同优先级内按权重分流。">
            <ClearableTextField type="number" value={form.weight} onChange={(e) => set("weight", e.target.value)} />
          </Field>
          <Field label="并发上限" code="max_concurrency" hint="0 不限。">
            <ClearableTextField type="number" value={form.max_concurrency} onChange={(e) => set("max_concurrency", e.target.value)} />
          </Field>
        </FieldGrid>
        <FieldGrid>
          <Field
            label="健康策略"
            code="pool_mode"
            hint={`当前生效：${POOL_MODE_LABEL[account.effective_pool_mode] ?? account.effective_pool_mode}${account.pool_mode ? "（账号覆盖）" : "（站点默认）"}`}
          >
            <Select.Root value={form.pool_mode || "inherit"} onValueChange={(v) => set("pool_mode", (v === "inherit" ? "" : v) as PoolMode)}>
              <Select.Trigger />
              <Select.Content>
                {POOL_MODE_OPTIONS.map((o) => (
                  <Select.Item key={o.value || "inherit"} value={o.value || "inherit"}>{o.label}</Select.Item>
                ))}
              </Select.Content>
            </Select.Root>
          </Field>
          <Field label="备注">
            <ClearableTextField value={form.note} onChange={(e) => set("note", e.target.value)} />
          </Field>
        </FieldGrid>
        <div className="jc-toggle-rows">
          <ToggleRow
            checked={form.enabled}
            onCheckedChange={(v) => set("enabled", v)}
            label="参与调度"
            hint={form.enabled ? "启用中，请求会路由到这个账号。" : "已停用：保留配置，不参与调度。"}
          />
        </div>
      </FormSection>

    </>
  );
}
