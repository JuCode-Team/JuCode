package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CORS allows the frontend dev server (and prod build behind any origin) to
// talk to the API. For MVP we set Access-Control-Allow-Origin from the
// request's Origin header so credentials and Authorization can flow.
//
// In production, replace this with an explicit allow-list driven by config.
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Vary", "Origin")
		} else {
			c.Header("Access-Control-Allow-Origin", "*")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Api-Key, X-JuCode-Group, X-JuCode-Billing-Source")
		// The web app looks a reply's cost up by its request id.
		c.Header("Access-Control-Expose-Headers", "X-Request-Id")
		c.Header("Access-Control-Max-Age", "86400")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
