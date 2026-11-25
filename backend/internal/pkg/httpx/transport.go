// Package httpx holds the HTTP client plumbing shared by the gateway and the
// account/operation services. It exists so the transport tuning lives in one
// place: the gateway dials upstreams directly, while accounts may egress
// through a proxy, and both must behave the same otherwise.
package httpx

import (
	"net"
	"net/http"
	"time"
)

// NewUpstreamTransport builds the transport used for upstream calls: a
// streaming workload with thousands of concurrent long-lived connections per
// host, where the stdlib defaults would force a handshake on nearly every
// reuse.
//
// Only connection establishment is bounded — a dead or refused upstream fails
// fast instead of hanging. ResponseHeaderTimeout is deliberately unset: a slow
// first byte is the stream first-text timeout's business, not the transport's.
func NewUpstreamTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxConnsPerHost:       0,
		MaxIdleConns:          10000,
		MaxIdleConnsPerHost:   5000,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		// DialContext disables HTTP/2 unless this is set. Anthropic and OpenAI
		// both speak HTTP/2; staying on HTTP/1.1 delays the first response byte.
		ForceAttemptHTTP2: true,
	}
}
