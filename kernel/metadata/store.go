// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Layer identifies which customization layer a stored schema definition
// belongs to (ADR-006).
type Layer string

const (
	LayerCore   Layer = "core"
	LayerModule Layer = "module"
	LayerPlugin Layer = "plugin"
	LayerTenant Layer = "tenant"
)

// coreTenant is the sentinel tenant_id core-layer rows use: core schemas
// are shared across every tenant, not tenant-specific (see the RLS policy
// in 0016_enable_rls_object_schemas.postgres.sql, which admits layer =
// 'core' rows regardless of session tenant context).
const coreTenant tenancy.ID = ""

// StoredSchema is a persisted object_schemas row.
type StoredSchema struct {
	TenantID   tenancy.ID
	Name       string
	Layer      Layer
	Version    int
	Definition []byte
	Checksum   string
	CreatedAt  time.Time
}

// ErrSchemaNotFound is returned when no schema row matches.
var ErrSchemaNotFound = errors.New("metadata: schema not found")

// SaveObjectSchema persists definition for (tenant, name, layer, version).
// Pass tenant = "" (or use LayerCore) for a core-layer definition — it
// will be stored under the shared sentinel tenant_id regardless.
func SaveObjectSchema(ctx context.Context, db *storage.DB, tenant tenancy.ID, layer Layer, name string, version int, definition []byte) error {
	if name == "" {
		return errors.New("metadata: name is required")
	}
	storeTenant := tenant
	if layer == LayerCore {
		storeTenant = coreTenant
	}
	sum := sha256.Sum256(definition)
	checksum := hex.EncodeToString(sum[:])

	err := tenancy.WithTenant(ctx, db, storeTenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO object_schemas (tenant_id, name, layer, version, definition, checksum, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			string(storeTenant), name, string(layer), version, string(definition), checksum, time.Now().UTC())
		return err
	})
	if err != nil {
		return fmt.Errorf("metadata: save object schema: %w", err)
	}
	return nil
}

// LoadObjectSchema returns the latest version of (tenant, name, layer).
func LoadObjectSchema(ctx context.Context, db *storage.DB, tenant tenancy.ID, layer Layer, name string) (*StoredSchema, error) {
	lookupTenant := tenant
	if layer == LayerCore {
		lookupTenant = coreTenant
	}

	var s StoredSchema
	err := tenancy.WithTenant(ctx, db, lookupTenant, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT tenant_id, name, layer, version, definition, checksum, created_at
			FROM object_schemas
			WHERE tenant_id = ? AND name = ? AND layer = ?
			ORDER BY version DESC LIMIT 1`),
			string(lookupTenant), name, string(layer))

		var tenantStr, layerStr, definition string
		var createdAt storage.Time
		scanErr := row.Scan(&tenantStr, &s.Name, &layerStr, &s.Version, &definition, &s.Checksum, &createdAt)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return ErrSchemaNotFound
		}
		if scanErr != nil {
			return scanErr
		}
		s.TenantID = tenancy.ID(tenantStr)
		s.Layer = Layer(layerStr)
		s.Definition = []byte(definition)
		s.CreatedAt = createdAt.Time
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &s, nil
}
