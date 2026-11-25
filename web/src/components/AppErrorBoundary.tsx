import { Component, useEffect, useState, type ErrorInfo, type ReactNode } from "react";
import { Button, Card, Text } from "@radix-ui/themes";
import { subscribeToast, type AppToast } from "../lib/appToast";
import i18n from "../i18n";

function errorMessage(error: unknown): string {
  if (error instanceof Error && error.message) return error.message;
  if (typeof error === "string") return error;
  return i18n.t("keys.shared.errorBoundary.fallback");
}

export function GlobalErrorToast() {
  const [toast, setToast] = useState<AppToast | null>(null);

  useEffect(() => {
    let timer: number | null = null;
    const show = (next: AppToast) => {
      setToast(next);
      if (timer != null) window.clearTimeout(timer);
      timer = window.setTimeout(() => setToast(null), 6000);
    };
    const showError = (error: unknown) => show({ title: i18n.t("keys.shared.errorBoundary.title"), message: errorMessage(error) });
    const onError = (event: ErrorEvent) => showError(event.error || event.message);
    const onUnhandledRejection = (event: PromiseRejectionEvent) => showError(event.reason);

    window.addEventListener("error", onError);
    window.addEventListener("unhandledrejection", onUnhandledRejection);
    const unsubscribe = subscribeToast(show);
    return () => {
      window.removeEventListener("error", onError);
      window.removeEventListener("unhandledrejection", onUnhandledRejection);
      unsubscribe();
      if (timer != null) window.clearTimeout(timer);
    };
  }, []);

  if (!toast) return null;
  return (
    <div className="jc-error-toast" role="alert">
      <Text size="2" weight="medium" as="div">{toast.title}</Text>
      <Text size="1" color="gray" as="div">{toast.message}</Text>
    </div>
  );
}

export class AppErrorBoundary extends Component<
  { children: ReactNode; resetKey?: string },
  { error: unknown }
> {
  state = { error: null as unknown };

  static getDerivedStateFromError(error: unknown) {
    return { error };
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    console.error("React render error", error, info);
  }

  componentDidUpdate(prevProps: { resetKey?: string }) {
    if (this.state.error && prevProps.resetKey !== this.props.resetKey) {
      this.setState({ error: null });
    }
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="jc-error-fallback">
        <div className="jc-error-toast" role="alert">
          <Text size="2" weight="medium" as="div">{i18n.t("keys.shared.errorBoundary.title")}</Text>
          <Text size="1" color="gray" as="div">{errorMessage(this.state.error)}</Text>
        </div>
        <Card className="pn-card-flat jc-error-fallback-card">
          <Text size="4" weight="bold" as="div">{i18n.t("keys.shared.errorBoundary.renderFailed")}</Text>
          <Text size="2" color="gray" as="div" mt="2">
            {i18n.t("keys.shared.errorBoundary.body")}
          </Text>
          <Button mt="4" onClick={() => window.location.reload()}>{i18n.t("keys.shared.errorBoundary.reload")}</Button>
        </Card>
      </div>
    );
  }
}
