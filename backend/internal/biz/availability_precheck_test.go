package biz

import (
	"net/http"
	"testing"

	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// A billing precheck rejection is the caller's problem, not the platform's.
// The gateway used to whitelist three error values and let everything else —
// including "insufficient balance", the most common rejection by an order of
// magnitude — fall through to internal_error. That produced a fake 1.6%
// internal-error rate in usage_logs and is why classifyAvailabilityOutcome needs the
// 4xx escape hatch below.
//
// These cases pin the contract the gateway now relies on: a precheck rejection
// carrying a 4xx must never count as a service failure.
func TestPrecheckRejectionsAreNotServiceFailures(t *testing.T) {
	cases := []struct {
		name       string
		status     entusagelog.Status
		httpStatus int
	}{
		{"insufficient balance", entusagelog.StatusQuotaExceeded, http.StatusPaymentRequired},
		{"plan quota exhausted", entusagelog.StatusQuotaExceeded, http.StatusTooManyRequests},
		{"api key cost limit", entusagelog.StatusQuotaExceeded, http.StatusTooManyRequests},
		{"org limit", entusagelog.StatusQuotaExceeded, http.StatusTooManyRequests},
		{"plan not allowed", entusagelog.StatusQuotaExceeded, http.StatusForbidden},
		// The legacy shape: internal_error carrying a 4xx. Still has to be
		// excluded, because rows written before this fix are in the table.
		{"legacy internal_error with 4xx", entusagelog.StatusInternalError, http.StatusPaymentRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, _ := classifyAvailabilityOutcome(WriteUsageLogInput{
				Status:     c.status,
				HTTPStatus: c.httpStatus,
			})
			if outcome == entavailability.OutcomeBad {
				t.Errorf("%s counted as a service failure; it is a caller-side rejection", c.name)
			}
		})
	}
}

// The other half of the contract: a genuine platform fault must still count.
// ErrUnsupportedBillingSource resolves to 503 and is a misconfiguration on our
// side, so it has to survive the reclassification above.
func TestPlatformFaultsStillCountAsBad(t *testing.T) {
	outcome, _ := classifyAvailabilityOutcome(WriteUsageLogInput{
		Status:     entusagelog.StatusInternalError,
		HTTPStatus: http.StatusServiceUnavailable,
	})
	if outcome != entavailability.OutcomeBad {
		t.Errorf("503 internal_error got %s; a platform fault must count as bad", outcome)
	}
}
