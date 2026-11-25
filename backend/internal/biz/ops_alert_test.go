package biz

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type staticOpsAlertConfig struct {
	cfg OpsAlertConfig
	err error
}

func (s staticOpsAlertConfig) GetOpsAlertConfig(context.Context) (OpsAlertConfig, error) {
	return s.cfg, s.err
}

// isOpsAlertDelivery tells a webhook delivery apart from unrelated traffic.
// The test server listens on a loopback port that other local processes may
// probe (for example a GET / from a port scanner), and counting those as
// deliveries made the call-count assertions flaky.
func isOpsAlertDelivery(r *http.Request) bool {
	return r.Method == http.MethodPost && r.Header.Get("Content-Type") == "application/json"
}

func testOpsEvent() OpsAlertEvent {
	return OpsAlertEvent{
		Event: "incident.opened", Title: "服务异常已打开", Severity: "critical",
		ResourceType: "model_group", ResourceName: "primary", ResourceKey: "private-id",
		State: "down", Message: "状态页模型组进入异常状态。",
		OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func TestOpsAlertSendsSignedGenericWebhook(t *testing.T) {
	var received []byte
	var timestamp, signature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		timestamp = r.Header.Get("X-JuCode-Alert-Timestamp")
		signature = r.Header.Get("X-JuCode-Alert-Signature")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	const secret = "signing-secret"
	svc := newOpsAlertSvc(staticOpsAlertConfig{cfg: OpsAlertConfig{
		Enabled: true, WebhookType: OpsAlertWebhookGeneric, WebhookURL: server.URL,
		SigningSecret: secret, Timeout: time.Second, MaxRetries: 0, SilenceWindow: time.Minute,
	}}, nil, server.Client(), nil)

	sent, err := svc.Notify(context.Background(), testOpsEvent())
	if err != nil || !sent {
		t.Fatalf("Notify() = sent %v, err %v", sent, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["event"] != "incident.opened" || payload["resource_name"] != "primary" {
		t.Fatalf("unexpected payload: %s", received)
	}
	if _, leaked := payload["ResourceKey"]; leaked || bytes.Contains(received, []byte("private-id")) {
		t.Fatalf("internal resource key leaked: %s", received)
	}
	if string(received) == "" || timestamp != strconv.FormatInt(testOpsEvent().OccurredAt.Unix(), 10) {
		t.Fatalf("missing body or timestamp")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "\n"))
	_, _ = mac.Write(received)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if signature != want {
		t.Fatalf("signature = %q, want %q", signature, want)
	}
}

func TestOpsAlertDeduplicatesWithinSilenceWindow(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOpsAlertDelivery(r) {
			calls.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	svc := newOpsAlertSvc(staticOpsAlertConfig{cfg: OpsAlertConfig{
		Enabled: true, WebhookURL: server.URL, Timeout: time.Second,
		MaxRetries: 0, SilenceWindow: time.Hour,
	}}, nil, server.Client(), nil)

	first, err := svc.Notify(context.Background(), testOpsEvent())
	if err != nil || !first {
		t.Fatalf("first Notify() = sent %v, err %v", first, err)
	}
	second, err := svc.Notify(context.Background(), testOpsEvent())
	if err != nil || second {
		t.Fatalf("second Notify() = sent %v, err %v", second, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("webhook calls = %d, want 1", got)
	}
}

func TestOpsAlertUnconfiguredIsNoop(t *testing.T) {
	for _, cfg := range []OpsAlertConfig{
		{},
		{Enabled: true},
		{Enabled: false, WebhookURL: "http://example.invalid"},
	} {
		svc := newOpsAlertSvc(staticOpsAlertConfig{cfg: cfg}, nil, nil, nil)
		sent, err := svc.Notify(context.Background(), testOpsEvent())
		if err != nil || sent {
			t.Fatalf("Notify(%+v) = sent %v, err %v", cfg, sent, err)
		}
	}
}

func TestOpsAlertRetriesAreBounded(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOpsAlertDelivery(r) {
			calls.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	svc := newOpsAlertSvc(staticOpsAlertConfig{cfg: OpsAlertConfig{
		Enabled: true, WebhookURL: server.URL, Timeout: time.Second,
		MaxRetries: 2, SilenceWindow: time.Minute,
	}}, nil, server.Client(), nil)

	if sent, err := svc.Notify(context.Background(), testOpsEvent()); err == nil || sent {
		t.Fatalf("Notify() = sent %v, err %v; want bounded failure", sent, err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("webhook calls = %d, want 3", got)
	}
}
