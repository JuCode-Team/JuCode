import type { ReactNode } from "react";
import { Box, Card, Flex, Grid, Heading, Text } from "@radix-ui/themes";
import { PageHeader } from "./PageHeader";

export function AdminPage({
  title,
  description,
  actions,
  children,
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Box className="jc-admin-page">
      <PageHeader title={title} description={description} actions={actions} />
      {children}
    </Box>
  );
}

export function AdminSection({
  title,
  description,
  actions,
  children,
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card className="pn-card-flat jc-admin-section">
      <Flex className="jc-admin-section-head" justify="between" align="start" gap="3" wrap="wrap">
        <Box style={{ minWidth: 0 }}>
          <Heading size="3">{title}</Heading>
          {description && (
            <Text size="2" color="gray" as="div" mt="1">
              {description}
            </Text>
          )}
        </Box>
        {actions && <Flex gap="2" wrap="wrap" justify="end">{actions}</Flex>}
      </Flex>
      <Box className="jc-admin-section-body">{children}</Box>
    </Card>
  );
}

export function AdminMetricGrid({ children }: { children: ReactNode }) {
  return (
    <Grid columns={{ initial: "1", sm: "2", lg: "4" }} gap="3" mb="4">
      {children}
    </Grid>
  );
}

export function AdminMetricCard({
  label,
  value,
  detail,
  tone = "gray",
}: {
  label: string;
  value: ReactNode;
  detail?: ReactNode;
  tone?: "gray" | "green" | "amber" | "red" | "blue" | "iris" | "purple";
}) {
  return (
    <Card className="pn-card-flat jc-admin-metric" data-tone={tone}>
      <Text size="1" color="gray" as="div">{label}</Text>
      <Heading size="6" className="jc-num" mt="1">{value}</Heading>
      {detail && <Text size="1" color="gray" as="div" mt="1">{detail}</Text>}
    </Card>
  );
}
