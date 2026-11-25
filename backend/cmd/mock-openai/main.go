// Command mock-openai is a development fixture: a tiny HTTP server that
// speaks an OpenAI Chat Completions-compatible response so we can e2e
// test the JuCode gateway without burning real API credits.
//
// Listens on :8081 by default. Accepts any Authorization (no validation).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	var addr string
	flag.StringVar(&addr, "addr", ":8081", "listen address")
	flag.Parse()

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		stream, _ := body["stream"].(bool)
		model, _ := body["model"].(string)
		if model == "" {
			model = "mock-model"
		}

		if !stream {
			resp := map[string]any{
				"id":      "mock-cmpl-1",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "hello from mock",
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens":     13,
					"completion_tokens": 7,
					"total_tokens":      20,
					"prompt_tokens_details": map[string]any{
						"cached_tokens": 5,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// SSE stream
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)

		writeFrame := func(obj map[string]any) {
			b, _ := json.Marshal(obj)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}

		writeFrame(map[string]any{
			"id":      "mock-cmpl-1",
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}},
		})
		writeFrame(map[string]any{
			"id":      "mock-cmpl-1",
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "hello "}}},
		})
		writeFrame(map[string]any{
			"id":      "mock-cmpl-1",
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "from mock"}}},
		})
		// Final chunk with usage (only present when stream_options.include_usage=true,
		// but mock always sends it for test simplicity).
		writeFrame(map[string]any{
			"id":      "mock-cmpl-1",
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{
				"prompt_tokens":     13,
				"completion_tokens": 7,
				"total_tokens":      20,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 5,
				},
			},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	log.Printf("mock-openai listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
