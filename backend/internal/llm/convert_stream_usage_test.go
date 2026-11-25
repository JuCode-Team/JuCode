package llm

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func inspectConvertedSSE(raw string) Accumulator {
	var acc Accumulator
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		acc.Inspect([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
	}
	return acc
}

func TestConvertStreamAnthropicSplitUsageReachesInspect(t *testing.T) {
	inputTokens := 17
	outputTokens := 4
	cacheRead := 6
	src := fmt.Sprintf(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_split\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":0,\"cache_read_input_tokens\":%d}}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		inputTokens, cacheRead, outputTokens,
	)

	for _, to := range []Dialect{DialectCompatible, DialectOpenAI} {
		t.Run(string(to), func(t *testing.T) {
			var dst bytes.Buffer
			if err := ConvertStream(DialectAnthropic, to, strings.NewReader(src), &dst); err != nil {
				t.Fatal(err)
			}
			got := dst.String()
			// Both OpenAI-family targets report the GROSS input (non-cached +
			// cache read) with the cache broken out under *_tokens_details,
			// which is that family's convention — Anthropic's net input_tokens
			// under an OpenAI field name would be read as gross and lose the
			// cached portion. Inspect subtracts the detail back out to reach
			// TokensIn=N either way.
			if to == DialectCompatible {
				if !strings.Contains(got, fmt.Sprintf(`"prompt_tokens":%d`, inputTokens+cacheRead)) {
					t.Fatalf("chat stream missing gross prompt_tokens=%d:\n%s", inputTokens+cacheRead, got)
				}
				if !strings.Contains(got, fmt.Sprintf(`"cached_tokens":%d`, cacheRead)) {
					t.Fatalf("chat stream missing cached_tokens=%d:\n%s", cacheRead, got)
				}
			} else if !strings.Contains(got, fmt.Sprintf(`"cached_tokens":%d`, cacheRead)) {
				t.Fatalf("responses stream missing cached_tokens=%d:\n%s", cacheRead, got)
			}
			acc := inspectConvertedSSE(got)
			if acc.TokensIn != inputTokens {
				t.Fatalf("TokensIn=%d want %d (output-only message_delta must not wipe message_start input)\n%s",
					acc.TokensIn, inputTokens, got)
			}
			if acc.TokensOut != outputTokens {
				t.Fatalf("TokensOut=%d want %d\n%s", acc.TokensOut, outputTokens, got)
			}
			if acc.CachedTokensIn != cacheRead {
				t.Fatalf("CachedTokensIn=%d want %d\n%s", acc.CachedTokensIn, cacheRead, got)
			}
		})
	}
}

// The two dialect families disagree about whether the cache is inside the
// input total: Anthropic's input_tokens is net of it, OpenAI's prompt_tokens
// contains it. A converter that copies one convention's number under the
// other's field name bills the cached prompt twice — once at the input rate
// inside the total, once at the cache-read rate — and quotes the client an
// input count it never used. This pins every reachable pairing to the total
// the source actually reported.
func TestConvertStreamCacheAccountingSurvivesEveryDirection(t *testing.T) {
	const (
		grossIn = 1000 // total input the upstream charged for
		cache   = 800  // the cache-hit part of it
		netIn   = grossIn - cache
		outTok  = 50
	)
	// Each source states the same fact in its own convention.
	chatSrc := sse(
		`{"id":"c","model":"m","choices":[{"delta":{"content":"hi"}}]}`,
		`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		fmt.Sprintf(`{"id":"c","model":"m","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`, grossIn, outTok, cache),
		`[DONE]`,
	)
	responsesSrc := sse(
		`{"type":"response.created","response":{"id":"r","model":"m"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		fmt.Sprintf(`{"type":"response.completed","response":{"id":"r","model":"m","usage":{"input_tokens":%d,"output_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}}}`, grossIn, outTok, cache),
	)
	anthropicSrc := sse(
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg","model":"m","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":0}}}`, netIn, cache),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, outTok),
		`{"type":"message_stop"}`,
	)

	for _, tc := range []struct {
		from, to Dialect
		src      string
	}{
		{DialectCompatible, DialectOpenAI, chatSrc},
		{DialectCompatible, DialectAnthropic, chatSrc},
		{DialectOpenAI, DialectCompatible, responsesSrc},
		{DialectOpenAI, DialectAnthropic, responsesSrc},
		{DialectAnthropic, DialectCompatible, anthropicSrc},
		{DialectAnthropic, DialectOpenAI, anthropicSrc},
	} {
		t.Run(string(tc.from)+"_to_"+string(tc.to), func(t *testing.T) {
			got := convertStreamString(t, tc.from, tc.to, tc.src)
			acc := inspectConvertedSSE(got)
			if !acc.HasUsage {
				t.Fatalf("converted stream carries no usage\n%s", got)
			}
			if acc.CachedTokensIn != cache {
				t.Errorf("CachedTokensIn=%d want %d\n%s", acc.CachedTokensIn, cache, got)
			}
			if acc.TokensIn != netIn {
				t.Errorf("TokensIn=%d want %d (non-cached remainder)\n%s", acc.TokensIn, netIn, got)
			}
			if total := acc.TokensIn + acc.CachedTokensIn + acc.CacheCreationTokensIn; total != grossIn {
				t.Errorf("input total=%d want %d\n%s", total, grossIn, got)
			}
			if acc.TokensOut != outTok {
				t.Errorf("TokensOut=%d want %d\n%s", acc.TokensOut, outTok, got)
			}
		})
	}
}

// Audio lives inside the parent totals and is only spelled out under
// *_tokens_details. A converter that copies the totals but drops the
// details bills those tokens at the text rate — several times cheaper
// than the audio price on a Realtime / audio-preview model.
func TestConvertStreamAudioSurvivesOpenAIFamily(t *testing.T) {
	const (
		grossIn  = 100
		audioIn  = 70
		textIn   = grossIn - audioIn
		grossOut = 60
		audioOut = 50
		textOut  = grossOut - audioOut
	)
	chatSrc := sse(
		`{"id":"c","model":"m","choices":[{"delta":{"content":"hi"}}]}`,
		`{"id":"c","model":"m","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		fmt.Sprintf(`{"id":"c","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"prompt_tokens_details":{"audio_tokens":%d},"completion_tokens_details":{"audio_tokens":%d}}}`,
			grossIn, grossOut, audioIn, audioOut),
		`[DONE]`,
	)
	responsesSrc := sse(
		`{"type":"response.created","response":{"id":"r","model":"m"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		fmt.Sprintf(`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":%d,"output_tokens":%d,"input_tokens_details":{"audio_tokens":%d},"output_tokens_details":{"audio_tokens":%d}}}}`,
			grossIn, grossOut, audioIn, audioOut),
	)
	for _, tc := range []struct {
		from, to Dialect
		src      string
	}{
		{DialectCompatible, DialectOpenAI, chatSrc},
		{DialectOpenAI, DialectCompatible, responsesSrc},
		{DialectCompatible, DialectAnthropic, chatSrc},
		{DialectOpenAI, DialectAnthropic, responsesSrc},
	} {
		t.Run(string(tc.from)+"_to_"+string(tc.to), func(t *testing.T) {
			got := convertStreamString(t, tc.from, tc.to, tc.src)
			acc := inspectConvertedSSE(got)
			if acc.AudioTokensIn != audioIn || acc.AudioTokensOut != audioOut {
				t.Errorf("audio = %d/%d, want %d/%d\n%s", acc.AudioTokensIn, acc.AudioTokensOut, audioIn, audioOut, got)
			}
			if acc.TokensIn != textIn || acc.TokensOut != textOut {
				t.Errorf("text = %d/%d, want %d/%d\n%s", acc.TokensIn, acc.TokensOut, textIn, textOut, got)
			}
		})
	}
}
