package llm

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestTTFBResponseWriterMarksOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	marks := 0
	c.Writer = newTTFBResponseWriter(c.Writer, func() {
		marks++
	})

	c.Writer.Flush()
	if marks != 1 {
		t.Fatalf("Flush marks = %d, want 1", marks)
	}
	if _, err := c.Writer.Write([]byte("ok")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if marks != 1 {
		t.Fatalf("Write after Flush marks = %d, want 1", marks)
	}
}
