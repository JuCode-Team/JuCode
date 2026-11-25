package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestProviderFormat(t *testing.T) {
	// explicit api_format wins over inference
	if got := ProviderFormat(&ent.UpstreamAccount{BaseURL: "https://x", APIFormat: "volcengine"}); got != "volcengine" {
		t.Fatalf("explicit: got %q", got)
	}
	// inference from base_url host
	infer := map[string]string{
		"https://dashscope.aliyuncs.com":            "dashscope",
		"https://ark.cn-beijing.volces.com/api/v3":  "volcengine",
		"https://open.bigmodel.cn/api/paas/v4":      "zhipu",
		"https://api.siliconflow.cn/v1":             "siliconflow",
		"https://api.minimaxi.com":                  "minimax",
		"https://generativelanguage.googleapis.com": "gemini",
		"https://api.openai.com/v1":                 "openai",
	}
	for base, want := range infer {
		if got := ProviderFormat(&ent.UpstreamAccount{BaseURL: base}); got != want {
			t.Fatalf("infer %s: got %q want %q", base, got, want)
		}
	}
}

func TestDashScopeVideoSubmitPoll(t *testing.T) {
	var submitPath, asyncHdr, submitBody, pollPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			submitPath = r.URL.Path
			asyncHdr = r.Header.Get("X-DashScope-Async")
			b, _ := io.ReadAll(r.Body)
			submitBody = string(b)
			_, _ = w.Write([]byte(`{"output":{"task_id":"task-123","task_status":"PENDING"}}`))
			return
		}
		pollPath = r.URL.Path
		_, _ = w.Write([]byte(`{"output":{"task_status":"SUCCEEDED","video_url":"https://cdn/v.mp4"},"usage":{"duration":5}}`))
	}))
	defer srv.Close()

	ad := dashscopeVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "k", "wanx2.1-t2v-turbo",
		[]byte(`{"model":"wanx2.1-t2v-turbo","prompt":"a cat","size":"1280*720"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitPath != "/api/v1/services/aigc/video-generation/video-synthesis" {
		t.Fatalf("submit path: %q", submitPath)
	}
	if asyncHdr != "enable" {
		t.Fatalf("async header: %q", asyncHdr)
	}
	if sub.UpstreamTaskID != "task-123" {
		t.Fatalf("task id: %q", sub.UpstreamTaskID)
	}
	// request translation: prompt -> input.prompt, size -> parameters.size
	var payload struct {
		Input      map[string]any `json:"input"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(submitBody), &payload); err != nil {
		t.Fatalf("submit body not json: %v", err)
	}
	if payload.Input["prompt"] != "a cat" {
		t.Fatalf("input.prompt: %v", payload.Input["prompt"])
	}
	if payload.Parameters["size"] != "1280*720" {
		t.Fatalf("parameters.size: %v", payload.Parameters["size"])
	}

	poll, err := ad.Poll(context.Background(), srv.URL, "k", "task-123")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if poll.Status != VideoStatusSucceeded {
		t.Fatalf("status: %q", poll.Status)
	}
	if poll.VideoURL != "https://cdn/v.mp4" {
		t.Fatalf("video url: %q", poll.VideoURL)
	}
	if poll.BilledSeconds != 5 {
		t.Fatalf("seconds: %d", poll.BilledSeconds)
	}
	_ = pollPath
}

func TestVolcengineVideoSubmitPoll(t *testing.T) {
	var submitBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			submitBody = string(b)
			_, _ = w.Write([]byte(`{"id":"cgt-abc"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"succeeded","content":{"video_url":"https://cdn/x.mp4"},"usage":{"completion_tokens":100}}`))
	}))
	defer srv.Close()

	ad := volcengineVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "k", "doubao-seedance",
		[]byte(`{"model":"doubao-seedance","prompt":"a dog","image_url":"https://img"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sub.UpstreamTaskID != "cgt-abc" {
		t.Fatalf("task id: %q", sub.UpstreamTaskID)
	}
	// translation: content array [{text}, {image_url}]
	var payload struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(submitBody), &payload); err != nil {
		t.Fatalf("submit body: %v", err)
	}
	if len(payload.Content) != 2 {
		t.Fatalf("content len: %d", len(payload.Content))
	}
	if payload.Content[0]["type"] != "text" || payload.Content[0]["text"] != "a dog" {
		t.Fatalf("content[0]: %v", payload.Content[0])
	}
	if payload.Content[1]["type"] != "image_url" {
		t.Fatalf("content[1]: %v", payload.Content[1])
	}

	poll, err := ad.Poll(context.Background(), srv.URL, "k", "cgt-abc")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://cdn/x.mp4" {
		t.Fatalf("poll: %+v", poll)
	}
}

func TestZhipuVideoSubmitPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"zid","task_status":"PROCESSING"}`))
			return
		}
		_, _ = w.Write([]byte(`{"task_status":"SUCCESS","video_result":[{"url":"https://z/v.mp4"}]}`))
	}))
	defer srv.Close()
	ad := zhipuVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "k", "cogvideox", []byte(`{"prompt":"p"}`))
	if err != nil || sub.UpstreamTaskID != "zid" {
		t.Fatalf("submit: %v %q", err, sub.UpstreamTaskID)
	}
	poll, err := ad.Poll(context.Background(), srv.URL, "k", "zid")
	if err != nil || poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://z/v.mp4" {
		t.Fatalf("poll: %v %+v", err, poll)
	}
}

func TestSiliconFlowVideoSubmitPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video/submit":
			_, _ = w.Write([]byte(`{"requestId":"rq1"}`))
		case "/video/status":
			_, _ = w.Write([]byte(`{"status":"Succeed","results":{"videos":[{"url":"https://s/v.mp4"}]}}`))
		}
	}))
	defer srv.Close()
	ad := siliconflowVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "k", "Wan2.1", []byte(`{"prompt":"p"}`))
	if err != nil || sub.UpstreamTaskID != "rq1" {
		t.Fatalf("submit: %v %q", err, sub.UpstreamTaskID)
	}
	poll, err := ad.Poll(context.Background(), srv.URL, "k", "rq1")
	if err != nil || poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://s/v.mp4" {
		t.Fatalf("poll: %v %+v", err, poll)
	}
}

func TestGeminiVideoVeo(t *testing.T) {
	var submitPath, keyHdr, pollPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyHdr = r.Header.Get("x-goog-api-key")
		if r.Method == http.MethodPost {
			submitPath = r.URL.Path
			_, _ = w.Write([]byte(`{"name":"models/veo/operations/op1"}`))
			return
		}
		pollPath = r.URL.Path
		_, _ = w.Write([]byte(`{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://veo/v.mp4"}}]}}}`))
	}))
	defer srv.Close()

	ad := geminiVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "gkey", "veo-3.0-generate",
		[]byte(`{"model":"veo-3.0-generate","prompt":"a wave"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !strings.HasSuffix(submitPath, ":predictLongRunning") {
		t.Fatalf("submit path: %q", submitPath)
	}
	if keyHdr != "gkey" {
		t.Fatalf("api key header: %q", keyHdr)
	}
	if sub.UpstreamTaskID != "models/veo/operations/op1" {
		t.Fatalf("op name: %q", sub.UpstreamTaskID)
	}
	poll, err := ad.Poll(context.Background(), srv.URL, "gkey", sub.UpstreamTaskID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://veo/v.mp4" {
		t.Fatalf("poll: %+v", poll)
	}
	if !strings.Contains(pollPath, "operations/op1") {
		t.Fatalf("poll path: %q", pollPath)
	}
}

func TestMiniMaxVideoThreeStage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/video_generation":
			_, _ = w.Write([]byte(`{"task_id":"mt1"}`))
		case r.URL.Path == "/v1/query/video_generation":
			_, _ = w.Write([]byte(`{"status":"Success","file_id":"f9"}`))
		case r.URL.Path == "/v1/files/retrieve":
			_, _ = w.Write([]byte(`{"file":{"download_url":"https://m/v.mp4"}}`))
		}
	}))
	defer srv.Close()
	ad := minimaxVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "k", "MiniMax-Hailuo", []byte(`{"prompt":"p"}`))
	if err != nil || sub.UpstreamTaskID != "mt1" {
		t.Fatalf("submit: %v %q", err, sub.UpstreamTaskID)
	}
	poll, err := ad.Poll(context.Background(), srv.URL, "k", "mt1")
	if err != nil || poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://m/v.mp4" {
		t.Fatalf("poll (3-stage): %v %+v", err, poll)
	}
}
