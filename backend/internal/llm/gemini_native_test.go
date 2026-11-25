package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestGeminiEmbeddingsSingle verifies the OpenAI->Gemini embeddings translation
// for a single string input: :embedContent path, x-goog-api-key auth, and an
// OpenAI-list response.
func TestGeminiEmbeddingsSingle(t *testing.T) {
	var gotPath, gotKeyHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKeyHdr = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2,0.3]}}`))
	}))
	defer srv.Close()

	p := &openaiProvider{id: uuid.New(), name: "g", baseURL: srv.URL, apiKey: "gkey", format: "gemini", hc: srv.Client()}
	resp, err := p.ForwardGeneration(context.Background(), &GenerationRequest{
		Kind:     EndpointEmbeddings,
		Model:    "text-embedding-004",
		JSONBody: []byte(`{"model":"text-embedding-004","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("ForwardGeneration: %v", err)
	}
	if gotPath != "/v1beta/models/text-embedding-004:embedContent" {
		t.Fatalf("path: %q", gotPath)
	}
	if gotKeyHdr != "gkey" {
		t.Fatalf("api key header: %q", gotKeyHdr)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("resp not json: %v (%s)", err, resp.Body)
	}
	if out.Object != "list" || len(out.Data) != 1 || len(out.Data[0].Embedding) != 3 {
		t.Fatalf("bad translation: %s", resp.Body)
	}
}

// TestGeminiEmbeddingsBatch verifies the array-input path uses
// batchEmbedContents and returns one data entry per input.
func TestGeminiEmbeddingsBatch(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[1,2]},{"values":[3,4]}]}`))
	}))
	defer srv.Close()

	p := &openaiProvider{id: uuid.New(), name: "g", baseURL: srv.URL, apiKey: "k", format: "gemini", hc: srv.Client()}
	resp, err := p.ForwardGeneration(context.Background(), &GenerationRequest{
		Kind:     EndpointEmbeddings,
		Model:    "text-embedding-004",
		JSONBody: []byte(`{"model":"text-embedding-004","input":["a","b"]}`),
	})
	if err != nil {
		t.Fatalf("ForwardGeneration: %v", err)
	}
	if !strings.HasSuffix(gotPath, ":batchEmbedContents") {
		t.Fatalf("path: %q", gotPath)
	}
	if !strings.Contains(gotBody, `"requests"`) {
		t.Fatalf("body missing requests: %s", gotBody)
	}
	var out struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(resp.Body, &out)
	if len(out.Data) != 2 {
		t.Fatalf("want 2 embeddings, got %d: %s", len(out.Data), resp.Body)
	}
}

// TestGeminiImagePredict verifies the OpenAI images -> Imagen :predict
// translation returns b64_json data.
func TestGeminiImagePredict(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"predictions":[{"bytesBase64Encoded":"QUJD","mimeType":"image/png"}]}`))
	}))
	defer srv.Close()

	p := &openaiProvider{id: uuid.New(), name: "g", baseURL: srv.URL, apiKey: "k", format: "gemini", hc: srv.Client()}
	resp, err := p.ForwardGeneration(context.Background(), &GenerationRequest{
		Kind:     EndpointImageGeneration,
		Model:    "imagen-3.0-generate-002",
		JSONBody: []byte(`{"model":"imagen-3.0-generate-002","prompt":"a fox","n":1,"size":"1024x1024"}`),
	})
	if err != nil {
		t.Fatalf("ForwardGeneration: %v", err)
	}
	if !strings.HasSuffix(gotPath, ":predict") {
		t.Fatalf("path: %q", gotPath)
	}
	if !strings.Contains(gotBody, `"instances"`) || !strings.Contains(gotBody, `"aspectRatio":"1:1"`) {
		t.Fatalf("body: %s", gotBody)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].B64 != "QUJD" {
		t.Fatalf("data: %+v", out.Data)
	}
	if resp.Usage == nil || resp.Usage.Images != 1 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}
