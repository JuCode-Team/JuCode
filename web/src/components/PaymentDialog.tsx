import { Badge, Box, Button, Card, Dialog, Flex, Spinner, Text } from "@radix-ui/themes";
import { useMutation } from "@tanstack/react-query";
import { CheckCircle2, Copy, ExternalLink, RefreshCw } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { meApi, type SiteOrder } from "../api/endpoints";
import { errMsg } from "../api/client";
import { copyText } from "../lib/copy";
import { useSiteStore } from "../store/site";
import { PointAmount } from "./PointAmount";

// 唯一的在线渠道是支付宝：电脑网站 / 手机网站支付，跳转收银台付款。
// 后端拒绝关闭状态下的新支付，所以开关以站点设置 payment_alipay_enabled 为准。

const ua = typeof navigator === "undefined" ? "" : navigator.userAgent;
const isMobileUA = /Android|iPhone|iPad|iPod|HarmonyOS|Mobile/i.test(ua);
// 微信和 QQ 的内置浏览器会拦掉支付宝的唤端跳转，支付宝官方也只能引导用户
// 转到系统浏览器，没有技术绕过手段。
const isInAppBrowser = /MicroMessenger|\bQQ\//i.test(ua);

function alipayCode(order: SiteOrder | null): string {
  return order?.payment_channel === "alipay" ? order.payment_code_url ?? "" : "";
}

function isPaid(order: SiteOrder | null): boolean {
  return order?.status === "paid" || order?.status === "partially_refunded" || order?.status === "refunded";
}

export default function PaymentDialog({
  order,
  open,
  onClose,
  onPaid,
}: {
  order: SiteOrder | null;
  open: boolean;
  onClose: () => void;
  onPaid: (order: SiteOrder) => void;
}) {
  const { t } = useTranslation();
  const enabled = useSiteStore((s) => !!s.site?.payment_alipay_enabled);
  const siteLoaded = useSiteStore((s) => s.loaded);
  const [current, setCurrent] = useState<SiteOrder | null>(order);
  const [codeURL, setCodeURL] = useState(alipayCode(order));

  useEffect(() => {
    setCurrent(order);
    setCodeURL(alipayCode(order));
  }, [order]);

  const prepay = useMutation({
    mutationFn: ({ id, returnURL }: { id: string; returnURL: string }) =>
      meApi.createAlipayPagePay(id, {
        return_url: returnURL,
        // 电脑网站支付在手机上唤不起支付宝 App，只会渲染成扫码页。
        client: isMobileUA ? "wap" : "pc",
        quit_url: returnURL,
      }),
    onSuccess: (res) => {
      setCurrent(res.order);
      setCodeURL(res.code_url || res.order.payment_code_url || "");
    },
  });

  const sync = useMutation({
    mutationFn: (id: string) => meApi.syncMyOrder(id),
    onSuccess: (o) => {
      setCurrent(o);
      if (o.payment_channel === "alipay" && o.payment_code_url) setCodeURL(o.payment_code_url);
      if (isPaid(o)) {
        onPaid(o);
      }
    },
  });

  useEffect(() => {
    if (!open || !current || current.status !== "pending") return;
    if (!codeURL) return;
    const poll = () => {
      if (!sync.isPending) sync.mutate(current.id);
    };
    const timer = window.setInterval(poll, 3000);
    // 跳去支付宝 App 期间页面被挂起，轮询停摆；iOS 付完还不会自动跳回来，
    // 用户手动切回时若不补一次同步，看到的仍是「待支付」。
    const onResume = () => {
      if (document.visibilityState === "visible") poll();
    };
    document.addEventListener("visibilitychange", onResume);
    window.addEventListener("pageshow", onResume);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", onResume);
      window.removeEventListener("pageshow", onResume);
    };
  }, [open, current, codeURL, sync]);

  const confirmPayment = async () => {
    if (!current || current.status !== "pending") return;
    if (isInAppBrowser) {
      // 跳不出去，先把链接拿到手，用户复制到系统浏览器打开。
      if (codeURL) sync.mutate(current.id);
      else await prepay.mutateAsync({ id: current.id, returnURL: window.location.href });
      return;
    }
    if (codeURL) {
      window.location.assign(codeURL);
      return;
    }
    const res = await prepay.mutateAsync({ id: current.id, returnURL: window.location.href });
    const nextURL = res.pay_url || res.code_url || res.order.payment_code_url || "";
    if (nextURL) window.location.assign(nextURL);
  };

  const paid = isPaid(current);
  const error = prepay.error || sync.error;
  const canStartPayment = !!current && current.status === "pending" && (enabled || !!codeURL);
  const confirmLabel = isInAppBrowser
    ? (codeURL ? t("billing.payment.refreshStatus") : t("billing.payment.generateLink"))
    : codeURL ? t("billing.payment.continueAlipay") : t("billing.payment.confirmAndRedirect");

  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content maxWidth="460px" className="jc-payment-dialog">
        <Dialog.Title>{t("billing.payment.title")}</Dialog.Title>
        <Flex direction="column" gap="4" mt="3">
          <Card variant="surface">
            <Flex direction="column" gap="3">
              <Flex width="100%" justify="between" align="start" gap="3">
                <Box>
                  <Text size="1" color="gray" as="div">{t("billing.payment.orderNo")}</Text>
                  <Text size="2" className="jc-num">{current?.order_no ?? "-"}</Text>
                </Box>
                <Badge color={paid ? "green" : "amber"} variant="soft">
                  {paid ? t("billing.payment.paid") : t("billing.payment.pending")}
                </Badge>
              </Flex>
              <Box>
                <Text size="1" color="gray" as="div">{t("billing.payment.amount")}</Text>
                <Text size="6" weight="bold" className="jc-num"><PointAmount amount={current?.amount ?? ""} decimals="preserve" iconSize={20} /></Text>
                {current?.credit_amount && Number(current.credit_amount) > Number(current.amount) && (
                  <Text size="1" color="green" as="div" mt="1">{t("billing.payment.creditAfterPay")} <PointAmount amount={current.credit_amount} iconSize={11} /></Text>
                )}
                {current?.coupon_code && (
                  <Text size="1" color="green" as="div" mt="1">
                    {t("billing.payment.coupon", { code: current.coupon_code })}
                    {current.coupon_discount_pct && Number(current.coupon_discount_pct) > 0
                      ? t("billing.payment.couponDiscount", { discount: current.coupon_discount_pct })
                      : ""}
                  </Text>
                )}
              </Box>
            </Flex>
          </Card>

          {paid ? (
            <Flex justify="center">
              <CheckCircle2 size={72} color="var(--green-9)" />
            </Flex>
          ) : isInAppBrowser ? (
            <Flex direction="column" align="center" gap="2">
              <Text size="2" align="center">{t("billing.payment.alipayBlocked")}</Text>
              <Text size="1" color="gray" align="center">
                {t("billing.payment.alipayBlockedHint")}
              </Text>
            </Flex>
          ) : siteLoaded && !enabled && !codeURL ? (
            <Text size="2" color="gray" align="center">
              {t("billing.payment.noChannel")}
            </Text>
          ) : prepay.isPending ? (
            <Flex justify="center">
              <Spinner size="3" />
            </Flex>
          ) : null}

          {isInAppBrowser && codeURL && !paid && (
            <Button size="2" variant="soft" color="gray" onClick={() => copyText(codeURL)}>
              <Copy size={14} />{t("billing.payment.copyLink")}
            </Button>
          )}
          {!isInAppBrowser && isMobileUA && codeURL && !paid && (
            <Button size="2" variant="ghost" color="gray" onClick={() => window.location.assign(codeURL)}>
              {t("billing.payment.retryRedirect")}
            </Button>
          )}
          {error && <Text size="2" color="red" align="center">{errMsg(error)}</Text>}
          <Flex className="jc-payment-actions" width="100%" justify="end" gap="2" mt="2" wrap="wrap">
            <Button variant="soft" color="gray" onClick={onClose}>{paid ? t("billing.payment.close") : t("billing.payment.payLater")}</Button>
            {!paid && (
              <Button
                loading={prepay.isPending || (isInAppBrowser && !!codeURL && sync.isPending)}
                disabled={!canStartPayment}
                onClick={confirmPayment}
              >
                {!isInAppBrowser && <ExternalLink size={14} />}
                {isInAppBrowser && !!codeURL && <RefreshCw size={14} />}
                {confirmLabel}
              </Button>
            )}
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
