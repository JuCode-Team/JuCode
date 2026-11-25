package biz

import (
	"testing"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestNormalizeIPWhitelist(t *testing.T) {
	got, err := NormalizeIPWhitelist([]string{" 1.2.3.4 ", "10.0.0.7/8", "::1", "1.2.3.4", ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"1.2.3.4", "10.0.0.0/8", "::1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	for _, bad := range []string{"nope", "1.2.3", "10.0.0.0/99", "1.2.3.4/-1"} {
		if _, err := NormalizeIPWhitelist([]string{bad}); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestAPIKeyIPAllowed(t *testing.T) {
	unrestricted := &ent.ApiKey{}
	if !APIKeyIPAllowed(unrestricted, "8.8.8.8") {
		t.Error("empty whitelist must allow any ip")
	}

	k := &ent.ApiKey{IPWhitelist: []string{"1.2.3.4", "10.0.0.0/8", "::1"}}
	cases := []struct {
		ip   string
		want bool
	}{
		{"1.2.3.4", true},
		{"10.200.1.9", true},
		{"::1", true},
		// IPv4-mapped IPv6 form of a whitelisted v4 address.
		{"::ffff:1.2.3.4", true},
		{"1.2.3.5", false},
		{"11.0.0.1", false},
		{"", false},
		{"not-an-ip", false},
	}
	for _, c := range cases {
		if got := APIKeyIPAllowed(k, c.ip); got != c.want {
			t.Errorf("APIKeyIPAllowed(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
}
