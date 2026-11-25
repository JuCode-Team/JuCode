import { useEffect, useState } from "react";
import { Badge, Button, Callout, Card, Code, Flex, Switch, Table, Text } from "@radix-ui/themes";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Info } from "lucide-react";
import { Link } from "react-router-dom";

import { errMsg } from "../../api/client";
import { webToolsAdminApi, type WebToolsAdmin, type WebToolsUpdate } from "../../api/webTools";
import { ClearableTextField } from "../../components/ClearableTextField";
import { AdminMetricCard, AdminMetricGrid, AdminPage } from "../../components/AdminPage";

// WebTools configures the platform's web search / page fetch API (/tools/v1).
// Engines are a fixed set; each has its own per-call price and switch. The
// vendor keys live on the accounts page as ordinary accounts (protocol "web"),
// matched to engines by vendor.
type Draft = WebToolsUpdate;

function draftFrom(data: WebToolsAdmin): Draft {
  return {
    engines: Object.fromEntries(data.engines.map((e) => [e.id, { enabled: e.enabled, price: e.price }])),
    user_rpm: data.user_rpm,
  };
}

const PRICE_PATTERN = /^\d+(\.\d{1,8})?$/;

export default function WebTools() {
  const qc = useQueryClient();
  const query = useQuery({ queryKey: ["admin", "web-tools"], queryFn: () => webToolsAdminApi.get() });
  const [draft, setDraft] = useState<Draft | null>(null);
  const [rpmText, setRpmText] = useState("");

  useEffect(() => {
    if (query.data && !draft) {
      setDraft(draftFrom(query.data));
      setRpmText(String(query.data.user_rpm));
    }
  }, [query.data, draft]);

  const save = useMutation({
    mutationFn: (b: WebToolsUpdate) => webToolsAdminApi.update(b),
    onSuccess: (data) => {
      qc.setQueryData(["admin", "web-tools"], data);
      setDraft(draftFrom(data));
      setRpmText(String(data.user_rpm));
    },
  });

  const data = query.data;
  const badPrice = draft ? Object.entries(draft.engines).find(([, e]) => !PRICE_PATTERN.test(e.price.trim())) : undefined;
  const rpm = Number(rpmText);
  const badRpm = !Number.isInteger(rpm) || rpm < 1 || rpm > 100000;
  const dirty = !!data && !!draft && JSON.stringify(draftFrom(data)) !== JSON.stringify({ ...draft, user_rpm: rpm });

  const setEngine = (id: string, patch: Partial<{ enabled: boolean; price: string }>) => {
    setDraft((d) => (d ? { ...d, engines: { ...d.engines, [id]: { ...d.engines[id], ...patch } } } : d));
  };

  const vendors = data ? Object.entries(data.accounts) : [];
  const schedulable = vendors.reduce((n, [, a]) => n + a.schedulable, 0);
  const total = vendors.reduce((n, [, a]) => n + a.total, 0);

  return (
    <AdminPage
      title="网页工具"
      description={<>平台提供的网页搜索与抓取接口（<Code>/tools/v1/search</Code>、<Code>/tools/v1/fetch</Code>）。每个引擎单独设置按次价格和开关，费用从用户余额扣除，不走模型和分组。</>}
      actions={
        <Button
          disabled={!dirty || !!badPrice || badRpm}
          loading={save.isPending}
          onClick={() => draft && save.mutate({ engines: draft.engines, user_rpm: rpm })}
        >
          保存
        </Button>
      }
    >
      {data && draft && (
        <>
          <AdminMetricGrid>
            <AdminMetricCard label="已开启引擎" value={`${data.engines.filter((e) => e.enabled).length} / ${data.engines.length}`} />
            <AdminMetricCard
              label="可调度厂商账号"
              value={`${schedulable} / ${total}`}
              tone={total === 0 ? "amber" : schedulable === 0 ? "red" : "green"}
              detail={total === 0 ? "还没有厂商账号" : undefined}
            />
            <AdminMetricCard label="每用户每分钟上限" value={data.user_rpm} />
          </AdminMetricGrid>

          {save.isError && (
            <Callout.Root color="red" mb="3"><Callout.Text>保存失败：{errMsg(save.error)}</Callout.Text></Callout.Root>
          )}

          <Card className="pn-card-flat" mb="4">
            <Table.Root size="2">
              <Table.Header>
                <Table.Row>
                  <Table.ColumnHeaderCell>引擎</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell>厂商账号（可调度 / 全部）</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell width="180px">单价（元 / 次）</Table.ColumnHeaderCell>
                  <Table.ColumnHeaderCell width="80px">开启</Table.ColumnHeaderCell>
                </Table.Row>
              </Table.Header>
              <Table.Body>
                {data.engines.map((e) => {
                  const d = draft.engines[e.id];
                  const priceOk = PRICE_PATTERN.test(d.price.trim());
                  return (
                    <Table.Row key={e.id} align="center">
                      <Table.Cell>
                        <Flex direction="column" gap="1">
                          <Text weight="medium">{e.name}</Text>
                          <Text size="1" color="gray"><Code variant="ghost">{e.id}</Code></Text>
                        </Flex>
                      </Table.Cell>
                      <Table.Cell>
                        <Flex gap="2" wrap="wrap">
                          {e.vendors.map((v) => {
                            const a = data.accounts[v] ?? { total: 0, schedulable: 0 };
                            return (
                              <Badge key={v} variant="soft" color={a.schedulable > 0 ? "green" : a.total > 0 ? "red" : "gray"}>
                                {v} {a.schedulable} / {a.total}
                              </Badge>
                            );
                          })}
                        </Flex>
                      </Table.Cell>
                      <Table.Cell>
                        <ClearableTextField
                          value={d.price}
                          onChange={(ev) => setEngine(e.id, { price: ev.target.value })}
                          color={priceOk ? undefined : "red"}
                          className="jc-mono"
                        />
                      </Table.Cell>
                      <Table.Cell>
                        <Switch checked={d.enabled} onCheckedChange={(v) => setEngine(e.id, { enabled: v })} />
                      </Table.Cell>
                    </Table.Row>
                  );
                })}
              </Table.Body>
            </Table.Root>
          </Card>

          <Card className="pn-card-flat" mb="4">
            <Flex align="center" gap="3" wrap="wrap">
              <Text weight="medium">每用户每分钟调用上限</Text>
              <ClearableTextField
                value={rpmText}
                onChange={(ev) => setRpmText(ev.target.value)}
                color={badRpm ? "red" : undefined}
                style={{ width: 120 }}
                className="jc-mono"
              />
              <Text size="1" color="gray">搜索与抓取合计计数，超出返回 429。API Key 的花费上限照常生效。</Text>
            </Flex>
          </Card>

          <Callout.Root color="gray" variant="surface">
            <Callout.Icon><Info size={16} /></Callout.Icon>
            <Callout.Text>
              厂商 Key 在<Link to="/admin/accounts">账号管理</Link>里添加：新建账号时选择 Parallel、Brave Search、Jina Reader 或 Firecrawl。
              同一厂商可以添加多个账号，按优先级和权重轮询，代理、并发上限与连通性测试和其他账号相同。
              「自动」搜索由 Parallel 和 Brave 的账号共同提供，一家出错时换另一家。
            </Callout.Text>
          </Callout.Root>
        </>
      )}
      {query.isError && (
        <Callout.Root color="red"><Callout.Text>加载失败：{errMsg(query.error)}</Callout.Text></Callout.Root>
      )}
    </AdminPage>
  );
}
