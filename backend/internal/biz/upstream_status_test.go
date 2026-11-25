package biz

import (
	"encoding/json"
	"testing"
	"time"
)

// The fixtures below are verbatim excerpts of what the two vendors actually
// served on 2026-08-01, trimmed to the fields we read. They exist because the
// two pages agree on the URL shape but not on the payload, and every
// divergence here cost a round of manual probing to find.

// anthropicSummaryFixture is a real Statuspage summary.json: incidents ride
// along in the same document, components carry the group flag, and incidents
// carry started_at plus a stspg.io shortlink.
const anthropicSummaryFixture = `{
  "page": {"id": "tymt9n04zgry", "name": "Claude"},
  "components": [
    {"id": "rwppv331jlwc", "name": "claude.ai", "status": "operational", "group": false},
    {"id": "x5m1kzr8pnqd", "name": "Claude API (api.anthropic.com)", "status": "partial_outage", "group": false},
    {"id": "gg0z5bmt2v6k", "name": "Claude Code", "status": "degraded_performance", "group": false},
    {"id": "hdr0000group", "name": "Some Group Header", "status": "operational", "group": true}
  ],
  "incidents": [
    {
      "id": "vcynh9cf33xp",
      "name": "Elevated errors on several models",
      "status": "investigating",
      "impact": "critical",
      "created_at": "2026-07-21T15:35:03.996Z",
      "started_at": "2026-07-21T15:35:03.985Z",
      "shortlink": "https://stspg.io/74sqh6d7f9vq",
      "incident_updates": [
        {"body": "older note", "status": "investigating", "created_at": "2026-07-21T15:40:00.000Z"},
        {"body": "We are continuing to investigate.", "status": "investigating", "created_at": "2026-07-21T16:10:00.000Z"}
      ]
    }
  ],
  "status": {"indicator": "critical", "description": "Elevated errors on several models"}
}`

// openaiSummaryFixture is OpenAI's incident.io emulation. Note what is
// missing versus the fixture above: no "incidents" key at all, and every
// component reports group:null. The duplicated "Login" entry is real — one is
// ChatGPT's and one is the API's, with nothing in the payload to tell them
// apart.
const openaiSummaryFixture = `{
  "page": {"id": "01JMDK9XYNY6RXSED6SDWW50WY", "name": "OpenAI"},
  "status": {"description": "Partial outage", "indicator": "major"},
  "components": [
    {"id": "01JMXBRMFE4MAP2BHSJNZ787WX", "name": "Images", "status": "operational", "group": null},
    {"id": "01JMXBNJXG1S2D9V65P1ZZTD94", "name": "Login", "status": "operational", "group": null},
    {"id": "01JSM5RTJWHRWDTS6Q604VEW3B", "name": "Login", "status": "major_outage", "group": null},
    {"id": "01JP8CD9JR3HR6Y7G4Q75N4DVW", "name": "Responses", "status": "degraded_performance", "group": null}
  ]
}`

// openaiIncidentsFixture is the separate feed OpenAI forces us to fetch. It
// returns recent incidents regardless of state, so the resolved ones must be
// filtered client-side — /incidents/unresolved.json, which would have done it
// server-side, 404s on their page. Incidents here have no started_at and no
// shortlink.
const openaiIncidentsFixture = `{
  "page": {"id": "01JMDK9XYNY6RXSED6SDWW50WY", "name": "OpenAI"},
  "incidents": [
    {
      "id": "01KYVPNEY9736Z04D3X1NSFFF9",
      "name": "Enterprise & Education Chat Errors",
      "status": "resolved",
      "impact": "none",
      "created_at": "2026-07-31T09:04:13Z",
      "resolved_at": "2026-07-31T09:28:52Z",
      "incident_updates": [{"body": "This is now resolved.", "status": "resolved", "created_at": "2026-07-31T09:28:52Z"}]
    },
    {
      "id": "01KYVPNEY9736Z04D3X1NSFFFA",
      "name": "Elevated error rates on Responses",
      "status": "identified",
      "impact": "major",
      "created_at": "2026-08-01T10:00:00Z",
      "incident_updates": [
        {"body": "We have identified the cause.", "status": "identified", "created_at": "2026-08-01T10:20:00Z"},
        {"body": "We are investigating.", "status": "investigating", "created_at": "2026-08-01T10:05:00Z"}
      ]
    }
  ]
}`

func TestIndicatorState(t *testing.T) {
	cases := map[string]UpstreamState{
		"none":        UpstreamOperational,
		"minor":       UpstreamDegraded,
		"major":       UpstreamOutage,
		"critical":    UpstreamOutage,
		"maintenance": UpstreamMaintenance,
		// An indicator we have never seen must not be read as health. If a
		// vendor invents a new severity, the page says "unknown" and a human
		// looks, rather than quietly showing green.
		"apocalypse": UpstreamUnknown,
		"":           UpstreamUnknown,
	}
	for in, want := range cases {
		if got := indicatorState(in); got != want {
			t.Errorf("indicatorState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnthropicSummaryParsing(t *testing.T) {
	var sum spSummary
	if err := json.Unmarshal([]byte(anthropicSummaryFixture), &sum); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := indicatorState(sum.Status.Indicator); got != UpstreamOutage {
		t.Errorf("state = %q, want outage", got)
	}

	comps := degradedComponents(sum.Components)
	if len(comps) != 2 {
		t.Fatalf("degraded components = %d, want 2 (healthy rows and group headers must drop): %+v", len(comps), comps)
	}
	if comps[0].Name != "Claude API (api.anthropic.com)" || comps[0].Status != "partial_outage" {
		t.Errorf("first component = %+v, want the API row with its raw vendor status", comps[0])
	}
	for _, c := range comps {
		if c.Name == "Some Group Header" {
			t.Error("group header row leaked into components")
		}
	}

	incs := openIncidents(sum.Incidents, "https://status.claude.com")
	if len(incs) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incs))
	}
	got := incs[0]
	// started_at, not created_at: Statuspage reports the two milliseconds
	// apart and the former is the one the vendor calls the onset.
	wantStart := time.Date(2026, 7, 21, 15, 35, 3, 985000000, time.UTC)
	if !got.StartedAt.Equal(wantStart) {
		t.Errorf("started_at = %v, want %v (must prefer started_at over created_at)", got.StartedAt, wantStart)
	}
	if got.URL != "https://stspg.io/74sqh6d7f9vq" {
		t.Errorf("url = %q, want the incident shortlink", got.URL)
	}
	if got.LatestUpdate != "We are continuing to investigate." {
		t.Errorf("latest update = %q, want the newest by timestamp", got.LatestUpdate)
	}
	if got.Impact != "critical" {
		t.Errorf("impact = %q, want critical", got.Impact)
	}
}

func TestOpenAISummaryParsing(t *testing.T) {
	var sum spSummary
	if err := json.Unmarshal([]byte(openaiSummaryFixture), &sum); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The whole reason fetchOpenAI makes a second request: this key is
	// absent, and a nil slice here is what proves it rather than an error.
	if sum.Incidents != nil {
		t.Errorf("openai summary carried incidents = %+v, want none — if this passes, the second fetch is now redundant", sum.Incidents)
	}
	if got := indicatorState(sum.Status.Indicator); got != UpstreamOutage {
		t.Errorf("state = %q, want outage", got)
	}
	// group:null must decode without error and leave the row treated as a
	// real component, since OpenAI's list is flat.
	comps := degradedComponents(sum.Components)
	if len(comps) != 2 {
		t.Fatalf("degraded components = %d, want 2: %+v", len(comps), comps)
	}
	if comps[0].Name != "Login" || comps[0].Status != "major_outage" {
		t.Errorf("first component = %+v, want the broken Login row", comps[0])
	}
}

func TestOpenAIIncidentFeedFiltering(t *testing.T) {
	var feed spIncidentFeed
	if err := json.Unmarshal([]byte(openaiIncidentsFixture), &feed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(feed.Incidents) != 2 {
		t.Fatalf("fixture parsed %d incidents, want 2", len(feed.Incidents))
	}

	incs := openIncidents(feed.Incidents, "https://status.openai.com")
	if len(incs) != 1 {
		t.Fatalf("live incidents = %d, want 1 — the resolved one must be filtered here, since the vendor has no unresolved endpoint", len(incs))
	}
	got := incs[0]
	if got.Name != "Elevated error rates on Responses" {
		t.Errorf("kept the wrong incident: %q", got.Name)
	}
	// No started_at in the payload, so created_at is the only onset available.
	wantStart := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	if !got.StartedAt.Equal(wantStart) {
		t.Errorf("started_at = %v, want created_at fallback %v", got.StartedAt, wantStart)
	}
	// No shortlink either, so the link must degrade to the vendor's page
	// rather than rendering as empty.
	if got.URL != "https://status.openai.com" {
		t.Errorf("url = %q, want the page URL fallback", got.URL)
	}
	if got.LatestUpdate != "We have identified the cause." {
		t.Errorf("latest update = %q, want the newest by timestamp not by array order", got.LatestUpdate)
	}
}

// A postmortem is a write-up of something already over. Leaving it in would
// keep a banner up for days after service returned.
func TestPostmortemIsNotLive(t *testing.T) {
	in := []spIncident{{ID: "a", Name: "old", Status: "postmortem", CreatedAt: time.Now()}}
	if got := openIncidents(in, "https://example.com"); len(got) != 0 {
		t.Errorf("postmortem survived filtering: %+v", got)
	}
}

// unknownUpstream is what every failure path returns. The invariant that
// matters is that it never claims health.
func TestUnknownUpstreamNeverClaimsHealth(t *testing.T) {
	snap := unknownUpstream(time.Now().UTC())
	if len(snap.Providers) != len(upstreamSources) {
		t.Fatalf("providers = %d, want %d — the layout must not collapse on failure", len(snap.Providers), len(upstreamSources))
	}
	for _, p := range snap.Providers {
		if p.State != UpstreamUnknown {
			t.Errorf("%s state = %q, want unknown", p.Key, p.State)
		}
		if p.FetchedAt != nil {
			t.Errorf("%s carried a fetched_at with no successful fetch", p.Key)
		}
		// Non-nil empty slices so the JSON is [] rather than null and the
		// client can map over it unguarded.
		if p.Components == nil || p.Incidents == nil {
			t.Errorf("%s has nil slices; they must serialize as []", p.Key)
		}
	}
}
