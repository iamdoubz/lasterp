// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestScheduleNext(t *testing.T) {
	cases := []struct {
		name string
		src  string
		from string
		want string
	}{
		// docs/05's own example: the manifest's `schedule: ["0 2 * * *"]`.
		{"daily 2am", "0 2 * * *", "2026-08-31T01:00:00Z", "2026-08-31T02:00:00Z"},
		{"daily 2am rolls over", "0 2 * * *", "2026-08-31T02:00:00Z", "2026-09-01T02:00:00Z"},
		{"every minute", "* * * * *", "2026-08-31T02:00:30Z", "2026-08-31T02:01:00Z"},
		{"step", "*/15 * * * *", "2026-08-31T02:01:00Z", "2026-08-31T02:15:00Z"},
		{"step wraps the hour", "*/15 * * * *", "2026-08-31T02:46:00Z", "2026-08-31T03:00:00Z"},
		{"list", "0,30 * * * *", "2026-08-31T02:05:00Z", "2026-08-31T02:30:00Z"},
		{"range", "0 9-17 * * *", "2026-08-31T08:00:00Z", "2026-08-31T09:00:00Z"},
		{"range ends", "0 9-17 * * *", "2026-08-31T17:30:00Z", "2026-09-01T09:00:00Z"},
		{"day of month", "0 0 1 * *", "2026-08-31T00:00:00Z", "2026-09-01T00:00:00Z"},
		{"month", "0 0 1 1 *", "2026-08-31T00:00:00Z", "2027-01-01T00:00:00Z"},
		// 2026-08-31 is a Monday; the next Monday is 2026-09-07.
		{"day of week", "0 0 * * 1", "2026-08-31T00:00:01Z", "2026-09-07T00:00:00Z"},
		// Leap day: the search has to cross four years to find it.
		{"leap day", "0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		// POSIX's OR rule: both day fields restricted means either may match,
		// so the 1st fires even though it is not a Monday.
		{"dom or dow", "0 0 1 * 1", "2026-08-31T00:00:01Z", "2026-09-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseCron(tc.src)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tc.src, err)
			}
			got := s.Next(at(tc.from))
			if !got.Equal(at(tc.want)) {
				t.Fatalf("Next(%s) = %s, want %s", tc.from, got.Format(time.RFC3339), tc.want)
			}
		})
	}
}

// Next is strictly after its argument: a schedule that fired at exactly this
// minute must not be handed the same minute again, or the runner re-enqueues
// the job it just ran, forever.
func TestNextIsStrictlyAfter(t *testing.T) {
	s, err := ParseCron("* * * * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	now := at("2026-08-31T02:00:00Z")
	next := s.Next(now)
	if !next.After(now) {
		t.Fatalf("Next(%s) = %s, want a time strictly after", now, next)
	}
	if next.Sub(now) != time.Minute {
		t.Fatalf("Next advanced by %s, want 1m", next.Sub(now))
	}
}

// An expression that can never fire answers "never" rather than looping. The
// 30th of February is the classic one.
func TestScheduleThatNeverFires(t *testing.T) {
	s, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if got := s.Next(at("2026-08-31T00:00:00Z")); !got.IsZero() {
		t.Fatalf("Next = %s, want the zero time", got)
	}
}

func TestParseScheduleRejects(t *testing.T) {
	for _, src := range []string{
		"",
		"* * * *",      // four fields
		"* * * * * *",  // six
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // day-of-month is 1-based
		"* * * 13 *",   // month out of range
		"* * * * 7",    // day-of-week is 0-6
		"5-1 * * * *",  // reversed range
		"*/0 * * * *",  // zero step
		"*/-1 * * * *", // negative step
		"JAN * * * *",  // names unsupported, and said so
		"@hourly",      // macros unsupported
		"* * * * MON",  // names again
		"1,,2 * * * *", // empty list element
	} {
		if _, err := ParseCron(src); err == nil {
			t.Fatalf("ParseCron(%q) succeeded; want an error", src)
		}
	}
}

// The lookahead bound is what keeps a never-firing expression from spinning.
// Asserted as a duration rather than by timing the call, so the check does not
// become flaky on a loaded machine.
func TestLookaheadCoversALeapCycle(t *testing.T) {
	if maxLookahead < 4*366*24*time.Hour {
		t.Fatalf("maxLookahead = %s, too short to reach a 29 February from any start", maxLookahead)
	}
}
