import { type ComponentType } from "react";
import {
  Activity,
  AlertTriangle,
  BarChart3,
  Brain,
  Briefcase,
  CreditCard,
  FlaskConical,
  FolderTree,
  Gauge,
  Gift,
  HandCoins,
  GlobeLock,
  Headphones,
  HeartPulse,
  KeyRound,
  Laptop,
  LayoutDashboard,
  LifeBuoy,
  Network,
  Package,
  Receipt,
  RotateCcw,
  Search,
  ScrollText,
  Settings,
  Shield,
  ShieldAlert,
  Tag,
  Ticket,
  Users,
  UsersRound,
  Wallet,
  Waypoints,
  TrendingUp,
  type LucideProps,
} from "lucide-react";
import { matchPath } from "react-router-dom";
import type { Translations } from "../i18n/zh";

export type ConsoleAccess = "user" | "cs" | "agent" | "admin";
export type ConsoleArea = "personal" | "support" | "agent" | "admin";
export type ConsoleGroup =
  | "personal"
  | "support"
  | "agent"
  | "admin-primary"
  | "admin-users"
  | "admin-models"
  | "admin-business"
  | "admin-compliance"
  | "admin-system";

// Personal-area sidebar sections, in display order. Titles come from
// console.group.* in the locale bundles.
export type PersonalGroup = "account" | "access" | "support";
export const personalNavGroups = ["account", "access", "support"] as const satisfies readonly PersonalGroup[];

export type ConsoleSkeletonKind =
  | "dashboard"
  | "overview"
  | "account"
  | "metrics-table"
  | "table"
  | "logs"
  | "charts"
  | "form"
  | "cards"
  | "metrics-cards"
  | "lab"
  | "list";

// Sidebar/breadcrumb labels live in the locale bundles; a route only
// carries the key. The template type keeps the key honest — a typo or a
// key removed from zh.ts fails `tsc`.
export type ConsoleLabelKey = `console.nav.${keyof Translations["console"]["nav"]}`;

export interface ConsoleRoute {
  path: string;
  labelKey: ConsoleLabelKey;
  area: ConsoleArea;
  access: ConsoleAccess;
  // pages/{page}.tsx — resolved by the console chunk, not here.
  page: string;
  skeleton: ConsoleSkeletonKind;
  icon?: ComponentType<LucideProps>;
  group?: ConsoleGroup;
  // Sidebar section inside the personal group (account / access / support).
  personalGroup?: PersonalGroup;
  exact?: boolean;
  nav?: boolean;
  requiresOrganization?: boolean;
  // Which account context (personal vs organization switcher state) shows
  // this tab. Default "both". Routes outside the personal group ignore it.
  context?: "personal" | "organization" | "both";
  // In org context, show only to owners/admins. Has no effect on the
  // personal context (a personal-context tab is always the user's own).
  requiresOrgManager?: boolean;
}

export const consoleRoutes: readonly ConsoleRoute[] = [
  { path: "/me", labelKey: "console.nav.overview", area: "personal", access: "user", page: "user/Me", skeleton: "overview", icon: LayoutDashboard, group: "personal", personalGroup: "account", exact: true, nav: true, context: "both" },
  { path: "/me/usage", labelKey: "console.nav.usage", area: "personal", access: "user", page: "user/Usage", skeleton: "logs", icon: BarChart3, group: "personal", personalGroup: "account", nav: true, context: "both" },
  { path: "/me/billing", labelKey: "console.nav.billing", area: "personal", access: "user", page: "user/Billing", skeleton: "metrics-table", icon: CreditCard, group: "personal", personalGroup: "account", nav: true, context: "both", requiresOrgManager: true },
  { path: "/me/plans", labelKey: "console.nav.plans", area: "personal", access: "user", page: "user/UserPlans", skeleton: "cards", icon: Package, group: "personal", personalGroup: "account", nav: true, context: "personal" },
  { path: "/me/org", labelKey: "console.nav.orgManage", area: "personal", access: "user", page: "user/OrgManage", skeleton: "table", icon: UsersRound, group: "personal", personalGroup: "account", nav: true, context: "organization", requiresOrgManager: true },
  { path: "/me/apikeys", labelKey: "console.nav.apiKeys", area: "personal", access: "user", page: "user/ApiKeys", skeleton: "list", icon: KeyRound, group: "personal", personalGroup: "access", nav: true, context: "both" },
  { path: "/me/access-tokens", labelKey: "console.nav.accessTokens", area: "personal", access: "user", page: "user/AccessTokens", skeleton: "list", icon: KeyRound, group: "personal", personalGroup: "access", nav: true, context: "personal" },
  { path: "/me/devices", labelKey: "console.nav.devices", area: "personal", access: "user", page: "user/Devices", skeleton: "list", icon: Laptop, group: "personal", personalGroup: "access", nav: true, context: "personal" },
  { path: "/me/lab", labelKey: "console.nav.lab", area: "personal", access: "user", page: "user/Lab", skeleton: "lab", icon: FlaskConical, group: "personal", personalGroup: "access", nav: true, context: "both" },
  { path: "/me/tickets", labelKey: "console.nav.tickets", area: "personal", access: "user", page: "user/Tickets", skeleton: "list", icon: LifeBuoy, group: "personal", personalGroup: "support", nav: true, context: "personal" },
  { path: "/me/invite", labelKey: "console.nav.invite", area: "personal", access: "user", page: "user/Invite", skeleton: "account", icon: Gift, group: "personal", personalGroup: "support", nav: true, context: "personal" },
  // Legacy standalone org page — kept only as a redirect into the org
  // context (/me/org) so old bookmarks keep working.
  { path: "/me/organizations", labelKey: "console.nav.organizations", area: "personal", access: "user", page: "user/Organizations", skeleton: "table", icon: UsersRound, group: "personal", requiresOrganization: true },

  { path: "/cs/tickets", labelKey: "console.nav.csTickets", area: "support", access: "cs", page: "cs/Tickets", skeleton: "table", icon: Headphones, group: "support", nav: true },
  { path: "/cs/refunds", labelKey: "console.nav.csRefunds", area: "support", access: "cs", page: "cs/Refunds", skeleton: "table", icon: RotateCcw, group: "support", nav: true },
  { path: "/cs/users", labelKey: "console.nav.csUsers", area: "support", access: "cs", page: "cs/Users", skeleton: "metrics-table", icon: Users, group: "support", nav: true },

  { path: "/agent", labelKey: "console.nav.agentDashboard", area: "agent", access: "agent", page: "agent/Dashboard", skeleton: "dashboard", icon: Briefcase, group: "agent", exact: true, nav: true },
  { path: "/agent/codes", labelKey: "console.nav.agentCodes", area: "agent", access: "agent", page: "agent/Codes", skeleton: "table", icon: Ticket, group: "agent", nav: true },
  { path: "/agent/coupon", labelKey: "console.nav.agentCoupon", area: "agent", access: "agent", page: "agent/Coupon", skeleton: "dashboard", icon: HandCoins, group: "agent", nav: true },
  { path: "/agent/sales", labelKey: "console.nav.agentSales", area: "agent", access: "agent", page: "agent/Sales", skeleton: "table", icon: Wallet, group: "agent", nav: true },
  { path: "/agent/refund", labelKey: "console.nav.agentRefund", area: "agent", access: "agent", page: "agent/Refund", skeleton: "form", icon: RotateCcw, group: "agent", nav: true },
  { path: "/agent/settlements", labelKey: "console.nav.agentSettlements", area: "agent", access: "agent", page: "agent/Settlements", skeleton: "table", icon: Receipt, group: "agent", nav: true },

  { path: "/admin/overview", labelKey: "console.nav.adminOverview", area: "admin", access: "admin", page: "admin/Overview", skeleton: "dashboard", icon: Gauge, group: "admin-primary", exact: true, nav: true },
  { path: "/admin/stats", labelKey: "console.nav.adminStats", area: "admin", access: "admin", page: "admin/Stats", skeleton: "charts", icon: BarChart3, group: "admin-primary", nav: true },
  { path: "/admin/observability", labelKey: "console.nav.adminObservability", area: "admin", access: "admin", page: "admin/Observability", skeleton: "charts", icon: Activity, group: "admin-primary", nav: true },
  { path: "/admin/users", labelKey: "console.nav.adminUsers", area: "admin", access: "admin", page: "admin/Users", skeleton: "metrics-table", icon: Users, group: "admin-users", nav: true },
  { path: "/admin/organizations", labelKey: "console.nav.adminOrganizations", area: "admin", access: "admin", page: "admin/Organizations", skeleton: "metrics-table", icon: UsersRound, group: "admin-users", nav: true },
  { path: "/admin/user-groups", labelKey: "console.nav.adminUserGroups", area: "admin", access: "admin", page: "admin/UserGroups", skeleton: "table", icon: UsersRound, group: "admin-users", nav: true },
  { path: "/admin/accounts", labelKey: "console.nav.adminAccounts", area: "admin", access: "admin", page: "admin/Accounts", skeleton: "metrics-table", icon: KeyRound, group: "admin-models", nav: true },
  { path: "/admin/web-tools", labelKey: "console.nav.adminWebTools", area: "admin", access: "admin", page: "admin/WebTools", skeleton: "table", icon: Search, group: "admin-models", nav: true },
  { path: "/admin/routing", labelKey: "console.nav.adminRouting", area: "admin", access: "admin", page: "admin/Routing", skeleton: "table", icon: Network, group: "admin-models", nav: true },
  { path: "/admin/models", labelKey: "console.nav.adminModels", area: "admin", access: "admin", page: "admin/Models", skeleton: "metrics-table", icon: Brain, group: "admin-models", nav: true },
  { path: "/admin/vendors", labelKey: "console.nav.adminVendors", area: "admin", access: "admin", page: "admin/Vendors", skeleton: "table", icon: Tag, group: "admin-models", nav: true },
  { path: "/admin/groups", labelKey: "console.nav.adminGroups", area: "admin", access: "admin", page: "admin/Groups", skeleton: "list", icon: FolderTree, group: "admin-models", nav: true },
  { path: "/admin/topology", labelKey: "console.nav.adminTopology", area: "admin", access: "admin", page: "admin/Topology", skeleton: "list", icon: Waypoints, group: "admin-models", nav: true },
  { path: "/admin/plans", labelKey: "console.nav.adminPlans", area: "admin", access: "admin", page: "admin/AdminPlans", skeleton: "metrics-table", icon: CreditCard, group: "admin-business", nav: true },
  { path: "/admin/user-plans", labelKey: "console.nav.adminUserPlans", area: "admin", access: "admin", page: "admin/AdminUserPlans", skeleton: "metrics-table", icon: RotateCcw, group: "admin-business", nav: true },
  { path: "/admin/redeem", labelKey: "console.nav.adminRedeem", area: "admin", access: "admin", page: "admin/RedeemCodes", skeleton: "table", icon: Ticket, group: "admin-business", nav: true },
  { path: "/admin/agents", labelKey: "console.nav.adminAgents", area: "admin", access: "admin", page: "admin/AdminAgents", skeleton: "table", icon: Briefcase, group: "admin-business", nav: true },
  { path: "/admin/ops", labelKey: "console.nav.adminOps", area: "admin", access: "admin", page: "admin/Ops", skeleton: "charts", icon: TrendingUp, group: "admin-business", nav: true },
  { path: "/admin/finance", labelKey: "console.nav.adminFinance", area: "admin", access: "admin", page: "admin/Finance", skeleton: "charts", icon: Receipt, group: "admin-business", nav: true },
  { path: "/admin/invite", labelKey: "console.nav.adminInvite", area: "admin", access: "admin", page: "admin/Invite", skeleton: "table", icon: Gift, group: "admin-business", nav: true },
  { path: "/admin/logs", labelKey: "console.nav.adminLogs", area: "admin", access: "admin", page: "admin/Logs", skeleton: "logs", icon: ScrollText, group: "admin-compliance", nav: true },
  { path: "/admin/policy", labelKey: "console.nav.adminPolicy", area: "admin", access: "admin", page: "admin/Policy", skeleton: "form", icon: Shield, group: "admin-compliance", nav: true },
  { path: "/admin/incidents", labelKey: "console.nav.adminIncidents", area: "admin", access: "admin", page: "admin/Incidents", skeleton: "charts", icon: AlertTriangle, group: "admin-compliance", nav: true },
  { path: "/admin/system-incidents", labelKey: "console.nav.adminSystemIncidents", area: "admin", access: "admin", page: "admin/SystemIncidents", skeleton: "charts", icon: HeartPulse, group: "admin-compliance", nav: true },
  { path: "/admin/audit-logs", labelKey: "console.nav.adminAuditLogs", area: "admin", access: "admin", page: "admin/AuditLogs", skeleton: "table", icon: ShieldAlert, group: "admin-compliance", nav: true },
  { path: "/admin/ip-policies", labelKey: "console.nav.adminIPPolicies", area: "admin", access: "admin", page: "admin/IPPolicies", skeleton: "metrics-table", icon: GlobeLock, group: "admin-system", nav: true },
  { path: "/admin/settings", labelKey: "console.nav.adminSettings", area: "admin", access: "admin", page: "admin/Settings", skeleton: "form", icon: Settings, group: "admin-system", nav: true },
] as const;

// Collapsible admin sections, in sidebar order. Titles come from
// console.group.* in the locale bundles.
export const consoleNavGroups = [
  "admin-users",
  "admin-models",
  "admin-business",
  "admin-compliance",
  "admin-system",
] as const satisfies readonly ConsoleGroup[];

export function getConsoleRoute(pathname: string): ConsoleRoute | undefined {
  return consoleRoutes.find((route) => matchPath({ path: route.path, end: route.exact ?? true }, pathname));
}

export function canAccessConsoleRoute(access: ConsoleAccess, role?: string, roles: readonly string[] = []): boolean {
  if (!role) return false;
  if (role === "admin") return true;
  if (access === "user") return true;
  return roles.includes(access);
}
