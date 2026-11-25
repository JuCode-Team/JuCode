package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

// Mount called from admin.Mount via init-style helper. Mount() adds
// these endpoints into the same admin group so they inherit the JWT +
// AdminOnly + AdminAudit middleware chain.
func (h *Handler) mountPolicy(rg *gin.RouterGroup) {
	// Dictionary CRUD. Routes use "groups" (user-facing term); the
	// underlying ent schema name stays "PolicyCategory" for code-gen
	// stability — see internal/data/ent/schema/policy_category.go.
	rg.GET("/policy/stats", h.policyStats)
	rg.GET("/policy/groups", h.listPolicyGroups)
	rg.POST("/policy/groups", h.createPolicyGroup)
	rg.PATCH("/policy/groups/:id", h.updatePolicyGroup)
	rg.DELETE("/policy/groups/:id", h.deletePolicyGroup)

	rg.GET("/policy/keywords", h.listPolicyKeywords)
	rg.POST("/policy/keywords", h.createPolicyKeyword)
	rg.PATCH("/policy/keywords/:id", h.updatePolicyKeyword)
	rg.DELETE("/policy/keywords/:id", h.deletePolicyKeyword)
	rg.POST("/policy/groups/:id/import", h.importPolicyKeywords)

	// Allow-list phrases (false-positive suppression)
	rg.GET("/policy/allow-phrases", h.listAllowPhrases)
	rg.POST("/policy/allow-phrases", h.createAllowPhrase)
	rg.PATCH("/policy/allow-phrases/:id", h.updateAllowPhrase)
	rg.DELETE("/policy/allow-phrases/:id", h.deleteAllowPhrase)

	// User incidents
	rg.GET("/incidents", h.listIncidents)
	rg.GET("/incidents/stats", h.incidentStats)
	rg.POST("/incidents/:id/annotate", h.annotateIncident)
	// Admin audit log
	rg.GET("/audit-logs", h.listAuditLogs)
}

// --- groups ---

type policyGroupView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	Description     string `json:"description"`
	DefaultAction   string `json:"default_action"`
	DefaultSeverity string `json:"default_severity"`
	Scope           string `json:"scope"`
	Enabled         bool   `json:"enabled"`
	SortOrder       int    `json:"sort_order"`
}

func toPolicyGroupView(c *ent.PolicyCategory) policyGroupView {
	return policyGroupView{
		ID:              c.ID.String(),
		Name:            c.Name,
		Slug:            c.Slug,
		Description:     c.Description,
		DefaultAction:   string(c.DefaultAction),
		DefaultSeverity: string(c.DefaultSeverity),
		Scope:           string(c.Scope),
		Enabled:         c.Enabled,
		SortOrder:       c.SortOrder,
	}
}

func (h *Handler) policyStats(c *gin.Context) {
	s, err := h.policy.Stats(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

func (h *Handler) listPolicyGroups(c *gin.Context) {
	cats, err := h.policy.ListCategories(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		filtered := cats[:0]
		for _, x := range cats {
			if strings.Contains(strings.ToLower(x.Name), q) ||
				strings.Contains(strings.ToLower(x.Slug), q) ||
				strings.Contains(strings.ToLower(x.Description), q) {
				filtered = append(filtered, x)
			}
		}
		cats = filtered
	}
	total := len(cats)
	limit, offset := parsePage(c, 500)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := cats[start:end]
	out := make([]policyGroupView, 0, len(page))
	for _, x := range page {
		out = append(out, toPolicyGroupView(x))
	}
	c.JSON(http.StatusOK, gin.H{
		"groups": out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

type policyGroupReq struct {
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	Description     string `json:"description"`
	DefaultAction   string `json:"default_action"`
	DefaultSeverity string `json:"default_severity"`
	Scope           string `json:"scope"`
	Enabled         *bool  `json:"enabled"`
	SortOrder       int    `json:"sort_order"`
}

func (h *Handler) createPolicyGroup(c *gin.Context) {
	var req policyGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Slug) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and slug required"})
		return
	}
	cat, err := h.policy.CreateCategory(c.Request.Context(), biz.CategoryInput{
		Name:            strings.TrimSpace(req.Name),
		Slug:            strings.TrimSpace(req.Slug),
		Description:     req.Description,
		DefaultAction:   req.DefaultAction,
		DefaultSeverity: req.DefaultSeverity,
		Scope:           req.Scope,
		Enabled:         req.Enabled,
		SortOrder:       req.SortOrder,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, toPolicyGroupView(cat))
}

func (h *Handler) updatePolicyGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req policyGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cat, err := h.policy.UpdateCategory(c.Request.Context(), id, biz.CategoryInput{
		Name:            req.Name,
		Slug:            req.Slug,
		Description:     req.Description,
		DefaultAction:   req.DefaultAction,
		DefaultSeverity: req.DefaultSeverity,
		Scope:           req.Scope,
		Enabled:         req.Enabled,
		SortOrder:       req.SortOrder,
	})
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, toPolicyGroupView(cat))
}

func (h *Handler) deletePolicyGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.policy.DeleteCategory(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- keywords ---

type policyKeywordView struct {
	ID        string `json:"id"`
	GroupID   string `json:"group_id"`
	Pattern   string `json:"pattern"`
	Severity  string `json:"severity,omitempty"`
	MatchMode string `json:"match_mode"`
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
}

func toPolicyKeywordView(k *ent.PolicyKeyword) policyKeywordView {
	v := policyKeywordView{
		ID:        k.ID.String(),
		GroupID:   k.CategoryID.String(),
		Pattern:   k.Pattern,
		MatchMode: string(k.MatchMode),
		Enabled:   k.Enabled,
		Note:      k.Note,
		CreatedAt: k.CreatedAt.Format(time.RFC3339),
	}
	if k.Severity != nil {
		v.Severity = string(*k.Severity)
	}
	return v
}

func (h *Handler) listPolicyKeywords(c *gin.Context) {
	var groupID uuid.UUID
	if v := c.Query("group_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			groupID = id
		}
	}
	q := c.Query("q")
	limit, offset := parsePage(c, 500)
	rows, total, err := h.policy.ListKeywords(c.Request.Context(), groupID, q, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]policyKeywordView, 0, len(rows))
	for _, k := range rows {
		out = append(out, toPolicyKeywordView(k))
	}
	c.JSON(http.StatusOK, gin.H{
		"keywords": out,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

type policyKeywordReq struct {
	GroupID   string `json:"group_id"`
	Pattern   string `json:"pattern"`
	Severity  string `json:"severity"`
	MatchMode string `json:"match_mode"`
	Note      string `json:"note"`
	Enabled   *bool  `json:"enabled"`
}

func (h *Handler) createPolicyKeyword(c *gin.Context) {
	var req policyKeywordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cid, err := uuid.Parse(req.GroupID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group_id"})
		return
	}
	k, err := h.policy.CreateKeyword(c.Request.Context(), biz.KeywordInput{
		CategoryID: cid,
		Pattern:    req.Pattern,
		Severity:   req.Severity,
		MatchMode:  req.MatchMode,
		Note:       req.Note,
		Enabled:    req.Enabled,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, toPolicyKeywordView(k))
}

func (h *Handler) updatePolicyKeyword(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req policyKeywordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.KeywordInput{
		Pattern:   req.Pattern,
		Severity:  req.Severity,
		MatchMode: req.MatchMode,
		Note:      req.Note,
		Enabled:   req.Enabled,
	}
	if req.GroupID != "" {
		if cid, err := uuid.Parse(req.GroupID); err == nil {
			in.CategoryID = cid
		}
	}
	k, err := h.policy.UpdateKeyword(c.Request.Context(), id, in)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, toPolicyKeywordView(k))
}

func (h *Handler) deletePolicyKeyword(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.policy.DeleteKeyword(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type policyImportReq struct {
	Text string `json:"text"`
}

func (h *Handler) importPolicyKeywords(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req policyImportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.policy.ImportKeywords(c.Request.Context(), id, req.Text)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": res})
}

// --- allow phrases ---

type allowPhraseView struct {
	ID        string `json:"id"`
	Phrase    string `json:"phrase"`
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
}

func toAllowPhraseView(a *ent.PolicyAllowPhrase) allowPhraseView {
	return allowPhraseView{
		ID:        a.ID.String(),
		Phrase:    a.Phrase,
		Enabled:   a.Enabled,
		Note:      a.Note,
		CreatedAt: a.CreatedAt.Format(time.RFC3339),
	}
}

type allowPhraseReq struct {
	Phrase  string `json:"phrase"`
	Note    string `json:"note"`
	Enabled *bool  `json:"enabled"`
}

func (h *Handler) listAllowPhrases(c *gin.Context) {
	rows, err := h.policy.ListAllowPhrases(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]allowPhraseView, 0, len(rows))
	for _, a := range rows {
		out = append(out, toAllowPhraseView(a))
	}
	c.JSON(http.StatusOK, gin.H{"allow_phrases": out, "total": len(out)})
}

func (h *Handler) createAllowPhrase(c *gin.Context) {
	var req allowPhraseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	a, err := h.policy.CreateAllowPhrase(c.Request.Context(), biz.AllowPhraseInput{
		Phrase:  req.Phrase,
		Note:    req.Note,
		Enabled: req.Enabled,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, toAllowPhraseView(a))
}

func (h *Handler) updateAllowPhrase(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req allowPhraseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	a, err := h.policy.UpdateAllowPhrase(c.Request.Context(), id, biz.AllowPhraseInput{
		Phrase:  req.Phrase,
		Note:    req.Note,
		Enabled: req.Enabled,
	})
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, toAllowPhraseView(a))
}

func (h *Handler) deleteAllowPhrase(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.policy.DeleteAllowPhrase(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// --- incidents ---

type incidentView struct {
	ID          string         `json:"id"`
	CreatedAt   string         `json:"created_at"`
	UserID      string         `json:"user_id,omitempty"`
	ApiKeyID    string         `json:"api_key_id,omitempty"`
	Kind        string         `json:"kind"`
	Severity    string         `json:"severity"`
	Action      string         `json:"action,omitempty"`
	ReviewLabel string         `json:"review_label"`
	Model       string         `json:"model,omitempty"`
	Endpoint    string         `json:"endpoint,omitempty"`
	RequestID   string         `json:"request_id,omitempty"`
	IP          string         `json:"ip,omitempty"`
	Message     string         `json:"message"`
	Payload     map[string]any `json:"payload,omitempty"`
}

func toIncidentView(x *ent.UserIncident) incidentView {
	v := incidentView{
		ID:          x.ID.String(),
		CreatedAt:   x.CreatedAt.Format(time.RFC3339),
		Kind:        string(x.Kind),
		Severity:    string(x.Severity),
		Action:      x.Action,
		ReviewLabel: string(x.ReviewLabel),
		Model:       x.Model,
		Endpoint:    x.Endpoint,
		RequestID:   x.RequestID,
		IP:          x.IP,
		Message:     x.Message,
		Payload:     x.Payload,
	}
	if x.UserID != nil {
		v.UserID = x.UserID.String()
	}
	if x.APIKeyID != nil {
		v.ApiKeyID = x.APIKeyID.String()
	}
	return v
}

func (h *Handler) listIncidents(c *gin.Context) {
	in := biz.IncidentListInput{
		Kind:     c.Query("kind"),
		Severity: c.Query("severity"),
	}
	if v := c.Query("user_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			in.UserID = &id
		}
	}
	if v := c.Query("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			in.Since = time.Unix(n, 0)
		}
	}
	if v := c.Query("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			in.Until = time.Unix(n, 0)
		}
	}
	limit, offset := parsePage(c, 500)
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))

	if q == "" {
		in.Limit, in.Offset = limit, offset
		rows, total, err := h.incidents.List(c.Request.Context(), in)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out := make([]incidentView, 0, len(rows))
		for _, r := range rows {
			out = append(out, toIncidentView(r))
		}
		c.JSON(http.StatusOK, gin.H{
			"incidents": out,
			"total":     total,
			"limit":     limit,
			"offset":    offset,
		})
		return
	}

	// With q: fetch the most-recent 500 matching the biz filters, then
	// substring-filter + paginate in handler. Effective search window is
	// capped to 500 rows; narrow with kind/severity/user_id/since/until
	// for larger backlogs.
	in.Limit, in.Offset = 500, 0
	rows, _, err := h.incidents.List(c.Request.Context(), in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	filtered := rows[:0]
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Message), q) ||
			strings.Contains(strings.ToLower(string(r.Kind)), q) {
			filtered = append(filtered, r)
		}
	}
	total := len(filtered)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := filtered[start:end]
	out := make([]incidentView, 0, len(page))
	for _, r := range page {
		out = append(out, toIncidentView(r))
	}
	c.JSON(http.StatusOK, gin.H{
		"incidents": out,
		"total":     total,
		"limit":     limit,
		"offset":    offset,
	})
}

// incidentStats powers the charts at the top of the "用户异常" page:
// breakdowns by kind/severity/action/review_label plus a daily trend.
// Optional user_id + since/until (unix seconds) scope the window; default
// is the trailing 30 days across all users.
func (h *Handler) incidentStats(c *gin.Context) {
	var in biz.IncidentStatsInput
	if v := c.Query("user_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			in.UserID = &id
		}
	}
	if v := c.Query("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			in.Since = time.Unix(n, 0)
		}
	}
	if v := c.Query("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			in.Until = time.Unix(n, 0)
		}
	}
	stats, err := h.incidents.Stats(c.Request.Context(), in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// --- audit logs ---

type auditView struct {
	ID          string         `json:"id"`
	CreatedAt   string         `json:"created_at"`
	ActorUserID string         `json:"actor_user_id"`
	Action      string         `json:"action"`
	Target      string         `json:"target,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
	IP          string         `json:"ip,omitempty"`
	UA          string         `json:"ua,omitempty"`
}

func toAuditView(x *ent.AdminAuditLog) auditView {
	return auditView{
		ID:          x.ID.String(),
		CreatedAt:   x.CreatedAt.Format(time.RFC3339),
		ActorUserID: x.ActorUserID.String(),
		Action:      x.Action,
		Target:      x.Target,
		Payload:     x.Payload,
		IP:          x.IP,
		UA:          x.Ua,
	}
}

func (h *Handler) listAuditLogs(c *gin.Context) {
	in := biz.AuditListInput{
		Action: c.Query("action"),
		Target: c.Query("target"),
	}
	if v := c.Query("actor_user_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			in.ActorUserID = &id
		}
	}
	if v := c.Query("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			in.Since = time.Unix(n, 0)
		}
	}
	if v := c.Query("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			in.Until = time.Unix(n, 0)
		}
	}
	limit, offset := parsePage(c, 500)
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))

	if q == "" {
		in.Limit, in.Offset = limit, offset
		rows, total, err := h.audit.List(c.Request.Context(), in)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out := make([]auditView, 0, len(rows))
		for _, r := range rows {
			out = append(out, toAuditView(r))
		}
		c.JSON(http.StatusOK, gin.H{
			"logs":   out,
			"total":  total,
			"limit":  limit,
			"offset": offset,
		})
		return
	}

	// With q: fetch the most-recent 500 matching the biz filters, then
	// substring-filter + paginate in handler. Effective search window is
	// capped to 500 rows; narrow with action/target/actor_user_id/since/until
	// for larger backlogs.
	in.Limit, in.Offset = 500, 0
	rows, _, err := h.audit.List(c.Request.Context(), in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	filtered := rows[:0]
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Action), q) ||
			strings.Contains(strings.ToLower(r.Target), q) {
			filtered = append(filtered, r)
		}
	}
	total := len(filtered)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := filtered[start:end]
	out := make([]auditView, 0, len(page))
	for _, r := range page {
		out = append(out, toAuditView(r))
	}
	c.JSON(http.StatusOK, gin.H{
		"logs":   out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// --- incident annotation (false-positive feedback loop) ---

type annotateReq struct {
	Label string `json:"label"` // true_positive | false_positive | unreviewed
}

func (h *Handler) annotateIncident(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req annotateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.incidents.Annotate(c.Request.Context(), id, req.Label); err != nil {
		if err == biz.ErrInvalidReviewLabel {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
