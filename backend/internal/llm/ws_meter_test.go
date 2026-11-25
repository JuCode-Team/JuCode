package llm

import (
	"errors"
	"testing"

	"github.com/jucode/jucode/internal/biz"
)

func TestWSProxyDeniedDoesNotBridge(t *testing.T) {
	for _, err := range []error{
		biz.ErrGroupDeniedForUser,
		biz.ErrGroupDeniedForApiKey,
		biz.ErrRPMLimit,
		biz.ErrTPMLimit,
		biz.ErrConcurrencyLimit,
	} {
		if !wsProxyDenied(err) {
			t.Errorf("%v must close the socket, not fall through to the bridge", err)
		}
	}
	if wsProxyDenied(errors.New("websocket: bad handshake")) {
		t.Error("a failed upstream dial must still fall through to the bridge")
	}
	if wsProxyDenied(nil) {
		t.Error("nil is success, not a deny")
	}
}

// A proxied socket is billed off the turn-terminal event, and the two dialects
// spell it differently: Realtime sends response.done, the Responses socket
// sends response.completed. Recognising only one of them silently serves every
// session on the other for free.
func TestWSResponseSettledAcrossDialects(t *testing.T) {
	cases := []struct {
		name      string
		frame     string
		want      bool
		wantID    string
		wantIn    int
		wantOut   int
		wantAudio int
	}{
		{
			name:   "realtime response.done",
			frame:  `{"type":"response.done","response":{"id":"resp_rt","usage":{"input_tokens":30,"output_tokens":12}}}`,
			want:   true,
			wantID: "resp_rt",
			wantIn: 30, wantOut: 12,
		},
		{
			name:   "responses response.completed",
			frame:  `{"type":"response.completed","response":{"id":"resp_http","usage":{"input_tokens":41,"output_tokens":7}}}`,
			want:   true,
			wantID: "resp_http",
			wantIn: 41, wantOut: 7,
		},
		{
			name: "audio tokens split off the text price",
			frame: `{"type":"response.done","response":{"id":"resp_voice","usage":{"input_tokens":100,"output_tokens":50,` +
				`"input_token_details":{"audio_tokens":80},"output_token_details":{"audio_tokens":40}}}}`,
			want:   true,
			wantID: "resp_voice",
			wantIn: 20, wantOut: 10, wantAudio: 80,
		},
		{
			name:  "response.created only opens the turn",
			frame: `{"type":"response.created","response":{"id":"resp_rt"}}`,
			want:  false,
		},
		{
			name:  "audio delta is not terminal",
			frame: `{"type":"response.audio.delta","delta":"AAAA"}`,
			want:  false,
		},
		{
			name:  "terminal without usage settles nothing",
			frame: `{"type":"response.completed","response":{"id":"resp_http"}}`,
			want:  false,
		},
		{
			name:  "the word alone is not the event",
			frame: `{"type":"response.output_text.delta","delta":"see response.completed for usage"}`,
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc, id, ok := wsResponseSettled([]byte(tc.frame))
			if ok != tc.want {
				t.Fatalf("settled = %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			if id != tc.wantID {
				t.Errorf("response id = %q, want %q", id, tc.wantID)
			}
			if acc.TokensIn != tc.wantIn {
				t.Errorf("TokensIn = %d, want %d", acc.TokensIn, tc.wantIn)
			}
			if acc.TokensOut != tc.wantOut {
				t.Errorf("TokensOut = %d, want %d", acc.TokensOut, tc.wantOut)
			}
			if acc.AudioTokensIn != tc.wantAudio {
				t.Errorf("AudioTokensIn = %d, want %d", acc.AudioTokensIn, tc.wantAudio)
			}
		})
	}
}

func TestWSResponseTerminalWithoutUsageIsStillTerminal(t *testing.T) {
	for _, frame := range []string{
		`{"type":"response.done","response":{"id":"rt-no-usage"}}`,
		`{"type":"response.completed","response":{"id":"responses-no-usage"}}`,
		`{"type":"response.failed","response":{"id":"failed"}}`,
		`{"type":"response.incomplete","response":{"id":"incomplete"}}`,
	} {
		id, terminal := wsResponseTerminal([]byte(frame))
		if !terminal || id == "" {
			t.Errorf("frame was not recognized as terminal: %s", frame)
		}
		if _, _, settled := wsResponseSettled([]byte(frame)); settled {
			t.Errorf("terminal without usage was billed as settled: %s", frame)
		}
	}
}
