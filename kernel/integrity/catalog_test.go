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
var invRef = regexp.MustCompile(`INV-[FETSX][0-9]+`)

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
			if d.Name() == "node_modules" || d.Name() == ".git" {
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
	// The two Phase-0 append-only tables must be present exactly.
	got := strings.Join(ProtectedTables(), ",")
	if got != "events,audit_log" {
		t.Errorf("ProtectedTables() = %q, want the two Phase-0 append-only tables", got)
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
