package llm

// video_extra.go adds three more async-video adaptors — Kling (可灵),
// Vidu (生数) and Jimeng (即梦) — following the VideoAdaptor pattern. Each
// upstream has its own auth scheme and submit/poll shapes, so they build their
// own HTTP requests instead of the shared Bearer helpers.
//
// Some upstreams need the submit-time sub-path (Kling text2video vs
// image2video) or model key (Jimeng req_key) again at poll time; since the
// VideoAdaptor.Poll signature only carries the task id, those are encoded into
// the stored UpstreamTaskID and split back out on poll.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func videoDoJSON(ctx context.Context, method, url, authValue string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authValue != "" {
		req.Header.Set("Authorization", authValue)
	}
	return genDo(req)
}

// =========================== Kling (可灵 / 快手) ===========================
//
// Submit  POST {base}/v1/videos/{text2video|image2video} -> {data:{task_id,task_status}}
// Poll    GET  {base}/v1/videos/{text2video|image2video}/{taskID}
// Auth: Authorization: Bearer <token> (a JWT assembled from ak/sk, or an
// aggregator's key). The poll sub-path is encoded into the task id as
// "<subpath>/<id>".

type klingVideo struct{}

func (klingVideo) Format() string { return "kling" }

func (klingVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	sub := "text2video"
	payload := map[string]any{"model_name": upstreamModel, "prompt": cr.Prompt}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		sub = "image2video"
		payload["image"] = img
	}
	if d := firstNonZero(cr.Duration, cr.Seconds); d > 0 {
		payload["duration"] = fmt.Sprintf("%d", d)
	}
	buf, _ := json.Marshal(payload)
	url := trimBase(baseURL) + "/v1/videos/" + sub
	status, respBody, err := videoDoJSON(ctx, http.MethodPost, url, "Bearer "+apiKey, buf)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("kling submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Data struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.Data.TaskID == "" {
		return VideoSubmitResult{}, fmt.Errorf("kling submit: no task_id")
	}
	return VideoSubmitResult{UpstreamTaskID: sub + "/" + out.Data.TaskID}, nil
}

func (klingVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	sub, id := "text2video", upstreamTaskID
	if i := strings.IndexByte(upstreamTaskID, '/'); i >= 0 {
		sub, id = upstreamTaskID[:i], upstreamTaskID[i+1:]
	}
	url := trimBase(baseURL) + "/v1/videos/" + sub + "/" + id
	status, respBody, err := videoDoJSON(ctx, http.MethodGet, url, "Bearer "+apiKey, nil)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("kling poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Data struct {
			TaskStatus    string `json:"task_status"`
			TaskStatusMsg string `json:"task_status_msg"`
			TaskResult    struct {
				Videos []struct {
					URL string `json:"url"`
				} `json:"videos"`
			} `json:"task_result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	res := VideoPollResult{Status: mapKlingStatus(out.Data.TaskStatus), ErrorMessage: out.Data.TaskStatusMsg}
	if len(out.Data.TaskResult.Videos) > 0 {
		res.VideoURL = out.Data.TaskResult.Videos[0].URL
	}
	return res, nil
}

func mapKlingStatus(s string) string {
	switch strings.ToLower(s) {
	case "submitted":
		return VideoStatusQueued
	case "processing":
		return VideoStatusRunning
	case "succeed", "succeeded", "success":
		return VideoStatusSucceeded
	case "failed", "fail":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}

// =========================== Vidu (生数科技) ===========================
//
// Submit  POST {base}/ent/v2/{text2video|img2video} -> {task_id,state}
// Poll    GET  {base}/ent/v2/tasks/{taskID}/creations -> {state,creations:[{url}]}
// Auth: Authorization: Token <key> (NOT Bearer).

type viduVideo struct{}

func (viduVideo) Format() string { return "vidu" }

func (viduVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	path := "text2video"
	payload := map[string]any{"model": upstreamModel, "prompt": cr.Prompt}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		path = "img2video"
		payload["images"] = []string{img}
	}
	if cr.Size != "" {
		payload["resolution"] = cr.Size
	}
	if d := firstNonZero(cr.Duration, cr.Seconds); d > 0 {
		payload["duration"] = d
	}
	buf, _ := json.Marshal(payload)
	url := trimBase(baseURL) + "/ent/v2/" + path
	status, respBody, err := videoDoJSON(ctx, http.MethodPost, url, "Token "+apiKey, buf)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("vidu submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.TaskID == "" {
		return VideoSubmitResult{}, fmt.Errorf("vidu submit: no task_id")
	}
	return VideoSubmitResult{UpstreamTaskID: out.TaskID}, nil
}

func (viduVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	url := trimBase(baseURL) + "/ent/v2/tasks/" + upstreamTaskID + "/creations"
	status, respBody, err := videoDoJSON(ctx, http.MethodGet, url, "Token "+apiKey, nil)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("vidu poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		State     string `json:"state"`
		ErrCode   string `json:"err_code"`
		Creations []struct {
			URL string `json:"url"`
		} `json:"creations"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	res := VideoPollResult{Status: mapViduStatus(out.State), ErrorMessage: out.ErrCode}
	if len(out.Creations) > 0 {
		res.VideoURL = out.Creations[0].URL
	}
	return res, nil
}

func mapViduStatus(s string) string {
	switch strings.ToLower(s) {
	case "created", "queueing":
		return VideoStatusQueued
	case "processing":
		return VideoStatusRunning
	case "success", "succeeded":
		return VideoStatusSucceeded
	case "failed", "fail":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}

// =========================== Jimeng (即梦 / 火山视觉) ===========================
//
// Jimeng video is Volcengine's Visual async task API (CVSync2AsyncSubmitTask /
// CVSync2AsyncGetResult, req_key = the model). Officially it requires
// Volcengine AK/SK request signing; most gateway deployments reach it through
// an aggregator that accepts a Bearer key and mirrors the visual shape, which
// is what this adaptor targets.
//
// TODO(jimeng-native): add direct Volcengine AK/SK (Signature V4) signing —
// store credentials as "ak:sk" in the provider key — for official direct use.
//
// The req_key (model) is needed again at poll time, so it is encoded into the
// task id as "<req_key>::<task_id>".

type jimengVideo struct{}

func (jimengVideo) Format() string { return "jimeng" }

func (jimengVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	payload := map[string]any{"req_key": upstreamModel, "prompt": cr.Prompt}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		payload["image_urls"] = []string{img}
	}
	buf, _ := json.Marshal(payload)
	url := trimBase(baseURL) + "/?Action=CVSync2AsyncSubmitTask&Version=2022-08-31"
	status, respBody, err := videoDoJSON(ctx, http.MethodPost, url, "Bearer "+apiKey, buf)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("jimeng submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Data struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.Data.TaskID == "" {
		return VideoSubmitResult{}, fmt.Errorf("jimeng submit: no task_id")
	}
	return VideoSubmitResult{UpstreamTaskID: upstreamModel + "::" + out.Data.TaskID}, nil
}

func (jimengVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	reqKey, id := "", upstreamTaskID
	if i := strings.Index(upstreamTaskID, "::"); i >= 0 {
		reqKey, id = upstreamTaskID[:i], upstreamTaskID[i+2:]
	}
	buf, _ := json.Marshal(map[string]any{"req_key": reqKey, "task_id": id})
	url := trimBase(baseURL) + "/?Action=CVSync2AsyncGetResult&Version=2022-08-31"
	status, respBody, err := videoDoJSON(ctx, http.MethodPost, url, "Bearer "+apiKey, buf)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("jimeng poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Data struct {
			Status   string `json:"status"`
			VideoURL string `json:"video_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	return VideoPollResult{Status: mapJimengStatus(out.Data.Status), VideoURL: out.Data.VideoURL}, nil
}

func mapJimengStatus(s string) string {
	switch strings.ToLower(s) {
	case "in_queue", "generating":
		return VideoStatusRunning
	case "done":
		return VideoStatusSucceeded
	case "not_found", "expired", "failed", "fail":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}
