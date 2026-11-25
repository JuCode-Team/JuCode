package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// CtxRequestID is the gin.Context key holding the per-request trace ID.
const CtxRequestID = "jucode.request_id"

// RequestID stamps every request with a stable identifier. Precedence:
//  1. CF-Ray (set by Cloudflare; lets ops correlate JuCode logs with CF
//     dashboard logs without an extra join)
//  2. X-Request-Id (set by upstream proxies / clients that already trace)
//  3. Fresh UUIDv4
//
// The chosen ID is echoed back as the X-Request-Id response header and
// stored at c.GetString(CtxRequestID) for handlers and other middlewares
// (audit, rate-limit logger, incident recorder).
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("CF-Ray")
		if id == "" {
			id = c.GetHeader("X-Request-Id")
		}
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(CtxRequestID, id)
		c.Header("X-Request-Id", id)
		c.Next()
	}
}

// RequestIDFrom returns the request ID stored by RequestID() — empty
// string when the middleware didn't run (callers should treat that as
// best-effort observability, not an error condition).
func RequestIDFrom(c *gin.Context) string {
	return c.GetString(CtxRequestID)
}
