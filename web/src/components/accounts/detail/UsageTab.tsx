// UsageTab: 用量 — the account's per-model traffic for a window, aggregated
// from usage_logs by (model, upstream_model). Token columns are raw sums;
// 成本 is the account cost basis (what the binding prices say we owe the
// upstream), 收取 is what users were charged. Both CNY.
import { useState } from "react";
import { Flex, Table, Text } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { adminApi } from "../../../api/endpoints";
import { errMsg } from "../../../api/client";
import { DateRangeInput, last30DaysRange, rangeToIso } from "../../UsageLogFilters";

function money(amount: string): string {
  const n = Number(amount);
  const value = Number.isFinite(n)
    ? n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
    : amount;
  return `¥${value}`;
}

export function UsageTab({ accountId }: { accountId: string }) {
  const [range, setRange] = useState(() => last30DaysRange());
  const params = rangeToIso(range.start, range.end);
  const usage = useQuery({
    queryKey: ["admin", "account", accountId, "usage", params.from, params.to],
    queryFn: () => adminApi.accountUsage(accountId, params),
  });
  const rows = usage.data?.rows ?? [];

  return (
    <Flex direction="column" gap="3">
      <Flex justify="between" align="center" gap="2" wrap="wrap">
        <Text size="1" color="gray">
          按模型聚合该账号服务的请求；成本按绑定价格表折算，收取是用户实际付费。
        </Text>
        <DateRangeInput
          start={range.start}
          setStart={(v) => setRange((r) => ({ ...r, start: v }))}
          end={range.end}
          setEnd={(v) => setRange((r) => ({ ...r, end: v }))}
        />
      </Flex>
      {usage.isError && <Text size="2" color="red">{errMsg(usage.error)}</Text>}
      <div className="jc-local-table-scroll">
        <Table.Root variant="surface" size="1">
          <Table.Header>
            <Table.Row>
              <Table.ColumnHeaderCell>模型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell>上游模型</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">请求</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">输入</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">输出</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">缓存读</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">缓存写</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">图片</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">成本</Table.ColumnHeaderCell>
              <Table.ColumnHeaderCell align="right">收取</Table.ColumnHeaderCell>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {rows.map((r) => (
              <Table.Row key={`${r.model}:${r.upstream_model}`}>
                <Table.Cell><Text className="jc-mono" size="1">{r.model}</Text></Table.Cell>
                <Table.Cell><Text className="jc-mono" size="1" color="gray">{r.upstream_model || "-"}</Text></Table.Cell>
                <Table.Cell align="right" className="jc-num">{r.requests.toLocaleString()}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{r.tokens_in.toLocaleString()}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{r.tokens_out.toLocaleString()}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{r.cached_tokens_in.toLocaleString()}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{r.cache_creation_tokens_in.toLocaleString()}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{r.output_images.toLocaleString()}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{money(r.cost)}</Table.Cell>
                <Table.Cell align="right" className="jc-num">{money(r.revenue)}</Table.Cell>
              </Table.Row>
            ))}
            {!usage.isLoading && rows.length === 0 && (
              <Table.Row>
                <Table.Cell colSpan={10}>
                  <Text size="2" color="gray">所选时间范围内没有用量记录。</Text>
                </Table.Cell>
              </Table.Row>
            )}
          </Table.Body>
        </Table.Root>
      </div>
    </Flex>
  );
}
