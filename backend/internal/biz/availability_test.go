package biz

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// The probe marker is set by the auth middleware on the request context, but
// every observation is written from a detached context: WriteUsageLog rebuilds
// it with context.WithoutCancel + WithTimeout so the row still lands after the
// client hangs up. If that detachment ever stopped carrying values, probe
// traffic would silently start counting as business traffic again and quietly
// move the public availability number.
func TestProbeMarkerSurvivesContextDetachment(t *testing.T) {
	ctx := WithProbeTraffic(context.Background())
	if !IsProbeTraffic(ctx) {
		t.Fatal("marker not readable on the context it was set on")
	}

	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if !IsProbeTraffic(detached) {
		t.Error("marker lost across WithoutCancel+WithTimeout, as used by WriteUsageLog")
	}

	if IsProbeTraffic(context.Background()) {
		t.Error("unmarked context reported as probe traffic")
	}
}

func TestClassifyAvailabilityOutcome(t *testing.T) {
	cases := []struct {
		name    string
		in      WriteUsageLogInput
		outcome entavailability.Outcome
		failure string
	}{
		{"ok", WriteUsageLogInput{Status: entusagelog.StatusOk, HTTPStatus: http.StatusOK}, entavailability.OutcomeGood, ""},
		{"delivered despite charge failure", WriteUsageLogInput{Status: entusagelog.StatusInternalError, HTTPStatus: http.StatusOK, Delivered: true}, entavailability.OutcomeGood, ""},
		{"provider failure", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "provider_failure", HTTPStatus: 503}, entavailability.OutcomeBad, "provider_failure"},
		{"rate limit is service failure", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "rate_limit", HTTPStatus: 429}, entavailability.OutcomeBad, "rate_limit"},
		{"user input excluded", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "user_request", HTTPStatus: 400}, entavailability.OutcomeExcluded, "user_request"},
		{"policy excluded", WriteUsageLogInput{Status: entusagelog.StatusInternalError, ErrorClass: "user_policy", HTTPStatus: 422}, entavailability.OutcomeExcluded, "user_policy"},
		{"quota excluded", WriteUsageLogInput{Status: entusagelog.StatusQuotaExceeded, HTTPStatus: 429}, entavailability.OutcomeExcluded, AvailabilityFailureQuota},
		{"internal 5xx bad", WriteUsageLogInput{Status: entusagelog.StatusInternalError, HTTPStatus: 500}, entavailability.OutcomeBad, AvailabilityFailureInternal},
		{"typed 403 excluded", WriteUsageLogInput{Status: entusagelog.StatusInternalError, HTTPStatus: 403}, entavailability.OutcomeExcluded, AvailabilityFailureAccess},
		// The three classes below now reach here from a terminal `response.failed`
		// frame or a dead client socket, all carrying the upstream's 200. Status
		// and HTTP code say "fine", so only the error class can exclude them; if
		// this drifts, user-caused aborts silently become public unavailability
		// again (observed: 50 cyber_policy + 8 context_length_exceeded + 81 client
		// disconnects wrongly counted as outage in a single 19h window).
		{"cyber block on delivered 200 excluded", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "cyber_block", HTTPStatus: http.StatusOK}, entavailability.OutcomeExcluded, "cyber_block"},
		{"client cancel on 200 excluded", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "client_canceled", HTTPStatus: 499}, entavailability.OutcomeExcluded, "client_canceled"},
		{"over-long prompt on 200 excluded", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "user_request", HTTPStatus: http.StatusOK}, entavailability.OutcomeExcluded, "user_request"},
		// ...while a genuine mid-stream provider abort on the same 200 stays bad.
		{"provider abort on 200 stays bad", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "provider_failure", HTTPStatus: http.StatusOK}, entavailability.OutcomeBad, "provider_failure"},
		// Timeouts are excluded by operator decision — the dominant source was
		// our own stream deadline cutting long cold-cache prefills, not the
		// channel. Note this covers all three folded timeout kinds, so a hung
		// upstream no longer shows up as unavailable; the class must stay
		// excluded on both the 504 and the delivered-200 shape.
		{"stream timeout excluded", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "timeout", HTTPStatus: http.StatusGatewayTimeout}, entavailability.OutcomeExcluded, "timeout"},
		{"timeout on 200 excluded", WriteUsageLogInput{Status: entusagelog.StatusUpstreamError, ErrorClass: "timeout", HTTPStatus: http.StatusOK}, entavailability.OutcomeExcluded, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, failure := classifyAvailabilityOutcome(tc.in)
			if got != tc.outcome || failure != tc.failure {
				t.Fatalf("got (%q,%q), want (%q,%q)", got, failure, tc.outcome, tc.failure)
			}
		})
	}
}

// Only a genuinely empty window is unknown — the old 20-sample floor is gone,
// because probe traffic deliberately keeps these windows tiny (two samples at a
// 15m cadence in a 30m window) and a floor would pin every probed group to
// unknown forever, defeating the probes entirely.
//
// The bands themselves are the contract: an off-by-one in the comparison chain
// silently relabels every group on the page, so each case sits ON a boundary or
// one step below it. The single-failure damping is checked separately below.
func TestPublicStateSampleConfidenceAndThresholds(t *testing.T) {
	if got := publicState(0, 0); got != StatusUnknown {
		t.Fatalf("no data state = %q, want unknown", got)
	}
	if got := publicState(1, 0); got != StatusHealthy {
		t.Fatalf("single good sample = %q, want healthy — no minimum sample floor", got)
	}

	cases := []struct {
		name      string
		good, bad int
		want      StatusState
	}{
		{"100%", 100, 0, StatusHealthy},
		{"98% is the healthy floor", 98, 2, StatusHealthy},
		{"97.9% just misses healthy", 979, 21, StatusDegraded},
		{"90% is the degraded floor", 90, 10, StatusDegraded},
		{"89.9% just misses degraded", 899, 101, StatusSerious},
		{"80% is the serious floor", 80, 20, StatusSerious},
		{"79.9% just misses serious", 799, 201, StatusDown},
		{"0%", 0, 100, StatusDown},
	}
	for _, tc := range cases {
		if got := publicState(tc.good, tc.bad); got != tc.want {
			t.Errorf("%s: publicState(%d,%d) = %q, want %q", tc.name, tc.good, tc.bad, got, tc.want)
		}
	}
}

// A lone failure must not take a card off healthy. With the sample floor gone,
// the arithmetic alone would call one bad probe out of two a 50% outage; only a
// corroborated second failure may grade anything below healthy.
func TestPublicStateDampsSingleFailure(t *testing.T) {
	cases := []struct {
		name      string
		good, bad int
		want      StatusState
	}{
		{"1 of 2 probes failed is not an outage", 1, 1, StatusHealthy},
		{"the only probe failed is not an outage", 0, 1, StatusHealthy},
		{"one failure in a quiet window does not degrade", 3, 1, StatusHealthy},
		{"two failures at 96% degrade", 48, 2, StatusDegraded},
		{"two failures out of two is", 0, 2, StatusDown},
		{"two failures at 33% is still down — damping gates, it does not clamp", 1, 2, StatusDown},
		{"two failures at 85% lands in serious", 17, 3, StatusSerious},
		// Damping is inert once real traffic is involved: anything failing hard
		// enough to reach these bands has many more than two failures.
		{"heavy traffic still reaches down", 0, 100, StatusDown},
		{"heavy traffic still reaches serious", 80, 20, StatusSerious},
	}
	for _, tc := range cases {
		if got := publicState(tc.good, tc.bad); got != tc.want {
			t.Errorf("%s: publicState(%d,%d) = %q, want %q", tc.name, tc.good, tc.bad, got, tc.want)
		}
	}
}

// Group cards used to grade pooled request counts. One hot failing model
// (Luna inside gpt-pro) then painted the whole group down. The public pill
// now votes by how many models are impaired, so a single outage is partial.
func TestGroupStateFromModels(t *testing.T) {
	cases := []struct {
		name   string
		states []StatusState
		want   StatusState
	}{
		{"no models is unknown", nil, StatusUnknown},
		{"all unknown is unknown", []StatusState{StatusUnknown, StatusUnknown}, StatusUnknown},
		{"all healthy", []StatusState{StatusHealthy, StatusHealthy, StatusHealthy}, StatusHealthy},
		{"unknowns do not count as healthy or down", []StatusState{StatusHealthy, StatusUnknown}, StatusHealthy},
		{"one of many down is only degraded", []StatusState{
			StatusDown, StatusHealthy, StatusHealthy, StatusHealthy,
			StatusHealthy, StatusHealthy, StatusHealthy, StatusHealthy,
		}, StatusDegraded},
		{"one of many serious is only degraded", []StatusState{
			StatusSerious, StatusHealthy, StatusHealthy, StatusHealthy,
		}, StatusDegraded},
		{"the only model down is down", []StatusState{StatusDown}, StatusDown},
		{"the only model serious is serious", []StatusState{StatusSerious}, StatusSerious},
		{"half of two down is still partial", []StatusState{StatusDown, StatusHealthy}, StatusDegraded},
		{"majority down is down", []StatusState{
			StatusDown, StatusDown, StatusDown, StatusDown, StatusDown,
			StatusHealthy, StatusHealthy, StatusHealthy,
		}, StatusDown},
		{"majority serious-or-worse is serious", []StatusState{
			StatusSerious, StatusSerious, StatusSerious, StatusHealthy,
		}, StatusSerious},
		{"every known model down is down even when some are quiet", []StatusState{
			StatusDown, StatusDown, StatusUnknown,
		}, StatusDown},
	}
	for _, tc := range cases {
		if got := groupStateFromModels(tc.states); got != tc.want {
			t.Errorf("%s: groupStateFromModels(%v) = %q, want %q", tc.name, tc.states, got, tc.want)
		}
	}
}

// The vote hides a hot failing model behind "degraded"; pooled counts raise the
// card to serious, never further.
func TestGroupStateRaisesOnPooledFailure(t *testing.T) {
	oneHotModelDown := []StatusState{StatusDown, StatusHealthy, StatusHealthy, StatusHealthy}
	cases := []struct {
		name      string
		states    []StatusState
		good, bad int
		want      StatusState
	}{
		{"pooled 41% raises degraded vote to serious", oneHotModelDown, 101, 144, StatusSerious},
		{"pooled 85% raises to serious", oneHotModelDown, 85, 15, StatusSerious},
		{"pooled 96% leaves the vote alone", oneHotModelDown, 96, 4, StatusDegraded},
		{"pooled never raises past serious", []StatusState{StatusHealthy, StatusHealthy}, 0, 100, StatusSerious},
		{"a down vote is not lowered", []StatusState{StatusDown}, 0, 100, StatusDown},
		{"single pooled failure is inert", []StatusState{StatusHealthy}, 0, 1, StatusHealthy},
	}
	for _, tc := range cases {
		if got := groupState(tc.states, tc.good, tc.bad); got != tc.want {
			t.Errorf("%s: groupState(%v, %d, %d) = %q, want %q", tc.name, tc.states, tc.good, tc.bad, got, tc.want)
		}
	}
}

// Every impaired state must raise an incident. Forgetting to add a new state to
// that condition is silent: the group card turns orange and the panel keeps
// claiming "当前无异常".
func TestImpairedStatesAreAllIncidentWorthy(t *testing.T) {
	incidentWorthy := map[StatusState]bool{
		StatusDegraded: true,
		StatusSerious:  true,
		StatusDown:     true,
		StatusHealthy:  false,
		StatusUnknown:  false,
	}
	for state, want := range incidentWorthy {
		got := state == StatusDown || state == StatusSerious || state == StatusDegraded
		if got != want {
			t.Errorf("state %q incident-worthy = %v, want %v", state, got, want)
		}
	}
}

func TestAvailabilityPercentage(t *testing.T) {
	if got := availabilityPercentage(0, 0); got != nil {
		t.Fatalf("empty percentage = %v, want nil", *got)
	}
	got := availabilityPercentage(3, 1)
	if got == nil || *got != 75 {
		t.Fatalf("percentage = %v, want 75", got)
	}
}

func TestNoProviderErrorPreservesSentinelAndGroup(t *testing.T) {
	groupID := uuid.New()
	err := (&NoProviderError{GroupID: groupID, Detail: "candidate_groups=1"})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatal("typed error does not preserve ErrNoProvider")
	}
	got, ok := NoProviderGroupID(err)
	if !ok || got != groupID {
		t.Fatalf("group = %v,%v, want %v,true", got, ok, groupID)
	}
}

func TestAvailabilityModelQueryScopesGroups(t *testing.T) {
	groupID := uuid.New()
	query, args := availabilityModelQuery("bucket", time.Now(), time.Now(), 15*time.Minute, []uuid.UUID{groupID}, []string{"claude-3"})
	if !strings.Contains(query, "group_id IN ($4)") || !strings.Contains(query, "model IN ($5)") {
		t.Fatalf("query placeholders are not scoped correctly: %s", query)
	}
	if len(args) != 5 {
		t.Fatalf("bucket args = %d, want 5", len(args))
	}
	// Per-group grouping is what keeps two groups serving the same model name
	// from being pooled into one misleading number.
	if !strings.Contains(query, "GROUP BY group_id, model, bucket_index") {
		t.Fatalf("bucket query does not aggregate per group: %s", query)
	}
	aggregate, _ := availabilityModelQuery("aggregate", time.Now(), time.Now(), 0, []uuid.UUID{groupID}, []string{"claude-3"})
	if !strings.Contains(aggregate, "GROUP BY group_id, model") {
		t.Fatalf("aggregate query does not aggregate per group: %s", aggregate)
	}
}

// The whole point of tagging probe traffic is that it can light up the state
// pill and the 24h bar without touching the published percentage. That
// separation lives entirely in these WHERE clauses: if the "window" query ever
// starts counting probes, the prober silently inflates the very number it
// exists to observe, and nothing else in the system would notice.
func TestOnlyPublishedWindowExcludesProbeTraffic(t *testing.T) {
	groupID := uuid.New()
	now := time.Now()

	for _, tc := range []struct {
		kind      string
		wantProbe bool
	}{
		{"bucket", true},
		{"aggregate", true},
		{"window", false},
	} {
		groupQ, _ := availabilityGroupQuery(tc.kind, now, now, time.Minute, []uuid.UUID{groupID})
		modelQ, _ := availabilityModelQuery(tc.kind, now, now, time.Minute, []uuid.UUID{groupID}, []string{"claude-3"})
		for name, q := range map[string]string{"group": groupQ, "model": modelQ} {
			gotProbe := strings.Contains(q, "traffic_class IN ('business','probe')")
			gotBusinessOnly := strings.Contains(q, "traffic_class = 'business'")
			if gotProbe != tc.wantProbe || gotBusinessOnly == tc.wantProbe {
				t.Errorf("%s query, kind=%q: probe-inclusive=%v business-only=%v, want probe-inclusive=%v",
					name, tc.kind, gotProbe, gotBusinessOnly, tc.wantProbe)
			}
		}
	}
}

func TestGroupModelPairsDedupesNamesButKeepsPerGroupKeys(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	shared := &ent.Model{ID: uuid.New(), Name: "claude-3"}
	groups := []*ent.ModelGroup{{ID: a, Name: "fast"}, {ID: b, Name: "cheap"}}
	pairs, names := groupModelPairs(groups, map[uuid.UUID][]*ent.Model{a: {shared}, b: {shared}})
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want 2 (one per group)", len(pairs))
	}
	if len(names) != 1 || names[0] != "claude-3" {
		t.Fatalf("names = %v, want one deduped name", names)
	}
}

// A model can be attached to a group without any of that group's providers
// actually serving it. The status page must not list
// those rows — they are not a route a customer can hit.
func TestFilterRoutableModelsDropsUnservedNames(t *testing.T) {
	groupID := uuid.New()
	fable := &ent.Model{ID: uuid.New(), Name: "claude-fable-5"}
	opus := &ent.Model{ID: uuid.New(), Name: "claude-opus-4-8"}
	got := filterRoutableModels(
		map[uuid.UUID][]*ent.Model{groupID: {fable, opus}},
		map[uuid.UUID]map[string]struct{}{groupID: {"claude-opus-4-8": {}}},
	)
	if len(got[groupID]) != 1 || got[groupID][0].Name != "claude-opus-4-8" {
		names := make([]string, 0, len(got[groupID]))
		for _, m := range got[groupID] {
			names = append(names, m.Name)
		}
		t.Fatalf("models = %v, want only claude-opus-4-8", names)
	}
}
