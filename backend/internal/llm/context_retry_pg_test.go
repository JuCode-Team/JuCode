package llm

// Postgres-backed tests for the context-window failover and the advertised
// window range (same JUCODE_TEST_PG_DSN / Redis opt-in as cross_group_pg_test).

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entgrpmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	"github.com/jucode/jucode/internal/providerkit"
)

func upstreamContextTooLong(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"This model's maximum context length is 272000 tokens."}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// addAccount adds one more account serving e.model to group, at the given
// group-link priority (lower is tried first) and optional window override.
func (e *crossGroupEnv) addAccount(group *ent.ModelGroup, upstreamURL string, priority int, window *int) *ent.UpstreamAccount {
	e.t.Helper()
	plain, err := providerkit.NewAPIKeyCredential("upstream-key").Encode()
	if err != nil {
		e.t.Fatalf("encode credential: %v", err)
	}
	enc, err := e.cipher.Encrypt(plain)
	if err != nil {
		e.t.Fatalf("encrypt: %v", err)
	}
	acct, err := e.client.UpstreamAccount.Create().
		SetName(uuid.NewString()).
		SetBaseURL(upstreamURL).
		SetCredentialEnc(enc).
		SetNativeProtocols([]string{biz.NativeProtocolChat}).
		SetStatus(entaccount.StatusActive).
		SetSchedulable(true).
		Save(e.ctx)
	if err != nil {
		e.t.Fatalf("create account: %v", err)
	}
	if _, err := e.client.GroupAccount.Create().
		SetGroupID(group.ID).SetAccountID(acct.ID).SetEnabled(true).SetPriority(priority).Save(e.ctx); err != nil {
		e.t.Fatalf("link account: %v", err)
	}
	if _, err := e.client.UpstreamAccountModel.Create().
		SetAccountID(acct.ID).SetModelName(e.model.Name).SetEnabled(true).
		SetNillableContextWindow(window).Save(e.ctx); err != nil {
		e.t.Fatalf("account model: %v", err)
	}
	return acct
}

// contextRetryEnv: one group whose first account (272K, the model default)
// rejects as too long, and a second account with the given window override.
func contextRetryEnv(t *testing.T, secondWindow *int) (*crossGroupEnv, *int32, *int32) {
	e := newCrossGroupEnv(t, false, 0)
	e.createModel("ctx-" + uuid.NewString()[:8])
	e.model = e.client.Model.UpdateOne(e.model).SetContextWindow(272_000).SaveX(e.ctx)
	group, err := e.client.ModelGroup.Create().
		SetName("ctx-" + uuid.NewString()[:8]).
		SetBillingSource(entgroup.BillingSourceBalanceOnly).
		SetRateMultiplier(1).
		Save(e.ctx)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	e.client.ModelGroupModel.Create().SetGroupID(group.ID).SetModelID(e.model.ID).SaveX(e.ctx)
	var smallHits, largeHits int32
	e.addAccount(group, upstreamContextTooLong(t, &smallHits).URL, 1, nil)
	e.addAccount(group, upstream200(t, &largeHits, "from-large").URL, 2, secondWindow)
	return e, &smallHits, &largeHits
}

func TestContextRetryFailsOverToLargerWindow(t *testing.T) {
	large := 1_000_000
	e, smallHits, largeHits := contextRetryEnv(t, &large)
	w := e.doChat(uuid.NewString(), map[string]any{
		"model":    e.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from-large") {
		t.Fatalf("status = %d body = %s, want 200 from the larger account", w.Code, w.Body.String())
	}
	if *smallHits != 1 || *largeHits != 1 {
		t.Fatalf("hits small=%d large=%d, want 1/1", *smallHits, *largeHits)
	}
}

func TestContextRetryKeepsRejectionWithoutLargerWindow(t *testing.T) {
	// Second account inherits the same 272K: retrying it cannot help, so the
	// caller gets the rejection (and compacts) instead of a wasted attempt.
	e, smallHits, largeHits := contextRetryEnv(t, nil)
	w := e.doChat(uuid.NewString(), map[string]any{
		"model":    e.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "context_length_exceeded") {
		t.Fatalf("status = %d body = %s, want 400 context_length_exceeded", w.Code, w.Body.String())
	}
	if *smallHits != 1 || *largeHits != 0 {
		t.Fatalf("hits small=%d large=%d, want 1/0", *smallHits, *largeHits)
	}
}

func TestModelCapabilitiesWindowRange(t *testing.T) {
	large := 1_000_000
	e, _, _ := contextRetryEnv(t, &large)
	e.model = e.client.Model.UpdateOne(e.model).SetMaxOutputTokens(128_000).
		SetReasoningEfforts([]string{"low", "high"}).SaveX(e.ctx)
	models, groups, err := e.gw.models.AccessibleModelGroups(e.ctx, e.user, e.apiKey)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := e.gw.models.ModelCapabilities(e.ctx, models, groups)
	if err != nil {
		t.Fatal(err)
	}
	got := caps[e.model.ID]
	if got.ContextWindow != 272_000 || got.MaxContextWindow != 1_000_000 {
		t.Fatalf("window range = %d..%d, want 272000..1000000", got.ContextWindow, got.MaxContextWindow)
	}
	if got.MaxOutputTokens != 128_000 || len(got.ReasoningEfforts) != 2 {
		t.Fatalf("caps = %+v", got)
	}
}

func TestModelCapabilitiesGroupWindow(t *testing.T) {
	large := 1_000_000
	e, _, _ := contextRetryEnv(t, &large)
	// The group's window replaces the model default for accounts without an
	// override of their own; the 1M override still wins on its account.
	groupWindow := 400_000
	link := e.client.ModelGroupModel.Query().Where(entgrpmodel.ModelIDEQ(e.model.ID)).OnlyX(e.ctx)
	e.client.ModelGroupModel.UpdateOne(link).SetContextWindow(groupWindow).SaveX(e.ctx)
	models, groups, err := e.gw.models.AccessibleModelGroups(e.ctx, e.user, e.apiKey)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := e.gw.models.ModelCapabilities(e.ctx, models, groups)
	if err != nil {
		t.Fatal(err)
	}
	got := caps[e.model.ID]
	if got.ContextWindow != 400_000 || got.MaxContextWindow != 1_000_000 {
		t.Fatalf("window range = %d..%d, want 400000..1000000", got.ContextWindow, got.MaxContextWindow)
	}
	if w := got.Groups[link.GroupID]; w.ContextWindow != 400_000 || w.MaxContextWindow != 1_000_000 {
		t.Fatalf("group window = %+v", w)
	}
}

// upstreamImageCap answers like Azure: 400 when the request carries more
// than limit images, 200 otherwise. seen records the image count of each hit.
func upstreamImageCap(t *testing.T, limit int, seen *[]int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		n := strings.Count(string(raw), `"type":"image_url"`)
		*seen = append(*seen, n)
		w.Header().Set("Content-Type", "application/json")
		if n > limit {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"Exceeded maximum number of images (%d) allowed in the request","type":"invalid_request_error","param":"input","code":null}}`, limit)
			return
		}
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fit"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestImageCapRetriesWithOldestImagesDropped(t *testing.T) {
	e := newCrossGroupEnv(t, false, 0)
	e.createModel("img-" + uuid.NewString()[:8])
	group, err := e.client.ModelGroup.Create().
		SetName("img-" + uuid.NewString()[:8]).
		SetBillingSource(entgroup.BillingSourceBalanceOnly).
		SetRateMultiplier(1).
		Save(e.ctx)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	e.client.ModelGroupModel.Create().SetGroupID(group.ID).SetModelID(e.model.ID).SaveX(e.ctx)
	var seen []int
	acct := e.addAccount(group, upstreamImageCap(t, 2, &seen).URL, 1, nil)
	t.Cleanup(func() { accountImageLimits.Delete(acct.ID.String()) })

	parts := []map[string]any{}
	for i := 0; i < 4; i++ {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": fmt.Sprintf("data:image/png;base64,%d", i)}})
	}
	req := map[string]any{"model": e.model.Name, "messages": []map[string]any{{"role": "user", "content": parts}}}

	w := e.doChat(uuid.NewString(), req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fit") {
		t.Fatalf("status = %d body = %s, want 200 after trimming", w.Code, w.Body.String())
	}
	if len(seen) != 2 || seen[0] != 4 || seen[1] != 2 {
		t.Fatalf("upstream saw image counts %v, want [4 2]", seen)
	}
	// The cap is remembered: the next request is trimmed before it is sent.
	w = e.doChat(uuid.NewString(), req)
	if w.Code != http.StatusOK || len(seen) != 3 || seen[2] != 2 {
		t.Fatalf("second request: status = %d seen = %v, want one hit with 2 images", w.Code, seen)
	}
}

// upstreamResetFirst resets the first n connections (like a restarting relay
// behind docker-proxy), then answers 200.
func upstreamResetFirst(t *testing.T, n int32, hits *int32) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(hits, 1) <= n {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.(*net.TCPConn).SetLinger(0)
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"back"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestRelayRestartWaitsAndRetriesSameAccount(t *testing.T) {
	e := newCrossGroupEnv(t, false, 0)
	e.createModel("relay-" + uuid.NewString()[:8])
	group, err := e.client.ModelGroup.Create().
		SetName("relay-" + uuid.NewString()[:8]).
		SetBillingSource(entgroup.BillingSourceBalanceOnly).
		SetRateMultiplier(1).
		Save(e.ctx)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	e.client.ModelGroupModel.Create().SetGroupID(group.ID).SetModelID(e.model.ID).SaveX(e.ctx)
	var hits int32
	e.addAccount(group, upstreamResetFirst(t, 5, &hits).URL, 1, nil)

	start := time.Now()
	w := e.doChat(uuid.NewString(), map[string]any{
		"model":    e.model.Name,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	// Five resets are more than max_attempts (3), yet the request still lands:
	// relay waits sit outside the budget. net/http replays some resets on a
	// reused connection by itself, so the gateway sees fewer than five.
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "back") {
		t.Fatalf("status = %d body = %s, want 200 after the relay comes back", w.Code, w.Body.String())
	}
	if hits != 6 {
		t.Fatalf("hits = %d, want 6", hits)
	}
	if d := time.Since(start); d < 3*time.Second {
		t.Fatalf("took %v, want at least the 1s+2s backoff", d)
	}
}
