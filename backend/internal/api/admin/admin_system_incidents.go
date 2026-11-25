package admin

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

func (h *Handler) mountSystemIncidents(rg *gin.RouterGroup) {
	// Register the literal collection route before the parameter route.
	rg.GET("/system-incidents", h.listSystemIncidents)
	rg.GET("/system-incidents/:id", h.getSystemIncident)
	rg.PATCH("/system-incidents/:id", h.updateSystemIncident)
}

type systemIncidentView struct {
	ID                   string   `json:"id"`
	GroupID              string   `json:"group_id"`
	GroupName            string   `json:"group_name"`
	Model                string   `json:"model"`
	Severity             string   `json:"severity"`
	WorstSeverity        string   `json:"worst_severity"`
	StartedAt            string   `json:"started_at"`
	DetectedAt           string   `json:"detected_at"`
	ResolvedAt           *string  `json:"resolved_at"`
	DurationSeconds      int64    `json:"duration_seconds"`
	OpenAvailabilityPct  *float64 `json:"open_availability_pct"`
	WorstAvailabilityPct *float64 `json:"worst_availability_pct"`
	LastAvailabilityPct  *float64 `json:"last_availability_pct"`
	SampleCount          int      `json:"sample_count"`
	Resolution           string   `json:"resolution"`
	Note                 string   `json:"note"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
}

func toSystemIncidentView(row *ent.SystemIncident, now time.Time) systemIncidentView {
	end := now
	var resolvedAt *string
	if row.ResolvedAt != nil {
		end = *row.ResolvedAt
		formatted := row.ResolvedAt.UTC().Format(time.RFC3339)
		resolvedAt = &formatted
	}
	duration := int64(end.Sub(row.StartedAt).Seconds())
	if duration < 0 {
		duration = 0
	}
	return systemIncidentView{
		ID:                   row.ID.String(),
		GroupID:              row.GroupID.String(),
		GroupName:            row.GroupName,
		Model:                row.Model,
		Severity:             string(row.Severity),
		WorstSeverity:        string(row.WorstSeverity),
		StartedAt:            row.StartedAt.UTC().Format(time.RFC3339),
		DetectedAt:           row.DetectedAt.UTC().Format(time.RFC3339),
		ResolvedAt:           resolvedAt,
		DurationSeconds:      duration,
		OpenAvailabilityPct:  row.OpenAvailabilityPct,
		WorstAvailabilityPct: row.WorstAvailabilityPct,
		LastAvailabilityPct:  row.LastAvailabilityPct,
		SampleCount:          row.SampleCount,
		Resolution:           row.Resolution,
		Note:                 row.Note,
		CreatedAt:            row.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:            row.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (h *Handler) listSystemIncidents(c *gin.Context) {
	status := strings.TrimSpace(c.DefaultQuery("status", "all"))
	if status != "open" && status != "resolved" && status != "all" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	in := biz.SystemIncidentListInput{Status: status}
	if raw := strings.TrimSpace(c.Query("group_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group_id"})
			return
		}
		in.GroupID = &id
	}
	var ok bool
	if in.Start, ok = parseRFC3339Query(c, "start"); !ok {
		return
	}
	if in.End, ok = parseRFC3339Query(c, "end"); !ok {
		return
	}
	in.Limit, in.Offset = parsePage(c, 200)
	rows, total, err := h.systemIncidents.List(c.Request.Context(), in)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	out := make([]systemIncidentView, 0, len(rows))
	for _, row := range rows {
		out = append(out, toSystemIncidentView(row, now))
	}
	c.JSON(http.StatusOK, gin.H{
		"incidents": out,
		"total":     total,
		"limit":     in.Limit,
		"offset":    in.Offset,
	})
}

func (h *Handler) getSystemIncident(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	row, err := h.systemIncidents.Get(c.Request.Context(), id)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"incident": toSystemIncidentView(row, time.Now().UTC())})
}

func (h *Handler) updateSystemIncident(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Note *string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Note == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "note is required"})
		return
	}
	if utf8.RuneCountInString(*req.Note) > 2000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "note exceeds 2000 characters"})
		return
	}
	row, err := h.systemIncidents.UpdateNote(c.Request.Context(), id, *req.Note)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"incident": toSystemIncidentView(row, time.Now().UTC())})
}

func parseRFC3339Query(c *gin.Context, name string) (*time.Time, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return nil, false
	}
	value = value.UTC()
	return &value, true
}
