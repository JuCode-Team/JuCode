package llm

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	errUpstreamStreamIdleTimeout  = errors.New("upstream stream idle timeout")
	errUpstreamFirstTextTimeout   = errors.New("upstream first text timeout")
	errUpstreamStreamTotalTimeout = errors.New("upstream stream total timeout")
)

// errUpstreamStreamIncomplete is raised when a 2xx stream ends before the
// terminal completion frame (response.completed / message_stop) without a
// timeout or client cancel — the upstream just dropped the connection. Kept
// as a sentinel so the final error path can name the reason for the client.
var errUpstreamStreamIncomplete = errors.New("stream closed before response.completed")

// errNoUpstreamCapacity means no candidate could even be attempted: every
// account of the routed channels was at its concurrency cap, or a channel's
// accounts were all parked. The upstream may be perfectly healthy, so this
// must answer as backpressure (503 + Retry-After), not as "unreachable" (502).
var errNoUpstreamCapacity = errors.New("no upstream capacity available")

// streamTotalTimeout is a single, non-resettable wall-clock cap on a whole
// streamed request (across all attempts). It bounds the long tail we saw
// in prod (single streams running ~470s) that no other timer caught —
// first-text only guards the head, idle resets on every token.
type streamTotalTimeout struct {
	mu      sync.Mutex
	timer   *time.Timer
	cancel  context.CancelCauseFunc
	stopped bool
}

func withStreamTotalTimeout(parent context.Context, timeout time.Duration) (context.Context, *streamTotalTimeout) {
	if timeout <= 0 {
		return parent, nil
	}
	ctx, cancel := context.WithCancelCause(parent)
	t := &streamTotalTimeout{cancel: cancel}
	t.timer = time.AfterFunc(timeout, func() {
		t.mu.Lock()
		if t.stopped {
			t.mu.Unlock()
			return
		}
		t.stopped = true
		t.mu.Unlock()
		cancel(errUpstreamStreamTotalTimeout)
	})
	return ctx, t
}

func (t *streamTotalTimeout) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	t.timer.Stop()
}

func streamTotalTimedOut(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errUpstreamStreamTotalTimeout)
}

type streamIdleTimeout struct {
	mu      sync.Mutex
	timeout time.Duration
	timer   *time.Timer
	cancel  context.CancelCauseFunc
	stopped bool
}

func withStreamIdleTimeout(parent context.Context, timeout time.Duration) (context.Context, *streamIdleTimeout) {
	if timeout <= 0 {
		return parent, nil
	}
	ctx, cancel := context.WithCancelCause(parent)
	t := &streamIdleTimeout{
		timeout: timeout,
		cancel:  cancel,
	}
	t.timer = time.AfterFunc(timeout, func() {
		cancel(errUpstreamStreamIdleTimeout)
	})
	return ctx, t
}

func (t *streamIdleTimeout) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.timer.Reset(t.timeout)
}

func (t *streamIdleTimeout) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	t.timer.Stop()
}

func streamIdleTimedOut(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errUpstreamStreamIdleTimeout)
}

type streamFirstTextTimeout struct {
	mu      sync.Mutex
	timeout time.Duration
	timer   *time.Timer
	cancel  context.CancelCauseFunc
	stopped bool
}

func withStreamFirstTextTimeout(parent context.Context, timeout time.Duration) (context.Context, *streamFirstTextTimeout) {
	if timeout <= 0 {
		return parent, nil
	}
	ctx, cancel := context.WithCancelCause(parent)
	t := &streamFirstTextTimeout{
		timeout: timeout,
		cancel:  cancel,
	}
	t.timer = time.AfterFunc(timeout, func() {
		t.mu.Lock()
		if t.stopped {
			t.mu.Unlock()
			return
		}
		t.stopped = true
		t.mu.Unlock()
		cancel(errUpstreamFirstTextTimeout)
	})
	return ctx, t
}

func (t *streamFirstTextTimeout) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	t.timer.Stop()
}

func streamFirstTextTimedOut(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errUpstreamFirstTextTimeout)
}
