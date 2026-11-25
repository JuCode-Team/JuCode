package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/llm"
	"github.com/jucode/jucode/internal/region"
)

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.ready)
	mux.HandleFunc("/v1/models", s.listModels)
	mux.HandleFunc("/v1/chat/completions", s.proxy)
	mux.HandleFunc("/v1/responses", s.proxy)
	mux.HandleFunc("/v1/responses/compact", s.proxy)
	mux.HandleFunc("/v1/messages", s.proxy)
	mux.HandleFunc("/anthropic/v1/messages", s.proxy)
	return requestIDMiddleware(mux)
}

// requestIDMiddleware echoes (or mints) an X-Request-ID for log correlation
// with the caller's own systems. The value is attacker-controlled and is
// NEVER used as a billing identifier: every accounting operation keys off a
// server-generated UUID minted per request in proxy(), so replaying a
// previously settled X-Request-ID cannot reuse a settled reservation.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = uuid.NewString()
		}
		if len(id) > 128 {
			id = id[:128]
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), correlationIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type correlationIDKey struct{}

// correlationID is the client-supplied (or generated) X-Request-ID. Logging
// only; never an accounting key.
func correlationID(ctx context.Context) string {
	if v, ok := ctx.Value(correlationIDKey{}).(string); ok {
		return v
	}
	return ""
}

func (s *Service) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "region": s.cfg.RegionID})
}

func (s *Service) ready(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	loaded := len(s.upstreams) > 0
	s.mu.RUnlock()
	if !loaded {
		// The status string stays "providers_not_ready": it is an operator-facing
		// readiness contract monitored from outside, and the snapshot it refers to
		// is the (now account-keyed) upstream snapshot.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "providers_not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "region": s.cfg.RegionID})
}

// listModels exposes the models that are both allowed for the caller's API
// key and currently available through at least one synchronized upstream
// account. Upstream credentials and account metadata never leave the edge.
func (s *Service) listModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	account, err := s.account(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	if account.Status != "active" || account.Revoked || (account.ExpiresAt != nil && !time.Now().Before(*account.ExpiresAt)) {
		writeError(w, http.StatusForbidden, "account_inactive", "account is not active")
		return
	}
	if !ipAllowed(s.clientIP(r), account.IPWhitelist) {
		writeError(w, http.StatusForbidden, "ip_not_allowed", "client IP is not allowed")
		return
	}

	models := s.visibleModels(account.AllowedModels)
	if llm.IsGrokBuildClient(r) {
		infos := make([]llm.GrokBuildModelInfo, 0, len(models))
		for _, model := range models {
			infos = append(infos, llm.GrokBuildModelInfo{ID: model, Name: model})
		}
		writeJSON(w, http.StatusOK, llm.BuildGrokBuildModelsResponse(infos))
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  int64(0),
			"owned_by": "jucode",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Service) visibleModels(allowed []string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	available := make(map[string]struct{})
	unrestricted := false
	for _, upstream := range s.upstreams {
		if len(upstream.snapshot.Models) == 0 {
			unrestricted = true
			continue
		}
		for _, model := range upstream.snapshot.Models {
			if strings.TrimSpace(model) != "" {
				available[model] = struct{}{}
			}
		}
	}

	seen := make(map[string]struct{}, len(allowed))
	models := make([]string, 0, len(allowed))
	for _, model := range allowed {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if !unrestricted {
			if _, ok := available[model]; !ok {
				continue
			}
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (s *Service) proxy(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	account, err := s.account(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	if account.Status != "active" || account.Revoked || (account.ExpiresAt != nil && !time.Now().Before(*account.ExpiresAt)) {
		writeError(w, http.StatusForbidden, "account_inactive", "account is not active")
		return
	}
	accountID, err := ledgerAccountID(account)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "billing_unavailable", "account billing scope is invalid")
		return
	}
	if !ipAllowed(s.clientIP(r), account.IPWhitelist) {
		writeError(w, http.StatusForbidden, "ip_not_allowed", "client IP is not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.cfg.MaxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "request body is too large")
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be JSON")
		return
	}
	model, _ := request["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeError(w, http.StatusBadRequest, "missing_model", "model is required")
		return
	}
	modelAllowed := false
	for _, allowed := range account.AllowedModels {
		if allowed == model {
			modelAllowed = true
			break
		}
	}
	if !modelAllowed {
		writeError(w, http.StatusForbidden, "model_not_allowed", "model is not allowed for this API key")
		return
	}
	price, ok := account.ModelPrices[model]
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "billing_unavailable", "model price is not synchronized")
		return
	}

	upstream, upstreamModel, err := s.chooseUpstream(model, requestProtocol(r.URL.Path))
	if err != nil {
		// The error code stays "provider_unavailable": it is part of the edge's
		// client-facing error vocabulary, and the thing it names is the upstream
		// account that could not be selected.
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", err.Error())
		return
	}
	if upstreamModel == "" {
		upstreamModel = model
	}
	body, err = llm.RewriteTopLevelModel(body, upstreamModel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request could not be prepared")
		return
	}

	if s.keyCostLimitExceeded(r.Context(), account) {
		writeError(w, http.StatusForbidden, "cost_limit_exceeded", "api key cost limit exceeded")
		return
	}

	// billingID is the accounting idempotency key for reserve/settle and the
	// central usage event. It is minted server-side for every request; the
	// client's X-Request-ID (see requestIDMiddleware) is correlation-only.
	billingID := uuid.NewString()
	logCorrelation := correlationID(r.Context())

	reserve := s.reserve
	reservation, err := s.reserveRequest(r.Context(), account, billingID, reserve)
	if err != nil {
		status, code, message := reservationError(err)
		writeError(w, status, code, message)
		return
	}
	// A reservation that is not freshly created must never buy an upstream
	// call: "existing" means this billing ID was seen before (impossible for
	// a server-minted UUID unless state was replayed or tampered with), and a
	// settled/released reservation has already been accounted.
	if reservation.Status != "reserved" {
		slog.Error("regional reservation was not freshly created; refusing to forward",
			"request_id", billingID, "correlation_id", logCorrelation, "status", reservation.Status)
		writeError(w, http.StatusConflict, "billing_conflict", "request billing state conflict")
		return
	}

	stream := boolValue(request["stream"])
	resp, err := s.forward(r, upstream, r.URL.Path, body, stream)
	if err != nil {
		s.releaseReservation(accountID, billingID)
		writeError(w, http.StatusBadGateway, "upstream_unavailable", "upstream request failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.CopyN(w, resp.Body, s.cfg.MaxBodyBytes)
		s.releaseReservation(accountID, billingID)
		return
	}

	actual := int64(0)
	tokensIn, tokensOut := 0, 0
	var ttftMs *int
	var responseBody []byte
	status := "ok"
	if stream || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		acc := &llm.Accumulator{}
		pipeErr := llm.PipeAndExtractOpts(w, resp.Body, acc, llm.PipeOptions{OnFirstText: func() {
			d := int(time.Since(startedAt).Milliseconds())
			ttftMs = &d
		}})
		if calculated, in, out, ok := s.accumulatedUsage(price, acc); ok {
			actual = calculated
			tokensIn, tokensOut = in, out
		} else {
			tokensIn, tokensOut = accumulatedTokens(acc)
		}
		// An upstream that stopped mid-answer did not deliver the response it
		// would be charged for. There is no failover out here and the partial
		// bytes are already on the wire, but the region must not book it as a
		// success: that is how a truncated answer came to be billed in the
		// first place, and it would also put the edge ledger at odds with the
		// central one, which now records this as an upstream error.
		if llm.IsStreamIncomplete(pipeErr) {
			status = "upstream_error"
			actual = 0
		}
	} else {
		responseBody, err = io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxBodyBytes+1))
		if err != nil || int64(len(responseBody)) > s.cfg.MaxBodyBytes {
			s.releaseReservation(accountID, billingID)
			writeError(w, http.StatusBadGateway, "upstream_response_too_large", "upstream response is too large")
			return
		}
		if calculated, in, out, ok := s.responseUsage(price, responseBody); ok {
			actual = calculated
			tokensIn, tokensOut = in, out
		}
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	}
	latencyMs := int(time.Since(startedAt).Milliseconds())

	// The durable usage event is written BEFORE the local ledger settles (see
	// finalizeUsage): a settle-then-enqueue order would let a disk failure
	// drop the central accounting event for money already taken locally.
	//
	// ProviderID carries the upstream ACCOUNT id (the snapshot id we scheduled
	// by); the field name is the frozen region contract, and the main site
	// resolves it as an account id.
	event := region.UsageEvent{
		EventID: uuid.New(), RequestID: billingID, RegionID: s.cfg.RegionID,
		LeaseID: reservation.LeaseID,
		UserID:  account.UserID, APIKeyID: account.APIKeyID, BillingScope: account.BillingScope,
		OrganizationID: account.OrganizationID, ProviderID: upstream.snapshot.ID,
		Model: model, Endpoint: regionalEndpoint(r.URL.Path), Stream: stream, APIKeyName: account.APIKeyName,
		TokensIn: tokensIn, TokensOut: tokensOut, LatencyMs: latencyMs, TtftMs: ttftMs,
		Status: status, ReservedCNY: region.FormatUnits(reserve), CostCNY: region.FormatUnits(actual), OccurredAt: time.Now(),
	}
	s.finalizeUsage(accountID, event, actual, logCorrelation)
}

// reservationError maps a failed reservation to the client response. A
// refusal from the control plane (it would not grant a lease) is the
// client's problem and reads like the local one; anything else means billing
// could not be reached.
func reservationError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrInsufficientCredit), errors.Is(err, region.ErrLeaseInsufficient):
		return http.StatusPaymentRequired, "insufficient_balance", "insufficient balance"
	case errors.Is(err, region.ErrApiKeyCostLimit):
		return http.StatusForbidden, "cost_limit_exceeded", "api key cost limit exceeded"
	case errors.Is(err, region.ErrOrganizationLimit), errors.Is(err, region.ErrMemberLimit):
		return http.StatusForbidden, "organization_limit_exceeded", "organization spending limit exceeded"
	case errors.Is(err, region.ErrOrganizationForbidden):
		return http.StatusForbidden, "organization_forbidden", "organization access is forbidden"
	default:
		return http.StatusServiceUnavailable, "billing_unavailable", "billing reservation failed"
	}
}

func regionalEndpoint(path string) string {
	path = strings.ToLower(strings.TrimSpace(path))
	if strings.Contains(path, "/responses") {
		return "responses"
	}
	if llm.IsAnthropicRequestPath(path) {
		return "messages"
	}
	return "chat_completions"
}

// requestProtocol maps the edge's client-facing path to the wire protocol it
// carries: /anthropic/* and /v1/messages are Messages; everything else Chat.
func requestProtocol(requestPath string) string {
	if llm.IsAnthropicRequestPath(requestPath) {
		return "anthropic"
	}
	return "openai"
}

func (s *Service) forward(r *http.Request, p cachedUpstream, requestPath string, body []byte, stream bool) (*http.Response, error) {
	protocol := requestProtocol(requestPath)
	// An upstream account that carries no key for this protocol cannot be
	// called. The default key is optional now (an Anthropic-only account may
	// only hold the override), so an empty value here means the snapshot has
	// nothing to authenticate with — forwarding "Bearer " gets an opaque
	// upstream 401 that reads like the customer's key was rejected.
	key := p.upstreamKey(protocol)
	if key == "" {
		return nil, fmt.Errorf("upstream account has no %s api key", protocol)
	}
	requestPath = strings.TrimPrefix(requestPath, "/anthropic")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, joinURL(p.snapshot.BaseURL, requestPath), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", r.Header.Get("Accept"))
	llm.ApplyOpenAIClientSessionHeaders(req.Header, r.Header)
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		// Some upstream WAFs reject the default Go HTTP signature when the
		// header is absent. Keep a small, deterministic agent for SDKs that
		// do not send one; callers that do send one retain their value.
		req.Header.Set("User-Agent", "curl/8.0")
	}
	// Auth-header convention follows the wire protocol being spoken, not the
	// account's kind — a multi-protocol upstream expects Bearer on its
	// OpenAI endpoint and X-API-Key on its Anthropic endpoint.
	if protocol == "anthropic" {
		req.Header.Set("X-API-Key", key)
		llm.ApplyAnthropicClientHeaders(req.Header, r.Header)
		if req.Header.Get("Anthropic-Version") == "" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
		if p.snapshot.Organization != "" {
			req.Header.Set("OpenAI-Organization", p.snapshot.Organization)
		}
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	client := s.hc
	if stream {
		client = s.streamHC
	}
	return client.Do(req)
}

func (s *Service) responseCost(price region.ModelPrice, body []byte) (int64, bool) {
	cost, _, _, ok := s.responseUsage(price, body)
	return cost, ok
}

func (s *Service) responseUsage(price region.ModelPrice, body []byte) (int64, int, int, bool) {
	acc := &llm.Accumulator{}
	acc.Inspect(body)
	return s.accumulatedUsage(price, acc)
}

func (s *Service) accumulatedUsage(price region.ModelPrice, acc *llm.Accumulator) (int64, int, int, bool) {
	in, out := accumulatedTokens(acc)
	if price.BillingMode == "per_request" || price.PerRequestCNY != "" {
		d, err := decimal.NewFromString(price.PerRequestCNY)
		if err == nil {
			u, e := region.ParseUnitsRounded(d.String())
			if e == nil {
				return u, in, out, true
			}
		}
	}
	if !acc.HasUsage {
		return 0, in, out, false
	}
	inputPrice, outputPrice := price.InputPer1K, price.OutputPer1K
	cacheReadPrice, cacheWritePrice := price.CacheReadPer1K, price.CacheWritePer1K
	cacheWrite1hPrice := price.CacheWrite1hPer1K
	for _, tier := range price.PriceTiers {
		if tier.UpToTokens != 0 && int64(in) > tier.UpToTokens {
			continue
		}
		if tier.InputPer1K != "" {
			inputPrice = tier.InputPer1K
		}
		if tier.OutputPer1K != "" {
			outputPrice = tier.OutputPer1K
		}
		if tier.CacheReadPer1K != "" {
			cacheReadPrice = tier.CacheReadPer1K
		}
		if tier.CacheWritePer1K != "" {
			cacheWritePrice = tier.CacheWritePer1K
		}
		if tier.CacheWrite1hPer1K != "" {
			cacheWrite1hPrice = tier.CacheWrite1hPer1K
		}
		break
	}
	if cacheReadPrice == "" {
		cacheReadPrice = inputPrice
	}
	if cacheWritePrice == "" {
		cacheWritePrice = inputPrice
	}
	if cacheWrite1hPrice == "" {
		cacheWrite1hPrice = cacheWritePrice
	}
	// The accumulator keeps audio tokens out of TokensIn/TokensOut so they can
	// be priced separately. Falling back to the text price is what makes an
	// unpriced audio model bill the same as it did before the split, rather
	// than dropping those tokens on the floor.
	audioInPrice := price.AudioInputPer1K
	if audioInPrice == "" {
		audioInPrice = inputPrice
	}
	audioOutPrice := price.AudioOutputPer1K
	if audioOutPrice == "" {
		audioOutPrice = outputPrice
	}
	parse := func(raw string) (decimal.Decimal, bool) {
		d, err := decimal.NewFromString(raw)
		return d, err == nil
	}
	inPrice, ok1 := parse(inputPrice)
	outPrice, ok2 := parse(outputPrice)
	readPrice, ok3 := parse(cacheReadPrice)
	writePrice, ok4 := parse(cacheWritePrice)
	write1hPrice, ok1h := parse(cacheWrite1hPrice)
	audInPrice, ok5 := parse(audioInPrice)
	audOutPrice, ok6 := parse(audioOutPrice)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok1h || !ok5 || !ok6 {
		return 0, in, out, false
	}
	write5m, write1h := acc.CacheCreation5mTokensIn, acc.CacheCreation1hTokensIn
	if write5m == 0 && write1h == 0 {
		write5m = acc.CacheCreationTokensIn
	}
	cost := inPrice.Mul(decimal.NewFromInt(int64(acc.TokensIn))).
		Add(readPrice.Mul(decimal.NewFromInt(int64(acc.CachedTokensIn)))).
		Add(writePrice.Mul(decimal.NewFromInt(int64(write5m)))).
		Add(write1hPrice.Mul(decimal.NewFromInt(int64(write1h)))).
		Add(outPrice.Mul(decimal.NewFromInt(int64(acc.TokensOut)))).
		Add(audInPrice.Mul(decimal.NewFromInt(int64(acc.AudioTokensIn)))).
		Add(audOutPrice.Mul(decimal.NewFromInt(int64(acc.AudioTokensOut)))).
		Div(decimal.NewFromInt(1000))
	u, err := region.ParseUnitsRounded(cost.String())
	if err != nil {
		return 0, in, out, false
	}
	return u, in, out, true
}

// accumulatedTokens is the reported input/output totals for the usage record,
// so both sides fold their modality splits back in.
func accumulatedTokens(acc *llm.Accumulator) (int, int) {
	return acc.TokensIn + acc.CachedTokensIn + acc.CacheCreationTokensIn + acc.AudioTokensIn, acc.TokensOut + acc.AudioTokensOut
}

func boolValue(v any) bool { b, _ := v.(bool); return b }

func ipAllowed(remote string, whitelist []string) bool {
	if len(whitelist) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, raw := range whitelist {
		if addr, err := netip.ParseAddr(raw); err == nil && addr == ip {
			return true
		}
		if prefix, err := netip.ParsePrefix(raw); err == nil && prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the address the API key IP allowlist is checked against.
// X-Forwarded-For is only believed with trust_forwarded_for, i.e. when the
// listener is reachable through a proxy that sets it and nothing else.
func (s *Service) clientIP(r *http.Request) string {
	if s.cfg.TrustForwardedFor {
		if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
			return strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
	}
	return r.RemoteAddr
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Transfer-Encoding") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": "api_error", "code": code, "message": message}})
}
