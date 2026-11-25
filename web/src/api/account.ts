import { http } from "./client";
import type { MeResponse, Organization } from "./endpoints";

// Shell-level account requests live separately from the full console API.
// This keeps the large admin/console endpoint registry out of public bundles.
export const accountApi = {
  profile: () => http.get<MeResponse>("/v1/me").then((response) => response.data),
  organizations: () =>
    http.get<{ organizations: Organization[] }>("/v1/me/organizations").then((response) => response.data.organizations),
};
