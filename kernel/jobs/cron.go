// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression: minute, hour, day-of-month,
// month, day-of-week. It is the form docs/05 already documents for a plugin
// manifest's `schedule:` capability (`"0 2 * * *"`).
//
// Hand-written rather than a dependency, and deliberately the opposite call
// from [ADR-022], which took cel-go for the authorization path. Cron has no
// security surface and no ambiguity in the five-field form: a wrong answer here
// produces a job at the wrong minute, not an authorization decision for the
// wrong actor. The whole parser is a hundred lines and one table-driven test.
//
// Supported per field: `*`, `N`, `a-b`, `a,b,c`, and any of those with a `/n`
// step. Names (`JAN`, `MON`) and the non-standard `@hourly` macros are not
// supported — a manifest is written once by a developer, and the numeric form
// is the one every cron agrees on.
//
// [ADR-022]: ../../docs/adr/ADR-022-expression-language.md
type Schedule struct {
	src     string
	minute  [60]bool
	hour    [24]bool
	dom     [32]bool // 1..31
	month   [13]bool // 1..12
	dow     [7]bool  // 0..6, Sunday = 0
	domStar bool
	dowStar bool
}

// Source returns the expression this schedule was parsed from.
func (s *Schedule) Source() string { return s.src }

type cronField struct {
	name     string
	min, max int
}

var cronFields = []cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 6},
}

// ParseSchedule parses a 5-field cron expression.
func ParseSchedule(src string) (*Schedule, error) {
	parts := strings.Fields(src)
	if len(parts) != 5 {
		return nil, fmt.Errorf("jobs: %q is not a 5-field cron expression (got %d fields)", src, len(parts))
	}
	s := &Schedule{src: src}
	// Day-of-month and day-of-week are ORed when both are restricted and
	// ANDed when either is `*` — the POSIX rule every cron implements and
	// nobody remembers. Recorded here because next() depends on it.
	s.domStar = parts[2] == "*"
	s.dowStar = parts[4] == "*"

	sets := []func(int){
		func(v int) { s.minute[v] = true },
		func(v int) { s.hour[v] = true },
		func(v int) { s.dom[v] = true },
		func(v int) { s.month[v] = true },
		func(v int) { s.dow[v] = true },
	}
	for i, f := range cronFields {
		if err := parseField(parts[i], f, sets[i]); err != nil {
			return nil, fmt.Errorf("jobs: %q: %s: %w", src, f.name, err)
		}
	}
	return s, nil
}

func parseField(spec string, f cronField, set func(int)) error {
	if spec == "" {
		return fmt.Errorf("empty")
	}
	for _, part := range strings.Split(spec, ",") {
		rangePart, step := part, 1
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			rangePart = part[:slash]
			n, err := strconv.Atoi(part[slash+1:])
			if err != nil || n < 1 {
				return fmt.Errorf("%q has an invalid step", part)
			}
			step = n
		}

		lo, hi := f.min, f.max
		switch {
		case rangePart == "*":
			// full range
		case strings.ContainsRune(rangePart, '-'):
			bounds := strings.SplitN(rangePart, "-", 2)
			a, errA := strconv.Atoi(bounds[0])
			b, errB := strconv.Atoi(bounds[1])
			if errA != nil || errB != nil {
				return fmt.Errorf("%q is not a valid range", part)
			}
			if a > b {
				return fmt.Errorf("%q is a reversed range", part)
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil {
				return fmt.Errorf("%q is not a number", part)
			}
			lo, hi = v, v
		}
		if lo < f.min || hi > f.max {
			return fmt.Errorf("%q is outside %d-%d", part, f.min, f.max)
		}
		for v := lo; v <= hi; v += step {
			set(v)
		}
	}
	return nil
}

// maxLookahead bounds Next's search. Five years covers every expression that
// ever fires — including 29 February, whose gap is four years — and turns one
// that never fires (`0 0 30 2 *`, the 30th of February) into an answerable
// "never" rather than an infinite loop.
const maxLookahead = 5 * 366 * 24 * time.Hour

// Next returns the first scheduled time strictly after `after`, in UTC, or the
// zero time if the expression can never fire.
//
// It steps minute by minute within a matching day and jumps a whole day
// forward when the date does not match, so the search is bounded by
// (days scanned + 1440) rather than by minutes in five years.
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := after.UTC().Add(maxLookahead)
	for t.Before(limit) {
		if !s.matchesDay(t) {
			// Next midnight. Truncate-then-add is wrong across a day whose
			// length is not 24h; this builds the date explicitly.
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if s.hour[t.Hour()] && s.minute[t.Minute()] {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// matchesDay applies the POSIX day rule: when both day-of-month and
// day-of-week are restricted, a day matches if *either* does; when either is
// `*`, both must match (which the `*` satisfies trivially).
func (s *Schedule) matchesDay(t time.Time) bool {
	if !s.month[int(t.Month())] {
		return false
	}
	dom := s.dom[t.Day()]
	dow := s.dow[int(t.Weekday())]
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}
