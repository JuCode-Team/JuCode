package llm

// gemini_native.go handles Google Gemini's native (non-OpenAI) generation
// dialects. Gemini uses `models/{model}:{action}` paths, an `x-goog-api-key`
// header (not Bearer), and its own request/response shapes, so it needs a
// translating adaptor rather than the passthrough. Today: text embeddings
// (embedContent / batchEmbedContents). Image (Imagen :predict) and video
// (Veo :predictLongRunning) are follow-ups.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// forwardGeminiEmbeddings translates an OpenAI embeddings request to Gemini's
// embedContent/batchEmbedContents API and normalizes the response back to the
// OpenAI list shape.
func (o *openaiProvider) forwardGeminiEmbeddings(ctx context.Context, req *GenerationRequest) (*GenerationResponse, error) {
	inputs, err := parseEmbeddingInputs(req.JSONBody)
	if err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("gemini embeddings: empty input")
	}
	model := req.Model
	// Gemini wants the bare model id; tolerate a "models/" prefix either way.
	geminiModel := model
	if len(geminiModel) > 7 && geminiModel[:7] == "models/" {
		geminiModel = geminiModel[7:]
	}

	var url string
	var payload []byte
	if len(inputs) == 1 {
		url = trimBase(o.baseURL) + "/v1beta/models/" + geminiModel + ":embedContent"
		payload, _ = json.Marshal(map[string]any{
			"model":   "models/" + geminiModel,
			"content": geminiContent(inputs[0]),
		})
	} else {
		url = trimBase(o.baseURL) + "/v1beta/models/" + geminiModel + ":batchEmbedContents"
		reqs := make([]map[string]any, len(inputs))
		for i, in := range inputs {
			reqs[i] = map[string]any{"model": "models/" + geminiModel, "content": geminiContent(in)}
		}
		payload, _ = json.Marshal(map[string]any{"requests": reqs})
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", o.apiKey)
	resp, err := o.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &GenerationResponse{StatusCode: resp.StatusCode, ContentType: "application/json", Body: body}, nil
	}

	// Collect embedding vectors from either response shape.
	var vectors [][]float64
	if len(inputs) == 1 {
		var single struct {
			Embedding struct {
				Values []float64 `json:"values"`
			} `json:"embedding"`
		}
		if err := json.Unmarshal(body, &single); err != nil {
			return nil, err
		}
		vectors = append(vectors, single.Embedding.Values)
	} else {
		var batch struct {
			Embeddings []struct {
				Values []float64 `json:"values"`
			} `json:"embeddings"`
		}
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		for _, e := range batch.Embeddings {
			vectors = append(vectors, e.Values)
		}
	}

	data := make([]map[string]any, len(vectors))
	for i, v := range vectors {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
	}
	out, _ := json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		// Gemini doesn't report embedding token usage; leave zeros so
		// per_request billing applies.
		"usage": map[string]any{"prompt_tokens": 0, "total_tokens": 0},
	})
	return &GenerationResponse{StatusCode: 200, ContentType: "application/json", Body: out}, nil
}

func geminiContent(text string) map[string]any {
	return map[string]any{"parts": []map[string]any{{"text": text}}}
}

// forwardGeminiImage translates an OpenAI images request to Imagen's :predict
// API and back. Imagen returns base64 image bytes, so the OpenAI response uses
// b64_json.
func (o *openaiProvider) forwardGeminiImage(ctx context.Context, req *GenerationRequest) (*GenerationResponse, error) {
	var cr struct {
		Prompt string `json:"prompt"`
		N      int    `json:"n"`
		Size   string `json:"size"`
	}
	_ = json.Unmarshal(req.JSONBody, &cr)

	geminiModel := req.Model
	if len(geminiModel) > 7 && geminiModel[:7] == "models/" {
		geminiModel = geminiModel[7:]
	}
	params := map[string]any{}
	if cr.N > 0 {
		params["sampleCount"] = cr.N
	}
	if ar := openAISizeToAspectRatio(cr.Size); ar != "" {
		params["aspectRatio"] = ar
	}
	payload, _ := json.Marshal(map[string]any{
		"instances":  []map[string]any{{"prompt": cr.Prompt}},
		"parameters": params,
	})
	url := trimBase(o.baseURL) + "/v1beta/models/" + geminiModel + ":predict"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", o.apiKey)
	resp, err := o.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &GenerationResponse{StatusCode: resp.StatusCode, ContentType: "application/json", Body: body}, nil
	}
	var pred struct {
		Predictions []struct {
			BytesBase64Encoded string `json:"bytesBase64Encoded"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(body, &pred); err != nil {
		return nil, err
	}
	data := make([]map[string]any, 0, len(pred.Predictions))
	for _, p := range pred.Predictions {
		if p.BytesBase64Encoded != "" {
			data = append(data, map[string]any{"b64_json": p.BytesBase64Encoded})
		}
	}
	out, _ := json.Marshal(map[string]any{"created": 0, "data": data})
	return &GenerationResponse{
		StatusCode: 200, ContentType: "application/json", Body: out,
		Usage: &GenerationUsage{Images: len(data)},
	}, nil
}

// openAISizeToAspectRatio maps a "WxH" size to Imagen's aspectRatio enum,
// falling back to "" (Imagen default) for unrecognized sizes.
func openAISizeToAspectRatio(size string) string {
	switch size {
	case "1024x1024", "512x512", "256x256":
		return "1:1"
	case "1792x1024", "1536x1024":
		return "16:9"
	case "1024x1792", "1024x1536":
		return "9:16"
	default:
		return ""
	}
}

// parseEmbeddingInputs extracts the input(s) from an OpenAI embeddings body,
// accepting both a single string and an array of strings.
func parseEmbeddingInputs(body []byte) ([]string, error) {
	var probe struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if len(probe.Input) == 0 {
		return nil, nil
	}
	// Try string first, then []string.
	var s string
	if err := json.Unmarshal(probe.Input, &s); err == nil {
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(probe.Input, &arr); err == nil {
		return arr, nil
	}
	return nil, fmt.Errorf("embeddings input must be a string or array of strings")
}
