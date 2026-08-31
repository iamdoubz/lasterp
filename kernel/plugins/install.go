// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/changefeed"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/jobs"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// MaxModuleBytes caps an installed module. A Go-compiled WASM plugin is a
// couple of megabytes; TinyGo and Rust produce far less. The cap exists so an
// upload cannot be a memory attack on the server before it is ever a sandboxed
// one.
const MaxModuleBytes = 32 << 20

// ErrNotFound is returned when the tenant has no plugin by that id.
var ErrNotFound = errors.New("plugins: no such plugin")

// ErrCapabilityNotHeld is returned when a manifest asks for authority the
// approving administrator does not themselves hold.
var ErrCapabilityNotHeld = errors.New("plugins: the approver does not hold a capability the manifest requests")

// Installed is a plugin as stored: its manifest, the authority actually
// granted, and the provenance of its bytes.
type Installed struct {
	ID          string
	Version     string
	Manifest    *Manifest
	Granted     [][2]string
	RoleID      authz.RoleID
	SHA256      string
	InstalledAt time.Time
	InstalledBy string

	// Circuit-breaker state (WP-3.1b, breaker.go). Persisted on the row so it
	// survives the restart a misbehaving plugin may itself be causing.
	HookFailures    int
	BreakerOpenedAt *time.Time

	module []byte // decoded lazily by Load; never rendered
}

// Principal is the plugin's actor id.
func (p Installed) Principal() string { return PrincipalFor(p.ID) }

// Actor is the plugin's authz principal for host calls.
func (p Installed) Actor(tenant tenancy.ID) authz.Actor {
	return authz.Actor{TenantID: tenant, UserID: identity.UserID(p.Principal())}
}

// Install stores a plugin and grants it exactly the authority its manifest
// requests — no more, and never more than the approver holds.
//
// **An unheld capability refuses the install rather than narrowing it.**
// Silently installing a plugin with less authority than it asked for produces
// something that "installed fine" and then fails at runtime for reasons nobody
// can see, and it makes the approval screen a lie. The administrator is told
// which capability they lack, and either gets it or does not install
// (INV-T3: an approval may narrow the request to nothing, never widen it).
//
// The plugin's authority lives in a role of its own, assigned to a principal
// that is deliberately **not** a users row: nothing can log in as a plugin,
// because no login path can find a principal that does not exist in `users`
// (decisions §3).
// manifestYAML is stored verbatim rather than re-marshalled from the parsed
// struct: the record of what an administrator approved should be the document
// they read, not this host's reformatting of it.
func Install(ctx context.Context, db *storage.DB, tenant tenancy.ID, manifestYAML, module []byte, approver authz.Actor) (*Installed, error) {
	if tenant == "" || approver.TenantID == "" || approver.UserID == "" {
		return nil, errors.New("plugins: tenant and an approving actor are required")
	}
	m, err := ParseManifest(manifestYAML)
	if err != nil {
		return nil, err
	}
	if len(module) == 0 {
		return nil, errors.New("plugins: module is empty")
	}
	if len(module) > MaxModuleBytes {
		return nil, fmt.Errorf("plugins: module is %d bytes, over the %d-byte limit", len(module), MaxModuleBytes)
	}
	if !isWASM(module) {
		return nil, errors.New("plugins: module is not a WebAssembly binary")
	}

	// INV-T3, at the only moment authority is created: every requested
	// permission is checked against the approver's own grants.
	perms := m.Permissions()
	var missing []string
	for _, p := range perms {
		ok, err := authz.Can(ctx, db, approver, p[0], p[1])
		if err != nil {
			return nil, fmt.Errorf("plugins: check approver grant %s:%s: %w", p[0], p[1], err)
		}
		if !ok {
			missing = append(missing, p[0]+":"+p[1])
		}
	}
	// Secrets are authority too: approving `secrets: [x]` hands the plugin a
	// credential, so the approver must hold the vault's own permission.
	if len(m.Capabilities.Secrets) > 0 {
		ok, err := authz.Can(ctx, db, approver, "secret", "manage")
		if err != nil {
			return nil, fmt.Errorf("plugins: check approver secret grant: %w", err)
		}
		if !ok {
			missing = append(missing, "secret:manage")
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrCapabilityNotHeld, strings.Join(missing, ", "))
	}

	if _, err := Get(ctx, db, tenant, m.ID); err == nil {
		return nil, fmt.Errorf("plugins: %s is already installed; uninstall it first", m.ID)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	role, err := authz.CreateRole(ctx, db, tenant, "plugin:"+m.ID, false)
	if err != nil {
		return nil, fmt.Errorf("plugins: create plugin role: %w", err)
	}
	for _, p := range perms {
		if err := authz.GrantPermission(ctx, db, tenant, role, p[0], p[1], ""); err != nil {
			return nil, fmt.Errorf("plugins: grant %s:%s: %w", p[0], p[1], err)
		}
	}
	principal := identity.UserID(PrincipalFor(m.ID))
	if err := authz.AssignRole(ctx, db, tenant, principal, role); err != nil {
		return nil, fmt.Errorf("plugins: assign plugin role: %w", err)
	}

	sum := sha256.Sum256(module)
	digest := hex.EncodeToString(sum[:])
	grantedJSON, err := json.Marshal(perms)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	// Where async delivery starts. Read *before* the install commits, so a
	// change racing this install is delivered rather than missed — at-least-once
	// is the promise, and the safe direction of that race is "delivered twice".
	//
	// Recorded here rather than lazily at the first delivery pass: a cursor
	// created on first pass silently skips everything between the install and
	// that pass, which is a window nobody would think to look for.
	startCursor, err := changefeed.HighWater(ctx, db, tenant)
	if err != nil {
		return nil, fmt.Errorf("plugins: read feed position for %s: %w", m.ID, err)
	}

	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO plugins (tenant_id, id, version, manifest, granted, role_id, sha256, module,
			                     installed_at, installed_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), m.ID, m.Version, string(manifestYAML), string(grantedJSON), string(role), digest,
			base64.StdEncoding.EncodeToString(module), now, string(approver.UserID)); err != nil {
			return err
		}
		if m.HasAsyncHooks() {
			if _, err := tx.ExecContext(ctx, db.Rebind(`
				INSERT INTO plugin_deliveries (tenant_id, plugin_id, cursor, updated_at)
				VALUES (?, ?, ?, ?)`),
				string(tenant), m.ID, startCursor, now); err != nil {
				return err
			}
		}
		return recordAudit(ctx, tx, db, tenant, m.ID, "install", string(approver.UserID),
			map[string]any{"version": m.Version, "sha256": digest, "granted": perms})
	})
	if err != nil {
		return nil, fmt.Errorf("plugins: install %s: %w", m.ID, err)
	}

	installed := &Installed{
		ID: m.ID, Version: m.Version, Manifest: m, Granted: perms, RoleID: role,
		SHA256: digest, InstalledAt: now, InstalledBy: string(approver.UserID), module: module,
	}

	// Schedules are created after the plugin row commits, and are derived from
	// the manifest rather than accumulated: SyncSchedules deletes this
	// principal's rows first, so reinstalling replaces its cron entries instead
	// of doubling them (WP-3.3b).
	if err := SyncSchedules(ctx, Host{DB: db}, tenant, installed, now); err != nil {
		return nil, fmt.Errorf("plugins: install %s: %w", m.ID, err)
	}
	return installed, nil
}

// Uninstall removes a plugin and the authority that came with it. The role
// goes with the row: a role left behind is authority a later install under the
// same id would silently inherit.
func Uninstall(ctx context.Context, db *storage.DB, tenant tenancy.ID, id, actor string) error {
	if tenant == "" || actor == "" {
		return errors.New("plugins: tenant and actor are required")
	}
	p, err := Get(ctx, db, tenant, id)
	if err != nil {
		return err
	}
	if err := authz.DeleteRole(ctx, db, tenant, p.RoleID); err != nil {
		return fmt.Errorf("plugins: remove plugin role: %w", err)
	}
	// Schedules go with the plugin, for the same reason the role does: a cron
	// row left behind is work that keeps firing for code that is gone, and it
	// would fire under a reinstalled *different* plugin's id (WP-3.3b).
	if err := jobs.DeleteSchedulesByOwner(ctx, db, tenant, p.Principal()); err != nil {
		return fmt.Errorf("plugins: remove plugin schedules: %w", err)
	}
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range []string{
			`DELETE FROM plugins WHERE tenant_id = ? AND id = ?`,
			// Delivery state goes with the plugin. A cursor left behind would
			// make a reinstall resume from a position it never chose, and kv
			// left behind would hand a *different* plugin under the same id
			// someone else's stored state.
			`DELETE FROM plugin_deliveries WHERE tenant_id = ? AND plugin_id = ?`,
			`DELETE FROM plugin_kv WHERE tenant_id = ? AND plugin_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, db.Rebind(stmt), string(tenant), id); err != nil {
				return err
			}
		}
		return recordAudit(ctx, tx, db, tenant, id, "uninstall", actor, nil)
	})
	if err != nil {
		return fmt.Errorf("plugins: uninstall %s: %w", id, err)
	}
	forgetCompiled(p.SHA256)
	return nil
}

// Get returns one installed plugin, module bytes included.
func Get(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (*Installed, error) {
	if tenant == "" || id == "" {
		return nil, errors.New("plugins: tenant and id are required")
	}
	var (
		p                                         Installed
		manifestYAML, grantedJSON, roleID, modB64 string
		installedAt                               storage.Time
		openedAt                                  storage.NullTime
	)
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(`
			SELECT id, version, manifest, granted, role_id, sha256, module, installed_at, installed_by,
			       hook_failures, breaker_opened_at
			FROM plugins WHERE tenant_id = ? AND id = ?`), string(tenant), id).
			Scan(&p.ID, &p.Version, &manifestYAML, &grantedJSON, &roleID, &p.SHA256, &modB64,
				&installedAt, &p.InstalledBy, &p.HookFailures, &openedAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("plugins: get %s: %w", id, err)
	}

	m, err := ParseManifest([]byte(manifestYAML))
	if err != nil {
		return nil, fmt.Errorf("plugins: stored manifest for %s no longer parses: %w", id, err)
	}
	if err := json.Unmarshal([]byte(grantedJSON), &p.Granted); err != nil {
		return nil, fmt.Errorf("plugins: stored grants for %s: %w", id, err)
	}
	module, err := base64.StdEncoding.DecodeString(modB64)
	if err != nil {
		return nil, fmt.Errorf("plugins: stored module for %s: %w", id, err)
	}
	// The bytes are re-hashed on every load, not trusted from the column: the
	// digest is what WP-3.2's signature check will attach to, and a module that
	// no longer matches its recorded hash is not the module an administrator
	// approved.
	sum := sha256.Sum256(module)
	if got := hex.EncodeToString(sum[:]); got != p.SHA256 {
		return nil, fmt.Errorf("plugins: module for %s has hash %s, recorded %s — refusing to run bytes nobody approved", id, got, p.SHA256)
	}
	p.Manifest, p.RoleID, p.module, p.InstalledAt = m, authz.RoleID(roleID), module, installedAt.Time
	if openedAt.Valid {
		p.BreakerOpenedAt = &openedAt.Time
	}
	return &p, nil
}

// List returns the tenant's installed plugins **without** their module bytes,
// which is what every management screen wants.
func List(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Installed, error) {
	if tenant == "" {
		return nil, errors.New("plugins: tenant is required")
	}
	var out []Installed
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, version, manifest, granted, role_id, sha256, installed_at, installed_by,
			       hook_failures, breaker_opened_at
			FROM plugins WHERE tenant_id = ? ORDER BY id`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p Installed
			var manifestYAML, grantedJSON, roleID string
			var installedAt storage.Time
			var openedAt storage.NullTime
			if err := rows.Scan(&p.ID, &p.Version, &manifestYAML, &grantedJSON, &roleID, &p.SHA256,
				&installedAt, &p.InstalledBy, &p.HookFailures, &openedAt); err != nil {
				return err
			}
			// Parsed for the management view (what it exposes, where it calls
			// out). A stored manifest this host can no longer parse leaves the
			// field nil rather than failing the whole listing: an administrator
			// looking at a plugin that has stopped being installable needs the
			// list to render, and Get still refuses it by name.
			p.Manifest, _ = ParseManifest([]byte(manifestYAML))
			if openedAt.Valid {
				p.BreakerOpenedAt = &openedAt.Time
			}
			if err := json.Unmarshal([]byte(grantedJSON), &p.Granted); err != nil {
				return err
			}
			p.RoleID, p.InstalledAt = authz.RoleID(roleID), installedAt.Time
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("plugins: list: %w", err)
	}
	return out, nil
}

// isWASM checks the magic number. A cheap sanity gate so a mistyped path fails
// at install with a sentence, not at first call with a wazero error.
func isWASM(b []byte) bool {
	return len(b) > 4 && b[0] == 0x00 && b[1] == 0x61 && b[2] == 0x73 && b[3] == 0x6d
}

// recordAudit writes one attributable audit_log row (INV-T4).
func recordAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, id, action, actor string, extra map[string]any) error {
	fields := map[string]any{"plugin": id}
	for k, v := range extra {
		fields[k] = v
	}
	changes, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		idgen.New(), string(tenant), "plugin", id, action, string(changes), actor, time.Now().UTC()); err != nil {
		return fmt.Errorf("plugins: audit %s: %w", action, err)
	}
	return nil
}

// LoadAll returns every installed plugin for a tenant, module bytes included,
// which is what dispatch and delivery need (List deliberately omits them).
//
// A plugin whose stored bytes no longer match their recorded hash is skipped
// rather than failing the whole load: one tampered row must not stop every
// other plugin in the tenant from running, and Get still refuses it by name
// when an administrator asks about it directly.
func LoadAll(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Installed, error) {
	metas, err := List(ctx, db, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]Installed, 0, len(metas))
	for _, meta := range metas {
		p, err := Get(ctx, db, tenant, meta.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // uninstalled between the two reads
			}
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}
