package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

// withUser sets an authenticated user on the gin context so the skeleton
// handlers pass the auth assertion.
func withUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(middleware.CtxUser, &ent.User{})
		c.Next()
	}
}

// TestVideoRoute501WithoutTaskStore confirms the video endpoint returns a
// stable 501 when no task store is wired (Gateway.tasks == nil). This 501s
// before touching any other service, so a zero-value Gateway suffices. The
// JSON-sync and multipart endpoints run the full routing/billing pipeline and
// are exercised by integration tests.
func TestVideoRoute501WithoutTaskStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	g := &Gateway{} // tasks == nil

	r := gin.New()
	grp := r.Group("/v1")
	grp.Use(withUser())
	grp.POST("/videos", g.HandleVideoGenerations)

	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("got status %d, want 501; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Error.Code != "async_not_implemented" {
		t.Fatalf("got code %q, want async_not_implemented", resp.Error.Code)
	}
}

// TestExtractGenerationUsage checks the billing-signal extractor against the
// OpenAI-compatible, native-DashScope, and image response shapes.
func TestExtractGenerationUsage(t *testing.T) {
	cases := []struct {
		name    string
		kind    EndpointKind
		body    string
		wantIn  int64
		wantOut int64
		wantImg int
		wantNil bool
	}{
		{"openai embeddings", EndpointEmbeddings, `{"usage":{"prompt_tokens":42,"total_tokens":42}}`, 42, 0, 0, false},
		{"dashscope native", EndpointEmbeddings, `{"usage":{"input_tokens":17}}`, 17, 0, 0, false},
		{"image count", EndpointImageGeneration, `{"data":[{"url":"a"},{"url":"b"}]}`, 0, 0, 2, false},
		{"image2 tokens", EndpointImageGeneration, `{"data":[{"b64_json":"abc"}],"usage":{"input_tokens":39,"output_tokens":229,"total_tokens":268}}`, 39, 229, 1, false},
		{"no usage", EndpointRerank, `{"results":[]}`, 0, 0, 0, true},
		{"invalid json", EndpointEmbeddings, `not json`, 0, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := extractGenerationUsage(tc.kind, []byte(tc.body))
			if tc.wantNil {
				if u != nil {
					t.Fatalf("want nil usage, got %+v", u)
				}
				return
			}
			if u == nil {
				t.Fatalf("want usage, got nil")
			}
			if u.InputToks != tc.wantIn {
				t.Fatalf("InputToks: got %d want %d", u.InputToks, tc.wantIn)
			}
			if u.OutputToks != tc.wantOut {
				t.Fatalf("OutputToks: got %d want %d", u.OutputToks, tc.wantOut)
			}
			if u.Images != tc.wantImg {
				t.Fatalf("Images: got %d want %d", u.Images, tc.wantImg)
			}
		})
	}
}

// TestOpenAIForwardGenerationJSON exercises the real OpenAI-compatible
// passthrough against a mock upstream: it must POST to baseURL + the
// endpoint's canonical path with Bearer auth, return the upstream body +
// content-type verbatim, and extract usage on success.
func TestOpenAIForwardGenerationJSON(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":7}}`))
	}))
	defer srv.Close()

	p := &openaiProvider{
		id:      uuid.New(),
		name:    "mock",
		baseURL: srv.URL,
		apiKey:  "sk-test",
		hc:      srv.Client(),
	}
	resp, err := p.ForwardGeneration(context.Background(), &GenerationRequest{
		Kind:     EndpointEmbeddings,
		Model:    "text-embedding-v3",
		JSONBody: []byte(`{"model":"text-embedding-v3","input":"hi"}`),
	})
	if err != nil {
		t.Fatalf("ForwardGeneration: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("upstream path: got %q want /v1/embeddings", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header: got %q", gotAuth)
	}
	if !strings.Contains(gotBody, "text-embedding-v3") {
		t.Fatalf("upstream body not forwarded: %q", gotBody)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if resp.Usage == nil || resp.Usage.InputToks != 7 {
		t.Fatalf("usage: got %+v want InputToks=7", resp.Usage)
	}
}

// TestOpenAIForwardGenerationMultipart verifies the multipart re-encoder:
// the upstream must receive multipart/form-data at the endpoint path, with the
// model field rewritten to the upstream model and the uploaded file preserved.
func TestOpenAIForwardGenerationMultipart(t *testing.T) {
	var gotCT, gotModel, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		if r.MultipartForm != nil {
			if fs := r.MultipartForm.File["file"]; len(fs) > 0 {
				f, _ := fs[0].Open()
				b, _ := io.ReadAll(f)
				_ = f.Close()
				gotFile = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}))
	defer srv.Close()

	p := &openaiProvider{id: uuid.New(), name: "mock", baseURL: srv.URL, apiKey: "k", hc: srv.Client()}
	resp, err := p.ForwardGeneration(context.Background(), &GenerationRequest{
		Kind:  EndpointAudioTranscription,
		Model: "whisper-upstream",
		Multipart: &MultipartInput{
			Fields: map[string][]string{"model": {"whisper-logical"}, "language": {"en"}},
			Files:  []MultipartFile{{Field: "file", Filename: "a.mp3", Content: []byte("AUDIOBYTES")}},
		},
	})
	if err != nil {
		t.Fatalf("ForwardGeneration multipart: %v", err)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Fatalf("upstream content-type: %q", gotCT)
	}
	if gotModel != "whisper-upstream" {
		t.Fatalf("model not rewritten: got %q", gotModel)
	}
	if gotFile != "AUDIOBYTES" {
		t.Fatalf("file not forwarded: got %q", gotFile)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

// TestOpenAIForwardGenerationUnsupported confirms a capability gap surfaces as
// ErrEndpointUnsupported (video is off by default for openai_compatible).
func TestOpenAIForwardGenerationUnsupported(t *testing.T) {
	p := &openaiProvider{id: uuid.New(), name: "mock", baseURL: "http://x", apiKey: "k"}
	_, err := p.ForwardGeneration(context.Background(), &GenerationRequest{Kind: EndpointVideoGeneration})
	if err != ErrEndpointUnsupported {
		t.Fatalf("got %v, want ErrEndpointUnsupported", err)
	}
}
