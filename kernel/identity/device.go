// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ErrDeviceWiped is returned by ValidateSession when the session is itself
// valid but the device behind it has been remotely wiped.
//
// It is the one authentication outcome deliberately distinguishable from
// ErrSessionInvalid, whose whole design is to be undifferentiated so a caller
// cannot tell "wrong token" from "right token, revoked" — an oracle. The
// exception is not convenience: **a client that cannot tell a wipe from an
// expiry does not delete anything.** It signs the user out and leaves the
// replica on disk, which is exactly the outcome the control exists to prevent
// (WP-2.5-decisions.md §3).
//
// What it discloses is bounded: the recipient already holds a session for the
// device in question, and the fact disclosed — "this device was wiped" — is the
// message being deliberately delivered. It says nothing about other devices,
// users or tokens.
var ErrDeviceWiped = errors.New("identity: device has been wiped")

// Device is a machine that holds (or held) a replica.
type Device struct {
	ID       string
	TenantID tenancy.ID
	UserID   UserID
	Label    string

	CreatedAt  time.Time
	LastSeenAt time.Time

	RevokedAt       *time.Time
	WipeRequestedAt *time.Time
	WipeDeliveredAt *time.Time
}

// Wiped reports whether a wipe has been requested for this device.
func (d Device) Wiped() bool { return d.WipeRequestedAt != nil }

// Revoked reports whether the device may no longer be issued sessions.
func (d Device) Revoked() bool { return d.RevokedAt != nil }

// RegisterDevice records that a device exists and has just been seen.
//
// It is an upsert called from IssueSession rather than an enrolment endpoint a
// client must remember to hit: any enrolment step a client could skip is one
// that leaves an untracked replica, which is the thing this table exists to
// control (decisions §1). Re-registering an existing device only moves
// last_seen_at — it never resurrects a revoked or wiped one, because logging in
// again must not be a way to launder a device an administrator has acted on.
func RegisterDevice(ctx context.Context, db *storage.DB, tenant tenancy.ID, user UserID, deviceID string) error {
	if tenant == "" || user == "" || deviceID == "" {
		return errors.New("identity: tenant, user and device are required to register a device")
	}
	now := time.Now().UTC()
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO devices (tenant_id, id, user_id, label, created_at, last_seen_at)
			VALUES (?, ?, ?, '', ?, ?)
			ON CONFLICT (tenant_id, id) DO UPDATE SET last_seen_at = excluded.last_seen_at`),
			string(tenant), deviceID, string(user), now, now)
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: register device: %w", err)
	}
	return nil
}

// ListDevices returns the tenant's devices, newest first.
func ListDevices(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Device, error) {
	if tenant == "" {
		return nil, errors.New("identity: tenant is required")
	}
	var devices []Device
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT tenant_id, id, user_id, label, created_at, last_seen_at,
			       revoked_at, wipe_requested_at, wipe_delivered_at
			FROM devices WHERE tenant_id = ?
			ORDER BY created_at DESC, id`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			d, err := scanDevice(rows)
			if err != nil {
				return err
			}
			devices = append(devices, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("identity: list devices: %w", err)
	}
	return devices, nil
}

// GetDevice returns one device, or sql.ErrNoRows when the tenant has none by
// that id.
func GetDevice(ctx context.Context, db *storage.DB, tenant tenancy.ID, deviceID string) (Device, error) {
	if tenant == "" || deviceID == "" {
		return Device{}, errors.New("identity: tenant and device are required")
	}
	var d Device
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT tenant_id, id, user_id, label, created_at, last_seen_at,
			       revoked_at, wipe_requested_at, wipe_delivered_at
			FROM devices WHERE tenant_id = ? AND id = ?`), string(tenant), deviceID)
		var err error
		d, err = scanDevice(row)
		return err
	})
	if err != nil {
		return Device{}, err
	}
	return d, nil
}

// WipeDevice marks a device for remote wipe.
//
// **It deliberately does not revoke the device's sessions.** If it did,
// ValidateSession would fail with ErrSessionInvalid first and the client would
// see an ordinary 401 with nothing to act on — it would sign the user out and
// leave the replica exactly where it is. The device keeps a session that can do
// precisely one thing: learn that it has been wiped (decisions §3).
//
// Idempotent: wiping twice keeps the first request's timestamp, so the audit
// trail records when the decision was made rather than when it was last
// repeated.
func WipeDevice(ctx context.Context, db *storage.DB, tenant tenancy.ID, deviceID, actorID string) error {
	return stamp(ctx, db, tenant, deviceID, "wipe_requested_at", "wipe", actorID, "wipe device")
}

// RevokeDevice stops a device being issued new sessions. It does not destroy
// what the device already holds — that is WipeDevice.
func RevokeDevice(ctx context.Context, db *storage.DB, tenant tenancy.ID, deviceID, actorID string) error {
	return stamp(ctx, db, tenant, deviceID, "revoked_at", "revoke", actorID, "revoke device")
}

// MarkWipeDelivered records that a wiped device has been told.
//
// "Delivered", not "confirmed": this fires when the server refuses the device,
// which proves the instruction reached it. Nothing can prove a remote client
// deleted anything (decisions §4). Stamped once — the WHERE clause makes a
// per-request write into a one-time one, since a wiped device may keep asking.
func MarkWipeDelivered(ctx context.Context, db *storage.DB, tenant tenancy.ID, deviceID string) error {
	if tenant == "" || deviceID == "" {
		return errors.New("identity: tenant and device are required")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE devices SET wipe_delivered_at = ?
			WHERE tenant_id = ? AND id = ? AND wipe_delivered_at IS NULL`),
			time.Now().UTC(), string(tenant), deviceID)
		return err
	})
	if err != nil {
		return fmt.Errorf("identity: mark wipe delivered: %w", err)
	}
	return nil
}

// stamp sets a nullable timestamp column once, leaving an existing value alone,
// and records who did it.
//
// column is never caller-supplied — the call sites pass literals — so the
// interpolation cannot carry input (SQL commandment: parameterized only; this
// is a constant, not a parameter).
//
// The audit row is written in the **same transaction** as the change, so a wipe
// that happened is a wipe that is attributable and vice versa (INV-T4). Wiping
// a device is the most destructive action in the product that does not touch
// the ledger; an unattributable one would be worse than a capability toggle
// nobody can trace, and kernel/capability already refuses to do that.
func stamp(ctx context.Context, db *storage.DB, tenant tenancy.ID, deviceID, column, action, actorID, what string) error {
	if tenant == "" || deviceID == "" {
		return errors.New("identity: tenant and device are required")
	}
	// The actor is passed in rather than read from the context: kernel/authz
	// imports kernel/identity, so this package cannot import it back. The
	// caller has already run authz.Authorize and holds the actor (INV-T2 and
	// INV-T4 are decided together, one line apart, in internal/app/devices.go).
	if actorID == "" {
		return errors.New("identity: an actor is required to act on a device (INV-T4)")
	}
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE devices SET `+column+` = ?
			WHERE tenant_id = ? AND id = ? AND `+column+` IS NULL`),
			time.Now().UTC(), string(tenant), deviceID)
		if err != nil {
			return err
		}
		// Idempotent repeats change nothing, so they record nothing — an audit
		// trail of no-ops is noise that makes the entries that matter harder to
		// find.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil
		}
		return recordDeviceAudit(ctx, tx, db, tenant, deviceID, action, actorID)
	})
	if err != nil {
		return fmt.Errorf("identity: %s: %w", what, err)
	}
	return nil
}

// recordDeviceAudit writes one attributable audit_log row for a device action
// (INV-T4), the same shape kernel/capability and kernel/metadata use.
func recordDeviceAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, deviceID, action, actorID string) error {
	changes, err := json.Marshal(map[string]any{"device_id": deviceID})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		idgen.New(), string(tenant), "device", deviceID, action, string(changes), actorID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("identity: audit %s: %w", action, err)
	}
	return nil
}

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var tenantStr, userStr string
	var createdAt, lastSeenAt storage.Time
	var revokedAt, wipeRequestedAt, wipeDeliveredAt storage.NullTime

	if err := row.Scan(&tenantStr, &d.ID, &userStr, &d.Label,
		&createdAt, &lastSeenAt, &revokedAt, &wipeRequestedAt, &wipeDeliveredAt); err != nil {
		return Device{}, err
	}
	d.TenantID = tenancy.ID(tenantStr)
	d.UserID = UserID(userStr)
	d.CreatedAt = createdAt.Time
	d.LastSeenAt = lastSeenAt.Time
	if revokedAt.Valid {
		d.RevokedAt = &revokedAt.Time
	}
	if wipeRequestedAt.Valid {
		d.WipeRequestedAt = &wipeRequestedAt.Time
	}
	if wipeDeliveredAt.Valid {
		d.WipeDeliveredAt = &wipeDeliveredAt.Time
	}
	return d, nil
}
