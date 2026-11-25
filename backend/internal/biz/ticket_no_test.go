package biz

import (
	"testing"
	"time"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestParseTicketNo(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"T-1842", 1842, true},
		{"t-1842", 1842, true},
		{"T1842", 1842, true},
		{"1842", 1842, true},
		{"  T-9  ", 9, true},
		{"T-", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"0", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseTicketNo(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("%q: got (%d,%v) want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFormatTicketNo(t *testing.T) {
	if FormatTicketNo(1842) != "T-1842" {
		t.Fatalf("got %q", FormatTicketNo(1842))
	}
	if FormatTicketNo(0) != "" {
		t.Fatalf("zero should be empty")
	}
}

func TestUnreadKind(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	tck := &ent.Ticket{LastStaffReplyAt: &now}
	if UnreadKind(tck, false) != "unread" {
		t.Fatal("never seen should be unread")
	}
	tck.UserSeenAt = &earlier
	if UnreadKind(tck, false) != "reply" {
		t.Fatal("older seen should be reply")
	}
	tck.UserSeenAt = &now
	if UnreadKind(tck, false) != "" {
		t.Fatal("caught up should be empty")
	}
}
