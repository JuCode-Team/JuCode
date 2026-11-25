package llm

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jucode/jucode/internal/biz"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// recordNoProviderAvailability closes the route-stage blind spot: Resolve
// fails before a usage_log exists, but the selected candidate group still
// represents the user's final business outcome.
func (g *Gateway) recordNoProviderAvailability(ctx context.Context, err error, requestID, model string, endpoint entusagelog.Endpoint, startedAt time.Time) {
	groupID, ok := biz.NoProviderGroupID(err)
	if !ok {
		return
	}
	var modelID *uuid.UUID
	if id, ok := biz.NoProviderModelID(err); ok {
		modelID = &id
	}
	g.recordRouteAvailabilityWithModel(ctx, groupID, modelID, requestID, model, endpoint, entavailability.OutcomeBad, biz.AvailabilityFailureNoProvider, 503, startedAt)
}

func (g *Gateway) recordRouteAvailability(ctx context.Context, groupID uuid.UUID, requestID, model string, endpoint entusagelog.Endpoint, outcome entavailability.Outcome, failureClass string, status int, startedAt time.Time) {
	g.recordRouteAvailabilityWithModel(ctx, groupID, nil, requestID, model, endpoint, outcome, failureClass, status, startedAt)
}

func (g *Gateway) recordRouteAvailabilityWithModel(ctx context.Context, groupID uuid.UUID, modelID *uuid.UUID, requestID, model string, endpoint entusagelog.Endpoint, outcome entavailability.Outcome, failureClass string, status int, startedAt time.Time) {
	_ = g.billing.RecordAvailability(ctx, biz.RecordAvailabilityInput{
		RequestID: requestID, GroupID: groupID, ModelID: modelID, Model: model, Endpoint: string(endpoint),
		Outcome: outcome, FailureClass: failureClass, HTTPStatus: status,
		LatencyMs: int(time.Since(startedAt).Milliseconds()),
		StartedAt: startedAt,
	})
}

func billingRouteAvailability(err error) (entavailability.Outcome, string, int) {
	if errors.Is(err, biz.ErrUnsupportedBillingSource) {
		return entavailability.OutcomeBad, biz.AvailabilityFailureInternal, 503
	}
	if errors.Is(err, biz.ErrPlanNotAllowed) {
		return entavailability.OutcomeExcluded, biz.AvailabilityFailureAccess, 403
	}
	if errors.Is(err, biz.ErrPlanQuotaExhausted) ||
		errors.Is(err, biz.ErrMonthlySpendLimitExceeded) ||
		errors.Is(err, biz.ErrApiKeyCostLimitExceeded) ||
		errors.Is(err, biz.ErrOrganizationLimit) ||
		errors.Is(err, biz.ErrMemberLimit) {
		return entavailability.OutcomeExcluded, biz.AvailabilityFailureQuota, 429
	}
	return entavailability.OutcomeBad, biz.AvailabilityFailureInternal, 500
}
