package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
)

const opsReportTTL = 30 * time.Second

func (h *Handler) loadOpsReport(c *gin.Context) (*biz.OpsReport, bool, error) {
	from, fromDate := parseTimeQuery(c.Query("from"))
	to, toDate := parseTimeQuery(c.Query("to"))
	from, to = biz.NormalizeOpsRange(from, to, fromDate, toDate)
	grain := c.Query("grain")
	key := fmt.Sprintf("cache:admin:ops:%s:%s:%s", from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), biz.ParseOpsGrain(grain, from, to))
	return cache.GetOrCompute(c.Request.Context(), h.cache, key, opsReportTTL, func(ctx context.Context) (*biz.OpsReport, error) {
		return h.finance.OpsReport(ctx, from, to, grain)
	})
}

func (h *Handler) opsReport(c *gin.Context) {
	out, hit, err := h.loadOpsReport(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	c.JSON(http.StatusOK, out)
}

func (h *Handler) opsExport(c *gin.Context) {
	out, hit, err := h.loadOpsReport(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	name := fmt.Sprintf("ops-%s-%s.xlsx", out.From.Format("20060102"), out.To.Format("20060102"))
	c.Header("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	if err := biz.WriteOpsXLSX(c.Writer, out); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
