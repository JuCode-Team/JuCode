// Command jucode starts the JuCode HTTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // server.timezone must load without the host's zoneinfo

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/api"
	apioauth "github.com/jucode/jucode/internal/api/oauth"
	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/conf"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/llm"
	"github.com/jucode/jucode/internal/payments/alipay"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	jwtpkg "github.com/jucode/jucode/internal/pkg/jwt"
	"github.com/jucode/jucode/internal/pkg/trustedproxy"
	regionpkg "github.com/jucode/jucode/internal/region"
	regioncontrol "github.com/jucode/jucode/internal/region/control"
	"github.com/jucode/jucode/internal/relay"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "configs/config.yaml", "path to config yaml")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := conf.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(2)
	}

	if err := biz.SetTimezone(cfg.Server.Timezone); err != nil {
		slog.Error("timezone init failed", "err", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := data.New(ctx, cfg)
	if err != nil {
		slog.Error("data init failed", "err", err)
		os.Exit(1)
	}
	defer d.Close()

	cipher, err := cryptopkg.New(cfg.EncryptKey)
	if err != nil {
		slog.Error("cipher init failed", "err", err)
		os.Exit(2)
	}

	signer := jwtpkg.NewSigner(cfg.JWT.Secret, cfg.JWT.AccessTTL, cfg.JWT.RefreshTTL)
	dashboardSvc := biz.NewDashboardSvc(d)
	modelSvc := biz.NewModelSvc(d)
	// SettingsSvc owns the currency rate cache that PlanSvc / RedeemSvc /
	// BillingSvc all read from on the hot path. Construct it first and
	// start the background refresh so the cache is warm by the time the
	// gateway starts taking traffic.
	drainSvc := biz.NewDrainSvc()
	settingsSvc := biz.NewSettingsSvc(d)
	settingsSvc.SetCipher(cipher) // enables encrypted SMTP and webhook secret storage
	settingsSvc.StartCurrencyRateRefresh(ctx)
	opsAlertSvc := biz.NewOpsAlertSvc(d, settingsSvc, slog.Default())
	planSvc := biz.NewPlanSvc(d, settingsSvc)
	redeemSvc := biz.NewRedeemSvc(d, planSvc, settingsSvc)
	statsSvc := biz.NewStatsSvc(d)
	statsSvc.EnableHourBuckets(cfg.UsageLog.HourBucketsEnabled)
	emailSvc := biz.NewEmailSvc(settingsSvc, slog.Default())
	verifySvc := biz.NewVerificationSvc(d, settingsSvc, emailSvc)
	billingSvc := biz.NewBillingSvc(d, planSvc, settingsSvc)
	billingSvc.SetOpsAlerts(opsAlertSvc)
	if cfg.UsageLog.BatchEnabled {
		usageWriter := biz.NewUsageLogWriter(d, cfg.UsageLog, slog.Default())
		billingSvc.SetUsageLogWriter(usageWriter)
		usageWriter.Start(ctx)
		slog.Info("usage log batch writer started",
			"buffer_size", cfg.UsageLog.BufferSize,
			"flush_max_rows", cfg.UsageLog.FlushMaxRows,
			"flush_max_age", cfg.UsageLog.FlushMaxAge.String())
		// Deferred here so it runs before d.Close() (registered earlier, so
		// it unwinds last) — the writer needs a live DB handle to drain.
		defer usageWriter.Stop()
	}
	// Pool-throttle: wired here so BillingSvc.ApplyPoolThrottle can read
	// the live snapshot on the request hot path without an extra setter
	// dance later. The monitor goroutine starts further down once
	// budgetSvc is also constructed.
	// (BillingSvc.SetPoolThrottle is no-op-safe so the line below can
	// move once budgetSvc is initialized.)
	concurGate := biz.NewConcurrencyGate(d, planSvc, settingsSvc)
	stickySvc := biz.NewStickySvc(d, settingsSvc)
	statusSvc := biz.NewStatusSvc(d, settingsSvc)
	statusSvc.SetOpsAlerts(opsAlertSvc)
	systemIncidentSvc := biz.NewSystemIncidentSvc(d)
	slaMonitor := biz.NewSLAMonitorSvc(d, settingsSvc, systemIncidentSvc, slog.Default())
	policySvc := biz.NewPolicySvc(d)
	if err := policySvc.Bootstrap(ctx); err != nil {
		slog.Warn("policy.bootstrap failed; gateway will start with empty rules", "err", err.Error())
	}
	incidentSvc := biz.NewIncidentSvc(d)
	auditSvc := biz.NewAuditSvc(d)
	ipPolicySvc := biz.NewIPPolicySvc(d)
	ipPolicySvc.StartRefreshLoop(ctx)
	roleSvc := biz.NewRoleSvc(d)
	ticketSvc := biz.NewTicketSvc(d, roleSvc, settingsSvc, emailSvc, slog.Default())
	ticketSvc.SetAttachmentDir(cfg.Server.TicketAttachmentDir)
	if err := ticketSvc.EnsureTicketNumbers(ctx); err != nil {
		slog.Warn("ticket number backfill skipped", "err", err)
	}
	if err := ticketSvc.EnsureUnreadStamps(ctx); err != nil {
		slog.Warn("ticket unread backfill skipped", "err", err)
	}
	agentSvc := biz.NewAgentSvc(d, settingsSvc)
	redeemSvc.SetAgentSvc(agentSvc) // closes the dependency cycle: redeem → agent
	financeSvc := biz.NewFinanceSvc(d, settingsSvc)
	alipayClient, err := alipay.New(cfg.Payments.Alipay)
	if err != nil {
		slog.Error("alipay init failed", "err", err)
		os.Exit(2)
	}
	alipayAdapter := alipay.NewFinanceAdapter(alipayClient)
	financeSvc.SetAlipay(alipayAdapter)
	if alipayAdapter != nil {
		financeSvc.SetRefundGateway("alipay", alipayAdapter)
	}
	organizationSvc := biz.NewOrganizationSvc(d)
	cacheSvc := cache.New(d.Redis)
	gateway := llm.NewGateway(modelSvc, billingSvc, concurGate, stickySvc, policySvc, incidentSvc, settingsSvc, cipher, cfg.LLM.MaxAttempts, cfg.Server.MaxBodyBytes, cfg.LLM.RequestTimeout)
	// The account pool is the routing entity: it owns every upstream endpoint,
	// API key, health signal and scheduling input.
	accountPoolSvc := biz.NewAccountPoolSvc(d, cipher, settingsSvc)
	gateway.SetAccounts(accountPoolSvc)
	// Web tools (/tools/v1): engine prices and switches, vendor account choice.
	webToolSvc := biz.NewWebToolSvc(d)
	gateway.SetWebTools(webToolSvc)
	// Provider definitions carry the per-vendor payload (endpoint shape,
	// credential label, models). Definitions are optional: with none
	// configured accounts are created by hand.
	definitionSvc := biz.NewProviderDefinitionSvc(d)
	accountPoolSvc.SetDefinitions(definitionSvc)
	// Public model catalog (models.dev): cached vendor/model metadata —
	// display names, context limits, reference pricing. Feeds the admin
	// catalog browser and supplements sync'd models' display costs.
	remoteCatalogSvc := biz.NewRemoteCatalogSvc()
	accountPoolSvc.SetRemoteCatalog(remoteCatalogSvc)
	// Async generation (video) task store — enables POST/GET /v1/videos.
	gateway.SetTaskSvc(biz.NewTaskSvc(d))
	rateLimitMode := biz.ParseRateLimitMode(cfg.LLM.RateLimitMode)
	gateway.SetRateLimitGate(biz.NewRateLimitGate(d, planSvc, rateLimitMode, slog.Default()))
	if rateLimitMode != biz.RateLimitOff {
		slog.Info("per-user RPM/TPM caps active", "mode", string(rateLimitMode))
	}

	// Automated sanction engine + async LLM moderation. Moderation runs off
	// the hot path and is the only path to account freezes/bans.
	sanctionSvc := biz.NewSanctionSvc(d, d.Redis, auditSvc, slog.Default())
	moderationSvc := biz.NewModerationSvc(settingsSvc, d.Redis, incidentSvc, sanctionSvc, slog.Default())
	gateway.SetModeration(moderationSvc)

	upstreamStatusSvc := biz.NewUpstreamStatusSvc(d, slog.Default())

	// Hoisted out of api.Deps below because the status-page prober needs it too
	// — it authenticates to our own gateway with a stored key, so it needs
	// Reveal() and therefore the cipher.
	apiKeySvc := biz.NewApiKeySvc(d)
	apiKeySvc.SetCipher(cipher)
	modelProbeSvc := biz.NewModelProbeSvc(d, apiKeySvc, cfg.Server.Listen, slog.Default())

	var regionControl *regioncontrol.Handler
	var regionLedger *regioncontrol.SQLLedger
	if cfg.Region.Enabled {
		transportCipher, cipherErr := cryptopkg.New(cfg.Region.SyncEncryptKey)
		if cipherErr != nil {
			slog.Error("regional control cipher init failed", "err", cipherErr)
			os.Exit(2)
		}
		regionLedger = regioncontrol.NewSQLLedger(d.DB(), regioncontrol.FixedLeasePolicy{
			MaxCNY:       decimal.RequireFromString(cfg.Region.LeaseCNY),
			OverdraftCNY: decimal.RequireFromString(cfg.Region.OverdraftCNY),
			TTL:          cfg.Region.LeaseTTL,
		})
		edgeTokens := regioncontrol.TokenAuth{}
		for _, edge := range cfg.Region.Edges {
			edgeTokens[edge.ServiceToken] = edge.ID
		}
		regionControl = regioncontrol.NewHandler(
			edgeTokens,
			regionSnapshots{pool: accountPoolSvc, keys: apiKeySvc, cipher: transportCipher},
			regionLedger,
		)
		slog.Warn("region control plane enabled: this feature is EXPERIMENTAL and its protocol and tables may change without notice",
			"edges", len(cfg.Region.Edges))
	}

	leader := biz.NewLeaderRunner(d.Redis, "jucode:leader:background", 30*time.Second, slog.Default())
	leader.Start(ctx, func(leaderCtx context.Context) {
		slog.Info("background leader loops starting")
		// Synthetic (group, model) probing for the public status page. Leader-only
		// because every tick is a billed upstream call — running it on both
		// instances would double the spend and file duplicate observations.
		modelProbeSvc.Start(leaderCtx)
		// Evaluate derived status-incident open/close edges even when the public
		// status endpoint has no traffic. Disabled config only reads the cache.
		go statusSvc.StartAlertLoop(leaderCtx, time.Minute)
		go slaMonitor.StartLoop(leaderCtx, time.Minute)
		// Gradually restore user priority decremented by abuse incidents:
		// +10/hour for users with no new policy/cyber incident in 24h.
		go incidentSvc.StartRecoveryLoop(leaderCtx, time.Hour, 10)
		// Drive async generation (video) tasks to completion + settle billing
		// even when clients stop polling.
		go gateway.RunTaskReconciler(leaderCtx, 30*time.Second)
		// Mirror the OpenAI/Anthropic status pages into Redis for
		// /public/status. Leader-only so a/b don't double the request
		// rate against the vendors for identical data.
		upstreamStatusSvc.Start(leaderCtx)
		if regionLedger != nil {
			regionLedger.StartCleanup(leaderCtx, 5*time.Minute)
		}
		// Return expired billing holds (crash safety for the reserve/settle
		// billing protocol — see docs/billing-reserve-settle.md).
		billingSvc.StartHoldCleanup(leaderCtx, 5*time.Minute)
		// Recompute account health and probe stale quota snapshots.
		go accountPoolSvc.StartMaintenanceLoop(leaderCtx, 5*time.Minute)
		<-leaderCtx.Done()
		slog.Info("background leader loops stopped")
	})

	accountSvc := biz.NewAccount(d, signer, settingsSvc)
	accountSvc.SetVerification(verifySvc)
	oauthSvc := biz.NewOAuthSvc(d, signer)
	// The account's computers and the relay that reaches them (/v1/relay).
	computerSvc := biz.NewComputerSvc(d)
	relaySrv := relay.New(relay.DefaultConfig(), computerSvc, slog.Default())
	computerSvc.SetKick(relaySrv.Kick)
	oauthSvc.SetOnRevoke(relaySrv.KickAuthz)

	// Invite/referral program. Late-wired into every service that can
	// trigger a bind, a signup re-evaluation, or a consumption rebate.
	inviteSvc := biz.NewInviteSvc(d, settingsSvc)
	accountSvc.SetInvites(inviteSvc)
	financeSvc.SetInvites(inviteSvc)
	financeSvc.SetAgents(agentSvc)
	billingSvc.SetInvites(inviteSvc)
	redeemSvc.SetInvites(inviteSvc)

	deps := api.Deps{
		Data:                d,
		Signer:              signer,
		Account:             accountSvc,
		ApiKeySvc:           apiKeySvc,
		OAuth:               oauthSvc,
		Dashboard:           dashboardSvc,
		Models:              modelSvc,
		Plans:               planSvc,
		Redeem:              redeemSvc,
		Stats:               statsSvc,
		Settings:            settingsSvc,
		Status:              statusSvc,
		SystemIncidents:     systemIncidentSvc,
		Policy:              policySvc,
		Incidents:           incidentSvc,
		Audit:               auditSvc,
		Billing:             billingSvc,
		Gateway:             gateway,
		IPPolicy:            ipPolicySvc,
		Roles:               roleSvc,
		Tickets:             ticketSvc,
		Agents:              agentSvc,
		Finance:             financeSvc,
		Alipay:              alipayAdapter,
		Organizations:       organizationSvc,
		Cache:               cacheSvc,
		Drain:               drainSvc,
		Invites:             inviteSvc,
		AccountPool:         accountPoolSvc,
		WebTools:            webToolSvc,
		ProviderDefinitions: definitionSvc,
		RemoteCatalog:       remoteCatalogSvc,
		Computers:           computerSvc,
		Relay:               relaySrv,
		SiteAssetDir:        filepath.Join(cfg.Server.TicketAttachmentDir, "site-assets"),
		RegionControl:       regionControl,

		OAuthClients: apioauth.Clients{
			IDs:             cfg.OAuth.Clients,
			WebClientID:     cfg.OAuth.WebClientID,
			WebRedirectURIs: cfg.OAuth.WebRedirectURIs,
		},
	}

	// Resolve trusted proxies before building the router. CF v4+v6
	// fetched live; ExtraTrustedProxies + DB-managed entries merged in.
	// Boot is non-fatal on fetch failure: a partial list still works.
	trustedCIDRs := buildTrustedProxies(ctx, cfg, ipPolicySvc)
	deps.TrustedProxies = trustedCIDRs

	// Default to release mode, and read the environment rather than
	// gin.Mode(): gin's own init() has already normalized an unset GIN_MODE to
	// "debug", so gin.Mode() never returns the empty string and a check
	// against it would leave the server in debug mode forever — which turns
	// on, among other things, a panic handler that dumps request headers.
	if os.Getenv(gin.EnvGinMode) == "" {
		gin.SetMode(gin.ReleaseMode)
	}
	router := api.NewRouter(deps)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           router,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		slog.Info("server starting", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server crashed", "err", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	slog.Info("shutdown signal received")

	// Drain in-flight HTTP requests BEFORE cancelling the root ctx.
	//
	// Ordering matters: background writers (the usage-log batch writer)
	// select on ctx.Done() and flush-and-exit. If we
	// cancelled first, those writers would stop reading while Shutdown was
	// still draining requests — every record those requests submitted would
	// land in a channel nobody reads and be lost silently. Their explicit
	// Stop() deferred above still runs after this, so they get a second,
	// ordered chance to drain.
	// Relay sockets are hijacked, so Shutdown would not close them: tell
	// daemons and clients to reconnect (4503).
	relaySrv.Close()
	shutdownCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer sCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "err", err)
	}

	cancel()
	slog.Info("server stopped cleanly")
}

// buildTrustedProxies merges all sources of trusted-proxy CIDRs:
// loopback + CF auto-fetch + config Extra + DB-managed entries.
// Fetch errors are logged but never fatal.
func buildTrustedProxies(ctx context.Context, cfg *conf.Config, ipPolicy *biz.IPPolicySvc) []string {
	fetched, err := trustedproxy.Fetch(ctx, cfg.Server.TrustedProxyFetchURLs)
	if err != nil {
		slog.Warn("trusted_proxy fetch partial/failed",
			"err", err.Error(),
			"fetched_count", len(fetched))
	}
	fromDB := ipPolicy.TrustedProxiesCIDRs(ctx)
	out := trustedproxy.Combine(
		trustedproxy.Loopback(),
		fetched,
		cfg.Server.ExtraTrustedProxies,
		fromDB,
	)
	slog.Info("trusted_proxies resolved",
		"total", len(out),
		"fetched", len(fetched),
		"extra", len(cfg.Server.ExtraTrustedProxies),
		"db", len(fromDB))
	return out
}

// regionSnapshots serves the region control plane's snapshots from the
// account pool and API key services.
type regionSnapshots struct {
	pool   *biz.AccountPoolSvc
	keys   *biz.ApiKeySvc
	cipher *cryptopkg.Cipher
}

func (r regionSnapshots) UpstreamSnapshot(ctx context.Context) ([]regionpkg.ProviderSnapshot, error) {
	return r.pool.RegionalSnapshot(ctx, r.cipher)
}

func (r regionSnapshots) AccountSnapshot(ctx context.Context, hash string) (*regionpkg.AccountSnapshot, error) {
	account, err := r.keys.RegionalSnapshot(ctx, hash)
	if errors.Is(err, biz.ErrApiKeyNotFound) || errors.Is(err, biz.ErrApiKeyOrgDenied) {
		return nil, regionpkg.ErrNotFound
	}
	return account, err
}
