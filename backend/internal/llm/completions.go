package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// HandleCompletions implements POST /v1/completions (legacy text
// completions). The request is rewritten to Chat Completions, run through
// the same handle() pipeline, then rewritten back so clients that still
// speak the old shape keep working.
func (g *Gateway) HandleCompletions(c *gin.Context) {
	raw, err := readLimitedBody(c, g.maxBodyBytes)
	if err != nil {
		requestID := gatewayRequestID(c)
		if errors.Is(err, errRequestBodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, openAIError("invalid_request_error", "request_body_too_large", "request body too large", requestID))
			return
		}
		c.JSON(http.StatusBadRequest, openAIError("invalid_request_error", "read_body_failed", "failed to read request body", requestID))
		return
	}
	chatBody, stream, err := completionsToChatBody(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, openAIError("invalid_request_error", "invalid_json", err.Error(), gatewayRequestID(c)))
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(chatBody))
	c.Request.ContentLength = int64(len(chatBody))
	c.Writer = newCompletionsRewriteWriter(c.Writer, stream)
	g.handle(c, "/v1/chat/completions", entusagelog.EndpointChatCompletions, true)
}

func readLimitedBody(c *gin.Context, max int64) ([]byte, error) {
	if max > 0 {
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) > max {
			return nil, errRequestBodyTooLarge
		}
		return raw, nil
	}
	return io.ReadAll(c.Request.Body)
}

var errRequestBodyTooLarge = errors.New("request body too large")

func completionsToChatBody(raw []byte) ([]byte, bool, error) {
	src, err := decodeObject(raw)
	if err != nil {
		return nil, false, err
	}
	if asSlice(src["messages"]) == nil {
		prompt := flattenChatContent(src["prompt"])
		if prompt == "" {
			if s := strVal(src["prompt"]); s != "" {
				prompt = s
			}
		}
		src["messages"] = []any{
			map[string]any{"role": "user", "content": prompt},
		}
	}
	delete(src, "prompt")
	delete(src, "suffix")
	delete(src, "echo")
	delete(src, "logprobs")
	delete(src, "best_of")
	stream := boolVal(src["stream"])
	out, err := json.Marshal(src)
	return out, stream, err
}

type completionsRewriteWriter struct {
	gin.ResponseWriter
	stream bool
	buf    bytes.Buffer
}

func newCompletionsRewriteWriter(w gin.ResponseWriter, stream bool) gin.ResponseWriter {
	return &completionsRewriteWriter{ResponseWriter: w, stream: stream}
}

func (w *completionsRewriteWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if !w.stream {
		if json.Valid(bytes.TrimSpace(w.buf.Bytes())) {
			out := chatJSONToCompletions(w.buf.Bytes())
			w.buf.Reset()
			return w.ResponseWriter.Write(out)
		}
		return len(p), nil
	}
	data := w.buf.Bytes()
	var rest []byte
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			rest = append([]byte(nil), data...)
			break
		}
		line := data[:i+1]
		data = data[i+1:]
		if _, err := w.ResponseWriter.Write(rewriteCompletionsSSE(line)); err != nil {
			return len(p), err
		}
	}
	w.buf.Reset()
	w.buf.Write(rest)
	return len(p), nil
}

func (w *completionsRewriteWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *completionsRewriteWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
}

func (w *completionsRewriteWriter) Flush() {
	if !w.stream && w.buf.Len() > 0 {
		out := chatJSONToCompletions(w.buf.Bytes())
		_, _ = w.ResponseWriter.Write(out)
		w.buf.Reset()
	}
	w.ResponseWriter.Flush()
}

func (w *completionsRewriteWriter) Written() bool {
	if w.buf.Len() > 0 {
		return true
	}
	return w.ResponseWriter.Written()
}

func chatJSONToCompletions(raw []byte) []byte {
	obj, err := decodeObject(raw)
	if err != nil {
		return raw
	}
	if choices := asSlice(obj["choices"]); len(choices) > 0 {
		outChoices := make([]any, 0, len(choices))
		for i, ch := range choices {
			m := asMap(ch)
			if m == nil {
				continue
			}
			text := ""
			if msg := asMap(m["message"]); msg != nil {
				text = flattenChatContent(msg["content"])
			}
			if text == "" {
				if d := asMap(m["delta"]); d != nil {
					text = flattenChatContent(d["content"])
				}
			}
			outChoices = append(outChoices, map[string]any{
				"text":          text,
				"index":         i,
				"logprobs":      nil,
				"finish_reason": m["finish_reason"],
			})
		}
		obj["choices"] = outChoices
	}
	obj["object"] = "text_completion"
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

func rewriteCompletionsSSE(p []byte) []byte {
	lines := bytes.Split(p, []byte("\n"))
	var out bytes.Buffer
	for i, line := range lines {
		trim := bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(trim, []byte("data:")) {
			payload := bytes.TrimSpace(trim[len("data:"):])
			if len(payload) > 0 && payload[0] == '{' {
				payload = chatChunkToCompletions(payload)
			}
			out.WriteString("data: ")
			out.Write(payload)
		} else {
			out.Write(trim)
		}
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func chatChunkToCompletions(payload []byte) []byte {
	obj, err := decodeObject(payload)
	if err != nil {
		return payload
	}
	if choices := asSlice(obj["choices"]); len(choices) > 0 {
		outChoices := make([]any, 0, len(choices))
		for i, ch := range choices {
			m := asMap(ch)
			if m == nil {
				continue
			}
			text := ""
			if d := asMap(m["delta"]); d != nil {
				text = flattenChatContent(d["content"])
			}
			outChoices = append(outChoices, map[string]any{
				"text":          text,
				"index":         i,
				"finish_reason": m["finish_reason"],
			})
		}
		obj["choices"] = outChoices
	}
	obj["object"] = "text_completion"
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}
