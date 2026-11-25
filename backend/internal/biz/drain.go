package biz

import (
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

type DrainSvc struct {
	draining atomic.Bool
	active   atomic.Int64
}

func NewDrainSvc() *DrainSvc { return &DrainSvc{} }

func (s *DrainSvc) SetDraining(v bool) { s.draining.Store(v) }
func (s *DrainSvc) IsDraining() bool   { return s.draining.Load() }
func (s *DrainSvc) Active() int64      { return s.active.Load() }

func (s *DrainSvc) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isInternalHealthPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		if s.IsDraining() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "instance draining"})
			c.Abort()
			return
		}
		s.active.Add(1)
		defer s.active.Add(-1)
		c.Next()
	}
}

func isInternalHealthPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/internal/drain/status", "/internal/drain/start", "/internal/drain/stop":
		return true
	default:
		return false
	}
}
