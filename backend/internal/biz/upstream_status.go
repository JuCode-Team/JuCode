// Upstream provider status — what OpenAI and Anthropic say about themselves
// on their own status pages, mirrored onto our public /status page.
//
// This is deliberately kept apart from the availability numbers in status.go.
// Those are measured: computed from real business traffic through our own
// gateway. This is reported: whatever the vendor chose to publish. The two
// disagree routinely — an upstream that still reads "All Systems Operational"
// while our success rate falls off a cliff is the normal case, not the
// exception. Merging them into one list would destroy that distinction, so
// they travel as separate fields and render as separate panels.
//
// Both endpoints speak the Atlassian Statuspage v2 shape, but only Anthropic
// actually runs Statuspage. OpenAI's page is an incident.io reimplementation
// that covers part of the same surface — see fetchOpenAI for the gaps.
package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data"
)

const (
	// upstreamFetchTimeout caps a single HTTP call. Measured from the
	// production host: Anthropic answers in ~20ms (CDN hit), OpenAI in
	// ~170ms. 10s is pure headroom for a bad day.
	upstreamFetchTimeout = 10 * time.Second

	// upstreamPollInterval is how often the leader refreshes the mirror.
	// Anthropic's own edge cache is s-maxage=10, so this is well inside
	// what their page considers a reasonable read rate.
	upstreamPollInterval = 60 * time.Second

	// upstreamStaleAfter is when a stored snapshot stops being evidence.
	// Past this we render "unknown" rather than the last known value: a
	// frozen green light is worse than an honest blank, because it is
	// wrong in exactly the situation the page exists to report.
	upstreamStaleAfter = 10 * time.Minute

	// upstreamRedisTTL outlives staleAfter so Snapshot can still read the
	// record and see for itself that it is stale, instead of finding a
	// missing key and being unable to tell "never fetched" from "expired".
	upstreamRedisTTL = 30 * time.Minute

	// upstreamMaxBody caps what we read off the wire. OpenAI's incident
	// feed is ~41KB, but its 404 page is a 64KB Next.js HTML blob — the
	// status check below rejects that first, this is the second line.
	upstreamMaxBody = 4 << 20
)

// UpstreamState is our normalized verdict for one provider. The raw vendor
// vocabulary (indicator/component status) is richer but inconsistent across
// the two pages, so we collapse it here and keep the raw strings alongside
// for display.
type UpstreamState string

const (
	UpstreamOperational UpstreamState = "operational"
	UpstreamDegraded    UpstreamState = "degraded"
	UpstreamOutage      UpstreamState = "outage"
	UpstreamMaintenance UpstreamState = "maintenance"
	// UpstreamUnknown means we could not reach the page, or what we have
	// is too old to trust. Never emitted as a synonym for "fine".
	UpstreamUnknown UpstreamState = "unknown"
)

// UpstreamComponent is one vendor subsystem, carried only when it is NOT
// operational. Listing all of them all the time would mean 34 permanently
// green rows for OpenAI alone, and the names drift with their product line
// (Ads Manager, ChatGPT Atlas) in ways that have nothing to do with us.
type UpstreamComponent struct {
	Name string `json:"name"`
	// Status is the vendor's raw string (degraded_performance,
	// partial_outage, major_outage, under_maintenance). Kept raw so the
	// frontend owns the wording and we don't lose detail in translation.
	Status string `json:"status"`
}

// UpstreamIncident is one unresolved vendor incident.
type UpstreamIncident struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // investigating | identified | monitoring
	Impact string `json:"impact"` // none | minor | major | critical
	// StartedAt prefers the vendor's own started_at and falls back to
	// created_at. OpenAI never sends started_at.
	StartedAt time.Time `json:"started_at"`
	// LatestUpdate is the body of the most recent incident update, which
	// is the one sentence a reader actually wants.
	LatestUpdate string `json:"latest_update"`
	URL          string `json:"url"`
}

// UpstreamProvider is the mirrored state of one vendor status page.
type UpstreamProvider struct {
	Key     string        `json:"key"`
	Name    string        `json:"name"`
	PageURL string        `json:"page_url"`
	State   UpstreamState `json:"state"`
	// Description is the vendor's own headline ("All Systems Operational").
	// Empty when State is unknown.
	Description string              `json:"description"`
	Components  []UpstreamComponent `json:"components"`
	Incidents   []UpstreamIncident  `json:"incidents"`
	// FetchedAt is the last time this provider was read successfully. Null
	// means the most recent attempt failed; we do not carry forward an
	// older success, because a timestamp next to stale content invites
	// reading the content as current.
	FetchedAt *time.Time `json:"fetched_at"`
	// Error is a short reason, present only when State is unknown.
	Error string `json:"error,omitempty"`
}

// UpstreamSnapshot is the whole mirror as stored in Redis and embedded in
// PublicStatus.
type UpstreamSnapshot struct {
	Providers []UpstreamProvider `json:"providers"`
	// PolledAt stamps the sweep, successful or not, and is what the
	// staleness check reads.
	PolledAt time.Time `json:"polled_at"`
}

// upstreamSource describes one page to mirror.
type upstreamSource struct {
	Key     string
	Name    string
	PageURL string
	// SummaryURL carries the top-level indicator plus components.
	SummaryURL string
	// IncidentsURL is set only when the summary payload omits incidents,
	// which is OpenAI's case.
	IncidentsURL string
}

// upstreamSources is intentionally hardcoded rather than configurable.
// These are not interchangeable feeds: each one needed its endpoint quirks
// discovered by hand (see fetchOpenAI), so an operator-supplied URL would
// parse into garbage more often than it would help.
//
// Note the Anthropic host: status.anthropic.com now 302s to
// status.claude.com. We point at the destination directly so a redirect
// policy change can't silently break the mirror.
var upstreamSources = []upstreamSource{
	{
		Key:        "anthropic",
		Name:       "Anthropic",
		PageURL:    "https://status.claude.com",
		SummaryURL: "https://status.claude.com/api/v2/summary.json",
	},
	{
		Key:          "openai",
		Name:         "OpenAI",
		PageURL:      "https://status.openai.com",
		SummaryURL:   "https://status.openai.com/api/v2/summary.json",
		IncidentsURL: "https://status.openai.com/api/v2/incidents.json",
	},
}

type UpstreamStatusSvc struct {
	d    *data.Data
	http *http.Client
	log  *slog.Logger
}

// NewUpstreamStatusSvc builds the poller. The client deliberately uses the
// default transport: it honors HTTPS_PROXY from the environment, which the
// hand-rolled transports elsewhere in this package do not. The production
// host reaches both pages directly today, but a move behind a proxy should
// not require a code change here.
func NewUpstreamStatusSvc(d *data.Data, log *slog.Logger) *UpstreamStatusSvc {
	if log == nil {
		log = slog.Default()
	}
	return &UpstreamStatusSvc{
		d:    d,
		http: &http.Client{Timeout: upstreamFetchTimeout},
		log:  log,
	}
}

// Start runs the poll loop until ctx is cancelled. Call it from the leader
// election callback only — jucode-a and jucode-b both polling would double
// our request rate against the vendors for no added freshness.
func (s *UpstreamStatusSvc) Start(ctx context.Context) {
	s.pollOnce(ctx)
	go func() {
		ticker := time.NewTicker(upstreamPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pollOnce(ctx)
			}
		}
	}()
}

// pollOnce refreshes every source and stores the result. A provider that
// fails is stored as unknown rather than dropped, so the page can say so
// explicitly instead of silently rendering one fewer row.
func (s *UpstreamStatusSvc) pollOnce(ctx context.Context) {
	snap := UpstreamSnapshot{
		Providers: make([]UpstreamProvider, 0, len(upstreamSources)),
		PolledAt:  time.Now().UTC(),
	}
	for _, src := range upstreamSources {
		p, err := s.fetchProvider(ctx, src)
		if err != nil {
			s.log.Warn("upstream status fetch failed", "provider", src.Key, "err", err.Error())
			p = UpstreamProvider{
				Key:        src.Key,
				Name:       src.Name,
				PageURL:    src.PageURL,
				State:      UpstreamUnknown,
				Components: []UpstreamComponent{},
				Incidents:  []UpstreamIncident{},
				Error:      "fetch failed",
			}
		}
		snap.Providers = append(snap.Providers, p)
	}
	blob, err := json.Marshal(snap)
	if err != nil {
		s.log.Error("upstream status marshal failed", "err", err.Error())
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.d.Redis.Set(wctx, cache.KeyUpstreamStatus, blob, upstreamRedisTTL).Err(); err != nil {
		s.log.Warn("upstream status store failed", "err", err.Error())
	}
}

func (s *UpstreamStatusSvc) fetchProvider(ctx context.Context, src upstreamSource) (UpstreamProvider, error) {
	if src.IncidentsURL != "" {
		return s.fetchOpenAI(ctx, src)
	}
	return s.fetchStatuspage(ctx, src)
}

// fetchStatuspage reads a genuine Atlassian Statuspage, where summary.json
// carries everything in one call: the indicator, every component, and the
// unresolved incidents.
func (s *UpstreamStatusSvc) fetchStatuspage(ctx context.Context, src upstreamSource) (UpstreamProvider, error) {
	var sum spSummary
	if err := s.getJSON(ctx, src.SummaryURL, &sum); err != nil {
		return UpstreamProvider{}, err
	}
	now := time.Now().UTC()
	return UpstreamProvider{
		Key:         src.Key,
		Name:        src.Name,
		PageURL:     src.PageURL,
		State:       indicatorState(sum.Status.Indicator),
		Description: sum.Status.Description,
		Components:  degradedComponents(sum.Components),
		Incidents:   openIncidents(sum.Incidents, src.PageURL),
		FetchedAt:   &now,
	}, nil
}

// fetchOpenAI works around OpenAI's partial Statuspage emulation. Their page
// is incident.io behind the Statuspage URL shape, and it diverges in three
// ways that each cost a debugging session if assumed away:
//
//  1. summary.json has no "incidents" key at all, so the incident feed
//     needs a second request.
//  2. /api/v2/incidents/unresolved.json — the endpoint that would make that
//     second request cheap — returns 404 with a 64KB HTML error page. We
//     pull the full feed and filter locally instead.
//  3. Components are flat, with group always null. Two of them are both
//     named "Login" (ChatGPT's and the API's) with nothing to tell them
//     apart, which is one more reason the component list only surfaces
//     during an actual incident.
//
// The incident feed failing is not fatal: an indicator with no detail still
// tells a reader more than a blank panel does.
func (s *UpstreamStatusSvc) fetchOpenAI(ctx context.Context, src upstreamSource) (UpstreamProvider, error) {
	var sum spSummary
	if err := s.getJSON(ctx, src.SummaryURL, &sum); err != nil {
		return UpstreamProvider{}, err
	}
	now := time.Now().UTC()
	out := UpstreamProvider{
		Key:         src.Key,
		Name:        src.Name,
		PageURL:     src.PageURL,
		State:       indicatorState(sum.Status.Indicator),
		Description: sum.Status.Description,
		Components:  degradedComponents(sum.Components),
		Incidents:   []UpstreamIncident{},
		FetchedAt:   &now,
	}
	var feed spIncidentFeed
	if err := s.getJSON(ctx, src.IncidentsURL, &feed); err != nil {
		s.log.Warn("openai incident feed failed, keeping indicator only", "err", err.Error())
		return out, nil
	}
	// The feed is every recent incident, mostly resolved; openIncidents
	// keeps only the live ones.
	out.Incidents = openIncidents(feed.Incidents, src.PageURL)
	return out, nil
}

func (s *UpstreamStatusSvc) getJSON(ctx context.Context, url string, dst any) error {
	rctx, cancel := context.WithTimeout(ctx, upstreamFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "status-mirror/1.0")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, upstreamMaxBody))
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, upstreamMaxBody)).Decode(dst)
}

// --- vendor payload shapes -------------------------------------------------

type spSummary struct {
	Status struct {
		Indicator   string `json:"indicator"`
		Description string `json:"description"`
	} `json:"status"`
	Components []spComponent `json:"components"`
	// Incidents is absent on OpenAI's page; nil there, unresolved-only on
	// a real Statuspage.
	Incidents []spIncident `json:"incidents"`
}

type spComponent struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Group marks a Statuspage container row rather than a real subsystem.
	// OpenAI sends null here, which leaves this false — correct, since
	// their list is flat.
	Group bool `json:"group"`
}

type spIncident struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Impact string `json:"impact"`
	// StartedAt is Statuspage-only; OpenAI omits it.
	StartedAt *time.Time `json:"started_at"`
	CreatedAt time.Time  `json:"created_at"`
	// Shortlink is Statuspage-only (stspg.io). Empty for OpenAI.
	Shortlink       string             `json:"shortlink"`
	IncidentUpdates []spIncidentUpdate `json:"incident_updates"`
}

type spIncidentUpdate struct {
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type spIncidentFeed struct {
	Incidents []spIncident `json:"incidents"`
}

// --- normalization ---------------------------------------------------------

// indicatorState maps the vendor's top-level indicator. Both pages use the
// same vocabulary here even though the implementations differ.
func indicatorState(indicator string) UpstreamState {
	switch indicator {
	case "none":
		return UpstreamOperational
	case "minor":
		return UpstreamDegraded
	case "major", "critical":
		return UpstreamOutage
	case "maintenance":
		return UpstreamMaintenance
	default:
		// An indicator we've never seen is not a reason to claim health.
		return UpstreamUnknown
	}
}

// degradedComponents keeps only subsystems that are currently not
// operational, dropping Statuspage's group header rows. See the
// UpstreamComponent doc for why the healthy ones never ship.
func degradedComponents(in []spComponent) []UpstreamComponent {
	out := make([]UpstreamComponent, 0, 4)
	for _, c := range in {
		if c.Group || c.Status == "" || c.Status == "operational" {
			continue
		}
		out = append(out, UpstreamComponent{Name: c.Name, Status: c.Status})
	}
	return out
}

// openIncidents keeps the live incidents and picks the newest update body
// from each. Statuspage's summary feed is already unresolved-only, so the
// filter is a no-op there and load-bearing for OpenAI's full feed.
func openIncidents(in []spIncident, pageURL string) []UpstreamIncident {
	out := make([]UpstreamIncident, 0, 2)
	for _, inc := range in {
		if inc.Status == "resolved" || inc.Status == "postmortem" {
			continue
		}
		started := inc.CreatedAt
		if inc.StartedAt != nil && !inc.StartedAt.IsZero() {
			started = *inc.StartedAt
		}
		url := inc.Shortlink
		if url == "" {
			url = pageURL
		}
		out = append(out, UpstreamIncident{
			ID:           inc.ID,
			Name:         inc.Name,
			Status:       inc.Status,
			Impact:       inc.Impact,
			StartedAt:    started.UTC(),
			LatestUpdate: latestUpdateBody(inc.IncidentUpdates),
			URL:          url,
		})
	}
	return out
}

// latestUpdateBody returns the most recent update text. Both vendors happen
// to send updates newest-first, but that is not a documented guarantee, so
// this compares timestamps rather than trusting the order.
func latestUpdateBody(updates []spIncidentUpdate) string {
	var (
		best     string
		bestTime time.Time
	)
	for _, u := range updates {
		if best == "" || u.CreatedAt.After(bestTime) {
			best, bestTime = u.Body, u.CreatedAt
		}
	}
	return best
}

// readUpstream loads the mirror for embedding in PublicStatus. Every failure
// mode — Redis down, key missing, snapshot too old — resolves to unknown
// providers rather than an error, because the vendor mirror must never be
// able to take down our own status page.
func readUpstream(ctx context.Context, d *data.Data) *UpstreamSnapshot {
	rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	blob, err := d.Redis.Get(rctx, cache.KeyUpstreamStatus).Bytes()
	if err != nil {
		return unknownUpstream(time.Now().UTC())
	}
	var snap UpstreamSnapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		slog.Default().Warn("upstream status unmarshal failed", "err", err.Error())
		return unknownUpstream(time.Now().UTC())
	}
	if time.Since(snap.PolledAt) > upstreamStaleAfter {
		// The poller is wedged or the leader is gone. Whatever the
		// providers said last is no longer evidence about now.
		return unknownUpstream(snap.PolledAt)
	}
	return &snap
}

// unknownUpstream renders the "we don't know" shape, preserving the source
// list so the page keeps its layout instead of collapsing to nothing.
func unknownUpstream(polledAt time.Time) *UpstreamSnapshot {
	out := &UpstreamSnapshot{
		Providers: make([]UpstreamProvider, 0, len(upstreamSources)),
		PolledAt:  polledAt,
	}
	for _, src := range upstreamSources {
		out.Providers = append(out.Providers, UpstreamProvider{
			Key:        src.Key,
			Name:       src.Name,
			PageURL:    src.PageURL,
			State:      UpstreamUnknown,
			Components: []UpstreamComponent{},
			Incidents:  []UpstreamIncident{},
			Error:      "unavailable",
		})
	}
	return out
}
