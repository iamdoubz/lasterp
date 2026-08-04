//go:build integrity

package integrity

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// invRef matches an invariant ID mention, e.g. "INV-E1", "INV-T4".
//
// The family letters are enumerated rather than `[A-Z]` so that a typo like
// "INV-Q1" is caught as malformed by TestCatalogWellFormed instead of silently
// becoming a family of its own. Adding a family therefore means editing this
// line — which is the intended friction: a new class of invariant is a docs/19
// change, not something that appears by writing a comment.
//
// D — device (WP-2.5).
var invRef = regexp.MustCompile(`INV-[FETSXD][0-9]+`)

// TestEveryRequiredInvariantHasATaggedTest is the registry gate docs/19 §1
// demands: "CI fails if an invariant has no tagged tests." It walks every
// *_test.go in the repo (excluding this checker) and requires each
// TestRequired invariant's ID to appear in at least one of them. Deleting or
// renaming a tagged invariant test — the thing CLAUDE.md forbids — turns this
// red. Grep-based on purpose: test binaries are separate processes, so a
// repo scan is both simpler and harder to fool than shared in-memory
// coverage state (see docs/notes/WP-0.8-decisions.md, decision 2).
func TestEveryRequiredInvariantHasATaggedTest(t *testing.T) {
	root := repoRoot(t)
	tagged := map[string]bool{}

	self := filepath.Join(root, "kernel", "integrity", "catalog_test.go")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// `.claude` holds agent worktrees — full checkouts of this repo at
			// other commits, gitignored and invisible to CI but present on a
			// developer's machine. Without this skip the gate harvests invariant
			// tags from those copies: a stale worktree measured here supplied 57
			// test files and 18 distinct IDs, so deleting every tagged test for
			// an invariant in the *real* tree would still go green locally
			// (phase-2-review.md P1.1). CI is unaffected — a clean checkout has
			// no worktrees — which is exactly why it went unnoticed: the gate
			// was weakest on the machine where it is checked before pushing.
			if d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == ".claude" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") || path == self {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, id := range invRef.FindAllString(string(body), -1) {
			tagged[id] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	for _, inv := range Catalog {
		if inv.TestRequired && !tagged[inv.ID] {
			t.Errorf("%s (%s) is TestRequired but no *_test.go references it — "+
				"add a tagged test or (if enforcement was removed) restore it; "+
				"never lower TestRequired to go green (CLAUDE.md)", inv.ID, inv.Title)
		}
	}
}

// TestCatalogWellFormed keeps the catalog itself trustworthy: unique, well-
// formed IDs, and append-only tables declared only where an append-only
// invariant lives.
func TestCatalogWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, inv := range Catalog {
		if !invRef.MatchString(inv.ID) || !strings.HasPrefix(inv.ID, "INV-") {
			t.Errorf("malformed invariant ID %q", inv.ID)
		}
		if seen[inv.ID] {
			t.Errorf("duplicate invariant ID %q", inv.ID)
		}
		seen[inv.ID] = true
		if inv.Title == "" {
			t.Errorf("%s has no title", inv.ID)
		}
		if !inv.TestRequired && inv.Note == "" {
			t.Errorf("%s is not TestRequired but has no Note saying which WP enables it", inv.ID)
		}
	}
	// The append-only set must match exactly. The point of pinning it is that
	// adding or removing a protected table is a deliberate act with an
	// enforcement story (grant revoke + trigger + tests), never a side effect
	// of editing the catalog — so this list grows only alongside that work.
	//
	//	events, audit_log — Phase 0 (WP-0.4 / WP-0.5)
	//	change_feed       — WP-2.1: a feed a writer can edit can lie to a
	//	                    replica that already consumed it
	got := strings.Join(ProtectedTables(), ",")
	if want := "events,audit_log,change_feed"; got != want {
		t.Errorf("ProtectedTables() = %q, want %q", got, want)
	}
}

// repoRoot walks up from the test's working directory (the package dir) to
// the module root, identified by go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the test working directory")
		}
		dir = parent
	}
}
