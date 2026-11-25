import { useState, type CSSProperties } from "react";
import { Code, Tooltip } from "@radix-ui/themes";
import { Check, Copy } from "lucide-react";
import { useTranslation } from "react-i18next";
import { copyText } from "../lib/copy";

// CopyableId renders an id (request id, account id, …) as a click-to-copy
// chip. It copies the full `value` while displaying the possibly-shortened
// `display`, and flips to a check mark + "已复制" tooltip for a moment on
// success. stopPropagation keeps a click from also toggling an enclosing
// expandable row.
export default function CopyableId({
  value,
  display,
  size = 11,
  color = "iris",
  label,
  style,
}: {
  value: string;
  display?: string;
  size?: number;
  color?: React.ComponentProps<typeof Code>["color"];
  label?: string;
  style?: CSSProperties;
}) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  const onCopy = async (e: React.MouseEvent) => {
    e.stopPropagation();
    const ok = await copyText(value);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1200);
    }
  };

  return (
    <Tooltip content={copied ? t("keys.shared.copied") : label ?? t("keys.shared.clickToCopy")}>
      <Code
        color={copied ? "green" : color}
        onClick={onCopy}
        style={{
          fontSize: size,
          cursor: "pointer",
          display: "inline-flex",
          alignItems: "center",
          gap: 4,
          ...style,
        }}
      >
        {copied ? <Check size={size} /> : <Copy size={size} />}
        {display ?? value}
      </Code>
    </Tooltip>
  );
}
