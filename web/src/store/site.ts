import { create } from "zustand";
import type { SiteSettings } from "../api/endpoints";
import { publicApi } from "../api/public";
import { deployment } from "../config/env";

interface SiteState {
  site: SiteSettings | null;
  loaded: boolean;
  load: () => Promise<void>;
  refresh: () => Promise<void>;
}

// Reasonable defaults so the UI never renders empty strings while loading.
const fallback: SiteSettings = {
  site_name: deployment.siteName,
  site_description: "高性能的统一大模型网关",
  site_url: "",
  site_logo: "",
  icp_number: "",
  contact_email: "",
  contact_group_label: "QQ 群",
  contact_group: "",
  contact_group_qr_url: "",
  contact_dock_enabled: false,
  marketing_banner: {
    enabled: false,
    text: "",
    link_url: "",
  },
  announcement_enabled: false,
  announcement_display_mode: "always",
  announcement_title: "",
  announcement_content: "",
  announcements: [],
  endpoint_openai_responses: "",
  endpoint_openai_compat: "",
  endpoint_anthropic: "",
  registration_enabled: true,
  registration_requires_approval: false,
  // Mirrors the backend default in biz/settings.go, so a failed site-info
  // fetch does not hide a payment channel the deployment has switched on.
  payment_alipay_enabled: true,
  topup_presets: [50, 100, 200, 500, 1000],
  sticky_ttl_seconds: 1800,
  status_page_group_ids: [],
  status_page_window_hours: 24,
  status_page_upstream_enabled: true,
  ops_alert_enabled: false,
  ops_alert_webhook_type: "generic",
  ops_alert_timeout_seconds: 5,
  ops_alert_max_retries: 2,
  ops_alert_silence_seconds: 300,
  // SMTP + email-verify defaults — the public siteInfo() endpoint
  // doesn't carry these fields, but the type requires them. We pick
  // safe blanks so the fallback satisfies the type.
  smtp_host: "",
  smtp_port: 0,
  smtp_username: "",
  smtp_from_email: "",
  smtp_from_name: "",
  smtp_use_tls: false,
  require_register_verification: false,
  require_change_email_verification: false,
  allow_password_reset: false,
  allow_image_output: false,
  format_conversion_enabled: false,
  websocket_enabled: true,
  stream_first_text_timeout_seconds: 20,
  moderation_enabled: false,
  moderation_base_url: "https://api.openai.com/v1",
  moderation_model: "omni-moderation-latest",
  moderation_threshold: 0.5,
  moderation_sample_rate: 0,
  moderation_max_per_min: 60,
  exchange_cny_per_usd: 7.2,
  max_incentive_pct: 30,
  agent_plan_refund_day_divisor: 30,
  agent_plan_refund_rate_pct: 70,
  agent_balance_refund_rate_pct: 90,
  reasoning_parse_rules: [],
};

export const useSiteStore = create<SiteState>((set, get) => ({
  site: null,
  loaded: false,
  async load() {
    if (get().loaded) return;
    try {
      const s = await publicApi.siteInfo();
      set({ site: s, loaded: true });
    } catch {
      set({ site: fallback, loaded: true });
    }
  },
  async refresh() {
    try {
      const s = await publicApi.siteInfo();
      set({ site: s, loaded: true });
    } catch { /* ignore */ }
  },
}));

export function useSite(): SiteSettings {
  return useSiteStore((s) => s.site) ?? fallback;
}
