package biz

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// AvailabilityFailureNoProvider is the stored failure_class for "no route was
// available". The value keeps its historical spelling (it is written into
// availability_observations and published by the status page), but the thing it
// now means is "no available ACCOUNT": the account pool is the routing entity.
const (
	AvailabilityFailureNoProvider = "no_available_provider"
	AvailabilityFailureClientAuth = "client_auth"
	AvailabilityFailureAccess     = "access_denied"
	AvailabilityFailureQuota      = "quota_or_limit"
	AvailabilityFailureInternal   = "internal_error"
)

// RecordAvailabilityInput is used for final outcomes that happen before a
// usage log can be created (most notably routing with no available account).
// Normal account outcomes are recorded automatically by WriteUsageLog.
type RecordAvailabilityInput struct {
	RequestID    string
	GroupID      uuid.UUID
	ModelID      *uuid.UUID
	Model        string
	Endpoint     string
	Outcome      entavailability.Outcome
	FailureClass string
	HTTPStatus   int
	LatencyMs    int
	TtftMs       *int
	StartedAt    time.Time
	TrafficClass entavailability.TrafficClass
}

type probeTrafficKey struct{}

// WithProbeTraffic marks a request as synthetic. Set by the auth middleware for
// keys flagged internal, and read only where an availability observation is
// built — it deliberately has no effect on billing, quota, or rate limiting, so
// a request that acquires this marker by mistake still costs its owner money
// and still obeys every limit. The only consequence is that it stops counting
// toward the public availability percentage.
func WithProbeTraffic(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeTrafficKey{}, true)
}

// IsProbeTraffic reports whether ctx was marked by WithProbeTraffic.
func IsProbeTraffic(ctx context.Context) bool {
	v, _ := ctx.Value(probeTrafficKey{}).(bool)
	return v
}

// RecordAvailability stores one final observation. A non-empty request ID is
// idempotent per group, so defensive duplicate calls cannot inflate public
// availability statistics.
func (s *BillingSvc) RecordAvailability(ctx context.Context, in RecordAvailabilityInput) error {
	if in.GroupID == uuid.Nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	return createAvailabilityObservation(ctx, s.d.Ent, in)
}

func createAvailabilityObservation(ctx context.Context, client *ent.Client, in RecordAvailabilityInput) error {
	if in.GroupID == uuid.Nil {
		return nil
	}
	return availabilityCreateBuilder(client, normalizeAvailabilityInput(ctx, in)).
		OnConflict().
		DoNothing().
		Exec(ctx)
}

// normalizeAvailabilityInput applies every default that depends on the calling
// context, most importantly the synthetic-traffic tag.
//
// This MUST run while the request context is still in hand. IsProbeTraffic
// reads a ctx value, so anything that defers the write to another goroutine
// (the usage-log batch writer) has to normalize at submit time — by flush time
// the probe marker is gone and synthetic traffic would be counted as business
// traffic in the public availability numbers.
func normalizeAvailabilityInput(ctx context.Context, in RecordAvailabilityInput) RecordAvailabilityInput {
	if in.Outcome == "" {
		in.Outcome = entavailability.OutcomeBad
	}
	// Single choke point for the synthetic tag: every writer lands here, so
	// the marker cannot be forgotten at one of the callers.
	if IsProbeTraffic(ctx) {
		in.TrafficClass = entavailability.TrafficClassProbe
	}
	if in.TrafficClass == "" {
		in.TrafficClass = entavailability.TrafficClassBusiness
	}
	if in.StartedAt.IsZero() {
		in.StartedAt = time.Now()
	}
	if in.LatencyMs < 0 {
		in.LatencyMs = 0
	}
	return in
}

// availabilityCreateBuilder maps an already-normalized input onto column
// setters. Shared by the one-row path and the batch writer's CreateBulk.
func availabilityCreateBuilder(client *ent.Client, in RecordAvailabilityInput) *ent.AvailabilityObservationCreate {
	return client.AvailabilityObservation.Create().
		SetRequestID(limitAvailabilityText(in.RequestID, 128)).
		SetGroupID(in.GroupID).
		SetNillableModelID(in.ModelID).
		SetModel(limitAvailabilityText(in.Model, 256)).
		SetEndpoint(limitAvailabilityText(in.Endpoint, 64)).
		SetOutcome(in.Outcome).
		SetFailureClass(limitAvailabilityText(in.FailureClass, 64)).
		SetHTTPStatus(in.HTTPStatus).
		SetLatencyMs(in.LatencyMs).
		SetNillableTtftMs(in.TtftMs).
		SetStartedAt(in.StartedAt.UTC()).
		SetTrafficClass(in.TrafficClass)
}

func availabilityInputFromUsage(in WriteUsageLogInput) (RecordAvailabilityInput, bool) {
	if in.GroupID == nil || *in.GroupID == uuid.Nil {
		return RecordAvailabilityInput{}, false
	}
	outcome, failureClass := classifyAvailabilityOutcome(in)
	return RecordAvailabilityInput{
		RequestID:    in.RequestID,
		GroupID:      *in.GroupID,
		ModelID:      in.ModelID,
		Model:        in.Model,
		Endpoint:     string(in.Endpoint),
		Outcome:      outcome,
		FailureClass: failureClass,
		HTTPStatus:   in.HTTPStatus,
		LatencyMs:    in.LatencyMs,
		TtftMs:       in.TtftMs,
		StartedAt:    in.StartedAt,
		TrafficClass: entavailability.TrafficClassBusiness,
	}, true
}

// classifyAvailabilityOutcome is deliberately based on structured status and
// error-class fields only. Free-form upstream messages are diagnostic data and
// must never change the public metric.
func classifyAvailabilityOutcome(in WriteUsageLogInput) (entavailability.Outcome, string) {
	failureClass := strings.TrimSpace(in.ErrorClass)

	// A response already delivered to the caller remains a successful service
	// outcome even if later charging or audit persistence reports an error.
	if in.Delivered || in.Status == entusagelog.StatusOk || failureClass == "ok" {
		return entavailability.OutcomeGood, ""
	}

	switch failureClass {
	case "provider_failure", "rate_limit", "auth", "pool_exhausted", "upstream_config", AvailabilityFailureNoProvider:
		return entavailability.OutcomeBad, failureClass
	// "timeout" folds first_text/idle/total stream timeouts together
	// (llm.deriveErrorClass). Excluded by operator decision: the dominant
	// source is our own stream deadline cutting long cold-cache prefills on
	// large-context requests, not an upstream account fault. Note the trade-off
	// — an account that accepts connections and never responds now reports as
	// available, and a group whose only failures are timeouts yields
	// good+bad == 0, which surfaces as "no data" rather than 0%.
	case "timeout", "user_request", "user_policy", "cyber_block", "client_canceled":
		return entavailability.OutcomeExcluded, failureClass
	}

	switch in.Status {
	case entusagelog.StatusQuotaExceeded:
		return entavailability.OutcomeExcluded, AvailabilityFailureQuota
	case entusagelog.StatusBanned:
		return entavailability.OutcomeExcluded, AvailabilityFailureAccess
	case entusagelog.StatusUnauthorized:
		return entavailability.OutcomeExcluded, AvailabilityFailureClientAuth
	case entusagelog.StatusUpstreamError:
		if failureClass == "" {
			failureClass = "upstream_error"
		}
		return entavailability.OutcomeBad, failureClass
	case entusagelog.StatusInternalError:
		// User-side precheck rejections sometimes use internal_error for a
		// typed 4xx. They are excluded unless a provider-side class above has
		// already established that the service itself failed.
		if in.HTTPStatus >= 400 && in.HTTPStatus < 500 {
			if failureClass == "" {
				failureClass = AvailabilityFailureAccess
			}
			return entavailability.OutcomeExcluded, failureClass
		}
		if failureClass == "" || failureClass == "unknown" {
			failureClass = AvailabilityFailureInternal
		}
		return entavailability.OutcomeBad, failureClass
	default:
		if failureClass == "" || failureClass == "unknown" {
			failureClass = AvailabilityFailureInternal
		}
		return entavailability.OutcomeBad, failureClass
	}
}

func limitAvailabilityText(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes])
}
