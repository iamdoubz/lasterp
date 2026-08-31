// SPDX-License-Identifier: AGPL-3.0-only

// Package expr is the WP-3.3a expression seam: CEL, in a closed environment,
// for the conditions on RBAC grants (docs/08 §AuthZ) and — from WP-3.3b — on
// automations.
//
// It is the **only** place in the tree that imports cel-go ([ADR-022]). That is
// not tidiness: the environment is the security boundary, and an environment
// widened from a call site is one nobody reviews. TestCELIsImportedOnlyHere
// asserts the rule.
//
// Two bindings exist and no others:
//
//	record  the object's field map
//	actor   {id, tenant, roles}
//
// No extension functions, no host state, no clock, no network, no protobuf
// message types. An expression cannot reach anything the caller did not hand it.
//
// Everything here is fail-closed. A program that does not compile is refused
// before it is stored; an evaluation that errors, exceeds its cost budget, or
// returns a non-boolean is false. There is deliberately no API that lets a
// caller distinguish "false" from "broken" *as an authorization outcome* — Eval
// returns both the verdict and the error, and the authz gate treats them the
// same way (docs/notes/WP-3.3-decisions.md §2).
//
// [ADR-022]: ../../docs/adr/ADR-022-expression-language.md
package expr

import (
	"errors"
	"fmt"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// Binding names. Exported so callers building an Input, and the tests that
// probe the environment, agree on one spelling.
const (
	BindRecord = "record"
	BindActor  = "actor"
)

// costLimit bounds one evaluation. CEL costs are abstract units, not
// nanoseconds; this is set high enough that no expression a human writes over a
// record's fields approaches it, and low enough that a pathological
// comprehension over a large map is a denied grant rather than a stalled
// request. Exceeding it is an error, and an error denies (see Program.Eval).
const costLimit uint64 = 100_000

// ErrNotBoolean is returned when an expression compiles but does not yield a
// bool. A condition that answers with a string is not a condition, and
// coercing one to "truthy" is how a permission widens by accident.
var ErrNotBoolean = errors.New("expr: condition must evaluate to a boolean")

// Actor is the subject half of the environment: who is asking.
//
// Deliberately not authz.Actor — this package is a leaf and importing authz
// would invert the dependency. Roles are names, and a condition can only
// narrow, so exposing them grants nothing (WP-3.3-decisions.md §4).
type Actor struct {
	ID     string
	Tenant string
	Roles  []string
}

func (a Actor) activation() map[string]any {
	roles := make([]any, len(a.Roles))
	for i, r := range a.Roles {
		roles[i] = r
	}
	return map[string]any{"id": a.ID, "tenant": a.Tenant, "roles": roles}
}

// env is the one CEL environment. Built once: construction parses the
// declarations, and doing it per compile would be the cost nobody measures.
var env = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(
		// Both bindings are dynamic maps. A typed environment would mean
		// compiling a grant against one object's effective schema and
		// recompiling every grant when an overlay adds a field (ADR-006) —
		// a schema-change fan-out for a check that is fail-closed anyway:
		// a missing key is an evaluation error, which denies.
		cel.Variable(BindRecord, cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable(BindActor, cel.MapType(cel.StringType, cel.DynType)),
	)
})

// Program is a compiled, reusable condition. Safe for concurrent use.
type Program struct {
	src string
	prg cel.Program
}

// Source returns the expression text this program was compiled from.
func (p *Program) Source() string { return p.src }

// Compile parses, type-checks and plans src. It is called at *grant* time: a
// condition that does not compile is refused rather than stored, so a
// mistyped rule fails where an administrator can see it instead of silently
// denying every request forever.
func Compile(src string) (*Program, error) {
	if src == "" {
		return nil, errors.New("expr: empty expression")
	}
	e, err := env()
	if err != nil {
		return nil, fmt.Errorf("expr: build environment: %w", err)
	}
	ast, issues := e.Compile(src)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("expr: compile %q: %w", src, issues.Err())
	}
	// Checked at compile time, not discovered at evaluation: a grant whose
	// condition answers with a string is malformed, and the administrator
	// should be told so while they are still typing it.
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("expr: compile %q: %w, got %s", src, ErrNotBoolean, ast.OutputType())
	}
	prg, err := e.Program(ast, cel.CostLimit(costLimit))
	if err != nil {
		return nil, fmt.Errorf("expr: plan %q: %w", src, err)
	}
	return &Program{src: src, prg: prg}, nil
}

// Eval evaluates the program against one record and actor.
//
// It returns false on every failure — an evaluation error, a cost overrun, a
// missing binding, a non-boolean result. The error is returned alongside for
// logging and for the tests that assert *why* something was denied, but no
// caller may treat "error" as anything but a denial.
func (p *Program) Eval(record map[string]any, actor Actor) (bool, error) {
	if p == nil || p.prg == nil {
		return false, errors.New("expr: nil program")
	}
	if record == nil {
		record = map[string]any{}
	}
	out, _, err := p.prg.Eval(map[string]any{
		BindRecord: record,
		BindActor:  actor.activation(),
	})
	if err != nil {
		return false, fmt.Errorf("expr: evaluate %q: %w", p.src, err)
	}
	return asBool(out, p.src)
}

// asBool refuses anything but a genuine CEL bool. types.Bool is the only
// accepted shape: CEL's own truthiness rules are narrower than a coercion
// would be, and we are narrower still.
func asBool(out ref.Val, src string) (bool, error) {
	b, ok := out.(types.Bool)
	if !ok {
		return false, fmt.Errorf("expr: evaluate %q: %w, got %s", src, ErrNotBoolean, out.Type())
	}
	return bool(b), nil
}

// cache memoises compiled programs by source text. Grants are read on every
// authorization decision and the condition text is the whole cache key —
// compilation is the expensive half and the result is immutable.
//
// ponytail: unbounded map, keyed by text that only a grant write can introduce.
// A tenant admin adding grants is the only way to grow it and each entry is one
// planned program. Swap for an LRU if a tenant ever accumulates conditions
// faster than roles.
var cache sync.Map // map[string]*cachedProgram

type cachedProgram struct {
	once sync.Once
	prg  *Program
	err  error
}

// Get returns the compiled program for src, compiling on first use.
//
// The compile error is cached too. A stored condition that no longer compiles
// (a cel-go upgrade tightening a rule, say) must not re-parse on every request
// — and it denies either way.
func Get(src string) (*Program, error) {
	v, _ := cache.LoadOrStore(src, &cachedProgram{})
	c := v.(*cachedProgram)
	c.once.Do(func() { c.prg, c.err = Compile(src) })
	return c.prg, c.err
}
