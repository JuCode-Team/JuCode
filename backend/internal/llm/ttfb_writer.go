package llm

import "github.com/gin-gonic/gin"

// ttfbResponseWriter marks the first downstream byte flush/write for
// request-level TTFB tracking.
type ttfbResponseWriter struct {
	gin.ResponseWriter
	mark   func()
	marked bool
}

func newTTFBResponseWriter(w gin.ResponseWriter, mark func()) gin.ResponseWriter {
	if w == nil || mark == nil {
		return w
	}
	return &ttfbResponseWriter{ResponseWriter: w, mark: mark}
}

func (w *ttfbResponseWriter) Write(data []byte) (int, error) {
	w.markOnce()
	return w.ResponseWriter.Write(data)
}

func (w *ttfbResponseWriter) WriteString(s string) (int, error) {
	w.markOnce()
	return w.ResponseWriter.WriteString(s)
}

func (w *ttfbResponseWriter) Flush() {
	w.markOnce()
	w.ResponseWriter.Flush()
}

func (w *ttfbResponseWriter) markOnce() {
	if w.marked {
		return
	}
	w.marked = true
	w.mark()
}
