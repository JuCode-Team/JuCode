import { useEffect } from "react";
import { useNavigate } from "react-router-dom";
import { useMyOrganizations } from "../../hooks/useOrgContext";
import { useOrgContextStore } from "../../store/orgContext";

// Legacy /me/organizations page. The standalone org page was folded into
// the account-context switcher: org data now lives under the regular
// console tabs once you switch to the organization, and management moved
// to /me/org. This component only keeps old bookmarks working — it
// activates the org context and forwards.
export default function Organizations() {
  const navigate = useNavigate();
  const { data: orgs, isSuccess } = useMyOrganizations();
  const activeOrgId = useOrgContextStore((s) => s.activeOrgId);
  const setActiveOrg = useOrgContextStore((s) => s.setActiveOrg);

  useEffect(() => {
    if (!isSuccess) return;
    if (!orgs.length) {
      navigate("/me", { replace: true });
      return;
    }
    const target = orgs.find((o) => o.id === activeOrgId) ?? orgs[0];
    if (target.id !== activeOrgId) setActiveOrg(target.id);
    const isManager = target.role === "owner" || target.role === "admin";
    navigate(isManager ? "/me/org" : "/me", { replace: true });
  }, [isSuccess, orgs, activeOrgId, setActiveOrg, navigate]);

  return null;
}
