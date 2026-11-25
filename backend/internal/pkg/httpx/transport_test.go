package httpx

import "testing"

func TestUpstreamTransportAttemptsHTTP2(t *testing.T) {
	if !NewUpstreamTransport().ForceAttemptHTTP2 {
		t.Fatal("upstream transport is not attempting HTTP/2")
	}
}
