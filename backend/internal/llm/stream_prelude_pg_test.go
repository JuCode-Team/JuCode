package llm

// Postgres-backed tests for failing a stream over while no model output has
// reached the client (streamPrelude). Same opt-in as cross_group_pg_test.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	"github.com/jucode/jucode/internal/middleware"
)

// upstreamSSE answers 200 with the given data frames, or with a plain error
// body when status > 0.
func upstreamSSE(t *testing.T, hits *int32, status int, frames ...string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only gateway calls count: other programs on the machine may probe
		// local ports with a plain GET.
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(hits, 1)
		if status > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"Service temporarily unavailable","type":"api_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = w.Write([]byte("data: " + f + "\n\n"))
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// preludeEnv: one group, two accounts tried in order, speaking proto.
func preludeEnv(t *testing.T, proto string, first, second *httptest.Server) *crossGroupEnv {
	e := newCrossGroupEnv(t, false, 0)
	e.createModel("prelude-" + uuid.NewString()[:8])
	group, err := e.client.ModelGroup.Create().
		SetName("prelude-" + uuid.NewString()[:8]).
		SetBillingSource(entgroup.BillingSourceBalanceOnly).
		SetRateMultiplier(1).
		Save(e.ctx)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	e.client.ModelGroupModel.Create().SetGroupID(group.ID).SetModelID(e.model.ID).SaveX(e.ctx)
	for i, s := range []*httptest.Server{first, second} {
		acct := e.addAccount(group, s.URL, i+1, nil)
		e.client.UpstreamAccount.UpdateOne(acct).SetNativeProtocols([]string{proto}).SaveX(e.ctx)
	}
	return e
}

func (e *crossGroupEnv) doResponses(body map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	gin.SetMode(gin.TestMode)
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", uuid.NewString())
	c.Request = req
	c.Set(middleware.CtxUser, e.user)
	c.Set(middleware.CtxApiKey, e.apiKey)
	e.gw.HandleResponses(c)
	return w
}

const (
	deadCreated = `{"type":"response.created","response":{"id":"resp_dead"}}`
	deadFailed  = `{"type":"response.failed","response":{"id":"resp_dead","status":"failed","error":{"code":"server_error","message":"The service is busy"}}}`
)

func liveResponse(text string) []string {
	return []string{
		`{"type":"response.created","response":{"id":"resp_live"}}`,
		`{"type":"response.output_text.delta","delta":"` + text + `"}`,
		`{"type":"response.completed","response":{"id":"resp_live","usage":{"input_tokens":5,"output_tokens":2}}}`,
	}
}

func streamReq(e *crossGroupEnv) map[string]any {
	return map[string]any{"model": e.model.Name, "stream": true, "input": "hi"}
}

func TestStreamFailsOverWhenFailedBeforeOutput(t *testing.T) {
	var deadHits, liveHits int32
	e := preludeEnv(t, biz.NativeProtocolResponses,
		upstreamSSE(t, &deadHits, 0, deadCreated, deadFailed),
		upstreamSSE(t, &liveHits, 0, liveResponse("hello")...))
	w := e.doResponses(streamReq(e))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "hello") || !strings.Contains(body, "response.completed") {
		t.Fatalf("status = %d body = %s, want the second account's full stream", w.Code, body)
	}
	if strings.Contains(body, "resp_dead") {
		t.Fatalf("client saw the failed attempt's frames: %s", body)
	}
	if deadHits != 1 || liveHits != 1 {
		t.Fatalf("hits dead=%d live=%d, want 1/1", deadHits, liveHits)
	}
}

func TestStreamKeepsFailureAfterOutput(t *testing.T) {
	var deadHits, liveHits int32
	e := preludeEnv(t, biz.NativeProtocolResponses,
		upstreamSSE(t, &deadHits, 0, deadCreated, `{"type":"response.output_text.delta","delta":"partial"}`, deadFailed),
		upstreamSSE(t, &liveHits, 0, liveResponse("hello")...))
	w := e.doResponses(streamReq(e))
	body := w.Body.String()
	// Output already reached the client: no failover, the upstream's own
	// frames (including the failure) go out as they came.
	if !strings.Contains(body, "partial") || !strings.Contains(body, "resp_dead") || !strings.Contains(body, "response.failed") {
		t.Fatalf("body = %s, want the first account's stream with its failure", body)
	}
	if liveHits != 0 {
		t.Fatalf("live hits = %d, want 0", liveHits)
	}
}

func TestStreamFailureNotRetryableIsForwarded(t *testing.T) {
	var deadHits, liveHits int32
	tooLong := `{"type":"response.failed","response":{"id":"resp_dead","status":"failed","error":{"code":"context_length_exceeded","message":"too long"}}}`
	e := preludeEnv(t, biz.NativeProtocolResponses,
		upstreamSSE(t, &deadHits, 0, deadCreated, tooLong),
		upstreamSSE(t, &liveHits, 0, liveResponse("hello")...))
	w := e.doResponses(streamReq(e))
	// A request-level failure fails the same everywhere: forward it (with the
	// held frames) instead of trying the next account.
	if body := w.Body.String(); !strings.Contains(body, "resp_dead") || !strings.Contains(body, "context_length_exceeded") {
		t.Fatalf("body = %s, want the held frames and the failure", body)
	}
	if liveHits != 0 {
		t.Fatalf("live hits = %d, want 0", liveHits)
	}
}

func TestStreamAllFailBeforeOutputEndsInBand(t *testing.T) {
	var deadHits, downHits int32
	e := preludeEnv(t, biz.NativeProtocolResponses,
		upstreamSSE(t, &deadHits, 0, deadCreated, deadFailed),
		upstreamSSE(t, &downHits, http.StatusServiceUnavailable))
	w := e.doResponses(streamReq(e))
	body := w.Body.String()
	// The SSE status line went out with the first attempt, so the last
	// failure must end the stream in-band, not append a JSON error body.
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status = %d content-type = %q", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "response.failed") || strings.Contains(body, "resp_dead") {
		t.Fatalf("body = %s, want one in-band response.failed and no dead frames", body)
	}
	if deadHits != 1 || downHits != 1 {
		t.Fatalf("hits = %d/%d, want 1/1", deadHits, downHits)
	}
}
