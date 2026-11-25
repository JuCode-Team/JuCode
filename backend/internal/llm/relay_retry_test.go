package llm

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

func TestRelayRestartDelay(t *testing.T) {
	local := biz.RouteAccount{Account: &ent.UpstreamAccount{BaseURL: "http://127.0.0.1:18200"}}
	remote := biz.RouteAccount{Account: &ent.UpstreamAccount{BaseURL: "https://api.example.com/v1"}}
	reset := &url.Error{Op: "Post", URL: "http://127.0.0.1:18200/v1/responses",
		Err: &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.ECONNRESET)}}
	closed := &url.Error{Op: "Post", Err: &net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed}}

	if d, ok := relayRestartDelay(local, reset, 0); !ok || d.Seconds() != 1 {
		t.Fatalf("first retry = %v %v, want 1s", d, ok)
	}
	if _, ok := relayRestartDelay(local, closed, 3); !ok {
		t.Fatal("closed connection must retry")
	}
	if _, ok := relayRestartDelay(local, reset, len(relayRetryDelays)); ok {
		t.Fatal("retries must run out")
	}
	if _, ok := relayRestartDelay(remote, reset, 0); ok {
		t.Fatal("a remote upstream is not the local relay")
	}
	if _, ok := relayRestartDelay(local, fmt.Errorf("x: %w", errors.New("tls: bad certificate")), 0); ok {
		t.Fatal("non-connection errors must not retry")
	}
}
