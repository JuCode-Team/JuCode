import type { CSSProperties } from "react";
import { useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Box, Card, Flex, Grid, Skeleton, Table } from "@radix-ui/themes";
import { AdminMetricGrid } from "./AdminPage";
import { PageHeader as SharedPageHeader } from "./PageHeader";
import { getConsoleRoute, type ConsoleSkeletonKind } from "../config/consoleRoutes";

function PageHeader({ title }: { title?: string }) {
  return (
    <SharedPageHeader
      title={title ?? <Skeleton width="160px" height="28px" style={{ borderRadius: 6 }} />}
      description={<Skeleton width="220px" height="14px" style={{ borderRadius: 4 }} />}
    />
  );
}

function Tiles({ n = 4 }: { n?: number }) {
  return (
    <AdminMetricGrid>
      {Array.from({ length: n }, (_, i) => (
        <Card key={i} className="pn-card-flat jc-admin-metric">
          <Skeleton width="42px" height="11px" style={{ borderRadius: 4 }} />
          <Skeleton width="72%" height="28px" style={{ borderRadius: 4, marginTop: 10 }} />
        </Card>
      ))}
    </AdminMetricGrid>
  );
}

function Toolbar({ inCard = false }: { inCard?: boolean }) {
  return (
    <Flex gap="2" mb={inCard ? "0" : "2"} wrap="wrap" className={inCard ? "jc-data-table-toolbar" : undefined}>
      <Skeleton width="220px" height="32px" style={{ borderRadius: 6 }} />
      <Skeleton width="88px" height="32px" style={{ borderRadius: 6 }} />
      <Skeleton width="32px" height="32px" style={{ borderRadius: 6 }} />
    </Flex>
  );
}

function Pager() {
  return (
    <Flex gap="1" align="center" justify="end" className="jc-data-table-pager">
      <Skeleton width="96px" height="12px" style={{ borderRadius: 4 }} />
      <Skeleton width="24px" height="24px" style={{ borderRadius: 6 }} />
      <Skeleton width="40px" height="24px" style={{ borderRadius: 6 }} />
      <Skeleton width="24px" height="24px" style={{ borderRadius: 6 }} />
    </Flex>
  );
}

export function TableSkeletonRows({ cols, rows = 4 }: { cols: number; rows?: number }) {
  return (
    <>
      {Array.from({ length: rows }, (_, r) => (
        <Table.Row key={r}>
          {Array.from({ length: cols }, (_, c) => (
            <Table.Cell key={c}>
              <Skeleton width={c === 0 ? "70%" : c === cols - 1 ? "40%" : "85%"} height="14px" style={{ borderRadius: 4 }} />
            </Table.Cell>
          ))}
        </Table.Row>
      ))}
    </>
  );
}

/* Mirrors the DataTable / SimpleDataTable shell: bordered card, 62px
   toolbar, size=1 table, 48px pager. Same DOM classes, so the skeleton and
   the real table share height, borders and radius and the swap is silent. */
function TableBlock({ cols = 6, rows = 8, toolbar = true }: { cols?: number; rows?: number; toolbar?: boolean }) {
  return (
    <Card className="pn-card-flat jc-data-table-card">
      {toolbar && <Toolbar inCard />}
      <Table.Root variant="ghost" size="1">
        <Table.Header>
          <Table.Row>
            {Array.from({ length: cols }, (_, i) => (
              <Table.ColumnHeaderCell key={i}>
                <Skeleton width={i === 0 ? "48%" : "70%"} height="12px" style={{ borderRadius: 4 }} />
              </Table.ColumnHeaderCell>
            ))}
          </Table.Row>
        </Table.Header>
        <Table.Body>
          <TableSkeletonRows cols={cols} rows={rows} />
        </Table.Body>
      </Table.Root>
      <Pager />
    </Card>
  );
}

function ChartBlock({ h = 220 }: { h?: number }) {
  return (
    <Card className="pn-card-flat" mb="4">
      <Skeleton width="100px" height="14px" style={{ borderRadius: 4, marginBottom: 14 }} />
      <Skeleton width="100%" height={`${h}px`} style={{ borderRadius: 8 }} />
    </Card>
  );
}

function CardGrid({ n = 6 }: { n?: number }) {
  return (
    <Grid columns={{ initial: "1", md: "2", xl: "3" }} gap="3">
      {Array.from({ length: n }, (_, i) => (
        <Card key={i} className="pn-card-flat">
          <Skeleton width="40%" height="16px" style={{ borderRadius: 4, marginBottom: 10 }} />
          <Skeleton width="100%" height="12px" style={{ borderRadius: 4, marginBottom: 8 }} />
          <Skeleton width="70%" height="12px" style={{ borderRadius: 4 }} />
        </Card>
      ))}
    </Grid>
  );
}

export default function ConsoleSkeleton({
  kind = "table",
  title,
}: {
  kind?: ConsoleSkeletonKind;
  title?: string;
}) {
  return (
    <Box className="jc-skel-page">
      <PageHeader title={title} />
      {kind === "dashboard" && (
        <>
          <Tiles n={4} />
          <Card className="pn-card-flat" mb="4">
            <Skeleton width="88px" height="16px" style={{ borderRadius: 4, marginBottom: 14 }} />
            <Tiles n={4} />
            <Skeleton width="100%" height="8px" style={{ borderRadius: 99 }} />
          </Card>
          <ChartBlock />
          <Grid columns={{ initial: "1", md: "2" }} gap="3">
            <Card className="pn-card-flat"><Tiles n={4} /></Card>
            <Card className="pn-card-flat"><Tiles n={4} /></Card>
          </Grid>
        </>
      )}
      {kind === "overview" && (
        <div className="jc-me">
          <div className="jc-kf" style={{ "--jc-kf-cols": 3 } as CSSProperties}>
            {Array.from({ length: 3 }, (_, i) => (
              <div key={i} className="jc-kf-cell">
                <div className="jc-kf-head"><Skeleton width="96px" height="12px" style={{ borderRadius: 4 }} /></div>
                <div className="jc-kf-value"><Skeleton width="7ch" height="1em" style={{ borderRadius: 4 }} /></div>
                <div className="jc-kf-note"><Skeleton width="14ch" height="12px" style={{ borderRadius: 4 }} /></div>
              </div>
            ))}
          </div>
          <div>
            <div className="jc-me-section-head">
              <Skeleton width="120px" height="16px" style={{ borderRadius: 4 }} />
              <Skeleton width="180px" height="24px" style={{ borderRadius: 6 }} />
            </div>
            <div className="jc-me-usage-grid">
              <Card className="pn-card-flat jc-me-panel">
                <div className="jc-me-panel-head">
                  <Skeleton width="72px" height="16px" style={{ borderRadius: 4 }} />
                  <Skeleton width="140px" height="24px" style={{ borderRadius: 6 }} />
                </div>
                <Skeleton width="100%" height="260px" style={{ borderRadius: 8 }} />
              </Card>
              <Card className="pn-card-flat jc-me-panel jc-mix">
                <div className="jc-me-panel-head"><Skeleton width="72px" height="16px" style={{ borderRadius: 4 }} /></div>
                <div className="jc-mix-total"><Skeleton width="9ch" height="1em" style={{ borderRadius: 4 }} /></div>
                <Skeleton width="100%" height="8px" style={{ borderRadius: 4, margin: "12px 0 14px" }} />
                {Array.from({ length: 3 }, (_, i) => (
                  <div key={i} className="jc-mix-row">
                    <Skeleton width="8px" height="8px" style={{ borderRadius: 2 }} />
                    <Skeleton width="48px" height="13px" style={{ borderRadius: 3 }} />
                    <Skeleton width="5ch" height="12px" style={{ borderRadius: 3 }} />
                    <span className="jc-mix-row-value"><Skeleton width="7ch" height="1em" style={{ borderRadius: 3 }} /></span>
                  </div>
                ))}
              </Card>
            </div>
          </div>
          <div className="jc-me-account">
            <Skeleton width="40px" height="40px" style={{ borderRadius: 8 }} />
            <Box style={{ flex: 1 }}>
              <Skeleton width="120px" height="16px" style={{ borderRadius: 4, marginBottom: 6 }} />
              <Skeleton width="200px" height="12px" style={{ borderRadius: 4 }} />
            </Box>
            <Skeleton width="220px" height="28px" style={{ borderRadius: 6 }} />
          </div>
        </div>
      )}
      {kind === "account" && (
        <>
          <Card className="pn-card-flat" mb="3">
            <Flex align="center" gap="4">
              <Skeleton width="48px" height="48px" style={{ borderRadius: 999 }} />
              <Box style={{ flex: 1 }}>
                <Skeleton width="160px" height="20px" style={{ borderRadius: 4, marginBottom: 8 }} />
                <Skeleton width="220px" height="12px" style={{ borderRadius: 4 }} />
              </Box>
            </Flex>
          </Card>
          <Grid columns={{ initial: "1", sm: "2" }} gap="3" mb="4">
            <Card className="pn-card-flat">
              <Skeleton width="36px" height="11px" style={{ borderRadius: 4 }} />
              <Skeleton width="50%" height="28px" style={{ borderRadius: 4, marginTop: 10 }} />
            </Card>
            <Card className="pn-card-flat">
              <Skeleton width="36px" height="11px" style={{ borderRadius: 4 }} />
              <Skeleton width="50%" height="28px" style={{ borderRadius: 4, marginTop: 10 }} />
            </Card>
          </Grid>
          <ChartBlock />
        </>
      )}
      {kind === "metrics-table" && (
        <>
          <Tiles n={4} />
          <TableBlock />
        </>
      )}
      {kind === "table" && <TableBlock />}
      {kind === "logs" && (
        <>
          <Flex gap="2" mb="3" wrap="wrap">
            {Array.from({ length: 5 }, (_, i) => (
              <Skeleton key={i} width={i === 0 ? "160px" : "110px"} height="32px" style={{ borderRadius: 8 }} />
            ))}
          </Flex>
          <TableBlock cols={8} toolbar={false} />
        </>
      )}
      {kind === "charts" && (
        <>
          <Tiles n={4} />
          <ChartBlock h={260} />
          <TableBlock cols={5} rows={6} toolbar={false} />
        </>
      )}
      {kind === "form" && (
        <Card className="pn-card-flat">
          <Flex gap="3" mb="4" wrap="wrap">
            {Array.from({ length: 6 }, (_, i) => (
              <Skeleton key={i} width="84px" height="28px" style={{ borderRadius: 6 }} />
            ))}
          </Flex>
          <Flex direction="column" gap="4" style={{ maxWidth: 640 }}>
            {Array.from({ length: 6 }, (_, i) => (
              <Box key={i}>
                <Skeleton width="72px" height="12px" style={{ borderRadius: 4, marginBottom: 8 }} />
                <Skeleton width="100%" height="32px" style={{ borderRadius: 8 }} />
              </Box>
            ))}
          </Flex>
        </Card>
      )}
      {kind === "cards" && <CardGrid />}
      {kind === "metrics-cards" && (
        <>
          <Tiles n={4} />
          <CardGrid />
        </>
      )}
      {kind === "lab" && (
        <Flex gap="3" align="stretch" wrap="wrap">
          <Card className="pn-card-flat" style={{ flex: "0 1 280px" }}>
            <Flex direction="column" gap="3">
              <Skeleton width="40%" height="12px" style={{ borderRadius: 4 }} />
              <Skeleton width="100%" height="32px" style={{ borderRadius: 8 }} />
              <Skeleton width="40%" height="12px" style={{ borderRadius: 4 }} />
              <Skeleton width="100%" height="32px" style={{ borderRadius: 8 }} />
              <Skeleton width="100%" height="80px" style={{ borderRadius: 8 }} />
              <Skeleton width="100%" height="36px" style={{ borderRadius: 8 }} />
            </Flex>
          </Card>
          <Card className="pn-card-flat" style={{ flex: "1 1 360px" }}>
            <Skeleton width="100%" height="420px" style={{ borderRadius: 8 }} />
          </Card>
        </Flex>
      )}
      {kind === "list" && (
        <Flex direction="column" gap="3">
          <Toolbar />
          {Array.from({ length: 5 }, (_, i) => (
            <Card key={i} className="pn-card-flat">
              <Flex justify="between" align="center" gap="3">
                <Box style={{ flex: 1 }}>
                  <Skeleton width="30%" height="18px" style={{ borderRadius: 4, marginBottom: 8 }} />
                  <Skeleton width="50%" height="12px" style={{ borderRadius: 4 }} />
                </Box>
                <Skeleton width="72px" height="28px" style={{ borderRadius: 6 }} />
              </Flex>
            </Card>
          ))}
        </Flex>
      )}
    </Box>
  );
}

export function RouteSkeleton() {
  const { t } = useTranslation();
  const route = getConsoleRoute(useLocation().pathname);
  return <ConsoleSkeleton kind={route?.skeleton ?? "table"} title={route ? t(route.labelKey) : undefined} />;
}
