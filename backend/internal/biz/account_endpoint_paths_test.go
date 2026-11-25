package biz

import (
	"errors"
	"strings"
	"testing"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestNormalizeEndpointPaths(t *testing.T) {
	cases := []struct {
		name    string
		in      map[string]string
		want    map[string]any
		wantErr string
	}{
		{
			name: "empty stays empty",
			in:   nil,
			want: nil,
		},
		{
			name: "canonical key is preserved",
			in:   map[string]string{"chat_completions": "/v2/chat/completions"},
			want: map[string]any{"chat_completions": "/v2/chat/completions"},
		},
		{
			name: "aliases normalize to the canonical protocol",
			in:   map[string]string{"chat": "/v2/chat", "response": "/v2/responses", "anthropic": "/v2/messages"},
			want: map[string]any{"chat_completions": "/v2/chat", "responses": "/v2/responses", "messages": "/v2/messages"},
		},
		{
			name: "a blank value clears that protocol and nothing else",
			in:   map[string]string{"chat_completions": "  ", "messages": "/v3/messages"},
			want: map[string]any{"messages": "/v3/messages"},
		},
		{
			name:    "unknown protocol key",
			in:      map[string]string{"embedding": "/v1/embed"},
			wantErr: "is not a known protocol",
		},
		{
			name:    "path must be rooted",
			in:      map[string]string{"chat_completions": "v2/chat"},
			wantErr: "must start with /",
		},
		{
			name:    "protocol relative path rejected",
			in:      map[string]string{"chat_completions": "//evil.example.com/x"},
			wantErr: "must not start with //",
		},
		{
			name:    "whitespace rejected",
			in:      map[string]string{"chat_completions": "/v2/chat completions"},
			wantErr: "must not contain whitespace",
		},
		{
			name:    "newline injection rejected",
			in:      map[string]string{"chat_completions": "/v2/chat\r\nHost: evil"},
			wantErr: "must not contain whitespace",
		},
		{
			name:    "absurdly long path rejected",
			in:      map[string]string{"chat_completions": "/" + strings.Repeat("a", maxEndpointPathLength+1)},
			wantErr: "longer than",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeEndpointPaths(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, got)
				}
				if !errors.Is(err, ErrAccountBadInput) {
					t.Fatalf("error should wrap ErrAccountBadInput: %v", err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q should contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("key %q: got %#v want %#v", key, got[key], want)
				}
			}
		})
	}
}

func TestEndpointPathFor(t *testing.T) {
	acct := &ent.UpstreamAccount{
		EndpointPaths: map[string]any{
			"chat_completions": "/v2/chat/completions",
			"anthropic":        "/v2/messages", // alias key written by hand
			"broken":           "/v1/broken",   // unknown protocol, ignored
			"responses":        42,             // non-string value, ignored
		},
	}

	if got := EndpointPathFor(acct, NativeProtocolChat); got != "/v2/chat/completions" {
		t.Fatalf("chat override: %q", got)
	}
	if got := EndpointPathFor(acct, "chat"); got != "/v2/chat/completions" {
		t.Fatalf("alias lookup should canonicalize: %q", got)
	}
	if got := EndpointPathFor(acct, NativeProtocolMessages); got != "/v2/messages" {
		t.Fatalf("alias key should resolve: %q", got)
	}
	if got := EndpointPathFor(acct, NativeProtocolResponses); got != "" {
		t.Fatalf("non-string values must be ignored, got %q", got)
	}
	if got := EndpointPathFor(acct, "embedding"); got != "" {
		t.Fatalf("unknown protocol: %q", got)
	}

	// The common case: no declaration at all.
	if got := EndpointPathFor(&ent.UpstreamAccount{}, NativeProtocolChat); got != "" {
		t.Fatalf("empty account must have no override, got %q", got)
	}
	if got := EndpointPathFor(nil, NativeProtocolChat); got != "" {
		t.Fatalf("nil account must not panic: %q", got)
	}
}

func TestEndpointPathsOfIsLenient(t *testing.T) {
	if got := EndpointPathsOf(nil); got != nil {
		t.Fatalf("nil account: %#v", got)
	}
	if got := EndpointPathsOf(&ent.UpstreamAccount{EndpointPaths: map[string]any{}}); got != nil {
		t.Fatalf("empty map: %#v", got)
	}

	acct := &ent.UpstreamAccount{EndpointPaths: map[string]any{
		"chat_completions": "/v2/chat/completions",
		"something":        "/v1/x",
		"responses":        nil,
	}}
	got := EndpointPathsOf(acct)
	if len(got) != 1 || got[NativeProtocolChat] != "/v2/chat/completions" {
		t.Fatalf("lenient decode: %#v", got)
	}
}
