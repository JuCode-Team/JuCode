package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/data/ent"
)

// silence unused-import for the moment if future code grows.
var _ = ent.WebVital{}

// Public ingest endpoint — no auth, no user id, no PII. Browsers post
// here with sendBeacon when web-vitals fires. The endpoint is rate-
// limit-bounded by accepting at most ~5 metrics per request (one batch
// per page-load is typical) and bounding field lengths.
type wvBatchReq struct {
	Session string `json:"session"`
	Path    string `json:"path"`
	Metrics []struct {
		Name    string  `json:"name"`
		Value   float64 `json:"value"`
		Rating  string  `json:"rating"`
		NavType string  `json:"nav_type"`
	} `json:"metrics"`
}

// IngestWebVitals is mounted on the public router (no admin auth)
// since the browser must reach it before any user signs in.
func IngestWebVitals(client *ent.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req wvBatchReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		if len(req.Metrics) == 0 || len(req.Metrics) > 16 {
			c.Status(http.StatusOK) // silently drop — don't waste DB round-trips
			return
		}
		ua := truncate(c.Request.UserAgent(), 200)
		path := truncate(stripQuery(req.Path), 200)
		session := truncate(req.Session, 36)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		// Bulk insert — one DB round-trip regardless of batch size.
		bulk := make([]*ent.WebVitalCreate, 0, len(req.Metrics))
		for _, m := range req.Metrics {
			name := strings.ToUpper(strings.TrimSpace(m.Name))
			if !knownMetric(name) {
				continue
			}
			bulk = append(bulk, client.WebVital.Create().
				SetName(name).
				SetValue(m.Value).
				SetRating(truncate(m.Rating, 24)).
				SetPath(path).
				SetNavType(truncate(m.NavType, 24)).
				SetUa(ua).
				SetSession(session),
			)
		}
		if len(bulk) == 0 {
			c.Status(http.StatusOK)
			return
		}
		if _, err := client.WebVital.CreateBulk(bulk...).Save(ctx); err != nil {
			// Don't bubble — RUM ingest must never block the page.
			c.Status(http.StatusAccepted)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// knownMetric guards the name field so the table doesn't grow weird
// values if a client misbehaves.
func knownMetric(name string) bool {
	switch name {
	case "LCP", "CLS", "INP", "FCP", "TTFB", "FID":
		return true
	}
	return false
}

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// --- admin query endpoint: aggregated p50/p75/p95 by metric ----------

type wvAggRow struct {
	Name    string  `json:"name"`
	Count   int     `json:"count"`
	P50     float64 `json:"p50"`
	P75     float64 `json:"p75"`
	P95     float64 `json:"p95"`
	GoodPct float64 `json:"good_pct"`
}

// listWebVitals returns one row per metric name with p50/p75/p95 and
// the % of samples rated "good" by the browser. Window defaults to
// the last 7 days; ?days=N to extend.
func (h *Handler) listWebVitals(c *gin.Context) {
	days := 7
	if v := c.Query("days"); v != "" {
		// best-effort parse — leave default if invalid
		_, _ = fmt.Sscanf(v, "%d", &days)
		if days < 1 || days > 90 {
			days = 7
		}
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	db := h.rawDB
	if db == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "raw db handle not wired"})
		return
	}
	rows, err := db.QueryContext(
		c.Request.Context(),
		`SELECT
		   name,
		   COUNT(*) AS n,
		   COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY value), 0) AS p50,
		   COALESCE(percentile_cont(0.75) WITHIN GROUP (ORDER BY value), 0) AS p75,
		   COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY value), 0) AS p95,
		   COALESCE(AVG(CASE WHEN rating='good' THEN 1.0 ELSE 0.0 END) * 100, 0) AS good_pct
		 FROM web_vitals
		 WHERE created_at >= $1
		 GROUP BY name
		 ORDER BY name`,
		cutoff,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusOK, gin.H{"window_days": days, "rows": []wvAggRow{}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []wvAggRow{}
	for rows.Next() {
		var r wvAggRow
		if err := rows.Scan(&r.Name, &r.Count, &r.P50, &r.P75, &r.P95, &r.GoodPct); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out = append(out, r)
	}
	c.JSON(http.StatusOK, gin.H{"window_days": days, "rows": out})
}
