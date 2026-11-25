import { http } from "./client";

// ---- types (mirror backend response shapes) ----

export interface Tokens {
  access_token: string;
  refresh_token: string;
  access_expires_at: number;
  refresh_expires_at: number;
  token_type: string;
}

export interface User {
  id: string;
  email: string;
  role: "user" | "admin";
  nickname?: string | null;
  // Functional roles (cs / agent) on top of the primary role. Empty
  // unless the server returned a `roles` field — older payloads omit it.
  roles?: string[];
}

export interface MeResponse extends User {
  status: string;
  balance: string;
  // Sum of reservations for in-flight requests, already subtracted from balance.
  held_balance?: string;
  currency: string;
  created_at: string;
  // Backend now surfaces a `roles` array carrying functional grants
  // (cs / agent). User type already declares it as optional; we
  // re-state here so MeResponse is self-documenting.
  roles?: string[];
  // TOTP two-factor state. setup_required = privileged account (admin /
  // cs / agent) that hasn't bound TOTP yet — UI nudges toward binding.
  totp_enabled?: boolean;
  totp_setup_required?: boolean;
  active_plan?: {
    user_plan_id: string;
    plan_id: string;
    name: string;
    type: Plan["type"];
    start_at: string;
    expire_at: string;
    quota_5h: string;
    quota_weekly: string;
    quota_monthly: string;
    monthly_spend_limit?: string;
  };
}

export interface ApiKeyView {
  id: string;
  prefix: string;
  name: string;
  last_used_at?: string;
  expires_at?: string;
  created_at: string;
  // User endpoints drop disabled and deleted groups; admin endpoints drop
  // only deleted ones.
  allowed_group_ids: string[];
  // True when the stored list is empty (the key may use every group). An
  // empty allowed_group_ids with this false means every bound group is gone.
  allowed_groups_unrestricted: boolean;
  ip_whitelist?: string[];
  auto_route: boolean;
  auto_route_mode?: "price" | "stable" | "fast";
  auto_route_max_rate: number;
  model_pins?: { model: string; group_id: string }[];
  billing_scope?: "personal" | "organization";
  organization_id?: string | null;
  daily_cost_limit: string | null;
  monthly_cost_limit: string | null;
  total_cost_limit: string | null;
  daily_cost_used: string;
  monthly_cost_used: string;
  total_cost_used: string;
  // Creator of the key. Only present in the org owner/admin view, where
  // the list spans keys created by multiple members.
  user_id?: string;
  user_email?: string;
  system?: boolean;
}

export type ApiKeyCostLimits = {
  daily_cost_limit?: string | null;
  monthly_cost_limit?: string | null;
  total_cost_limit?: string | null;
};

export interface CreatedApiKey {
  api_key: ApiKeyView;
  secret: string;
}

export interface ProviderPerformance {
  window_seconds: number;
  request_count: number;
  success_count: number;
  failure_count: number;
  failure_rate: number;
  tps?: number | null;
  ttft_count: number;
  avg_ttft_ms?: number | null;
  p50_ttft_ms?: number | null;
  p95_ttft_ms?: number | null;
  p99_ttft_ms?: number | null;
}

export interface Provider {
  id: string;
  name: string;
  kind: "openai_compatible" | "anthropic";
  base_url: string;
  // Generation-endpoint dialect adaptor (openai/dashscope/volcengine/…).
  // Empty = inferred from base_url host.
  api_format?: string;
  // Client-facing wire protocols the upstream accepts. Absent = derived
  // from kind (openai_compatible → openai, anthropic → anthropic).
  protocols?: ("openai" | "anthropic" | "compatible" | "responses")[];
  organization?: string;
  health: "healthy" | "degraded" | "unhealthy" | "unknown";
  weight: number;
  enabled: boolean;
  block_image_generation_tool: boolean;
  // Upstream omits the cache-write usage field; gateway derives it by
  // reclassifying non-cached input (rows carry cache_write_estimated).
  estimate_cache_write: boolean;
  // Estimation guard: below this input-side context total, no upstream
  // cache write happens, so tokens stay plain input. 0 = no minimum.
  cache_write_min_context_tokens: number;
  note?: string;
  api_key_set?: boolean;
  anthropic_api_key_set?: boolean;
  supported_model_count: number;
  account_count?: number;
  performance?: ProviderPerformance;
  created_at: string;
}

// ProviderCatalogEntry is one row of the vendored provider catalog the admin
// UI offers as a template.
export interface ProviderCatalogEntry {
  id: string;
  name: string;
  // "" (key only) | "api-key" | "custom". The console only offers API-key
  // templates.
  login_kind: string;
  api_key_format: string;
  base_url: string;
  protocol: string;
  default_model?: string;
  model_count: number;
  // false = the catalog knows the provider but this gateway cannot speak its
  // dialect yet (Codex/Azure Responses), so it must not be offered.
  usable: boolean;
}

export interface ProviderCatalog {
  catalog_version: string;
  providers: ProviderCatalogEntry[];
}

// ProviderCatalogModel is one model of a catalog provider. `supported` marks
// the dialects this gateway can actually serve; the others are shown for
// reference but cannot be bound.
export interface ProviderCatalogModel {
  id: string;
  name: string;
  // Upstream wire-dialect id (e.g. "anthropic-messages").
  api: string;
  protocol?: string;
  supported: boolean;
  base_url?: string;
  context_window: number;
  max_tokens: number;
  reasoning: boolean;
  input?: string[];
  input_cost?: number;
  output_cost?: number;
  cached_input_cost?: number;
  cache_write_cost?: number;
}

export interface ProviderCatalogDetail {
  catalog_version: string;
  provider: ProviderCatalogEntry;
  models: ProviderCatalogModel[];
}

// HeaderRules rewrite the headers sent to an upstream account, applied in the
// order remove → drop_values → set. Set values may use {api_key} for the
// account credential; drop_values removes entries of a comma-separated header
// (a trailing * matches by prefix), e.g. an anthropic-beta flag the vendor
// rejects.
export interface HeaderRules {
  set?: Record<string, string>;
  remove?: string[];
  drop_values?: Record<string, string[]>;
}

// ProviderDefinitionModel is one model a definition seeds onto new accounts.
export interface ProviderDefinitionModel {
  name: string;
  protocol?: AccountProtocol;
}

// ProviderDefinition is the per-vendor payload an API-key account is created
// from: endpoint shape, protocols, header rules and seeded models.
export interface ProviderDefinition {
  definition_id: string;
  name: string;
  vendor: string;
  base_url: string;
  api_format?: string;
  native_protocols?: AccountProtocol[];
  default_protocol?: string;
  allow_conversion?: boolean;
  endpoint_paths?: Partial<Record<AccountProtocol, string>>;
  // Header rules accounts created from this definition start with.
  header_rules?: HeaderRules;
  // models_path overrides the upstream model-list endpoint for sync; empty
  // means the conventional {base}/v1/models, "none" means the vendor has no
  // list endpoint and sync binds the definition's own models.
  models_path?: string;
  models?: ProviderDefinitionModel[];
  // CredentialLabel / CredentialHint name the secret this vendor wants
  // ("会话令牌（Bearer）" instead of a generic "API Key").
  credential_label?: string;
  credential_hint?: string;
  enabled: boolean;
  // builtin marks a definition shipped in code; source is "builtin" until a
  // database row (seeded or authored) takes over.
  builtin: boolean;
  source: "builtin" | "database";
  version: number;
  note?: string;
  updated_at?: string;
  // A stored payload that failed to parse; surfaced instead of hiding the
  // definition.
  payload_error?: string;
}

export interface ProviderDefinitionInput {
  name?: string;
  vendor?: string;
  base_url?: string;
  api_format?: string;
  native_protocols?: AccountProtocol[];
  default_protocol?: string;
  allow_conversion?: boolean;
  endpoint_paths?: Partial<Record<AccountProtocol, string>>;
  // Whole replacement: omitting it clears the definition's rules.
  header_rules?: HeaderRules;
  models_json?: string;
  models_path?: string;
  enabled?: boolean;
  note?: string;
  credential_label?: string;
  credential_hint?: string;
}

// AccountProtocol is the wire-protocol vocabulary an upstream endpoint
// declares it speaks natively (mirrors biz.NativeProtocol*).
export type AccountProtocol = "chat_completions" | "responses" | "messages" | "web";
// Protocols whose upstream path an account may override; web tool vendors
// (parallel / brave / jina / firecrawl) have fixed paths per vendor.
export type PathProtocol = Exclude<AccountProtocol, "web">;

// PoolMode overrides the account's health policy. "" inherits the site default.
export type PoolMode = "" | "balanced" | "conservative" | "lenient" | "observe";

// Account is the admin view of one upstream account: endpoint + dialect +
// credential + scheduling + health + quota. The account layer is flat now —
// there is no provider/channel above it. Secrets never appear here.
export interface Account {
  id: string;
  name: string;
  // Endpoint & protocol declaration: everything that used to live on the
  // channel row now travels with the account.
  base_url: string;
  // Generation-endpoint dialect adaptor (openai/dashscope/volcengine/...).
  // Empty = inferred from base_url host.
  api_format?: string;
  native_protocols: AccountProtocol[];
  // The dialect a converted request is spoken to in. Empty = first native.
  default_protocol?: string;
  allow_conversion: boolean;
  // Per-protocol upstream path overrides for vendors outside /v1/* (e.g.
  // {"chat_completions": "/v2/chat/completions"}). Absent = gateway defaults.
  endpoint_paths?: Partial<Record<AccountProtocol, string>>;
  // Upstream header rewrites. Absent = headers go out unchanged.
  header_rules?: HeaderRules;
  // Optional config the flat account view may omit; the admin dialog reads
  // them when present and otherwise leaves the field at its default.
  organization?: string;
  block_image_generation_tool?: boolean;
  estimate_cache_write?: boolean;
  cache_write_min_context_tokens?: number;
  // Links a migrated account to the channel row it came from ("" for accounts
  // created after the migration).
  legacy_provider_id?: string;
  // Provider definition this account was created from ("azure", …).
  definition_id?: string;
  status: "active" | "disabled" | "expired" | "error";
  health: "healthy" | "degraded" | "unhealthy" | "unknown";
  priority: number;
  weight: number;
  max_concurrency: number;
  last_used_at?: string | null;
  last_error?: string;
  note?: string;
  has_credential: boolean;
  cooldown_active: boolean;
  cooldown_reason?: string;
  cooldown_ttl_seconds?: number;
  created_at: string;
  updated_at: string;
  // --- health state (account state machine) ---
  schedulable: boolean;
  pool_mode?: PoolMode;
  effective_pool_mode: "balanced" | "conservative" | "lenient" | "observe";
  rate_limit_reset_at?: string | null;
  overload_until?: string | null;
  temp_unschedulable_until?: string | null;
  temp_unschedulable_reason?: string;
  last_error_class?: string;
  last_error_at?: string | null;
  consecutive_failures: number;
  model_cooldowns?: Record<string, { until_ms?: number; reason?: string }>;
  quota_snapshot?: QuotaSnapshot | null;
  quota_checked_at?: string | null;
  // Whether the vendor exposes a usage endpoint the gateway can probe.
  has_usage_endpoint?: boolean;
  schedulable_now: boolean;
}

// QuotaSnapshot is what the vendor reports or the gateway observes passively
// about an account's remaining allowance.
export interface QuotaSnapshot {
  source?: "passive" | "probe";
  at?: number;
  plan?: string;
  windows?: { name: string; used_percent?: number; used?: number; limit?: number; reset_at?: number; status?: string }[];
  balance?: { amount: number; currency?: string };
}

// AccountEndpointFields is the endpoint, dialect and request-shaping block an
// account carries. It is optional here so the update body can omit anything it
// does not want to change (only the sent fields change); the create body
// tightens base_url/native_protocols to required.
export interface AccountEndpointFields {
  base_url?: string;
  api_format?: string;
  native_protocols?: AccountProtocol[];
  default_protocol?: string;
  allow_conversion?: boolean;
  // Whole-map replacement: send only the protocols that keep an override.
  endpoint_paths?: Partial<Record<AccountProtocol, string>>;
  // Whole replacement; an empty object clears the rules. Omitted on create =
  // the definition's rules (if any).
  header_rules?: HeaderRules;
  organization?: string;
  // Request shaping: rewrites the body sent upstream, not the client contract.
  block_image_generation_tool?: boolean;
  estimate_cache_write?: boolean;
  cache_write_min_context_tokens?: number;
}

// AccountInput is the create body. The cache-write estimation fields are
// update-only: the create handler does not read them.
export interface AccountInput extends Omit<AccountEndpointFields, "estimate_cache_write" | "cache_write_min_context_tokens"> {
  name: string;
  // definition_id prefills endpoint/protocols/models from a provider
  // definition; explicit fields in this body still win.
  definition_id?: string;
  base_url: string;
  native_protocols: AccountProtocol[];
  api_key?: string;
  priority?: number;
  weight?: number;
  max_concurrency?: number;
  note?: string;
  pool_mode?: PoolMode;
}

// AccountUpdate is the PATCH body. Every field is optional and only the fields
// actually sent change. legacy_provider_id is fixed at creation and is not
// accepted here.
export interface AccountUpdate extends AccountEndpointFields {
  name?: string;
  api_key?: string; // omit = keep, non-empty = replace
  status?: "active" | "disabled";
  priority?: number;
  weight?: number;
  max_concurrency?: number;
  note?: string;
  // "" clears the override and inherits the site default.
  pool_mode?: PoolMode;
}

// AccountTestResult is the verdict of POST /accounts/:id/test — one real
// request sent to the account's own upstream. ok=false carries either a
// transport/credential error in error or the upstream status + truncated body
// in detail.
export interface AccountTestResult {
  ok: boolean;
  // Upstream HTTP status; 0 when nothing answered.
  status: number;
  latency_ms: number;
  // Model name actually sent upstream (a binding may rewrite it).
  model: string;
  protocol: string;
  url: string;
  error?: string;
  detail?: string;
  // The start of what the upstream answered on success.
  sample?: string;
}

// AccountModel is one model binding under an account (the flat successor of
// the old provider→model map).
export interface AccountModel {
  id: string;
  account_id: string;
  model_name: string;
  upstream_model?: string;
  upstream_protocol?: string;
  enabled: boolean;
  cost_mode: "none" | "per_request" | "per_usage";
  price_per_request_cost?: string;
  input_unit_cost?: string;
  output_unit_cost?: string;
  cache_read_input_cost?: string;
  cache_write_input_cost?: string;
  cache_write_1h_input_cost?: string;
  currency: string;
  // Display-only reference prices from the public catalog / upstream list
  // (USD per 1M tokens) — never read by billing.
  ref_input_cost?: string;
  ref_output_cost?: string;
  // Overrides the model's context_window for this account; absent = inherit.
  context_window?: number;
}

// AccountModelInput mirrors the write body. Note the asymmetric cost field
// names: the request takes cache_*_unit_cost while the view returns
// cache_*_input_cost (see admin_accounts.go).
export interface AccountModelInput {
  model_name: string;
  upstream_model?: string;
  upstream_protocol?: string;
  enabled?: boolean;
  cost_mode?: "none" | "per_request" | "per_usage";
  currency?: string;
  price_per_request_cost?: string;
  input_unit_cost?: string;
  output_unit_cost?: string;
  cache_read_unit_cost?: string;
  cache_write_unit_cost?: string;
  cache_write_1h_unit_cost?: string;
  ref_input_cost?: string;
  ref_output_cost?: string;
  // 0 clears the override (inherit the model default); absent = no change.
  context_window?: number;
}

// AccountModelsSyncResult is the summary of pulling the upstream /models
// catalog into an account's bindings.
export interface AccountModelsSyncResult {
  remote_count: number;
  added: number;
  existing: number;
  // Rows already bound that the sync backfilled (upstream_model /
  // upstream_protocol / ref costs) without touching manual settings.
  updated: number;
  added_models: string[];
  // Upstream ids no enabled platform model claims — skipped, not bound.
  unmatched_models?: string[];
}

// AccountModelUsageRow is one (model, upstream_model) slice of an account's
// usage_logs traffic for a window. cost is the account cost basis (what the
// binding prices say we owe the upstream); revenue is what users were charged
// (usage_logs.cost_final). Both CNY decimal strings.
export interface AccountModelUsageRow {
  model: string;
  upstream_model: string;
  requests: number;
  tokens_in: number;
  tokens_out: number;
  cached_tokens_in: number;
  cache_creation_tokens_in: number;
  output_images: number;
  cost: string;
  revenue: string;
}

// RemoteCatalogProvider / RemoteCatalogModel are entries of the cached public
// model catalog (models.dev). Costs are USD per 1M tokens, display-only.
export interface RemoteCatalogProvider {
  id: string;
  name: string;
  models: number;
}

export interface RemoteCatalogModel {
  id: string;
  name: string;
  context?: number;
  cost?: { input: number; output: number; cache_read?: number; cache_write?: number };
  // Present only on the merged canonical view (/model-catalog/models): the
  // catalog providers this canonical model was seen under.
  providers?: string[];
}

// ImportAccountModelItem is one row of the bulk-bind body (catalog picker or
// hand-picked ids).
export interface ImportAccountModelItem {
  model_name: string;
  upstream_model?: string;
  upstream_protocol?: string;
  ref_input_cost?: string;
  ref_output_cost?: string;
}

// ImportPlatformModelItem is one row of the platform-model bulk import.
// Prices are storage units (per-1K points) like createModel — the client
// applies any currency conversion itself; the backend never treats catalog
// USD costs as billing prices.
export interface ImportPlatformModelItem {
  name: string;
  billing_mode?: string;
  description?: string;
  currency?: string;
  enabled?: boolean;
  price_per_request?: string;
  input_unit_price?: string;
  output_unit_price?: string;
  cache_read_input_price?: string;
  cache_write_input_price?: string;
  context_window?: number;
}

// PlatformModelsImportResult: rows with an existing name are skipped, never
// overwritten; per-row failures are reported alongside the successes.
export interface PlatformModelsImportResult {
  added: number;
  existing: number;
  failed: { name: string; error: string }[];
  added_models: string[];
}

// AccountPool (管理分组) is a purely organizational pool of accounts that share
// a health / quota policy. It is NOT a routing entity and is never visible to
// end users.
export interface AccountPool {
  id: string;
  name: string;
  note?: string;
  sort_order: number;
  pool_mode: string;
  enabled: boolean;
  quota_policy?: Record<string, unknown>;
  account_count: number;
  created_at: string;
  updated_at: string;
}

// GroupAccount is one account bound to a business model group (业务分组直连账号):
// the link's scheduling inputs plus the account's health summary.
export interface GroupAccount {
  binding_id: string;
  group_id: string;
  account_id: string;
  account_name: string;
  priority: number;
  weight: number;
  enabled: boolean;
  status: string;
  health: string;
  schedulable: boolean;
  base_url?: string;
  kind?: string;
  protocols?: string[];
  has_credential: boolean;
  cooldown_active: boolean;
  cooldown_reason?: string;
  schedulable_now: boolean;
  // model_names is the optional per-group model whitelist; absent/empty
  // means the account serves every model it is configured for.
  model_names?: string[];
}

// AccountListParams mirrors the account list query string.
export interface AccountListParams {
  status?: string;
  health?: string;
  pool_id?: string;
  group_id?: string;
  protocol?: string;
  q?: string;
  limit?: number;
  offset?: number;
}

// PriceTier is one context-length band of a model's tiered pricing.
// Selected by the request's input-side token total (non-cached +
// cache-read + cache-write); up_to_tokens is the inclusive upper
// bound, 0 = unbounded (the last band). Prices left unset inherit the
// model's base price of the same name. All prices are per-1K storage
// strings, same convention as the flat fields on Model.
export interface PriceTier {
  up_to_tokens: number;
  input_unit_price?: string;
  output_unit_price?: string;
  cache_read_input_price?: string;
  cache_write_input_price?: string;
  cache_write_1h_input_price?: string;
}

export interface Model {
  id: string;
  name: string;
  display_name?: string;
  // Left out of client model pickers; the model stays callable by name.
  picker_hidden: boolean;
  billing_mode: "per_request" | "per_token";
  price_per_request?: string;
  input_unit_price?: string;
  output_unit_price?: string;
  cache_read_input_price?: string;
  cache_write_input_price?: string;
  cache_write_1h_input_price?: string;
  image_input_unit_price?: string;
  image_output_unit_price?: string;
  audio_input_unit_price?: string;
  audio_output_unit_price?: string;
  // Flat surcharges for the non-chat generation endpoints (points per unit).
  output_image_price?: string;
  output_second_price?: string;
  input_character_price?: string;
  price_tiers?: PriceTier[];
  currency: string;
  enabled: boolean;
  description?: string;
  vendor_id?: string;
  // Lower shows first; ties fall back to name. Rewritten densely by the
  // reorder endpoint, so treat it as opaque rather than editing it by hand.
  sort_order: number;
  // Capability metadata; absent = not configured (never defaulted).
  context_window?: number;
  max_output_tokens?: number;
  reasoning_efforts?: string[];
  // What /v1/models reports: smallest / largest window among the accounts
  // serving the model (an account binding may override context_window).
  advertised_context_window?: number;
  advertised_max_context_window?: number;
}

export interface Vendor {
  id: string;
  name: string;
  slug: string;
  description?: string;
  sort_order: number;
}

export interface UserGroup {
  id: string;
  name: string;
  description?: string;
  sort_order: number;
  concurrency_limit: number;
  is_default: boolean;
}

export interface PagedUsers {
  users: any[];
  total: number;
  limit: number;
  offset: number;
}

export interface PagedLogs {
  logs: any[];
  total: number;
  limit: number;
  offset: number;
}

export interface OrganizationMember {
  user_id: string;
  email?: string;
  nickname?: string | null;
  role: "owner" | "admin" | "member";
  monthly_spend_limit: string;
  /** Cumulative cap over the member's whole history in this org. "0" = unlimited. */
  total_spend_limit: string;
  created_at: string;
}

export interface Organization {
  id: string;
  name: string;
  owner_user_id: string;
  balance: string;
  currency: string;
  monthly_spend_limit: string;
  created_at: string;
  updated_at?: string;
  // The querying user's own role in this organization; set on the
  // /v1/me/organizations list so the console can gate manager-only tabs.
  role?: "owner" | "admin" | "member";
  members?: OrganizationMember[];
}

// OrgOverview backs the console overview page in org context. The
// org-wide financial fields are only present for owners/admins.
export interface OrgOverview {
  organization: { id: string; name: string; currency: string; created_at: string };
  my_role: "owner" | "admin" | "member";
  my_monthly_spend: string;
  my_monthly_spend_limit: string;
  my_total_spend: string;
  my_total_spend_limit: string;
  balance?: string;
  held_balance?: string;
  monthly_spend_limit?: string;
  monthly_spend?: string;
  member_count?: number;
  api_key_count?: number;
}

export interface OrgBillingEntry {
  id: string;
  created_at: string;
  kind: "topup" | "deduct" | "adjust";
  amount: string;
  balance_after: string;
  currency: string;
  note: string;
  actor_user_id?: string;
  actor_email?: string;
  related_usage_log_id?: string;
  hold_amount?: string;
}

// AnomalyFilter mirrors ?anomaly= on /v1/admin/usage-logs. Omitting it
// keeps the pre-anomaly behavior (no filtering, no implicit time window).
export type AnomalyFilter = "any" | "time" | "error" | "none";

// UsageLogAnomaly is the per-row verdict the admin log endpoint attaches
// (biz.AnomalyFlags backend-side). Older backends omit it entirely, so
// every reader must treat it as optional.
export interface UsageLogAnomaly {
  time: boolean;
  error: boolean;
  reasons: string[];
}

// UsageLogFilterParams — superset of the query strings accepted by
// both /v1/me/usage-logs and /v1/admin/usage-logs.
export interface UsageLogFilterParams {
  q?: string;
  status?: string;
  endpoint?: string;
  billing_source?: string;
  model?: string;
  ip?: string;
  api_key?: string;
  request_id?: string;
  start?: string;
  end?: string;
  limit?: number;
  offset?: number;
  // Admin-only — ignored on /me endpoint.
  user_id?: string;
  user?: string;
  anomaly?: AnomalyFilter;
  // Carried from the 性能观测 drill-down links. The log endpoint ignores
  // params it does not know, so sending them is harmless on a backend
  // that has not shipped the corresponding filters yet.
  provider_id?: string;
  group_id?: string;
  // Admin-only: 1 skips the COUNT for a page turn inside an unchanged
  // filter set; the response then carries total = -1 and the client keeps
  // the total it already has.
  skip_count?: number;
  // Org-manager only on /me endpoints: narrow the org-wide view to one
  // member. Ignored for personal / member scopes.
  member_user_id?: string;
}

// One axis of the six-dimension radar chart on the public group intro card.
// Unset dimensions are simply absent — never rendered as zero.
export interface GroupProfileDim {
  name: string;
  value: number;
}

export interface ModelGroup {
  id: string;
  name: string;
  billing_source: "plan_only" | "balance_only";
  rate_multiplier: number;
  description?: string;
  profile?: GroupProfileDim[];
  allowed_user_group_ids: string[];
  granted_user_ids?: string[];
  granted_user_count?: number;
  exclusive?: boolean;
  // True when the group serves an image-generation model.
  supports_image?: boolean;
  // Same verdict as the public status page, including exclusive groups.
  status?: "healthy" | "degraded" | "serious" | "down" | "unknown";
  // False = retired: stops routing and is hidden from all user-facing
  // surfaces, but stays visible to admins so it can be re-enabled.
  enabled?: boolean;
  model_ids?: string[];
  // Enabled model names and their vendor names. /me/available-groups fills
  // these so the key picker can search by 厂商, model, or group name.
  models?: string[];
  vendors?: string[];
  // Per-link settings model_ids cannot carry. Parallel to model_ids: same
  // members, plus the probe configuration attached to the link.
  model_links?: ModelGroupModelLink[];
  provider_ids?: string[];
  provider_links?: ModelGroupProviderLink[];
  // Lower shows first; ties fall back to name.
  sort_order: number;
}

export interface ModelGroupProviderLink {
  provider_id: string;
  priority: number;
  weight: number;
}

// The probe belongs to the group↔model link, not to the model: the same model
// reached through two groups sits behind different providers and is probed
// independently, so each link owns its own switch and cadence.
export interface ModelGroupModelLink {
  model_id: string;
  probe_enabled: boolean;
  probe_interval_seconds: number;
  // The model's context window through this group; absent = the model default.
  context_window?: number;
}

export interface Plan {
  id: string;
  name: string;
  price: string;
  currency: string;
  type: "quota_subscription" | "pay_as_you_go";
  period_days: number;
  quota_5h: string;
  quota_weekly: string;
  quota_monthly: string;
  allowed_group_ids: string[];
  status: "active" | "archived";
  description?: string;
  // Populated by /v1/me/plans (resolved server-side) — admin /v1/admin/plans
  // returns only allowed_group_ids; UI should fall back to empty when missing.
  allowed_groups?: Array<{ id: string; name: string; billing_source: string }>;
  concurrency_limit: number;
  monthly_spend_limit: string;
  initial_balance: string;
  auto_reload_enabled: boolean;
  auto_reload_threshold: string;
  auto_reload_amount: string;
}

export interface PlanUsage {
  has_active_plan: boolean;
  plan_name?: string;
  plan_type?: Plan["type"];
  expire_at?: string;
  currency?: string;
  quota_5h?: string;
  used_5h?: string;
  quota_weekly?: string;
  used_weekly?: string;
  quota_monthly?: string;
  used_monthly?: string;
  reset_5h_at?: string;
  reset_weekly_at?: string;
  reset_monthly_at?: string;
  monthly_spend_limit?: string;
  initial_balance?: string;
  monthly_spend_used?: string;
  auto_reload_enabled?: boolean;
  auto_reload_threshold?: string;
  auto_reload_amount?: string;
  banked_resets?: BankedReset[];
}

// A quota reset an admin granted for the user to spend when they choose; it
// clears the used quota of its window.
export interface BankedReset { id: string; window: "all" | "5h" | "weekly" | "monthly"; created_at: string }


export interface Announcement {
  id: string;
  title: string;
  content: string;
  enabled: boolean;
  display_mode: "always" | "after_login";
}

export type InviteSignupCondition = "none" | "email_verified" | "any_topup" | "min_topup";

export interface InviteProgram {
  enabled: boolean;
  signup_reward_enabled: boolean;
  signup_reward_amount: string;
  signup_condition: InviteSignupCondition;
  signup_min_topup_amount: string;
  topup_rebate_enabled: boolean;
  topup_rebate_rate_pct: string;
}

// What /v1/me/invite adds on top of InviteProgram for the rules text.
export interface InviteProgramPublic extends InviteProgram {
  topup_rebate_window_days: number;
  // Flagged payouts are held for an admin.
  risk_review: boolean;
  // Payouts made while the inviter's email is unverified are held.
  email_verification_hold: boolean;
}

// Reason codes for a held reward or rebate.
export type InviteHeldReason =
  | "inviter_email_unverified"
  | "inviter_account_too_new"
  | "inviter_account_restricted"
  | "self_referral_suspected"
  | "daily_limit"
  | "amount_review";

export interface InviteStats {
  invited_count: number;
  qualified_count: number;
  pending_count: number;
  held_count: number;
  signup_reward_paid: string;
  // Net rebate credited: paid minus anything a refund clawed back. Covers
  // both live topup rebates and the retired consumption rebates.
  topup_rebate_paid: string;
}

export interface Invitee {
  user_id: string;
  email_masked: string;
  created_at: string;
  signup_reward_status: string;
  signup_reward_amount: string;
  rebate_paid: string;
  signup_condition: InviteSignupCondition | "";
  signup_paid_amount: string;
  signup_required_amount: string;
  signup_email_verified: boolean;
  signup_email_verification_required: boolean;
  rebate_held: string;
  rebate_email_verification_required: boolean;
  signup_reward_held_reasons: InviteHeldReason[];
  rebate_held_reasons: InviteHeldReason[];
}

export interface InviteResponse {
  invite_code: string;
  invite_url: string;
  program: InviteProgramPublic;
  stats: InviteStats;
  invitees: Invitee[];
  rewards: Array<Record<string, unknown>>;
  rebates: Array<Record<string, unknown>>;
  total?: number;
  limit?: number;
  offset?: number;
  inviter_email_verification_required: boolean;
}

export type OpsAlertWebhookType = "generic" | "dingtalk" | "feishu";

export interface MarketingBannerConfig {
  enabled: boolean;
  text: string;
  link_url: string;
}

export interface SiteSettings {
  site_name: string;
  site_description: string;
  site_url: string;
  site_logo: string; // http(s):// URL or data:image/...;base64,...
  icp_number: string;
  contact_email: string;
  // Community group shown in the contact dock: its display name (default
  // "QQ 群"), number or id, and QR image.
  contact_group_label: string;
  contact_group: string;
  contact_group_qr_url: string; // /v1/public/site-assets/<name> or external http(s) URL
  contact_dock_enabled: boolean;
  marketing_banner?: MarketingBannerConfig;
  announcement_enabled: boolean;
  announcement_display_mode: "always" | "after_login";
  announcement_title: string;
  announcement_content: string;
  announcements: Announcement[];
  endpoint_openai_responses: string;
  endpoint_openai_compat: string;
  endpoint_anthropic: string;
  registration_enabled: boolean;
  registration_requires_approval: boolean;
  payment_alipay_enabled: boolean;
  // Top-up amounts (points) offered as one-tap buttons on the plans page.
  topup_presets: number[];
  agent_coupons_enabled?: boolean;
  // Self-service refunds (public) and automatic approval of refund tickets
  // (admin only). refund_auto_approve_max: "" or "0" means no cap.
  self_refund_enabled?: boolean;
  refund_auto_approve_enabled?: boolean;
  refund_auto_approve_max?: string;
  sticky_ttl_seconds: number;
  status_page_group_ids: string[];
  status_page_window_hours: number;
  status_page_upstream_enabled: boolean;
  // Operations alerts: status-page incidents and provider health flips
  // pushed to a webhook. Disabled by default. Both the URL and the
  // signing secret are write-only (encrypted at rest); the backend
  // returns only the *_set presence flags, and an empty string on write
  // keeps the stored value.
  ops_alert_enabled: boolean;
  ops_alert_webhook_type: OpsAlertWebhookType;
  ops_alert_webhook_url?: string;
  ops_alert_webhook_url_set?: boolean;
  ops_alert_webhook_secret?: string;
  ops_alert_webhook_secret_set?: boolean;
  ops_alert_timeout_seconds: number;
  ops_alert_max_retries: number;
  ops_alert_silence_seconds: number;
  // SMTP outbound
  smtp_host: string;
  smtp_port: number;
  smtp_username: string;
  smtp_password?: string;    // write-only; backend never returns the plaintext
  smtp_password_set?: boolean; // read-only flag indicating a password is configured
  smtp_from_email: string;
  smtp_from_name: string;
  smtp_use_tls: boolean;
  // Email-verification feature toggles
  require_register_verification: boolean;
  require_change_email_verification: boolean;
  allow_password_reset: boolean;
  // Gateway: gate model-emitted images. When false the gateway
  // substitutes every image block in the response stream with a
  // text notice; when true images pass through and are billed via
  // Model.OutputImagePrice.
  allow_image_output: boolean;
  format_conversion_enabled: boolean;
  websocket_enabled: boolean;
  // Stream first visible text delta timeout in seconds. 0 disables.
  stream_first_text_timeout_seconds: number;
  // Content moderation (OpenAI Moderation API). Async, off the request
  // hot path. moderation_api_key is write-only (encrypted at rest); the
  // backend never returns it — moderation_api_key_set reports presence.
  // moderation_threshold is the category-score cutoff (0-1): lower = stricter.
  moderation_enabled: boolean;
  moderation_base_url: string;
  moderation_model: string;
  moderation_api_key?: string;      // write-only; backend never returns the plaintext
  moderation_api_key_set?: boolean; // read-only flag indicating a key is configured
  moderation_threshold: number;
  moderation_sample_rate: number;
  moderation_max_per_min: number;
  // Channel settlement rate (CNY per 1 USD). Only used to settle
  // channel/upstream costs that vendors quote in USD — it no longer
  // drives any user-facing display, which is denominated in platform
  // points (1 积分 = 1 CNY).
  exchange_cny_per_usd: number;
  // Max percent of an order's face value any incentive may give away:
  // an agent's budget (redeem-code margin, or coupon discount + rebate)
  // and the invite top-up rebate rate. New agents start at this budget.
  max_incentive_pct: number;
  // Reseller refunds of redeem codes: a plan refund is price × remaining
  // days ÷ day_divisor × plan_rate_pct%; a balance refund is the face value
  // × balance_rate_pct%.
  agent_plan_refund_day_divisor: number;
  agent_plan_refund_rate_pct: number;
  agent_balance_refund_rate_pct: number;
  // Ordered probes that extract a request's thinking-strength label
  // from the inbound body; the label lands in usage_log.reasoning_effort.
  // [] disables extraction. The backend seeds the built-in defaults.
  reasoning_parse_rules: ReasoningParseRule[];
  // Invite / referral program rules. Optional so settings GET/PUT keeps
  // typechecking against backends that don't ship the field yet; the
  // admin invite page merges it into the full settings blob on save.
  invite_program?: InviteProgramSettings;
}

// ---- invite program (admin) ----

// The user-facing invite page only sees the InviteProgram subset; the admin
// settings blob carries the full rule set including the risk controls.
export interface InviteProgramSettings extends InviteProgram {
  topup_rebate_window_days: number;
  topup_rebate_cap_per_invitee: string;
  signup_daily_count_cap_per_inviter: number;
  signup_daily_amount_cap_per_inviter: string;
  inviter_min_age_hours: number;
  same_ip_daily_cap: number;
  review_amount_threshold: string;
  auto_hold_on_risk: boolean;
  require_inviter_email_verified: boolean;
}

// Rewards and rebates share one status ramp; bindings only use
// bound / voided.
export type InvitePayoutStatus = "pending" | "qualified" | "held" | "paid" | "rejected" | "voided";
export type InviteBindingStatus = "bound" | "voided";

export interface InviteBinding {
  id: string;
  inviter_user_id: string;
  inviter_email?: string;
  invitee_user_id: string;
  invitee_email?: string;
  invite_code?: string;
  status: InviteBindingStatus;
  register_ip?: string;
  created_at: string;
}

export interface InviteReward {
  id: string;
  binding_id?: string;
  inviter_user_id: string;
  inviter_email?: string;
  invitee_user_id: string;
  invitee_email?: string;
  amount: string;
  currency?: string;
  condition_snapshot?: string;
  status: InvitePayoutStatus;
  risk_flags?: string[];
  note?: string;
  created_at: string;
  paid_at?: string;
  paid_balance_tx_id?: string;
}

export interface InviteRebate {
  id: string;
  inviter_user_id: string;
  inviter_email?: string;
  invitee_user_id: string;
  invitee_email?: string;
  // "topup" is the live program (one rebate per paid balance order);
  // "consume" rows are the retired per-request rebates kept for history.
  source_kind: "topup" | "consume";
  source_order_id?: string;
  source_usage_log_id?: string;
  // The order (or, for retired rows, the charge) this rebate was computed
  // from, and the rate applied to it at that moment.
  source_amount: string;
  rate_pct_snapshot: string;
  rebate_amount: string;
  // How much of a paid rebate a refund has taken back.
  clawed_back_amount: string;
  currency?: string;
  status: InvitePayoutStatus;
  risk_flags?: string[];
  note?: string;
  created_at: string;
  paid_at?: string;
  paid_balance_tx_id?: string;
}

export interface InviteListParams {
  q?: string;
  status?: string;
  inviter_id?: string;
  invitee_id?: string;
  limit?: number;
  offset?: number;
}

// Admin invite rows have carried two generations of key names: the short
// ones (inviter_id / amount / base_amount / rate_pct / ip) and the DB column
// names (inviter_user_id / rebate_amount / source_amount /
// rate_pct_snapshot / register_ip). The UI reads the column names, so
// normalize toward those — a backend that only sends the short set must not
// leave the table blank.
function invitePartyIDs(raw: Record<string, unknown>): { inviter_user_id: string; invitee_user_id: string } {
  return {
    inviter_user_id: String(raw.inviter_user_id ?? raw.inviter_id ?? ""),
    invitee_user_id: String(raw.invitee_user_id ?? raw.invitee_id ?? ""),
  };
}

export function normalizeInviteBinding(raw: InviteBinding | Record<string, unknown>): InviteBinding {
  const row = raw as Record<string, unknown>;
  return {
    ...(raw as InviteBinding),
    ...invitePartyIDs(row),
    register_ip: String(row.register_ip ?? row.ip ?? "") || undefined,
  };
}

export function normalizeInviteReward(raw: InviteReward | Record<string, unknown>): InviteReward {
  const row = raw as Record<string, unknown>;
  return {
    ...(raw as InviteReward),
    ...invitePartyIDs(row),
    amount: String(row.amount ?? "0"),
  };
}

export function normalizeInviteRebate(raw: InviteRebate | Record<string, unknown>): InviteRebate {
  const row = raw as Record<string, unknown>;
  return {
    ...(raw as InviteRebate),
    ...invitePartyIDs(row),
    source_amount: String(row.source_amount ?? row.base_amount ?? "0"),
    rate_pct_snapshot: String(row.rate_pct_snapshot ?? row.rate_pct ?? "0"),
    rebate_amount: String(row.rebate_amount ?? row.amount ?? "0"),
    // The confirm dialog subtracts this, so an absent key must read 0 and
    // not turn the payout amount into NaN.
    clawed_back_amount: String(row.clawed_back_amount ?? "0"),
  };
}

// ReasoningParseRule: one probe into the LLM request body. Rules run
// in order; the first that yields a non-empty value wins.
// kind=number records the RAW numeric value (e.g. the thinking-token
// budget, shown to users as "thinking:16384") — token counts are never
// bucketed into strength labels.
export interface ReasoningParseRule {
  path: string; // dot path, e.g. "reasoning.effort" / "thinking.budget_tokens"
  kind: "string" | "number" | "exists";
  value_map?: Record<string, string>; // kind=string: raw → label; missing keys pass through raw
  label?: string; // kind=exists: emitted label; kind=number: optional display prefix
}

// ---- tickets ----
export type TicketStatus = "open" | "in_progress" | "waiting_user" | "resolved" | "closed";
export type TicketPriority = "low" | "normal" | "high" | "urgent";
export type TicketCategory = "general" | "refund" | "billing" | "technical" | "account";

export interface TicketRow {
  id: string;
  ticket_no?: number;
  ticket_no_label?: string;
  title: string;
  category?: TicketCategory;
  status: TicketStatus;
  priority: TicketPriority;
  last_reply_at: string;
  created_at: string;
  user_id?: string;
  user_email?: string;
  assignee_user_id?: string;
  order_id?: string;
  unread?: boolean;
  unread_kind?: "unread" | "reply" | "";
}

export interface TicketAttachment {
  name: string;
  type: string;
  id?: string;
  url?: string;
  data_url?: string;
}

export interface TicketMessage {
  id: string;
  sender_role: "user" | "cs" | "admin" | "system";
  sender_id: string;
  body: string;
  attachments?: TicketAttachment[];
  internal_note?: boolean;
  created_at: string;
}

export interface TicketDetail {
  ticket: TicketRow;
  messages: TicketMessage[];
}

export interface TicketAssignee {
  id: string;
  email: string;
  role: "user" | "admin";
  nickname?: string | null;
}

// ---- agent ----
export interface AgentProfile {
  agent_id: string;
  platform_share_pct: string;
  status: "active" | "suspended";
  note?: string;
  created_at?: string;
}

export interface AgentDashboard {
  profile: { platform_share_pct: string; status: string };
  pending: { total_payable: string; entries_count: number; sales: string; refunds: string; coupon_rebates: string; withdrawn: string; withdrawable: string };
}

export type AgentEntryKind = "sale" | "refund" | "coupon_rebate" | "coupon_refund" | "balance_withdraw";

export interface AgentEntry {
  id: string;
  kind: AgentEntryKind;
  sale_amount: string;
  platform_payable: string;
  share_pct_at_event: string;
  currency: string;
  source_type: string;
  source_id: string;
  redeem_code_id?: string;
  order_id?: string;
  user_id?: string;
  settlement_id?: string | null;
  created_at: string;
}

export interface AgentSettlement {
  id: string;
  total_payable: string;
  entries_count: number;
  currency: string;
  created_at: string;
  note?: string;
  settled_by_user_id?: string;
}

export interface AgentRefund {
  id: string;
  redeem_code_id: string;
  redeem_record_id: string;
  user_id: string;
  kind: "balance" | "plan";
  refund_amount: string;
  platform_deduct: string;
  currency: string;
  reason?: string;
  created_at: string;
}

export interface RefundPreview {
  code: string;
  redeem_record_id: string;
  user_id: string;
  kind: "balance" | "plan";
  refund_amount: string;
  platform_deduct: string;
  already_refunded: boolean;
  currency: string;
  plan_name?: string;
  plan_price?: string;
  remaining_days?: number;
}

export interface AdminAgentRow extends AgentProfile {
  email?: string;
  nickname?: string;
  pending_total_payable?: string;
  pending_entries?: number;
  pending_sales?: string;
  pending_refunds?: string;
  pending_coupon_rebates?: string;
  coupon_code?: string;
  coupon_discount_pct?: string;
  coupon_rebate_rate_pct?: string;
}

export interface CouponQuote {
  coupon_code: string;
  discount_pct: string;
  // What the buyer's inviter would lose if this coupon is used on a balance
  // top-up. Absent when the invite service is unavailable.
  invite_conflict?: InviteCouponConflict;
}

export interface InviteCouponConflict {
  topup_rebate: boolean;
  signup_reward: boolean;
}

// Agent's own coupon as GET /v1/agent/coupon returns it. budget_pct is the
// agent's incentive budget under the site cap; discount + rebate = budget.
export interface AgentCoupon {
  coupon_code?: string;
  status: "active" | "suspended";
  budget_pct: string;
  discount_pct: string;
  rebate_rate_pct: string;
  buyers: number;
}

export interface SiteOrder {
  id: string;
  order_no: string;
  user_id?: string;
  kind: "balance" | "plan";
  plan_id?: string;
  user_plan_id?: string;
  amount: string;
  credit_amount?: string;
  coupon_code?: string;
  coupon_discount_pct?: string;
  currency: string;
  status: "pending" | "paid" | "canceled" | "partially_refunded" | "refunded";
  note?: string;
  payment_channel?: "manual" | "alipay" | string;
  payment_trade_type?: string;
  payment_code_url?: string;
  payment_trade_state?: string;
  payment_transaction_id?: string;
  payment_amount_cents?: number;
  // Currency and rate frozen when the payment was created.
  payment_currency?: string;
  payment_exchange_rate?: string;
  paid_at?: string;
  created_at: string;
  // Only populated by the admin listing, which eager-loads the buyer.
  user_email?: string;
  user_nickname?: string;
  refunds?: Array<Pick<SiteRefund, "id" | "amount" | "status" | "reason" | "created_at">>;
  refund_ticket?: { id: string; status: TicketStatus };
}

export interface SiteRefund {
  id: string;
  order_id: string;
  user_id?: string;
  // Only populated by the admin listing, which eager-loads the order.
  order_no?: string;
  user_email?: string;
  user_nickname?: string;
  amount: string;
  currency: string;
  status: "pending" | "approved" | "rejected" | "canceled";
  reason?: string;
  note?: string;
  reviewed_by_user_id?: string;
  reviewed_at?: string;
  created_at: string;
}

export interface ProviderCost {
  id: string;
  provider_id: string;
  amount: string;
  currency: string;
  occurred_at: string;
  note?: string;
  created_at: string;
}

export interface FinanceDayPoint {
  date: string;
  income: string;
  expense: string;
}

export interface FinanceSummary {
  from: string;
  to: string;
  agent_income: string;
  site_income: string;
  total_income: string;
  fixed_cost: string;
  manual_cost: string;
  total_cost: string;
  net_profit: string;
  providers: Array<{
    provider_id: string;
    provider_name: string;
    fixed_cost: string;
    manual_cost: string;
    total_cost: string;
  }>;
  days?: FinanceDayPoint[];
}

export interface OpsPoint {
  period: string;
  new_users: number;
  active_users: number;
  paying_users: number;
  revenue: string;
  refunds: string;
  net: string;
  requests: number;
}

export interface OpsNamedAmt {
  key: string;
  label: string;
  count: number;
  amount: string;
}

export interface OpsReport {
  from: string;
  to: string;
  grain: "day" | "month";
  kpi: {
    gross_revenue: string;
    refunds: string;
    net_revenue: string;
    paying_users: number;
    new_paying_users: number;
    period_active: number;
    arpu: string;
    arppu: string;
    new_users: number;
    pay_conversion: string;
    dau: number;
    wau: number;
    mau: number;
    stickiness: string;
    requests: number;
    repeat_pay_rate: string;
  };
  compare: {
    from: string;
    to: string;
    net_revenue: string;
    new_users: number;
    paying_users: number;
    requests: number;
    net_revenue_delta: string;
    new_users_delta: string;
    paying_users_delta: string;
    requests_delta: string;
  };
  funnel: {
    signed_up: number;
    called: number;
    paid: number;
    call_rate: string;
    pay_rate: string;
  };
  orders: {
    paid_orders: number;
    aov: string;
    refund_rate: string;
    created: number;
    created_paid: number;
    created_pending: number;
    pay_rate: string;
  };
  plans: { active: number; opened: number; expired: number; canceled: number };
  series: OpsPoint[];
  cohorts: Array<{ month: string; new_users: number; retained: number; rate: string }>;
  top_payers: Array<{ user_id: string; email: string; orders: number; amount: string }>;
  kind_split: Array<{ kind: string; orders: number; amount: string }>;
  plan_mix: Array<{ key: string; label: string; count: number }>;
  plan_sales: OpsNamedAmt[];
  consume_scope: OpsNamedAmt[];
  consume_source: OpsNamedAmt[];
}



export interface BillingSummary {
  from: string;
  to: string;
  income: string;
  expense: string;
  days: FinanceDayPoint[];
}

export interface ProviderSettlementUsageDetail {
  group_id: string;
  group_name: string;
  model: string;
  requests: number;
  tokens_in: number;
  tokens_out: number;
  cached_tokens_in: number;
  cache_creation_tokens_in: number;
  output_images: number;
  cost: string;
}

export interface ProviderSettlementManualCostDetail {
  id: string;
  amount: string;
  currency: string;
  amount_cny: string;
  occurred_at: string;
  note?: string;
}

export interface ProviderSettlement {
  provider_id: string;
  provider_name: string;
  period_start: string;
  period_end: string;
  calculated_cost: string;
  actual_cost: string;
  correction_delta: string;
  currency: string;
  status: "pending" | "confirmed";
  confirmed_by_user_id?: string;
  confirmed_at?: string;
  note?: string;
  usage_cost: string;
  manual_cost: string;
  usage_details: ProviderSettlementUsageDetail[];
  manual_cost_details: ProviderSettlementManualCostDetail[];
}

export interface RefundPolicy {
  plan_no_reason_days: number;
  balance_no_reason_days: number;
  plan_refund_ratio: number;
  balance_refund_ratio: number;
  min_refund_amount: string;
}

// Public /status page payload (no auth required).
//
// The payload ships verdicts, never the evidence behind them: raw request
// counts would let anyone polling this endpoint reconstruct our traffic volume
// cell by cell, so the backend withholds them (json:"-" in internal/biz/status.go)
// and the client renders from the states and percentages alone.
export interface PublicStatusBucket {
  time: string;
  // Already graded server-side against the same thresholds the group card uses,
  // so the client only maps it to a colour.
  state: PublicStatusState;
}
// "serious" sits between degraded and down — badly impaired but still serving
// most requests. Kept ordered worst-last so the union reads as a severity ramp.
export type PublicStatusState = "healthy" | "degraded" | "serious" | "down" | "unknown";
interface PublicStatusMetrics {
  id: string;
  name: string;
  state: PublicStatusState;
  // How far back the state light looks. Read it rather than hardcoding a
  // number: the backend has widened this window before and any copy that
  // repeats it must follow.
  state_window_minutes: number;
  // Rounded to two decimals server-side.
  availability_pct: number | null;
  last_observed_at: string | null;
  fresh: boolean;
  avg_latency_ms: number;
  buckets: PublicStatusBucket[];
}
export interface PublicStatusGroup extends PublicStatusMetrics {
  description: string;
  // Per-model numbers scoped to this group. A model served by two groups sits
  // behind different providers, so it appears once under each with its own
  // availability rather than pooled into a single figure.
  models: PublicStatusModel[];
}
export interface PublicStatusModel extends PublicStatusMetrics {
  group_id: string;
}
// One currently-misbehaving group. Derived server-side from the same state
// window as the group card (state_window_minutes on the group), so incidents
// come and go with the traffic — there is no lifecycle and no human-authored
// copy behind them.
export interface PublicStatusIncident {
  group_id: string;
  group_name: string;
  state: Extract<PublicStatusState, "degraded" | "serious" | "down">;
  success_pct: number | null;
  // Inferred from the bucket grid, precise to one bucket. Null when the outage
  // is older than the whole window.
  started_at: string | null;
}
// What the vendors publish about themselves, mirrored from the OpenAI and
// Anthropic status pages. Kept separate from PublicStatusIncident on purpose:
// incidents above are measured from our own traffic, these are self-reported,
// and a vendor reading "All Systems Operational" while our success rate drops
// is the ordinary case rather than a contradiction to resolve.
export type PublicUpstreamState =
  | "operational"
  | "degraded"
  | "outage"
  | "maintenance"
  // Unreachable page or a snapshot too old to trust. Never means healthy.
  | "unknown";

export interface PublicUpstreamComponent {
  name: string;
  // The vendor's raw string (degraded_performance, partial_outage,
  // major_outage, under_maintenance) — the client owns the wording.
  status: string;
}

export interface PublicUpstreamIncident {
  id: string;
  name: string;
  status: string;
  impact: string;
  started_at: string;
  latest_update: string;
  url: string;
}

export interface PublicUpstreamProvider {
  key: string;
  name: string;
  page_url: string;
  state: PublicUpstreamState;
  // The vendor's own headline. Empty when state is unknown.
  description: string;
  // Only non-operational subsystems ship; healthy ones are omitted so the
  // panel does not carry ~40 permanently green rows.
  components: PublicUpstreamComponent[];
  incidents: PublicUpstreamIncident[];
  // Null when the last fetch failed.
  fetched_at: string | null;
  error?: string;
}

export interface PublicUpstream {
  providers: PublicUpstreamProvider[];
  polled_at: string;
}

export interface PublicStatus {
  updated_at: string;
  window_hours: number;
  groups: PublicStatusGroup[];
  incidents: PublicStatusIncident[];
  // Absent when the operator has the vendor panel switched off.
  upstream?: PublicUpstream | null;
}

// ---- admin observability (性能观测) ----
//
// Mirrors GET /v1/admin/observability/{series,leaderboard,sla}. The series
// endpoints always return a fixed 48-point grid, zero-filled where a bucket
// had no traffic; rate/latency fields are null (not 0) when there was no
// sample to measure, so charts must break the line instead of dipping to 0.

export type ObservabilityDim = "platform" | "provider" | "group" | "account" | "model";
export type ObservabilityWindow = "1h" | "6h" | "24h";
export type ObservabilitySlaWindow = "24h" | "7d" | "30d";

// Shared metric block: a series point carries these plus `time`, a
// leaderboard row carries these plus `id`/`name`.
export interface ObservabilityMetrics {
  requests: number;
  ok: number;
  failures: number;
  // Fraction 0–1 over all requests in the bucket (0 when requests = 0).
  failure_rate: number;
  // Failures attributable to the upstream channel (as opposed to caller
  // errors); provider_fault_rate is null when there was nothing to grade.
  provider_faults: number;
  provider_fault_rate: number | null;
  capacity_faults: number;
  avg_ttft_ms: number | null;
  p50_ttft_ms: number | null;
  p95_ttft_ms: number | null;
  p99_ttft_ms: number | null;
  // How many requests contributed a TTFT sample; weights TTFT aggregation.
  // Not avg_tps's sample — that one is tps_tokens.
  ttft_count: number;
  avg_tps: number | null;
  // Output tokens avg_tps was computed from: its coverage, and the weight
  // for recombining per-bucket rates.
  tps_tokens: number;
  avg_latency_ms: number | null;
  p95_latency_ms: number | null;
  tokens_out: number;
  retries: number;
  anomalies: number;
  time_anomalies: number;
  error_anomalies: number;
}

export interface ObservabilityPoint extends ObservabilityMetrics {
  time: string;
}

// Query params sent to the series endpoint. `window` / `include_synthetic`
// are request-side only: the response echoes window_seconds/bucket_seconds
// and the resolved start/end instead.
export interface ObservabilitySeriesParams {
  dim: ObservabilityDim;
  provider_id?: string;
  group_id?: string;
  account_id?: string;
  model?: string;
  window: ObservabilityWindow;
  include_synthetic: boolean;
}

export interface ObservabilitySeries {
  dim: ObservabilityDim;
  window_seconds: number;
  bucket_seconds: number;
  start: string;
  end: string;
  // Echo of the selected entity — exactly one is present, matching dim
  // (absent for dim=platform).
  provider_id?: string;
  group_id?: string;
  account_id?: string;
  model?: string;
  points: ObservabilityPoint[];
}

// The identity field depends on the leaderboard dim: provider rows carry
// provider_id, group rows group_id, account rows account_id, model rows
// model. There is no `id`.
export interface ObservabilityLeaderboardRow extends ObservabilityMetrics {
  name: string;
  provider_id?: string;
  group_id?: string;
  account_id?: string;
  model?: string;
  // Trailing-minute throughput, independent of the window. TPM counts output
  // tokens only, the same definition as the platform rate.
  rpm: number;
  tpm: number;
}

export interface ObservabilityLeaderboard {
  dim: Exclude<ObservabilityDim, "platform">;
  window_seconds: number;
  start: string;
  end: string;
  rows: ObservabilityLeaderboardRow[];
}

// SLA report: business availability per group over 24h/7d/30d, graded
// server-side against the thresholds echoed in the response — states reuse
// PublicStatusState so the color map stays shared.
//
// Unlike the public status page, the admin buckets ship the evidence
// (counts + pct) alongside the verdict.
export interface ObservabilitySlaBucket {
  time: string;
  state: PublicStatusState;
  availability_pct: number | null;
  good: number;
  bad: number;
  excluded: number;
}

export interface ObservabilitySlaModel {
  // Models are keyed by name string, not id.
  model: string;
  state: PublicStatusState;
  availability_pct: number | null;
  // Request counts behind the percentage. `excluded` are requests the SLA
  // does not judge (synthetic probes, caller-side errors).
  good: number;
  bad: number;
  excluded: number;
  avg_latency_ms: number | null;
  // Reserved for the incidents subsystem; null until that ships.
  open_incident_id: string | null;
  buckets: ObservabilitySlaBucket[];
}

export interface ObservabilitySlaGroup {
  group_id: string;
  name: string;
  state: PublicStatusState;
  availability_pct: number | null;
  good: number;
  bad: number;
  excluded: number;
  avg_latency_ms: number | null;
  open_incident_id: string | null;
  buckets: ObservabilitySlaBucket[];
  models: ObservabilitySlaModel[];
}

export interface ObservabilitySla {
  window_hours: number;
  updated_at: string;
  thresholds: { healthy: number; degraded: number; serious: number };
  groups: ObservabilitySlaGroup[];
}

// ---- policy moderation types ----
// (Backend term "policy_category" is exposed as "group" in the API
// and UI; legacy code-gen names stay for ent stability.)
export type PolicyScope = "input_only" | "input_output" | "input_output_tools";

export interface PolicyGroup {
  id: string;
  name: string;
  slug: string;
  description: string;
  default_action: "block" | "log_only";
  default_severity: "low" | "medium" | "high" | "critical";
  scope: PolicyScope;
  enabled: boolean;
  sort_order: number;
}
export interface PolicyGroupInput {
  name?: string;
  slug?: string;
  description?: string;
  default_action?: "block" | "log_only";
  default_severity?: "low" | "medium" | "high" | "critical";
  scope?: PolicyScope;
  enabled?: boolean;
  sort_order?: number;
}
export interface PolicyKeyword {
  id: string;
  group_id: string;
  pattern: string;
  severity?: "low" | "medium" | "high" | "critical";
  enabled: boolean;
  note: string;
  created_at: string;
}
export interface PolicyKeywordInput {
  group_id?: string;
  pattern?: string;
  severity?: "" | "low" | "medium" | "high" | "critical";
  note?: string;
  enabled?: boolean;
}
export interface PolicyImportResult {
  pattern: string;
  created: boolean;
  error?: string;
}

export interface UserIncident {
  id: string;
  created_at: string;
  user_id?: string;
  api_key_id?: string;
  kind: string;
  severity: "info" | "low" | "medium" | "high" | "critical";
  action?: string;
  model?: string;
  endpoint?: string;
  request_id?: string;
  ip?: string;
  message: string;
}

export type SystemIncidentSeverity = "degraded" | "serious" | "down";
export type SystemIncidentResolution = "" | "auto_recovered" | "stale_closed" | "manual";
export type SystemIncidentStatusFilter = "open" | "resolved" | "all";

export interface SystemIncident {
  id: string;
  group_id: string;
  group_name: string;
  model: string;
  severity: SystemIncidentSeverity;
  worst_severity: SystemIncidentSeverity;
  started_at: string;
  detected_at: string;
  resolved_at: string | null;
  duration_seconds: number;
  open_availability_pct: number | null;
  worst_availability_pct: number | null;
  last_availability_pct: number | null;
  sample_count: number;
  resolution: SystemIncidentResolution;
  note: string;
  created_at: string;
  updated_at: string;
}

export interface SystemIncidentListParams {
  status?: SystemIncidentStatusFilter;
  group_id?: string;
  start?: string;
  end?: string;
  limit?: number;
  offset?: number;
}

export interface SystemIncidentListResponse {
  incidents: SystemIncident[];
  total: number;
  limit: number;
  offset: number;
}

export interface IncidentBucket {
  key: string;
  count: number;
}
export interface IncidentDaily {
  date: string; // YYYY-MM-DD
  total: number;
  blocked: number;
}
export interface IncidentStats {
  total: number;
  blocked: number;
  false_positive: number;
  reviewed: number;
  by_kind: IncidentBucket[];
  by_severity: IncidentBucket[];
  by_action: IncidentBucket[];
  by_review_label: IncidentBucket[];
  daily: IncidentDaily[];
}

export interface AdminAuditLog {
  id: string;
  created_at: string;
  actor_user_id: string;
  action: string;
  target?: string;
  payload?: Record<string, unknown>;
  ip?: string;
  ua?: string;
}

export type IPPolicyKind = "trusted_proxy" | "allow" | "block";

export interface IPPolicy {
  id: string;
  cidr: string;
  kind: IPPolicyKind;
  note: string;
  expires_at?: string;
  created_at: string;
  created_by?: string;
}

export interface Overview {
  users: { total: number; active: number; banned: number; dau: number; mau: number };
  today: { requests: number; cost_usd: string; tokens: number; errors: number; avg_latency_ms: number };
}

// ---- endpoints ----

export interface AccessTokenView {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  expires_at?: string | null;
  last_used_at?: string | null;
}

export interface DeviceView {
  id: string;
  client_id: string;
  name: string;
  scopes: string[];
  created_at: string;
  last_used_at?: string;
  last_ip: string;
  last_user_agent: string;
}

export interface RedeemCode {
  id: string;
  code: string;
  kind: "balance" | "plan";
  amount?: string;
  currency: string;
  plan_id?: string;
  max_uses: number;
  used_count: number;
  expires_at?: string;
  status: "active" | "exhausted" | "revoked" | "expired";
  batch_id: string;
  note?: string;
  created_at: string;
}

export interface StatsBucket {
  time: string;
  requests: number;
  tokens_in: number;
  tokens_out: number;
  cached_tokens_in: number;
  cache_creation_tokens_in: number;
  audio_tokens_in?: number;
  audio_tokens_out?: number;
  cost_final_sum: string;
  errors: number;
}

export interface StatsBreakdownRow {
  key: string;
  label?: string;
  requests: number;
  errors?: number;
  tokens_in: number;
  tokens_out: number;
  cost_final_sum: string;
}

export interface RoutingCooldown {
  active: boolean;
  reason?: string;
  ttl_seconds?: number;
}

// Attempt-level per-channel stats, read from provider_chain rather than the
// request's final provider — a channel that fails every attempt and is
// rescued by failover has almost no requests but plenty of attempts.
export interface ProviderAttemptRow {
  provider_id: string;
  name: string;
  attempts: number;
  ok: number;
  provider_faults: number;
  provider_fault_rate: number | null;
  by_class: Record<string, number>;
}

export interface ProviderAttemptsResponse {
  window: ObservabilityWindow;
  window_seconds: number;
  start: string;
  end: string;
  rows: ProviderAttemptRow[];
}

// ---- load map (topology canvas) ----

export type LoadMapWindow = "5m" | "15m" | "1h";

// One (group, account) edge's traffic inside the window. Node values are the
// row sums on each side — the canvas draws one edge per row.
export interface LoadMapEdge {
  group_id: string;
  account_id: string;
  requests: number;
  failures: number;
  provider_faults: number;
  avg_latency_ms: number | null;
  tokens_out: number;
}

export interface LoadMapResponse {
  window_seconds: number;
  start: string;
  end: string;
  edges: LoadMapEdge[];
}

// ---- live error monitor (/observability/errors) ----

export type ObservabilityErrorWindow = "15m" | "1h" | "6h" | "24h";

// One error_class inside the window: share of all errors, first/last sighting,
// and the most frequent sample message / account / model so a row reads as
// "what does this failure look like" without shipping raw rows.
export interface ObservabilityErrorBreakdown {
  error_class: string;
  count: number;
  share: number;
  first_seen: string;
  last_seen: string;
  sample: string;
  top_account: string;
  top_model: string;
}

export interface ObservabilityErrorBucket {
  t: string;
  requests: number;
  errors: number;
}

export interface ObservabilityErrorRecent {
  at: string;
  error_class: string;
  http_status: number;
  model: string;
  account: string;
  group: string;
  message: string;
}

export interface ObservabilityErrorsResponse {
  window_seconds: number;
  bucket_seconds: number;
  start: string;
  end: string;
  total_requests: number;
  total_errors: number;
  // Caller-side failures left out of total_errors (bad request parameters,
  // client cancels, content-safety blocks, quota/balance/key rejections).
  excluded_errors: number;
  breakdown: ObservabilityErrorBreakdown[];
  buckets: ObservabilityErrorBucket[];
  recent: ObservabilityErrorRecent[];
}

export interface RoutingBoardProvider {
  id: string;
  name: string;
  kind: string;
  enabled: boolean;
  health: string;
  last_health_at?: string;
  cooldown: RoutingCooldown;
}

export interface RoutingPreviewProvider {
  id: string;
  name: string;
  kind: string;
  health: string;
  priority: number;
  weight: number;
  effective_weight: number;
  upstream_model: string;
  cooldown: RoutingCooldown;
  eligible: boolean;
}

// Why auto-route did not pick a candidate group. Empty on the chosen one.
export type RoutingSkipReason =
  | "no_providers"
  | "all_providers_cooling"
  | "billing_source_mismatch";

export interface RoutingPreviewGroup {
  id: string;
  name: string;
  billing_source: string;
  rate_multiplier: number;
  chosen: boolean;
  skip_reason?: RoutingSkipReason;
  providers: RoutingPreviewProvider[];
}

export interface TotpStatus {
  enabled: boolean;
  confirmed_at?: string | null;
  recovery_codes_left: number;
  setup_required: boolean;
}

// Self-service refund: the user's paid-for balance goes back through the
// order's payment gateway, newest order first; money already spent is never
// refunded. What no gateway can return (other channels, manual credits) is
// handled by staff through a ticket.
// How much of one order can be refunded now. Quota subscriptions carry no
// balance (balance_bearing=false) and use the refund policy.
export interface OrderRefundable {
  order_id: string;
  balance_bearing: boolean;
  cash: string;
  refundable: string;
}

export interface SelfRefundOrder {
  order_id: string;
  order_no: string;
  paid_at?: string;
  cash: string;
  credit: string;
}

// An active Coding Plan, refunded for the time left. auto=false means its
// channel has no refund gateway and staff handle it.
export interface SelfRefundPlanItem {
  order_id: string;
  order_no: string;
  plan_name: string;
  channel: string;
  cash: string;
  remaining_days: number;
  total_days: number;
  auto: boolean;
}

export interface SelfRefundPlan {
  enabled: boolean;
  refundable: string;
  orders: SelfRefundOrder[] | null;
  cash: string;
  unsupported: string;
  plans: SelfRefundPlanItem[] | null;
}

export interface SelfRefundResult {
  refunded: SelfRefundOrder[] | null;
  refunded_plans: SelfRefundPlanItem[] | null;
  failed?: string[] | null;
  unsupported: string;
}

export const meApi = {
  selfRefundPlan: () => http.get<SelfRefundPlan>("/v1/me/self-refund").then((r) => r.data),
  runSelfRefund: () => http.post<SelfRefundResult>("/v1/me/self-refund").then((r) => r.data),
  orderRefundable: (orderId: string) => http.get<OrderRefundable>(`/v1/me/orders/${orderId}/refundable`).then((r) => r.data),
  me: () => http.get<MeResponse>("/v1/me").then((r) => r.data),
  totpStatus: () => http.get<TotpStatus>("/v1/me/totp").then((r) => r.data),
  totpSetup: () =>
    http.post<{ secret: string; otpauth_url: string }>("/v1/me/totp/setup").then((r) => r.data),
  totpConfirm: (code: string) =>
    http.post<{ ok: boolean; recovery_codes: string[] }>("/v1/me/totp/confirm", { code }).then((r) => r.data),
  totpDisable: (b: { password: string; code: string }) =>
    http.post<{ ok: boolean }>("/v1/me/totp/disable", b).then((r) => r.data),
  invite: (params: { limit?: number; offset?: number } = {}) =>
    http.get<InviteResponse>("/v1/me/invite", { params }).then((r) => r.data),
  couponQuote: (code: string) =>
    http.get<CouponQuote>("/v1/me/coupon/quote", { params: { code } }).then((r) => r.data),
  updateMe: (b: { nickname?: string }) =>
    http.patch<MeResponse>("/v1/me", b).then((r) => r.data),
  changePassword: (b: { old_password: string; new_password: string }) =>
    http.post<{ ok: boolean; message: string }>("/v1/me/change-password", b).then((r) => r.data),
  changeEmail: (b: { new_email: string }) =>
    http.post<{ ok: boolean; message: string }>("/v1/me/change-email", b).then((r) => r.data),
  sendEmailVerification: () =>
    http.post<{ ok: boolean; email: string; cooldown_seconds: number }>("/v1/me/send-email-verification").then((r) => r.data),
  usage: () => http.get<PlanUsage>("/v1/me/usage").then((r) => r.data),
  useBankedReset: (id: string) => http.post(`/v1/me/banked-resets/${id}/use`),
  apiKeys: () => http.get<{ api_keys: ApiKeyView[] }>("/v1/me/apikeys").then((r) => r.data.api_keys),
  createApiKey: (body: {
    name: string;
    expires_at?: string | null;
    allowed_group_ids?: string[];
    ip_whitelist?: string[];
    auto_route?: boolean;
    auto_route_mode?: "price" | "stable" | "fast";
    auto_route_max_rate?: number;
    model_pins?: { model: string; group_id: string }[];
    billing_scope?: "personal" | "organization";
    organization_id?: string | null;
  } & ApiKeyCostLimits) =>
    http.post<CreatedApiKey>("/v1/me/apikeys", body).then((r) => r.data),
  updateApiKey: (
    id: string,
    body: {
      name?: string;
      expires_at?: string | null;
      allowed_group_ids?: string[];
      ip_whitelist?: string[];
      auto_route?: boolean;
      auto_route_mode?: "price" | "stable" | "fast";
      auto_route_max_rate?: number;
      model_pins?: { model: string; group_id: string }[];
      billing_scope?: "personal" | "organization";
      organization_id?: string | null;
    } & ApiKeyCostLimits,
  ) =>
    http.patch<{ api_key: ApiKeyView }>(`/v1/me/apikeys/${id}`, body).then((r) => r.data.api_key),
  revokeApiKey: (id: string) => http.delete(`/v1/me/apikeys/${id}`),
  rotateApiKey: (id: string) =>
    http.post<CreatedApiKey>(`/v1/me/apikeys/${id}/rotate`).then((r) => r.data),
  ensureLabKey: () =>
    http.post<{ api_key: ApiKeyView; secret?: string }>("/v1/me/lab-key").then((r) => r.data),
  revealApiKey: (id: string) =>
    http.get<{ secret: string }>(`/v1/me/apikeys/${id}/reveal`).then((r) => r.data.secret),
  devices: () => http.get<{ devices: DeviceView[] }>("/v1/me/devices").then((r) => r.data.devices),
  revokeDevice: (id: string) => http.delete(`/v1/me/devices/${id}`),
  accessTokens: () => http.get<{ tokens: AccessTokenView[] }>("/v1/me/access-tokens").then((r) => r.data.tokens),
  createAccessToken: (b: { name: string; expires_in_days: number }) =>
    http.post<AccessTokenView & { token: string }>("/v1/me/access-tokens", b).then((r) => r.data),
  revokeAccessToken: (id: string) => http.delete(`/v1/me/access-tokens/${id}`),
  availableGroups: () =>
    http.get<{ groups: ModelGroup[] }>("/v1/me/available-groups").then((r) => r.data.groups),
  organizations: () =>
    http.get<{ organizations: Organization[] }>("/v1/me/organizations").then((r) => r.data.organizations),
  organizationDetail: (id: string) =>
    http.get<{ organization: Organization }>(`/v1/me/organizations/${id}`).then((r) => r.data.organization),
  updateOrganization: (id: string, b: { name?: string; monthly_spend_limit?: string }) =>
    http.patch<{ organization: Organization }>(`/v1/me/organizations/${id}`, b).then((r) => r.data.organization),
  topupOrganization: (id: string, b: { amount: string; note?: string }) =>
    http.post<{ balance: string }>(`/v1/me/organizations/${id}/topup`, b).then((r) => r.data),
  upsertOrganizationMember: (
    id: string,
    userId: string,
    b: { role?: "owner" | "admin" | "member"; monthly_spend_limit?: string; total_spend_limit?: string },
  ) => http.put(`/v1/me/organizations/${id}/members/${userId}`, b),
  removeOrganizationMember: (id: string, userId: string) =>
    http.delete(`/v1/me/organizations/${id}/members/${userId}`),
  orgOverview: (id: string) =>
    http.get<OrgOverview>(`/v1/me/organizations/${id}/overview`).then((r) => r.data),
  orgBillingPaged: (id: string, params: { q?: string; kind?: string; from?: string; to?: string; limit?: number; offset?: number } = {}) =>
    http
      .get<{ items: OrgBillingEntry[]; total: number; limit: number; offset: number }>(
        `/v1/me/organizations/${id}/billing`,
        { params },
      )
      .then((r) => ({ items: r.data.items, total: r.data.total, limit: r.data.limit, offset: r.data.offset })),
  orgBillingSummary: (id: string, params: { from?: string; to?: string } = {}) =>
    http.get<BillingSummary>(`/v1/me/organizations/${id}/billing/summary`, { params }).then((r) => r.data),
  availablePlans: () => http.get<{ plans: Plan[] }>("/v1/me/plans").then((r) => r.data.plans),
  redeem: (code: string) =>
    http.post<{ kind: string; amount?: string; currency?: string; balance_after?: string; plan_name?: string; expire_at?: string }>(
      "/v1/me/redeem", { code }).then((r) => r.data),
  myStats: (params: { start?: string; end?: string; bucket?: string; model?: string }) =>
    http.get<{ buckets: StatsBucket[]; start: string; end: string; bucket: string }>(
      "/v1/me/stats/usage", { params }).then((r) => r.data),
  // Live trailing-60s throughput for the calling user. Uncached server-side;
  // the point of the widget is that it tracks the last minute.
  myRate: () =>
    http.get<{ rpm: number; tpm: number; rpm_limit: number; tpm_limit: number }>(
      "/v1/me/stats/rate").then((r) => r.data),
  myLifetimeStats: () =>
    http.get<{ total_requests: number; total_cost: string }>(
      "/v1/me/stats/lifetime").then((r) => r.data),
  myStatsByModel: (params: { start?: string; end?: string; bucket?: string }) =>
    http.get<{ rows: Array<{ time: string; model: string; requests: number; tokens_in: number; tokens_out: number; cost_final_sum: string }>; start: string; end: string; bucket: string }>(
      "/v1/me/stats/usage-by-model", { params }).then((r) => r.data),
  // Ranked, not bucketed: the API-key dimension is only ever drawn as a
  // distribution, so the server collapses the window into one row per key.
  // api_key_id is "" for calls made without a key (OAuth device sessions);
  // api_key_name is "" for a key that was never named.
  myStatsByKey: (params: { start?: string; end?: string; bucket?: string }) =>
    http.get<{ rows: Array<{ api_key_id: string; api_key_name: string; revoked: boolean; requests: number; tokens_in: number; tokens_out: number; cost_final_sum: string }>; start: string; end: string }>(
      "/v1/me/stats/usage-by-key", { params }).then((r) => r.data),
  myUsageLogs: (params: UsageLogFilterParams = {}) =>
    http.get<{ logs: any[]; total: number; limit: number; offset: number }>(
      "/v1/me/usage-logs", { params }).then((r) => r.data),

  // UsageLogFilterParams covers the full filter surface for both
  // /v1/me/usage-logs and /v1/admin/usage-logs. The admin endpoint
  // additionally honors `user_id` (UUID) and `user` (email/nickname
  // fuzzy match); user routes ignore those.
  myUsageLogsPaged: (params: UsageLogFilterParams = {}) =>
    http
      .get<{ logs: any[]; total: number; limit: number; offset: number }>(
        "/v1/me/usage-logs",
        { params },
      )
      .then((r) => ({ items: r.data.logs, total: r.data.total, limit: r.data.limit, offset: r.data.offset })),
  myBillingPaged: (params: { q?: string; kind?: string; from?: string; to?: string; limit?: number; offset?: number } = {}) =>
    http
      .get<{ items: BillingEntry[]; total: number; limit: number; offset: number }>(
        "/v1/me/billing",
        { params },
      )
      .then((r) => ({ items: r.data.items, total: r.data.total, limit: r.data.limit, offset: r.data.offset })),
  myBillingSummary: (params: { from?: string; to?: string } = {}) =>
    http.get<BillingSummary>("/v1/me/billing/summary", { params }).then((r) => r.data),

  // ---- self-service tickets ----
  myTickets: (params: { status?: TicketStatus; category?: TicketCategory; q?: string; limit?: number; offset?: number } = {}) =>
    http
      .get<{ tickets: TicketRow[]; total: number; limit: number; offset: number }>(
        "/v1/me/tickets",
        { params },
      )
      .then((r) => r.data),
  createMyTicket: (b: { title: string; body: string; category?: TicketCategory; priority?: TicketPriority; order_id?: string; attachments?: TicketAttachment[] }) =>
    http.post<TicketRow>("/v1/me/tickets", b).then((r) => r.data),
  getMyTicket: (id: string) =>
    http.get<TicketDetail>(`/v1/me/tickets/${id}`).then((r) => r.data),
  replyMyTicket: (id: string, body: string, attachments: TicketAttachment[] = []) =>
    http.post<TicketMessage>(`/v1/me/tickets/${id}/reply`, { body, attachments }).then((r) => r.data),
  closeMyTicket: (id: string) =>
    http.post<{ id: string; status: string }>(`/v1/me/tickets/${id}/close`).then((r) => r.data),
  reopenMyTicket: (id: string) =>
    http.post<{ id: string; status: string }>(`/v1/me/tickets/${id}/reopen`).then((r) => r.data),
  myOrders: (params: { status?: string; kind?: string; limit?: number; offset?: number } = {}) =>
    http.get<{ orders: SiteOrder[]; total: number; limit: number; offset: number }>("/v1/me/orders", { params }).then((r) => r.data),
  createMyOrder: (b: { kind: "balance" | "plan"; plan_id?: string; amount?: string; currency?: string; note?: string; coupon_code?: string }) =>
    http.post<{ order: SiteOrder }>("/v1/me/orders", b).then((r) => r.data.order),
  // client: "wap" 走支付宝手机网站支付（会唤起 App），缺省是电脑网站支付（扫码收银台）。
  createAlipayPagePay: (id: string, b: { return_url?: string; client?: "pc" | "wap"; quit_url?: string } = {}) =>
    http.post<{ order: SiteOrder; code_url: string; pay_url?: string; pay_type?: string }>(`/v1/me/orders/${id}/alipay/page-pay`, b).then((r) => r.data),
  syncMyOrder: (id: string) =>
    http.post<{ order: SiteOrder }>(`/v1/me/orders/${id}/sync`).then((r) => r.data.order),
  createMyRefund: (id: string, b: { reason?: string; note?: string }) =>
    http.post<{ ticket: TicketRow }>(`/v1/me/orders/${id}/refunds`, b).then((r) => r.data.ticket),
};

export const csApi = {
  orderRefundable: (orderId: string) => http.get<OrderRefundable>(`/v1/cs/orders/${orderId}/refundable`).then((r) => r.data),
  tickets: (params: { status?: TicketStatus; category?: TicketCategory; assignee?: "me" | "unassigned" | "any"; q?: string; user_id?: string; limit?: number; offset?: number } = {}) =>
    http
      .get<{ tickets: TicketRow[]; total: number; limit: number; offset: number }>("/v1/cs/tickets", { params })
      .then((r) => r.data),
  ticketCounts: () =>
    http.get<{ counts: Record<TicketStatus, number>; unread: number }>("/v1/cs/tickets/counts").then((r) => r.data),
  ticketAssignees: () =>
    http.get<{ assignees: TicketAssignee[] }>("/v1/cs/tickets/assignees").then((r) => r.data.assignees),
  getTicket: (id: string) => http.get<TicketDetail>(`/v1/cs/tickets/${id}`).then((r) => r.data),
  replyTicket: (id: string, body: string, internalNote = false, attachments: TicketAttachment[] = []) =>
    http
      .post<TicketMessage>(`/v1/cs/tickets/${id}/reply`, { body, internal_note: internalNote, attachments })
      .then((r) => r.data),
  assignTicket: (id: string, assignee: "" | "me" | string) =>
    http.post<{ id: string; status: string; assignee_user_id?: string }>(`/v1/cs/tickets/${id}/assign`, { assignee_user_id: assignee }).then((r) => r.data),
  setTicketStatus: (id: string, status: TicketStatus) =>
    http.post<{ id: string; status: string }>(`/v1/cs/tickets/${id}/status`, { status }).then((r) => r.data),
  createTicketRefund: (id: string, b: { amount?: string; note?: string } = {}) =>
    http.post<{ refund: SiteRefund }>(`/v1/cs/tickets/${id}/refunds`, b).then((r) => r.data.refund),
  refunds: (params: { status?: SiteRefund["status"]; limit?: number; offset?: number } = {}) =>
    http
      .get<{ refunds: SiteRefund[]; total: number; limit: number; offset: number }>("/v1/cs/refunds", { params })
      .then((r) => r.data),
  approveRefund: (id: string, note?: string) =>
    http.post<{ refund: SiteRefund }>(`/v1/cs/refunds/${id}/approve`, { note }).then((r) => r.data.refund),
  rejectRefund: (id: string, note?: string) =>
    http.post<{ refund: SiteRefund }>(`/v1/cs/refunds/${id}/reject`, { note }).then((r) => r.data.refund),

  users: (params: { q?: string; limit?: number; offset?: number } = {}) =>
    http
      .get<{ users: any[]; total: number; limit: number; offset: number }>("/v1/cs/users", { params })
      .then((r) => r.data),
  userDetail: (id: string) =>
    http.get<{ user: any }>(`/v1/cs/users/${id}`).then((r) => r.data.user),
  adjustBalance: (id: string, b: { amount: string; note?: string; kind?: "topup" | "refund" | "deduct" | "adjust" }) =>
    http.post<{ balance: string }>(`/v1/cs/users/${id}/balance/adjust`, b).then((r) => r.data),
  resetUserPassword: (id: string, newPassword: string) =>
    http.post<{ ok: boolean; message: string }>(`/v1/cs/users/${id}/reset-password`, { new_password: newPassword }).then((r) => r.data),
  cancelUserPlan: (uid: string, upId: string) =>
    http.post<{ user_plan_id: string; status: string }>(`/v1/cs/users/${uid}/plans/${upId}/cancel`).then((r) => r.data),
  extendUserPlan: (uid: string, upId: string, days: number) =>
    http.post<{ user_plan_id: string; expire_at: string }>(`/v1/cs/users/${uid}/plans/${upId}/extend`, { days }).then((r) => r.data),
};

export const agentApi = {
  profile: () => http.get<AgentProfile>("/v1/agent/profile").then((r) => r.data),
  dashboard: () => http.get<AgentDashboard>("/v1/agent/dashboard").then((r) => r.data),
  codes: () => http.get<{ codes: RedeemCode[] }>("/v1/agent/redeem-codes").then((r) => r.data.codes),
  generate: (b: { kind: "balance" | "plan"; amount?: string; plan_id?: string; count?: number; expires_at?: string; note?: string }) =>
    http.post<{ codes: RedeemCode[]; count: number }>("/v1/agent/redeem-codes", b).then((r) => r.data),
  sales: (params: { kind?: AgentEntryKind; status?: "pending" | "settled"; limit?: number; offset?: number } = {}) =>
    http
      .get<{ entries: AgentEntry[]; total: number }>("/v1/agent/sales", { params })
      .then((r) => r.data),
  settlements: (params: { limit?: number; offset?: number } = {}) =>
    http.get<{ settlements: AgentSettlement[]; total: number }>("/v1/agent/settlements", { params }).then((r) => r.data),
  refunds: (params: { limit?: number; offset?: number } = {}) =>
    http.get<{ refunds: AgentRefund[]; total: number }>("/v1/agent/refunds", { params }).then((r) => r.data),
  previewRefund: (code: string) =>
    http.get<RefundPreview>("/v1/agent/refund-preview", { params: { code } }).then((r) => r.data),
  registerRefund: (b: { redeem_record_id: string; reason?: string }) =>
    http
      .post<{
        refund_id: string;
        kind: string;
        refund_amount: string;
        platform_deduct: string;
        user_plan_removed: boolean;
      }>("/v1/agent/refund", b)
      .then((r) => r.data),
  coupon: () => http.get<AgentCoupon>("/v1/agent/coupon").then((r) => r.data),
  createCoupon: (couponCode?: string) =>
    http.post<{ coupon_code: string }>("/v1/agent/coupon", { coupon_code: couponCode || undefined }).then((r) => r.data),
  setCouponDiscount: (discountPct: string) =>
    http.patch<{ discount_pct: string }>("/v1/agent/coupon", { discount_pct: discountPct }).then((r) => r.data),
  withdraw: (amount: string) =>
    http.post<{ amount: string; balance_after: string }>("/v1/agent/withdraw", { amount }).then((r) => r.data),
};

export interface BillingEntry {
  id: string;
  created_at: string;
  kind: "topup" | "refund" | "deduct" | "adjust" | "agent_commission";
  amount: string;
  balance_after: string;
  currency: string;
  note: string;
  related_usage_log_id?: string;
  hold_amount?: string;
}

// Helper: turns a paged list endpoint into the shape consumed by
// usePagedQuery. Backend response is expected to be
// { <itemsKey>: T[], total: number, limit: number, offset: number }.
function pagedAdaptor<T>(itemsKey: string, url: string) {
  return (params: Record<string, unknown> = {}) =>
    http
      .get<Record<string, unknown>>(url, { params })
      .then((r) => ({
        items: (r.data[itemsKey] as T[]) ?? [],
        total: (r.data.total as number) ?? 0,
        limit: (r.data.limit as number) ?? 0,
        offset: (r.data.offset as number) ?? 0,
      }));
}

export interface AdminUserPlan {
  user_plan_id: string; plan_id: string; name: string;
  type: "quota_subscription" | "pay_as_you_go";
  status: "active" | "expired" | "canceled";
  current: boolean; start_at: string; expire_at: string;
  quota_5h: string; quota_weekly: string; quota_monthly: string;
  used_5h?: string; used_weekly?: string; used_monthly: string;
  reset_5h_at?: string; reset_weekly_at?: string; reset_monthly_at?: string;
  monthly_spend_limit: string; requests: number; total_cost: string;
}

export interface AdminResettableUserPlan {
  user_plan_id: string; user_id: string; email: string; username: string;
  plan_id: string; plan_name: string; start_at: string; expire_at: string;
  quota_5h: string; quota_weekly: string; quota_monthly: string;
  used_5h: string; used_weekly: string; used_monthly: string;
  banked_resets: BankedReset[];
}

export interface PlanResetFilter { q?: string; plan_id?: string; expire_after?: string; expire_before?: string }

export interface PlanResetJob {
  id: string; action: "clear" | "restart" | "grant"; window?: string;
  status: "running" | "done"; total: number; done: number; failed: number; errors: string[];
  created_at: string; updated_at: string;
}

export const adminApi = {
  resettableUserPlansPaged: pagedAdaptor<AdminResettableUserPlan>("user_plans", "/v1/admin/user-plans"),
  startPlanReset: (b: { action: PlanResetJob["action"]; window: string; ids?: string[]; filter?: PlanResetFilter }) =>
    http.post<{ job: PlanResetJob }>("/v1/admin/user-plans/reset", b).then((r) => r.data.job),
  planResetJobs: () => http.get<{ jobs: PlanResetJob[] }>("/v1/admin/user-plans/reset-jobs").then((r) => r.data.jobs),
  userPlans: (uid: string) => http.get<{ plans: AdminUserPlan[] }>(`/v1/admin/users/${uid}/plans`).then((r) => r.data.plans),
  // Step-up two-factor is handled centrally: the axios interceptor catches
  // totp_stepup_required, collects a code, and replays the request. Call
  // sites do not pass X-TOTP-Code.
  grantUserPlan: (uid: string, body: { plan_id: string; expire_at?: string; request_id: string }) => http.post(`/v1/admin/users/${uid}/plans`, body),
  updateUserPlan: (uid: string, id: string, body: { plan_id: string; expire_at: string }) => http.patch(`/v1/admin/users/${uid}/plans/${id}`, body),
  deleteUserPlan: (uid: string, id: string) => http.delete(`/v1/admin/users/${uid}/plans/${id}`),
  resetUserPlanQuota: (uid: string, id: string, window: string) => http.post(`/v1/admin/users/${uid}/plans/${id}/reset-quota`, { window }),

  overview: () => http.get<Overview>("/v1/admin/stats/overview").then((r) => r.data),

  // The non-paged variants (providers/models/...) now ask the server
  // for a generous limit so callers that still want "the full list"
  // (e.g. dropdowns) keep working. New DataTable-driven pages use the
  // *Paged variants below.
  //
  // /v1/admin/providers is read-only this release (writes answer 410 Gone).
  // No page lists it any more — the accounts page owns the pool UI — but the
  // finance and observability views still resolve provider rows by id.
  providers: () =>
    http.get<{ providers: Provider[] }>("/v1/admin/providers", { params: { limit: 1000 } })
      .then((r) => r.data.providers),

  // ---- provider catalog (templates) ----
  providerCatalog: (params: { login_kind?: string } = {}) =>
    http.get<ProviderCatalog>("/v1/admin/provider-catalog", { params }).then((r) => r.data),
  providerCatalogEntry: (id: string) =>
    http.get<ProviderCatalogDetail>(`/v1/admin/provider-catalog/${id}`).then((r) => r.data),

  // ---- provider definitions (per-vendor payload: endpoint, protocols,
  // models). A DB row overrides the shipped definition with the same id;
  // deleting the row falls back to the shipped copy.
  providerDefinitions: () =>
    http.get<{ definitions: ProviderDefinition[] }>("/v1/admin/provider-definitions")
      .then((r) => r.data.definitions),
  providerDefinition: (id: string) =>
    http.get<{ definition: ProviderDefinition }>(`/v1/admin/provider-definitions/${id}`)
      .then((r) => r.data.definition),
  // PATCH upserts: it creates the row when only the shipped copy exists.
  upsertProviderDefinition: (id: string, b: ProviderDefinitionInput) =>
    http.patch<{ definition: ProviderDefinition }>(`/v1/admin/provider-definitions/${id}`, b)
      .then((r) => r.data.definition),
  deleteProviderDefinition: (id: string) =>
    http.delete(`/v1/admin/provider-definitions/${id}`),
  seedProviderDefinitions: () =>
    http.post<{ inserted: number; skipped: number }>("/v1/admin/provider-definitions/seed")
      .then((r) => r.data),

  // ---- accounts (the flat pool; replaces the provider → account model) ----
  // The list endpoint returns {accounts, limit, offset} with no total, so the
  // paged adapter reports items on the page and lets the caller drive "next".
  accounts: (params: AccountListParams = {}) =>
    http.get<{ accounts: Account[]; limit: number; offset: number }>("/v1/admin/accounts", { params })
      .then((r) => r.data.accounts),
  accountsPage: (params: AccountListParams = {}) =>
    http.get<{ accounts: Account[]; limit: number; offset: number }>("/v1/admin/accounts", { params })
      .then((r) => ({ accounts: r.data.accounts ?? [], limit: r.data.limit ?? 0, offset: r.data.offset ?? 0 })),
  createAccount: (b: AccountInput) => http.post<Account>("/v1/admin/accounts", b).then((r) => r.data),
  account: (id: string) => http.get<Account>(`/v1/admin/accounts/${id}`).then((r) => r.data),
  updateAccount: (id: string, b: AccountUpdate) =>
    http.patch<Account>(`/v1/admin/accounts/${id}`, b).then((r) => r.data),
  deleteAccount: (id: string) => http.delete(`/v1/admin/accounts/${id}`),

  // per-account model bindings
  accountModels: (id: string) =>
    http.get<{ models: AccountModel[] }>(`/v1/admin/accounts/${id}/models`).then((r) => r.data.models),
  syncAccountModels: (id: string) =>
    http.post<AccountModelsSyncResult>(`/v1/admin/accounts/${id}/models/sync`).then((r) => r.data),
  importAccountModels: (id: string, models: ImportAccountModelItem[]) =>
    http.post<AccountModelsSyncResult>(`/v1/admin/accounts/${id}/models/import`, { models }).then((r) => r.data),
  catalogProviders: () =>
    http.get<{ providers: RemoteCatalogProvider[] }>("/v1/admin/model-catalog/providers").then((r) => r.data.providers),
  catalogModels: (providerId: string) =>
    http.get<{ models: RemoteCatalogModel[] }>(`/v1/admin/model-catalog/providers/${providerId}/models`).then((r) => r.data.models),
  catalogAllModels: () =>
    http.get<{ models: RemoteCatalogModel[] }>("/v1/admin/model-catalog/models").then((r) => r.data.models),
  addAccountModel: (id: string, b: AccountModelInput) =>
    http.post<{ model: AccountModel }>(`/v1/admin/accounts/${id}/models`, b).then((r) => r.data.model),
  updateAccountModel: (accountId: string, modelId: string, b: AccountModelInput) =>
    http.patch<{ model: AccountModel }>(`/v1/admin/accounts/${accountId}/models/${modelId}`, b).then((r) => r.data.model),
  removeAccountModel: (accountId: string, modelId: string) =>
    http.delete(`/v1/admin/accounts/${accountId}/models/${modelId}`),
  accountUsage: (id: string, params: { from?: string; to?: string } = {}) =>
    http.get<{ from: string; to: string; rows: AccountModelUsageRow[] }>(`/v1/admin/accounts/${id}/usage`, { params }).then((r) => r.data),

  // health / quota actions
  // recoverAccount clears every runtime health signal: cooldowns, model
  // cooldowns, failure counters and an error disable.
  recoverAccount: (id: string) =>
    http.post<Account>(`/v1/admin/accounts/${id}/recover`).then((r) => r.data),
  // setAccountSchedulable parks/resumes an account without changing its status.
  setAccountSchedulable: (id: string, schedulable: boolean) =>
    http.put<Account>(`/v1/admin/accounts/${id}/schedulable`, { schedulable }).then((r) => r.data),
  // setAccountCooldown parks an account by hand; it lifts on its own at TTL.
  setAccountCooldown: (id: string, b: { reason?: string; seconds: number }) =>
    http.post<Account>(`/v1/admin/accounts/${id}/cooldown`, b).then((r) => r.data),
  clearAccountCooldown: (id: string) =>
    http.delete<Account>(`/v1/admin/accounts/${id}/cooldown`).then((r) => r.data),
  clearAccountModelCooldowns: (id: string) =>
    http.delete<Account>(`/v1/admin/accounts/${id}/model-cooldowns`).then((r) => r.data),
  refreshAccountQuota: (id: string) =>
    http.post<Account>(`/v1/admin/accounts/${id}/quota/refresh`).then((r) => r.data),
  setAccountQuotaPolicy: (id: string, b: { pause_at_percent: number; windows?: string[] }) =>
    http.put<Account>(`/v1/admin/accounts/${id}/quota/policy`, b).then((r) => r.data),
  // testAccount fires one targeted probe — this account, this protocol, this
  // model — straight at the account's own upstream. The verdict is returned;
  // no health state, cooldown or billing is touched.
  testAccount: (id: string, b: { protocol: AccountProtocol; model: string }) =>
    http.post<AccountTestResult>(`/v1/admin/accounts/${id}/test`, b).then((r) => r.data),

  // ---- account pools (管理分组: organizational + health policy) ----
  accountPools: () =>
    http.get<{ pools: AccountPool[] }>("/v1/admin/account-pools").then((r) => r.data.pools),
  createAccountPool: (b: { name: string; note?: string; sort_order?: number; pool_mode?: string; enabled?: boolean }) =>
    http.post<{ pool: AccountPool }>("/v1/admin/account-pools", b).then((r) => r.data.pool),
  updateAccountPool: (id: string, b: { name?: string; note?: string; sort_order?: number; pool_mode?: string; enabled?: boolean }) =>
    http.patch<{ pool: AccountPool }>(`/v1/admin/account-pools/${id}`, b).then((r) => r.data.pool),
  deleteAccountPool: (id: string) => http.delete(`/v1/admin/account-pools/${id}`),
  accountPoolMembers: (id: string) =>
    http.get<{ accounts: { account_id: string; name?: string; status?: string; health?: string; schedulable?: boolean; base_url?: string }[] }>(
      `/v1/admin/account-pools/${id}/accounts`,
    ).then((r) => r.data.accounts),
  // The backend adds members one at a time (idempotent); fan out client-side.
  addAccountPoolMembers: (id: string, accountIds: string[]) =>
    Promise.all(accountIds.map((accountId) =>
      http.post(`/v1/admin/account-pools/${id}/accounts`, { account_id: accountId }))),
  removeAccountPoolMember: (id: string, accountId: string) =>
    http.delete(`/v1/admin/account-pools/${id}/accounts/${accountId}`),

  // ---- group ↔ account bindings (业务分组直连账号) ----
  groupAccounts: (groupId: string) =>
    http.get<{ accounts: GroupAccount[] }>(`/v1/admin/model-groups/${groupId}/accounts`)
      .then((r) => r.data.accounts),
  attachAccount: (groupId: string, b: { account_id: string; priority?: number; weight?: number; enabled?: boolean; model_names?: string[] }) =>
    http.post<{ account: GroupAccount }>(`/v1/admin/model-groups/${groupId}/accounts`, b).then((r) => r.data.account),
  updateGroupAccount: (groupId: string, accountId: string, b: { priority?: number; weight?: number; enabled?: boolean; model_names?: string[] }) =>
    http.patch<{ account: GroupAccount }>(`/v1/admin/model-groups/${groupId}/accounts/${accountId}`, b)
      .then((r) => r.data.account),
  detachAccount: (groupId: string, accountId: string) =>
    http.delete(`/v1/admin/model-groups/${groupId}/accounts/${accountId}`),

  financeSummary: (params: { from?: string; to?: string } = {}) =>
    http.get<FinanceSummary>("/v1/admin/finance/summary", { params }).then((r) => r.data),
  refundPolicy: () => http.get<RefundPolicy>("/v1/admin/finance/refund-policy").then((r) => r.data),
  updateRefundPolicy: (b: RefundPolicy) =>
    http.put<RefundPolicy>("/v1/admin/finance/refund-policy", b).then((r) => r.data),
  providerCosts: (params: { provider_id?: string; from?: string; to?: string; limit?: number; offset?: number } = {}) =>
    http.get<{ costs: ProviderCost[]; total: number; limit: number; offset: number }>("/v1/admin/finance/provider-costs", { params }).then((r) => r.data),
  createProviderCost: (b: { provider_id: string; amount: string; currency?: string; occurred_at?: string; note?: string }) =>
    http.post<{ cost: ProviderCost }>("/v1/admin/finance/provider-costs", b).then((r) => r.data.cost),
  deleteProviderCost: (id: string) => http.delete(`/v1/admin/finance/provider-costs/${id}`),
  providerSettlements: (params: { month?: string } = {}) =>
    http.get<{ month: string; period_start: string; period_end: string; settlements: ProviderSettlement[] }>("/v1/admin/finance/provider-settlements", { params }).then((r) => r.data),
  confirmProviderSettlement: (providerId: string, b: { month: string; actual_cost?: string; note?: string }) =>
    http.post<{ settlement: ProviderSettlement }>(`/v1/admin/finance/provider-settlements/${providerId}/confirm`, b).then((r) => r.data.settlement),
  // q matches the order number or the buyer's email / username / nickname.
  financeOrders: (params: { q?: string; status?: string; kind?: string; user_id?: string; limit?: number; offset?: number } = {}) =>
    http.get<{ orders: SiteOrder[]; total: number; limit: number; offset: number }>("/v1/admin/finance/orders", { params }).then((r) => r.data),
  createFinanceOrder: (b: { user_id: string; kind: "balance" | "plan"; plan_id?: string; amount: string; currency?: string; note?: string }) =>
    http.post<{ order: SiteOrder }>("/v1/admin/finance/orders", b).then((r) => r.data.order),
  markFinanceOrderPaid: (id: string) =>
    http.post<{ order: SiteOrder }>(`/v1/admin/finance/orders/${id}/pay`).then((r) => r.data.order),
  cancelFinanceOrder: (id: string) =>
    http.post<{ order: SiteOrder }>(`/v1/admin/finance/orders/${id}/cancel`).then((r) => r.data.order),
  orderRefundable: (orderId: string) => http.get<OrderRefundable>(`/v1/admin/finance/orders/${orderId}/refundable`).then((r) => r.data),
  createFinanceRefund: (orderId: string, b: { amount?: string; reason?: string; note?: string }) =>
    http.post<{ refund: SiteRefund }>(`/v1/admin/finance/orders/${orderId}/refunds`, b).then((r) => r.data.refund),
  // q matches the refunded order's number or its buyer's identity.
  financeRefunds: (params: { q?: string; status?: string; limit?: number; offset?: number } = {}) =>
    http.get<{ refunds: SiteRefund[]; total: number; limit: number; offset: number }>("/v1/admin/finance/refunds", { params }).then((r) => r.data),
  approveFinanceRefund: (id: string, note?: string) =>
    http.post<{ refund: SiteRefund }>(`/v1/admin/finance/refunds/${id}/approve`, { note }).then((r) => r.data.refund),
  rejectFinanceRefund: (id: string, note?: string) =>
    http.post<{ refund: SiteRefund }>(`/v1/admin/finance/refunds/${id}/reject`, { note }).then((r) => r.data.refund),

  // ---- invite program ----
  // Program rules live on SiteSettings.invite_program and are saved
  // through getSettings/updateSettings (merge, never overwrite others).
  inviteBindingsPaged: (params: InviteListParams = {}) =>
    http
      .get<{ bindings: InviteBinding[]; total: number; limit: number; offset: number }>(
        "/v1/admin/invite/bindings",
        { params },
      )
      .then((r) => ({
        items: (r.data.bindings ?? []).map(normalizeInviteBinding),
        total: r.data.total ?? 0,
        limit: r.data.limit ?? 0,
        offset: r.data.offset ?? 0,
      })),
  inviteRewardsPaged: (params: InviteListParams = {}) =>
    http
      .get<{ rewards: InviteReward[]; total: number; limit: number; offset: number }>(
        "/v1/admin/invite/rewards",
        { params },
      )
      .then((r) => ({
        items: (r.data.rewards ?? []).map(normalizeInviteReward),
        total: r.data.total ?? 0,
        limit: r.data.limit ?? 0,
        offset: r.data.offset ?? 0,
      })),
  inviteRebatesPaged: (params: InviteListParams = {}) =>
    http
      .get<{ rebates: InviteRebate[]; total: number; limit: number; offset: number }>(
        "/v1/admin/invite/rebates",
        { params },
      )
      .then((r) => ({
        items: (r.data.rebates ?? []).map(normalizeInviteRebate),
        total: r.data.total ?? 0,
        limit: r.data.limit ?? 0,
        offset: r.data.offset ?? 0,
      })),
  payInviteReward: (id: string, note?: string) =>
    http.post(`/v1/admin/invite/rewards/${id}/pay`, { note: note || undefined }),
  rejectInviteReward: (id: string, note?: string) =>
    http.post(`/v1/admin/invite/rewards/${id}/reject`, { note: note || undefined }),
  payInviteRebate: (id: string) =>
    http.post(`/v1/admin/invite/rebates/${id}/pay`),
  rejectInviteRebate: (id: string) =>
    http.post(`/v1/admin/invite/rebates/${id}/reject`),
  voidInviteBinding: (id: string) =>
    http.post(`/v1/admin/invite/bindings/${id}/void`),

  opsReport: (params: { from?: string; to?: string; grain?: "day" | "month" } = {}) =>
    http.get<OpsReport>("/v1/admin/ops", { params }).then((r) => r.data),
  opsExport: (params: { from?: string; to?: string; grain?: "day" | "month" } = {}) =>
    http.get<Blob>("/v1/admin/ops/export", { params, responseType: "blob" }).then((r) => r.data),


  models: () =>
    http.get<{ models: Model[] }>("/v1/admin/models", { params: { limit: 1000 } })
      .then((r) => r.data.models),
  modelsPaged: pagedAdaptor<Model>("models", "/v1/admin/models"),
  createModel: (b: any) => http.post<{ model: Model }>("/v1/admin/models", b).then((r) => r.data.model),
  importModels: (models: ImportPlatformModelItem[]) =>
    http.post<PlatformModelsImportResult>("/v1/admin/models/import", { models }).then((r) => r.data),
  updateModel: (id: string, b: any) =>
    http.patch<{ model: Model }>(`/v1/admin/models/${id}`, b).then((r) => r.data.model),
  deleteModel: (id: string) => http.delete(`/v1/admin/models/${id}`),
  // Send the ids of the rows currently on screen in their new order. The
  // backend splices them into the global ordering, so reordering one page
  // never renumbers the others on top of it.
  reorderModels: (ids: string[]) => http.post("/v1/admin/models/reorder", { ids }),

  vendors: () =>
    http.get<{ vendors: Vendor[] }>("/v1/admin/vendors", { params: { limit: 1000 } })
      .then((r) => r.data.vendors),
  vendorsPaged: pagedAdaptor<Vendor>("vendors", "/v1/admin/vendors"),
  createVendor: (b: { name: string; slug?: string; description?: string; sort_order?: number }) =>
    http.post<{ vendor: Vendor }>("/v1/admin/vendors", b).then((r) => r.data.vendor),
  updateVendor: (id: string, b: Partial<{ name: string; slug: string; description: string; sort_order: number }>) =>
    http.patch<{ vendor: Vendor }>(`/v1/admin/vendors/${id}`, b).then((r) => r.data.vendor),
  deleteVendor: (id: string) => http.delete(`/v1/admin/vendors/${id}`),

  groups: () =>
    http.get<{ groups: ModelGroup[] }>("/v1/admin/model-groups", { params: { limit: 1000 } })
      .then((r) => r.data.groups),
  groupsPaged: pagedAdaptor<ModelGroup>("groups", "/v1/admin/model-groups"),
  createGroup: (b: any) => http.post<{ group: ModelGroup }>("/v1/admin/model-groups", b).then((r) => r.data.group),
  deleteGroup: (id: string) => http.delete(`/v1/admin/model-groups/${id}`),
  reorderGroups: (ids: string[]) => http.post("/v1/admin/model-groups/reorder", { ids }),
  attachModel: (groupId: string, modelId: string) =>
    http.post(`/v1/admin/model-groups/${groupId}/models/${modelId}`),
  detachModel: (groupId: string, modelId: string) =>
    http.delete(`/v1/admin/model-groups/${groupId}/models/${modelId}`),
  // Both fields are optional server-side, so the switch can be flipped without
  // knowing the interval and the interval changed without touching the switch.
  setModelProbe: (
    groupId: string,
    modelId: string,
    body: { probe_enabled?: boolean; probe_interval_seconds?: number; context_window?: number },
  ) => http.patch(`/v1/admin/model-groups/${groupId}/models/${modelId}`, body),
  attachProvider: (groupId: string, providerId: string, body: { priority?: number; weight?: number }) =>
    http.post(`/v1/admin/model-groups/${groupId}/providers/${providerId}`, body),
  detachProvider: (groupId: string, providerId: string) =>
    http.delete(`/v1/admin/model-groups/${groupId}/providers/${providerId}`),

  plans: () =>
    http.get<{ plans: Plan[] }>("/v1/admin/plans", { params: { limit: 1000 } })
      .then((r) => r.data.plans),
  plansPaged: pagedAdaptor<Plan>("plans", "/v1/admin/plans"),
  createPlan: (b: any) => http.post<{ plan: Plan }>("/v1/admin/plans", b).then((r) => r.data.plan),
  updatePlan: (id: string, b: any) =>
    http.patch<{ plan: Plan }>(`/v1/admin/plans/${id}`, b).then((r) => r.data.plan),
  // Default deletePlan archives. Pass { hard: true } to evaporate the
  // plan + every UserPlan/snapshot referencing it. Returns counts on
  // hard delete so the UI can confirm impact in a toast.
  deletePlan: (id: string, opts?: { hard?: boolean }) =>
    http.delete<{ hard?: boolean; user_plans_deleted?: number; quota_snapshots_deleted?: number } | undefined>(
      `/v1/admin/plans/${id}${opts?.hard ? "?hard=true" : ""}`,
    ).then((r) => r.data ?? null),

  // ---- roles ----
  setUserRoles: (id: string, roles: string[]) =>
    http.put<{ roles: string[] }>(`/v1/admin/users/${id}/roles`, { roles }).then((r) => r.data.roles),

  // ---- agents ----
  agents: () => http.get<{ agents: AdminAgentRow[] }>("/v1/admin/agents").then((r) => r.data.agents),
  updateAgent: (id: string, b: { platform_share_pct?: string; coupon_discount_pct?: string; status?: "active" | "suspended" }) =>
    http.patch<AgentProfile>(`/v1/admin/agents/${id}`, b).then((r) => r.data),
  agentEntries: (id: string, params: { kind?: AgentEntryKind; status?: "pending" | "settled"; limit?: number; offset?: number } = {}) =>
    http.get<{ entries: AgentEntry[]; total: number }>(`/v1/admin/agents/${id}/entries`, { params }).then((r) => r.data),
  agentRefunds: (id: string, params: { limit?: number; offset?: number } = {}) =>
    http.get<{ refunds: AgentRefund[]; total: number }>(`/v1/admin/agents/${id}/refunds`, { params }).then((r) => r.data),
  agentSettlements: (id: string, params: { limit?: number; offset?: number } = {}) =>
    http.get<{ settlements: AgentSettlement[]; total: number }>(`/v1/admin/agents/${id}/settlements`, { params }).then((r) => r.data),
  settleAgent: (id: string, note?: string) =>
    http.post<{ id: string; total_payable: string; entries_count: number; created_at: string }>(
      `/v1/admin/agents/${id}/settle`, { note }
    ).then((r) => r.data),

  topupUser: (id: string, amount: string, note?: string) =>
    http.post(`/v1/admin/users/${id}/topup`, { amount, note }),
  resetUserPassword: (id: string, newPassword: string) =>
    http.post<{ ok: boolean; message: string }>(`/v1/admin/users/${id}/reset-password`, { new_password: newPassword }).then((r) => r.data),
  updateUser: (
    id: string,
    b: {
      email?: string;
      nickname?: string;
      role?: string;
      status?: string;
      user_group_id?: string;
      concurrency_limit_override?: number;
      priority?: number;
    },
  ) => http.patch(`/v1/admin/users/${id}`, b),
  deleteUser: (id: string) => http.delete(`/v1/admin/users/${id}`),
  userDetail: (id: string) =>
    http.get<{ user: any }>(`/v1/admin/users/${id}`).then((r) => r.data.user),
  users: (params: { q?: string; status?: string; limit?: number; offset?: number } = {}) =>
    http.get<PagedUsers>("/v1/admin/users", { params }).then((r) => r.data),
  approveUser: (id: string) =>
    http.post<{ id: string; status: string }>(`/v1/admin/users/${id}/approve`).then((r) => r.data),
  rejectUser: (id: string) =>
    http.post<{ id: string; status: string }>(`/v1/admin/users/${id}/reject`).then((r) => r.data),
  usageLogs: (params: UsageLogFilterParams = {}) =>
    http.get<PagedLogs>("/v1/admin/usage-logs", { params }).then((r) => r.data),
  usageLogsPaged: (params: UsageLogFilterParams = {}) =>
    http.get<PagedLogs>("/v1/admin/usage-logs", { params }).then((r) => ({
      items: r.data.logs, total: r.data.total, limit: r.data.limit, offset: r.data.offset,
    })),

  organizations: () =>
    http.get<{ organizations: Organization[] }>("/v1/admin/organizations", { params: { limit: 1000 } })
      .then((r) => r.data.organizations),
  organizationsPaged: pagedAdaptor<Organization>("organizations", "/v1/admin/organizations"),
  createOrganization: (b: { name: string; owner_user_id: string; monthly_spend_limit?: string }) =>
    http.post<{ organization: Organization }>("/v1/admin/organizations", b).then((r) => r.data.organization),
  organizationDetail: (id: string) =>
    http.get<{ organization: Organization }>(`/v1/admin/organizations/${id}`).then((r) => r.data.organization),
  updateOrganization: (id: string, b: { name?: string; owner_user_id?: string; monthly_spend_limit?: string }) =>
    http.patch<{ organization: Organization }>(`/v1/admin/organizations/${id}`, b).then((r) => r.data.organization),
  topupOrganization: (id: string, b: { amount: string; note?: string }) =>
    http.post<{ balance: string }>(`/v1/admin/organizations/${id}/topup`, b).then((r) => r.data),
  upsertOrganizationMember: (
    id: string,
    userId: string,
    b: { role?: "owner" | "admin" | "member"; monthly_spend_limit?: string; total_spend_limit?: string },
  ) => http.put(`/v1/admin/organizations/${id}/members/${userId}`, b),
  removeOrganizationMember: (id: string, userId: string) =>
    http.delete(`/v1/admin/organizations/${id}/members/${userId}`),

  // user groups
  userGroups: () =>
    http.get<{ user_groups: UserGroup[] }>("/v1/admin/user-groups", { params: { limit: 1000 } })
      .then((r) => r.data.user_groups),
  userGroupsPaged: pagedAdaptor<UserGroup>("user_groups", "/v1/admin/user-groups"),
  createUserGroup: (b: { name: string; description?: string; sort_order?: number; concurrency_limit?: number }) =>
    http.post<{ user_group: UserGroup }>("/v1/admin/user-groups", b).then((r) => r.data.user_group),
  updateUserGroup: (id: string, b: Partial<{ name: string; description: string; sort_order: number; concurrency_limit: number }>) =>
    http.patch<{ user_group: UserGroup }>(`/v1/admin/user-groups/${id}`, b).then((r) => r.data.user_group),
  deleteUserGroup: (id: string) => http.delete(`/v1/admin/user-groups/${id}`),
  setDefaultUserGroup: (id: string) =>
    http.post(`/v1/admin/user-groups/${id}/set-default`),

  // admin-managed apikeys for a target user
  userApikeys: (uid: string) =>
    http.get<{ api_keys: ApiKeyView[] }>(`/v1/admin/users/${uid}/apikeys`).then((r) => r.data.api_keys),
  createUserApikey: (uid: string, b: { name: string; allowed_group_ids?: string[] } & ApiKeyCostLimits) =>
    http.post<CreatedApiKey>(`/v1/admin/users/${uid}/apikeys`, b).then((r) => r.data),
  updateUserApikey: (uid: string, kid: string, b: ApiKeyCostLimits) =>
    http.patch<{ api_key: ApiKeyView }>(`/v1/admin/users/${uid}/apikeys/${kid}`, b).then((r) => r.data.api_key),
  revokeUserApikey: (uid: string, kid: string) =>
    http.delete(`/v1/admin/users/${uid}/apikeys/${kid}`),
  revealUserApikey: (uid: string, kid: string) =>
    http
      .get<{ secret: string }>(`/v1/admin/users/${uid}/apikeys/${kid}/reveal`)
      .then((r) => r.data.secret),

  // model group update (allowed_user_group_ids)
  cloneGroup: (id: string, b?: { name?: string; exclusive?: boolean; rate_multiplier?: number }) =>
    http.post<{ group: ModelGroup }>(`/v1/admin/model-groups/${id}/clone`, b ?? {}).then((r) => r.data.group),
  replaceGroupGrants: (id: string, b: { user_ids: string[]; user_group_ids: string[] }) =>
    http.put(`/v1/admin/model-groups/${id}/grants`, b),
  userModelGroupGrants: (id: string) =>
    http.get<{ group_ids: string[] }>(`/v1/admin/users/${id}/model-group-grants`).then((r) => r.data.group_ids),
  replaceUserModelGroupGrants: (id: string, group_ids: string[]) =>
    http.put(`/v1/admin/users/${id}/model-group-grants`, { group_ids }),
  userGroupModelGroupGrants: (id: string) =>
    http.get<{ group_ids: string[] }>(`/v1/admin/user-groups/${id}/model-group-grants`).then((r) => r.data.group_ids),
  replaceUserGroupModelGroupGrants: (id: string, group_ids: string[]) =>
    http.put(`/v1/admin/user-groups/${id}/model-group-grants`, { group_ids }),
  updateGroup: (id: string, b: Partial<{ name: string; billing_source: string; rate_multiplier: number; description: string; profile: GroupProfileDim[]; allowed_user_group_ids: string[]; enabled: boolean; exclusive: boolean }>) =>
    http.patch<{ group: ModelGroup }>(`/v1/admin/model-groups/${id}`, b).then((r) => r.data.group),

  redeemCodes: (params: { status?: string; kind?: string; batch_id?: string } = {}) =>
    http.get<{ codes: RedeemCode[] }>("/v1/admin/redeem-codes", { params: { ...params, limit: 1000 } }).then((r) => r.data.codes),
  redeemCodesPaged: (params: { q?: string; status?: string; kind?: string; batch_id?: string; limit?: number; offset?: number } = {}) =>
    http
      .get<{ codes: RedeemCode[]; total: number; limit: number; offset: number }>(
        "/v1/admin/redeem-codes",
        { params },
      )
      .then((r) => ({ items: r.data.codes, total: r.data.total, limit: r.data.limit, offset: r.data.offset })),
  generateRedeemCodes: (b: any) =>
    http.post<{ codes: RedeemCode[]; batch_id: string }>("/v1/admin/redeem-codes", b).then((r) => r.data),
  revokeRedeemCode: (id: string) => http.post(`/v1/admin/redeem-codes/${id}/revoke`),
  revokeRedeemBatch: (batchId: string) =>
    http.post<{ revoked: number }>(`/v1/admin/redeem-codes/batch/${batchId}/revoke`).then((r) => r.data),

  statsUsage: (params: { start?: string; end?: string; bucket?: string; model?: string; status?: string; user_id?: string }) =>
    http.get<{ buckets: StatsBucket[]; start: string; end: string; bucket: string }>(
      "/v1/admin/stats/usage", { params }).then((r) => r.data),
  // Platform-wide live trailing-60s throughput.
  statsRate: () =>
    http.get<{ rpm: number; tpm: number; rpm_limit: number; tpm_limit: number }>(
      "/v1/admin/stats/rate").then((r) => r.data),
  statsBreakdown: (params: { by: string; start?: string; end?: string }) =>
    http.get<{ by: string; rows: StatsBreakdownRow[] }>("/v1/admin/stats/breakdown", { params }).then((r) => r.data),
  // ---- observability (性能观测) ----
  observabilitySeries: (params: ObservabilitySeriesParams) =>
    http.get<ObservabilitySeries>("/v1/admin/observability/series", { params }).then((r) => r.data),
  observabilityLeaderboard: (params: {
    dim: Exclude<ObservabilityDim, "platform">;
    window: ObservabilityWindow;
    include_synthetic: boolean;
  }) =>
    http.get<ObservabilityLeaderboard>("/v1/admin/observability/leaderboard", { params }).then((r) => r.data),
  observabilitySla: (params: { window: ObservabilitySlaWindow; group_id?: string }) =>
    http.get<ObservabilitySla>("/v1/admin/observability/sla", { params }).then((r) => r.data),
  observabilityProviderAttempts: (params: { window: ObservabilityWindow; include_synthetic: boolean }) =>
    http.get<ProviderAttemptsResponse>("/v1/admin/observability/provider-attempts", { params }).then((r) => r.data),
  observabilityLoadMap: (params: { window?: LoadMapWindow } = {}) =>
    http.get<LoadMapResponse>("/v1/admin/observability/load-map", { params }).then((r) => r.data),
  observabilityErrors: (params: { window?: ObservabilityErrorWindow; include_synthetic?: boolean; limit?: number } = {}) =>
    http.get<ObservabilityErrorsResponse>("/v1/admin/observability/errors", { params }).then((r) => r.data),

  routingBoard: () =>
    http.get<{ providers: RoutingBoardProvider[] }>("/v1/admin/routing/board").then((r) => r.data),
  routingPreview: (model: string) =>
    http.get<{
      model: string;
      group: string;
      group_id: string;
      providers: RoutingPreviewProvider[];
      groups: RoutingPreviewGroup[];
    }>("/v1/admin/routing/preview", { params: { model } }).then((r) => r.data),

  getSettings: () => http.get<SiteSettings>("/v1/admin/settings").then((r) => r.data),
  updateSettings: (s: SiteSettings) => http.put<SiteSettings>("/v1/admin/settings", s).then((r) => r.data),
  uploadSiteAsset: (file: File) => {
    const fd = new FormData();
    fd.append("file", file, file.name);
    return http.post<{ url: string }>("/v1/admin/site-assets", fd).then((r) => r.data);
  },
  webVitals: (days = 7) =>
    http
      .get<{
        window_days: number;
        rows: { name: string; count: number; p50: number; p75: number; p95: number; good_pct: number }[];
      }>(`/v1/admin/web-vitals?days=${days}`)
      .then((r) => r.data),
  policyStats: () =>
    http.get<{ categories: number; keywords: number; active: number }>("/v1/admin/policy/stats").then((r) => r.data),
  policyGroups: () =>
    http
      .get<{ groups: PolicyGroup[] }>("/v1/admin/policy/groups")
      .then((r) => r.data.groups),
  createPolicyGroup: (b: PolicyGroupInput) =>
    http.post<PolicyGroup>("/v1/admin/policy/groups", b).then((r) => r.data),
  updatePolicyGroup: (id: string, b: Partial<PolicyGroupInput>) =>
    http.patch<PolicyGroup>(`/v1/admin/policy/groups/${id}`, b).then((r) => r.data),
  deletePolicyGroup: (id: string) => http.delete(`/v1/admin/policy/groups/${id}`),
  policyKeywords: (params: { group_id?: string; q?: string; limit?: number; offset?: number } = {}) =>
    http
      .get<{ keywords: PolicyKeyword[]; total: number; limit: number; offset: number }>(
        "/v1/admin/policy/keywords",
        { params },
      )
      .then((r) => r.data),
  createPolicyKeyword: (b: PolicyKeywordInput) =>
    http.post<PolicyKeyword>("/v1/admin/policy/keywords", b).then((r) => r.data),
  updatePolicyKeyword: (id: string, b: Partial<PolicyKeywordInput>) =>
    http.patch<PolicyKeyword>(`/v1/admin/policy/keywords/${id}`, b).then((r) => r.data),
  deletePolicyKeyword: (id: string) => http.delete(`/v1/admin/policy/keywords/${id}`),
  importPolicyKeywords: (groupId: string, text: string) =>
    http
      .post<{ results: PolicyImportResult[] }>(`/v1/admin/policy/groups/${groupId}/import`, { text })
      .then((r) => r.data.results),

  // ---- user incidents ----
  incidents: (params: {
    q?: string;
    kind?: string;
    severity?: string;
    user_id?: string;
    since?: number;
    until?: number;
    limit?: number;
    offset?: number;
  } = {}) =>
    http
      .get<{ incidents: UserIncident[]; total: number; limit: number; offset: number }>("/v1/admin/incidents", {
        params,
      })
      .then((r) => r.data),
  incidentStats: (params: { user_id?: string; since?: number; until?: number } = {}) =>
    http
      .get<IncidentStats>("/v1/admin/incidents/stats", { params })
      .then((r) => r.data),

  // ---- system availability incidents ----
  systemIncidents: (params: SystemIncidentListParams = {}) =>
    http
      .get<SystemIncidentListResponse>("/v1/admin/system-incidents", { params })
      .then((r) => r.data),
  systemIncident: (id: string) =>
    http
      .get<{ incident: SystemIncident }>(`/v1/admin/system-incidents/${id}`)
      .then((r) => r.data.incident),
  updateSystemIncidentNote: (id: string, note: string) =>
    http
      .patch<{ incident: SystemIncident }>(`/v1/admin/system-incidents/${id}`, { note })
      .then((r) => r.data.incident),

  // ---- IP policy ----
  ipPolicies: (params: { kind?: IPPolicyKind } = {}) =>
    http
      .get<{ policies: IPPolicy[] }>("/v1/admin/ip-policies", { params })
      .then((r) => r.data.policies ?? []),
  createIPPolicy: (body: { cidr: string; kind: IPPolicyKind; note?: string; expires_at?: string }) =>
    http.post<IPPolicy>("/v1/admin/ip-policies", body).then((r) => r.data),
  deleteIPPolicy: (id: string) => http.delete(`/v1/admin/ip-policies/${id}`),

  // ---- admin audit logs ----
  auditLogs: (params: {
    q?: string;
    action?: string;
    target?: string;
    actor_user_id?: string;
    since?: number;
    until?: number;
    limit?: number;
    offset?: number;
  } = {}) =>
    http
      .get<{ logs: AdminAuditLog[]; total: number; limit: number; offset: number }>("/v1/admin/audit-logs", {
        params,
      })
      .then((r) => r.data),
};
