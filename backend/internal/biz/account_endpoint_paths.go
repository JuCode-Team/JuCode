package biz

import (
	"fmt"
	"strings"

	"github.com/jucode/jucode/internal/data/ent"
)

// Per-account upstream path overrides.
//
// The gateway forwards a generation call to `<base_url> + <path>`, and that
// path used to be a constant per dialect (/v1/chat/completions, /v1/responses,
// /v1/messages). Vendors outside those conventions cannot be expressed by any
// base_url — some answer on /v2/chat/completions — so the account may
// declare its own paths. Absent/empty keeps the dialect default, which is why
// this is inert for every account that does not set it.

// maxEndpointPathLength bounds a declaration. The longest path we have seen is
// well under this; the limit exists so a typo cannot bloat the account row.
const maxEndpointPathLength = 512

// NormalizeEndpointPaths validates an admin-supplied override map. Keys are
// protocol names (aliases accepted, normalized); an empty value means "no
// override for that protocol" and is dropped, so an edit can clear one entry
// without touching the others.
func NormalizeEndpointPaths(raw map[string]string) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		proto, ok := ParseNativeProtocol(key)
		if !ok {
			return nil, fmt.Errorf("%w: endpoint_paths key %q is not a known protocol", ErrAccountBadInput, key)
		}
		path := strings.TrimSpace(value)
		if path == "" {
			continue
		}
		if err := validateEndpointPath(path); err != nil {
			return nil, err
		}
		out[proto] = path
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func validateEndpointPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: endpoint path %q must start with /", ErrAccountBadInput, path)
	}
	if strings.HasPrefix(path, "//") {
		// "//host" would turn the request into a protocol-relative URL.
		return fmt.Errorf("%w: endpoint path %q must not start with //", ErrAccountBadInput, path)
	}
	if strings.ContainsAny(path, " \t\r\n") {
		return fmt.Errorf("%w: endpoint path %q must not contain whitespace", ErrAccountBadInput, path)
	}
	if len(path) > maxEndpointPathLength {
		return fmt.Errorf("%w: endpoint path is longer than %d bytes", ErrAccountBadInput, maxEndpointPathLength)
	}
	return nil
}

// EndpointPathsOf decodes the stored overrides into a clean map. It is
// lenient on read: a row written by an older build, or hand-edited, must never
// break a request path.
func EndpointPathsOf(acct *ent.UpstreamAccount) map[string]string {
	if acct == nil || len(acct.EndpointPaths) == 0 {
		return nil
	}
	out := make(map[string]string, len(acct.EndpointPaths))
	for key, value := range acct.EndpointPaths {
		proto, ok := ParseNativeProtocol(key)
		if !ok {
			continue
		}
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		out[proto] = strings.TrimSpace(text)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EndpointPathFor returns the account's declared path for one protocol, or ""
// when the account wants the dialect default. Called on the request path, so it
// avoids building a map for accounts that never set an override.
func EndpointPathFor(acct *ent.UpstreamAccount, proto string) string {
	if acct == nil || len(acct.EndpointPaths) == 0 {
		return ""
	}
	canonical, ok := ParseNativeProtocol(proto)
	if !ok {
		return ""
	}
	for key, value := range acct.EndpointPaths {
		keyProto, ok := ParseNativeProtocol(key)
		if !ok || keyProto != canonical {
			continue
		}
		if text, ok := value.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
