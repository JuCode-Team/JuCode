import { useEffect, useMemo, useState } from "react";
import { Box, Button, Checkbox, Dialog, Flex, Text } from "@radix-ui/themes";
import { Megaphone } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { Announcement, SiteSettings } from "../api/endpoints";
import { useAuthStore } from "../store/auth";
import { useSiteStore } from "../store/site";

const DISMISS_PREFIX = "jucode-announcement-dismissed:";

function announcementSignature(announcement: Announcement): string {
  const raw = [
    announcement.id,
    announcement.display_mode,
    announcement.title.trim(),
    announcement.content.trim(),
  ].join("\n");
  let hash = 0;
  for (let i = 0; i < raw.length; i += 1) {
    hash = ((hash << 5) - hash + raw.charCodeAt(i)) | 0;
  }
  return Math.abs(hash).toString(36);
}

function isDismissed(signature: string): boolean {
  try {
    return localStorage.getItem(`${DISMISS_PREFIX}${signature}`) === "1";
  } catch {
    return false;
  }
}

export function activeAnnouncements(site: SiteSettings, loggedIn: boolean): Announcement[] {
  if (!site.announcement_enabled) return [];
  const siteAnnouncements = site.announcements ?? [];
  const announcements = siteAnnouncements.length > 0
    ? siteAnnouncements
    : (site.announcement_title.trim() || site.announcement_content.trim())
      ? [{
          id: "legacy",
          title: site.announcement_title,
          content: site.announcement_content,
          enabled: true,
          display_mode: site.announcement_display_mode,
        }]
      : [];
  return announcements.filter((announcement) =>
    announcement.enabled &&
    (announcement.title.trim() !== "" || announcement.content.trim() !== "") &&
    (announcement.display_mode === "always" || loggedIn)
  );
}

export function AnnouncementDialog() {
  const loaded = useSiteStore((s) => s.loaded);
  const site = useSiteStore((s) => s.site);
  const user = useAuthStore((s) => s.user);
  const [closedSignatures, setClosedSignatures] = useState<Set<string>>(new Set());
  const [index, setIndex] = useState(0);

  const announcements = useMemo(() => {
    if (!site || !loaded) return [];
    return activeAnnouncements(site, !!user)
      .map((announcement) => ({ announcement, signature: announcementSignature(announcement) }))
      .filter((item) => !closedSignatures.has(item.signature) && !isDismissed(item.signature));
  }, [closedSignatures, loaded, site, user]);

  useEffect(() => {
    if (index >= announcements.length) setIndex(0);
  }, [announcements.length, index]);

  const current = announcements[index];

  if (!current) return null;

  return (
    <AnnouncementDialogContent
      key={current.signature}
      open
      announcement={current.announcement}
      signature={current.signature}
      index={index}
      total={announcements.length}
      onPrev={() => setIndex((v) => Math.max(0, v - 1))}
      onNext={() => setIndex((v) => Math.min(announcements.length - 1, v + 1))}
      onClose={() => setClosedSignatures((signatures) => new Set(signatures).add(current.signature))}
    />
  );
}

function AnnouncementDialogContent({
  open,
  announcement,
  signature,
  index,
  total,
  onPrev,
  onNext,
  onClose,
}: {
  open: boolean;
  announcement: Announcement;
  signature: string;
  index: number;
  total: number;
  onPrev: () => void;
  onNext: () => void;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [dontShowAgain, setDontShowAgain] = useState(false);

  const close = () => {
    if (dontShowAgain) {
      try {
        localStorage.setItem(`${DISMISS_PREFIX}${signature}`, "1");
      } catch {
        // localStorage may be blocked; closing for this page view still works.
      }
    }
    onClose();
  };

  return (
    <Dialog.Root open={open} onOpenChange={(v) => { if (!v) close(); }}>
      <Dialog.Content className="jc-announcement-dialog" style={{ maxWidth: 520 }}>
        <Flex align="center" gap="3" mb="3">
          <Flex
            align="center"
            justify="center"
            style={{
              width: 36,
              height: 36,
              borderRadius: 8,
              background: "var(--accent-a3)",
              color: "var(--accent-11)",
              flexShrink: 0,
            }}
          >
            <Megaphone size={18} strokeWidth={1.8} />
          </Flex>
          <Box style={{ minWidth: 0, flex: 1 }}>
            <Dialog.Title mb="0">{announcement.title.trim() || t("keys.shared.announcement.fallback")}</Dialog.Title>
            {total > 1 && <Text size="1" color="gray">{t("keys.shared.announcement.position", { index: index + 1, total })}</Text>}
          </Box>
        </Flex>

        {announcement.content.trim() && (
          <Box
            mb="4"
            style={{
              color: "var(--gray-12)",
              lineHeight: 1.7,
              whiteSpace: "pre-wrap",
              wordBreak: "break-word",
            }}
          >
            {announcement.content.trim()}
          </Box>
        )}

        <Flex align="center" justify="between" gap="3" wrap="wrap">
          <Text as="label" size="2" color="gray">
            <Flex align="center" gap="2">
              <Checkbox
                checked={dontShowAgain}
                onCheckedChange={(v) => setDontShowAgain(!!v)}
              />
              {t("keys.shared.announcement.dontShowAgain")}
            </Flex>
          </Text>
          <Flex className="jc-announcement-actions" gap="2" wrap="wrap">
            {total > 1 && (
              <>
                <Button variant="soft" color="gray" disabled={index === 0} onClick={onPrev}>{t("keys.shared.announcement.prev")}</Button>
                <Button variant="soft" color="gray" disabled={index >= total - 1} onClick={onNext}>{t("keys.shared.announcement.next")}</Button>
              </>
            )}
            <Button onClick={close}>{t("keys.shared.announcement.ack")}</Button>
          </Flex>
        </Flex>
      </Dialog.Content>
    </Dialog.Root>
  );
}
