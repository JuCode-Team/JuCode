import { useEffect, useState, type ChangeEvent } from "react";
import { Badge, Box, Button, Flex, Text } from "@radix-ui/themes";
import { FileText, Image, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import { http } from "../api/client";
import type { TicketAttachment } from "../api/endpoints";

export const MAX_TICKET_ATTACHMENTS = 3;
export const MAX_TICKET_ATTACHMENT_SIZE = 2 * 1024 * 1024;

export async function filesToAttachments(files: FileList | null): Promise<TicketAttachment[]> {
  if (!files) return [];
  const picked = Array.from(files)
    .filter((f) => f.type.startsWith("image/") && f.size <= MAX_TICKET_ATTACHMENT_SIZE)
    .slice(0, MAX_TICKET_ATTACHMENTS);
  return Promise.all(picked.map((file) => new Promise<TicketAttachment>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve({ name: file.name, type: file.type, data_url: String(reader.result) });
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  })));
}

export function AttachmentPicker({
  attachments,
  onChange,
}: {
  attachments: TicketAttachment[];
  onChange: (next: TicketAttachment[]) => void;
}) {
  const { t } = useTranslation();
  const pick = async (event: ChangeEvent<HTMLInputElement>) => {
    const next = await filesToAttachments(event.target.files);
    onChange([...attachments, ...next].slice(0, MAX_TICKET_ATTACHMENTS));
    event.target.value = "";
  };
  return (
    <Box>
      <Flex gap="2" align="center" wrap="wrap">
        <Button asChild size="2" variant="soft" color="gray">
          <label>
            <Image size={14} /> {t("billing.tickets.attachments.upload")}
            <input type="file" accept="image/*" multiple hidden onChange={pick} />
          </label>
        </Button>
        <Text size="1" color="gray">{t("billing.tickets.attachments.limit")}</Text>
      </Flex>
      {attachments.length > 0 && (
        <Flex gap="2" mt="2" wrap="wrap">
          {attachments.map((a, idx) => (
            <Badge key={`${a.name}-${idx}`} variant="soft">
              {a.name}
              <button
                type="button"
                aria-label={t("billing.tickets.attachments.remove")}
                onClick={() => onChange(attachments.filter((_, i) => i !== idx))}
                style={{ marginLeft: 6, display: "inline-flex" }}
              >
                <X size={12} />
              </button>
            </Badge>
          ))}
        </Flex>
      )}
    </Box>
  );
}

export function MessageAttachments({ attachments }: { attachments: TicketAttachment[] }) {
  if (attachments.length === 0) return null;
  return (
    <Flex gap="2" mt="2" wrap="wrap">
      {attachments.map((a, idx) => (
        <AuthThumb key={`${a.name}-${a.id ?? idx}`} attachment={a} />
      ))}
    </Flex>
  );
}

function AuthThumb({ attachment }: { attachment: TicketAttachment }) {
  const src = useAuthObjectURL(attachment);
  if (!src) return null;
  // Logs a desktop bug report attaches: a download, not a picture.
  if (!(attachment.type ?? "").startsWith("image/")) {
    return (
      <Button asChild size="1" variant="soft" color="gray">
        <a href={src} download={attachment.name} title={attachment.name}>
          <FileText size={14} /> {attachment.name}
        </a>
      </Button>
    );
  }
  return (
    <a href={src} target="_blank" rel="noreferrer" title={attachment.name}>
      <img
        src={src}
        alt={attachment.name}
        style={{ width: 96, height: 72, objectFit: "cover", borderRadius: 6, border: "1px solid var(--gray-5)" }}
      />
    </a>
  );
}

function useAuthObjectURL(attachment: TicketAttachment): string {
  const [src, setSrc] = useState(attachment.data_url || "");
  useEffect(() => {
    if (attachment.data_url) {
      setSrc(attachment.data_url);
      return;
    }
    if (!attachment.url) {
      setSrc("");
      return;
    }
    let dead = false;
    let objectUrl = "";
    http.get(attachment.url, { responseType: "blob" }).then((r) => {
      objectUrl = URL.createObjectURL(r.data);
      if (!dead) setSrc(objectUrl);
    }).catch(() => {
      if (!dead) setSrc("");
    });
    return () => {
      dead = true;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [attachment.url, attachment.data_url]);
  return src;
}
