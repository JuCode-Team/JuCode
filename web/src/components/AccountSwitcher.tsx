import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Badge, DropdownMenu } from "@radix-ui/themes";
import { Building2, Check, ChevronsUpDown, User as UserIcon } from "lucide-react";
import type { Organization } from "../api/endpoints";
import { useActiveOrg, useMyOrganizations } from "../hooks/useOrgContext";
import { useAuthStore } from "../store/auth";
import { useOrgContextStore } from "../store/orgContext";

type OrgRole = NonNullable<Organization["role"]>;

function roleLabelKey(role: OrgRole) {
  return role === "owner"
    ? "console.switcher.roleOwner"
    : role === "admin"
      ? "console.switcher.roleAdmin"
      : "console.switcher.roleMember";
}

function roleColor(role: OrgRole): "iris" | "blue" | "gray" {
  return role === "owner" ? "iris" : role === "admin" ? "blue" : "gray";
}

// AccountSwitcher sits at the top of the console sidebar and flips the
// account context between the personal account and each organization the
// user belongs to. Everything below it (tabs, data, key creation,
// billing) follows the selected context. Hidden entirely for users
// without organizations — their console is implicitly personal.
export function AccountSwitcher() {
  const { t } = useTranslation();
  const user = useAuthStore((s) => s.user);
  const { data: organizations } = useMyOrganizations();
  const activeOrg = useActiveOrg();
  const setActiveOrg = useOrgContextStore((s) => s.setActiveOrg);
  const navigate = useNavigate();

  if (!user || !organizations?.length) return null;

  const personalLabel = user.nickname || user.email.split("@")[0];
  const currentLabel = activeOrg ? activeOrg.org?.name ?? t("common.organization") : personalLabel;

  const switchTo = (id: string | null) => {
    if ((activeOrg?.id ?? null) === id) return;
    setActiveOrg(id);
    // Land on the overview — the current tab may not exist in the target
    // context (e.g. 组织管理 after switching back to personal).
    navigate("/me");
  };

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger>
        <button type="button" className="jc-account-switcher" aria-label={t("console.switcher.trigger")}>
          {activeOrg ? <Building2 size={15} strokeWidth={1.8} /> : <UserIcon size={15} strokeWidth={1.8} />}
          <span className="jc-account-switcher-label">{currentLabel}</span>
          {activeOrg?.role && (
            <Badge size="1" color={roleColor(activeOrg.role)} variant="soft">{t(roleLabelKey(activeOrg.role))}</Badge>
          )}
          <ChevronsUpDown size={13} strokeWidth={1.8} />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Content style={{ minWidth: 220 }}>
        <DropdownMenu.Label>{t("console.switcher.personalAccount")}</DropdownMenu.Label>
        <DropdownMenu.Item onSelect={() => switchTo(null)}>
          <UserIcon size={14} strokeWidth={1.8} />
          <span className="jc-account-switcher-item-label">{personalLabel}</span>
          {!activeOrg && <Check size={14} strokeWidth={2} />}
        </DropdownMenu.Item>
        <DropdownMenu.Separator />
        <DropdownMenu.Label>{t("console.switcher.organizations")}</DropdownMenu.Label>
        {organizations.map((org) => (
          <DropdownMenu.Item key={org.id} onSelect={() => switchTo(org.id)}>
            <Building2 size={14} strokeWidth={1.8} />
            <span className="jc-account-switcher-item-label">{org.name}</span>
            {org.role && (
              <Badge size="1" color={roleColor(org.role)} variant="soft">{t(roleLabelKey(org.role))}</Badge>
            )}
            {activeOrg?.id === org.id && <Check size={14} strokeWidth={2} />}
          </DropdownMenu.Item>
        ))}
      </DropdownMenu.Content>
    </DropdownMenu.Root>
  );
}
