// SPDX-License-Identifier: AGPL-3.0-only

package expr

import (
	"errors"
	"strings"
	"testing"
)

func TestCompileAndEval(t *testing.T) {
	actor := Actor{ID: "u1", Tenant: "t1", Roles: []string{"clerk", "sales"}}
	cases := []struct {
		name string
		src  string
		rec  map[string]any
		want bool
	}{
		// docs/08 §AuthZ's own example, both halves.
		{"owner match", `record.owner == actor.id`, map[string]any{"owner": "u1"}, true},
		{"owner mismatch", `record.owner == actor.id`, map[string]any{"owner": "u2"}, false},
		{"role in list", `"clerk" in actor.roles`, nil, true},
		{"role absent", `"admin" in actor.roles`, nil, false},
		{"disjunction", `record.owner == actor.id || "admin" in actor.roles`, map[string]any{"owner": "u2"}, false},
		{"tenant", `actor.tenant == "t1"`, nil, true},
		{"numeric", `record.total_minor > 100000`, map[string]any{"total_minor": 250000}, true},
		{"numeric below", `record.total_minor > 100000`, map[string]any{"total_minor": 5}, false},
		{"negation", `!(record.status == "draft")`, map[string]any{"status": "posted"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prg, err := Compile(tc.src)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tc.src, err)
			}
			got, err := prg.Eval(tc.rec, actor)
			if err != nil {
				t.Fatalf("Eval(%q): %v", tc.src, err)
			}
			if got != tc.want {
				t.Fatalf("Eval(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// A condition that is not a boolean question is refused where the
// administrator can see it, rather than coerced into one. "Truthy" is how a
// permission widens by accident.
func TestCompileRefusesNonBoolean(t *testing.T) {
	for _, src := range []string{
		`record.owner`,
		`1 + 1`,
		`"yes"`,
		`actor.roles`,
	} {
		if _, err := Compile(src); !errors.Is(err, ErrNotBoolean) {
			t.Fatalf("Compile(%q): err = %v, want ErrNotBoolean", src, err)
		}
	}
}

func TestCompileRefusesMalformed(t *testing.T) {
	for _, src := range []string{
		``,
		`record.owner ==`,
		`record.owner === actor.id`,
		`)(`,
		`record.owner == actor.id &&`,
	} {
		if _, err := Compile(src); err == nil {
			t.Fatalf("Compile(%q) succeeded; want a compile error", src)
		}
	}
}

// The environment is the security boundary (ADR-022): two bindings, and
// nothing that reaches host state, the clock, the network, or an unknown
// identifier. Each of these must fail to *compile* — an undeclared reference is
// caught by the type checker, not discovered at evaluation.
func TestEnvironmentIsClosed(t *testing.T) {
	for _, src := range []string{
		`request.headers["cookie"] != ""`, // no ambient request
		`now() > timestamp("2020-01-01T00:00:00Z")`,
		`os.Getenv("LASTERP_DSN") != ""`,
		`db.query("select 1")`,
		`tenant == "t1"`, // tenant is under actor, not a bare binding
		`user.id == "u1"`,
		`secrets.get("acme_api_key") != ""`,
	} {
		if _, err := Compile(src); err == nil {
			t.Fatalf("Compile(%q) succeeded; the environment must not expose it", src)
		}
	}
}

// A missing key is an evaluation error, and an evaluation error denies. The
// dynamic-map environment is what makes this the behaviour rather than a
// compile error, so it is asserted rather than assumed: a condition naming a
// field an overlay has not added yet must deny, never allow.
func TestMissingFieldDenies(t *testing.T) {
	prg, err := Compile(`record.owner == actor.id`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	ok, err := prg.Eval(map[string]any{"other": "u1"}, Actor{ID: "u1"})
	if err == nil {
		t.Fatal("Eval over a record without the field returned no error; want one")
	}
	if ok {
		t.Fatal("Eval returned true for a missing field — it must deny")
	}
}

// The cost budget turns a pathological expression into a denied grant rather
// than a stalled request. The non-vacuity half matters as much as the refusal:
// a cheap expression over the same data must still evaluate, or the budget
// would only be proving that the input was large.
func TestCostBudgetDenies(t *testing.T) {
	big := make([]any, 5000)
	for i := range big {
		big[i] = "role"
	}
	rec := map[string]any{"xs": big}

	cheap, err := Compile(`size(record.xs) > 0`)
	if err != nil {
		t.Fatalf("Compile(cheap): %v", err)
	}
	ok, err := cheap.Eval(rec, Actor{ID: "u1"})
	if err != nil || !ok {
		t.Fatalf("cheap expression over the same record: ok = %v, err = %v; want true, nil", ok, err)
	}

	costly, err := Compile(`record.xs.all(a, record.xs.all(b, a == b))`)
	if err != nil {
		t.Fatalf("Compile(costly): %v", err)
	}
	ok, err = costly.Eval(rec, Actor{ID: "u1"})
	if err == nil {
		t.Fatal("a quadratic comprehension over 5000 elements evaluated within budget; the budget is not binding")
	}
	// Asserted on the reason, not just on failure: any bug that made this
	// expression fail for some other cause would otherwise report the budget
	// as working when it is not being reached at all.
	if !strings.Contains(err.Error(), "cost limit exceeded") {
		t.Fatalf("Eval failed for the wrong reason: %v", err)
	}
	if ok {
		t.Fatal("Eval returned true after exceeding the cost budget — it must deny")
	}
}

func TestGetCachesCompilation(t *testing.T) {
	const src = `record.owner == actor.id`
	a, err := Get(src)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, err := Get(src)
	if err != nil {
		t.Fatalf("Get (second): %v", err)
	}
	if a != b {
		t.Fatal("Get returned a different program for the same source; the cache is not being used")
	}
	if a.Source() != src {
		t.Fatalf("Source() = %q, want %q", a.Source(), src)
	}
}

// A stored condition that no longer compiles must fail the same way every time,
// without re-parsing on every request — and must still deny.
func TestGetCachesTheCompileError(t *testing.T) {
	const src = `record.owner ==`
	if _, err := Get(src); err == nil {
		t.Fatal("Get on a malformed expression returned no error")
	}
	if _, err := Get(src); err == nil {
		t.Fatal("Get on a malformed expression returned no error on the second call")
	}
}

func TestEvalOnNilProgramDenies(t *testing.T) {
	var p *Program
	ok, err := p.Eval(nil, Actor{ID: "u1"})
	if ok || err == nil {
		t.Fatalf("nil program: ok = %v, err = %v; want false and an error", ok, err)
	}
	if !strings.Contains(err.Error(), "expr:") {
		t.Fatalf("error is not wrapped with package context: %v", err)
	}
}
