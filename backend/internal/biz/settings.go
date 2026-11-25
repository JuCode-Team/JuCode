package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	entsetting "github.com/jucode/jucode/internal/data/ent/sitesetting"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
)

type MarketingBannerSettings struct {
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
	LinkURL string `json:"link_url"`
}

// SiteSettings is the typed projection over the site_settings KV table.
// Scalar fields ride as plain strings; structured fields are serialized as
// JSON in the KV value and parsed on read.
type SiteSettings struct {
	SiteName        string `json:"site_name"`
	SiteDescription string `json:"site_description"`
	SiteURL         string `json:"site_url"`
	// SiteLogo is either an http(s):// URL or an inline data: URL
	// (data:image/png;base64,...). The frontend uses it directly in
	// <img src>, so the same field handles both forms without a separate
	// upload service.
	SiteLogo     string `json:"site_logo"`
	ICPNumber    string `json:"icp_number"`
	ContactEmail string `json:"contact_email"`
	// ContactGroup is the support community's id or invite text (a chat
	// group number, an invite link; empty hides it), at most 64 characters.
	// ContactGroupLabel names the kind of group for display, e.g. "QQ 群".
	// ContactGroupQRURL is the group's QR image: an uploaded
	// /v1/public/site-assets/<name> path or an external http(s) URL.
	// ContactDockEnabled shows the bottom-right contact dock and the home
	// page "join the group" entry.
	ContactGroup       string `json:"contact_group"`
	ContactGroupLabel  string `json:"contact_group_label"`
	ContactGroupQRURL  string `json:"contact_group_qr_url"`
	ContactDockEnabled bool   `json:"contact_dock_enabled"`

	MarketingBanner MarketingBannerSettings `json:"marketing_banner"`

	AnnouncementEnabled     bool           `json:"announcement_enabled"`
	AnnouncementDisplayMode string         `json:"announcement_display_mode"`
	AnnouncementTitle       string         `json:"announcement_title"`
	AnnouncementContent     string         `json:"announcement_content"`
	Announcements           []Announcement `json:"announcements"`

	// Public base URLs admins want users to paste into their LLM clients.
	// Empty values are auto-derived from SiteURL at display time so the
	// admin only has to fill the site_url and everything else falls into
	// place; explicit values are useful when JuCode is behind a path-rewriting
	// proxy or a separate CDN per protocol.
	EndpointOpenAIResponses string `json:"endpoint_openai_responses"`
	EndpointOpenAICompat    string `json:"endpoint_openai_compat"`
	EndpointAnthropic       string `json:"endpoint_anthropic"`

	// DefaultPoolMode is the site-wide account health policy used by accounts
	// that do not override it: balanced (default) | conservative | lenient |
	// observe. Pool mode only picks how aggressively a failing account is
	// cooled or disabled; it never changes routing or billing.
	DefaultPoolMode string `json:"default_pool_mode"`

	// Registration policy. Defaults: open + no approval (legacy behavior).
	// When RegistrationEnabled=false, /v1/auth/register returns 403 outright.
	// When RegistrationRequiresApproval=true, new users land in
	// status="pending_approval" and cannot log in until an admin clears them.
	RegistrationEnabled          bool `json:"registration_enabled"`
	RegistrationRequiresApproval bool `json:"registration_requires_approval"`

	// PaymentAlipayEnabled only controls whether the frontend may initiate
	// new Alipay checkouts; already-issued payment links can still be
	// rendered from the stored order payload.
	PaymentAlipayEnabled bool `json:"payment_alipay_enabled"`

	// SelfRefundEnabled lets users refund their paid-for balance and quota
	// subscriptions themselves, back through the payment channel
	// (self_refund.go).
	SelfRefundEnabled bool `json:"self_refund_enabled"`
	// RefundAutoApproveEnabled approves the refund request a user's refund
	// ticket raises when the order's channel can refund automatically and
	// the amount is within RefundAutoApproveMax ("" or "0" = no cap).
	RefundAutoApproveEnabled bool   `json:"refund_auto_approve_enabled"`
	RefundAutoApproveMax     string `json:"refund_auto_approve_max"`
	// AgentCouponsEnabled=false stops agent coupon codes at checkout, so no
	// new coupon discounts or agent rebates arise.
	AgentCouponsEnabled bool `json:"agent_coupons_enabled"`

	// Sticky session TTL (seconds) for provider-level pinning.
	// 0 disables sticky entirely (gateway falls back to plain
	// priority/weight ordering). Default 1800s (30min).
	StickyTTLSeconds int `json:"sticky_ttl_seconds"`

	// Status page configuration. StatusPageGroupIDs lists the model groups
	// whose real business-request availability appears on the public page;
	// empty = page is hidden. The slice order is meaningful: it is the display
	// order on the page, so persistence must never sort or dedupe it.
	StatusPageGroupIDs    []uuid.UUID `json:"status_page_group_ids"`
	StatusPageWindowHours int         `json:"status_page_window_hours"`
	// StatusPageUpstreamEnabled controls whether the public page mirrors
	// what OpenAI/Anthropic publish on their own status pages. Off hides
	// the panel entirely; the poll loop keeps running either way, so
	// flipping it back on shows fresh data immediately.
	StatusPageUpstreamEnabled bool `json:"status_page_upstream_enabled"`

	// Operations alerts. URL and signing secret are write-only plaintext:
	// both are encrypted in site_settings and reads expose presence flags
	// only. The feature is deliberately disabled by default.
	OpsAlertEnabled          bool   `json:"ops_alert_enabled"`
	OpsAlertWebhookType      string `json:"ops_alert_webhook_type"`
	OpsAlertWebhookURL       string `json:"ops_alert_webhook_url,omitempty"`
	OpsAlertWebhookURLSet    bool   `json:"ops_alert_webhook_url_set"`
	OpsAlertWebhookSecret    string `json:"ops_alert_webhook_secret,omitempty"`
	OpsAlertWebhookSecretSet bool   `json:"ops_alert_webhook_secret_set"`
	OpsAlertTimeoutSeconds   int    `json:"ops_alert_timeout_seconds"`
	OpsAlertMaxRetries       int    `json:"ops_alert_max_retries"`
	OpsAlertSilenceSeconds   int    `json:"ops_alert_silence_seconds"`

	// SMTP outbound. SMTPHost empty = email feature disabled regardless of
	// the verification toggle below. Password is encrypted-at-rest with
	// the global ENCRYPT_KEY (same cipher as Provider.APIKeyEnc), so the
	// raw value never leaves this struct in a GET — admin UI displays
	// "******" placeholder. Setting an empty SMTPPassword on update is a
	// no-op (keep previous); setting a sentinel "" via UI's explicit
	// "clear" is rare so we accept the tradeoff.
	SMTPHost        string `json:"smtp_host"`
	SMTPPort        int    `json:"smtp_port"`
	SMTPUsername    string `json:"smtp_username"`
	SMTPPassword    string `json:"smtp_password,omitempty"` // PLAINTEXT only on write; never returned populated on read
	SMTPPasswordSet bool   `json:"smtp_password_set"`       // read-only: "password is configured"
	SMTPFromEmail   string `json:"smtp_from_email"`
	SMTPFromName    string `json:"smtp_from_name"`
	SMTPUseTLS      bool   `json:"smtp_use_tls"`
	// Email verification toggles. Each one is independent so an operator
	// can phase the feature in: e.g. enable RequireRegisterVerification
	// only once SMTP is proven working, leave change-email always-on.
	RequireRegisterVerification    bool `json:"require_register_verification"`
	RequireChangeEmailVerification bool `json:"require_change_email_verification"`
	AllowPasswordReset             bool `json:"allow_password_reset"`

	// AllowImageOutput gates whether the LLM gateway may surface model-
	// generated images (assistant content[].type = image_url etc) to
	// the client. Default false. When false, every image in the
	// response stream is replaced with a notice text and the original
	// bytes are discarded. When true, images pass through to the client
	// and are billed via Model.OutputImagePrice.
	AllowImageOutput bool `json:"allow_image_output"`
	// FormatConversionEnabled rewrites client requests among Chat Completions
	// (compatible), Responses (openai), and Anthropic Messages when the
	// upstream speaks a different dialect. Off (default) keeps native
	// passthrough and today's anthropic→chat fallback only.
	FormatConversionEnabled bool `json:"format_conversion_enabled"`
	// WebSocketEnabled gates GET /v1/realtime and the Responses WebSocket.
	// Default true so authenticated clients can upgrade; operators can flip
	// it off without a redeploy. Off makes both endpoints 404, which is what
	// a probing client needs to fall back to the HTTP endpoints.
	WebSocketEnabled bool `json:"websocket_enabled"`
	// StreamFirstTextTimeoutSeconds caps streamed requests before the
	// first upstream content frame (any data frame — response.created,
	// a reasoning delta, ...). Once frames flow, the idle timer takes
	// over stall detection. 0 disables this guard.
	StreamFirstTextTimeoutSeconds int `json:"stream_first_text_timeout_seconds"`
	// StreamTotalTimeoutSeconds is a wall-clock cap on a whole streamed
	// request (across attempts), bounding pathological long tails. 0 disables.
	StreamTotalTimeoutSeconds int `json:"stream_total_timeout_seconds"`
	// StreamIdleTimeoutSeconds caps the gap between streamed bytes. 0 falls
	// back to the request timeout.
	StreamIdleTimeoutSeconds int `json:"stream_idle_timeout_seconds"`

	// Content moderation via an OpenAI-compatible Moderation API
	// (omni-moderation-latest). Async, off the hot path. ModerationAPIKey is
	// write-only plaintext on update (encrypted at rest like SMTPPassword);
	// reads never return it populated — ModerationAPIKeySet reports presence.
	//
	// ModerationThreshold is the category-score cutoff (0-1): a request is
	// treated as a violation when any returned category score meets it.
	// Lower = stricter. ModerationSampleRate spot-checks otherwise-clean
	// traffic; ModerationMaxPerMin caps upstream calls/min (cost guard).
	ModerationEnabled    bool    `json:"moderation_enabled"`
	ModerationBaseURL    string  `json:"moderation_base_url"`
	ModerationModel      string  `json:"moderation_model"`
	ModerationAPIKey     string  `json:"moderation_api_key,omitempty"` // PLAINTEXT only on write; never returned populated
	ModerationAPIKeySet  bool    `json:"moderation_api_key_set"`       // read-only: "key is configured"
	ModerationThreshold  float64 `json:"moderation_threshold"`
	ModerationSampleRate float64 `json:"moderation_sample_rate"`
	ModerationMaxPerMin  int     `json:"moderation_max_per_min"`

	// Currency display & provider-settlement conversion. The platform's
	// unit of account is 积分 (points): 1 point = 1 CNY, stored with
	// currency = "CNY" (user.balance, plan.price, model unit prices,
	// usage_log.cost_*). User-facing paths never apply an exchange
	// rate — prices are entered and charged directly in points.
	//
	// ExchangeCNYPerUSD: how many CNY one USD is worth, used ONLY on
	// the provider (channel) settlement link — provider costs may be
	// entered in USD and are converted to CNY at settlement time (see
	// FinanceSvc provider settlement functions). 7.2 is the default;
	// admin can re-tune via the settings UI as the spot rate drifts.
	// NEVER set to 0 — settlement conversion treats rate ≤ 0 as
	// "not configured" and falls through unconverted.
	//
	// DefaultDisplayCurrency: the currency a new browser sees before
	// the user opens the toggle. "CNY" matches the audience and the
	// internal unit, so almost all reads stay 1:1 with no rounding loss.
	ExchangeCNYPerUSD      float64 `json:"exchange_cny_per_usd"`
	DefaultDisplayCurrency string  `json:"default_display_currency"`

	// MaxIncentivePct caps what any single order may give away, in
	// percent of face value: an agent's whole budget (redeem-code
	// wholesale margin, or coupon discount plus rebate) and the invite
	// top-up rebate rate. It is the agent's wholesale discount — 30 means
	// the platform nets at least 70% of face value on every channel. New
	// agent profiles start at exactly this budget. Range (0, 100].
	MaxIncentivePct float64 `json:"max_incentive_pct"`

	// Agent refund terms (AgentSvc.RegisterRefund). A plan refund is the
	// plan price × remaining days / AgentPlanRefundDayDivisor ×
	// AgentPlanRefundRatePct%; the divisor is fixed rather than the plan's
	// own period. A balance refund is the redeemed face value ×
	// AgentBalanceRefundRatePct%. The rest is kept to cover overhead.
	// TopupPresets are the balance top-up amounts (CNY) the console offers
	// as one-click choices, in display order. Omitted on update: unchanged.
	TopupPresets []int `json:"topup_presets"`

	AgentPlanRefundDayDivisor int     `json:"agent_plan_refund_day_divisor"`
	AgentPlanRefundRatePct    float64 `json:"agent_plan_refund_rate_pct"`
	AgentBalanceRefundRatePct float64 `json:"agent_balance_refund_rate_pct"`

	// ReasoningParseRules: ordered probes that extract a request's
	// thinking-strength label from the inbound body (see
	// biz/reasoning.go). Admin-editable. The default reproduces the
	// legacy hardcoded probes; an explicitly-saved empty list disables
	// extraction entirely.
	ReasoningParseRules []ReasoningParseRule `json:"reasoning_parse_rules"`

	// InviteProgram: invite/referral rebate configuration (see
	// settings_invite.go). nil on update = leave stored config untouched
	// (same convention as ReasoningParseRules); reads always populate it.
	// NOTE: this is the FULL admin object — /v1/public/site-info must NOT
	// serialize it as-is; the public handler swaps in Public() (router.go).
	InviteProgram *InviteProgramSettings `json:"invite_program,omitempty"`
}

type Announcement struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Enabled     bool   `json:"enabled"`
	DisplayMode string `json:"display_mode"`
}

const (
	keySiteName        = "site_name"
	keyDefaultPoolMode = "default_pool_mode"
	keySiteDescription = "site_description"
	keySiteURL         = "site_url"
	keySiteLogo        = "site_logo"
	keyICPNumber       = "icp_number"
	keyContactEmail    = "contact_email"

	keyContactGroup       = "contact_group"
	keyContactGroupLabel  = "contact_group_label"
	keyContactGroupQRURL  = "contact_group_qr_url"
	keyContactDockEnabled = "contact_dock_enabled"

	keyMarketingBannerEnabled = "marketing_banner_enabled"
	keyMarketingBannerText    = "marketing_banner_text"
	keyMarketingBannerLinkURL = "marketing_banner_link_url"

	keyAnnouncementEnabled     = "announcement_enabled"
	keyAnnouncementDisplayMode = "announcement_display_mode"
	keyAnnouncementTitle       = "announcement_title"
	keyAnnouncementContent     = "announcement_content"
	keyAnnouncements           = "announcements"

	keyEndpointOpenAIResponses = "endpoint_openai_responses"
	keyEndpointOpenAICompat    = "endpoint_openai_compat"
	keyEndpointAnthropic       = "endpoint_anthropic"

	keyRegistrationEnabled          = "registration_enabled"
	keyRegistrationRequiresApproval = "registration_requires_approval"
	keyPaymentAlipayEnabled         = "payment_alipay_enabled"
	keySelfRefundEnabled            = "self_refund_enabled"
	keyRefundAutoApproveEnabled     = "refund_auto_approve_enabled"
	keyRefundAutoApproveMax         = "refund_auto_approve_max"
	keyAgentCouponsEnabled          = "agent_coupons_enabled"

	keyStickyTTLSeconds = "sticky_ttl_seconds"

	keyStatusPageGroupIDs        = "status_page_group_ids"
	keyStatusPageWindowHours     = "status_page_window_hours"
	keyStatusPageUpstreamEnabled = "status_page_upstream_enabled"

	keyOpsAlertEnabled          = "ops_alert_enabled"
	keyOpsAlertWebhookType      = "ops_alert_webhook_type"
	keyOpsAlertWebhookURLEnc    = "ops_alert_webhook_url_enc"
	keyOpsAlertWebhookSecretEnc = "ops_alert_webhook_secret_enc"
	keyOpsAlertTimeoutSeconds   = "ops_alert_timeout_seconds"
	keyOpsAlertMaxRetries       = "ops_alert_max_retries"
	keyOpsAlertSilenceSeconds   = "ops_alert_silence_seconds"

	keySMTPHost                      = "smtp_host"
	keySMTPPort                      = "smtp_port"
	keySMTPUsername                  = "smtp_username"
	keySMTPPasswordEnc               = "smtp_password_enc"
	keySMTPFromEmail                 = "smtp_from_email"
	keySMTPFromName                  = "smtp_from_name"
	keySMTPUseTLS                    = "smtp_use_tls"
	keyReqRegisterVerify             = "require_register_verification"
	keyReqChangeVerify               = "require_change_email_verification"
	keyAllowResetPwd                 = "allow_password_reset"
	keyAllowImageOutput              = "allow_image_output"
	keyFormatConversionEnabled       = "format_conversion_enabled"
	keyWebSocketEnabled              = "websocket_enabled"
	keyStreamFirstTextTimeoutSeconds = "stream_first_text_timeout_seconds"
	keyStreamTotalTimeoutSeconds     = "stream_total_timeout_seconds"
	keyStreamIdleTimeoutSeconds      = "stream_idle_timeout_seconds"

	keyModerationEnabled    = "moderation_enabled"
	keyModerationBaseURL    = "moderation_base_url"
	keyModerationModel      = "moderation_model"
	keyModerationAPIKeyEnc  = "moderation_api_key_enc"
	keyModerationThreshold  = "moderation_threshold"
	keyModerationSampleRate = "moderation_sample_rate"
	keyModerationMaxPerMin  = "moderation_max_per_min"

	keyExchangeCNYPerUSD      = "exchange_cny_per_usd"
	keyDefaultDisplayCurrency = "default_display_currency"
	keyMaxIncentivePct        = "max_incentive_pct"

	keyTopupPresets              = "topup_presets"
	keyAgentPlanRefundDayDivisor = "agent_plan_refund_day_divisor"
	keyAgentPlanRefundRatePct    = "agent_plan_refund_rate_pct"
	keyAgentBalanceRefundRatePct = "agent_balance_refund_rate_pct"

	// maxAgentPlanRefundDayDivisor bounds the plan refund divisor (ten years).
	maxAgentPlanRefundDayDivisor = 3650
	keyReasoningParseRules       = "reasoning_parse_rules"
	keyInviteProgram             = "invite_program"
)

var siteDefaults = SiteSettings{
	SiteName:                      "JuCode",
	ContactGroupLabel:             "QQ 群",
	DefaultPoolMode:               string(PoolModeBalanced),
	SiteDescription:               "AI 编程平台 · 统一 LLM 网关",
	AnnouncementDisplayMode:       "always",
	Announcements:                 []Announcement{},
	RegistrationEnabled:           true,
	PaymentAlipayEnabled:          true,
	AgentCouponsEnabled:           true,
	StickyTTLSeconds:              1800,
	StatusPageWindowHours:         24,
	StatusPageUpstreamEnabled:     true,
	OpsAlertWebhookType:           OpsAlertWebhookGeneric,
	OpsAlertTimeoutSeconds:        5,
	OpsAlertMaxRetries:            2,
	OpsAlertSilenceSeconds:        300,
	StreamFirstTextTimeoutSeconds: 90,
	StreamTotalTimeoutSeconds:     600,
	StreamIdleTimeoutSeconds:      60,
	FormatConversionEnabled:       false,
	WebSocketEnabled:              true,
	ModerationBaseURL:             "https://api.openai.com/v1",
	ModerationModel:               "omni-moderation-latest",
	ModerationThreshold:           0.5,
	ModerationSampleRate:          0,
	ModerationMaxPerMin:           60,
	ExchangeCNYPerUSD:             7.2,
	DefaultDisplayCurrency:        "CNY",
	MaxIncentivePct:               30,
	TopupPresets:                  []int{50, 100, 200, 500, 1000},
	AgentPlanRefundDayDivisor:     30,
	AgentPlanRefundRatePct:        70,
	AgentBalanceRefundRatePct:     90,
	// Populated lazily via DefaultReasoningParseRules() in loadSite —
	// a package-level func call can't appear in this literal.
	ReasoningParseRules: nil,
}

type SettingsSvc struct {
	d      *data.Data
	cipher *cryptopkg.Cipher // optional — only required for SMTP password en/decrypt

	site atomic.Pointer[SiteSettings]

	// cnyPerUSD is an atomic snapshot of SiteSettings.ExchangeCNYPerUSD,
	// refreshed by StartCurrencyRateRefresh and read by the provider
	// (channel) settlement path. Encoded as a *decimal.Decimal pointer
	// so reads are wait-free and never block on the kv-backed GetSite
	// query.
	cnyPerUSD atomic.Pointer[decimal.Decimal]
}

func NewSettingsSvc(d *data.Data) *SettingsSvc { return &SettingsSvc{d: d} }

// CNYPerUSD returns the cached USD→CNY rate. Zero (decimal.Zero) when the
// background loader hasn't populated the cache yet OR when the admin
// configured an invalid rate. Used ONLY by the provider settlement path:
// provider_cost_usd × CNYPerUSD = provider_cost_cny. User-facing billing
// never applies this rate (1 point = 1 CNY).
func (s *SettingsSvc) CNYPerUSD() decimal.Decimal {
	p := s.cnyPerUSD.Load()
	if p == nil {
		return decimal.Zero
	}
	return *p
}

func cloneSiteSettings(in *SiteSettings) *SiteSettings {
	if in == nil {
		return nil
	}
	out := *in
	if in.Announcements != nil {
		out.Announcements = append([]Announcement(nil), in.Announcements...)
	}
	if in.StatusPageGroupIDs != nil {
		out.StatusPageGroupIDs = append([]uuid.UUID(nil), in.StatusPageGroupIDs...)
	}
	if in.TopupPresets != nil {
		out.TopupPresets = append([]int(nil), in.TopupPresets...)
	}
	if in.ReasoningParseRules != nil {
		out.ReasoningParseRules = cloneReasoningParseRules(in.ReasoningParseRules)
	}
	if in.InviteProgram != nil {
		ip := *in.InviteProgram
		out.InviteProgram = &ip
	}
	return &out
}

func cloneReasoningParseRules(in []ReasoningParseRule) []ReasoningParseRule {
	out := make([]ReasoningParseRule, len(in))
	copy(out, in)
	for i := range out {
		if in[i].ValueMap != nil {
			vm := make(map[string]string, len(in[i].ValueMap))
			for k, v := range in[i].ValueMap {
				vm[k] = v
			}
			out[i].ValueMap = vm
		}
	}
	return out
}

// RefundAutoApproveCap is the largest refund approved automatically; zero
// means no cap. An unparsable value counts as approving nothing: UpdateSite
// rejects one, so it can only come from a hand edit.
func (st *SiteSettings) RefundAutoApproveCap() (decimal.Decimal, bool) {
	v := strings.TrimSpace(st.RefundAutoApproveMax)
	if v == "" {
		return decimal.Zero, true
	}
	d, err := decimal.NewFromString(v)
	if err != nil || d.Sign() < 0 {
		return decimal.Zero, false
	}
	return d, true
}

func (s *SettingsSvc) storeSite(st *SiteSettings) {
	if st == nil {
		return
	}
	s.site.Store(cloneSiteSettings(st))
	if st.ExchangeCNYPerUSD > 0 {
		d := decimal.NewFromFloat(st.ExchangeCNYPerUSD)
		s.cnyPerUSD.Store(&d)
	}
}

// StartCurrencyRateRefresh kicks off a background loop that re-reads the
// site settings every minute. Called once from main after the settings
// service is wired up. Cheap: readers (provider settlement) see an
// atomic pointer swap, never block on the GetSite query.
func (s *SettingsSvc) StartCurrencyRateRefresh(ctx context.Context) {
	load := func() {
		st, err := s.loadSite(ctx)
		if err != nil || st == nil {
			return
		}
		s.storeSite(st)
	}
	load()
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				load()
			}
		}
	}()
}

// SetCipher wires the global encrypt cipher in after main has it
// available; matches the lifecycle of ApiKeySvc.SetCipher.
func (s *SettingsSvc) SetCipher(c *cryptopkg.Cipher) { s.cipher = c }

func (s *SettingsSvc) GetSite(ctx context.Context) (*SiteSettings, error) {
	if st := s.site.Load(); st != nil {
		return cloneSiteSettings(st), nil
	}
	st, err := s.loadSite(ctx)
	if err != nil {
		return nil, err
	}
	s.storeSite(st)
	return cloneSiteSettings(st), nil
}

func (s *SettingsSvc) loadSite(ctx context.Context) (*SiteSettings, error) {
	rows, err := s.d.Ent.SiteSetting.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	out := siteDefaults
	// Fresh slice per load so callers can't mutate a shared default.
	out.ReasoningParseRules = DefaultReasoningParseRules()
	if v, ok := m[keySiteName]; ok {
		out.SiteName = v
	}
	if v, ok := m[keyDefaultPoolMode]; ok {
		out.DefaultPoolMode = v
	}
	if v, ok := m[keySiteDescription]; ok {
		out.SiteDescription = v
	}
	if v, ok := m[keySiteURL]; ok {
		out.SiteURL = v
	}
	if v, ok := m[keySiteLogo]; ok {
		out.SiteLogo = v
	}
	if v, ok := m[keyICPNumber]; ok {
		out.ICPNumber = v
	}
	if v, ok := m[keyContactEmail]; ok {
		out.ContactEmail = v
	}
	if v, ok := m[keyContactGroup]; ok {
		out.ContactGroup = v
	}
	if v, ok := m[keyContactGroupLabel]; ok && strings.TrimSpace(v) != "" {
		out.ContactGroupLabel = v
	}
	if v, ok := m[keyContactGroupQRURL]; ok {
		out.ContactGroupQRURL = v
	}
	if v, ok := m[keyContactDockEnabled]; ok {
		out.ContactDockEnabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyMarketingBannerEnabled]; ok {
		out.MarketingBanner.Enabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyMarketingBannerText]; ok {
		out.MarketingBanner.Text = v
	}
	if v, ok := m[keyMarketingBannerLinkURL]; ok {
		out.MarketingBanner.LinkURL = v
	}
	if v, ok := m[keyAnnouncementEnabled]; ok {
		out.AnnouncementEnabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyAnnouncementDisplayMode]; ok {
		switch strings.TrimSpace(v) {
		case "always", "after_login":
			out.AnnouncementDisplayMode = strings.TrimSpace(v)
		}
	}
	if v, ok := m[keyAnnouncementTitle]; ok {
		out.AnnouncementTitle = v
	}
	if v, ok := m[keyAnnouncementContent]; ok {
		out.AnnouncementContent = v
	}
	if v, ok := m[keyAnnouncements]; ok {
		out.Announcements = parseAnnouncements(v)
	}
	if len(out.Announcements) == 0 && (strings.TrimSpace(out.AnnouncementTitle) != "" || strings.TrimSpace(out.AnnouncementContent) != "") {
		out.Announcements = []Announcement{{
			ID:          "legacy",
			Title:       out.AnnouncementTitle,
			Content:     out.AnnouncementContent,
			Enabled:     true,
			DisplayMode: out.AnnouncementDisplayMode,
		}}
	}
	if v, ok := m[keyEndpointOpenAIResponses]; ok {
		out.EndpointOpenAIResponses = v
	}
	if v, ok := m[keyEndpointOpenAICompat]; ok {
		out.EndpointOpenAICompat = v
	}
	if v, ok := m[keyEndpointAnthropic]; ok {
		out.EndpointAnthropic = v
	}
	if v, ok := m[keyRegistrationEnabled]; ok {
		out.RegistrationEnabled = parseBoolDefault(v, true)
	}
	if v, ok := m[keyRegistrationRequiresApproval]; ok {
		out.RegistrationRequiresApproval = parseBoolDefault(v, false)
	}
	if v, ok := m[keyPaymentAlipayEnabled]; ok {
		out.PaymentAlipayEnabled = parseBoolDefault(v, true)
	}
	if v, ok := m[keySelfRefundEnabled]; ok {
		out.SelfRefundEnabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyRefundAutoApproveEnabled]; ok {
		out.RefundAutoApproveEnabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyRefundAutoApproveMax]; ok {
		out.RefundAutoApproveMax = v
	}
	if v, ok := m[keyAgentCouponsEnabled]; ok {
		out.AgentCouponsEnabled = parseBoolDefault(v, true)
	}
	if v, ok := m[keyStickyTTLSeconds]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			out.StickyTTLSeconds = n
		}
	}
	if v, ok := m[keyStatusPageGroupIDs]; ok {
		out.StatusPageGroupIDs = parseUUIDList(v)
	}
	if v, ok := m[keyStatusPageWindowHours]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 && n <= 168 {
			out.StatusPageWindowHours = n
		}
	}
	if v, ok := m[keyStatusPageUpstreamEnabled]; ok {
		out.StatusPageUpstreamEnabled = parseBoolDefault(v, true)
	}
	if v, ok := m[keyOpsAlertEnabled]; ok {
		out.OpsAlertEnabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyOpsAlertWebhookType]; ok {
		out.OpsAlertWebhookType = normalizeOpsAlertWebhookType(v)
	}
	if v, ok := m[keyOpsAlertWebhookURLEnc]; ok && v != "" {
		out.OpsAlertWebhookURLSet = true
	}
	if v, ok := m[keyOpsAlertWebhookSecretEnc]; ok && v != "" {
		out.OpsAlertWebhookSecretSet = true
	}
	if v, ok := m[keyOpsAlertTimeoutSeconds]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 && n <= 30 {
			out.OpsAlertTimeoutSeconds = n
		}
	}
	if v, ok := m[keyOpsAlertMaxRetries]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= 5 {
			out.OpsAlertMaxRetries = n
		}
	}
	if v, ok := m[keyOpsAlertSilenceSeconds]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 30 && n <= 86400 {
			out.OpsAlertSilenceSeconds = n
		}
	}
	if v, ok := m[keySMTPHost]; ok {
		out.SMTPHost = v
	}
	if v, ok := m[keySMTPPort]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			out.SMTPPort = n
		}
	}
	if v, ok := m[keySMTPUsername]; ok {
		out.SMTPUsername = v
	}
	if v, ok := m[keySMTPPasswordEnc]; ok && v != "" {
		out.SMTPPasswordSet = true
		// SMTPPassword stays empty in the public projection
	}
	if v, ok := m[keySMTPFromEmail]; ok {
		out.SMTPFromEmail = v
	}
	if v, ok := m[keySMTPFromName]; ok {
		out.SMTPFromName = v
	}
	if v, ok := m[keySMTPUseTLS]; ok {
		out.SMTPUseTLS = parseBoolDefault(v, false)
	}
	if v, ok := m[keyReqRegisterVerify]; ok {
		out.RequireRegisterVerification = parseBoolDefault(v, false)
	}
	if v, ok := m[keyReqChangeVerify]; ok {
		out.RequireChangeEmailVerification = parseBoolDefault(v, false)
	}
	if v, ok := m[keyAllowResetPwd]; ok {
		out.AllowPasswordReset = parseBoolDefault(v, false)
	}
	if v, ok := m[keyAllowImageOutput]; ok {
		out.AllowImageOutput = parseBoolDefault(v, false)
	}
	if v, ok := m[keyFormatConversionEnabled]; ok {
		out.FormatConversionEnabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyWebSocketEnabled]; ok {
		out.WebSocketEnabled = parseBoolDefault(v, true)
	}
	if v, ok := m[keyStreamFirstTextTimeoutSeconds]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= 600 {
			out.StreamFirstTextTimeoutSeconds = n
		}
	}
	if v, ok := m[keyStreamTotalTimeoutSeconds]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= 3600 {
			out.StreamTotalTimeoutSeconds = n
		}
	}
	if v, ok := m[keyStreamIdleTimeoutSeconds]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= 600 {
			out.StreamIdleTimeoutSeconds = n
		}
	}
	if v, ok := m[keyModerationEnabled]; ok {
		out.ModerationEnabled = parseBoolDefault(v, false)
	}
	if v, ok := m[keyModerationBaseURL]; ok && strings.TrimSpace(v) != "" {
		out.ModerationBaseURL = strings.TrimSpace(v)
	}
	if v, ok := m[keyModerationModel]; ok && strings.TrimSpace(v) != "" {
		out.ModerationModel = strings.TrimSpace(v)
	}
	if v, ok := m[keyModerationAPIKeyEnc]; ok && v != "" {
		out.ModerationAPIKeySet = true
		// ModerationAPIKey stays empty in the public projection
	}
	if v, ok := m[keyModerationThreshold]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 && f <= 1 {
			out.ModerationThreshold = f
		}
	}
	if v, ok := m[keyModerationSampleRate]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 && f <= 1 {
			out.ModerationSampleRate = f
		}
	}
	if v, ok := m[keyModerationMaxPerMin]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			out.ModerationMaxPerMin = n
		}
	}
	if v, ok := m[keyExchangeCNYPerUSD]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			out.ExchangeCNYPerUSD = f
		}
	}
	if v, ok := m[keyDefaultDisplayCurrency]; ok {
		switch strings.ToUpper(strings.TrimSpace(v)) {
		case "CNY", "USD":
			out.DefaultDisplayCurrency = strings.ToUpper(strings.TrimSpace(v))
		}
	}
	if v, ok := m[keyMaxIncentivePct]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 && f <= 100 {
			out.MaxIncentivePct = f
		}
	}
	if v, ok := m[keyTopupPresets]; ok {
		var presets []int
		if json.Unmarshal([]byte(v), &presets) == nil && validTopupPresets(presets) == nil {
			out.TopupPresets = presets
		}
	}
	if v, ok := m[keyAgentPlanRefundDayDivisor]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 && n <= maxAgentPlanRefundDayDivisor {
			out.AgentPlanRefundDayDivisor = n
		}
	}
	if v, ok := m[keyAgentPlanRefundRatePct]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 && f <= 100 {
			out.AgentPlanRefundRatePct = f
		}
	}
	if v, ok := m[keyAgentBalanceRefundRatePct]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 && f <= 100 {
			out.AgentBalanceRefundRatePct = f
		}
	}
	if v, ok := m[keyReasoningParseRules]; ok {
		// Key present = the admin has saved a config at least once; an
		// empty stored list intentionally disables extraction. A parse
		// error falls back to the defaults set above.
		var rules []ReasoningParseRule
		if err := json.Unmarshal([]byte(strings.TrimSpace(v)), &rules); err == nil && rules != nil {
			if normalized, err := NormalizeReasoningParseRules(rules); err == nil {
				out.ReasoningParseRules = normalized
			}
		}
	}
	invite := parseInviteProgram(m[keyInviteProgram])
	out.InviteProgram = &invite
	return &out, nil
}

// GetSMTPSecret returns the decrypted SMTP password if one is set.
// Returns ("", nil) when no password configured; ("", err) when decrypt
// fails (likely a key rotation issue — admin must re-enter the
// password). NEVER include the return value in any HTTP response.
func (s *SettingsSvc) GetSMTPSecret(ctx context.Context) (string, error) {
	if s.cipher == nil {
		return "", errors.New("settings: cipher not configured")
	}
	row, err := s.d.Ent.SiteSetting.Query().
		Where(entsetting.KeyEQ(keySMTPPasswordEnc)).
		Only(ctx)
	if err != nil {
		// Treat missing row as "no password" — common on first install.
		return "", nil
	}
	if row.Value == "" {
		return "", nil
	}
	pt, err := s.cipher.Decrypt(row.Value)
	if err != nil {
		return "", err
	}
	return pt, nil
}

func parseBoolDefault(v string, def bool) bool {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

func parseUUIDList(v string) []uuid.UUID {
	v = strings.TrimSpace(v)
	if v == "" || v == "null" {
		return nil
	}
	var raw []string
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil
	}
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

func parseAnnouncements(v string) []Announcement {
	v = strings.TrimSpace(v)
	if v == "" || v == "null" {
		return nil
	}
	var raw []Announcement
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil
	}
	return normalizeAnnouncements(raw)
}

func normalizeAnnouncements(in []Announcement) []Announcement {
	out := make([]Announcement, 0, len(in))
	for _, a := range in {
		a.ID = strings.TrimSpace(a.ID)
		if a.ID == "" {
			a.ID = uuid.NewString()
		}
		a.DisplayMode = strings.TrimSpace(a.DisplayMode)
		if a.DisplayMode != "after_login" {
			a.DisplayMode = "always"
		}
		out = append(out, a)
	}
	return out
}

// UpdateSite upserts every field. Empty strings are persisted (admin may
// intentionally clear a value). SMTPPassword: an empty value KEEPS the
// previous password (it's a no-op), a non-empty value re-encrypts and
// replaces it. Use SetSMTPPassword(ctx, "") to explicitly clear.
// ErrInvalidSiteSettings marks admin input the settings form must fix
// (HTTP 400) rather than a storage failure.
var ErrInvalidSiteSettings = errors.New("invalid site settings")

// contactTextOK accepts a short single-line display string.
// maxTopupPresets / maxTopupPreset bound the top-up choices.
const (
	maxTopupPresets = 12
	maxTopupPreset  = 1_000_000
)

func validTopupPresets(presets []int) error {
	if len(presets) > maxTopupPresets {
		return fmt.Errorf("%w: topup_presets allows at most %d amounts", ErrInvalidSiteSettings, maxTopupPresets)
	}
	for _, p := range presets {
		if p < 1 || p > maxTopupPreset {
			return fmt.Errorf("%w: topup_presets amounts must be within [1, %d]", ErrInvalidSiteSettings, maxTopupPreset)
		}
	}
	return nil
}

func contactTextOK(s string, maxRunes int) bool {
	if utf8.RuneCountInString(s) > maxRunes {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (s *SettingsSvc) UpdateSite(ctx context.Context, in SiteSettings) error {
	// Account health policy: an unknown mode would silently fall back to
	// balanced, so reject it instead of storing garbage.
	mode, ok := ParsePoolMode(in.DefaultPoolMode)
	if !ok {
		return fmt.Errorf("default_pool_mode must be balanced, conservative, lenient or observe")
	}
	if mode == "" {
		mode = PoolModeBalanced
	}
	in.DefaultPoolMode = string(mode)
	// Rejected rather than clamped: a client that omits the field would
	// otherwise zero every agent's budget.
	if in.MaxIncentivePct <= 0 || in.MaxIncentivePct > 100 {
		return fmt.Errorf("%w: max_incentive_pct must be within (0, 100]", ErrInvalidSiteSettings)
	}
	// The divisor also catches a client that omits the agent refund terms:
	// its zero value is rejected instead of turning every refund into 0.
	if in.AgentPlanRefundDayDivisor < 1 || in.AgentPlanRefundDayDivisor > maxAgentPlanRefundDayDivisor {
		return fmt.Errorf("%w: agent_plan_refund_day_divisor must be within [1, %d]", ErrInvalidSiteSettings, maxAgentPlanRefundDayDivisor)
	}
	if in.TopupPresets != nil {
		if err := validTopupPresets(in.TopupPresets); err != nil {
			return err
		}
	}
	if in.AgentPlanRefundRatePct < 0 || in.AgentPlanRefundRatePct > 100 {
		return fmt.Errorf("%w: agent_plan_refund_rate_pct must be within [0, 100]", ErrInvalidSiteSettings)
	}
	if in.AgentBalanceRefundRatePct < 0 || in.AgentBalanceRefundRatePct > 100 {
		return fmt.Errorf("%w: agent_balance_refund_rate_pct must be within [0, 100]", ErrInvalidSiteSettings)
	}
	in.RefundAutoApproveMax = strings.TrimSpace(in.RefundAutoApproveMax)
	if _, ok := in.RefundAutoApproveCap(); !ok {
		return fmt.Errorf("%w: refund_auto_approve_max must be a non-negative amount", ErrInvalidSiteSettings)
	}
	in.ContactGroup = strings.TrimSpace(in.ContactGroup)
	if !contactTextOK(in.ContactGroup, 64) {
		return fmt.Errorf("%w: contact_group must be a single line of at most 64 characters", ErrInvalidSiteSettings)
	}
	in.ContactGroupLabel = strings.TrimSpace(in.ContactGroupLabel)
	if in.ContactGroupLabel == "" {
		in.ContactGroupLabel = siteDefaults.ContactGroupLabel
	}
	if !contactTextOK(in.ContactGroupLabel, 32) {
		return fmt.Errorf("%w: contact_group_label must be a single line of at most 32 characters", ErrInvalidSiteSettings)
	}
	in.ContactGroupQRURL = strings.TrimSpace(in.ContactGroupQRURL)
	if u := in.ContactGroupQRURL; u != "" && !strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("%w: contact_group_qr_url must be an http(s) URL or a site path", ErrInvalidSiteSettings)
	}
	groupIDs, _ := json.Marshal(uuidStrings(in.StatusPageGroupIDs))
	if in.StatusPageWindowHours < 1 {
		in.StatusPageWindowHours = siteDefaults.StatusPageWindowHours
	}
	if in.StatusPageWindowHours > 168 {
		in.StatusPageWindowHours = 168
	}
	in.OpsAlertWebhookType = normalizeOpsAlertWebhookType(in.OpsAlertWebhookType)
	if in.OpsAlertTimeoutSeconds < 1 || in.OpsAlertTimeoutSeconds > 30 {
		in.OpsAlertTimeoutSeconds = siteDefaults.OpsAlertTimeoutSeconds
	}
	if in.OpsAlertMaxRetries < 0 || in.OpsAlertMaxRetries > 5 {
		in.OpsAlertMaxRetries = siteDefaults.OpsAlertMaxRetries
	}
	if in.OpsAlertSilenceSeconds < 30 || in.OpsAlertSilenceSeconds > 86400 {
		in.OpsAlertSilenceSeconds = siteDefaults.OpsAlertSilenceSeconds
	}
	if in.StreamFirstTextTimeoutSeconds < 0 {
		in.StreamFirstTextTimeoutSeconds = siteDefaults.StreamFirstTextTimeoutSeconds
	}
	if in.StreamFirstTextTimeoutSeconds > 600 {
		in.StreamFirstTextTimeoutSeconds = 600
	}
	if in.StreamTotalTimeoutSeconds < 0 {
		in.StreamTotalTimeoutSeconds = siteDefaults.StreamTotalTimeoutSeconds
	}
	if in.StreamTotalTimeoutSeconds > 3600 {
		in.StreamTotalTimeoutSeconds = 3600
	}
	if in.StreamIdleTimeoutSeconds < 0 {
		in.StreamIdleTimeoutSeconds = siteDefaults.StreamIdleTimeoutSeconds
	}
	if in.StreamIdleTimeoutSeconds > 600 {
		in.StreamIdleTimeoutSeconds = 600
	}
	announcementMode := strings.TrimSpace(in.AnnouncementDisplayMode)
	if announcementMode != "after_login" {
		announcementMode = "always"
	}
	announcements := normalizeAnnouncements(in.Announcements)
	if len(announcements) == 0 && (strings.TrimSpace(in.AnnouncementTitle) != "" || strings.TrimSpace(in.AnnouncementContent) != "") {
		announcements = []Announcement{{
			ID:          "legacy",
			Title:       in.AnnouncementTitle,
			Content:     in.AnnouncementContent,
			Enabled:     true,
			DisplayMode: announcementMode,
		}}
	}
	if len(announcements) > 0 {
		in.AnnouncementTitle = announcements[0].Title
		in.AnnouncementContent = announcements[0].Content
		announcementMode = announcements[0].DisplayMode
	} else {
		in.AnnouncementTitle = ""
		in.AnnouncementContent = ""
	}
	announcementsJSON, _ := json.Marshal(announcements)
	pairs := map[string]string{
		keySiteName:                      in.SiteName,
		keyDefaultPoolMode:               string(mode),
		keySiteDescription:               in.SiteDescription,
		keySiteURL:                       in.SiteURL,
		keySiteLogo:                      in.SiteLogo,
		keyICPNumber:                     in.ICPNumber,
		keyContactEmail:                  in.ContactEmail,
		keyContactGroup:                  in.ContactGroup,
		keyContactGroupLabel:             in.ContactGroupLabel,
		keyContactGroupQRURL:             in.ContactGroupQRURL,
		keyContactDockEnabled:            strconv.FormatBool(in.ContactDockEnabled),
		keyMarketingBannerEnabled:        strconv.FormatBool(in.MarketingBanner.Enabled),
		keyMarketingBannerText:           in.MarketingBanner.Text,
		keyMarketingBannerLinkURL:        in.MarketingBanner.LinkURL,
		keyAnnouncementEnabled:           strconv.FormatBool(in.AnnouncementEnabled),
		keyAnnouncementDisplayMode:       announcementMode,
		keyAnnouncementTitle:             in.AnnouncementTitle,
		keyAnnouncementContent:           in.AnnouncementContent,
		keyAnnouncements:                 string(announcementsJSON),
		keyEndpointOpenAIResponses:       in.EndpointOpenAIResponses,
		keyEndpointOpenAICompat:          in.EndpointOpenAICompat,
		keyEndpointAnthropic:             in.EndpointAnthropic,
		keyRegistrationEnabled:           strconv.FormatBool(in.RegistrationEnabled),
		keyRegistrationRequiresApproval:  strconv.FormatBool(in.RegistrationRequiresApproval),
		keyPaymentAlipayEnabled:          strconv.FormatBool(in.PaymentAlipayEnabled),
		keySelfRefundEnabled:             strconv.FormatBool(in.SelfRefundEnabled),
		keyRefundAutoApproveEnabled:      strconv.FormatBool(in.RefundAutoApproveEnabled),
		keyRefundAutoApproveMax:          in.RefundAutoApproveMax,
		keyAgentCouponsEnabled:           strconv.FormatBool(in.AgentCouponsEnabled),
		keyStickyTTLSeconds:              strconv.Itoa(in.StickyTTLSeconds),
		keyStatusPageGroupIDs:            string(groupIDs),
		keyStatusPageWindowHours:         strconv.Itoa(in.StatusPageWindowHours),
		keyStatusPageUpstreamEnabled:     strconv.FormatBool(in.StatusPageUpstreamEnabled),
		keyOpsAlertEnabled:               strconv.FormatBool(in.OpsAlertEnabled),
		keyOpsAlertWebhookType:           in.OpsAlertWebhookType,
		keyOpsAlertTimeoutSeconds:        strconv.Itoa(in.OpsAlertTimeoutSeconds),
		keyOpsAlertMaxRetries:            strconv.Itoa(in.OpsAlertMaxRetries),
		keyOpsAlertSilenceSeconds:        strconv.Itoa(in.OpsAlertSilenceSeconds),
		keySMTPHost:                      in.SMTPHost,
		keySMTPPort:                      strconv.Itoa(in.SMTPPort),
		keySMTPUsername:                  in.SMTPUsername,
		keySMTPFromEmail:                 in.SMTPFromEmail,
		keySMTPFromName:                  in.SMTPFromName,
		keySMTPUseTLS:                    strconv.FormatBool(in.SMTPUseTLS),
		keyReqRegisterVerify:             strconv.FormatBool(in.RequireRegisterVerification),
		keyReqChangeVerify:               strconv.FormatBool(in.RequireChangeEmailVerification),
		keyAllowResetPwd:                 strconv.FormatBool(in.AllowPasswordReset),
		keyAllowImageOutput:              strconv.FormatBool(in.AllowImageOutput),
		keyFormatConversionEnabled:       strconv.FormatBool(in.FormatConversionEnabled),
		keyWebSocketEnabled:              strconv.FormatBool(in.WebSocketEnabled),
		keyStreamFirstTextTimeoutSeconds: strconv.Itoa(in.StreamFirstTextTimeoutSeconds),
		keyStreamTotalTimeoutSeconds:     strconv.Itoa(in.StreamTotalTimeoutSeconds),
		keyStreamIdleTimeoutSeconds:      strconv.Itoa(in.StreamIdleTimeoutSeconds),

		keyModerationEnabled:    strconv.FormatBool(in.ModerationEnabled),
		keyModerationBaseURL:    strings.TrimSpace(in.ModerationBaseURL),
		keyModerationModel:      strings.TrimSpace(in.ModerationModel),
		keyModerationThreshold:  strconv.FormatFloat(clampUnit(in.ModerationThreshold, siteDefaults.ModerationThreshold), 'f', -1, 64),
		keyModerationSampleRate: strconv.FormatFloat(clampUnit(in.ModerationSampleRate, siteDefaults.ModerationSampleRate), 'f', -1, 64),
		keyModerationMaxPerMin:  strconv.Itoa(in.ModerationMaxPerMin),

		keyExchangeCNYPerUSD:      strconv.FormatFloat(in.ExchangeCNYPerUSD, 'f', -1, 64),
		keyDefaultDisplayCurrency: strings.ToUpper(strings.TrimSpace(in.DefaultDisplayCurrency)),
		keyMaxIncentivePct:        strconv.FormatFloat(in.MaxIncentivePct, 'f', -1, 64),

		keyAgentPlanRefundDayDivisor: strconv.Itoa(in.AgentPlanRefundDayDivisor),
		keyAgentPlanRefundRatePct:    strconv.FormatFloat(in.AgentPlanRefundRatePct, 'f', -1, 64),
		keyAgentBalanceRefundRatePct: strconv.FormatFloat(in.AgentBalanceRefundRatePct, 'f', -1, 64),
	}
	if in.TopupPresets != nil {
		presets, _ := json.Marshal(in.TopupPresets)
		pairs[keyTopupPresets] = string(presets)
	}
	// ReasoningParseRules: nil = leave stored config untouched (the admin
	// panel always sends the full list, including [] to disable).
	if in.ReasoningParseRules != nil {
		rules, err := NormalizeReasoningParseRules(in.ReasoningParseRules)
		if err != nil {
			return err
		}
		rulesJSON, _ := json.Marshal(rules)
		pairs[keyReasoningParseRules] = string(rulesJSON)
	}
	// InviteProgram: nil = leave stored config untouched. Invalid values
	// reject the whole update — a mistyped rebate rate is real money.
	program := in.InviteProgram
	if program == nil {
		cur, err := s.GetSite(ctx)
		if err != nil {
			return err
		}
		program = cur.InviteProgram
	}
	if program != nil {
		normalized, err := NormalizeInviteProgram(*program)
		if err != nil {
			return err
		}
		if normalized.TopupRebateRatePct.GreaterThan(decimal.NewFromFloat(in.MaxIncentivePct)) {
			return fmt.Errorf("%w: invite topup_rebate_rate_pct must not exceed max_incentive_pct", ErrInvalidSiteSettings)
		}
		if in.InviteProgram != nil {
			programJSON, _ := json.Marshal(normalized)
			pairs[keyInviteProgram] = string(programJSON)
		}
	}
	for k, v := range pairs {
		err := s.d.Ent.SiteSetting.Create().
			SetKey(k).SetValue(v).
			OnConflictColumns(entsetting.FieldKey).
			UpdateNewValues().
			Exec(ctx)
		if err != nil {
			return err
		}
	}
	// SMTP password is the one field that's "write-only on present":
	// empty input means "don't change" so the admin UI can submit
	// without re-typing the password every time.
	if in.SMTPPassword != "" {
		if err := s.SetSMTPPassword(ctx, in.SMTPPassword); err != nil {
			return err
		}
	}
	// Moderation API key: same write-only-on-present rule as SMTP password.
	if in.ModerationAPIKey != "" {
		if err := s.SetModerationAPIKey(ctx, in.ModerationAPIKey); err != nil {
			return err
		}
	}
	// Webhook URL and signing secret are write-only-on-present. This keeps an
	// unrelated settings save from clearing credentials the UI cannot read.
	if in.OpsAlertWebhookURL != "" {
		if err := s.setEncryptedSetting(ctx, keyOpsAlertWebhookURLEnc, in.OpsAlertWebhookURL); err != nil {
			return err
		}
	}
	if in.OpsAlertWebhookSecret != "" {
		if err := s.setEncryptedSetting(ctx, keyOpsAlertWebhookSecretEnc, in.OpsAlertWebhookSecret); err != nil {
			return err
		}
	}
	if st, err := s.loadSite(ctx); err == nil {
		s.storeSite(st)
	}
	return nil
}

// SetSMTPPassword writes the encrypted SMTP password. Pass "" to clear.
func (s *SettingsSvc) SetSMTPPassword(ctx context.Context, plaintext string) error {
	if s.cipher == nil {
		return errors.New("settings: cipher not configured")
	}
	var stored string
	if plaintext != "" {
		ct, err := s.cipher.Encrypt(plaintext)
		if err != nil {
			return err
		}
		stored = ct
	}
	err := s.d.Ent.SiteSetting.Create().
		SetKey(keySMTPPasswordEnc).SetValue(stored).
		OnConflictColumns(entsetting.FieldKey).
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		return err
	}
	if st, err := s.loadSite(ctx); err == nil {
		s.storeSite(st)
	}
	return nil
}

// SetModerationAPIKey writes the encrypted moderation API key. Pass "" to clear.
func (s *SettingsSvc) SetModerationAPIKey(ctx context.Context, plaintext string) error {
	if s.cipher == nil {
		return errors.New("settings: cipher not configured")
	}
	var stored string
	if plaintext != "" {
		ct, err := s.cipher.Encrypt(plaintext)
		if err != nil {
			return err
		}
		stored = ct
	}
	err := s.d.Ent.SiteSetting.Create().
		SetKey(keyModerationAPIKeyEnc).SetValue(stored).
		OnConflictColumns(entsetting.FieldKey).
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		return err
	}
	if st, err := s.loadSite(ctx); err == nil {
		s.storeSite(st)
	}
	return nil
}

// SetOpsAlertWebhookURL and SetOpsAlertWebhookSecret support explicit
// credential clearing. Normal UpdateSite calls preserve empty inputs.
func (s *SettingsSvc) SetOpsAlertWebhookURL(ctx context.Context, plaintext string) error {
	return s.setEncryptedSetting(ctx, keyOpsAlertWebhookURLEnc, strings.TrimSpace(plaintext))
}

func (s *SettingsSvc) SetOpsAlertWebhookSecret(ctx context.Context, plaintext string) error {
	return s.setEncryptedSetting(ctx, keyOpsAlertWebhookSecretEnc, plaintext)
}

func (s *SettingsSvc) setEncryptedSetting(ctx context.Context, key, plaintext string) error {
	if s.cipher == nil {
		return errors.New("settings: cipher not configured")
	}
	stored, err := s.cipher.Encrypt(plaintext)
	if err != nil {
		return err
	}
	if err := s.d.Ent.SiteSetting.Create().
		SetKey(key).SetValue(stored).
		OnConflictColumns(entsetting.FieldKey).
		UpdateNewValues().
		Exec(ctx); err != nil {
		return err
	}
	if st, err := s.loadSite(ctx); err == nil {
		s.storeSite(st)
	}
	return nil
}

// GetOpsAlertConfig returns runtime-only decrypted delivery configuration.
// Callers must never serialize or log this object.
func (s *SettingsSvc) GetOpsAlertConfig(ctx context.Context) (OpsAlertConfig, error) {
	st, err := s.GetSite(ctx)
	if err != nil {
		return OpsAlertConfig{}, err
	}
	cfg := OpsAlertConfig{
		Enabled:       st.OpsAlertEnabled,
		WebhookType:   normalizeOpsAlertWebhookType(st.OpsAlertWebhookType),
		Timeout:       time.Duration(st.OpsAlertTimeoutSeconds) * time.Second,
		MaxRetries:    st.OpsAlertMaxRetries,
		SilenceWindow: time.Duration(st.OpsAlertSilenceSeconds) * time.Second,
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	if s.cipher == nil {
		return OpsAlertConfig{}, errors.New("settings: cipher not configured")
	}
	rows, err := s.d.Ent.SiteSetting.Query().
		Where(entsetting.KeyIn(keyOpsAlertWebhookURLEnc, keyOpsAlertWebhookSecretEnc)).
		All(ctx)
	if err != nil {
		return OpsAlertConfig{}, err
	}
	for _, row := range rows {
		if row.Value == "" {
			continue
		}
		plaintext, err := s.cipher.Decrypt(row.Value)
		if err != nil {
			return OpsAlertConfig{}, err
		}
		switch row.Key {
		case keyOpsAlertWebhookURLEnc:
			cfg.WebhookURL = strings.TrimSpace(plaintext)
		case keyOpsAlertWebhookSecretEnc:
			cfg.SigningSecret = plaintext
		}
	}
	return cfg, nil
}

// GetModerationSecret returns the decrypted moderation API key, or ("", nil)
// when none is set. NEVER include the return value in any HTTP response.
func (s *SettingsSvc) GetModerationSecret(ctx context.Context) (string, error) {
	if s.cipher == nil {
		return "", errors.New("settings: cipher not configured")
	}
	row, err := s.d.Ent.SiteSetting.Query().
		Where(entsetting.KeyEQ(keyModerationAPIKeyEnc)).
		Only(ctx)
	if err != nil {
		return "", nil // missing row = no key
	}
	if row.Value == "" {
		return "", nil
	}
	return s.cipher.Decrypt(row.Value)
}

// clampUnit bounds a 0..1 ratio; out-of-range falls back to def.
func clampUnit(v, def float64) float64 {
	if v < 0 || v > 1 {
		return def
	}
	return v
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}
