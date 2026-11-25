package biz

import (
	"regexp"
	"testing"

	"github.com/google/uuid"
	ahocorasick "github.com/petar-dambovaliev/aho-corasick"
)

// buildTable mirrors rebuild() in-memory so the matching layer can be
// unit-tested without a DB. allowPhrases are normalized like real ones.
func buildTable(entries []policyEntry, allowPhrases []string) *policyTable {
	nb := func() ahocorasick.AhoCorasickBuilder {
		return ahocorasick.NewAhoCorasickBuilder(ahocorasick.Opts{MatchKind: ahocorasick.StandardMatch, DFA: true})
	}
	var pats, pyPats [][]byte
	var ents, pyEnts []policyEntry
	var rxs []regexEntry
	for _, e := range entries {
		if e.matchMode == "regex" {
			rxs = append(rxs, regexEntry{re: regexp.MustCompile("(?i)" + e.pattern), entry: e})
			continue
		}
		pats = append(pats, []byte(NormalizeExact(e.pattern)))
		ents = append(ents, e)
		if HasHan(e.pattern) {
			if py := ToPinyin(NormalizeExact(e.pattern)); py != "" {
				pyPats = append(pyPats, []byte(py))
				pyEnts = append(pyEnts, e)
			}
		}
	}
	tbl := &policyTable{patterns: ents, pyEntries: pyEnts, regexes: rxs}
	if len(pats) > 0 {
		b := nb()
		tbl.ac = b.BuildByte(pats)
	}
	if len(pyPats) > 0 {
		b := nb()
		tbl.acPinyin = b.BuildByte(pyPats)
		tbl.hasPinyin = true
	}
	if len(allowPhrases) > 0 {
		var ap [][]byte
		for _, p := range allowPhrases {
			ap = append(ap, []byte(NormalizeExact(p)))
		}
		b := nb()
		tbl.allowAC = b.BuildByte(ap)
		tbl.hasAllow = true
	}
	return tbl
}

func newTestPolicySvc(entries []policyEntry) *PolicySvc {
	return newTestPolicySvcA(entries, nil)
}

func newTestPolicySvcA(entries []policyEntry, allow []string) *PolicySvc {
	svc := &PolicySvc{}
	svc.active.Store(buildTable(entries, allow))
	return svc
}

func kw(pattern string, cat uuid.UUID, sev, action, mode string) policyEntry {
	return policyEntry{pattern: pattern, categoryID: cat, slug: "c", severity: sev, action: action, scope: ScopeInputOnly, matchMode: mode}
}

func TestScanCaseFoldAndOverlap(t *testing.T) {
	drugs, rug := uuid.New(), uuid.New()
	svc := newTestPolicySvc([]policyEntry{
		kw("drug", drugs, "high", "block", "substring"),
		kw("rug", rug, "low", "log_only", "substring"),
	})
	res := svc.Scan("I bought DRUGS today")
	if !res.HasBlock {
		t.Fatalf("expected HasBlock=true, got false")
	}
	if len(res.Hits) != 2 {
		t.Fatalf("expected 2 deduped hits (drug+rug overlap), got %d: %+v", len(res.Hits), res.Hits)
	}
}

func TestScanDedupesByCategory(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvc([]policyEntry{
		kw("bomb", cat, "high", "block", "substring"),
		kw("gun", cat, "high", "block", "substring"),
	})
	if res := svc.Scan("the bomb and the gun"); len(res.Hits) != 1 {
		t.Fatalf("expected 1 hit after same-category dedup, got %d", len(res.Hits))
	}
}

func TestScanEmptyDictionary(t *testing.T) {
	svc := newTestPolicySvc(nil)
	if res := svc.Scan("anything goes here"); len(res.Hits) != 0 || res.HasBlock {
		t.Fatalf("empty dictionary must return empty result, got %+v", res)
	}
}

func TestNormalizeExactObfuscation(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvc([]policyEntry{kw("cocaine", cat, "high", "block", "substring")})
	cases := []struct {
		name, text string
	}{
		{"fullwidth", "buy ｃｏｃａｉｎｅ now"},  // NFKC folds full-width
		{"cyrillic", "buy сосаine now"},   // с/о are Cyrillic homoglyphs
		{"zerowidth", "buy coc​aine now"}, // zero-width space inside
		{"uppercase", "BUY COCAINE NOW"},
	}
	for _, c := range cases {
		if !svc.Scan(c.text).HasBlock {
			t.Errorf("%s: expected obfuscated keyword to be caught: %q", c.name, c.text)
		}
	}
}

func TestWordBoundaryMode(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvc([]policyEntry{kw("ass", cat, "high", "block", "word")})
	if svc.Scan("this is a class assignment").HasBlock {
		t.Errorf("word-mode 'ass' must NOT match inside 'class'/'assignment'")
	}
	if !svc.Scan("what an ass.").HasBlock {
		t.Errorf("word-mode 'ass' must match as a standalone word")
	}
}

func TestAllowPhraseSuppression(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvcA(
		[]policyEntry{kw("ass", cat, "high", "block", "substring")},
		[]string{"class action"},
	)
	if svc.Scan("we filed a class action lawsuit").HasBlock {
		t.Errorf("allow phrase 'class action' must suppress the 'ass' hit inside it")
	}
	if !svc.Scan("what an ass move").HasBlock {
		t.Errorf("hit outside any allow phrase must still fire")
	}
}

func TestSuspectAggressivePath(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvc([]policyEntry{kw("drug", cat, "high", "block", "substring")})
	res := svc.Scan("buy d r u g here") // separators defeat the block path
	if res.HasBlock || len(res.Hits) != 0 {
		t.Fatalf("separated keyword must not hard-block, got %+v", res)
	}
	if len(res.Suspects) != 1 {
		t.Fatalf("separated keyword must surface as a suspect, got %d", len(res.Suspects))
	}
}

func TestSuspectPinyinHomophone(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvc([]policyEntry{kw("毒品", cat, "high", "block", "substring")})
	// 读品 is a homophone of 毒品 (both "dupin") — caught only via pinyin.
	res := svc.Scan("我想买读品")
	if res.HasBlock {
		t.Fatalf("homophone must not hard-block, got %+v", res)
	}
	if len(res.Suspects) != 1 {
		t.Fatalf("pinyin homophone must surface as a suspect, got %d: %+v", len(res.Suspects), res.Suspects)
	}
}

func TestRegexMode(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvc([]policyEntry{kw(`seed\s*phrase`, cat, "critical", "block", "regex")})
	if !svc.Scan("share your seed   phrase please").HasBlock {
		t.Errorf("regex keyword must match across whitespace")
	}
}

func TestBlockDecisionGrading(t *testing.T) {
	cat1, cat2 := uuid.New(), uuid.New()
	// Single low-severity block hit → should NOT hard-block.
	low := PolicyScanResult{Hits: []PolicyMatch{{CategoryID: cat1, Severity: "low", Action: "block"}}}
	if block, _ := low.BlockDecision(); block {
		t.Errorf("single low-severity hit must not block")
	}
	// Single high-severity → block.
	high := PolicyScanResult{Hits: []PolicyMatch{{CategoryID: cat1, Severity: "high", Action: "block"}}}
	if block, _ := high.BlockDecision(); !block {
		t.Errorf("high-severity hit must block")
	}
	// Two distinct low block categories → block.
	two := PolicyScanResult{Hits: []PolicyMatch{
		{CategoryID: cat1, Severity: "low", Action: "block"},
		{CategoryID: cat2, Severity: "medium", Action: "block"},
	}}
	if block, _ := two.BlockDecision(); !block {
		t.Errorf("two distinct block categories must block")
	}
}

func TestStreamScannerStraddle(t *testing.T) {
	cat := uuid.New()
	svc := newTestPolicySvc([]policyEntry{
		{pattern: "drug", categoryID: cat, slug: "drugs", severity: "high", action: "block", scope: ScopeInputOutput, matchMode: "substring"},
	})
	sc := svc.NewStreamScanner()
	if hit := sc.Feed([]byte("dr"), ScopeInputOutput); hit != nil {
		t.Fatalf("partial 'dr' must not match, got %+v", hit)
	}
	if hit := sc.Feed([]byte("ug now"), ScopeInputOutput); hit == nil {
		t.Fatalf("keyword straddling two frames must be caught")
	}
}
