package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDashScopeImageBlockingPoll drives the native Wanx text-to-image flow:
// submit (with X-DashScope-Async) then poll PENDING -> SUCCEEDED, returning an
// OpenAI images-shaped body with data[].url and a usage image count.
func TestDashScopeImageBlockingPoll(t *testing.T) {
	old := dashscopeImagePollInterval
	dashscopeImagePollInterval = 5 * time.Millisecond
	defer func() { dashscopeImagePollInterval = old }()

	var submitPath, asyncHdr, submitBody string
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			submitPath = r.URL.Path
			asyncHdr = r.Header.Get("X-DashScope-Async")
			b, _ := io.ReadAll(r.Body)
			submitBody = string(b)
			_, _ = w.Write([]byte(`{"output":{"task_id":"img-1","task_status":"PENDING"}}`))
			return
		}
		polls++
		if polls < 2 {
			_, _ = w.Write([]byte(`{"output":{"task_status":"RUNNING"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"output":{"task_status":"SUCCEEDED","results":[{"url":"https://img/a.png"}]},"usage":{"image_count":1}}`))
	}))
	defer srv.Close()

	p := &openaiProvider{id: uuid.New(), name: "wanx", baseURL: srv.URL, apiKey: "k", format: "dashscope", hc: srv.Client()}
	resp, err := p.ForwardGeneration(context.Background(), &GenerationRequest{
		Kind:     EndpointImageGeneration,
		Model:    "wanx2.1-t2i-turbo",
		JSONBody: []byte(`{"model":"wanx2.1-t2i-turbo","prompt":"a fox","size":"1024x1024","n":1}`),
	})
	if err != nil {
		t.Fatalf("ForwardGeneration: %v", err)
	}
	if submitPath != "/api/v1/services/aigc/text2image/image-synthesis" {
		t.Fatalf("submit path: %q", submitPath)
	}
	if asyncHdr != "enable" {
		t.Fatalf("async header: %q", asyncHdr)
	}
	// size normalized to Wanx form
	if !strings.Contains(submitBody, `"size":"1024*1024"`) {
		t.Fatalf("size not normalized: %s", submitBody)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d body=%s", resp.StatusCode, resp.Body)
	}
	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].URL != "https://img/a.png" {
		t.Fatalf("data: %+v", out.Data)
	}
	if resp.Usage == nil || resp.Usage.Images != 1 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}
