// Package biz — text normalization for policy moderation.
//
// Two passes feed the scanner:
//
//   - NormalizeExact: canonicalization that drives the BLOCK path — NFKC
//     (folds full-width / compatibility chars), zero-width strip, homoglyph
//     fold (Cyrillic/Greek look-alikes -> Latin), lowercase. Cheap and high
//     value: it closes the obvious bypass vectors while word-boundary checks
//     still operate on the result.
//
//   - NormalizeAggressive: additionally strips separators and folds
//     leetspeak. Catches "d.r.u.g" / "dru9" / "d r u g". Too lossy to drive a
//     hard block (it would manufacture matches across word gaps), so its hits
//     are SOFT signals only — they raise suspicion and feed async LLM review.
//
// Pinyin rendering (ToPinyin) covers Chinese homophone evasion on the soft
// path: the dictionary's CJK keywords are also indexed by their pinyin form.
package biz

import (
	"strings"
	"unicode"

	"github.com/mozillazg/go-pinyin"
	"golang.org/x/text/unicode/norm"
)

// homoglyphFold maps common confusable codepoints to their ASCII canonical
// form. NFKC already handles full-width and many compatibility variants, so
// this table only needs the script-confusables NFKC leaves alone.
var homoglyphFold = map[rune]rune{
	// Cyrillic -> Latin
	'а': 'a', 'в': 'b', 'е': 'e', 'к': 'k', 'м': 'm',
	'н': 'h', 'о': 'o', 'р': 'p', 'с': 'c', 'т': 't',
	'у': 'y', 'х': 'x', 'ѕ': 's', 'і': 'i', 'ј': 'j',
	'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w', 'г': 'r',
	// Greek -> Latin
	'α': 'a', 'β': 'b', 'ε': 'e', 'ι': 'i', 'κ': 'k',
	'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u',
	'χ': 'x', 'ϲ': 'c',
}

// leetFold maps leetspeak substitutions to letters. Applied on the soft path
// only — folding digits to letters in the block path would wreck precision.
var leetFold = map[rune]rune{
	'0': 'o', '1': 'l', '3': 'e', '4': 'a', '5': 's', '7': 't', '8': 'b',
	'@': 'a', '$': 's', '!': 'i', '|': 'l',
}

func isZeroWidth(r rune) bool {
	switch r {
	case 0x200b, // zero-width space
		0x200c, // zero-width non-joiner
		0x200d, // zero-width joiner
		0x2060, // word joiner
		0xfeff, // zero-width no-break space (BOM)
		0x00ad: // soft hyphen
		return true
	}
	return false
}

// NormalizeExact canonicalizes text for the block path. Length is not
// preserved (NFKC may expand), but the scanner only needs offsets that are
// self-consistent within the returned string (for boundary/allow checks),
// not a mapping back to the original.
func NormalizeExact(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isZeroWidth(r) {
			continue
		}
		if m, ok := homoglyphFold[r]; ok {
			r = m
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// NormalizeAggressive removes separators and folds leetspeak. The input
// should already be NormalizeExact output. Soft-signal use only.
func NormalizeAggressive(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if m, ok := leetFold[r]; ok {
			r = m
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
		// drop everything else: spaces, punctuation, separators
	}
	return b.String()
}

var pinyinArgs = pinyin.NewArgs()

// ToPinyin renders the CJK runes of s into concatenated toneless pinyin,
// dropping non-han characters. Empty when s has no han characters.
func ToPinyin(s string) string {
	parts := pinyin.LazyPinyin(s, pinyinArgs)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "")
}

// HasHan reports whether s contains any CJK ideograph (gates pinyin work).
func HasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
