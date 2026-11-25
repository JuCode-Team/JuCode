package llm

// video.go implements the async video-generation adaptors. Each upstream has
// a different submit/poll dialect, so a VideoAdaptor
// translates the OpenAI-Sora-shaped client request into the upstream's task
// API and normalizes its poll response back. The gateway (video_handlers.go)
// owns persistence + billing; adaptors are pure HTTP translators.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jucode/jucode/internal/data/ent"
)

// genHTTPClient is the shared client for generation adaptor calls. Video
// submit/poll are ordinary short JSON round-trips (not the long-lived
// streaming relay the chat transport is tuned for), so a plain bounded client
// is appropriate.
var genHTTPClient = &http.Client{Timeout: 60 * time.Second}

// Video task lifecycle values shared with the ent enum + client surface.
const (
	VideoStatusQueued    = "queued"
	VideoStatusRunning   = "running"
	VideoStatusSucceeded = "succeeded"
	VideoStatusFailed    = "failed"
)

// VideoSubmitResult is what an adaptor returns after creating an upstream job.
type VideoSubmitResult struct {
	UpstreamTaskID string
}

// VideoPollResult is the normalized upstream job state.
type VideoPollResult struct {
	Status        string // queued | running | succeeded | failed
	VideoURL      string // populated on success
	BilledSeconds int    // duration signal for billing, when the upstream reports it
	ErrorMessage  string
}

// VideoAdaptor translates the OpenAI-shaped video request to one upstream's
// async task API and back.
type VideoAdaptor interface {
	Format() string
	// Submit creates the upstream job. `body` is the raw client JSON (model
	// already rewritten to upstreamModel by the gateway).
	Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error)
	// Poll queries a previously-submitted job.
	Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error)
}

// videoAdaptorFor returns the adaptor for a provider format, or nil when the
// format has no async-video support wired yet.
func videoAdaptorFor(format string) VideoAdaptor {
	switch format {
	case "dashscope":
		return dashscopeVideo{}
	case "volcengine":
		return volcengineVideo{}
	case "zhipu":
		return zhipuVideo{}
	case "siliconflow":
		return siliconflowVideo{}
	case "minimax":
		return minimaxVideo{}
	case "gemini":
		return geminiVideo{}
	case "kling":
		return klingVideo{}
	case "vidu":
		return viduVideo{}
	case "jimeng":
		return jimengVideo{}
	default:
		return nil
	}
}

// ProviderFormat resolves the generation dialect for an account: the explicit
// api_format when set, else inferred from the base_url host, else "openai".
func ProviderFormat(a *ent.UpstreamAccount) string {
	if a == nil {
		return "openai"
	}
	if f := strings.TrimSpace(a.APIFormat); f != "" {
		return f
	}
	return inferFormatFromBaseURL(a.BaseURL)
}

func inferFormatFromBaseURL(baseURL string) string {
	host := strings.ToLower(baseURL)
	switch {
	case strings.Contains(host, "dashscope.aliyuncs.com"), strings.Contains(host, "aliyuncs.com"):
		return "dashscope"
	case strings.Contains(host, "volces.com"), strings.Contains(host, "ark.cn-"):
		return "volcengine"
	case strings.Contains(host, "bigmodel.cn"):
		return "zhipu"
	case strings.Contains(host, "siliconflow"):
		return "siliconflow"
	case strings.Contains(host, "minimax"):
		return "minimax"
	case strings.Contains(host, "generativelanguage.googleapis.com"):
		return "gemini"
	case strings.Contains(host, "klingai") || strings.Contains(host, "kling"):
		return "kling"
	case strings.Contains(host, "vidu"):
		return "vidu"
	case strings.Contains(host, "jimeng"):
		return "jimeng"
	default:
		return "openai"
	}
}

// --- shared http helpers ---

func genJSONPost(ctx context.Context, url, apiKey string, body []byte, extraHeaders map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return genDo(req)
}

func genJSONGet(ctx context.Context, url, apiKey string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return genDo(req)
}

func genDo(req *http.Request) (int, []byte, error) {
	resp, err := genHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

func trimBase(baseURL string) string { return strings.TrimRight(baseURL, "/") }

// clientVideoRequest is the subset of the OpenAI-Sora-shaped client body the
// adaptors read. Unknown fields are ignored; providers get only what maps.
type clientVideoRequest struct {
	Model    string `json:"model"`
	Prompt   string `json:"prompt"`
	ImageURL string `json:"image_url"` // for image-to-video
	Image    string `json:"image"`     // alt field some clients use
	Size     string `json:"size"`
	Duration int    `json:"duration"`
	Seconds  int    `json:"seconds"` // OpenAI Sora uses "seconds"
}

func parseClientVideo(body []byte) clientVideoRequest {
	var r clientVideoRequest
	_ = json.Unmarshal(body, &r)
	return r
}

// =========================== DashScope (Alibaba Wanx) ===========================

type dashscopeVideo struct{}

func (dashscopeVideo) Format() string { return "dashscope" }

func (dashscopeVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	input := map[string]any{"prompt": cr.Prompt}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		input["img_url"] = img
	}
	params := map[string]any{}
	if cr.Size != "" {
		params["size"] = cr.Size
	}
	if d := firstNonZero(cr.Duration, cr.Seconds); d > 0 {
		params["duration"] = d
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      upstreamModel,
		"input":      input,
		"parameters": params,
	})
	url := trimBase(baseURL) + "/api/v1/services/aigc/video-generation/video-synthesis"
	status, respBody, err := genJSONPost(ctx, url, apiKey, payload, map[string]string{"X-DashScope-Async": "enable"})
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("dashscope submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Output struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.Output.TaskID == "" {
		return VideoSubmitResult{}, fmt.Errorf("dashscope submit: no task_id in response")
	}
	return VideoSubmitResult{UpstreamTaskID: out.Output.TaskID}, nil
}

func (dashscopeVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	url := trimBase(baseURL) + "/api/v1/tasks/" + upstreamTaskID
	status, respBody, err := genJSONGet(ctx, url, apiKey)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("dashscope poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Output struct {
			TaskStatus string `json:"task_status"`
			VideoURL   string `json:"video_url"`
			Results    []struct {
				URL string `json:"url"`
			} `json:"results"`
			Message string `json:"message"`
		} `json:"output"`
		Usage struct {
			Duration int `json:"duration"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	res := VideoPollResult{
		Status:        mapDashScopeStatus(out.Output.TaskStatus),
		VideoURL:      firstNonEmpty(out.Output.VideoURL, firstResultURL(out.Output.Results)),
		BilledSeconds: out.Usage.Duration,
		ErrorMessage:  out.Output.Message,
	}
	return res, nil
}

func mapDashScopeStatus(s string) string {
	switch strings.ToUpper(s) {
	case "PENDING", "RUNNING":
		return VideoStatusRunning
	case "SUCCEEDED":
		return VideoStatusSucceeded
	case "FAILED", "CANCELED", "CANCELLED", "UNKNOWN":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}

func firstResultURL(rs []struct {
	URL string `json:"url"`
},
) string {
	if len(rs) > 0 {
		return rs[0].URL
	}
	return ""
}

// =========================== Volcengine Ark (Doubao Seedance) ===========================

type volcengineVideo struct{}

func (volcengineVideo) Format() string { return "volcengine" }

func (volcengineVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	content := []map[string]any{{"type": "text", "text": cr.Prompt}}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": img},
		})
	}
	payload := map[string]any{"model": upstreamModel, "content": content}
	if cr.Size != "" {
		payload["ratio"] = cr.Size
	}
	if d := firstNonZero(cr.Duration, cr.Seconds); d > 0 {
		payload["duration"] = d
	}
	buf, _ := json.Marshal(payload)
	url := trimBase(baseURL) + "/api/v3/contents/generations/tasks"
	status, respBody, err := genJSONPost(ctx, url, apiKey, buf, nil)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("volcengine submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.ID == "" {
		return VideoSubmitResult{}, fmt.Errorf("volcengine submit: no task id in response")
	}
	return VideoSubmitResult{UpstreamTaskID: out.ID}, nil
}

func (volcengineVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	url := trimBase(baseURL) + "/api/v3/contents/generations/tasks/" + upstreamTaskID
	status, respBody, err := genJSONGet(ctx, url, apiKey)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("volcengine poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Status  string `json:"status"`
		Content struct {
			VideoURL string `json:"video_url"`
		} `json:"content"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	return VideoPollResult{
		Status:       mapVolcengineStatus(out.Status),
		VideoURL:     out.Content.VideoURL,
		ErrorMessage: out.Error.Message,
	}, nil
}

func mapVolcengineStatus(s string) string {
	switch strings.ToLower(s) {
	case "queued":
		return VideoStatusQueued
	case "running":
		return VideoStatusRunning
	case "succeeded":
		return VideoStatusSucceeded
	case "failed", "cancelled", "canceled", "expired":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// =========================== Zhipu GLM (CogVideoX) ===========================
//
// Submit POST {base}/videos/generations -> {id, task_status}
// Poll   GET  {base}/async-result/{id}   -> {task_status, video_result:[{url}]}
// base_url is typically https://open.bigmodel.cn/api/paas/v4.

type zhipuVideo struct{}

func (zhipuVideo) Format() string { return "zhipu" }

func (zhipuVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	payload := map[string]any{"model": upstreamModel, "prompt": cr.Prompt}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		payload["image_url"] = img
	}
	if cr.Size != "" {
		payload["size"] = cr.Size
	}
	buf, _ := json.Marshal(payload)
	url := trimBase(baseURL) + "/videos/generations"
	status, respBody, err := genJSONPost(ctx, url, apiKey, buf, nil)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("zhipu submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	id := firstNonEmpty(out.ID, out.RequestID)
	if id == "" {
		return VideoSubmitResult{}, fmt.Errorf("zhipu submit: no task id in response")
	}
	return VideoSubmitResult{UpstreamTaskID: id}, nil
}

func (zhipuVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	url := trimBase(baseURL) + "/async-result/" + upstreamTaskID
	status, respBody, err := genJSONGet(ctx, url, apiKey)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("zhipu poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		TaskStatus  string `json:"task_status"`
		VideoResult []struct {
			URL string `json:"url"`
		} `json:"video_result"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	res := VideoPollResult{Status: mapZhipuStatus(out.TaskStatus)}
	if len(out.VideoResult) > 0 {
		res.VideoURL = out.VideoResult[0].URL
	}
	return res, nil
}

func mapZhipuStatus(s string) string {
	switch strings.ToUpper(s) {
	case "PROCESSING":
		return VideoStatusRunning
	case "SUCCESS":
		return VideoStatusSucceeded
	case "FAIL", "FAILED":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}

// =========================== SiliconFlow ===========================
//
// Submit POST {base}/video/submit  -> {requestId}
// Poll   POST {base}/video/status  {requestId} -> {status, results:{videos:[{url}]}}
// base_url is typically https://api.siliconflow.cn/v1.

type siliconflowVideo struct{}

func (siliconflowVideo) Format() string { return "siliconflow" }

func (siliconflowVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	payload := map[string]any{"model": upstreamModel, "prompt": cr.Prompt}
	if cr.Size != "" {
		payload["image_size"] = cr.Size
	}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		payload["image"] = img
	}
	buf, _ := json.Marshal(payload)
	url := trimBase(baseURL) + "/video/submit"
	status, respBody, err := genJSONPost(ctx, url, apiKey, buf, nil)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("siliconflow submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.RequestID == "" {
		return VideoSubmitResult{}, fmt.Errorf("siliconflow submit: no requestId in response")
	}
	return VideoSubmitResult{UpstreamTaskID: out.RequestID}, nil
}

func (siliconflowVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	url := trimBase(baseURL) + "/video/status"
	buf, _ := json.Marshal(map[string]any{"requestId": upstreamTaskID})
	status, respBody, err := genJSONPost(ctx, url, apiKey, buf, nil)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("siliconflow poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Status  string `json:"status"`
		Results struct {
			Videos []struct {
				URL string `json:"url"`
			} `json:"videos"`
		} `json:"results"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	res := VideoPollResult{Status: mapSiliconFlowStatus(out.Status), ErrorMessage: out.Reason}
	if len(out.Results.Videos) > 0 {
		res.VideoURL = out.Results.Videos[0].URL
	}
	return res, nil
}

func mapSiliconFlowStatus(s string) string {
	switch strings.ToLower(s) {
	case "inqueue":
		return VideoStatusQueued
	case "inprogress":
		return VideoStatusRunning
	case "succeed", "succeeded", "success":
		return VideoStatusSucceeded
	case "failed", "fail":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}

// =========================== MiniMax (Hailuo) ===========================
//
// Submit  POST {base}/v1/video_generation        -> {task_id}
// Poll    GET  {base}/v1/query/video_generation?task_id=..  -> {status, file_id}
// Retrieve GET {base}/v1/files/retrieve?file_id=.. -> {file:{download_url}}
// Three-stage: a Success poll then resolves the file to a download url.
// base_url is typically https://api.minimaxi.com.

type minimaxVideo struct{}

func (minimaxVideo) Format() string { return "minimax" }

func (minimaxVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	payload := map[string]any{"model": upstreamModel, "prompt": cr.Prompt}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		payload["first_frame_image"] = img
	}
	buf, _ := json.Marshal(payload)
	url := trimBase(baseURL) + "/v1/video_generation"
	status, respBody, err := genJSONPost(ctx, url, apiKey, buf, nil)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("minimax submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.TaskID == "" {
		return VideoSubmitResult{}, fmt.Errorf("minimax submit: no task_id in response")
	}
	return VideoSubmitResult{UpstreamTaskID: out.TaskID}, nil
}

func (minimaxVideo) Poll(ctx context.Context, baseURL, apiKey, upstreamTaskID string) (VideoPollResult, error) {
	url := trimBase(baseURL) + "/v1/query/video_generation?task_id=" + upstreamTaskID
	status, respBody, err := genJSONGet(ctx, url, apiKey)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("minimax poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Status string `json:"status"`
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	st := mapMiniMaxStatus(out.Status)
	res := VideoPollResult{Status: st}
	if st == VideoStatusSucceeded && out.FileID != "" {
		// Resolve the file id to a download url (stage 3).
		furl := trimBase(baseURL) + "/v1/files/retrieve?file_id=" + out.FileID
		fstatus, fbody, ferr := genJSONGet(ctx, furl, apiKey)
		if ferr == nil && fstatus >= 200 && fstatus < 300 {
			var fout struct {
				File struct {
					DownloadURL string `json:"download_url"`
				} `json:"file"`
			}
			if json.Unmarshal(fbody, &fout) == nil {
				res.VideoURL = fout.File.DownloadURL
			}
		}
	}
	return res, nil
}

func mapMiniMaxStatus(s string) string {
	switch strings.ToLower(s) {
	case "queueing", "preparing", "processing":
		return VideoStatusRunning
	case "success":
		return VideoStatusSucceeded
	case "fail", "failed":
		return VideoStatusFailed
	default:
		return VideoStatusRunning
	}
}

// =========================== Google Gemini (Veo) ===========================
//
// Submit POST {base}/v1beta/models/{model}:predictLongRunning -> {name:"operations/.."}
// Poll   GET  {base}/v1beta/{operationName} -> {done, response:{generateVideoResponse:{generatedSamples:[{video:{uri}}]}}}
// Gemini authenticates with x-goog-api-key (not Bearer), so this adaptor
// builds its own requests instead of the shared Bearer helpers.

type geminiVideo struct{}

func (geminiVideo) Format() string { return "gemini" }

func (geminiVideo) doJSON(ctx context.Context, method, url, apiKey string, body []byte) (int, []byte, error) {
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
	req.Header.Set("x-goog-api-key", apiKey)
	return genDo(req)
}

func (a geminiVideo) Submit(ctx context.Context, baseURL, apiKey, upstreamModel string, body []byte) (VideoSubmitResult, error) {
	cr := parseClientVideo(body)
	model := upstreamModel
	if len(model) > 7 && model[:7] == "models/" {
		model = model[7:]
	}
	instance := map[string]any{"prompt": cr.Prompt}
	if img := firstNonEmpty(cr.ImageURL, cr.Image); img != "" {
		instance["image"] = map[string]any{"imageUri": img}
	}
	payload, _ := json.Marshal(map[string]any{
		"instances":  []map[string]any{instance},
		"parameters": map[string]any{},
	})
	url := trimBase(baseURL) + "/v1beta/models/" + model + ":predictLongRunning"
	status, respBody, err := a.doJSON(ctx, http.MethodPost, url, apiKey, payload)
	if err != nil {
		return VideoSubmitResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoSubmitResult{}, fmt.Errorf("gemini submit %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoSubmitResult{}, err
	}
	if out.Name == "" {
		return VideoSubmitResult{}, fmt.Errorf("gemini submit: no operation name")
	}
	return VideoSubmitResult{UpstreamTaskID: out.Name}, nil
}

func (a geminiVideo) Poll(ctx context.Context, baseURL, apiKey, operationName string) (VideoPollResult, error) {
	url := trimBase(baseURL) + "/v1beta/" + strings.TrimPrefix(operationName, "/")
	status, respBody, err := a.doJSON(ctx, http.MethodGet, url, apiKey, nil)
	if err != nil {
		return VideoPollResult{}, err
	}
	if status < 200 || status >= 300 {
		return VideoPollResult{}, fmt.Errorf("gemini poll %d: %s", status, truncate(string(respBody), 256))
	}
	var out struct {
		Done  bool `json:"done"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Response struct {
			GenerateVideoResponse struct {
				GeneratedSamples []struct {
					Video struct {
						URI string `json:"uri"`
					} `json:"video"`
				} `json:"generatedSamples"`
			} `json:"generateVideoResponse"`
		} `json:"response"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return VideoPollResult{}, err
	}
	if !out.Done {
		return VideoPollResult{Status: VideoStatusRunning}, nil
	}
	if out.Error != nil && out.Error.Message != "" {
		return VideoPollResult{Status: VideoStatusFailed, ErrorMessage: out.Error.Message}, nil
	}
	res := VideoPollResult{Status: VideoStatusSucceeded}
	if s := out.Response.GenerateVideoResponse.GeneratedSamples; len(s) > 0 {
		res.VideoURL = s[0].Video.URI
	}
	return res, nil
}
