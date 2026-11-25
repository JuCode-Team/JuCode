package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// WebVital captures a single Core Web Vital measurement reported from
// the browser via the web-vitals library. Each row is one metric
// emission (LCP, CLS, INP, FCP, TTFB), tagged with the URL the user
// was on. We keep things append-only — aggregation (p50/p75/p95) is
// computed at query time over rolling windows.
//
// Volume estimate: ~5 metrics × N page-loads/day. At ~1 KB per row and
// 10k page-loads/day → 50 MB/month, easy to vacuum or down-sample.
type WebVital struct{ ent.Schema }

func (WebVital) Mixin() []ent.Mixin { return []ent.Mixin{BaseMixin{}} }

func (WebVital) Fields() []ent.Field {
	return []ent.Field{
		// "LCP" / "CLS" / "INP" / "FCP" / "TTFB"
		field.String("name").NotEmpty(),
		// Raw metric value. Units differ per metric (ms for LCP/INP/FCP/TTFB,
		// unitless for CLS); the name field disambiguates.
		field.Float("value"),
		// Web-vitals rating bucket: "good" / "needs-improvement" / "poor".
		// Calibrated per metric by the library itself.
		field.String("rating").Default(""),
		// Pathname only — querystrings are stripped client-side to avoid
		// cardinality blow-up and accidental PII leakage.
		field.String("path").Default(""),
		// "navigate" / "reload" / "back-forward" / "back-forward-cache" /
		// "restore" / "prerender" — useful when comparing first-visit vs
		// repeat-visit LCP since back-forward cache typically hits 0 ms.
		field.String("nav_type").Default(""),
		// Truncated UA string for rough device-class slicing without
		// shipping a full UA parser. 200-char cap keeps storage bounded.
		field.String("ua").Default("").MaxLen(220),
		// Coarse session correlator — random per-session ID generated
		// client-side. NOT a user ID; lets us correlate multiple metrics
		// from the same page load without holding identity.
		field.String("session").Default("").MaxLen(36),
	}
}

func (WebVital) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		// Aggregation by metric over time windows is the main query.
		index.Fields("name", "created_at"),
		// Per-path drilldown ("which page has the worst LCP?").
		index.Fields("path", "created_at"),
	}
}
