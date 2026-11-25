package llm

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The Anthropic bridge streams upstream for every client, including the
// stream:false ones it serves by buffering. If the forwarded body omits
// stream_options.include_usage the terminal chunk carries no usage block,
// tokens_out lands at 0 and the request bills nothing — a revenue invariant,
// not a formatting detail. It regressed once because the two body-building
// paths were each responsible for satisfying it and one of them stopped.
func TestBridgeAlwaysAsksUpstreamForUsage(t *testing.T) {
	t.Parallel()

	for _, clientStream := range []bool{false, true} {
		raw := []byte(`{"model":"claude-3","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
		if clientStream {
			raw = []byte(`{"model":"claude-3","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		}
		converted, err := ConvertRequest(DialectAnthropic, DialectCompatible, raw)
		if err != nil {
			t.Fatalf("client stream=%v: convert: %v", clientStream, err)
		}
		got, err := forceChatStreamWithUsage(converted)
		if err != nil {
			t.Fatalf("client stream=%v: patch: %v", clientStream, err)
		}
		assertStreamsWithUsage(t, got, fmt.Sprintf("conversion path, client stream=%v", clientStream))
	}

	// The non-conversion path is fed through the same call, so it has to come
	// out the same way even though anthropicToChatBody already set both.
	body, err := anthropicToChatBody(&anthropicRequest{
		Model:     "claude-3",
		MaxTokens: 16,
		Messages:  []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}, true /*upstreamStream*/)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := forceChatStreamWithUsage(body)
	if err != nil {
		t.Fatal(err)
	}
	assertStreamsWithUsage(t, patched, "non-conversion path")
}

// Bodies that carry no JSON object come back untouched instead of panicking.
// `null` is the one that bites: it unmarshals cleanly into a nil map, which
// then panics on the first write.
func TestForceChatStreamWithUsageSurvivesBodiesWithoutAnObject(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`null`, `["not","an","object"]`, `not json at all`} {
		got, err := forceChatStreamWithUsage([]byte(raw))
		if err != nil {
			t.Errorf("%s: err = %v, want nil", raw, err)
		}
		if string(got) != raw {
			t.Errorf("%s: body = %s, want it unchanged", raw, got)
		}
	}
}

func assertStreamsWithUsage(t *testing.T, body []byte, label string) {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: body is not an object: %s", label, body)
	}
	if out["stream"] != true {
		t.Errorf("%s: stream = %v, want true", label, out["stream"])
	}
	opts, _ := out["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Errorf("%s: stream_options = %v, want include_usage true — without it the request bills as zero", label, out["stream_options"])
	}
}
