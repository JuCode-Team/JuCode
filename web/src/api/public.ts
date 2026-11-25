import { http } from "./client";
import type { GroupProfileDim, Plan, PublicStatus, SiteSettings, Vendor } from "./endpoints";

export interface PublicModelGroup {
  id: string;
  name: string;
  billing_source: "plan_only" | "balance_only";
  rate_multiplier: number;
  // Group intro card content, both optional. profile holds up to six named
  // radar axes; unset dimensions are absent rather than zero.
  description?: string;
  profile?: GroupProfileDim[];
}

export interface PublicModel {
  name: string;
  billing_mode: "per_request" | "per_token";
  currency: string;
  description?: string;
  vendor?: { id: string; name: string; slug: string };
  groups?: PublicModelGroup[];
}

export const publicApi = {
  siteInfo: () => http.get<SiteSettings>("/v1/public/site-info").then((response) => response.data),
  models: () => http.get<{ models: PublicModel[] }>("/v1/public/models").then((response) => response.data.models),
  publicStatus: () => http.get<PublicStatus>("/v1/public/status").then((response) => response.data),
  plans: () => http.get<{ plans: Plan[] }>("/v1/public/plans").then((response) => response.data.plans),
  vendors: () => http.get<{ vendors: Vendor[] }>("/v1/public/vendors").then((response) => response.data.vendors),
};
