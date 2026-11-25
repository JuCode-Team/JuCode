package llm

import "github.com/jucode/jucode/internal/biz"

// contextRetryWindow decides whether an upstream "input exceeds the context
// window" rejection should fail over instead of going back to the caller.
//
// /v1/models advertises the smallest window among a model's accounts and lets
// a client opt into the largest, so a request between the two is only
// servable by some of them. When the account that rejected it has a known
// window and a later candidate offers a larger one (within the attempt
// budget), the rejection is retried there; the returned window becomes the
// floor that later candidates must exceed. 0 means "terminal: forward the
// rejection", which is also the answer when windows are not configured — the
// gateway never guesses which account might fit.
func contextRetryWindow(providers []biz.RouteAccount, i int, rp biz.RouteAccount, body []byte, attemptsUsed, maxAttempts int) int {
	if rp.ContextWindow <= 0 || attemptsUsed >= maxAttempts || i+1 >= len(providers) {
		return 0
	}
	if _, code, ok := actionableUpstreamError(body); !ok || code != "context_length_exceeded" {
		return 0
	}
	for _, next := range providers[i+1:] {
		if next.ContextWindow > rp.ContextWindow {
			return rp.ContextWindow
		}
	}
	return 0
}
