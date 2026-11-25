// admin_provider_definitions.go: provider definitions.
//
// A definition is the vendor payload an account is created from — endpoint
// shape, the credential it asks for, and models.
package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent/schema"
)

// SetProviderDefinitions wires the definition service in after construction;
// nil hides the definition routes.
func (h *Handler) SetProviderDefinitions(defs *biz.ProviderDefinitionSvc) {
	h.providerDefinitions = defs
}

func (h *Handler) mountProviderDefinitions(rg *gin.RouterGroup) {
	if h.providerDefinitions == nil {
		return
	}
	rg.GET("/provider-definitions", h.listProviderDefinitions)
	rg.POST("/provider-definitions", h.upsertProviderDefinition)
	rg.POST("/provider-definitions/seed", h.seedProviderDefinitions)
	rg.GET("/provider-definitions/:definition_id", h.getProviderDefinition)
	rg.PATCH("/provider-definitions/:definition_id", h.updateProviderDefinition)
	rg.DELETE("/provider-definitions/:definition_id", h.deleteProviderDefinition)
}

func writeDefinitionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrDefinitionNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "provider definition not found"})
	case errors.Is(err, biz.ErrDefinitionBadInput), errors.Is(err, biz.ErrAccountBadInput):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func (h *Handler) listProviderDefinitions(c *gin.Context) {
	views, err := h.providerDefinitions.List(c.Request.Context())
	if err != nil {
		writeDefinitionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"definitions": views})
}

func (h *Handler) getProviderDefinition(c *gin.Context) {
	view, err := h.providerDefinitions.Resolve(c.Request.Context(), c.Param("definition_id"))
	if err != nil {
		writeDefinitionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"definition": view})
}

func (h *Handler) seedProviderDefinitions(c *gin.Context) {
	inserted, skipped, err := h.providerDefinitions.SeedBuiltins(c.Request.Context())
	if err != nil {
		writeDefinitionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"inserted": inserted, "skipped": skipped})
}

type upsertProviderDefinitionRequest struct {
	Name            string              `json:"name"`
	Vendor          string              `json:"vendor"`
	BaseURL         string              `json:"base_url"`
	APIFormat       string              `json:"api_format"`
	NativeProtocols []string            `json:"native_protocols"`
	DefaultProtocol string              `json:"default_protocol"`
	AllowConversion bool                `json:"allow_conversion"`
	EndpointPaths   map[string]string   `json:"endpoint_paths"`
	HeaderRules     *schema.HeaderRules `json:"header_rules"`
	ModelsJSON      string              `json:"models_json"`
	ModelsPath      string              `json:"models_path"`
	Enabled         *bool               `json:"enabled"`
	Note            string              `json:"note"`
	CredentialLabel string              `json:"credential_label"`
	CredentialHint  string              `json:"credential_hint"`
}

func (h *Handler) upsertProviderDefinition(c *gin.Context) {
	definitionID := strings.TrimSpace(c.Param("definition_id"))
	if definitionID == "" {
		var body struct {
			DefinitionID string `json:"definition_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.DefinitionID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "definition_id is required"})
			return
		}
		definitionID = strings.TrimSpace(body.DefinitionID)
	}
	h.upsertProviderDefinitionBody(c, definitionID)
}

func (h *Handler) updateProviderDefinition(c *gin.Context) {
	definitionID := strings.TrimSpace(c.Param("definition_id"))
	if definitionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "definition_id is required"})
		return
	}
	h.upsertProviderDefinitionBody(c, definitionID)
}

func (h *Handler) upsertProviderDefinitionBody(c *gin.Context, definitionID string) {
	var req upsertProviderDefinitionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	view, err := h.providerDefinitions.Upsert(c.Request.Context(), biz.UpsertProviderDefinitionInput{
		DefinitionID:    definitionID,
		Name:            req.Name,
		Vendor:          req.Vendor,
		BaseURL:         req.BaseURL,
		APIFormat:       req.APIFormat,
		NativeProtocols: req.NativeProtocols,
		DefaultProtocol: req.DefaultProtocol,
		AllowConversion: req.AllowConversion,
		EndpointPaths:   req.EndpointPaths,
		HeaderRules:     req.HeaderRules,
		ModelsJSON:      req.ModelsJSON,
		ModelsPath:      req.ModelsPath,
		Enabled:         req.Enabled,
		Note:            req.Note,
		CredentialLabel: req.CredentialLabel,
		CredentialHint:  req.CredentialHint,
	})
	if err != nil {
		writeDefinitionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"definition": view})
}

func (h *Handler) deleteProviderDefinition(c *gin.Context) {
	if err := h.providerDefinitions.Delete(c.Request.Context(), c.Param("definition_id")); err != nil {
		writeDefinitionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
