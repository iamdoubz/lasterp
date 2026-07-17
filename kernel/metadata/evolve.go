// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"errors"
	"fmt"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/storage"
)

// ErrDestructiveDiff is returned by PlanEvolution when the new schema would
// require a destructive change to an already-populated table: dropping a
// core field, narrowing a column's type, or tightening nullability. Per
// docs/03 ("no destructive DDL within a major version; expand → backfill →
// contract"), WP-1.0a plans the expand half only — the contract half needs
// a backfill and a major-version boundary (WP-1.0a decisions, decision 7).
var ErrDestructiveDiff = errors.New("metadata: destructive schema diff refused")

// ErrNonMonotonicVersion is returned when a schema version is applied out of
// order (target ≤ the version being evolved from).
var ErrNonMonotonicVersion = errors.New("metadata: schema version applied out of order")

// StepKind is the kind of additive ALTER a PlanStep represents.
type StepKind int

const (
	// AddColumn adds a new (nullable) core-field column.
	AddColumn StepKind = iota
	// WidenColumn widens a core field's column type in place (lossless).
	WidenColumn
	// AddIndex adds a tenant-scoped index on a core field.
	AddIndex
	// DropNotNull loosens a core field from required to optional.
	DropNotNull
)

// PlanStep is one additive step in a schema evolution.
type PlanStep struct {
	Kind    StepKind
	Field   string
	FromCol string // WidenColumn: the column's SQL type before
	ToCol   string // AddColumn/WidenColumn: the column's SQL type after
}

// Plan is the ordered set of additive ALTERs that evolve an object's table
// from FromVersion to ToVersion. An empty Steps slice means the two versions
// are storage-equivalent (e.g. the diff only touched overlay/custom_fields
// or relabel-only attributes) — the version still advances.
type Plan struct {
	Object      string
	FromVersion int
	ToVersion   int
	Steps       []PlanStep
}

// widenable[[2]string{from, to}] is true when a column of SQL type `from` can
// be widened in place to `to` without losing data. Every allowed widening
// targets TEXT — the universal lossless representation — so the Postgres
// USING cast is always a plain `::TEXT` (no boolean→int special-casing).
var widenable = map[[2]string]bool{
	{"BOOLEAN", "TEXT"}:     true,
	{"INT", "TEXT"}:         true,
	{"TIMESTAMPTZ", "TEXT"}: true,
}

// PlanEvolution diffs the last-applied effective schema (prev) against the
// new one (next) and returns the additive ALTER plan, or ErrDestructiveDiff /
// ErrNonMonotonicVersion. Only core (non-overlay) fields produce steps:
// overlay fields live in the custom_fields blob and never alter the table
// (ADR-006 + WP-0.5 decision 8). Fields are identified by name.
func PlanEvolution(prev, next *EffectiveSchema, from, to int) (*Plan, error) {
	if to <= from {
		return nil, fmt.Errorf("%w: cannot evolve %s from v%d to v%d", ErrNonMonotonicVersion, next.ObjectName, from, to)
	}

	prevCore := coreFieldMap(prev)
	nextCore := coreFieldMap(next)

	// A removed core field is a column drop — a contract step, refused.
	for name := range prevCore {
		if _, ok := nextCore[name]; !ok {
			return nil, fmt.Errorf("%w: field %q was removed (dropping a column is a contract step, not expand)", ErrDestructiveDiff, name)
		}
	}

	plan := &Plan{Object: next.ObjectName, FromVersion: from, ToVersion: to}
	// Iterate next.Fields (not the map) so step order is deterministic.
	for _, f := range next.Fields {
		if f.FromOverlay {
			continue
		}
		newCol, err := columnType(f.Type)
		if err != nil {
			return nil, err
		}

		pf, existed := prevCore[f.Name]
		if !existed {
			// A new required field cannot be added to a populated table: it
			// would need NOT NULL with no default. The author must add it
			// nullable + backfill, then enforce in a later contract step
			// (WP-1.0a decisions, decision 4; Dan 2026-07-17).
			if f.Required {
				return nil, fmt.Errorf("%w: new field %q is required — add it nullable and backfill, then enforce NOT NULL in a contract step", ErrDestructiveDiff, f.Name)
			}
			plan.Steps = append(plan.Steps, PlanStep{Kind: AddColumn, Field: f.Name, ToCol: newCol})
			if f.Index {
				plan.Steps = append(plan.Steps, PlanStep{Kind: AddIndex, Field: f.Name})
			}
			continue
		}

		oldCol, err := columnType(pf.Type)
		if err != nil {
			return nil, err
		}
		if oldCol != newCol {
			if !widenable[[2]string{oldCol, newCol}] {
				return nil, fmt.Errorf("%w: field %q changes column type %s→%s, which is not a lossless widening", ErrDestructiveDiff, f.Name, oldCol, newCol)
			}
			plan.Steps = append(plan.Steps, PlanStep{Kind: WidenColumn, Field: f.Name, FromCol: oldCol, ToCol: newCol})
		}

		switch {
		case f.Required && !pf.Required:
			return nil, fmt.Errorf("%w: field %q becomes required — NOT NULL on a populated table needs a backfill+contract step", ErrDestructiveDiff, f.Name)
		case !f.Required && pf.Required:
			plan.Steps = append(plan.Steps, PlanStep{Kind: DropNotNull, Field: f.Name})
		}

		if f.Index && !pf.Index {
			plan.Steps = append(plan.Steps, PlanStep{Kind: AddIndex, Field: f.Name})
		}
		// Index removal leaves a stale-but-harmless index; no data risk, no
		// AC need — not planned (WP-1.0a decisions).
	}
	return plan, nil
}

// DDL renders the plan as executable SQL for dialect. Widen/loosen steps are
// Postgres-only: SQLite has no ALTER COLUMN TYPE / DROP NOT NULL, but its
// type affinity already stores values flexibly, so the data round-trips
// without a physical alter (WP-1.0a decisions, decision 3).
func (p *Plan) DDL(dialect storage.Dialect) string {
	if len(p.Steps) == 0 {
		return ""
	}
	table := TableName(p.Object)
	var b strings.Builder
	for _, s := range p.Steps {
		switch s.Kind {
		case AddColumn:
			fmt.Fprintf(&b, "ALTER TABLE %s ADD COLUMN %s %s;\n", table, s.Field, s.ToCol)
		case WidenColumn:
			if dialect == storage.Postgres {
				fmt.Fprintf(&b, "ALTER TABLE %s ALTER COLUMN %s TYPE %s USING (%s::%s);\n", table, s.Field, s.ToCol, s.Field, s.ToCol)
			}
		case AddIndex:
			fmt.Fprintf(&b, "CREATE INDEX idx_%s_%s ON %s (tenant_id, %s);\n", table, s.Field, table, s.Field)
		case DropNotNull:
			if dialect == storage.Postgres {
				fmt.Fprintf(&b, "ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL;\n", table, s.Field)
			}
		}
	}
	return b.String()
}

// coreFieldMap indexes an effective schema's core (non-overlay) fields by name.
func coreFieldMap(s *EffectiveSchema) map[string]Field {
	m := make(map[string]Field, len(s.Fields))
	for _, f := range s.Fields {
		if f.FromOverlay {
			continue
		}
		m[f.Name] = f
	}
	return m
}
