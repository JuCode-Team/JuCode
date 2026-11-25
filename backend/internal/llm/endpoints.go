package llm

// endpoints.go declares the gateway's internal, provider-agnostic taxonomy
// of NON-chat generation endpoints: embeddings, image, audio, video, rerank.
//
// This file defines the endpoint types, their canonical client-facing
// (OpenAI-compatible) surface, their sync/async nature and billing
// dimension, plus a per-provider-kind capability map. Upstream forwarding
// lives in provider_generation.go and billing settle in generation.go
// (chargeAndLogGeneration); both are wired.
//
// The capability map is the part that is still coarse — see the TODO on
// DefaultCapabilities below.
//
// Chat Completions / Responses / Anthropic Messages are handled by the
// existing streaming path in gateway.go and are deliberately NOT modeled
// here; this taxonomy is only for the request/response shapes that path
// cannot serve (per-image billing, binary TTS output, multipart STT input,
// async video task poll, etc.).

// EndpointKind enumerates the internal unified generation endpoint types.
type EndpointKind string

const (
	// EndpointEmbeddings: text (and, per-provider, multimodal) vectorization.
	// Synchronous JSON in / JSON out. Billed per input token (or per request).
	EndpointEmbeddings EndpointKind = "embeddings"

	// EndpointImageGeneration: text-to-image. Synchronous for OpenAI
	// (gpt-image-1 / dall-e-3); ASYNC task-based for some providers
	// (Alibaba Wanx). The gateway normalizes both behind the sync OpenAI
	// surface — an async upstream is polled internally before responding.
	EndpointImageGeneration EndpointKind = "image_generation"

	// EndpointImageEdit: image-to-image / multimodal image editing
	// (inpaint, outpaint, reference-guided edit). Client sends a source
	// image (multipart file OR image URL/b64 in JSON, provider-dependent).
	EndpointImageEdit EndpointKind = "image_edit"

	// EndpointAudioSpeech: text-to-speech (TTS). JSON in, BINARY audio out
	// (mp3/opus/aac/flac/wav/pcm). Billed per input character (or request).
	EndpointAudioSpeech EndpointKind = "audio_speech"

	// EndpointAudioTranscription: speech-to-text (STT/ASR). MULTIPART file
	// in, JSON/text out. Some providers (Alibaba Paraformer async) require
	// a task poll. Billed per audio second (or request).
	EndpointAudioTranscription EndpointKind = "audio_transcription"

	// EndpointVideoGeneration: text/image-to-video. ASYNC everywhere
	// (OpenAI Sora jobs, Alibaba Wanx tasks, Google Veo operations).
	// Requires the task submit -> poll -> fetch pattern and a persistent
	// task store — built in a later pass, not the skeleton.
	EndpointVideoGeneration EndpointKind = "video_generation"

	// EndpointImageVariation: produce variations of a source image
	// (OpenAI /v1/images/variations). Multipart source image in, JSON out.
	EndpointImageVariation EndpointKind = "image_variation"

	// EndpointAudioTranslation: speech-to-English-text (OpenAI
	// /v1/audio/translations). Multipart file in, JSON/text out. Sibling of
	// transcription but always translates to English.
	EndpointAudioTranslation EndpointKind = "audio_translation"

	// EndpointRerank: query/document relevance reranking (Cohere/Jina/
	// Voyage/DashScope gte-rerank). Synchronous JSON in / JSON out.
	EndpointRerank EndpointKind = "rerank"
)

// RequestEncoding describes how the client sends the request body.
type RequestEncoding string

const (
	EncodingJSON      RequestEncoding = "json"      // application/json
	EncodingMultipart RequestEncoding = "multipart" // multipart/form-data (file upload)
)

// ResponseEncoding describes what the gateway returns to the client.
type ResponseEncoding string

const (
	RespJSON   ResponseEncoding = "json"   // application/json
	RespBinary ResponseEncoding = "binary" // raw bytes (audio/*, image/* passthrough)
)

// BillingDimension is the unit the endpoint is metered in. The existing
// per_request / per_token model billing (billing.go) covers "request" and
// "token"; the others (image/second/character) are new dimensions the
// billing pass must add to the Model schema + CalcCost.
type BillingDimension string

const (
	BillImage     BillingDimension = "image"     // per generated image (n)
	BillSecond    BillingDimension = "second"    // per second of audio/video
	BillCharacter BillingDimension = "character" // per input character (TTS)
	BillToken     BillingDimension = "token"     // per token (embeddings)
	BillRequest   BillingDimension = "request"   // flat per call
)

// EndpointSpec is the static contract for one endpoint kind.
type EndpointSpec struct {
	Kind EndpointKind
	// OpenAIPath is the canonical client-facing path under /v1 that the
	// gateway exposes for this endpoint (the de-facto lingua franca that
	// new-api and most SDKs speak).
	OpenAIPath string
	// Async is true when at least some upstreams serve this endpoint via a
	// submit/poll task flow. Even a "sync" client surface may poll an async
	// upstream internally; Async here means the gateway MAY need the task
	// store for this kind.
	Async bool
	// AlwaysAsync is true when the CLIENT surface itself is a job pattern
	// (create job -> poll -> download), i.e. video. These need the task
	// store unconditionally and are out of scope for the skeleton.
	AlwaysAsync bool
	Request     RequestEncoding
	Response    ResponseEncoding
	Billing     BillingDimension
}

// endpointSpecs is the registry of all non-chat generation endpoints.
var endpointSpecs = map[EndpointKind]EndpointSpec{
	EndpointEmbeddings: {
		Kind: EndpointEmbeddings, OpenAIPath: "/v1/embeddings",
		Async: false, Request: EncodingJSON, Response: RespJSON, Billing: BillToken,
	},
	EndpointImageGeneration: {
		Kind: EndpointImageGeneration, OpenAIPath: "/v1/images/generations",
		Async: true, Request: EncodingJSON, Response: RespJSON, Billing: BillImage,
	},
	EndpointImageEdit: {
		Kind: EndpointImageEdit, OpenAIPath: "/v1/images/edits",
		Async: true, Request: EncodingMultipart, Response: RespJSON, Billing: BillImage,
	},
	EndpointAudioSpeech: {
		Kind: EndpointAudioSpeech, OpenAIPath: "/v1/audio/speech",
		Async: false, Request: EncodingJSON, Response: RespBinary, Billing: BillCharacter,
	},
	EndpointAudioTranscription: {
		Kind: EndpointAudioTranscription, OpenAIPath: "/v1/audio/transcriptions",
		Async: true, Request: EncodingMultipart, Response: RespJSON, Billing: BillSecond,
	},
	EndpointVideoGeneration: {
		Kind: EndpointVideoGeneration, OpenAIPath: "/v1/videos",
		Async: true, AlwaysAsync: true, Request: EncodingJSON, Response: RespJSON, Billing: BillSecond,
	},
	EndpointImageVariation: {
		Kind: EndpointImageVariation, OpenAIPath: "/v1/images/variations",
		Async: true, Request: EncodingMultipart, Response: RespJSON, Billing: BillImage,
	},
	EndpointAudioTranslation: {
		Kind: EndpointAudioTranslation, OpenAIPath: "/v1/audio/translations",
		Async: false, Request: EncodingMultipart, Response: RespJSON, Billing: BillSecond,
	},
	EndpointRerank: {
		Kind: EndpointRerank, OpenAIPath: "/v1/rerank",
		Async: false, Request: EncodingJSON, Response: RespJSON, Billing: BillRequest,
	},
}

// SpecFor returns the static contract for an endpoint kind (ok=false if
// unknown).
func SpecFor(k EndpointKind) (EndpointSpec, bool) {
	s, ok := endpointSpecs[k]
	return s, ok
}

// EndpointSet is a capability set: which generation endpoints a provider
// (kind) can serve.
type EndpointSet map[EndpointKind]bool

func (s EndpointSet) Has(k EndpointKind) bool { return s[k] }

// DefaultCapabilities returns the generation endpoints a provider of the
// given kind is ASSUMED to serve, before any per-provider override.
//
// This is a coarse default keyed only on the account's api_format. The
// billing/compat pass should replace it with a real per-account capability
// declaration (e.g. a JSON column on ent.UpstreamAccount, or an adaptor
// registry keyed by the upstream vendor) so that, say, an OpenAI-compatible
// account pointed at a DashScope base_url can advertise wanx video while one
// pointed at a bare embeddings server advertises only embeddings.
//
// TODO(billing-compat-pass): make this per-provider, not per-kind.
func DefaultCapabilities(kind string) EndpointSet {
	switch kind {
	case "openai_compatible":
		// The OpenAI-compatible surface is the broadest: most third-party
		// gateways (new-api, DashScope compatible-mode, etc.) expose these.
		return EndpointSet{
			EndpointEmbeddings:         true,
			EndpointImageGeneration:    true,
			EndpointImageEdit:          true,
			EndpointImageVariation:     true,
			EndpointAudioSpeech:        true,
			EndpointAudioTranscription: true,
			EndpointAudioTranslation:   true,
			EndpointRerank:             true,
			// Video stays off by default until the async task store exists.
			EndpointVideoGeneration: false,
		}
	case "anthropic":
		// Anthropic Messages API is chat-only; no first-party embeddings,
		// image, audio or video generation endpoints.
		return EndpointSet{}
	default:
		return EndpointSet{}
	}
}
