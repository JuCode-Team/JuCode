package llm

// image_native.go handles image generation for upstreams whose image API is
// NOT OpenAI-compatible. Today: DashScope (Alibaba Wanx) text-to-image, which
// is async-only (no compatible-mode /images path). Because the OpenAI images
// surface is synchronous, the gateway submits the Wanx task and internally
// polls to completion (bounded), then returns the OpenAI-shaped result. Image
// jobs finish in seconds, so a bounded blocking poll is appropriate here —
// unlike video, which is minutes-long and modeled as an async job.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Poll cadence for the internal blocking wait. Vars (not consts) so tests can
// shrink them; production values give a Wanx task room to finish.
var (
	dashscopeImagePollInterval = 2 * time.Second
	dashscopeImageMaxWait      = 120 * time.Second
)

// forwardDashScopeImage submits a Wanx text-to-image task and blocks (bounded)
// until it terminates, returning an OpenAI images-shaped response.
func (o *openaiProvider) forwardDashScopeImage(ctx context.Context, req *GenerationRequest) (*GenerationResponse, error) {
	var cr struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		NegativePrompt string `json:"negative_prompt"`
		Size           string `json:"size"`
		N              int    `json:"n"`
	}
	_ = json.Unmarshal(req.JSONBody, &cr)

	input := map[string]any{"prompt": cr.Prompt}
	if cr.NegativePrompt != "" {
		input["negative_prompt"] = cr.NegativePrompt
	}
	params := map[string]any{}
	if cr.Size != "" {
		// OpenAI uses "1024x1024"; Wanx uses "1024*1024".
		params["size"] = normalizeWanxSize(cr.Size)
	}
	if cr.N > 0 {
		params["n"] = cr.N
	}
	submit, _ := json.Marshal(map[string]any{
		"model":      req.Model,
		"input":      input,
		"parameters": params,
	})
	submitURL := trimBase(o.baseURL) + "/api/v1/services/aigc/text2image/image-synthesis"
	status, body, err := genJSONPost(ctx, submitURL, o.apiKey, submit, map[string]string{"X-DashScope-Async": "enable"})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		// Surface the upstream error to the client verbatim.
		return &GenerationResponse{StatusCode: status, ContentType: "application/json", Body: body}, nil
	}
	var sub struct {
		Output struct {
			TaskID string `json:"task_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &sub); err != nil || sub.Output.TaskID == "" {
		return nil, fmt.Errorf("dashscope image submit: no task_id")
	}

	// Bounded internal poll.
	deadline := time.Now().Add(dashscopeImageMaxWait)
	taskURL := trimBase(o.baseURL) + "/api/v1/tasks/" + sub.Output.TaskID
	for {
		pstatus, pbody, err := genJSONGet(ctx, taskURL, o.apiKey)
		if err != nil {
			return nil, err
		}
		if pstatus < 200 || pstatus >= 300 {
			return &GenerationResponse{StatusCode: pstatus, ContentType: "application/json", Body: pbody}, nil
		}
		var pr struct {
			Output struct {
				TaskStatus string `json:"task_status"`
				Message    string `json:"message"`
				Results    []struct {
					URL string `json:"url"`
				} `json:"results"`
			} `json:"output"`
			Usage struct {
				ImageCount int `json:"image_count"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(pbody, &pr); err != nil {
			return nil, err
		}
		switch mapDashScopeStatus(pr.Output.TaskStatus) {
		case VideoStatusSucceeded:
			data := make([]map[string]any, 0, len(pr.Output.Results))
			for _, r := range pr.Output.Results {
				if r.URL != "" {
					data = append(data, map[string]any{"url": r.URL})
				}
			}
			n := pr.Usage.ImageCount
			if n == 0 {
				n = len(data)
			}
			out, _ := json.Marshal(map[string]any{
				"created": time.Now().Unix(),
				"data":    data,
				"usage":   map[string]any{"image_count": n},
			})
			return &GenerationResponse{
				StatusCode: 200, ContentType: "application/json", Body: out,
				Usage: &GenerationUsage{Images: n},
			}, nil
		case VideoStatusFailed:
			msg := pr.Output.Message
			if msg == "" {
				msg = "image generation failed"
			}
			errBody, _ := json.Marshal(map[string]any{
				"error": map[string]any{"type": "upstream_error", "message": msg},
			})
			return &GenerationResponse{StatusCode: 502, ContentType: "application/json", Body: errBody}, nil
		}
		// still running
		if time.Now().After(deadline) {
			errBody, _ := json.Marshal(map[string]any{
				"error": map[string]any{"type": "upstream_timeout", "message": "image generation did not complete in time"},
			})
			return &GenerationResponse{StatusCode: 504, ContentType: "application/json", Body: errBody}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(dashscopeImagePollInterval):
		}
	}
}

// normalizeWanxSize converts an OpenAI "WxH" size to Wanx's "W*H".
func normalizeWanxSize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 'x' || s[i] == 'X' {
			out = append(out, '*')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
