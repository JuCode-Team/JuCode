import { IconButton, TextField, Tooltip } from "@radix-ui/themes";
import { X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { type ChangeEvent, type ComponentProps } from "react";

type TextFieldRootProps = ComponentProps<typeof TextField.Root>;

type ClearableTextFieldProps = TextFieldRootProps & {
  clearable?: boolean;
  clearLabel?: string;
  onClear?: () => void;
};

const nonClearableTypes = new Set(["hidden"]);

export function ClearableTextField({
  children,
  clearable = true,
  clearLabel: clearLabelProp,
  disabled,
  onChange,
  onClear,
  readOnly,
  type,
  value,
  ...props
}: ClearableTextFieldProps) {
  const { t } = useTranslation();
  const clearLabel = clearLabelProp ?? t("keys.shared.clear");
  const hasValue = value !== undefined && String(value).length > 0;
  const showClear = clearable && hasValue && !disabled && !readOnly && !nonClearableTypes.has(type ?? "text");

  const clear = () => {
    if (onClear) {
      onClear();
      return;
    }

    onChange?.({
      target: { value: "" },
      currentTarget: { value: "" },
    } as ChangeEvent<HTMLInputElement>);
  };

  return (
    <TextField.Root
      {...props}
      disabled={disabled}
      onChange={onChange}
      readOnly={readOnly}
      type={type}
      value={value}
    >
      {children}
      {showClear && (
        <TextField.Slot side="right">
          <Tooltip content={clearLabel}>
            <IconButton
              aria-label={clearLabel}
              color="gray"
              size="1"
              type="button"
              variant="ghost"
              onClick={clear}
              onMouseDown={(event) => event.preventDefault()}
            >
              <X size={13} />
            </IconButton>
          </Tooltip>
        </TextField.Slot>
      )}
    </TextField.Root>
  );
}
