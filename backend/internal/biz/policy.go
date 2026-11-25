// Package biz — policy moderation service.
//
// PolicySvc owns the runtime Aho-Corasick automaton that scans inbound
// requests for keywords. The automaton is rebuilt every time a keyword
// or category mutates (CRUD methods call rebuild() internally) and the
// active pointer is swapped atomically so in-flight scans never see a
// half-built state.
//
// Scan throughput at idle: ~600 MB/s – 1 GB/s on a single core with a
// few-thousand-entry dictionary; per-request overhead is sub-millisecond
// for the kilobyte-class payloads we see on /v1/chat/completions.
package biz

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	ahocorasick "github.com/petar-dambovaliev/aho-corasick"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entallow "github.com/jucode/jucode/internal/data/ent/policyallowphrase"
	entcat "github.com/jucode/jucode/internal/data/ent/policycategory"
	entkw "github.com/jucode/jucode/internal/data/ent/policykeyword"
)

var (
	ErrPolicyCategoryNotFound    = errors.New("policy category not found")
	ErrPolicyKeywordNotFound     = errors.New("policy keyword not found")
	ErrPolicyDuplicate           = errors.New("policy keyword already exists in this category")
	ErrPolicyAllowPhraseNotFound = errors.New("policy allow phrase not found")
)

// PolicyMatch is one hit returned by Scan. The same offset may produce
// multiple matches (e.g. "drug" + "drugs") — we collapse them at the
// caller so the user incident records the longest one.
type PolicyMatch struct {
	Pattern    string
	KeywordID  uuid.UUID // policy_keywords.id of the matched entry
	CategoryID uuid.UUID
	Category   string // human-readable group name (legacy field name)
	Slug       string // group slug
	Severity   string // resolved severity (keyword override or group default)
	Action     string // "block" | "log_only"
	Scope      string // group scope at the time of match
}

// PolicyScanResult summarizes a Scan() call.
//
// Hits are exact/normalized matches (block path). Suspects are softer
// signals — matches found only after aggressive normalization (separator
// folding, leetspeak) or pinyin transliteration. Suspects never hard-block
// on their own; the gateway routes them to async LLM review instead, which
// keeps the obfuscation-coverage gains from inflating the user-facing
// false-positive rate.
type PolicyScanResult struct {
	Hits      []PolicyMatch
	Suspects  []PolicyMatch
	HasBlock  bool // true if at least one matched category has default_action=block
	Truncated bool // true if Scan stopped early after exceeding max_hits
}

// policyTable is the immutable snapshot the scanner consults. Stored
// behind an atomic.Pointer so reads are lock-free and writes (during
// CRUD reload) are O(1).
type policyTable struct {
	ac ahocorasick.AhoCorasick
	// patterns is parallel-indexed to the automaton's pattern id space —
	// we look up the metadata for a hit via patterns[match.Pattern()].
	patterns []policyEntry

	// regexes are evaluated separately from the automaton (block path).
	regexes []regexEntry

	// Soft-signal pinyin automaton, parallel-indexed to pyEntries. Built
	// only from CJK keywords; empty when the dictionary has none.
	acPinyin  ahocorasick.AhoCorasick
	pyEntries []policyEntry
	hasPinyin bool

	// Allow-phrase automaton: spans matched here suppress overlapping
	// keyword hits. Empty when no allow phrases are configured.
	allowAC  ahocorasick.AhoCorasick
	hasAllow bool
}

// allowSpans returns every allow-phrase span present in the normalized text.
func (t *policyTable) allowSpans(normalized string) []span {
	if !t.hasAllow {
		return nil
	}
	var out []span
	iter := t.allowAC.IterOverlappingByte([]byte(normalized))
	for m := iter.Next(); m != nil; m = iter.Next() {
		out = append(out, span{m.Start(), m.End()})
	}
	return out
}

type policyEntry struct {
	pattern    string
	keywordID  uuid.UUID
	categoryID uuid.UUID
	category   string
	slug       string
	severity   string
	action     string
	scope      string // "input_only" | "input_output" | "input_output_tools"
	matchMode  string // "substring" | "word" | "regex"
}

// regexEntry pairs a compiled RE2 pattern with its metadata. Regex keywords
// live outside the Aho-Corasick automaton and are evaluated sequentially, so
// operators should keep them few.
type regexEntry struct {
	re    *regexp.Regexp
	entry policyEntry
}

// span is a [start,end) byte range within the normalized text, used for
// allow-phrase suppression and boundary checks.
type span struct{ start, end int }

// Scope* constants mirror the ent enum values so callers don't have
// to import the ent package.
const (
	ScopeInputOnly        = "input_only"
	ScopeInputOutput      = "input_output"
	ScopeInputOutputTools = "input_output_tools"
)

const policyMaxHits = 64

type PolicySvc struct {
	d      *data.Data
	active atomic.Pointer[policyTable]
	mu     sync.Mutex // serialises rebuilds
}

func NewPolicySvc(d *data.Data) *PolicySvc {
	return &PolicySvc{d: d}
}

// Bootstrap loads the dictionary at process start. Safe to call again
// to force a reload after out-of-band DB edits.
func (s *PolicySvc) Bootstrap(ctx context.Context) error {
	return s.rebuild(ctx)
}

// Scan walks text once through the AC automaton. Empty dictionary →
// always returns an empty result, no allocation. Case-folded scan so
// keywords stored as "drug" match "Drug" / "DRUG".
func (s *PolicySvc) Scan(text string) PolicyScanResult {
	t := s.active.Load()
	if t == nil || text == "" || (len(t.patterns) == 0 && len(t.regexes) == 0) {
		return PolicyScanResult{}
	}
	norm := NormalizeExact(text)
	allow := t.allowSpans(norm)

	// Deduplicate by category to keep the incident record concise. The
	// same category firing twice on the same request is one incident.
	// IterOverlapping reports every keyword present even when one keyword
	// is a substring of another, so we don't miss a category whose
	// pattern overlaps a longer hit.
	seen := make(map[uuid.UUID]struct{}, 4)        // categories that hard-hit
	suspectSeen := make(map[uuid.UUID]struct{}, 4) // categories that soft-hit
	out := PolicyScanResult{}

	// --- block path: AC over the canonical form, with per-keyword word
	// boundaries and allow-phrase suppression. ---
	if len(t.patterns) > 0 {
		iter := t.ac.IterOverlappingByte([]byte(norm))
		for match := iter.Next(); match != nil; match = iter.Next() {
			id := match.Pattern()
			if id < 0 || id >= len(t.patterns) {
				continue
			}
			e := t.patterns[id]
			if e.matchMode == string(entkw.MatchModeWord) && !wordBoundaryOK(norm, match.Start(), match.End()) {
				continue
			}
			if spanCovered(allow, match.Start(), match.End()) {
				continue
			}
			if _, ok := seen[e.categoryID]; ok {
				continue
			}
			seen[e.categoryID] = struct{}{}
			out.Hits = append(out.Hits, matchFrom(e))
			if e.action == "block" {
				out.HasBlock = true
			}
			if len(out.Hits) >= policyMaxHits {
				out.Truncated = true
				return out
			}
		}
	}

	// --- regex path (block-eligible, evaluated over the canonical form). ---
	for _, rx := range t.regexes {
		if _, ok := seen[rx.entry.categoryID]; ok {
			continue
		}
		loc := rx.re.FindStringIndex(norm)
		if loc == nil || spanCovered(allow, loc[0], loc[1]) {
			continue
		}
		seen[rx.entry.categoryID] = struct{}{}
		out.Hits = append(out.Hits, matchFrom(rx.entry))
		if rx.entry.action == "block" {
			out.HasBlock = true
		}
	}

	// --- soft path: aggressive normalization (separator/leet folding) +
	// pinyin. Hits not already seen on the block path become Suspects, which
	// feed async LLM review rather than blocking. ---
	if aggr := NormalizeAggressive(norm); aggr != "" && len(t.patterns) > 0 {
		it := t.ac.IterOverlappingByte([]byte(aggr))
		for match := it.Next(); match != nil; match = it.Next() {
			id := match.Pattern()
			if id < 0 || id >= len(t.patterns) {
				continue
			}
			addSuspect(&out, seen, suspectSeen, t.patterns[id])
		}
	}
	if t.hasPinyin {
		if py := ToPinyin(norm); py != "" {
			it := t.acPinyin.IterOverlappingByte([]byte(py))
			for match := it.Next(); match != nil; match = it.Next() {
				id := match.Pattern()
				if id < 0 || id >= len(t.pyEntries) {
					continue
				}
				addSuspect(&out, seen, suspectSeen, t.pyEntries[id])
			}
		}
	}
	return out
}

// addSuspect records a soft-signal hit for a category that has not already
// produced a hard hit or an earlier suspect.
func addSuspect(out *PolicyScanResult, seen, suspectSeen map[uuid.UUID]struct{}, e policyEntry) {
	if _, ok := seen[e.categoryID]; ok {
		return
	}
	if _, ok := suspectSeen[e.categoryID]; ok {
		return
	}
	suspectSeen[e.categoryID] = struct{}{}
	out.Suspects = append(out.Suspects, matchFrom(e))
}

func matchFrom(e policyEntry) PolicyMatch {
	return PolicyMatch{
		Pattern:    e.pattern,
		KeywordID:  e.keywordID,
		CategoryID: e.categoryID,
		Category:   e.category,
		Slug:       e.slug,
		Severity:   e.severity,
		Action:     e.action,
		Scope:      e.scope,
	}
}

// spanCovered reports whether [start,end) lies entirely within any span.
func spanCovered(spans []span, start, end int) bool {
	for _, sp := range spans {
		if start >= sp.start && end <= sp.end {
			return true
		}
	}
	return false
}

// wordBoundaryOK reports whether the match [start,end) in s is flanked by
// non-word runes (or string edges) on both sides — the test that turns a
// substring match into a whole-word match.
func wordBoundaryOK(s string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:start])
		if isWordRune(r) {
			return false
		}
	}
	if end < len(s) {
		r, _ := utf8.DecodeRuneInString(s[end:])
		if isWordRune(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// BlockDecision grades the exact hits into a final block verdict. To curb
// user-facing false positives, a single low/medium-severity keyword no longer
// hard-blocks on its own: a block requires either a high+ severity block hit
// OR at least two distinct block categories firing together. Lower-confidence
// single hits still get recorded (action=logged) and can drive sanctions once
// LLM review confirms them.
func (r PolicyScanResult) BlockDecision() (block bool, topSeverity string) {
	topSeverity = "low"
	blockCats := 0
	hasHigh := false
	for _, h := range r.Hits {
		if severityRankBiz(h.Severity) > severityRankBiz(topSeverity) {
			topSeverity = h.Severity
		}
		if h.Action == "block" {
			blockCats++
			if severityRankBiz(h.Severity) >= severityRankBiz("high") {
				hasHigh = true
			}
		}
	}
	return hasHigh || blockCats >= 2, topSeverity
}

func severityRankBiz(s string) int {
	switch s {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "critical":
		return 4
	}
	return 0
}

// rebuild snapshots every enabled keyword + its category metadata and
// installs a fresh automaton. Disabled keywords / disabled categories
// are skipped — toggling enabled is the operator's "pause" switch.
func (s *PolicySvc) rebuild(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cats, err := s.d.Ent.PolicyCategory.Query().Where(entcat.EnabledEQ(true)).All(ctx)
	if err != nil {
		return err
	}
	catByID := make(map[uuid.UUID]*ent.PolicyCategory, len(cats))
	for _, c := range cats {
		catByID[c.ID] = c
	}
	kws, err := s.d.Ent.PolicyKeyword.Query().Where(entkw.EnabledEQ(true)).All(ctx)
	if err != nil {
		return err
	}
	patterns := make([][]byte, 0, len(kws))
	entries := make([]policyEntry, 0, len(kws))
	var pyPatterns [][]byte
	var pyEntries []policyEntry
	var regexes []regexEntry
	for _, k := range kws {
		cat, ok := catByID[k.CategoryID]
		if !ok {
			continue // category disabled — skip
		}
		sev := string(cat.DefaultSeverity)
		if k.Severity != nil {
			sev = string(*k.Severity)
		}
		e := policyEntry{
			pattern:    k.Pattern,
			keywordID:  k.ID,
			categoryID: cat.ID,
			category:   cat.Name,
			slug:       cat.Slug,
			severity:   sev,
			action:     string(cat.DefaultAction),
			scope:      string(cat.Scope),
			matchMode:  string(k.MatchMode),
		}
		if k.MatchMode == entkw.MatchModeRegex {
			re, cErr := regexp.Compile("(?i)" + k.Pattern)
			if cErr != nil {
				slog.Warn("policy.regex_invalid", "pattern", k.Pattern, "err", cErr.Error())
				continue
			}
			regexes = append(regexes, regexEntry{re: re, entry: e})
			continue
		}
		patterns = append(patterns, []byte(NormalizeExact(k.Pattern)))
		entries = append(entries, e)
		// CJK keywords also get a pinyin entry for the soft (suspect) path.
		if HasHan(k.Pattern) {
			if py := ToPinyin(NormalizeExact(k.Pattern)); py != "" {
				pyPatterns = append(pyPatterns, []byte(py))
				pyEntries = append(pyEntries, e)
			}
		}
	}

	// Allow phrases (suppression list) — normalized the same way as text.
	aps, err := s.d.Ent.PolicyAllowPhrase.Query().Where(entallow.EnabledEQ(true)).All(ctx)
	if err != nil {
		return err
	}
	allowPats := make([][]byte, 0, len(aps))
	for _, a := range aps {
		if p := NormalizeExact(a.Phrase); p != "" {
			allowPats = append(allowPats, []byte(p))
		}
	}

	newBuilder := func() ahocorasick.AhoCorasickBuilder {
		return ahocorasick.NewAhoCorasickBuilder(ahocorasick.Opts{
			MatchKind: ahocorasick.StandardMatch,
			DFA:       true,
		})
	}
	tbl := &policyTable{patterns: entries, pyEntries: pyEntries, regexes: regexes}
	if len(patterns) > 0 {
		b := newBuilder()
		tbl.ac = b.BuildByte(patterns)
	}
	if len(pyPatterns) > 0 {
		b := newBuilder()
		tbl.acPinyin = b.BuildByte(pyPatterns)
		tbl.hasPinyin = true
	}
	if len(allowPats) > 0 {
		b := newBuilder()
		tbl.allowAC = b.BuildByte(allowPats)
		tbl.hasAllow = true
	}
	s.active.Store(tbl)
	return nil
}

// --- CRUD ---

type CategoryInput struct {
	Name            string
	Slug            string
	Description     string
	DefaultAction   string // "block" | "log_only"
	DefaultSeverity string
	Scope           string // "input_only" | "input_output" | "input_output_tools"
	Enabled         *bool
	SortOrder       int
}

func (s *PolicySvc) ListCategories(ctx context.Context) ([]*ent.PolicyCategory, error) {
	return s.d.Ent.PolicyCategory.Query().
		Order(ent.Asc(entcat.FieldSortOrder), ent.Asc(entcat.FieldName)).
		All(ctx)
}

func (s *PolicySvc) CreateCategory(ctx context.Context, in CategoryInput) (*ent.PolicyCategory, error) {
	q := s.d.Ent.PolicyCategory.Create().
		SetName(in.Name).
		SetSlug(in.Slug).
		SetDescription(in.Description).
		SetSortOrder(in.SortOrder)
	if in.DefaultAction != "" {
		q = q.SetDefaultAction(entcat.DefaultAction(in.DefaultAction))
	}
	if in.DefaultSeverity != "" {
		q = q.SetDefaultSeverity(entcat.DefaultSeverity(in.DefaultSeverity))
	}
	if in.Scope != "" {
		q = q.SetScope(entcat.Scope(in.Scope))
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	c, err := q.Save(ctx)
	if err != nil {
		return nil, err
	}
	_ = s.rebuild(ctx)
	return c, nil
}

func (s *PolicySvc) UpdateCategory(ctx context.Context, id uuid.UUID, in CategoryInput) (*ent.PolicyCategory, error) {
	q := s.d.Ent.PolicyCategory.UpdateOneID(id)
	if in.Name != "" {
		q = q.SetName(in.Name)
	}
	if in.Slug != "" {
		q = q.SetSlug(in.Slug)
	}
	if in.DefaultAction != "" {
		q = q.SetDefaultAction(entcat.DefaultAction(in.DefaultAction))
	}
	if in.DefaultSeverity != "" {
		q = q.SetDefaultSeverity(entcat.DefaultSeverity(in.DefaultSeverity))
	}
	if in.Scope != "" {
		q = q.SetScope(entcat.Scope(in.Scope))
	}
	q = q.SetDescription(in.Description).SetSortOrder(in.SortOrder)
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	c, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrPolicyCategoryNotFound
		}
		return nil, err
	}
	_ = s.rebuild(ctx)
	return c, nil
}

func (s *PolicySvc) DeleteCategory(ctx context.Context, id uuid.UUID) error {
	// Cascade-delete keywords in this category. ent doesn't support
	// ON DELETE CASCADE without an explicit annotation, so do it
	// manually in one txn.
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.PolicyKeyword.Delete().Where(entkw.CategoryIDEQ(id)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.PolicyCategory.DeleteOneID(id).Exec(ctx); err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return ErrPolicyCategoryNotFound
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = s.rebuild(ctx)
	return nil
}

type KeywordInput struct {
	CategoryID uuid.UUID
	Pattern    string
	Severity   string // "" = inherit category default
	MatchMode  string // "" = keep/default substring; substring|word|regex
	Note       string
	Enabled    *bool
}

func (s *PolicySvc) ListKeywords(ctx context.Context, categoryID uuid.UUID, q string, limit, offset int) ([]*ent.PolicyKeyword, int, error) {
	query := s.d.Ent.PolicyKeyword.Query()
	if categoryID != uuid.Nil {
		query = query.Where(entkw.CategoryIDEQ(categoryID))
	}
	if q = strings.TrimSpace(q); q != "" {
		query = query.Where(entkw.PatternContainsFold(q))
	}
	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := query.
		Order(ent.Asc(entkw.FieldPattern)).
		Limit(limit).Offset(offset).
		All(ctx)
	return rows, total, err
}

func (s *PolicySvc) CreateKeyword(ctx context.Context, in KeywordInput) (*ent.PolicyKeyword, error) {
	pat := strings.TrimSpace(in.Pattern)
	if pat == "" {
		return nil, errors.New("pattern required")
	}
	if in.MatchMode == string(entkw.MatchModeRegex) {
		if _, cErr := regexp.Compile(pat); cErr != nil {
			return nil, errors.New("invalid regex: " + cErr.Error())
		}
	}
	q := s.d.Ent.PolicyKeyword.Create().
		SetCategoryID(in.CategoryID).
		SetPattern(pat).
		SetNote(in.Note)
	if in.Severity != "" {
		q = q.SetSeverity(entkw.Severity(in.Severity))
	}
	if in.MatchMode != "" {
		q = q.SetMatchMode(entkw.MatchMode(in.MatchMode))
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	k, err := q.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, ErrPolicyDuplicate
		}
		return nil, err
	}
	_ = s.rebuild(ctx)
	return k, nil
}

func (s *PolicySvc) UpdateKeyword(ctx context.Context, id uuid.UUID, in KeywordInput) (*ent.PolicyKeyword, error) {
	q := s.d.Ent.PolicyKeyword.UpdateOneID(id)
	if in.Pattern != "" {
		q = q.SetPattern(strings.TrimSpace(in.Pattern))
	}
	if in.CategoryID != uuid.Nil {
		q = q.SetCategoryID(in.CategoryID)
	}
	if in.Severity == "" {
		q = q.ClearSeverity()
	} else {
		q = q.SetSeverity(entkw.Severity(in.Severity))
	}
	if in.MatchMode != "" {
		if in.MatchMode == string(entkw.MatchModeRegex) && strings.TrimSpace(in.Pattern) != "" {
			if _, cErr := regexp.Compile(strings.TrimSpace(in.Pattern)); cErr != nil {
				return nil, errors.New("invalid regex: " + cErr.Error())
			}
		}
		q = q.SetMatchMode(entkw.MatchMode(in.MatchMode))
	}
	q = q.SetNote(in.Note)
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	k, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrPolicyKeywordNotFound
		}
		return nil, err
	}
	_ = s.rebuild(ctx)
	return k, nil
}

func (s *PolicySvc) DeleteKeyword(ctx context.Context, id uuid.UUID) error {
	if err := s.d.Ent.PolicyKeyword.DeleteOneID(id).Exec(ctx); err != nil {
		if ent.IsNotFound(err) {
			return ErrPolicyKeywordNotFound
		}
		return err
	}
	_ = s.rebuild(ctx)
	return nil
}

// --- allow-phrase CRUD ---

type AllowPhraseInput struct {
	Phrase  string
	Note    string
	Enabled *bool
}

func (s *PolicySvc) ListAllowPhrases(ctx context.Context) ([]*ent.PolicyAllowPhrase, error) {
	return s.d.Ent.PolicyAllowPhrase.Query().Order(ent.Asc(entallow.FieldPhrase)).All(ctx)
}

func (s *PolicySvc) CreateAllowPhrase(ctx context.Context, in AllowPhraseInput) (*ent.PolicyAllowPhrase, error) {
	ph := strings.TrimSpace(in.Phrase)
	if ph == "" {
		return nil, errors.New("phrase required")
	}
	q := s.d.Ent.PolicyAllowPhrase.Create().SetPhrase(ph).SetNote(in.Note)
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	a, err := q.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return nil, ErrPolicyDuplicate
		}
		return nil, err
	}
	_ = s.rebuild(ctx)
	return a, nil
}

func (s *PolicySvc) UpdateAllowPhrase(ctx context.Context, id uuid.UUID, in AllowPhraseInput) (*ent.PolicyAllowPhrase, error) {
	q := s.d.Ent.PolicyAllowPhrase.UpdateOneID(id)
	if ph := strings.TrimSpace(in.Phrase); ph != "" {
		q = q.SetPhrase(ph)
	}
	q = q.SetNote(in.Note)
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	a, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrPolicyAllowPhraseNotFound
		}
		return nil, err
	}
	_ = s.rebuild(ctx)
	return a, nil
}

func (s *PolicySvc) DeleteAllowPhrase(ctx context.Context, id uuid.UUID) error {
	if err := s.d.Ent.PolicyAllowPhrase.DeleteOneID(id).Exec(ctx); err != nil {
		if ent.IsNotFound(err) {
			return ErrPolicyAllowPhraseNotFound
		}
		return err
	}
	_ = s.rebuild(ctx)
	return nil
}

// ImportResult is per-line outcome from a bulk import.
type ImportResult struct {
	Pattern string `json:"pattern"`
	Created bool   `json:"created"`
	Error   string `json:"error,omitempty"`
}

// ImportKeywords accepts a newline-separated list of patterns and inserts
// them into the given category. Lines that already exist or fail
// validation are reported back individually so the operator can see what
// landed and what didn't.
func (s *PolicySvc) ImportKeywords(ctx context.Context, categoryID uuid.UUID, raw string) ([]ImportResult, error) {
	if _, err := s.d.Ent.PolicyCategory.Get(ctx, categoryID); err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrPolicyCategoryNotFound
		}
		return nil, err
	}
	lines := strings.Split(raw, "\n")
	out := make([]ImportResult, 0, len(lines))
	for _, ln := range lines {
		// Strip a stray UTF-8 BOM the operator may have left from
		// copying a Notepad-saved word list. TrimPrefix here keeps the
		// loop O(n) instead of normalising the whole input twice.
		pat := strings.TrimSpace(strings.TrimPrefix(ln, "\xef\xbb\xbf"))
		if pat == "" || strings.HasPrefix(pat, "#") {
			continue
		}
		// upsert: create, swallow duplicate-key as "already there"
		_, err := s.d.Ent.PolicyKeyword.Create().
			SetCategoryID(categoryID).
			SetPattern(pat).
			Save(ctx)
		switch {
		case err == nil:
			out = append(out, ImportResult{Pattern: pat, Created: true})
		case ent.IsConstraintError(err):
			out = append(out, ImportResult{Pattern: pat, Created: false, Error: "duplicate"})
		default:
			out = append(out, ImportResult{Pattern: pat, Created: false, Error: err.Error()})
		}
	}
	_ = s.rebuild(ctx)
	return out, nil
}

// Stats returns a snapshot summary used by the admin dashboard.
type PolicyStats struct {
	Categories int `json:"categories"`
	Keywords   int `json:"keywords"`
	Active     int `json:"active"` // enabled and in an enabled category
}

// --- streaming scanner (output / tool-call moderation) ---

// StreamScanner is a stateful scanner for the SSE output path. It owns
// a sliding-window buffer so a keyword that straddles two SSE frames
// is still caught. Concurrency: one scanner per request, NOT shared.
//
// Scope semantics — the gateway calls Feed only when the active scope
// of any enabled group covers the kind of text being fed:
//
//   - assistant text deltas: fed when ANY enabled group has scope
//     input_output or input_output_tools
//   - tool_use.input args / tool_result.content: fed when ANY enabled
//     group has scope input_output_tools
//
// Each Feed returns at most one hit per call (the first match in the
// fresh bytes); the caller short-circuits the stream on a block hit
// rather than scanning the entire payload.
type StreamScanner struct {
	svc    *PolicySvc
	buffer []byte // tail of previously-fed text, up to maxKeywordLen-1 bytes
	maxKw  int    // longest pattern length, for buffer trimming
}

// NewStreamScanner snapshots the current automaton. If the dictionary
// is empty it still returns a non-nil scanner whose Feed is a no-op,
// so callers don't have to nil-check.
func (s *PolicySvc) NewStreamScanner() *StreamScanner {
	t := s.active.Load()
	maxKw := 0
	if t != nil {
		for _, e := range t.patterns {
			if l := len(e.pattern); l > maxKw {
				maxKw = l
			}
		}
	}
	return &StreamScanner{svc: s, maxKw: maxKw}
}

// HasAnyScope returns true if any active group's scope falls in the
// given set. The gateway uses this to skip the SSE hook entirely when
// no output scanning is configured — zero overhead on the default
// path.
func (s *PolicySvc) HasAnyScope(scopes ...string) bool {
	t := s.active.Load()
	if t == nil {
		return false
	}
	want := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		want[sc] = struct{}{}
	}
	for _, e := range t.patterns {
		if _, ok := want[e.scope]; ok {
			return true
		}
	}
	return false
}

// Feed scans the concatenation of buffer + chunk. Returns the first
// match whose group's scope includes scopeFilter (the gateway passes
// the scope appropriate to the current frame kind), or nil hit when
// nothing matched in the new bytes.
//
// scopeFilter values: "input_output" for assistant text, or
// "input_output_tools" for tool args/results. A group with scope
// "input_output_tools" also matches "input_output" frames.
func (sc *StreamScanner) Feed(chunk []byte, scopeFilter string) *PolicyMatch {
	if sc == nil || sc.svc == nil || len(chunk) == 0 {
		return nil
	}
	t := sc.svc.active.Load()
	if t == nil || len(t.patterns) == 0 {
		return nil
	}

	// Lower-case the input lazily — the dictionary itself is lower-cased
	// at rebuild() time.
	combined := make([]byte, 0, len(sc.buffer)+len(chunk))
	combined = append(combined, sc.buffer...)
	combined = append(combined, chunk...)
	folded := []byte(NormalizeExact(string(combined)))

	iter := t.ac.IterOverlappingByte(folded)
	var hit *PolicyMatch
	for match := iter.Next(); match != nil; match = iter.Next() {
		id := match.Pattern()
		if id < 0 || id >= len(t.patterns) {
			continue
		}
		e := t.patterns[id]
		if !scopeIncludes(e.scope, scopeFilter) {
			continue
		}
		m := PolicyMatch{
			Pattern: e.pattern, CategoryID: e.categoryID, Category: e.category,
			Slug: e.slug, Severity: e.severity, Action: e.action, Scope: e.scope,
		}
		hit = &m
		break
	}

	// Trim the buffer: retain the last maxKw-1 bytes so a keyword
	// straddling the next chunk still resolves.
	if sc.maxKw > 1 && len(combined) >= sc.maxKw {
		sc.buffer = append(sc.buffer[:0], combined[len(combined)-(sc.maxKw-1):]...)
	} else {
		sc.buffer = append(sc.buffer[:0], combined...)
	}
	return hit
}

// scopeIncludes returns true if a group's configured scope authorizes
// scanning under the given filter. input_output_tools is the superset
// (covers both text and tool frames); input_output covers only text.
func scopeIncludes(groupScope, filter string) bool {
	switch groupScope {
	case ScopeInputOutputTools:
		return filter == ScopeInputOutput || filter == ScopeInputOutputTools
	case ScopeInputOutput:
		return filter == ScopeInputOutput
	}
	return false
}

func (s *PolicySvc) Stats(ctx context.Context) (PolicyStats, error) {
	var out PolicyStats
	c, err := s.d.Ent.PolicyCategory.Query().Count(ctx)
	if err != nil {
		return out, err
	}
	out.Categories = c
	k, err := s.d.Ent.PolicyKeyword.Query().Count(ctx)
	if err != nil {
		return out, err
	}
	out.Keywords = k
	t := s.active.Load()
	if t != nil {
		out.Active = len(t.patterns)
	}
	return out, nil
}

// PolicyIncidentPayload is what a keyword-hit incident records: which
// dictionary entries and categories matched. The request text itself, and
// the keyword text found in it, are never stored.
func PolicyIncidentPayload(hits []PolicyMatch, truncated bool) map[string]any {
	ids := make([]string, 0, len(hits))
	cats := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.KeywordID != uuid.Nil {
			ids = append(ids, h.KeywordID.String())
		}
		cats = append(cats, h.Slug)
	}
	return map[string]any{
		"keyword_ids": ids,
		"categories":  cats,
		"truncated":   truncated,
	}
}
