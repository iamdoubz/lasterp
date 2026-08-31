//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package expr

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCELIsImportedOnlyHere is the structural half of ADR-022 and of INV-T3.
//
// The closed environment — two bindings, no extension functions, no host state
// — is the security boundary, and it is a property of *this package's* call to
// cel.NewEnv. A second call site anywhere else in the tree builds a second
// environment that nobody reviewed, and the first thing such an environment
// grows is the binding that made it necessary. So the fence is the rule, and
// this is the rule as a test rather than as a paragraph.
//
// Grep-based, like the invariant registry gate it sits beside: an import graph
// walk would need the toolchain to agree about build tags, and a file that
// mentions the package is worth looking at either way.
func TestCELIsImportedOnlyHere(t *testing.T) {
	root := repoRoot(t)
	selfDir := filepath.Join(root, "kernel", "expr")

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .claude holds agent worktrees: full checkouts at other commits,
			// gitignored and absent from CI. A stale copy must not answer a
			// question about this tree (phase-2-review.md P1.1).
			if d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == ".claude" || d.Name() == "web" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if filepath.Dir(path) == selfDir {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), `"cel.dev/cel-go`) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("cel-go is imported outside kernel/expr, which builds a second evaluation environment "+
			"nobody reviewed (ADR-022): %v", offenders)
	}

	// Non-vacuity: the walk must actually have been able to see this package's
	// own import, or "no offenders" would only mean the scan found nothing at
	// all — a wrong root, a bad suffix filter, a skip that swallowed the tree.
	self, err := os.ReadFile(filepath.Join(selfDir, "expr.go"))
	if err != nil {
		t.Fatalf("read own source: %v", err)
	}
	if !strings.Contains(string(self), `"cel.dev/cel-go`) {
		t.Fatal("kernel/expr/expr.go does not import cel-go; the scan above proves nothing")
	}
}

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
			t.Fatal("could not find the repo root (no go.mod above the test)")
		}
		dir = parent
	}
}
