package llm

import (
	"context"
	"errors"
	"net"
	"net/url"
	"syscall"
	"time"

	"github.com/jucode/jucode/internal/biz"
)

// An upstream relay running on the same host restarts on every deploy. For about ten seconds its port refuses or resets connections before
// the request reaches it. Failing over at once lands every attempt in the same
// window, and cooling the account punishes an account that is fine. A request
// to a loopback upstream that drops this way waits and retries the same
// account instead, outside the attempt budget.
var relayRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// relayRestartDelay returns how long to wait before the n-th (0-based) retry,
// false when err is not a relay restart or the retries are spent.
func relayRestartDelay(rp biz.RouteAccount, err error, n int) (time.Duration, bool) {
	if n >= len(relayRetryDelays) || rp.Account == nil || !isLoopbackURL(rp.Account.BaseURL) || !isConnDrop(err) {
		return 0, false
	}
	return relayRetryDelays[n], true
}

// isConnDrop reports a connection refused, reset or closed under the request.
func isConnDrop(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed)
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sleepCtx waits d, returning false if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
