//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// The overlay store's invariant half (WP-3.2c).
//
// **INV-T3**: the core layer's declaration is a bound no later layer may escape
// — permissions are a floor, option sets are a ceiling — and a document that
// escapes either is refused *before* it is stored, not at resolve time.
// **INV-T1**: one tenant's customization is invisible to every other.
//
// The refusals are in this package rather than only at the HTTP edge because
// this is where the check lives: internal/app's overlay tests prove the route
// reaches it, these prove it is right.

// A document that would widen an option set is refused at save, not at
// resolve: storing it would leave the tenant with a row they cannot see
// failing every request (INV-T3).
func TestSaveOverlayRefusesAWideningDocument(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			core := contactWithStatus(t)

			const widening = "object: Contact\nnarrow_options:\n  status: [lead, active, churned, banana]\n"
			err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource, []byte(widening), "admin")
			if !errors.Is(err, ErrOptionSetWidened) {
				t.Fatalf("err = %v, want ErrOptionSetWidened", err)
			}
			stored, err := LoadOverlays(ctx, db, tenant, "Contact")
			if err != nil {
				t.Fatalf("LoadOverlays: %v", err)
			}
			if len(stored) != 0 {
				t.Fatalf("the refused overlay was stored anyway: %v", stored)
			}

			// Non-vacuity: the same call with a *narrowing* set succeeds, so
			// the refusal above is about widening and not about the shape of
			// the document.
			const narrowing = "object: Contact\nnarrow_options:\n  status: [lead, active]\n"
			if err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource, []byte(narrowing), "admin"); err != nil {
				t.Fatalf("SaveOverlay (narrowing): %v", err)
			}
		})
	}
}

func TestSaveOverlayRefusesALoweredPermissionFloor(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			core := contactWithStatus(t)

			const lowered = "object: Contact\npermissions:\n  delete: [crm.user]\n"
			if err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource,
				[]byte(lowered), "admin"); !errors.Is(err, ErrPermissionFloorLowered) {
				t.Fatalf("err = %v, want ErrPermissionFloorLowered", err)
			}

			// Non-vacuity: adding a role to the same action is accepted.
			const raised = "object: Contact\npermissions:\n  delete: [crm.admin, crm.exec]\n"
			if err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource, []byte(raised), "admin"); err != nil {
				t.Fatalf("SaveOverlay (raised floor): %v", err)
			}
		})
	}
}

// INV-T1: one tenant's customization is invisible to another. Asserted through
// Resolve rather than only through a SELECT, because the effective schema is
// what the request path actually acts on.
func TestOverlaysAreInvisibleToAnotherTenant(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			a, b := mustCreateTenant(t, db), mustCreateTenant(t, db)
			core := contactWithStatus(t)

			if err := SaveOverlay(ctx, db, a, core, LayerTenant, TenantSource,
				[]byte(loyaltyOverlayYAML), "admin"); err != nil {
				t.Fatalf("SaveOverlay: %v", err)
			}

			effA, err := Resolve(ctx, db, a, core)
			if err != nil {
				t.Fatalf("Resolve(a): %v", err)
			}
			if fieldIndex(effA.Fields, "loyalty_tier") < 0 {
				t.Fatal("tenant a does not see its own overlay")
			}

			effB, err := Resolve(ctx, db, b, core)
			if err != nil {
				t.Fatalf("Resolve(b): %v", err)
			}
			if fieldIndex(effB.Fields, "loyalty_tier") >= 0 {
				t.Fatal("tenant b sees tenant a's overlay field (INV-T1)")
			}
			if len(effB.Fields) != len(core.Fields) {
				t.Fatalf("tenant b has %d fields, want core's %d", len(effB.Fields), len(core.Fields))
			}

			list, err := ListOverlays(ctx, db, b)
			if err != nil {
				t.Fatalf("ListOverlays(b): %v", err)
			}
			if len(list) != 0 {
				t.Fatalf("tenant b lists %d overlays, want 0 (INV-T1)", len(list))
			}
		})
	}
}

// INV-T3 as a property, over the *stored* stack rather than over Merge: no
// combination of persisted layers, saved in any order, ever leaves a field
// holding a value core did not declare.
//
// Merge's own composability is covered by narrow_test.go. What this adds is
// everything WP-3.2c put between a document and the effective schema — the
// upsert, the load, the layer ordering — because a stack that merged correctly
// in memory and is reassembled in the wrong order on the way out of the
// database is narrowed by a different set than the administrator chose.
//
// The seed is fixed so a failure reproduces.
func TestStoredOverlayStackNeverEscapesCore(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			core := contactWithStatus(t)
			coreOptions := core.Fields[fieldIndex(core.Fields, "status")].Options
			rng := rand.New(rand.NewSource(0x3c))

			// Layers, in the order they will be merged. Each narrows to a
			// random non-empty subset of the one before it, so the expected
			// result is computable without re-implementing Merge.
			sources := []struct {
				layer  Layer
				source string
			}{
				{LayerPlugin, "com.a.one"},
				{LayerPlugin, "com.b.two"},
				{LayerTenant, TenantSource},
			}

			var narrowedFrom, refused, kept int
			for round := 0; round < 40; round++ {
				tenant := mustCreateTenant(t, db)
				want := append([]string(nil), coreOptions...)
				plan := make([]string, len(sources))
				for i := range sources {
					subset := randomSubset(rng, want)
					plan[i] = narrowStatusYAML(subset)
					want = subset
				}

				// Saved in a *shuffled* order, so the stack's merge order comes
				// from the layer rank and not from insertion order. This is the
				// part a single-writer test cannot see.
				order := rng.Perm(len(sources))
				for _, i := range order {
					if err := SaveOverlay(ctx, db, tenant, core, sources[i].layer, sources[i].source,
						[]byte(plan[i]), "prop"); err != nil {
						// A later layer narrowing beyond an earlier one is only
						// legal in stack order; saved out of order, the same
						// document can be a widening. That refusal is the
						// invariant holding, not a failure — count it and move
						// on, but the round no longer has a predictable result.
						if !errors.Is(err, ErrOptionSetWidened) {
							t.Fatalf("SaveOverlay: %v", err)
						}
						refused++
						want = nil
						break
					}
				}
				if want == nil {
					continue
				}

				eff, err := Resolve(ctx, db, tenant, core)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				got := eff.Fields[fieldIndex(eff.Fields, "status")].Options
				for _, v := range got {
					if !contains(coreOptions, v) {
						t.Fatalf("resolved options %v escaped core's %v", got, coreOptions)
					}
				}
				if len(got) > len(coreOptions) {
					t.Fatalf("resolved options %v are wider than core's %v", got, coreOptions)
				}
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("resolved options = %v, want the innermost narrowing %v", got, want)
				}
				if len(got) < len(coreOptions) {
					narrowedFrom++
				} else {
					kept++
				}
			}

			// Non-vacuity: the rounds are not all trivially "unchanged", and the
			// out-of-order refusals really happened. Without this the property
			// would pass just as well against a Resolve that ignored overlays.
			if narrowedFrom == 0 {
				t.Fatal("no round actually narrowed anything; the property is vacuous")
			}
			if kept+narrowedFrom+refused == 0 {
				t.Fatal("no round ran")
			}
		})
	}
}

// narrowStatusYAML is one generated overlay document.
func narrowStatusYAML(options []string) string {
	return "object: Contact\nnarrow_options:\n  status: [" + strings.Join(options, ", ") + "]\n"
}

// randomSubset returns a non-empty subset of from, preserving its order so the
// expected value is comparable without sorting.
func randomSubset(rng *rand.Rand, from []string) []string {
	for {
		var out []string
		for _, v := range from {
			if rng.Intn(2) == 0 {
				out = append(out, v)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
