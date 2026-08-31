// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"fmt"
	"strconv"
	"strings"
)

// The `lasterp:` host range (WP-3.2b, WP-3.2-decisions.md §4).
//
// docs/05 asks for "semver, dependency solver". The dependency that exists is
// this one: a manifest claiming which host versions it runs on. Plugin→plugin
// dependencies have no consumer — no plugin can call another — so a solver
// would be resolving edges that cannot exist, and the manifest refuses one by
// name instead.
//
// WP-3.1a parsed `lasterp:` and recorded it without enforcing it. This is the
// enforcement, and it is deliberately a subset of semver rather than a
// dependency: `>=`, `>`, `<=`, `<` and `=` over dotted numbers, joined by
// spaces, which is every range the docs use and every range a host-version
// claim needs. Pre-release tags and build metadata are not accepted, because a
// plugin that means to run on `1.0.0-rc1` is making a claim nobody can honour.

// HostVersion is the version this server reports to a plugin's `lasterp:`
// range.
//
// It tracks the Helm chart's appVersion, which is the only place the product
// has ever written a version down; `TestHostVersionMatchesTheChart` fails if
// the two drift. A release pipeline that stamps a version belongs to whichever
// WP builds one — until then, one constant beats a version string invented in
// three places.
const HostVersion = "0.1.0"

// ErrHostVersion is returned when a manifest's range excludes this host.
var ErrHostVersion = fmt.Errorf("plugins: this host's version does not satisfy the manifest's lasterp range")

// checkHostVersion reports whether HostVersion satisfies the manifest's range.
// An empty range is satisfied by anything: declaring nothing is a plugin that
// makes no claim, which is not the same as a plugin that claims wrongly.
func checkHostVersion(rng string) error {
	if strings.TrimSpace(rng) == "" {
		return nil
	}
	host, err := parseVersion(HostVersion)
	if err != nil {
		return err
	}
	for _, term := range strings.Fields(rng) {
		op, want, err := parseConstraint(term)
		if err != nil {
			return err
		}
		if !satisfies(host, op, want) {
			return fmt.Errorf("%w: host is %s, manifest wants %s", ErrHostVersion, HostVersion, rng)
		}
	}
	return nil
}

// version is a dotted numeric version, padded to three parts. A range that
// says `1.0` means `1.0.0`, which is what an author writing `>=1.0` means.
type version [3]int

func parseVersion(s string) (version, error) {
	var v version
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v, fmt.Errorf("plugins: %q is not a version", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("plugins: %q is not a version", s)
		}
		v[i] = n
	}
	return v, nil
}

func parseConstraint(term string) (op string, want version, err error) {
	for _, candidate := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(term, candidate) {
			v, err := parseVersion(strings.TrimPrefix(term, candidate))
			return candidate, v, err
		}
	}
	// A bare version is an exact match, as `1.2.0` reads.
	v, err := parseVersion(term)
	return "=", v, err
}

func satisfies(host version, op string, want version) bool {
	cmp := compare(host, want)
	switch op {
	case ">=":
		return cmp >= 0
	case ">":
		return cmp > 0
	case "<=":
		return cmp <= 0
	case "<":
		return cmp < 0
	default:
		return cmp == 0
	}
}

func compare(a, b version) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
