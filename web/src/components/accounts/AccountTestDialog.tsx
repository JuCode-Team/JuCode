// AccountTestDialog: one-shot health probe — fires a real request through
// this account + protocol + model straight at the account's own upstream
// (POST /accounts/:id/test). The verdict answers "does THIS account serve
// this model on this protocol right now"; it touches no health state,
// cooldown or billing. A web tool account (protocol "web") is probed with one
// real search or page fetch instead of a model.
import { useEffect, useState } from "react";
import {
  Badge, Box, Button, Callout, Dialog, Flex, Select, Text,
} from "@radix-ui/themes";
import { useMutation, useQuery } from "@tanstack/react-query";
import { CheckCircle2, FlaskConical, XCircle } from "lucide-react";
import { adminApi, type Account, type AccountProtocol } from "../../api/endpoints";
import { errMsg } from "../../api/client";
import { ClearableTextField } from "../ClearableTextField";
import { accountLabel, protocolLabel } from "./AccountHealthBadges";
import { Field } from "./AccountFormFields";

const FALLBACK_PROTOCOLS: AccountProtocol[] = ["chat_completions"];

// Which web tool kinds each vendor (api_format) serves.
const WEB_KINDS: Record<string, { value: string; label: string }[]> = {
  parallel: [{ value: "search", label: "网页搜索" }, { value: "fetch", label: "网页抓取" }],
  brave: [{ value: "search", label: "网页搜索" }],
  jina: [{ value: "fetch", label: "网页抓取" }],
  firecrawl: [{ value: "fetch", label: "网页抓取" }],
};

export function AccountTestDialog({
  account, open, onOpenChange,
}: {
  account: Account | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const protocols = account?.native_protocols?.length ? account.native_protocols : FALLBACK_PROTOCOLS;
  const [protocol, setProtocol] = useState<AccountProtocol>(protocols[0]);
  const [model, setModel] = useState("");
  const webKinds = WEB_KINDS[account?.api_format ?? ""] ?? [];
  const [webKind, setWebKind] = useState("");
  const isWeb = protocol === "web";

  // Re-arm the defaults whenever the dialog targets a different account —
  // the previous verdict does not carry over.
  useEffect(() => {
    setProtocol(
      account?.default_protocol && protocols.includes(account.default_protocol as AccountProtocol)
        ? (account.default_protocol as AccountProtocol)
        : protocols[0],
    );
    setWebKind(webKinds[0]?.value ?? "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [account?.id]);

  const models = useQuery({
    queryKey: ["admin", "account-models", account?.id],
    queryFn: () => adminApi.accountModels(account!.id),
    enabled: open && !!account && !isWeb,
  });
  const bound = (models.data ?? []).filter((m) => m.enabled);

  // Default the model to the first enabled binding once they load.
  useEffect(() => {
    if (!model && bound.length > 0) setModel(bound[0].model_name);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bound.length]);

  const test = useMutation({
    mutationFn: () => adminApi.testAccount(account!.id, { protocol, model: isWeb ? webKind : model.trim() }),
  });

  const result = test.data;

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Content style={{ maxWidth: 520 }}>
        <Dialog.Title>连通性测试</Dialog.Title>
        <Dialog.Description size="2">
          以该账号的身份向它自己的上游发一次真实请求（{account ? accountLabel(account) : ""}）。
          不经过调度，不影响健康状态、冷却与计费。
        </Dialog.Description>

        <Flex gap="3" align="end" wrap="wrap" mt="4">
          <Box style={{ width: 180 }}>
            <Field label="协议">
              <Select.Root value={protocol} onValueChange={(v) => setProtocol(v as AccountProtocol)}>
                <Select.Trigger style={{ width: "100%" }} />
                <Select.Content>
                  {protocols.map((p) => (
                    <Select.Item key={p} value={p}>{protocolLabel(p)}</Select.Item>
                  ))}
                </Select.Content>
              </Select.Root>
            </Field>
          </Box>
          {isWeb ? (
            <Box style={{ flex: 1, minWidth: 180 }}>
              <Field label="测试项目">
                <Select.Root value={webKind} onValueChange={setWebKind}>
                  <Select.Trigger style={{ width: "100%" }} placeholder="该厂商不提供网页工具" />
                  <Select.Content>
                    {webKinds.map((k) => (
                      <Select.Item key={k.value} value={k.value}>{k.label}</Select.Item>
                    ))}
                  </Select.Content>
                </Select.Root>
              </Field>
            </Box>
          ) : (
          <Box style={{ flex: 1, minWidth: 220 }}>
            <Field label="模型">
              <ClearableTextField
                value={model}
                onChange={(e) => setModel(e.target.value)}
                placeholder="选择已绑定模型或输入任意模型名"
                list="account-test-models"
              />
            </Field>
            <datalist id="account-test-models">
              {bound.map((m) => (
                <option
                  key={m.id}
                  value={m.model_name}
                  label={m.upstream_model ? `上游：${m.upstream_model}` : undefined}
                />
              ))}
            </datalist>
          </Box>
          )}
          <Button loading={test.isPending} disabled={isWeb ? !webKind : !model.trim()} onClick={() => test.mutate()}>
            <FlaskConical size={14} /> 发起测试
          </Button>
        </Flex>

        {isWeb && (
          <Text as="p" size="1" color="gray" mt="2">
            搜索测试查询「JuCode」取 3 条结果，抓取测试读取 https://example.com，厂商会按一次调用计费。
          </Text>
        )}

        {test.isError && (
          <Callout.Root color="red" mt="4">
            <Callout.Text>请求失败：{errMsg(test.error)}</Callout.Text>
          </Callout.Root>
        )}

        {result && (
          <Callout.Root color={result.ok ? "green" : "red"} mt="4">
            <Callout.Icon>
              {result.ok ? <CheckCircle2 size={16} /> : <XCircle size={16} />}
            </Callout.Icon>
            <Callout.Text>
              <Text weight="medium">{result.ok ? "正常" : "异常"}</Text>{" "}
              {result.status > 0 && <Badge variant="soft" color={result.ok ? "green" : "red"}>HTTP {result.status}</Badge>}{" "}
              <Text size="1" color="gray" className="jc-num">{result.latency_ms} ms</Text>
            </Callout.Text>
            <Flex direction="column" gap="1" mt="1">
              <Text size="1" color="gray" style={{ fontFamily: "ui-monospace, monospace", wordBreak: "break-all" }}>
                {result.protocol} · {result.model} · {result.url}
              </Text>
              {result.error && <Text size="1" style={{ whiteSpace: "pre-wrap" }}>{result.error}</Text>}
              {result.detail && (
                <Text size="1" style={{ fontFamily: "ui-monospace, monospace", whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                  {result.detail}
                </Text>
              )}
              {result.sample && (
                <Box mt="1">
                  <Text size="1" color="gray">返回内容</Text>
                  <Box
                    mt="1"
                    p="2"
                    style={{
                      background: "var(--gray-a3)", borderRadius: "var(--radius-2)", maxHeight: 180, overflow: "auto",
                      fontFamily: "ui-monospace, monospace", fontSize: 12, whiteSpace: "pre-wrap", wordBreak: "break-all",
                    }}
                  >
                    {result.sample}
                  </Box>
                </Box>
              )}
            </Flex>
          </Callout.Root>
        )}

        <Flex justify="end" mt="4">
          <Dialog.Close>
            <Button variant="soft" color="gray">关闭</Button>
          </Dialog.Close>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
