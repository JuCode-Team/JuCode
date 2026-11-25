import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Dialog, Flex, Text, TextField } from "@radix-ui/themes";
import { useTranslation } from "react-i18next";
import { registerStepUpPrompt } from "../api/stepup";

// Collects a fresh two-factor code when the backend asks for one. Mounted
// once at the app root; the axios interceptor drives it, so no call site has
// to know which endpoints are guarded.
export default function TotpStepUpDialog() {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [retry, setRetry] = useState(false);
  const [code, setCode] = useState("");
  const resolver = useRef<((v: string | null) => void) | null>(null);

  const settle = useCallback((value: string | null) => {
    const resolve = resolver.current;
    resolver.current = null;
    setOpen(false);
    setCode("");
    resolve?.(value);
  }, []);

  useEffect(() => {
    registerStepUpPrompt(({ retry: isRetry }) =>
      new Promise<string | null>((resolve) => {
        resolver.current = resolve;
        setRetry(isRetry);
        setCode("");
        setOpen(true);
      }),
    );
    return () => registerStepUpPrompt(null);
  }, []);

  // Closing by escape or overlay click must resolve too, or the request that
  // opened the dialog would hang forever.
  const onOpenChange = (next: boolean) => {
    if (!next && resolver.current) settle(null);
    else setOpen(next);
  };

  const submit = () => {
    const value = code.trim();
    if (value.length !== 6) return;
    settle(value);
  };

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Content style={{ maxWidth: 380 }}>
        <Dialog.Title>{t("keys.shared.totp.title")}</Dialog.Title>
        <Dialog.Description size="2" color="gray" mb="4">
          {t("keys.shared.totp.body")}
        </Dialog.Description>

        <TextField.Root
          value={code}
          onChange={(e) => {
            // Authenticator apps and password managers paste with spaces.
            const digits = e.target.value.replace(/\D/g, "").slice(0, 6);
            setCode(digits);
          }}
          onKeyDown={(e) => { if (e.key === "Enter") submit(); }}
          placeholder="000000"
          inputMode="numeric"
          autoComplete="one-time-code"
          autoFocus
          size="3"
          style={{ letterSpacing: "0.3em", fontVariantNumeric: "tabular-nums" }}
        />

        {retry && (
          <Text size="1" color="red" as="p" mt="2">
            {t("keys.shared.totp.invalid")}
          </Text>
        )}

        <Flex gap="3" mt="4" justify="end">
          <Button variant="soft" color="gray" onClick={() => settle(null)}>{t("keys.shared.cancel")}</Button>
          <Button onClick={submit} disabled={code.length !== 6}>{t("keys.shared.totp.verify")}</Button>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
