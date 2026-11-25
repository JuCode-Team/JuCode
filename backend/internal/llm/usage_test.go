package llm

import (
	"encoding/json"
	"testing"
)

func TestAccumulatorInspectUsage(t *testing.T) {
	tests := []struct {
		name                               string
		payload                            string
		in, out, cachedIn, cacheCreationIn int
		imageIn, imageOut                  int
		hasUsage                           bool
	}{
		{
			name:    "openai chat plain",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":20}}`,
			in:      100, out: 20, hasUsage: true,
		},
		{
			name:    "openai chat cached subset",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":60}}}`,
			in:      40, out: 20, cachedIn: 60, hasUsage: true,
		},
		{
			name:    "gpt cache write in prompt_tokens_details",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":50}}}`,
			in:      20, out: 20, cachedIn: 30, cacheCreationIn: 50, hasUsage: true,
		},
		{
			name:    "gpt cache write as usage sibling",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":20,"cache_write_tokens":70}}`,
			in:      30, out: 20, cacheCreationIn: 70, hasUsage: true,
		},
		{
			name:    "gpt cache write in input_tokens_details (responses flat)",
			payload: `{"usage":{"input_tokens":200,"output_tokens":10,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":50}}}`,
			in:      50, out: 10, cachedIn: 100, cacheCreationIn: 50, hasUsage: true,
		},
		{
			name:    "responses end frame with cache write",
			payload: `{"response":{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}}}`,
			in:      50, out: 5, cachedIn: 20, cacheCreationIn: 30, hasUsage: true,
		},
		{
			name:    "cache write exceeding total clamps to zero non-cached",
			payload: `{"usage":{"prompt_tokens":40,"completion_tokens":1,"cache_write_tokens":70}}`,
			in:      0, out: 1, cacheCreationIn: 70, hasUsage: true,
		},
		{
			name:    "anthropic separate cache fields unchanged",
			payload: `{"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":500,"cache_creation_input_tokens":200}}`,
			in:      10, out: 3, cachedIn: 500, cacheCreationIn: 200, hasUsage: true,
		},
		{
			name:    "responses image generation tool usage",
			payload: `{"usage":{"input_tokens":100,"output_tokens":5},"tool_usage":{"image_gen":{"input_tokens":40,"output_tokens":229}}}`,
			in:      100, out: 5, imageIn: 40, imageOut: 229, hasUsage: true,
		},
		{
			name:    "responses completed event nested image generation tool usage",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":5},"tool_usage":{"image_gen":{"input_tokens":40,"output_tokens":229}}}}`,
			in:      100, out: 5, imageIn: 40, imageOut: 229, hasUsage: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a Accumulator
			a.Inspect([]byte(tt.payload))
			if a.HasUsage != tt.hasUsage {
				t.Fatalf("HasUsage = %v, want %v", a.HasUsage, tt.hasUsage)
			}
			if a.TokensIn != tt.in || a.TokensOut != tt.out ||
				a.CachedTokensIn != tt.cachedIn || a.CacheCreationTokensIn != tt.cacheCreationIn ||
				a.ImageTokensIn != tt.imageIn || a.ImageTokensOut != tt.imageOut {
				t.Fatalf("got in=%d out=%d cached=%d cacheWrite=%d imageIn=%d imageOut=%d, want in=%d out=%d cached=%d cacheWrite=%d imageIn=%d imageOut=%d",
					a.TokensIn, a.TokensOut, a.CachedTokensIn, a.CacheCreationTokensIn, a.ImageTokensIn, a.ImageTokensOut,
					tt.in, tt.out, tt.cachedIn, tt.cacheCreationIn, tt.imageIn, tt.imageOut)
			}
		})
	}
}

// TestAccumulatorAnthropicStreamingUsage feeds the multi-frame Anthropic
// streaming sequence. Input tokens arrive on message_start (under
// .message.usage); output arrives on message_delta (flat .usage). A
// trailing output-only message_delta must NOT zero the input — this is
// the regression that caused streamed requests to bill on output only.
func TestAccumulatorAnthropicStreamingUsage(t *testing.T) {
	tests := []struct {
		name                               string
		frames                             []string
		in, out, cachedIn, cacheCreationIn int
	}{
		{
			name: "message_start input + bare message_delta output (the bug)",
			frames: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":95000,"cache_creation_input_tokens":300,"output_tokens":1}}}`,
				`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
				`{"type":"message_delta","usage":{"output_tokens":431}}`,
			},
			in: 1200, out: 431, cachedIn: 95000, cacheCreationIn: 300,
		},
		{
			name: "message_start input + full message_delta (upstream repeats input)",
			frames: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":95000,"cache_creation_input_tokens":300,"output_tokens":1}}}`,
				`{"type":"message_delta","usage":{"input_tokens":1200,"cache_read_input_tokens":95000,"cache_creation_input_tokens":300,"output_tokens":431}}`,
			},
			in: 1200, out: 431, cachedIn: 95000, cacheCreationIn: 300,
		},
		{
			name: "no-cache request: input on message_start, bare delta output",
			frames: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":800,"output_tokens":1}}}`,
				`{"type":"message_delta","usage":{"output_tokens":50}}`,
			},
			in: 800, out: 50,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a Accumulator
			for _, f := range tt.frames {
				a.Inspect([]byte(f))
			}
			if !a.HasUsage {
				t.Fatalf("HasUsage = false, want true")
			}
			if a.TokensIn != tt.in || a.TokensOut != tt.out ||
				a.CachedTokensIn != tt.cachedIn || a.CacheCreationTokensIn != tt.cacheCreationIn {
				t.Fatalf("got in=%d out=%d cached=%d cacheWrite=%d, want in=%d out=%d cached=%d cacheWrite=%d",
					a.TokensIn, a.TokensOut, a.CachedTokensIn, a.CacheCreationTokensIn,
					tt.in, tt.out, tt.cachedIn, tt.cacheCreationIn)
			}
		})
	}
}

// Anthropic bills cache writes at two TTLs. The nested cache_creation
// object carries the split; cache_creation_input_tokens is the aggregate.
// Streaming may put the split on message_start and a later delta must not
// zero a field it omitted or reported as 0.
func TestAccumulatorAnthropicCacheCreationTTL(t *testing.T) {
	tests := []struct {
		name                   string
		frames                 []string
		in, out, cached, total int
		fiveM, oneH            int
	}{
		{
			name: "flat usage with 5m and 1h breakdown",
			frames: []string{
				`{"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":500,"cache_creation_input_tokens":123,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":23}}}`,
			},
			in: 10, out: 3, cached: 500, total: 123, fiveM: 100, oneH: 23,
		},
		{
			name: "nested only — aggregate is 0, lift total from 5m+1h",
			frames: []string{
				`{"usage":{"input_tokens":21,"output_tokens":34,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":8}}}`,
			},
			in: 21, out: 34, total: 13, fiveM: 5, oneH: 8,
		},
		{
			name: "aggregate only — no nested object, leave 5m/1h unset",
			frames: []string{
				`{"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":500,"cache_creation_input_tokens":200}}`,
			},
			in: 10, out: 3, cached: 500, total: 200, fiveM: 0, oneH: 0,
		},
		{
			name: "message_start nested under message.usage",
			frames: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}}}}`,
			},
			in: 12, cached: 9, total: 7, fiveM: 3, oneH: 4,
		},
		{
			name: "message_delta zero must not overwrite message_start 1h",
			frames: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":8,"cache_read_input_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":6}}}}`,
				`{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":5,"cache_creation_input_tokens":8,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":0}}}`,
			},
			in: 12, out: 5, cached: 9, total: 8, fiveM: 2, oneH: 6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a Accumulator
			for _, f := range tt.frames {
				a.Inspect([]byte(f))
			}
			if a.TokensIn != tt.in || a.TokensOut != tt.out ||
				a.CachedTokensIn != tt.cached || a.CacheCreationTokensIn != tt.total ||
				a.CacheCreation5mTokensIn != tt.fiveM || a.CacheCreation1hTokensIn != tt.oneH {
				t.Fatalf("got in=%d out=%d cached=%d total=%d 5m=%d 1h=%d, want in=%d out=%d cached=%d total=%d 5m=%d 1h=%d",
					a.TokensIn, a.TokensOut, a.CachedTokensIn, a.CacheCreationTokensIn, a.CacheCreation5mTokensIn, a.CacheCreation1hTokensIn,
					tt.in, tt.out, tt.cached, tt.total, tt.fiveM, tt.oneH)
			}
		})
	}
}

func TestAccumulatorImageToolUsageDedup(t *testing.T) {
	var a Accumulator
	payload := []byte(`{"usage":{"input_tokens":100,"output_tokens":5},"tool_usage":{"image_gen":{"input_tokens":40,"output_tokens":229}}}`)
	a.Inspect(payload)
	a.Inspect(payload)
	if a.TokensIn != 100 || a.TokensOut != 5 || a.ImageTokensIn != 40 || a.ImageTokensOut != 229 {
		t.Fatalf("got in=%d out=%d imageIn=%d imageOut=%d, want in=100 out=5 imageIn=40 imageOut=229",
			a.TokensIn, a.TokensOut, a.ImageTokensIn, a.ImageTokensOut)
	}
}

// The audio counts are a SUBSET of the reported totals, so they have to come
// out of TokensIn/TokensOut. Billing adds the two back together at different
// unit prices; leaving audio inside the text counts would charge it twice.
func TestAccumulatorAudioTokensSplitFromText(t *testing.T) {
	tests := []struct {
		name                               string
		payload                            string
		in, out, cached, audioIn, audioOut int
	}{
		{
			name:    "chat audio preview",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":60,"prompt_tokens_details":{"cached_tokens":10,"audio_tokens":70},"completion_tokens_details":{"audio_tokens":50}}}`,
			in:      20, out: 10, cached: 10, audioIn: 70, audioOut: 50,
		},
		{
			// Realtime spells the detail objects singular and nests usage
			// under .response — the shape realtimeMeter feeds in.
			name:    "realtime response.done",
			payload: `{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":200,"output_tokens":150,"input_token_details":{"cached_tokens":40,"text_tokens":20,"audio_tokens":140},"output_token_details":{"text_tokens":10,"audio_tokens":140}}}}`,
			in:      20, out: 10, cached: 40, audioIn: 140, audioOut: 140,
		},
		{
			name:    "responses plural details",
			payload: `{"response":{"usage":{"input_tokens":90,"output_tokens":40,"input_tokens_details":{"audio_tokens":80},"output_tokens_details":{"audio_tokens":30}}}}`,
			in:      10, out: 10, cached: 0, audioIn: 80, audioOut: 30,
		},
		{
			// No audio at all: the split must be a no-op for text-only traffic.
			name:    "text only unaffected",
			payload: `{"usage":{"prompt_tokens":100,"completion_tokens":60,"prompt_tokens_details":{"cached_tokens":10}}}`,
			in:      90, out: 60, cached: 10, audioIn: 0, audioOut: 0,
		},
		{
			// Defensive: an audio count larger than its own total is clamped
			// to that total, so the text side cannot go negative and the sum
			// of the parts cannot exceed what was reported.
			name:    "audio exceeding total clamps",
			payload: `{"usage":{"prompt_tokens":50,"completion_tokens":20,"prompt_tokens_details":{"audio_tokens":80},"completion_tokens_details":{"audio_tokens":90}}}`,
			in:      0, out: 0, cached: 0, audioIn: 50, audioOut: 20,
		},
		{
			// The case that matters most: cached_tokens is NOT disjoint from
			// audio_tokens, so the cached audio has to stay out of the audio
			// charge. 200 total = 20 text + 180 audio, 60 of it cached (50
			// audio + 10 text), leaves 130 uncached audio and 10 uncached text.
			name: "cached audio is not charged the audio rate",
			payload: `{"type":"response.done","response":{"usage":{"input_tokens":200,"output_tokens":40,` +
				`"input_token_details":{"cached_tokens":60,"text_tokens":20,"audio_tokens":180,` +
				`"cached_tokens_details":{"text_tokens":10,"audio_tokens":50}},` +
				`"output_token_details":{"text_tokens":10,"audio_tokens":30}}}}`,
			in: 10, out: 10, cached: 60, audioIn: 130, audioOut: 30,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a Accumulator
			a.Inspect([]byte(tt.payload))
			if a.TokensIn != tt.in || a.TokensOut != tt.out ||
				a.CachedTokensIn != tt.cached ||
				a.AudioTokensIn != tt.audioIn || a.AudioTokensOut != tt.audioOut {
				t.Fatalf("got in=%d out=%d cached=%d audioIn=%d audioOut=%d, want in=%d out=%d cached=%d audioIn=%d audioOut=%d",
					a.TokensIn, a.TokensOut, a.CachedTokensIn, a.AudioTokensIn, a.AudioTokensOut,
					tt.in, tt.out, tt.cached, tt.audioIn, tt.audioOut)
			}
		})
	}
}

// The split exists so billing can price each modality once. Whatever the
// breakdown, the parts have to add back up to the input total the upstream
// reported — anything else is an over- or under-charge.
func TestAccumulatorAudioSplitSumsToReportedTotal(t *testing.T) {
	payloads := []string{
		// Multi-turn Realtime: a large audio cache hit, the case where
		// double-subtracting the overlap inflated the charge the most.
		`{"response":{"usage":{"input_tokens":4000,"output_tokens":100,` +
			`"input_token_details":{"text_tokens":200,"audio_tokens":3800,"cached_tokens":3456,` +
			`"cached_tokens_details":{"text_tokens":128,"audio_tokens":3328}},` +
			`"output_token_details":{"text_tokens":20,"audio_tokens":80}}}}`,
		// Same session shape, but the upstream omits cached_tokens_details.
		// Without it the cached audio cannot be identified, so the audio share
		// is capped at the remainder instead of overflowing past the total.
		`{"response":{"usage":{"input_tokens":4000,"output_tokens":100,` +
			`"input_token_details":{"audio_tokens":3800,"cached_tokens":3456},` +
			`"output_token_details":{"audio_tokens":80}}}}`,
		// Cache write instead of cache read.
		`{"usage":{"prompt_tokens":900,"completion_tokens":50,` +
			`"prompt_tokens_details":{"cache_write_tokens":300,"audio_tokens":400},` +
			`"completion_tokens_details":{"audio_tokens":20}}}`,
		// No cache at all.
		`{"usage":{"prompt_tokens":1000,"completion_tokens":80,` +
			`"prompt_tokens_details":{"audio_tokens":900},"completion_tokens_details":{"audio_tokens":70}}}`,
	}
	for _, p := range payloads {
		var a Accumulator
		a.Inspect([]byte(p))
		gotIn := a.TokensIn + a.CachedTokensIn + a.CacheCreationTokensIn + a.AudioTokensIn
		gotOut := a.TokensOut + a.AudioTokensOut
		var want struct {
			Usage    map[string]any `json:"usage"`
			Response struct {
				Usage map[string]any `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(p), &want); err != nil {
			t.Fatal(err)
		}
		u := want.Usage
		if u == nil {
			u = want.Response.Usage
		}
		wantIn := int(firstNonZeroFloat(u["prompt_tokens"], u["input_tokens"]))
		wantOut := int(firstNonZeroFloat(u["completion_tokens"], u["output_tokens"]))
		if gotIn != wantIn || gotOut != wantOut {
			t.Errorf("payload %s\n  input parts sum to %d (want %d), output parts sum to %d (want %d)",
				p, gotIn, wantIn, gotOut, wantOut)
		}
	}
}

func firstNonZeroFloat(vals ...any) float64 {
	for _, v := range vals {
		if f, ok := v.(float64); ok && f != 0 {
			return f
		}
	}
	return 0
}

// Every usage field merges as max across frames, which is what makes the
// audio split order-independent: whether the total or the breakdown arrives
// first, the text remainder has to end up the same.
func TestAccumulatorAudioSplitIsOrderIndependent(t *testing.T) {
	totalOnly := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":60}}`)
	withAudio := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":60,"prompt_tokens_details":{"audio_tokens":70},"completion_tokens_details":{"audio_tokens":50}}}`)

	orders := map[string][][]byte{
		"total then breakdown": {totalOnly, withAudio},
		"breakdown then total": {withAudio, totalOnly},
		"breakdown repeated":   {withAudio, withAudio},
	}
	for name, frames := range orders {
		t.Run(name, func(t *testing.T) {
			var a Accumulator
			for _, f := range frames {
				a.Inspect(f)
			}
			if a.TokensIn != 30 || a.TokensOut != 10 || a.AudioTokensIn != 70 || a.AudioTokensOut != 50 {
				t.Fatalf("got in=%d out=%d audioIn=%d audioOut=%d, want in=30 out=10 audioIn=70 audioOut=50",
					a.TokensIn, a.TokensOut, a.AudioTokensIn, a.AudioTokensOut)
			}
		})
	}
}
