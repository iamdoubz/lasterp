// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// TenantSource is the `source` value of a tenant's own overlay, as opposed to
// one a plugin brought with it. Empty rather than a sentinel word: a plugin id
// is a reverse-DNS name and can never collide with it.
const TenantSource = ""

// ErrOverlayTarget is returned when an overlay names an object this host has
// not registered.
//
// Refused rather than ignored, and refused at the moment the overlay is stored
// rather than at first use, for the reason the plugin manifest refuses a
// capability it cannot honour: a customization that "saves fine" and then
// changes nothing is indistinguishable from one that was silently dropped.
// A *fully custom* object — one the overlay itself defines — is out of this
// WP's scope (WP-3.2c-decisions.md §2); it needs per-tenant DDL and a route
// table the gateway does not have.
var ErrOverlayTarget = errors.New("metadata: overlay targets an object this host has not registered")

// ParseOverlay parses and shape-checks one overlay document
// (WP-3.2c-decisions.md §5). It does not check the overlay against a core
// object — that is Merge's job, and it needs the object.
//
// An unknown key is an error, not a no-op: the same call ParseManifest makes,
// and for the same reason. An administrator who writes `add_field:` and gets a
// silent success has been told their customization landed when it did not.
func ParseOverlay(data []byte) (*Overlay, error) {
	var ov Overlay
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&ov); err != nil {
		return nil, fmt.Errorf("metadata: parse overlay: %w", err)
	}
	if ov.Object == "" {
		return nil, fmt.Errorf("%w: the document must name the object it targets", ErrInvalidObject)
	}
	if len(ov.AddFields) == 0 && len(ov.NarrowOptions) == 0 && len(ov.Permissions) == 0 {
		return nil, fmt.Errorf("%w: overlay for %q changes nothing", ErrInvalidObject, ov.Object)
	}
	return &ov, nil
}

// StoredOverlay is one persisted object_overlays row.
type StoredOverlay struct {
	ObjectName string
	Layer      Layer
	Source     string
	Definition []byte
	UpdatedAt  time.Time
	UpdatedBy  string
}

// SaveOverlay stores (or replaces) one overlay layer for one object.
//
// core is the object the overlay targets, and the overlay is merged against it
// here: a document that would widen an option set, lower a permission floor or
// collide with an existing field is refused *before* it is stored (INV-T3).
// Storing first and failing at resolve time would leave a tenant whose every
// request 500s on a row they cannot see, which is a worse failure than a 422.
//
// The merge runs against the tenant's other layers too, in the order Resolve
// will use them, because a document that is legal alone can still be illegal
// on top of what is already there.
func SaveOverlay(ctx context.Context, db *storage.DB, tenant tenancy.ID, core *Object, layer Layer, source string, definition []byte, actor string) error {
	if err := CheckOverlay(ctx, db, tenant, core, layer, source, definition); err != nil {
		return err
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return SaveOverlayTx(ctx, db, tx, tenant, core.ObjectName, layer, source, definition, actor)
	})
	if err != nil {
		return fmt.Errorf("metadata: save overlay for %s: %w", core.ObjectName, err)
	}
	return nil
}

// CheckOverlay is SaveOverlay's refusal half: it merges definition onto core in
// the position SaveOverlayTx would store it, against the tenant's other layers,
// and returns what Merge would have said.
//
// It exists apart from the write so a caller with its own transaction — plugin
// install, which must not commit a plugin row whose overlay turns out to be
// illegal — can refuse before opening it.
func CheckOverlay(ctx context.Context, db *storage.DB, tenant tenancy.ID, core *Object, layer Layer, source string, definition []byte) error {
	if tenant == "" {
		return errors.New("metadata: tenant is required")
	}
	if layer != LayerPlugin && layer != LayerTenant {
		return fmt.Errorf("metadata: overlays live in the %q or %q layer, not %q", LayerPlugin, LayerTenant, layer)
	}
	ov, err := ParseOverlay(definition)
	if err != nil {
		return err
	}
	if core == nil || ov.Object != core.ObjectName {
		return fmt.Errorf("%w: %q", ErrOverlayTarget, ov.Object)
	}
	existing, err := LoadOverlays(ctx, db, tenant, core.ObjectName)
	if err != nil {
		return err
	}
	incoming := StoredOverlay{ObjectName: core.ObjectName, Layer: layer, Source: source, Definition: definition}
	_, err = mergeStored(core, replaceLayer(existing, incoming))
	return err
}

// SaveOverlayTx writes one overlay layer inside a transaction the caller holds.
// It does not validate: pair it with CheckOverlay, which SaveOverlay does.
func SaveOverlayTx(ctx context.Context, db *storage.DB, tx *sql.Tx, tenant tenancy.ID, object string, layer Layer, source string, definition []byte, actor string) error {
	now := time.Now().UTC()
	// Upsert by hand rather than with ON CONFLICT: the two dialects spell it
	// differently and the storage adapter deliberately speaks the intersection
	// (ADR-005). Both statements are inside one transaction, so a concurrent
	// writer sees either the old row or the new one.
	res, err := tx.ExecContext(ctx, db.Rebind(`
		UPDATE object_overlays SET definition = ?, updated_at = ?, updated_by = ?
		WHERE tenant_id = ? AND object_name = ? AND layer = ? AND source = ?`),
		string(definition), now, actor,
		string(tenant), object, string(layer), source)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO object_overlays
			(tenant_id, object_name, layer, source, definition, created_at, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		string(tenant), object, string(layer), source, string(definition), now, now, actor)
	return err
}

// LoadOverlays returns every overlay layer for one object in one tenant, in
// merge order (see sortOverlays).
func LoadOverlays(ctx context.Context, db *storage.DB, tenant tenancy.ID, object string) ([]StoredOverlay, error) {
	var out []StoredOverlay
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT object_name, layer, source, definition, updated_at, updated_by
			FROM object_overlays WHERE tenant_id = ? AND object_name = ?`),
			string(tenant), object)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		var list []StoredOverlay
		for rows.Next() {
			var s StoredOverlay
			var layer, definition string
			var updatedAt storage.Time
			if err := rows.Scan(&s.ObjectName, &layer, &s.Source, &definition, &updatedAt, &s.UpdatedBy); err != nil {
				return err
			}
			s.Layer = Layer(layer)
			s.Definition = []byte(definition)
			s.UpdatedAt = updatedAt.Time
			list = append(list, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: load overlays for %s: %w", object, err)
	}
	sortOverlays(out)
	return out, nil
}

// ListOverlays returns every overlay in the tenant, across objects, in merge
// order within each object. It is the admin surface's read and the export
// half of "customization packages" (ADR-006).
func ListOverlays(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]StoredOverlay, error) {
	var out []StoredOverlay
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT object_name, layer, source, definition, updated_at, updated_by
			FROM object_overlays WHERE tenant_id = ?`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()

		var list []StoredOverlay
		for rows.Next() {
			var s StoredOverlay
			var layer, definition string
			var updatedAt storage.Time
			if err := rows.Scan(&s.ObjectName, &layer, &s.Source, &definition, &updatedAt, &s.UpdatedBy); err != nil {
				return err
			}
			s.Layer = Layer(layer)
			s.Definition = []byte(definition)
			s.UpdatedAt = updatedAt.Time
			list = append(list, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: list overlays: %w", err)
	}
	sortOverlays(out)
	return out, nil
}

// DeleteOverlay removes one layer. Deleting an absent one is not an error:
// the caller asked for it to be gone and it is.
func DeleteOverlay(ctx context.Context, db *storage.DB, tenant tenancy.ID, object string, layer Layer, source string) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			DELETE FROM object_overlays
			WHERE tenant_id = ? AND object_name = ? AND layer = ? AND source = ?`),
			string(tenant), object, string(layer), source)
		return err
	})
	if err != nil {
		return fmt.Errorf("metadata: delete overlay for %s: %w", object, err)
	}
	return nil
}

// DeleteOverlaysBySourceTx removes every overlay one source owns, across
// objects, inside a transaction the caller already holds.
//
// It takes the tx rather than opening its own because its one caller is plugin
// uninstall, which must drop the plugin row and the schema changes that came
// with it together: an overlay outliving its plugin is a field nobody can find
// the owner of, and a plugin row outliving its overlay is a reinstall that
// silently inherits one.
func DeleteOverlaysBySourceTx(ctx context.Context, db *storage.DB, tx *sql.Tx, tenant tenancy.ID, source string) error {
	_, err := tx.ExecContext(ctx, db.Rebind(
		`DELETE FROM object_overlays WHERE tenant_id = ? AND source = ?`), string(tenant), source)
	if err != nil {
		return fmt.Errorf("metadata: delete overlays for %s: %w", source, err)
	}
	return nil
}

// Resolve returns core with this tenant's overlays merged in — the effective
// schema the CRUD engine, the metadata endpoint and the replica all operate on
// (WP-3.2c-decisions.md §7).
//
// ponytail: one indexed SELECT per call, no cache. A cache that is correct
// across nodes needs a per-tenant generation to validate against, which is the
// same round trip it was avoiding; and object_overlays is empty for a tenant
// that has customized nothing, which is most of them. If this read ever shows
// up in a profile the upgrade is a `tenants.overlay_generation` counter bumped
// by SaveOverlay/DeleteOverlay and an in-process cache validated against it —
// not a TTL, which would serve a schema the tenant has already changed.
func Resolve(ctx context.Context, db *storage.DB, tenant tenancy.ID, core *Object) (*EffectiveSchema, error) {
	stored, err := LoadOverlays(ctx, db, tenant, core.ObjectName)
	if err != nil {
		return nil, err
	}
	return mergeStored(core, stored)
}

// DBResolver adapts Resolve to the seam kernel/api and the sync resolver take,
// so those packages depend on a two-method surface rather than on a *storage.DB
// they would then have to thread through their own handlers.
type DBResolver struct{ DB *storage.DB }

// Resolve implements the gateway's SchemaResolver.
func (r DBResolver) Resolve(ctx context.Context, tenant tenancy.ID, core *Object) (*EffectiveSchema, error) {
	return Resolve(ctx, r.DB, tenant, core)
}

// mergeStored parses each stored document and folds it onto core in order.
//
// A row that no longer parses or no longer merges fails the resolve rather than
// being skipped. Skipping would serve a schema quietly narrower than the one
// the tenant configured — and on the write path, "quietly narrower" means
// refusing values the administrator believes are legal, with nothing anywhere
// saying why.
func mergeStored(core *Object, stored []StoredOverlay) (*EffectiveSchema, error) {
	if len(stored) == 0 {
		return Merge(core)
	}
	overlays := make([]Overlay, 0, len(stored))
	for _, s := range stored {
		ov, err := ParseOverlay(s.Definition)
		if err != nil {
			return nil, fmt.Errorf("metadata: overlay %s/%s on %s: %w", s.Layer, s.Source, s.ObjectName, err)
		}
		ov.Layer = string(s.Layer)
		overlays = append(overlays, *ov)
	}
	eff, err := Merge(core, overlays...)
	if err != nil {
		return nil, err
	}
	return eff, nil
}

// layerRank orders the ADR-006 customization stack: core ⊕ module ⊕ plugin ⊕
// tenant.
//
// Written out rather than left to `ORDER BY layer`, which is right today only
// because "plugin" happens to sort before "tenant" — a rename would reverse the
// stack silently. Order is load-bearing: narrowing composes monotonically only
// because each layer sees what the one before it left, so the tenant
// administrator gets the last word by construction rather than by luck.
var layerRank = map[Layer]int{LayerCore: 0, LayerModule: 1, LayerPlugin: 2, LayerTenant: 3}

// sortOverlays puts overlays in merge order in place: by object, then by layer,
// then by source within a layer, so two plugins overlaying one object always
// merge in the same order and an error names the same one first.
func sortOverlays(list []StoredOverlay) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.ObjectName != b.ObjectName {
			return a.ObjectName < b.ObjectName
		}
		if ra, rb := layerRank[a.Layer], layerRank[b.Layer]; ra != rb {
			return ra < rb
		}
		return a.Source < b.Source
	})
}

// replaceLayer returns list with incoming substituted for the layer it
// occupies (or appended), so SaveOverlay validates the stack it is about to
// create rather than the one that exists.
func replaceLayer(list []StoredOverlay, incoming StoredOverlay) []StoredOverlay {
	out := make([]StoredOverlay, 0, len(list)+1)
	for _, s := range list {
		if s.Layer == incoming.Layer && s.Source == incoming.Source {
			continue
		}
		out = append(out, s)
	}
	out = append(out, incoming)
	sortOverlays(out)
	return out
}
