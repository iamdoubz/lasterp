package metadata

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// sampleContactWithStatusYAML is sampleContactYAML plus an enum, so the
// narrowing half of INV-T3 has something to narrow.
const sampleContactWithStatusYAML = `
object: Contact
module: crm
persistence: crud
fields:
  - {name: full_name, type: text, required: true}
  - {name: email, type: email}
  - {name: status, type: enum, options: [lead, active, churned]}
permissions:
  read: [crm.viewer]
  create: [crm.user]
  update: [crm.user]
  delete: [crm.admin]
`

const loyaltyOverlayYAML = `
object: Contact
add_fields:
  - {name: loyalty_tier, type: enum, options: [bronze, silver, gold]}
`

func contactWithStatus(t *testing.T) *Object {
	t.Helper()
	o, err := ParseObject([]byte(sampleContactWithStatusYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	return o
}

func TestParseOverlay(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "adds a field", yaml: loyaltyOverlayYAML},
		{name: "narrows an option set", yaml: "object: Contact\nnarrow_options:\n  status: [lead, active]\n"},
		{name: "raises a permission floor", yaml: "object: Contact\npermissions:\n  read: [crm.viewer, crm.exec]\n"},
		{
			name:    "no object named",
			yaml:    "add_fields:\n  - {name: vip, type: bool}\n",
			wantErr: "must name the object",
		},
		{
			name:    "changes nothing",
			yaml:    "object: Contact\n",
			wantErr: "changes nothing",
		},
		{
			// A typo in a verb is the failure ParseManifest's KnownFields(true)
			// exists to prevent: a customization that saves and does nothing.
			name:    "unknown verb",
			yaml:    "object: Contact\nadd_field:\n  - {name: vip, type: bool}\n",
			wantErr: "add_field",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseOverlay([]byte(tc.yaml))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ParseOverlay: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// A field type ParseOverlay accepts is still refused by Merge, which is the
// only place holding the core object to check it against. The split matters:
// ParseOverlay is a shape check, SaveOverlay is where a document meets the
// schema it customizes.
func TestOverlayFieldTypeIsCheckedAtMerge(t *testing.T) {
	ov, err := ParseOverlay([]byte("object: Contact\nadd_fields:\n  - {name: vip, type: not_a_type}\n"))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	if _, err := Merge(contactWithStatus(t), *ov); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("err = %v, want ErrInvalidObject", err)
	}
}

func TestSaveLoadAndResolveOverlay(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			core := contactWithStatus(t)

			if err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource,
				[]byte(loyaltyOverlayYAML), "admin@example.test"); err != nil {
				t.Fatalf("SaveOverlay: %v", err)
			}

			stored, err := LoadOverlays(ctx, db, tenant, "Contact")
			if err != nil {
				t.Fatalf("LoadOverlays: %v", err)
			}
			if len(stored) != 1 {
				t.Fatalf("got %d overlays, want 1", len(stored))
			}
			// Stored verbatim: the record is the document the administrator
			// wrote, not this host's re-marshalling of it.
			if string(stored[0].Definition) != loyaltyOverlayYAML {
				t.Fatalf("Definition = %q, want it stored verbatim", stored[0].Definition)
			}
			if stored[0].UpdatedBy != "admin@example.test" {
				t.Fatalf("UpdatedBy = %q, want the actor (INV-T4)", stored[0].UpdatedBy)
			}

			eff, err := Resolve(ctx, db, tenant, core)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			i := fieldIndex(eff.Fields, "loyalty_tier")
			if i < 0 {
				t.Fatal("resolved schema has no loyalty_tier field")
			}
			if !eff.Fields[i].FromOverlay {
				t.Fatal("loyalty_tier is not marked FromOverlay, so it would become a physical column")
			}

			// Replacing the layer overwrites rather than accumulating.
			const narrowed = "object: Contact\nnarrow_options:\n  status: [lead, active]\n"
			if err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource,
				[]byte(narrowed), "admin@example.test"); err != nil {
				t.Fatalf("SaveOverlay (replace): %v", err)
			}
			stored, err = LoadOverlays(ctx, db, tenant, "Contact")
			if err != nil {
				t.Fatalf("LoadOverlays: %v", err)
			}
			if len(stored) != 1 {
				t.Fatalf("got %d overlays after replace, want 1", len(stored))
			}
			eff, err = Resolve(ctx, db, tenant, core)
			if err != nil {
				t.Fatalf("Resolve after replace: %v", err)
			}
			if fieldIndex(eff.Fields, "loyalty_tier") >= 0 {
				t.Fatal("loyalty_tier survived being replaced; the layer accumulated instead of being overwritten")
			}
			if got := eff.Fields[fieldIndex(eff.Fields, "status")].Options; len(got) != 2 {
				t.Fatalf("status options = %v, want the narrowed pair", got)
			}

			if err := DeleteOverlay(ctx, db, tenant, "Contact", LayerTenant, TenantSource); err != nil {
				t.Fatalf("DeleteOverlay: %v", err)
			}
			eff, err = Resolve(ctx, db, tenant, core)
			if err != nil {
				t.Fatalf("Resolve after delete: %v", err)
			}
			if got := eff.Fields[fieldIndex(eff.Fields, "status")].Options; len(got) != 3 {
				t.Fatalf("status options = %v, want core's three back after the overlay was deleted", got)
			}
		})
	}
}

func TestSaveOverlayRefusesAnUnknownTargetObject(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			core := contactWithStatus(t)

			const elsewhere = "object: NotAThing\nadd_fields:\n  - {name: vip, type: bool}\n"
			if err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource,
				[]byte(elsewhere), "admin"); !errors.Is(err, ErrOverlayTarget) {
				t.Fatalf("err = %v, want ErrOverlayTarget", err)
			}
		})
	}
}

// Two layers on one object merge in ADR-006's order — plugin, then tenant —
// which is what lets the tenant administrator narrow what a plugin left.
func TestOverlayLayersMergeInStackOrder(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			core := contactWithStatus(t)

			if err := SaveOverlay(ctx, db, tenant, core, LayerPlugin, "com.acme.loyalty",
				[]byte(loyaltyOverlayYAML), "admin"); err != nil {
				t.Fatalf("SaveOverlay (plugin): %v", err)
			}
			// The tenant narrows the field the plugin added. This can only work
			// if the plugin layer merged first.
			const tenantNarrows = "object: Contact\nnarrow_options:\n  loyalty_tier: [bronze, silver]\n"
			if err := SaveOverlay(ctx, db, tenant, core, LayerTenant, TenantSource,
				[]byte(tenantNarrows), "admin"); err != nil {
				t.Fatalf("SaveOverlay (tenant): %v", err)
			}

			stored, err := LoadOverlays(ctx, db, tenant, "Contact")
			if err != nil {
				t.Fatalf("LoadOverlays: %v", err)
			}
			if len(stored) != 2 || stored[0].Layer != LayerPlugin || stored[1].Layer != LayerTenant {
				t.Fatalf("merge order = %v, want plugin then tenant", stored)
			}

			eff, err := Resolve(ctx, db, tenant, core)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			got := eff.Fields[fieldIndex(eff.Fields, "loyalty_tier")].Options
			if len(got) != 2 || got[0] != "bronze" || got[1] != "silver" {
				t.Fatalf("loyalty_tier options = %v, want [bronze silver]", got)
			}
		})
	}
}
