// CredentialTab: 凭据 — the API key the account logs in with. This tab
// rotates the secret.
import { ClearableTextField } from "../../ClearableTextField";
import type { AccountForm, SetFormField } from "../AccountFormFields";
import { Field, FieldGrid, FormSection } from "../../form/FormLayout";

export function CredentialTab({
  form, set,
}: {
  form: AccountForm;
  set: SetFormField;
}) {
  return (
    <FormSection
      title="登录凭据"
      description="账号用 API Key 登录。"
    >
      <FieldGrid columns={1}>
        <Field label="API Key" code="api_key" hint="加密存储，永不回显。留空保持当前，填入新值即覆盖。">
          <ClearableTextField
            type="password"
            value={form.api_key}
            onChange={(e) => set("api_key", e.target.value)}
            placeholder="留空保持当前"
            className="jc-mono"
          />
        </Field>
      </FieldGrid>
    </FormSection>
  );
}
