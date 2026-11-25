package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKlingVideoSubmitPoll(t *testing.T) {
	var submitPath, auth, pollPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.Method == http.MethodPost {
			submitPath = r.URL.Path
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"k1","task_status":"submitted"}}`))
			return
		}
		pollPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"task_status":"succeed","task_result":{"videos":[{"url":"https://k/v.mp4"}]}}}`))
	}))
	defer srv.Close()

	ad := klingVideo{}
	// image2video path chosen when image present
	sub, err := ad.Submit(context.Background(), srv.URL, "kk", "kling-v2-master",
		[]byte(`{"prompt":"a cat","image_url":"https://img"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitPath != "/v1/videos/image2video" {
		t.Fatalf("submit path: %q", submitPath)
	}
	if auth != "Bearer kk" {
		t.Fatalf("auth: %q", auth)
	}
	if sub.UpstreamTaskID != "image2video/k1" {
		t.Fatalf("task id: %q", sub.UpstreamTaskID)
	}
	poll, err := ad.Poll(context.Background(), srv.URL, "kk", sub.UpstreamTaskID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !strings.HasSuffix(pollPath, "/v1/videos/image2video/k1") {
		t.Fatalf("poll path: %q", pollPath)
	}
	if poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://k/v.mp4" {
		t.Fatalf("poll: %+v", poll)
	}
}

func TestViduVideoSubmitPoll(t *testing.T) {
	var auth, submitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.Method == http.MethodPost {
			submitPath = r.URL.Path
			_, _ = w.Write([]byte(`{"task_id":"v1","state":"created"}`))
			return
		}
		_, _ = w.Write([]byte(`{"state":"success","creations":[{"url":"https://vidu/v.mp4"}]}`))
	}))
	defer srv.Close()

	ad := viduVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "vk", "vidu-2.0", []byte(`{"prompt":"p"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitPath != "/ent/v2/text2video" {
		t.Fatalf("submit path: %q", submitPath)
	}
	if auth != "Token vk" {
		t.Fatalf("auth: %q (want Token prefix)", auth)
	}
	if sub.UpstreamTaskID != "v1" {
		t.Fatalf("task id: %q", sub.UpstreamTaskID)
	}
	poll, err := ad.Poll(context.Background(), srv.URL, "vk", "v1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://vidu/v.mp4" {
		t.Fatalf("poll: %+v", poll)
	}
}

func TestJimengVideoSubmitPoll(t *testing.T) {
	var submitQuery, pollBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "CVSync2AsyncSubmitTask" {
			submitQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"data":{"task_id":"j1"}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		pollBody = string(b)
		_, _ = w.Write([]byte(`{"data":{"status":"done","video_url":"https://jm/v.mp4"}}`))
	}))
	defer srv.Close()

	ad := jimengVideo{}
	sub, err := ad.Submit(context.Background(), srv.URL, "jk", "jimeng_vgfm_t2v_l20", []byte(`{"prompt":"p"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !strings.Contains(submitQuery, "CVSync2AsyncSubmitTask") {
		t.Fatalf("submit query: %q", submitQuery)
	}
	if sub.UpstreamTaskID != "jimeng_vgfm_t2v_l20::j1" {
		t.Fatalf("task id: %q", sub.UpstreamTaskID)
	}
	poll, err := ad.Poll(context.Background(), srv.URL, "jk", sub.UpstreamTaskID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	// req_key + task_id are split back out for the poll body
	var pb struct {
		ReqKey string `json:"req_key"`
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal([]byte(pollBody), &pb)
	if pb.ReqKey != "jimeng_vgfm_t2v_l20" || pb.TaskID != "j1" {
		t.Fatalf("poll body split wrong: %s", pollBody)
	}
	if poll.Status != VideoStatusSucceeded || poll.VideoURL != "https://jm/v.mp4" {
		t.Fatalf("poll: %+v", poll)
	}
}
