// Package trustedproxy resolves the list of CIDRs / IPs that should be
// treated as trusted upstream proxies by Gin (so X-Forwarded-For from
// them is honored when extracting the real client IP).
//
// Sources, merged at startup:
//
//   - Loopback (127.0.0.1 + ::1) — always trusted; the Caddy peer.
//   - Cloudflare ranges fetched from cloudflare.com/ips-v4 + ips-v6;
//     plain-text newline-separated CIDRs.
//   - Config ExtraTrustedProxies — operator-managed fallback list.
//   - DB ip_policies table, kind=trusted_proxy — operator-managed
//     dynamic list (added at admin runtime, fetched fresh on each
//     server boot via IPPolicySvc.TrustedProxiesCIDRs).
//
// Fetch failures are non-fatal: callers fall back to whatever subset
// succeeded. The boot must not block on Cloudflare being reachable.
package trustedproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// fetchTimeout caps one HTTP fetch. Boot doesn't wait long on CF.
const fetchTimeout = 5 * time.Second

// Fetch GETs each URL, parses one CIDR per non-empty line, returns
// the merged list. Errors per URL are wrapped together; partial
// successes still return a populated list.
func Fetch(ctx context.Context, urls []string) ([]string, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	client := &http.Client{Timeout: fetchTimeout}
	var (
		out  []string
		errs []string
	)
	for _, u := range urls {
		cidrs, err := fetchOne(ctx, client, u)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		out = append(out, cidrs...)
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("trusted-proxy fetch errors: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

func fetchOne(ctx context.Context, client *http.Client, url string) ([]string, error) {
	rctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !valid(line) {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// valid reports whether s parses as either a CIDR block or a bare IP
// that we can normalize into a /32 or /128. Skipping invalid lines
// keeps a single typo from poisoning the whole list.
func valid(s string) bool {
	if _, _, err := net.ParseCIDR(s); err == nil {
		return true
	}
	return net.ParseIP(s) != nil
}

// Combine returns the deduplicated union of the input slices. Stable
// order: each source is appended in the order it was passed.
func Combine(sources ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 32)
	for _, src := range sources {
		for _, s := range src {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// Loopback returns the always-trusted local-peer entries.
func Loopback() []string {
	return []string{"127.0.0.1/32", "::1/128"}
}
