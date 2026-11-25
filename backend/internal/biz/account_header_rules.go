package biz

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jucode/jucode/internal/data/ent/schema"
)

// Per-account upstream header rewrites.
//
// Some upstreams speak a standard body dialect behind non-standard headers:
// Azure AI Foundry serves the Anthropic Messages API but 400s on any
// anthropic-beta value it has not enabled, and API gateways (APIM, corporate
// proxies) want the key under their own header name. The account declares the
// difference instead of the gateway growing a branch per vendor. An account
// without rules is untouched.

// HeaderRulesAPIKeyPlaceholder is replaced by the account credential in set
// values, so a gateway that wants the key under another name can say so
// without the key being copied into the (non-secret) rules.
const HeaderRulesAPIKeyPlaceholder = "{api_key}"

// maxHeaderRuleEntries bounds a declaration so a paste accident cannot bloat
// every request.
const maxHeaderRuleEntries = 32

// headerRulesForbidden are headers a rule must not touch: rewriting them
// breaks the transport's own framing or target selection.
var headerRulesForbidden = map[string]struct{}{
	"Host":              {},
	"Content-Length":    {},
	"Transfer-Encoding": {},
	"Connection":        {},
}

// NormalizeHeaderRules validates admin-supplied rules and canonicalizes header
// names. It returns nil when nothing is declared, so storing the result clears
// the column.
func NormalizeHeaderRules(in *schema.HeaderRules) (*schema.HeaderRules, error) {
	if in == nil {
		return nil, nil
	}
	if len(in.Set)+len(in.Remove)+len(in.DropValues) > maxHeaderRuleEntries {
		return nil, fmt.Errorf("%w: header_rules has more than %d entries", ErrAccountBadInput, maxHeaderRuleEntries)
	}
	out := schema.HeaderRules{}
	for name, value := range in.Set {
		key, err := headerRuleName(name)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("%w: header_rules.set %q value must not contain line breaks", ErrAccountBadInput, key)
		}
		if out.Set == nil {
			out.Set = map[string]string{}
		}
		out.Set[key] = strings.TrimSpace(value)
	}
	for _, name := range in.Remove {
		if strings.TrimSpace(name) == "" {
			continue
		}
		key, err := headerRuleName(name)
		if err != nil {
			return nil, err
		}
		out.Remove = append(out.Remove, key)
	}
	for name, patterns := range in.DropValues {
		key, err := headerRuleName(name)
		if err != nil {
			return nil, err
		}
		for _, p := range patterns {
			p = strings.TrimSpace(p)
			if p == "" || p == "*" {
				// "*" would drop every value; that is what remove is for.
				continue
			}
			if strings.ContainsAny(p, ",\r\n") {
				return nil, fmt.Errorf("%w: header_rules.drop_values %q entry %q must be a single value", ErrAccountBadInput, key, p)
			}
			if out.DropValues == nil {
				out.DropValues = map[string][]string{}
			}
			out.DropValues[key] = append(out.DropValues[key], p)
		}
	}
	if len(out.Set) == 0 && len(out.Remove) == 0 && len(out.DropValues) == 0 {
		return nil, nil
	}
	return &out, nil
}

func headerRuleName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || strings.ContainsAny(name, " \t\r\n:") {
		return "", fmt.Errorf("%w: header_rules header name %q is invalid", ErrAccountBadInput, raw)
	}
	key := http.CanonicalHeaderKey(name)
	if _, forbidden := headerRulesForbidden[key]; forbidden {
		return "", fmt.Errorf("%w: header_rules must not rewrite %s", ErrAccountBadInput, key)
	}
	return key, nil
}

// HeaderRulesEmpty reports whether the rules would change nothing.
func HeaderRulesEmpty(r schema.HeaderRules) bool {
	return len(r.Set) == 0 && len(r.Remove) == 0 && len(r.DropValues) == 0
}

// ApplyHeaderRules rewrites h in place: remove, then drop_values, then set.
// Set runs last so a rule can both remove the gateway's default credential
// header and set the vendor's own.
func ApplyHeaderRules(h http.Header, r schema.HeaderRules, apiKey string) {
	if HeaderRulesEmpty(r) {
		return
	}
	for _, name := range r.Remove {
		h.Del(name)
	}
	for name, patterns := range r.DropValues {
		dropHeaderValues(h, name, patterns)
	}
	for name, value := range r.Set {
		h.Set(name, strings.ReplaceAll(value, HeaderRulesAPIKeyPlaceholder, apiKey))
	}
}

// dropHeaderValues removes the comma-separated entries of header name that
// match any pattern (exact, or prefix when the pattern ends in "*"), across
// every occurrence of the header. The header is deleted when nothing is left.
func dropHeaderValues(h http.Header, name string, patterns []string) {
	values := h.Values(name)
	if len(values) == 0 {
		return
	}
	kept := make([]string, 0, len(values))
	for _, line := range values {
		for _, item := range strings.Split(line, ",") {
			item = strings.TrimSpace(item)
			if item == "" || headerValueMatches(item, patterns) {
				continue
			}
			kept = append(kept, item)
		}
	}
	if len(kept) == 0 {
		h.Del(name)
		return
	}
	h.Set(name, strings.Join(kept, ","))
}

func headerValueMatches(value string, patterns []string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(value, prefix) {
				return true
			}
			continue
		}
		if value == p {
			return true
		}
	}
	return false
}
