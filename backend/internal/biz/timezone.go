package biz

import (
	"fmt"
	"regexp"
	"time"
)

// DefaultTimezone is the business timezone unless config server.timezone
// names another.
const DefaultTimezone = "Asia/Shanghai"

// The business timezone decides what a calendar day and month are: finance
// and ledger reports, day buckets in usage stats, API key daily/monthly
// spend limits, invite windows and incident trends. It is set once at boot
// (SetTimezone) before any request is served.
var (
	tzName = DefaultTimezone
	tzLoc  = time.FixedZone("UTC+8", 8*3600) // replaced by init below
)

// tzNamePattern bounds what may be spliced into SQL as a timezone literal.
// IANA names (Asia/Shanghai, America/Argentina/Buenos_Aires, Etc/GMT+8)
// all match it.
var tzNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+\-/]{1,64}$`)

func init() {
	if loc, err := time.LoadLocation(tzName); err == nil {
		tzLoc = loc
	}
}

// SetTimezone switches the business timezone. name must be an IANA zone
// name that Postgres also knows.
func SetTimezone(name string) error {
	if !tzNamePattern.MatchString(name) {
		return fmt.Errorf("invalid timezone %q", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", name, err)
	}
	tzName, tzLoc = name, loc
	return nil
}

// TimezoneName is the business timezone's IANA name, for SQL AT TIME ZONE.
func TimezoneName() string { return tzName }

// Location is the business timezone.
func Location() *time.Location { return tzLoc }

// CalendarPeriods returns the start of the day and of the month containing
// now, in the business timezone.
func CalendarPeriods(now time.Time) (day, month time.Time) {
	n := now.In(tzLoc)
	day = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, tzLoc)
	month = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, tzLoc)
	return day, month
}

// startOfDay is the inclusive start of the business calendar day that
// contains t.
func startOfDay(t time.Time) time.Time {
	day, _ := CalendarPeriods(t)
	return day
}
