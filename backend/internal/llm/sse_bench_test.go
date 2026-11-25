package llm

import "testing"

// isStreamTerminalPayload runs on every frame of every relayed stream, so the
// cost of answering "did this end the answer" is paid thousands of times per
// long reply. It is benchmarked because the first version decoded each frame
// into map[string]any, which a review measured at roughly 1.5x the time and 4x
// the allocations of the typed decode now in use.
func BenchmarkIsStreamTerminalPayload(b *testing.B) {
	cases := map[string][]byte{
		// The overwhelmingly common shape: a content delta, not terminal.
		"chat delta": []byte(`{"id":"chatcmpl-9x","object":"chat.completion.chunk","created":1730000000,"model":"gpt-4o-2024-08-06","system_fingerprint":"fp_abc123","choices":[{"index":0,"delta":{"content":"the quick brown fox"},"logprobs":null,"finish_reason":null}]}`),
		"chat final": []byte(`{"id":"chatcmpl-9x","object":"chat.completion.chunk","created":1730000000,"model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		"done":       []byte(`[DONE]`),
		"responses":  []byte(`{"type":"response.output_text.delta","sequence_number":42,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"the quick brown fox"}`),
		"anthropic":  []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the quick brown fox"}}`),
	}
	for name, payload := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				isStreamTerminalPayload(payload)
			}
		})
	}
}
