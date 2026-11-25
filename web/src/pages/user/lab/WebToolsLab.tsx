import { useState, type FormEvent } from "react";
import { Button, Select, Text, TextField } from "@radix-ui/themes";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { FileText, Globe } from "lucide-react";
import {
  labWebToolsApi, webToolsPublicApi,
  type WebFetchResponse, type WebSearchRequest, type WebSearchResponse, type WebToolKind, type WebToolPrice,
} from "../../../api/webTools";
import { webToolErrorMessage } from "../../../lib/labWebTools";
import { LabMarkdown } from "./LabMarkdown";
import i18n, { localeTag } from "../../../i18n";

// 网页工具: call the platform search and page-fetch tools by hand. Each
// successful call is billed per call to the balance.

const FRESHNESS = ["any", "day", "week", "month", "year"] as const;

function priceLabel(price: string): string {
  const value = Number(price);
  return i18n.t("bench.webTools.perCall", { price: Number.isFinite(value) ? `¥${value}` : price });
}

function EngineSelect({ engines, value, onChange }: { engines: WebToolPrice[]; value: string; onChange: (v: string) => void }) {
  const { t } = useTranslation();
  return (
    <Select.Root value={value} onValueChange={onChange} size="2" disabled={engines.length === 0}>
      <Select.Trigger className="jc-webtools-engine" placeholder={t("bench.webTools.noEngine")} aria-label={t("bench.webTools.engine")} />
      <Select.Content position="popper">
        {engines.map((item) => (
          <Select.Item key={item.id} value={item.engine}>
            {item.name} · {priceLabel(item.price)}
          </Select.Item>
        ))}
      </Select.Content>
    </Select.Root>
  );
}

function useEngine(engines: WebToolPrice[]) {
  const [picked, setPicked] = useState("");
  const value = engines.some((item) => item.engine === picked) ? picked : (engines[0]?.engine ?? "");
  return [value, setPicked] as const;
}

function SearchPanel({ engines }: { engines: WebToolPrice[] }) {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const [engine, setEngine] = useEngine(engines);
  const [max, setMax] = useState("5");
  const [freshness, setFreshness] = useState("any");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<WebSearchResponse | null>(null);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const q = query.trim();
    if (!q || busy) return;
    setBusy(true);
    setError("");
    try {
      setResult(await labWebToolsApi.search({
        query: q,
        ...(engine ? { engine } : {}),
        max_results: Number(max),
        ...(freshness !== "any" ? { freshness: freshness as WebSearchRequest["freshness"] } : {}),
      }));
    } catch (err) {
      setError(webToolErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="jc-webtools-panel">
      <header className="jc-webtools-head">
        <Globe size={15} />
        <Text size="2" weight="medium">{t("bench.webTools.searchTitle")}</Text>
        <Text size="1" color="gray">{t("bench.webTools.searchSub")}</Text>
      </header>
      <form className="jc-webtools-form" onSubmit={submit}>
        <TextField.Root size="2" placeholder={t("bench.webTools.queryPlaceholder")} value={query} onChange={(event) => setQuery(event.target.value)} aria-label={t("bench.webTools.queryAria")} />
        <div className="jc-webtools-row">
          <EngineSelect engines={engines} value={engine} onChange={setEngine} />
          <Select.Root value={max} onValueChange={setMax} size="2">
            <Select.Trigger aria-label={t("bench.webTools.maxAria")} />
            <Select.Content position="popper">
              {Array.from({ length: 10 }, (_, i) => String(i + 1)).map((n) => <Select.Item key={n} value={n}>{t("bench.webTools.maxOption", { count: Number(n) })}</Select.Item>)}
            </Select.Content>
          </Select.Root>
          <Select.Root value={freshness} onValueChange={setFreshness} size="2">
            <Select.Trigger aria-label={t("bench.webTools.freshnessAria")} />
            <Select.Content position="popper">
              {FRESHNESS.map((item) => <Select.Item key={item} value={item}>{t(`bench.webTools.freshness.${item}`)}</Select.Item>)}
            </Select.Content>
          </Select.Root>
          <Button type="submit" size="2" loading={busy} disabled={!query.trim() || engines.length === 0}>{t("bench.webTools.search")}</Button>
        </div>
      </form>
      <div className="jc-webtools-body">
        {error && <Text size="2" color="red">{error}</Text>}
        {!error && result && (
          <>
            <Text size="1" color="gray" as="p" className="jc-webtools-meta">
              {t("bench.webTools.searchMeta", { engine: result.engine, count: result.results.length })}
            </Text>
            {result.results.length === 0 && <Text size="2" color="gray">{t("bench.webTools.noResults")}</Text>}
            <ol className="jc-webtools-results">
              {result.results.map((item, i) => (
                <li key={`${item.url}-${i}`}>
                  <a href={item.url} target="_blank" rel="noopener noreferrer">{item.title || item.url}</a>
                  <span className="jc-webtools-url jc-mono">{item.url}</span>
                  {item.snippet && <p>{item.snippet}</p>}
                  {item.published_at && <small>{item.published_at}</small>}
                </li>
              ))}
            </ol>
          </>
        )}
        {!error && !result && <Text size="2" color="gray">{t("bench.webTools.resultsHere")}</Text>}
      </div>
    </section>
  );
}

function FetchPanel({ engines }: { engines: WebToolPrice[] }) {
  const { t } = useTranslation();
  const [url, setUrl] = useState("");
  const [engine, setEngine] = useEngine(engines);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<WebFetchResponse | null>(null);
  const valid = /^https?:\/\/\S+$/i.test(url.trim());

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!valid || busy) return;
    setBusy(true);
    setError("");
    try {
      setResult(await labWebToolsApi.fetch({ url: url.trim(), ...(engine ? { engine } : {}) }));
    } catch (err) {
      setError(webToolErrorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="jc-webtools-panel">
      <header className="jc-webtools-head">
        <FileText size={15} />
        <Text size="2" weight="medium">{t("bench.webTools.fetchTitle")}</Text>
        <Text size="1" color="gray">{t("bench.webTools.fetchSub")}</Text>
      </header>
      <form className="jc-webtools-form" onSubmit={submit}>
        <TextField.Root size="2" placeholder="https://" value={url} onChange={(event) => setUrl(event.target.value)} aria-label={t("bench.webTools.urlAria")} />
        <div className="jc-webtools-row">
          <EngineSelect engines={engines} value={engine} onChange={setEngine} />
          <Button type="submit" size="2" loading={busy} disabled={!valid || engines.length === 0}>{t("bench.webTools.fetch")}</Button>
        </div>
        {url.trim() && !valid && <Text size="1" color="red">{t("bench.webTools.urlInvalid")}</Text>}
      </form>
      <div className="jc-webtools-body">
        {error && <Text size="2" color="red">{error}</Text>}
        {!error && result && (
          <>
            <div className="jc-webtools-page-head">
              <a href={result.url} target="_blank" rel="noopener noreferrer">{result.title || result.url}</a>
              <Text size="1" color="gray" as="p" className="jc-webtools-meta">
                {result.status != null && t("bench.webTools.status", { status: result.status })}
                {t("bench.webTools.engineMeta", { engine: result.engine })}
                {result.published_at && <> · {result.published_at}</>}
                {" "}· {t("bench.webTools.chars", { count: result.content.length.toLocaleString(localeTag()) })}
              </Text>
            </div>
            <div className="jc-chatbox-md jc-webtools-page">
              <LabMarkdown>{result.content || t("bench.webTools.noContent")}</LabMarkdown>
            </div>
          </>
        )}
        {!error && !result && <Text size="2" color="gray">{t("bench.webTools.contentHere")}</Text>}
      </div>
    </section>
  );
}

export function WebToolsLab({ enabled }: { enabled: boolean }) {
  const { t } = useTranslation();
  const { data: prices = [], isLoading, error } = useQuery({
    queryKey: ["web-tool-prices"],
    queryFn: webToolsPublicApi.prices,
    enabled,
  });
  const of = (kind: WebToolKind) => prices.filter((item) => item.kind === kind);
  return (
    <div className="jc-webtools">
      {(error || (!isLoading && enabled && prices.length === 0)) && (
        <Text size="2" color={error ? "red" : "gray"} as="p" className="jc-webtools-note">
          {error ? t("bench.webTools.priceFailed", { error: webToolErrorMessage(error) }) : t("bench.webTools.closed")}
        </Text>
      )}
      <Text size="1" color="gray" as="p" className="jc-webtools-note">{t("bench.webTools.billingNote")}</Text>
      <div className="jc-webtools-grid">
        <SearchPanel engines={of("search")} />
        <FetchPanel engines={of("fetch")} />
      </div>
    </div>
  );
}
