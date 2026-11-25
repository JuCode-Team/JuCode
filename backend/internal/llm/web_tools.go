package llm

// web_tools.go serves the platform's web tools: POST /tools/v1/search and
// POST /tools/v1/fetch (biz/web_tools.go has the engines, prices and account
// selection). They are not models: the flow here is validate → per-user rate
// limit → balance precheck → vendor accounts in order → charge the engine's
// flat price on success. Accounts share the pool machinery with model traffic
// (egress proxy, concurrency slot, health reports, quota sampling).
//
// Vendors fetch the pages; the gateway never dials a user-supplied URL itself,
// which keeps our own network out of reach of the URLs callers send.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/middleware"
)

const (
	// maxWebSearchResults stays at the count every vendor includes in its base
	// price (Parallel bills each result past 10).
	maxWebSearchResults = 10
	maxWebSearchQuery   = 400
	maxWebFetchURL      = 2048
	// webFetchTimeout is what the fetch vendors are told to spend on one page.
	webFetchTimeout = 30 * time.Second
	// maxWebVendorResponse bounds one vendor response held in memory.
	maxWebVendorResponse = 16 << 20
	// webSnippetChars bounds the excerpt text Parallel returns per result.
	webSnippetChars = 1500
)

// SetWebTools wires the web tool settings and account selection; nil leaves
// /tools/v1 answering 503.
func (g *Gateway) SetWebTools(svc *biz.WebToolSvc) { g.webTools = svc }

// HandleWebSearch implements POST /tools/v1/search.
func (g *Gateway) HandleWebSearch(c *gin.Context) { g.handleWebTool(c, biz.WebToolSearch) }

// HandleWebFetch implements POST /tools/v1/fetch.
func (g *Gateway) HandleWebFetch(c *gin.Context) { g.handleWebTool(c, biz.WebToolFetch) }

// webToolRequest is one validated call.
type webToolRequest struct {
	engine biz.WebEngine
	search webSearchInput
	fetch  webFetchInput
}

func (g *Gateway) handleWebTool(c *gin.Context, kind string) {
	startedAt := time.Now()
	requestID := gatewayRequestID(c)
	writeErr := func(status int, errKind, code, message string) {
		c.JSON(status, openAIError(errKind, code, message, requestID))
	}
	user, ok := middleware.UserFrom(c)
	if !ok {
		writeErr(http.StatusUnauthorized, "authentication_error", "missing_user", "no user")
		return
	}
	apiKey, _ := middleware.ApiKeyFrom(c)
	if g.webTools == nil {
		writeErr(http.StatusServiceUnavailable, "service_unavailable", "web_tools_unavailable", "web tools are not available")
		return
	}
	ctx := c.Request.Context()
	if g.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.requestTimeout)
		defer cancel()
	}

	raw, err := readLimitedBody(c, g.maxBodyBytes)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeErr(http.StatusRequestEntityTooLarge, "invalid_request_error", "request_body_too_large", "request body too large")
			return
		}
		writeErr(http.StatusBadRequest, "invalid_request_error", "read_body_failed", "failed to read request body")
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		writeErr(http.StatusBadRequest, "invalid_request_error", "invalid_json", "body must be a JSON object")
		return
	}
	req, err := parseWebToolRequest(kind, body)
	if err != nil {
		writeErr(http.StatusBadRequest, "invalid_request_error", "invalid_web_request", err.Error())
		return
	}
	settings, err := g.webTools.Settings(ctx)
	if err != nil {
		writeErr(http.StatusInternalServerError, "api_error", "internal_error", "internal server error")
		return
	}
	setting := settings.Engines[req.engine.ID]
	if !setting.Enabled {
		writeErr(http.StatusBadRequest, "invalid_request_error", "engine_disabled", "engine "+req.engine.Engine+" is not available")
		return
	}
	if !g.webTools.AllowCall(ctx, user.ID, settings.UserRPM) {
		c.Header("Retry-After", "60")
		writeErr(http.StatusTooManyRequests, "rate_limit_error", "rpm_limit",
			fmt.Sprintf("web tool calls are limited to %d per minute", settings.UserRPM))
		return
	}
	if g.policy != nil {
		scan := g.policy.Scan(string(raw))
		if block, _ := scan.BlockDecision(); block {
			writeErr(http.StatusUnprocessableEntity, "content_policy_violation", "input_policy_block", "blocked by input moderation policy")
			return
		}
	}
	if err := g.billing.PrecheckBalance(ctx, user, apiKey); err != nil {
		g.writeBillingError(c, writeErr, err)
		return
	}
	accounts, err := g.webTools.Accounts(ctx, req.engine)
	if err != nil {
		writeErr(http.StatusInternalServerError, "api_error", "internal_error", "internal server error")
		return
	}
	if len(accounts) == 0 {
		writeErr(http.StatusServiceUnavailable, "service_unavailable", "no_available_provider", "engine "+req.engine.Engine+" has no available account")
		return
	}

	lc := logContext{IP: c.ClientIP(), UserAgent: c.Request.UserAgent(), ApiKeyName: keyName(apiKey), AuthorizationID: middleware.AuthzIDFrom(c)}
	attempts := &usageAttemptRecorder{endpoint: webToolEndpoint(kind), group: ""}
	var (
		lastErr    error
		lastStatus int
		used       biz.RouteAccount
	)
	for i, rp := range accounts {
		if i >= g.maxAttempts {
			break
		}
		lc.AccountID = accountIDRef(rp.Account)
		used = rp
		attemptIdx := attempts.Start(rp)
		if !g.acquireAccountSlot(ctx, rp) {
			lastErr, lastStatus = errAccountsBusy, http.StatusTooManyRequests
			attempts.SetResult(attemptIdx, 0, "account at concurrency cap", "account_busy")
			continue
		}
		prov, err := g.buildProviderForAttempt(ctx, rp, biz.RequestProtocolAny)
		var res *webResult
		if err == nil {
			res, err = forwardWebTool(ctx, prov, req)
		}
		g.releaseAccountSlot(ctx, rp)
		if err != nil {
			lastErr, lastStatus = err, 0
			attempts.SetResult(attemptIdx, 0, err.Error(), "forward_error")
			g.reportGenerationFailure(ctx, rp, req.engine.ID, accountFailureClass("forward_error"), 0, nil, err.Error())
			continue
		}
		g.sampleAccountQuota(ctx, rp, res.Header)
		if generationShouldFailover(res.StatusCode) {
			lastErr, lastStatus = upstreamStatusErr(res.StatusCode, res.Body), res.StatusCode
			lc.UpstreamRawError = truncate(string(res.Body), 8192)
			attempts.SetResult(attemptIdx, res.StatusCode, string(res.Body), switchStrategyForStatus(res.StatusCode))
			g.reportGenerationFailure(ctx, rp, req.engine.ID,
				string(classifyUpstreamResponse(res.StatusCode, res.Body)), res.StatusCode, res.Header, string(res.Body))
			continue
		}
		attempts.SetResult(attemptIdx, res.StatusCode, "", "")
		attempts.MarkFinal(attemptIdx)
		lc.ErrorClass = string(attempts.FinalClass())
		c.Data(res.StatusCode, "application/json", res.Body)
		if res.StatusCode >= 400 {
			// The request itself failed (for example a page that could not be
			// fetched): nothing is charged.
			g.logWebTool(ctx, user, apiKey, requestID, kind, req.engine, rp, decimal.Zero, nil, entusagelog.StatusUpstreamError,
				res.StatusCode, fmt.Sprintf("upstream %d", res.StatusCode), startedAt, lc, attempts)
			return
		}
		g.recordGenerationSuccess(ctx, rp)
		// Detached like chargeAndLog: the result is already written, so a
		// client disconnect must not cancel the charge.
		chargeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		charge, chargeErr := g.billing.ChargeBalance(chargeCtx, user, apiKey, setting.Price, "web tool: "+req.engine.ID)
		status, message := entusagelog.StatusOk, ""
		if chargeErr != nil {
			status, message = entusagelog.StatusInternalError, chargeErr.Error()
		}
		g.logWebTool(chargeCtx, user, apiKey, requestID, kind, req.engine, rp, setting.Price, charge, status,
			res.StatusCode, message, startedAt, lc, attempts)
		return
	}

	status := http.StatusBadGateway
	message := "upstream unavailable"
	if lastErr != nil {
		message = lastErr.Error()
	}
	if lastStatus == http.StatusTooManyRequests || lastStatus >= 500 {
		status = http.StatusServiceUnavailable
	}
	writeErr(status, "upstream_error", "upstream_unavailable", message)
	lc.ErrorClass = string(attempts.FinalClass())
	g.logWebTool(ctx, user, apiKey, requestID, kind, req.engine, used, decimal.Zero, nil, entusagelog.StatusUpstreamError,
		status, message, startedAt, lc, attempts)
}

func webToolEndpoint(kind string) string {
	if kind == biz.WebToolFetch {
		return string(entusagelog.EndpointWebFetch)
	}
	return string(entusagelog.EndpointWebSearch)
}

// logWebTool writes the usage row of one web tool call. The row's model column
// carries the engine id ("search.parallel") and upstream_model the vendor, so
// usage views tell tool calls apart by endpoint (web_search / web_fetch).
func (g *Gateway) logWebTool(ctx context.Context, user *ent.User, apiKey *ent.ApiKey, requestID, kind string,
	engine biz.WebEngine, rp biz.RouteAccount, price decimal.Decimal, charge *biz.ChargeResult, status entusagelog.Status,
	httpStatus int, message string, startedAt time.Time, lc logContext, attempts *usageAttemptRecorder,
) {
	legacyProvID, acctID := usageAttribution(rp)
	vendor := ""
	if rp.Account != nil {
		vendor = rp.Account.APIFormat
	}
	in := biz.WriteUsageLogInput{
		UserID: user.ID, ApiKeyID: apiKeyID(apiKey), AuthorizationID: lc.AuthorizationID, RequestID: requestID,
		OrganizationID: organizationID(apiKey), Endpoint: entusagelog.Endpoint(webToolEndpoint(kind)), Model: engine.ID,
		ProviderID: legacyProvID, ProviderAccountID: acctID, UpstreamModel: vendor,
		Status: status, HTTPStatus: httpStatus,
		LatencyMs: int(time.Since(startedAt).Milliseconds()), StartedAt: startedAt,
		ErrorMessage: message, UpstreamRawError: lc.UpstreamRawError, ErrorClass: lc.ErrorClass,
		RetryCount: attempts.RetryCount(), ProviderChain: attempts.Chain(),
		IP: lc.IP, UserAgent: lc.UserAgent, ApiKeyName: lc.ApiKeyName,
	}
	if charge != nil {
		in.CostBase, in.CostFinal, in.Currency = price, price, biz.CurrencyCNY
		in.RateMultiplierApplied = 1
		in.BillingSource = charge.BillingSource
		if charge.OrganizationID != nil {
			in.OrganizationID = charge.OrganizationID
		}
		in.Delivered = true
	}
	_, _ = g.billing.WriteUsageLog(ctx, in)
}

// parseWebToolRequest validates a body and resolves its engine.
func parseWebToolRequest(kind string, body map[string]any) (webToolRequest, error) {
	engineName := ""
	if value, present := body["engine"]; present {
		text, ok := value.(string)
		if !ok {
			return webToolRequest{}, errors.New("engine must be a string")
		}
		engineName = text
	}
	engine, ok := biz.WebEngineByKind(kind, engineName)
	if !ok {
		return webToolRequest{}, fmt.Errorf("unknown engine %q: want %s", engineName, strings.Join(biz.WebEngineNames(kind), ", "))
	}
	req := webToolRequest{engine: engine}
	var err error
	if kind == biz.WebToolFetch {
		req.fetch, err = parseWebFetchInput(body)
	} else {
		req.search, err = parseWebSearchInput(body)
	}
	return req, err
}

// webSearchInput is the validated /tools/v1/search body.
type webSearchInput struct {
	Query      string
	MaxResults int
	// Freshness is "", "day", "week", "month" or "year".
	Freshness string
}

func parseWebSearchInput(body map[string]any) (webSearchInput, error) {
	in := webSearchInput{MaxResults: maxWebSearchResults}
	query, isText := body["query"].(string)
	if _, present := body["query"]; present && !isText {
		return in, errors.New("query must be a string")
	}
	in.Query = strings.TrimSpace(query)
	if in.Query == "" {
		return in, errors.New("query is required")
	}
	if utf8.RuneCountInString(in.Query) > maxWebSearchQuery {
		return in, fmt.Errorf("query must be at most %d characters", maxWebSearchQuery)
	}
	if value, present := body["max_results"]; present {
		n, ok := value.(float64)
		if !ok || n != float64(int(n)) || n < 1 || n > maxWebSearchResults {
			return in, fmt.Errorf("max_results must be an integer from 1 to %d", maxWebSearchResults)
		}
		in.MaxResults = int(n)
	}
	if value, present := body["freshness"]; present {
		text, _ := value.(string)
		switch text {
		case "day", "week", "month", "year":
			in.Freshness = text
		default:
			return in, errors.New("freshness must be day, week, month or year")
		}
	}
	return in, nil
}

// webFetchInput is the validated /tools/v1/fetch body.
type webFetchInput struct {
	URL string
}

func parseWebFetchInput(body map[string]any) (webFetchInput, error) {
	raw, _ := body["url"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return webFetchInput{}, errors.New("url is required")
	}
	if len(raw) > maxWebFetchURL {
		return webFetchInput{}, fmt.Errorf("url must be at most %d characters", maxWebFetchURL)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return webFetchInput{}, errors.New("url must be an absolute http or https URL")
	}
	if u.User != nil {
		return webFetchInput{}, errors.New("url must not contain credentials")
	}
	if isNonPublicHost(u.Hostname()) {
		return webFetchInput{}, errors.New("url must point to a public address")
	}
	return webFetchInput{URL: raw}, nil
}

// isNonPublicHost spots hosts no vendor can fetch: loopback, private and
// link-local addresses and local-only names. Jina answers them with a 403
// that would otherwise read as a bad account key.
func isNonPublicHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast())
}

// --- normalized responses ---

type webSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet"`
	PublishedAt string `json:"published_at,omitempty"`
}

type webSearchResponse struct {
	Object  string            `json:"object"`
	Engine  string            `json:"engine"`
	Query   string            `json:"query"`
	Results []webSearchResult `json:"results"`
}

type webFetchResponse struct {
	Object      string `json:"object"`
	Engine      string `json:"engine"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Status      int    `json:"status,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
}

// webResult is a vendor answer in the gateway's own shape: the normalized
// JSON on success, the gateway error envelope otherwise.
type webResult struct {
	StatusCode int
	Body       []byte
	// Header carries the vendor's quota headers to the passive sampler.
	Header http.Header
}

// forwardWebTool sends one call to the vendor behind this account.
func forwardWebTool(ctx context.Context, prov Provider, req webToolRequest) (*webResult, error) {
	o, ok := prov.(*openaiProvider)
	if !ok {
		return nil, fmt.Errorf("account is not a web tool vendor")
	}
	vendor := strings.ToLower(strings.TrimSpace(o.format))
	switch req.engine.Kind + "/" + vendor {
	case biz.WebToolSearch + "/parallel":
		return o.parallelSearch(ctx, req.search)
	case biz.WebToolSearch + "/brave":
		return o.braveSearch(ctx, req.search)
	case biz.WebToolFetch + "/parallel":
		return o.parallelExtract(ctx, req.fetch)
	case biz.WebToolFetch + "/jina":
		return o.jinaFetch(ctx, req.fetch)
	case biz.WebToolFetch + "/firecrawl":
		return o.firecrawlScrape(ctx, req.fetch)
	}
	return nil, fmt.Errorf("vendor %q does not serve web %s", vendor, req.engine.Kind)
}

// webCall sends one vendor request and reads the (bounded) response.
func (o *openaiProvider) webCall(ctx context.Context, method, path string, query url.Values, payload any, auth func(http.Header)) (*http.Response, []byte, error) {
	target := o.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, nil, err
	}
	if payload != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	auth(httpReq.Header)
	biz.ApplyHeaderRules(httpReq.Header, o.headerRules, o.apiKey)
	resp, err := o.hc.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWebVendorResponse))
	if err != nil {
		return nil, nil, err
	}
	return resp, raw, nil
}

func webJSONResponse(value any, header http.Header) (*webResult, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &webResult{StatusCode: http.StatusOK, Body: raw, Header: header}, nil
}

// webErrorResponse is a vendor failure in the gateway's error shape. The
// status keeps its meaning for failover: 401/402/403/429/5xx move on to the
// next account, 400/422 go back to the caller.
func webErrorResponse(status int, code, message string, header http.Header) *webResult {
	raw, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": "upstream_error", "code": code, "message": message},
	})
	return &webResult{StatusCode: status, Body: raw, Header: header}
}

// vendorError reads a vendor's error message from the first matching dotted
// path; the raw body stands in when none matches.
func vendorError(vendor string, status int, raw []byte, code string, paths ...string) *webResult {
	var doc any
	_ = json.Unmarshal(raw, &doc)
	message := ""
	for _, path := range paths {
		if text, ok := lookupString(doc, path); ok && text != "" {
			message = text
			break
		}
	}
	if message == "" {
		message = truncate(strings.TrimSpace(string(raw)), 300)
	}
	if code == "" {
		code = "upstream_status_" + strconv.Itoa(status)
	}
	return webErrorResponse(status, code, vendor+": "+message, nil)
}

func lookupString(doc any, path string) (string, bool) {
	cur := doc
	for _, key := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur = obj[key]
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	}
	return "", false
}

// freshnessSince is the first day inside a freshness window.
func freshnessSince(freshness string, now time.Time) time.Time {
	switch freshness {
	case "day":
		return now.AddDate(0, 0, -1)
	case "week":
		return now.AddDate(0, 0, -7)
	case "month":
		return now.AddDate(0, -1, 0)
	default:
		return now.AddDate(-1, 0, 0)
	}
}

// --- Parallel (https://docs.parallel.ai) ---

func (o *openaiProvider) parallelAuth(h http.Header) { h.Set("x-api-key", o.apiKey) }

func (o *openaiProvider) parallelSearch(ctx context.Context, in webSearchInput) (*webResult, error) {
	settings := map[string]any{
		"max_results":      in.MaxResults,
		"excerpt_settings": map[string]any{"max_chars_per_result": webSnippetChars},
	}
	if in.Freshness != "" {
		settings["source_policy"] = map[string]any{
			"after_date": freshnessSince(in.Freshness, time.Now().UTC()).Format("2006-01-02"),
		}
	}
	resp, raw, err := o.webCall(ctx, http.MethodPost, "/v1/search", nil, map[string]any{
		"objective":         in.Query,
		"search_queries":    []string{in.Query},
		"advanced_settings": settings,
	}, o.parallelAuth)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return vendorError("parallel", resp.StatusCode, raw, "", "error.message", "error.detail"), nil
	}
	var doc struct {
		Results []struct {
			URL         string   `json:"url"`
			Title       *string  `json:"title"`
			PublishDate *string  `json:"publish_date"`
			Excerpts    []string `json:"excerpts"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parallel: decode search response: %w", err)
	}
	out := webSearchResponse{Object: "web.search", Engine: "parallel", Query: in.Query, Results: []webSearchResult{}}
	for _, r := range doc.Results {
		out.Results = append(out.Results, webSearchResult{
			Title:       derefString(r.Title),
			URL:         r.URL,
			Snippet:     strings.Join(r.Excerpts, "\n…\n"),
			PublishedAt: derefString(r.PublishDate),
		})
	}
	return webJSONResponse(out, nil)
}

func (o *openaiProvider) parallelExtract(ctx context.Context, in webFetchInput) (*webResult, error) {
	resp, raw, err := o.webCall(ctx, http.MethodPost, "/v1/extract", nil, map[string]any{
		"urls": []string{in.URL},
		"advanced_settings": map[string]any{
			"full_content": true,
			"fetch_policy": map[string]any{"timeout_seconds": int(webFetchTimeout / time.Second)},
		},
	}, o.parallelAuth)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return vendorError("parallel", resp.StatusCode, raw, "", "error.message", "error.detail"), nil
	}
	var doc struct {
		Results []struct {
			URL         string   `json:"url"`
			Title       *string  `json:"title"`
			PublishDate *string  `json:"publish_date"`
			FullContent *string  `json:"full_content"`
			Excerpts    []string `json:"excerpts"`
		} `json:"results"`
		Errors []struct {
			URL            string `json:"url"`
			ErrorType      string `json:"error_type"`
			HTTPStatusCode *int   `json:"http_status_code"`
			Content        string `json:"content"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parallel: decode extract response: %w", err)
	}
	if len(doc.Results) == 0 {
		message := "the page could not be fetched"
		if len(doc.Errors) > 0 {
			e := doc.Errors[0]
			message = strings.TrimSpace(e.ErrorType + " " + e.Content)
			if e.HTTPStatusCode != nil {
				message = fmt.Sprintf("%s (HTTP %d)", message, *e.HTTPStatusCode)
			}
		}
		return webErrorResponse(http.StatusUnprocessableEntity, "fetch_failed", "parallel: "+message, nil), nil
	}
	r := doc.Results[0]
	// Some pages come back with excerpts only and an empty full_content.
	content := derefString(r.FullContent)
	if strings.TrimSpace(content) == "" {
		content = strings.Join(nonEmpty(r.Excerpts), "\n\n")
	}
	if strings.TrimSpace(content) == "" {
		return webErrorResponse(http.StatusUnprocessableEntity, "fetch_failed", "parallel: the page returned no content", nil), nil
	}
	return webJSONResponse(webFetchResponse{
		Object:      "web.fetch",
		Engine:      "parallel",
		URL:         firstNonEmpty(r.URL, in.URL),
		Title:       derefString(r.Title),
		Content:     content,
		PublishedAt: datePart(derefString(r.PublishDate)),
	}, nil)
}

// --- Brave (https://api-dashboard.search.brave.com) ---

// braveFreshness maps the freshness window onto Brave's codes.
var braveFreshness = map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}

func (o *openaiProvider) braveSearch(ctx context.Context, in webSearchInput) (*webResult, error) {
	query := url.Values{}
	query.Set("q", in.Query)
	query.Set("count", strconv.Itoa(in.MaxResults))
	query.Set("text_decorations", "false")
	query.Set("extra_snippets", "true")
	if code := braveFreshness[in.Freshness]; code != "" {
		query.Set("freshness", code)
	}
	resp, raw, err := o.webCall(ctx, http.MethodGet, "/res/v1/web/search", query, nil, func(h http.Header) {
		h.Set("X-Subscription-Token", o.apiKey)
	})
	if err != nil {
		return nil, err
	}
	// The monthly quota rides on every response, errors included.
	header := resp.Header.Clone()
	if resp.StatusCode != http.StatusOK {
		failure := vendorError("brave", braveStatus(resp.StatusCode, raw), raw, "", "error.detail", "error.code")
		failure.Header = header
		return failure, nil
	}
	var doc struct {
		Web struct {
			Results []struct {
				Title         string   `json:"title"`
				URL           string   `json:"url"`
				Description   string   `json:"description"`
				PageAge       string   `json:"page_age"`
				ExtraSnippets []string `json:"extra_snippets"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("brave: decode search response: %w", err)
	}
	out := webSearchResponse{Object: "web.search", Engine: "brave", Query: in.Query, Results: []webSearchResult{}}
	for _, r := range doc.Web.Results {
		// Unescape before comparing: the same text may arrive escaped once
		// and plain once.
		snippets := []string{html.UnescapeString(r.Description)}
		for _, extra := range r.ExtraSnippets {
			snippets = append(snippets, html.UnescapeString(extra))
		}
		out.Results = append(out.Results, webSearchResult{
			Title:       html.UnescapeString(r.Title),
			URL:         r.URL,
			Snippet:     strings.Join(distinctSnippets(snippets), "\n"),
			PublishedAt: datePart(r.PageAge),
		})
	}
	return webJSONResponse(out, header)
}

// braveStatus gives Brave's failures their HTTP meaning: an invalid key comes
// back as 422 and quota exhaustion as a plain error code, and both must fail
// over like a 401 / 429 would.
func braveStatus(status int, raw []byte) int {
	var doc any
	_ = json.Unmarshal(raw, &doc)
	code, _ := lookupString(doc, "error.code")
	switch code {
	case "SUBSCRIPTION_TOKEN_INVALID":
		return http.StatusUnauthorized
	case "QUOTA_LIMITED", "RATE_LIMITED":
		return http.StatusTooManyRequests
	}
	return status
}

// --- Jina Reader (https://r.jina.ai) ---

func (o *openaiProvider) jinaFetch(ctx context.Context, in webFetchInput) (*webResult, error) {
	resp, raw, err := o.webCall(ctx, http.MethodPost, "/", nil, map[string]any{"url": in.URL}, func(h http.Header) {
		h.Set("Authorization", "Bearer "+o.apiKey)
		h.Set("X-Timeout", strconv.Itoa(int(webFetchTimeout/time.Second)))
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		failure := vendorError("jina", resp.StatusCode, raw, "", "readableMessage", "message")
		// Errors about the target page (unresolvable domain, non-public IP)
		// name the url field. They are the caller's to fix, not a reason to
		// fail the account over.
		var doc any
		_ = json.Unmarshal(raw, &doc)
		if field, _ := lookupString(doc, "path"); field == "url" && resp.StatusCode < 500 {
			var env struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(failure.Body, &env)
			failure = webErrorResponse(http.StatusUnprocessableEntity, "fetch_failed", env.Error.Message, nil)
		}
		return failure, nil
	}
	var doc struct {
		Data struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedTime string `json:"publishedTime"`
			HTTPStatus    int    `json:"httpStatus"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("jina: decode reader response: %w", err)
	}
	return webJSONResponse(webFetchResponse{
		Object:      "web.fetch",
		Engine:      "jina",
		URL:         firstNonEmpty(doc.Data.URL, in.URL),
		Title:       doc.Data.Title,
		Content:     doc.Data.Content,
		Status:      doc.Data.HTTPStatus,
		PublishedAt: datePart(doc.Data.PublishedTime),
	}, nil)
}

// --- Firecrawl (https://docs.firecrawl.dev) ---

func (o *openaiProvider) firecrawlScrape(ctx context.Context, in webFetchInput) (*webResult, error) {
	resp, raw, err := o.webCall(ctx, http.MethodPost, "/v2/scrape", nil, map[string]any{
		"url":             in.URL,
		"formats":         []string{"markdown"},
		"onlyMainContent": true,
		"timeout":         int(webFetchTimeout / time.Millisecond),
	}, func(h http.Header) {
		h.Set("Authorization", "Bearer "+o.apiKey)
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var doc any
		_ = json.Unmarshal(raw, &doc)
		code, _ := lookupString(doc, "code")
		return vendorError("firecrawl", resp.StatusCode, raw, code, "error"), nil
	}
	var doc struct {
		Success *bool  `json:"success"`
		Code    string `json:"code"`
		Error   string `json:"error"`
		Data    struct {
			Markdown string `json:"markdown"`
			Metadata struct {
				Title      json.RawMessage `json:"title"`
				URL        string          `json:"url"`
				SourceURL  string          `json:"sourceURL"`
				StatusCode int             `json:"statusCode"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("firecrawl: decode scrape response: %w", err)
	}
	// A page Firecrawl could not load (DNS failure, blocked site) still
	// arrives as HTTP 200 with success:false.
	if doc.Success != nil && !*doc.Success {
		return webErrorResponse(http.StatusUnprocessableEntity, "fetch_failed",
			"firecrawl: "+firstNonEmpty(doc.Error, doc.Code, "the page could not be fetched"), nil), nil
	}
	meta := doc.Data.Metadata
	return webJSONResponse(webFetchResponse{
		Object:  "web.fetch",
		Engine:  "firecrawl",
		URL:     firstNonEmpty(meta.URL, meta.SourceURL, in.URL),
		Title:   firecrawlTitle(meta.Title),
		Content: doc.Data.Markdown,
		Status:  meta.StatusCode,
	}, nil)
}

// firecrawlTitle reads metadata.title, which is a string or a string array.
func firecrawlTitle(raw json.RawMessage) string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
		return many[0]
	}
	return ""
}

// --- helpers ---

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// distinctSnippets drops Brave snippet parts repeated in another part: the
// description is often an extra snippet again, or its cut-off start.
func distinctSnippets(values []string) []string {
	parts := nonEmpty(values)
	out := parts[:0:0]
	for i, p := range parts {
		core := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(p), ".…"))
		repeated := false
		for j, q := range parts {
			if j != i && (len(q) > len(p) || (q == p && j < i)) && strings.Contains(q, core) {
				repeated = true
				break
			}
		}
		if !repeated {
			out = append(out, p)
		}
	}
	return out
}

func nonEmpty(values []string) []string {
	out := values[:0:0]
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// datePart keeps the date of an RFC 3339 timestamp.
func datePart(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700", time.RFC1123, time.RFC1123Z} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if len(value) >= 10 {
		if t, err := time.Parse("2006-01-02", value[:10]); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}
