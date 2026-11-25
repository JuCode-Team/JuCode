// CatalogPickerDialog: two-step picker over the cached public model catalog
// (models.dev) — searchable provider list → model checklist with context and
// reference pricing. Callers receive the picked RemoteCatalogModel rows and
// decide what to do with them (bind to an account, create platform models).
// `extra` renders above the footer on the model step for per-page options.
import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Box, Button, Checkbox, Dialog, Flex, Table, Text, TextField,
} from "@radix-ui/themes";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { adminApi, type RemoteCatalogModel, type RemoteCatalogProvider } from "../api/endpoints";
import { errMsg } from "../api/client";
import { localeTag } from "../i18n";

export function CatalogPickerDialog({
  open, onClose, onImport, onImported, title, hint, extra, merged,
}: {
  open: boolean;
  onClose: () => void;
  onImport: (models: RemoteCatalogModel[]) => Promise<unknown>;
  onImported: () => void;
  title?: string;
  hint?: string;
  extra?: ReactNode;
  // merged: skip the provider step and show one canonical model list —
  // every provider's models folded into a single entry per model name,
  // with the providers it was seen under listed on each row.
  merged?: boolean;
}) {
  const { t } = useTranslation();
  const [provider, setProvider] = useState<RemoteCatalogProvider | null>(null);
  const [providerQuery, setProviderQuery] = useState("");
  const [modelQuery, setModelQuery] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const providers = useQuery({
    queryKey: ["admin", "model-catalog", "providers"],
    queryFn: adminApi.catalogProviders,
    enabled: open && !merged,
    staleTime: 10 * 60_000,
  });
  const catalogModels = useQuery({
    queryKey: merged ? ["admin", "model-catalog", "models", "_all"] : ["admin", "model-catalog", "models", provider?.id],
    queryFn: () => (merged ? adminApi.catalogAllModels() : adminApi.catalogModels(provider!.id)),
    enabled: open && (merged || provider != null),
    staleTime: 10 * 60_000,
  });

  useEffect(() => {
    if (open) return;
    setProvider(null);
    setProviderQuery("");
    setModelQuery("");
    setSelected(new Set());
  }, [open]);

  const filteredProviders = useMemo(() => {
    const q = providerQuery.trim().toLowerCase();
    const list = providers.data ?? [];
    if (!q) return list;
    return list.filter((p) => p.id.toLowerCase().includes(q) || p.name.toLowerCase().includes(q));
  }, [providers.data, providerQuery]);

  const filteredModels = useMemo(() => {
    const q = modelQuery.trim().toLowerCase();
    const list = catalogModels.data ?? [];
    if (!q) return list;
    return list.filter((m) => m.id.toLowerCase().includes(q) || m.name.toLowerCase().includes(q));
  }, [catalogModels.data, modelQuery]);

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const allFilteredSelected = filteredModels.length > 0 && filteredModels.every((m) => selected.has(m.id));
  const toggleAll = () =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (allFilteredSelected) filteredModels.forEach((m) => next.delete(m.id));
      else filteredModels.forEach((m) => next.add(m.id));
      return next;
    });

  const doImport = useMutation({
    mutationFn: () => {
      const byId = new Map((catalogModels.data ?? []).map((m) => [m.id, m]));
      const picked = [...selected].map((id) => byId.get(id)).filter((m): m is RemoteCatalogModel => m != null);
      return onImport(picked);
    },
    onSuccess: () => { onImported(); onClose(); },
  });

  return (
    <Dialog.Root open={open} onOpenChange={(v) => !v && onClose()}>
      <Dialog.Content style={{ maxWidth: 720 }}>
        <Dialog.Title>{title ?? t("keys.catalog.title")}</Dialog.Title>
        <Text size="1" color="gray" as="div" mt="1">
          {hint ?? t("keys.catalog.hint")}
        </Text>
        {provider == null && !merged ? (
          <Flex direction="column" gap="2" mt="3">
            <TextField.Root
              size="1"
              placeholder={t("keys.catalog.searchProvider")}
              value={providerQuery}
              onChange={(e) => setProviderQuery(e.target.value)}
            />
            <Box style={{ maxHeight: 380, overflowY: "auto" }}>
              <Table.Root variant="surface" size="1">
                <Table.Body>
                  {filteredProviders.map((p) => (
                    <Table.Row key={p.id} style={{ cursor: "pointer" }} onClick={() => setProvider(p)}>
                      <Table.Cell>{p.name}</Table.Cell>
                      <Table.Cell><Text color="gray">{p.id}</Text></Table.Cell>
                      <Table.Cell align="right"><Text color="gray">{t("keys.catalog.modelCount", { n: p.models })}</Text></Table.Cell>
                    </Table.Row>
                  ))}
                  {providers.isLoading && (
                    <Table.Row><Table.Cell><Text color="gray">{t("keys.catalog.loadingCatalog")}</Text></Table.Cell></Table.Row>
                  )}
                  {providers.isError && (
                    <Table.Row><Table.Cell><Text color="red">{errMsg(providers.error)}</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
            </Box>
            <Flex justify="end"><Button variant="soft" color="gray" onClick={onClose}>{t("keys.shared.cancel")}</Button></Flex>
          </Flex>
        ) : (
          <Flex direction="column" gap="2" mt="3">
            <Flex align="center" gap="2">
              {provider != null && (
                <Button size="1" variant="ghost" onClick={() => setProvider(null)}>← {provider.name}</Button>
              )}
              <TextField.Root
                size="1"
                placeholder={t("keys.catalog.searchModel")}
                value={modelQuery}
                onChange={(e) => setModelQuery(e.target.value)}
                style={{ flex: 1 }}
              />
            </Flex>
            <Box style={{ maxHeight: 380, overflowY: "auto" }}>
              <Table.Root variant="surface" size="1">
                <Table.Header>
                  <Table.Row>
                    <Table.ColumnHeaderCell style={{ width: 32 }}>
                      <Checkbox checked={allFilteredSelected} onCheckedChange={toggleAll} />
                    </Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell>{t("keys.catalog.model")}</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">{t("keys.catalog.context")}</Table.ColumnHeaderCell>
                    <Table.ColumnHeaderCell align="right">{t("keys.catalog.price")}</Table.ColumnHeaderCell>
                  </Table.Row>
                </Table.Header>
                <Table.Body>
                  {filteredModels.map((m) => (
                    <Table.Row key={m.id} style={{ cursor: "pointer" }} onClick={() => toggle(m.id)}>
                      <Table.Cell><Checkbox checked={selected.has(m.id)} onCheckedChange={() => toggle(m.id)} /></Table.Cell>
                      <Table.Cell>
                        <Text as="div">{m.name}</Text>
                        <Text size="1" color="gray" as="div">
                          {merged && m.providers?.length ? m.providers.join(t("keys.shared.listSep")) : m.id}
                        </Text>
                      </Table.Cell>
                      <Table.Cell align="right" className="jc-num">{m.context ? m.context.toLocaleString(localeTag()) : "-"}</Table.Cell>
                      <Table.Cell align="right" className="jc-num">
                        {m.cost ? `${m.cost.input} / ${m.cost.output}` : "-"}
                      </Table.Cell>
                    </Table.Row>
                  ))}
                  {catalogModels.isLoading && (
                    <Table.Row><Table.Cell colSpan={4}><Text color="gray">{t("keys.catalog.loadingModels")}</Text></Table.Cell></Table.Row>
                  )}
                  {catalogModels.isError && (
                    <Table.Row><Table.Cell colSpan={4}><Text color="red">{errMsg(catalogModels.error)}</Text></Table.Cell></Table.Row>
                  )}
                </Table.Body>
              </Table.Root>
            </Box>
            {extra}
            {doImport.isError && <Text size="2" color="red">{errMsg(doImport.error)}</Text>}
            <Flex gap="2" justify="end" align="center">
              <Text size="1" color="gray">{t("keys.catalog.selected", { n: selected.size })}</Text>
              <Button variant="soft" color="gray" onClick={onClose}>{t("keys.shared.cancel")}</Button>
              <Button loading={doImport.isPending} disabled={selected.size === 0} onClick={() => doImport.mutate()}>
                {t("keys.catalog.importSelected")}
              </Button>
            </Flex>
          </Flex>
        )}
      </Dialog.Content>
    </Dialog.Root>
  );
}
