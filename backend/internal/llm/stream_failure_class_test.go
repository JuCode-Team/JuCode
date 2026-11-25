package llm

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
)

// The codes below are the ones actually observed on the terminal
// `response.failed` frame in production over a 19h window. Before the strategy
// carried the code, every one of them derived to classUnknown, which counted
// user-caused aborts (cyber_policy, context_length_exceeded) as service
// unavailability on the public status page.
func TestDeriveErrorClassResponseFailed(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		want     errorClass
	}{
		{"rate limit", "response_failed:rate_limit_exceeded", classRateLimit},
		{"generic upstream", "response_failed:upstream_error", classProviderFailure},
		{"cyber policy is user content", "response_failed:cyber_policy", classCyberBlock},
		{"overloaded", "response_failed:server_is_overloaded", classProviderFailure},
		{"context too long is user request", "response_failed:context_length_exceeded", classUserRequest},
		{"ws pressure", "response_failed:codex_ws_connection_pressure", classProviderFailure},
		{"connection error", "response_failed:upstream_connection_error", classProviderFailure},
		// No code on the frame, and codes we have never seen: an upstream that
		// aborted a response it already committed to is provider-side.
		{"no code", "response_failed", classProviderFailure},
		{"unrecognized code", "response_failed:something_new_2027", classProviderFailure},
	}
	for _, tc := range cases {
		// status is always the 200 already on the wire — it must not influence
		// the result, which is exactly why the code has to ride the strategy.
		if got := deriveErrorClass(tc.strategy, 200, "upstream response.failed"); got != tc.want {
			t.Errorf("%s: deriveErrorClass(%q) = %q, want %q", tc.name, tc.strategy, got, tc.want)
		}
	}
}

// A response.failed carrying a user-caused code must not cool the channel down:
// the same request fails identically on every provider, so cooling one only
// shrinks the pool. This locks the decision table the gateway now gates on.
func TestResponseFailedCooldownOnlyForChannelFaults(t *testing.T) {
	cases := []struct {
		code         string
		wantCooldown bool
	}{
		{"rate_limit_exceeded", true},
		{"upstream_error", true},
		{"server_is_overloaded", true},
		{"cyber_policy", false},
		{"context_length_exceeded", false},
	}
	for _, tc := range cases {
		class := deriveErrorClass("response_failed:"+tc.code, 200, "")
		if got := class.CountsForCooldown(); got != tc.wantCooldown {
			t.Errorf("code %q → class %q: CountsForCooldown = %v, want %v", tc.code, class, got, tc.wantCooldown)
		}
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write(p []byte) (int, error) { return 0, f.err }

type failingReader struct{ err error }

func (f failingReader) Read(p []byte) (int, error) { return 0, f.err }

// clientGoneError reproduces the production error shape: a write on our own
// listening socket failing with EPIPE/ECONNRESET because the caller hung up.
func clientGoneError(errno syscall.Errno) error {
	return &net.OpError{
		Op:     "write",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 18080},
		Addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40676},
		Err:    &net.OpError{Op: "write", Err: errno},
	}
}

// A failed write to the caller and a failed read from the upstream both surface
// as one pipeErr, but they mean opposite things. Only the write side may be
// treated as a client cancel; misreading an upstream read failure that way
// would silently erase real outages from the public metric.
func TestPipeAndExtractDistinguishesClientWriteFromUpstreamRead(t *testing.T) {
	frame := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"

	for _, errno := range []syscall.Errno{syscall.EPIPE, syscall.ECONNRESET} {
		underlying := clientGoneError(errno)
		err := PipeAndExtractOpts(failingWriter{err: underlying}, strings.NewReader(frame), nil, PipeOptions{})
		if err == nil {
			t.Fatalf("%v: expected an error from a failed client write", errno)
		}
		if !errors.Is(err, errClientWrite) {
			t.Errorf("%v: client write failure not marked as errClientWrite: %v", errno, err)
		}
		// The original cause must survive the wrap so operators keep the errno.
		if !errors.Is(err, underlying) {
			t.Errorf("%v: wrap lost the underlying error: %v", errno, err)
		}
	}

	// Upstream read failure: same package, opposite verdict.
	readErr := errors.New("read tcp: connection reset by peer")
	err := PipeAndExtractOpts(&bytes.Buffer{}, failingReader{err: readErr}, nil, PipeOptions{})
	if !errors.Is(err, readErr) {
		t.Fatalf("expected the upstream read error, got %v", err)
	}
	if errors.Is(err, errClientWrite) {
		t.Error("upstream read failure must NOT be classified as a client write failure")
	}

	// A clean stream still returns nil — the wrap must not fire on the happy
	// path. It has to carry its terminal event, or the pipe would (correctly)
	// report it as cut short and this case would prove nothing about the wrap.
	complete := frame + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"
	var out bytes.Buffer
	if err := PipeAndExtractOpts(&out, strings.NewReader(complete), nil, PipeOptions{}); err != nil {
		t.Fatalf("clean stream returned %v", err)
	}
	if out.String() != complete {
		t.Errorf("clean stream body = %q, want %q", out.String(), complete)
	}
}

// A client write that fails must classify as a cancel, never as a provider
// fault — that is the whole point of the sentinel.
func TestClientWriteFailureClassifiesAsCancel(t *testing.T) {
	if got := deriveErrorClass("client_canceled", 200, "client_canceled: "+clientGoneError(syscall.EPIPE).Error()); got != classClientCanceled {
		t.Errorf("deriveErrorClass = %q, want %q", got, classClientCanceled)
	}
	if classClientCanceled.CountsForCooldown() {
		t.Error("a client cancel must not cool the channel")
	}
	if count, _ := classClientCanceled.HealthOutcome(); count {
		t.Error("a client cancel must not be a provider-health sample")
	}
}

var (
	_ io.Writer = failingWriter{}
	_ io.Reader = failingReader{}
)
