//go:build integrity

package idgen

import "testing"

// TestGauntletDiscoveryCanary proves the Integrity Gauntlet's build-tag
// discovery (`go test -tags integrity ./...`, WP-0.10) reaches packages
// *outside* kernel/integrity. idgen has no other integrity test, so this file
// only compiles and runs under the tag — if the gauntlet job ever stops
// picking up tagged tests repo-wide, this canary silently vanishes from its
// output. Keep it: it is the wiring smoke test the WP-0.10 AC calls for, not
// dead weight.
func TestGauntletDiscoveryCanary(t *testing.T) {
	if New() == "" {
		t.Fatal("idgen.New() returned empty ID")
	}
}
