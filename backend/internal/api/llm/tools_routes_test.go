package llm

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// The web tools are JuCode's own API: they mount under /tools/v1 and are not
// part of the vendor-compatible /v1 generation surface.
func TestWebToolsMountOnlyUnderTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := New(nil, nil)
	h.MountTools(r.Group("/tools/v1"))
	h.MountMediaGeneration(r.Group("/v1"))

	routes := map[string]bool{}
	for _, rt := range r.Routes() {
		routes[rt.Method+" "+rt.Path] = true
	}
	for _, want := range []string{"POST /tools/v1/search", "POST /tools/v1/fetch"} {
		if !routes[want] {
			t.Errorf("missing route %s", want)
		}
	}
	for path := range routes {
		if path == http.MethodPost+" /v1/web/search" || path == http.MethodPost+" /v1/web/fetch" {
			t.Errorf("web tools must not mount under /v1: %s", path)
		}
	}
}
