package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/metrics"
)

// Metrics records per-route request count and latency. The path label is
// the route pattern (c.FullPath), never the raw URL — a raw path would
// put every :id parameter value into its own time series.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		metrics.HTTPRequestsTotal.WithLabelValues(c.Request.Method, path, strconv.Itoa(c.Writer.Status())).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(c.Request.Method, path).Observe(time.Since(start).Seconds())
	}
}
