//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// propSeed makes the generated schedule deterministic: a failure reproduces.
const propSeed = 0x3A3A

// conditionCorpus is what a grant's condition may be in the property run. It
// deliberately mixes rules that hold, rules that do not, and rules that cannot
// be evaluated at all — because INV-T3's claim is about *all* of them, and the
// interesting half is the third.
var conditionCorpus = []struct {
	name string
	src  string
	// storeRaw bypasses GrantPermission's compile check, for the conditions
	// that could only reach the table another way: a row written before a
	// cel-go upgrade tightened a rule, or by a migration.
	storeRaw bool
	// deniesForeign is true when this condition must refuse a record owned by
	// somebody else — which is every rule that is false, erroring, over budget,
	// or unevaluable. TestUnevaluableConditionDenies asserts exactly these.
	deniesForeign bool
}{
	{name: "unconditional", src: ""},
	{name: "owner", src: `record.owner == actor.id`, deniesForeign: true},
	{name: "always true", src: `1 == 1`},
	{name: "always false", src: `1 == 2`, deniesForeign: true},
	{name: "role held", src: `"clerk" in actor.roles`},
	{name: "role not held", src: `"nobody" in actor.roles`, deniesForeign: true},
	{name: "tenant", src: `actor.tenant != ""`},
	{name: "missing field", src: `record.no_such_field == "x"`, deniesForeign: true},
	{name: "type error", src: `record.owner == 7`, deniesForeign: true},
	{name: "cost bomb", src: `record.xs.all(a, record.xs.all(b, a == b))`, deniesForeign: true},
	{name: "uncompilable", src: `record.owner ==`, storeRaw: true, deniesForeign: true},
	{name: "not a boolean", src: `record.owner`, storeRaw: true, deniesForeign: true},
	{name: "outside the environment", src: `secrets.get("k") != ""`, storeRaw: true, deniesForeign: true},
	{name: "sql-shaped", src: `record.owner == "' OR 1=1 --"`, deniesForeign: true},
}

// propObjects and propActions are the probe grid. It is deliberately wider than
// what any generated role is granted, so most cells are ungranted — the case a
// widening bug would exploit.
var (
	propObjects = []string{"invoice", "receipt", "contact", "ungranted"}
	propActions = []string{ActionCreate, ActionUpdate, ActionDelete}
)

// TestConditionCanOnlyNarrowAGrant is the WP-3.3 acceptance criterion and the
// INV-T3 property: a CEL condition can only narrow a grant, never widen one.
//
// Each round builds two tenants that differ in exactly one respect — one
// carries the generated conditions, its twin has the identical grants with
// every condition stripped — and probes both across the full
// (object × action × record) grid, most of which is *not* granted at all. The
// assertion is the containment:
//
//	allowed(with conditions)  ⟹  allowed(with every condition removed)
//
// The grid is what makes the property non-vacuous. If the twin were only ever
// probed where it holds a grant it would allow everything, and the implication
// would hold for free. Here a condition that reached past its own
// (object, action) — the shape a widening bug actually takes — lands on a cell
// where the twin denies, and the round fails.
//
// The property does not depend on cel-go being semantically correct: a
// condition is consulted only after a matching grant is found, and every
// failure mode denies (docs/notes/WP-3.3-decisions.md §2). What this proves is
// that the *code* has that shape, over conditions that are true, false,
// erroring, budget-busting and uncompilable.
func TestConditionCanOnlyNarrowAGrant(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			rng := rand.New(rand.NewSource(propSeed))
			var narrowed, allowedBoth, deniedBoth int

			for round := 0; round < 40; round++ {
				// A role holding two or three grants, each on its own
				// (object, action), each with an independently chosen condition.
				n := 2 + rng.Intn(2)
				grants := make([]propGrant, n)
				used := map[string]bool{}
				for i := range grants {
					var object, action string
					for {
						object = propObjects[rng.Intn(len(propObjects)-1)] // never "ungranted"
						action = propActions[rng.Intn(len(propActions))]
						if !used[object+"/"+action] {
							used[object+"/"+action] = true
							break
						}
					}
					c := conditionCorpus[rng.Intn(len(conditionCorpus))]
					grants[i] = propGrant{object: object, action: action, condition: c.src, raw: c.storeRaw}
				}

				actorA := tenantWithGrants(t, db, round, "cond", grants)
				actorB := tenantWithGrants(t, db, round, "plain", stripConditions(grants))

				for probe := 0; probe < 6; probe++ {
					record := randomRecord(rng)
					// Owner is the conditional actor's id, so an owner rule can
					// succeed; the twin's decision never reads the record.
					record["owner"] = string(actorA.UserID)

					for _, object := range propObjects {
						for _, action := range propActions {
							condAllowed := allows(t, db, actorA, object, action, record)
							plainAllowed := allows(t, db, actorB, object, action, record)

							if condAllowed && !plainAllowed {
								t.Fatalf("round %d: %s/%s was ALLOWED with conditions and DENIED with the same grants "+
									"unconditional — a condition widened a grant (INV-T3). grants: %v", round, object, action, grants)
							}
							switch {
							case condAllowed:
								allowedBoth++
							case plainAllowed:
								narrowed++
							default:
								deniedBoth++
							}
						}
					}
				}
			}

			// Non-vacuity, all three ways. Without `narrowed` the conditions
			// never denied anything and the containment held for free; without
			// `allowedBoth` the conditional path is a wall rather than a filter;
			// without `deniedBoth` the grid never probed an ungranted cell, and
			// the ungranted cells are where a widening bug shows up.
			if narrowed == 0 {
				t.Fatal("no condition ever denied a cell its unconditional twin allowed — the property held vacuously")
			}
			if allowedBoth == 0 {
				t.Fatal("no condition ever allowed — conditional grants are denying unconditionally, not narrowing")
			}
			if deniedBoth == 0 {
				t.Fatal("every probed cell was granted — the grid never covered an ungranted (object, action)")
			}
			t.Logf("%d narrowed, %d allowed by both, %d denied by both", narrowed, allowedBoth, deniedBoth)
		})
	}
}

type propGrant struct {
	object    string
	action    string
	condition string
	raw       bool
}

func stripConditions(in []propGrant) []propGrant {
	out := make([]propGrant, len(in))
	for i, g := range in {
		out[i] = propGrant{object: g.object, action: g.action}
	}
	return out
}

// TestUnevaluableConditionDenies pins the fail-closed direction one condition
// at a time, so a regression names the case rather than moving a counter in the
// property run.
func TestUnevaluableConditionDenies(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			checked := 0
			for i, c := range conditionCorpus {
				actor := grantForProperty(t, db, 9000+i, c.name, ActionUpdate, c.src, c.storeRaw)
				rec := map[string]any{"owner": "somebody-else", "xs": bigList()}
				got := allows(t, db, actor, "invoice", ActionUpdate, rec)
				if c.deniesForeign && got {
					t.Fatalf("condition %q (%s) allowed a record it must deny", c.src, c.name)
				}
				// The mirrored half: a rule that should allow must still allow
				// over the same record, or "denies" would only be reporting
				// that the whole conditional path is dead.
				if !c.deniesForeign && !got {
					t.Fatalf("condition %q (%s) denied a record it must allow — the conditional path is failing closed on everything", c.src, c.name)
				}
				if c.deniesForeign {
					checked++
				}
			}
			if checked == 0 {
				t.Fatal("no corpus entry was expected to deny; the fail-closed direction is untested")
			}
		})
	}
}

// A conditional grant must not satisfy a gate that has no record to judge.
// Treating it as sufficient there is the widening INV-T3 forbids, and it is the
// shape the bug would actually take.
func TestConditionalGrantDoesNotSatisfyARecordlessGate(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant, user, role, actor := grantee(t, db, "cond@example.com", "clerk")
			// A condition that is unconditionally true: if a record-less gate
			// were evaluating it, this grant would pass. It must still not.
			if err := GrantPermission(ctx, db, tenant, role, "invoice", ActionUpdate, `1 == 1`); err != nil {
				t.Fatalf("GrantPermission: %v", err)
			}

			ok, err := Can(ctx, db, actor, "invoice", ActionUpdate)
			if err != nil {
				t.Fatalf("Can: %v", err)
			}
			if ok {
				t.Fatal("Can was satisfied by a conditional grant — INV-T2/T3: a record-less gate has nothing to evaluate the condition against")
			}
			if _, err := Authorize(WithActor(ctx, actor), db, "invoice", ActionUpdate); !errors.Is(err, ErrPermissionDenied) {
				t.Fatalf("Authorize on a conditional grant: err = %v, want ErrPermissionDenied", err)
			}

			// GrantedObjects feeds WP-2.4's sync scope. A conditionally-granted
			// object listed there hands a replica the rows the condition denies.
			objects, err := GrantedObjects(ctx, db, actor, ActionUpdate)
			if err != nil {
				t.Fatalf("GrantedObjects: %v", err)
			}
			for _, o := range objects {
				if o == "invoice" {
					t.Fatal("GrantedObjects listed a conditionally-granted object — INV-T1/T2 through the sync door")
				}
			}

			// Non-vacuity: the same actor, the same object, granted
			// unconditionally, is visible to all three. Without this the
			// assertions above would also pass if the grant had never been
			// stored at all.
			if err := GrantPermission(ctx, db, tenant, role, "receipt", ActionUpdate, ""); err != nil {
				t.Fatalf("GrantPermission (unconditional control): %v", err)
			}
			ok, err = Can(ctx, db, Actor{TenantID: tenant, UserID: user}, "receipt", ActionUpdate)
			if err != nil || !ok {
				t.Fatalf("the unconditional control grant is not visible either: ok = %v, err = %v", ok, err)
			}
		})
	}
}

// TestEveryGrantSetIsChecked is the structural half. AuthorizeGrants hands back
// a GrantSet whose conditions are not yet evaluated; a call site that takes it
// and never calls Allow has silently widened every conditional grant it covers
// to unconditional. That is not a bug a runtime test would catch — the widened
// path returns exactly what a correct one returns whenever the condition
// happens to be true — so it is asserted against the source.
func TestEveryGrantSetIsChecked(t *testing.T) {
	root := repoRootForAuthz(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .claude holds agent worktrees: full checkouts at other commits,
			// gitignored and absent from CI. Scanning them would let a stale
			// copy satisfy — or falsely fail — a gate about *this* tree
			// (phase-2-review.md P1.1).
			if d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == ".claude" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(body)
		if !strings.Contains(src, "AuthorizeGrants(") {
			return nil
		}
		// condition.go declares it; AuthorizeRecord is the checked wrapper.
		if filepath.Base(path) == "condition.go" {
			return nil
		}
		if !strings.Contains(src, ".Allow(") {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these files call AuthorizeGrants and never call GrantSet.Allow, which widens every "+
			"conditional grant they cover to unconditional (INV-T3): %v", offenders)
	}
}

// --- helpers ---

// tenantWithGrants builds an isolated tenant/user/role carrying exactly the
// given grants, and returns its actor. Each arm of each round gets its own
// tenant so nothing leaks between iterations.
func tenantWithGrants(t *testing.T, db *storage.DB, round int, arm string, grants []propGrant) Actor {
	t.Helper()
	ctx := context.Background()
	tenant := mustCreateTenant(t, db)
	user := mustCreateUser(t, db, tenant, fmt.Sprintf("prop-%d-%s@example.com", round, arm))
	role, err := CreateRole(ctx, db, tenant, "clerk", false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := AssignRole(ctx, db, tenant, user, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	for _, g := range grants {
		if g.raw {
			insertRawGrant(t, db, tenant, role, g.object, g.action, g.condition)
			continue
		}
		if err := GrantPermission(ctx, db, tenant, role, g.object, g.action, g.condition); err != nil {
			t.Fatalf("GrantPermission(%q on %s/%s): %v", g.condition, g.object, g.action, err)
		}
	}
	return Actor{TenantID: tenant, UserID: user}
}

// grantForProperty is the single-grant form the fail-closed table uses.
func grantForProperty(t *testing.T, db *storage.DB, i int, arm, action, condition string, raw bool) Actor {
	return tenantWithGrants(t, db, i, arm, []propGrant{
		{object: "invoice", action: action, condition: condition, raw: raw},
	})
}

// insertRawGrant writes a condition GrantPermission would refuse. It is how a
// row that no longer compiles gets into the table — a cel-go upgrade
// tightening a rule, or a hand-run migration — and the gate has to deny it,
// not crash on it.
func insertRawGrant(t *testing.T, db *storage.DB, tenant tenancy.ID, role RoleID, object, action, condition string) {
	t.Helper()
	err := tenancy.WithTenant(context.Background(), db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO role_permissions (id, tenant_id, role_id, object, action, condition)
			VALUES (?, ?, ?, ?, ?, ?)`),
			idgen.New(), string(tenant), string(role), object, action, condition)
		return err
	})
	if err != nil {
		t.Fatalf("insert raw grant: %v", err)
	}
}

func allows(t *testing.T, db *storage.DB, actor Actor, object, action string, record map[string]any) bool {
	t.Helper()
	ctx := WithActor(context.Background(), actor)
	_, err := AuthorizeRecord(ctx, db, object, action, record)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrPermissionDenied) {
		return false
	}
	t.Fatalf("AuthorizeRecord returned an unexpected error: %v", err)
	return false
}

func randomRecord(rng *rand.Rand) map[string]any {
	rec := map[string]any{
		"status":      []string{"draft", "posted", "void"}[rng.Intn(3)],
		"total_minor": rng.Intn(1_000_000),
	}
	if rng.Intn(4) == 0 {
		rec["xs"] = bigList()
	}
	return rec
}

// bigList is the cost bomb's fuel: large enough that a quadratic comprehension
// over it exceeds the evaluation budget.
func bigList() []any {
	xs := make([]any, 5000)
	for i := range xs {
		xs[i] = "role"
	}
	return xs
}

func repoRootForAuthz(t *testing.T) string {
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
