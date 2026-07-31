//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/metadata"
)

// Boot assembly has to survive a second boot. Every `lasterp serve` runs Setup,
// which re-registers the built-in modules, so a restart against an existing
// database re-saves schemas that are already there. Before WP-1.5 that was a
// UNIQUE violation on object_schemas — the server came up exactly once per
// database and then refused to start, which no test covered because every
// harness booted a fresh database.

// TestSetupIsIdempotent is the restart path: Setup twice over one database.
func TestSetupIsIdempotent(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			// bootDBs already ran Migrate + Setup once; this is the restart.
			if err := Setup(ctx, db); err != nil {
				t.Fatalf("second Setup failed — the server cannot restart: %v", err)
			}
			if err := Setup(ctx, db); err != nil {
				t.Fatalf("third Setup failed: %v", err)
			}

			// And the server still works afterwards, rather than merely not
			// erroring during boot.
			e := seed(t, db)
			if status, body, _ := e.get("/api/v1/contact"); status != http.StatusOK {
				t.Fatalf("after restart, list = %d, want 200; body=%s", status, body)
			}
		})
	}
}

// Idempotence must not become "last write wins": changing a schema without
// bumping its version would silently rewrite what every tenant on that version
// is running against (ADR-006 expand→migrate→contract).
func TestSchemaVersionConflictIsRejected(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			const object = "RestartProbe"

			original := []byte(`{"object":"RestartProbe","fields":[{"name":"a","type":"text"}]}`)
			if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, object, 1, original); err != nil {
				t.Fatalf("initial save: %v", err)
			}
			// Byte-identical re-save is the idempotent path.
			if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, object, 1, original); err != nil {
				t.Fatalf("identical re-save should be a no-op: %v", err)
			}

			changed := []byte(`{"object":"RestartProbe","fields":[{"name":"b","type":"text"}]}`)
			err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, object, 1, changed)
			if !errors.Is(err, metadata.ErrSchemaVersionConflict) {
				t.Fatalf("changed definition at the same version = %v, want ErrSchemaVersionConflict", err)
			}

			// A new version is the correct move and must still work.
			if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, object, 2, changed); err != nil {
				t.Fatalf("save at a bumped version: %v", err)
			}
		})
	}
}
