//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package tenancy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoWithTenantCallbackAccumulatesIntoAnEnclosingSlice is the structural
// half of WP-3.3d, and the half that makes the fix stay fixed.
//
// WithTenant re-runs the whole callback on SQLITE_BUSY. Its contract — stated
// on the function itself — is that the callback must be idempotent with
// respect to anything it touches outside the transaction. Sixteen read paths
// broke that contract in the same way, by appending into a slice declared in
// the enclosing function, and every one of them looked correct: the database
// stays consistent, no error is raised, and the caller simply receives some
// rows twice. Nothing but this test notices, which is why the seventeenth
// would have landed the same way.
//
// go/ast rather than grep — the question is whether the accumulator is
// declared inside the closure or outside it, and that is a scoping question no
// amount of brace-counting answers reliably. The stdlib parser costs nothing
// over the grep gates this sits beside (TestCELIsImportedOnlyHere,
// TestEveryGrantSetIsChecked).
//
// Test files are scanned too: the same shape makes a test flaky under load
// rather than wrong, which is worse to diagnose, and an exclusion list is one
// more thing to get wrong.
func TestNoWithTenantCallbackAccumulatesIntoAnEnclosingSlice(t *testing.T) {
	root := repoRootForTenancy(t)

	offenders, closures := scanForAliasing(t, root)
	if len(offenders) > 0 {
		t.Fatalf("these WithTenant callbacks append into a slice declared outside the closure, "+
			"so a SQLITE_BUSY retry returns the failed attempt's rows plus the good one's "+
			"(docs/notes/WP-3.3d-retry-aliasing.md — build into a local and assign it once, "+
			"after the last thing that can fail):\n  %s", strings.Join(offenders, "\n  "))
	}

	// Non-vacuity: "no offenders" must mean the scan looked at real callbacks,
	// not that a wrong root or a bad filter made it look at nothing.
	if closures < 50 {
		t.Fatalf("the scan found only %d WithTenant closures in the tree; it is not looking "+
			"where the callbacks are, so the result above proves nothing", closures)
	}
}

// TestTheAliasingGateFlagsAReintroducedSite is the gate's own check: the
// detector must fail on the shape it exists to catch, and pass on the shape
// WP-3.3d replaced it with. Without this, a detector that silently matched
// nothing would stay green forever and read as proof.
func TestTheAliasingGateFlagsAReintroducedSite(t *testing.T) {
	const reintroduced = `package p

func read() ([]int, error) {
	var out []int
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		for rows.Next() {
			out = append(out, 1)
		}
		return rows.Err()
	})
	return out, err
}
`
	const fixed = `package p

func read() ([]int, error) {
	var out []int
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var list []int
		for rows.Next() {
			list = append(list, 1)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	return out, err
}
`
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"the shape WP-3.3d fixed", reintroduced, 1},
		{"the shape it was fixed to", fixed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}
			got, closures := aliasingIn(fset, file, "fixture.go")
			if closures != 1 {
				t.Fatalf("found %d WithTenant closures in the fixture, want 1", closures)
			}
			if len(got) != tc.want {
				t.Fatalf("detector reported %d offenders (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}

func scanForAliasing(t *testing.T, root string) (offenders []string, closures int) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .claude holds agent worktrees: full checkouts at other commits,
			// gitignored and absent from CI. A stale copy must not answer a
			// question about this tree (phase-2-review.md P1.1).
			switch d.Name() {
			case "node_modules", ".git", ".claude", "web", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			// Not this test's job to police syntax; the build says that
			// louder. Skipping keeps a generated or in-progress file from
			// turning an invariant gate red for the wrong reason.
			return nil //nolint:nilerr // deliberate: an unparseable file is the compiler's finding, not this gate's
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		found, n := aliasingIn(fset, file, filepath.ToSlash(rel))
		offenders = append(offenders, found...)
		closures += n
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return offenders, closures
}

// aliasingIn reports every `x = append(x, …)` inside a WithTenant callback
// whose x is declared outside that callback, and how many callbacks it looked
// at.
func aliasingIn(fset *token.FileSet, file *ast.File, name string) (offenders []string, closures int) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isWithTenant(call.Fun) {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.FuncLit)
			if !ok {
				continue
			}
			closures++
			local := declaredIn(lit)
			ast.Inspect(lit.Body, func(n ast.Node) bool {
				target, ok := appendsToItself(n)
				if !ok || local[target] {
					return true
				}
				pos := fset.Position(n.Pos())
				offenders = append(offenders,
					name+":"+strconv.Itoa(pos.Line)+": "+target+" is declared outside the callback")
				return true
			})
		}
		return true
	})
	return offenders, closures
}

func isWithTenant(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name == "WithTenant"
	case *ast.Ident: // a call from inside this package
		return f.Name == "WithTenant"
	}
	return false
}

// appendsToItself matches `x = append(x, …)` and returns x.
func appendsToItself(n ast.Node) (string, bool) {
	assign, ok := n.(*ast.AssignStmt)
	if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return "", false
	}
	lhs, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return "", false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "append" {
		return "", false
	}
	if first, ok := call.Args[0].(*ast.Ident); !ok || first.Name != lhs.Name {
		return "", false
	}
	return lhs.Name, true
}

// declaredIn is every name the closure introduces itself: parameters, results,
// short declarations, var/const declarations, range variables and type-switch
// bindings. A name outside this set, assigned inside the closure, survives the
// rollback the retry depends on.
func declaredIn(lit *ast.FuncLit) map[string]bool {
	names := map[string]bool{}
	add := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
			names[id.Name] = true
		}
	}
	for _, field := range lit.Type.Params.List {
		for _, n := range field.Names {
			add(n)
		}
	}
	if lit.Type.Results != nil {
		for _, field := range lit.Type.Results.List {
			for _, n := range field.Names {
				add(n)
			}
		}
	}
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				for _, lhs := range s.Lhs {
					add(lhs)
				}
			}
		case *ast.ValueSpec:
			for _, n := range s.Names {
				add(n)
			}
		case *ast.RangeStmt:
			if s.Tok == token.DEFINE {
				add(s.Key)
				add(s.Value)
			}
		case *ast.TypeSwitchStmt:
			if a, ok := s.Assign.(*ast.AssignStmt); ok && len(a.Lhs) == 1 {
				add(a.Lhs[0])
			}
		}
		return true
	})
	return names
}

func repoRootForTenancy(t *testing.T) string {
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
