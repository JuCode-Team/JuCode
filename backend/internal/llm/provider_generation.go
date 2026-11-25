package llm

// provider_generation.go extends the Provider abstraction (provider.go) with
// an OPTIONAL capability for serving non-chat generation endpoints
// (embeddings / image / audio / video / rerank).
//
// It is deliberately a separate, optional interface rather than new methods
// on Provider: the existing chat path (anthropic / openai_compatible
// Forward) is untouched, and only providers that actually serve these
// endpoints implement GenerationForwarder. A provider that doesn't implement
// it is treated as chat-only.
//
// ForwardGeneration translates the OpenAI-shaped GenerationRequest into the
// upstream dialect: native DashScope image (async submit + internal poll),
// native Gemini embeddings and image, and OpenAI-compatible JSON / multipart
// passthrough for everything else. It extracts a billing signal from the
// response; generation.go settles it through chargeAndLogGeneration.
//
// Still coarse: which endpoints an account is assumed to serve comes from
// DefaultCapabilities(api_format) rather than a per-account declaration —
// see the TODO on that function in endpoints.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"

	"github.com/jucode/jucode/internal/biz"
)

// ErrEndpointNotImplemented means the provider advertises the endpoint but
// this request shape has no forwarding path: an encoding the forwarder does
// not speak, or a multipart endpoint reached without a parsed multipart body.
var ErrEndpointNotImplemented = errors.New("generation endpoint not implemented")

// ErrEndpointUnsupported is returned when a provider is asked to serve a
// generation endpoint it does not advertise in its capability set.
var ErrEndpointUnsupported = errors.New("generation endpoint not supported by provider")

// GenerationRequest is the normalized, provider-agnostic input to a non-chat
// generation endpoint. It carries the already-parsed client request plus the
// routing context the forwarder needs. Fields are a superset across endpoint
// kinds; a given kind uses the subset that applies (see EndpointSpec).
type GenerationRequest struct {
	Kind EndpointKind
	// Model is the upstream model name (after per-provider rewrite).
	Model string
	// JSONBody is the raw client JSON body for EncodingJSON endpoints
	// (embeddings, image_generation, audio_speech, rerank, video). Nil for
	// multipart endpoints.
	JSONBody []byte
	// Multipart carries the parsed multipart form for EncodingMultipart
	// endpoints (image_edit, image_variation, audio_transcription,
	// audio_translation). Nil for JSON endpoints.
	Multipart *MultipartInput
	// ClientHeaders is the inbound request header set, for providers that
	// passthrough auth-adjacent headers.
	ClientHeaders http.Header
}

// MultipartInput is a parsed multipart/form-data upload (audio files for
// transcription, source images for edits) plus its non-file fields. The
// gateway parses the inbound request into this shape once; adaptors re-encode
// it toward the upstream (rewriting the model field).
type MultipartInput struct {
	Fields map[string][]string
	Files  []MultipartFile
}

// MultipartFile is one uploaded file part.
type MultipartFile struct {
	Field       string
	Filename    string
	ContentType string
	Content     []byte
}

// GenerationResponse is the normalized upstream result. The gateway writes
// Body to the client with ContentType. For binary endpoints (TTS) Body is
// the raw audio; for JSON endpoints it's the (possibly translated-back-to-
// OpenAI-shape) JSON.
type GenerationResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
	// Usage is the metering signal (image count, audio seconds, character
	// count, token count) extracted from the upstream response and settled
	// by chargeAndLogGeneration. Nil when the response carried nothing
	// usable, in which case the model's per_request price applies.
	Usage *GenerationUsage
	// Header carries the vendor's quota headers, when the adaptor keeps them,
	// into the account's passive quota snapshot.
	Header http.Header
}

// GenerationUsage is the billing signal extracted from an upstream response.
type GenerationUsage struct {
	Images     int   // images produced (per-image billing)
	AudioSecs  int   // audio/video seconds (per-second billing)
	Characters int   // input characters (TTS per-character billing)
	InputToks  int64 // embeddings input tokens
	OutputToks int64
}

// GenerationForwarder is the optional capability a Provider implements to
// serve generation endpoints. Chat-only providers do not implement it.
type GenerationForwarder interface {
	// SupportsEndpoint reports whether this provider advertises the kind.
	SupportsEndpoint(k EndpointKind) bool
	// ForwardGeneration issues the upstream request and returns a normalized
	// response. Returns ErrEndpointUnsupported if the kind isn't advertised,
	// ErrEndpointNotImplemented if the request shape has no forwarding path.
	ForwardGeneration(ctx context.Context, req *GenerationRequest) (*GenerationResponse, error)
}

// asGenerationForwarder returns the provider's GenerationForwarder view, or
// (nil,false) when the provider is chat-only.
func asGenerationForwarder(p Provider) (GenerationForwarder, bool) {
	gf, ok := p.(GenerationForwarder)
	return gf, ok
}

// --- openai_compatible generation capability ---
//
// The OpenAI-compatible provider is the broadest generation surface (most
// third-party gateways and DashScope compatible-mode expose these). Its
// capability set comes from DefaultCapabilities("openai_compatible") for now.

func (o *openaiProvider) SupportsEndpoint(k EndpointKind) bool {
	return DefaultCapabilities(o.Kind()).Has(k)
}

func (o *openaiProvider) ForwardGeneration(ctx context.Context, req *GenerationRequest) (*GenerationResponse, error) {
	if !o.SupportsEndpoint(req.Kind) {
		return nil, ErrEndpointUnsupported
	}
	spec, ok := SpecFor(req.Kind)
	if !ok {
		return nil, ErrEndpointUnsupported
	}
	// Native (non-OpenAI-shaped) dialects that need request/response
	// translation are dispatched before the passthrough. DashScope image is
	// async-only, so it submits + internally polls to satisfy the sync
	// OpenAI images surface.
	if o.format == "dashscope" && req.Kind == EndpointImageGeneration {
		return o.forwardDashScopeImage(ctx, req)
	}
	if o.format == "gemini" && req.Kind == EndpointEmbeddings {
		return o.forwardGeminiEmbeddings(ctx, req)
	}
	if o.format == "gemini" && req.Kind == EndpointImageGeneration {
		return o.forwardGeminiImage(ctx, req)
	}
	switch spec.Request {
	case EncodingJSON:
		return o.forwardGenerationJSON(ctx, spec, req)
	case EncodingMultipart:
		return o.forwardGenerationMultipart(ctx, spec, req)
	default:
		return nil, ErrEndpointNotImplemented
	}
}

// forwardGenerationMultipart re-encodes the parsed multipart upload toward the
// upstream's OpenAI-compatible path, rewriting the `model` field to the
// per-provider upstream model. Covers image edits/variations and audio
// transcription/translation.
func (o *openaiProvider) forwardGenerationMultipart(ctx context.Context, spec EndpointSpec, req *GenerationRequest) (*GenerationResponse, error) {
	if req.Multipart == nil {
		return nil, ErrEndpointNotImplemented
	}
	buf, formType, err := encodeGenerationMultipart(req.Model, req.Multipart)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+spec.OpenAIPath, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", formType)
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	if o.organization != "" {
		httpReq.Header.Set("OpenAI-Organization", o.organization)
	}
	biz.ApplyHeaderRules(httpReq.Header, o.headerRules, o.apiKey)
	resp, err := o.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	out := &GenerationResponse{StatusCode: resp.StatusCode, ContentType: ct, Body: body}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		out.Usage = extractGenerationUsage(spec.Kind, body)
	}
	return out, nil
}

// encodeGenerationMultipart rebuilds an OpenAI multipart generation body.
// CreateFormFile always labels the part application/octet-stream, which the
// Images API rejects even when the client sent image/png. The part type is
// the sniffed image/audio format when the bytes identify one, otherwise the
// type the client declared, otherwise a filename extension. More than one
// reference picture is sent as image[], because a repeated image parameter
// is rejected.
func encodeGenerationMultipart(model string, mp *MultipartInput) ([]byte, string, error) {
	if mp == nil {
		return nil, "", ErrEndpointNotImplemented
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	wroteModel := false
	for name, vals := range mp.Fields {
		for _, v := range vals {
			if name == "model" {
				v = model
				wroteModel = true
			}
			if err := mw.WriteField(name, v); err != nil {
				return nil, "", err
			}
		}
	}
	if !wroteModel && model != "" {
		if err := mw.WriteField("model", model); err != nil {
			return nil, "", err
		}
	}
	imageCount := countImageFiles(mp.Files)
	for _, f := range mp.Files {
		header := make(textproto.MIMEHeader)
		field := imageMultipartField(f.Field, imageCount)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeMultipartQuotes(field), escapeMultipartQuotes(f.Filename)))
		header.Set("Content-Type", generationFileContentType(f.ContentType, f.Filename, f.Content))
		part, err := mw.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.Content); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

func escapeMultipartQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// countImageFiles counts reference pictures. The Images API allows one part
// named image; a list has to be sent as image[].
func countImageFiles(files []MultipartFile) int {
	n := 0
	for _, f := range files {
		if f.Field == "image" || f.Field == "image[]" {
			n++
		}
	}
	return n
}

func imageMultipartField(field string, imageCount int) string {
	if imageCount > 1 && (field == "image" || field == "image[]") {
		return "image[]"
	}
	return field
}

func generationFileContentType(declared, filename string, content []byte) string {
	declaredType := canonicalGenerationMIME(declared)
	sample := content
	if len(sample) > 512 {
		sample = sample[:512]
	}
	detected := canonicalGenerationMIME(http.DetectContentType(sample))
	if strings.HasPrefix(detected, "image/") || strings.HasPrefix(detected, "audio/") || strings.HasPrefix(detected, "video/") {
		return detected
	}
	if declaredType != "" && !isGenericBinaryMIME(declaredType) {
		return declaredType
	}
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(filename))) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".webm":
		return "audio/webm"
	case ".mp4":
		return "video/mp4"
	}
	if declaredType != "" {
		return declaredType
	}
	return "application/octet-stream"
}

func isGenericBinaryMIME(mediaType string) bool {
	switch mediaType {
	case "application/octet-stream", "application/binary", "binary/octet-stream":
		return true
	default:
		return false
	}
}

func canonicalGenerationMIME(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	}
	return strings.ToLower(strings.TrimSpace(mediaType))
}

// forwardGenerationJSON passes a JSON generation request straight through to
// baseURL + spec.OpenAIPath. This is the OpenAI-compatible relay path that
// covers every upstream speaking the standard surface (DashScope
// compatible-mode, Volcengine Ark, SiliconFlow, Zhipu OpenAI-mode, …). The
// body is forwarded verbatim (the client already sent the OpenAI shape, and
// per-provider model rewrite was applied upstream in the gateway), so no
// dialect translation happens here.
func (o *openaiProvider) forwardGenerationJSON(ctx context.Context, spec EndpointSpec, req *GenerationRequest) (*GenerationResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+spec.OpenAIPath, bytes.NewReader(req.JSONBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	if o.organization != "" {
		httpReq.Header.Set("OpenAI-Organization", o.organization)
	}
	// audio_speech returns raw audio bytes; everything else returns JSON.
	if spec.Response == RespBinary {
		httpReq.Header.Set("Accept", "*/*")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	biz.ApplyHeaderRules(httpReq.Header, o.headerRules, o.apiKey)
	resp, err := o.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		if spec.Response == RespBinary {
			ct = "application/octet-stream"
		} else {
			ct = "application/json"
		}
	}
	out := &GenerationResponse{
		StatusCode:  resp.StatusCode,
		ContentType: ct,
		Body:        body,
	}
	// Extract a billing signal on success. Best-effort: absence just means
	// the endpoint's fallback (per_request) billing applies.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		out.Usage = extractGenerationUsage(spec.Kind, body)
	}
	return out, nil
}

// extractGenerationUsage pulls the metering signal from an upstream JSON
// response. It tolerates both OpenAI-compatible (`prompt_tokens`) and native
// DashScope compatible-mode field names, and counts images for image
// endpoints. Returns nil when nothing usable is present.
func extractGenerationUsage(kind EndpointKind, body []byte) *GenerationUsage {
	var parsed struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			InputTokens      int64 `json:"input_tokens"`  // native DashScope
			OutputTokens     int64 `json:"output_tokens"` // native DashScope
		} `json:"usage"`
		Data []json.RawMessage `json:"data"` // images / embeddings array
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	u := &GenerationUsage{
		InputToks:  firstNonZero64(parsed.Usage.PromptTokens, parsed.Usage.InputTokens),
		OutputToks: firstNonZero64(parsed.Usage.CompletionTokens, parsed.Usage.OutputTokens),
	}
	switch kind {
	case EndpointImageGeneration, EndpointImageEdit, EndpointImageVariation:
		u.Images = len(parsed.Data)
	}
	if u.InputToks == 0 && u.OutputToks == 0 && u.Images == 0 {
		return nil
	}
	return u
}

func firstNonZero64(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// Compile-time assertion that openaiProvider satisfies GenerationForwarder.
var _ GenerationForwarder = (*openaiProvider)(nil)
