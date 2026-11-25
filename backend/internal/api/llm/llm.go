// Package llm exposes the gateway HTTP handlers and route registration.
package llm

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	llmpkg "github.com/jucode/jucode/internal/llm"
	"github.com/jucode/jucode/internal/middleware"
)

type Handler struct {
	gw     *llmpkg.Gateway
	models *biz.ModelSvc
}

func New(gw *llmpkg.Gateway, models *biz.ModelSvc) *Handler {
	return &Handler{gw: gw, models: models}
}

// MountGeneration registers OpenAI-compatible generation endpoints.
// The group must already have the apikey middleware applied.
func (h *Handler) MountGeneration(rg *gin.RouterGroup) {
	rg.POST("/chat/completions", h.gw.HandleChatCompletions)
	rg.POST("/completions", h.gw.HandleCompletions)
	rg.POST("/responses", h.gw.HandleResponses)
	rg.POST("/responses/compact", h.gw.HandleResponsesCompact)
	rg.POST("/alpha/search", h.gw.HandleAlphaSearch)
	// Authenticated WebSocket upgrades (OpenAI Realtime + Responses WS).
	rg.GET("/realtime", h.gw.HandleRealtime)
	rg.GET("/responses", h.gw.HandleResponsesWS)
	// Grok Build Anthropic surface. Claude Code still uses
	// /anthropic/v1/messages; this is the /v1 alias that client hardcodes
	// (api_backend=messages → {base_url}/messages).
	rg.POST("/messages", h.gw.HandleMessages)
	rg.POST("/messages/count_tokens", h.gw.HandleMessagesCountTokens)
}

// MountMediaGeneration registers the non-chat generation endpoints
// (embeddings / image / audio / rerank / video). The group must already
// have the apikey middleware applied.
//
// See internal/llm/generation.go for the forwarding and billing pipeline.
func (h *Handler) MountMediaGeneration(rg *gin.RouterGroup) {
	// Embeddings.
	rg.POST("/embeddings", h.gw.HandleEmbeddings)
	// Images.
	rg.POST("/images/generations", h.gw.HandleImageGenerations)
	rg.POST("/images/edits", h.gw.HandleImageEdits)
	rg.POST("/images/variations", h.gw.HandleImageVariations)
	// Audio.
	rg.POST("/audio/speech", h.gw.HandleAudioSpeech)
	rg.POST("/audio/transcriptions", h.gw.HandleAudioTranscriptions)
	rg.POST("/audio/translations", h.gw.HandleAudioTranslations)
	// Rerank.
	rg.POST("/rerank", h.gw.HandleRerank)
	// Video (async job): submit + poll. Returns 501 until the task store is
	// wired on the Gateway (Gateway.SetTaskSvc).
	rg.POST("/videos", h.gw.HandleVideoGenerations)
	rg.GET("/videos/:id", h.gw.HandleVideoGet)
}

// MountTools registers the platform's own tools (web search and page fetch).
// They live under /tools/v1, apart from the vendor-compatible /v1 and
// /anthropic/v1 surfaces. The group must already have the client auth
// middleware applied.
func (h *Handler) MountTools(rg *gin.RouterGroup) {
	rg.POST("/search", h.gw.HandleWebSearch)
	rg.POST("/fetch", h.gw.HandleWebFetch)
}

// MountModels registers OpenAI-compatible model listing endpoints.
// The group must already have the apikey middleware applied.
func (h *Handler) MountModels(rg *gin.RouterGroup) {
	rg.GET("/models", h.listModelsOpenAI)
}

// MountAnthropicMessages registers Anthropic generation endpoints.
// The group must already have the apikey middleware applied.
func (h *Handler) MountAnthropicMessages(rg *gin.RouterGroup) {
	rg.POST("/messages", h.gw.HandleMessages)
	rg.POST("/messages/count_tokens", h.gw.HandleMessagesCountTokens)
}

// MountAnthropicModels registers Anthropic model listing endpoints.
// The group must already have the apikey middleware applied.
func (h *Handler) MountAnthropicModels(rg *gin.RouterGroup) {
	rg.GET("/models", h.listModelsAnthropic)
}

// callerModels lists the models the authenticated caller may actually route
// to, with their capabilities over the caller's own groups. Like the
// Anthropic listing it starts from AccessibleModels, so the catalog matches
// what /chat/completions and /messages would accept for the same credential.
func (h *Handler) callerModels(c *gin.Context) ([]*ent.Model, map[uuid.UUID]biz.ModelCapabilities, error) {
	user, _ := middleware.UserFrom(c)
	apiKey, _ := middleware.ApiKeyFrom(c)
	ctx := c.Request.Context()
	ms, groups, err := h.models.AccessibleModelGroups(ctx, user, apiKey)
	if err != nil {
		return nil, nil, err
	}
	caps, err := h.models.ModelCapabilities(ctx, ms, groups)
	return ms, caps, err
}

func (h *Handler) listModelsOpenAI(c *gin.Context) {
	ms, caps, err := h.callerModels(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if llmpkg.IsGrokBuildClient(c.Request) {
		c.JSON(http.StatusOK, grokBuildModels(ms, caps))
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": openAIModels(ms, caps)})
}

// openAIModels builds the /v1/models data entries.
func openAIModels(ms []*ent.Model, caps map[uuid.UUID]biz.ModelCapabilities) []gin.H {
	out := make([]gin.H, 0, len(ms))
	for _, m := range ms {
		entry := gin.H{
			"id":       m.Name,
			"object":   "model",
			"created":  m.CreatedAt.Unix(),
			"owned_by": "jucode",
		}
		// Unconfigured capabilities are omitted, never defaulted: see
		// biz.ModelCapabilities.
		cp := caps[m.ID]
		if cp.ContextWindow > 0 {
			entry["context_window"] = cp.ContextWindow
			entry["max_context_window"] = cp.MaxContextWindow
		}
		if cp.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = cp.MaxOutputTokens
		}
		if len(cp.ReasoningEfforts) > 0 {
			entry["reasoning_efforts"] = cp.ReasoningEfforts
		}
		if m.DisplayName != nil {
			entry["display_name"] = *m.DisplayName
		}
		// Still listed so clients that validate a name against the list keep
		// working; pickers leave it out.
		if m.PickerHidden {
			entry["hidden"] = true
		}
		// The window through each of the caller's groups, for a client that
		// pins one (X-JuCode-Group): keyed by group id.
		if len(cp.Groups) > 0 {
			groups := make(gin.H, len(cp.Groups))
			for id, w := range cp.Groups {
				groups[id.String()] = gin.H{"context_window": w.ContextWindow, "max_context_window": w.MaxContextWindow}
			}
			entry["group_context_windows"] = groups
		}
		out = append(out, entry)
	}
	return out
}

func grokBuildModels(ms []*ent.Model, caps map[uuid.UUID]biz.ModelCapabilities) llmpkg.GrokBuildModelsResponse {
	infos := make([]llmpkg.GrokBuildModelInfo, 0, len(ms))
	for _, m := range ms {
		infos = append(infos, llmpkg.GrokBuildModelInfo{
			ID:               m.Name,
			Name:             m.Name,
			ContextWindow:    caps[m.ID].ContextWindow,
			ReasoningEfforts: caps[m.ID].ReasoningEfforts,
		})
	}
	return llmpkg.BuildGrokBuildModelsResponse(infos)
}

func (h *Handler) listModelsAnthropic(c *gin.Context) {
	user, _ := middleware.UserFrom(c)
	apiKey, _ := middleware.ApiKeyFrom(c)
	ms, err := h.models.AccessibleModels(c.Request.Context(), user, apiKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"type":  "error",
			"error": gin.H{"type": "api_error", "message": err.Error()},
		})
		return
	}
	out := make([]gin.H, 0, len(ms))
	for _, m := range ms {
		display := m.Name
		if m.DisplayName != nil {
			display = *m.DisplayName
		}
		out = append(out, gin.H{
			"type":         "model",
			"id":           m.Name,
			"display_name": display,
			"created_at":   m.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	resp := gin.H{
		"data":     out,
		"has_more": false,
	}
	if len(out) > 0 {
		resp["first_id"] = out[0]["id"]
		resp["last_id"] = out[len(out)-1]["id"]
	}
	c.JSON(http.StatusOK, resp)
}
