// SPDX-License-Identifier: AGPL-3.0-only

package outbound

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// A destination is the generalised allowlist entry (WP-3.3c). A plugin's
// allowlist is its manifest, approved once at install; an automation has no
// manifest, so what an administrator approves is a row.
//
// It carries the *reviewable* half of the destination — a host — and a pointer
// to the vaulted URL. WP-3.2b found the reason: a Slack incoming webhook is
// `/services/T000/B000/<secret>`, so the path is itself a credential, and
// INV-K1 forbids persisting that in the clear. It cannot live in the
// automation's YAML either — `GET /api/v1/automations/{id}` hands that document
// to anyone holding `Automation:manage`.

// The authz vocabulary for the outbound surface. Two powers, deliberately
// separate (docs/notes/WP-3.3c-decisions.md §1): `manage` decides *where* this
// deployment may call out, `send` is permission to make it. Folded together,
// anyone who can write an automation picks the host.
const (
	ObjectWebhook = "Webhook"
	ActionManage  = "manage"
	ActionSend    = "send"
)

// ErrNoDestination is returned when a tenant has no destination by that id.
var ErrNoDestination = errors.New("outbound: no such destination")

// destIDRE bounds a destination id: it is a path segment, an audit field and
// the name an automation refers to.
var destIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// hostPortRE bounds a destination host: a DNS name, optionally with a port. No
// scheme (https is not a choice), no path (a path-scoped allowlist implies a
// containment it cannot deliver — the same host serves whatever paths it
// likes), no wildcard.
var hostPortRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.\-]{0,253}[a-z0-9])?(:[0-9]{1,5})?$`)

// Destination is one approved outbound target.
type Destination struct {
	ID string
	// Host is normalised to host:port, so it compares like with like against
	// the address a request actually resolves to.
	Host        string
	SecretName  string
	Description string
	CreatedAt   time.Time
	CreatedBy   string
}

// NormalizeHost lowercases a host and makes its port explicit.
func NormalizeHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if !hostPortRE.MatchString(host) {
		return "", fmt.Errorf("%q must be a DNS name, optionally with a port (no scheme, no path, no wildcard)", host)
	}
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	return host, nil
}

// Register creates or replaces a destination. The caller has already
// authorized: `Webhook:manage` is the power to decide where this deployment may
// call out, and it is checked at the edge (internal/app/webhooks.go).
//
// The secret is required to exist. A destination pointing at a secret nobody
// stored is one that fails on its first firing, at 2am, in a job retry — the
// same refuse-never-defer rule the plugin manifest follows.
func Register(ctx context.Context, db *storage.DB, keys secrets.KeySource, tenant tenancy.ID, d Destination, actor string) (*Destination, error) {
	if tenant == "" || actor == "" {
		return nil, errors.New("outbound: tenant and an actor are required")
	}
	if !destIDRE.MatchString(d.ID) {
		return nil, fmt.Errorf("outbound: %q is not a valid destination id", d.ID)
	}
	host, err := NormalizeHost(d.Host)
	if err != nil {
		return nil, fmt.Errorf("outbound: %w", err)
	}
	if !secrets.ValidName(d.SecretName) {
		return nil, fmt.Errorf("outbound: %q is not a valid secret name", d.SecretName)
	}
	if len(d.Description) > 1000 {
		return nil, errors.New("outbound: description is too long")
	}
	// Checked here, where an administrator sees the answer. The read also
	// proves the URL parses and points at the host being registered — a
	// destination whose secret says something else is a redirect nobody
	// approved.
	if _, err := resolveURL(ctx, db, keys, tenant, Destination{ID: d.ID, Host: host, SecretName: d.SecretName},
		secrets.Reader{Kind: "module", ID: "outbound"}); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, db.Rebind(`
			UPDATE outbound_destinations SET host = ?, secret_name = ?, description = ?
			WHERE tenant_id = ? AND id = ?`),
			host, d.SecretName, d.Description, string(tenant), d.ID)
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
			INSERT INTO outbound_destinations (tenant_id, id, host, secret_name, description, created_at, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			string(tenant), d.ID, host, d.SecretName, d.Description, now, actor)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("outbound: register destination %s: %w", d.ID, err)
	}
	out := d
	out.Host, out.CreatedAt, out.CreatedBy = host, now, actor
	return &out, nil
}

// GetDestination loads one destination. It never returns the URL: the row does
// not hold it (INV-K1).
func GetDestination(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (*Destination, error) {
	var out *Destination
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		out = nil // reset per attempt: WithTenant retries this callback
		var d Destination
		var createdAt storage.Time
		err := tx.QueryRowContext(ctx, db.Rebind(`
			SELECT id, host, secret_name, description, created_at, created_by
			FROM outbound_destinations WHERE tenant_id = ? AND id = ?`), string(tenant), id).
			Scan(&d.ID, &d.Host, &d.SecretName, &d.Description, &createdAt, &d.CreatedBy)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoDestination
		}
		if err != nil {
			return err
		}
		d.CreatedAt = createdAt.Time
		out = &d
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNoDestination) {
			return nil, err
		}
		return nil, fmt.Errorf("outbound: get destination %s: %w", id, err)
	}
	return out, nil
}

// ListDestinations returns a tenant's destinations, by id.
func ListDestinations(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Destination, error) {
	var out []Destination
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		// Built locally and assigned once: WithTenant retries the whole
		// callback on SQLITE_BUSY, and a slice captured from the enclosing
		// scope keeps what a half-finished attempt put in it (WP-3.3d).
		var list []Destination
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT id, host, secret_name, description, created_at, created_by
			FROM outbound_destinations WHERE tenant_id = ? ORDER BY id`), string(tenant))
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var d Destination
			var createdAt storage.Time
			if err := rows.Scan(&d.ID, &d.Host, &d.SecretName, &d.Description, &createdAt, &d.CreatedBy); err != nil {
				return err
			}
			d.CreatedAt = createdAt.Time
			list = append(list, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = list
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("outbound: list destinations: %w", err)
	}
	return out, nil
}

// DeleteDestination removes a destination.
//
// An automation still naming it keeps failing visibly in `automation_runs`
// rather than silently doing nothing, which is the outcome an administrator who
// revoked a destination wants to see.
func DeleteDestination(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) error {
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(
			`DELETE FROM outbound_destinations WHERE tenant_id = ? AND id = ?`), string(tenant), id)
		return err
	})
	if err != nil {
		return fmt.Errorf("outbound: delete destination %s: %w", id, err)
	}
	return nil
}

// Send POSTs a body to a registered destination as the given caller.
//
// ponytail: POST only. A webhook is a POST; per-destination method sets are a
// dial nobody has asked for, and the upgrade is a `methods` column mirroring
// plugins.HTTPHost.Methods.
//
// The reader is the caller, so the vault read is attributable too (WP-3.0), and
// the grants function permits exactly one name: a destination is not a way to
// read the vault.
func Send(ctx context.Context, db *storage.DB, keys secrets.KeySource, tenant tenancy.ID, p Policy, c Caller, destination string, body []byte) (*Response, error) {
	d, err := GetDestination(ctx, db, tenant, destination)
	if err != nil {
		return nil, err
	}
	reader := secrets.Reader{Kind: c.Object, ID: c.ID}
	u, err := resolveURL(ctx, db, keys, tenant, *d, reader)
	if err != nil {
		return nil, err
	}
	return Do(ctx, db, tenant, p, c,
		// The registered host is the ceiling, and it is the only thing this
		// caller may reach: the destination is named in the definition, the
		// host is named in the row an administrator approved.
		func(method, hostPort string) bool {
			return strings.EqualFold(method, "POST") && hostPort == d.Host
		},
		Request{
			Method:  "POST",
			URL:     u,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    string(body),
		})
}

// resolveURL opens the destination's vaulted URL and checks it is still the
// host that was registered.
//
// The host check is what makes the reviewable half load-bearing: the name an
// administrator approved is what binds, so a secret rotated to point somewhere
// else does not silently redirect the traffic to a destination nobody saw. The
// error deliberately does not quote the URL — it is a credential.
func resolveURL(ctx context.Context, db *storage.DB, keys secrets.KeySource, tenant tenancy.ID, d Destination, reader secrets.Reader) (string, error) {
	if keys == nil {
		return "", errors.New("outbound: no secrets key source is configured for this deployment")
	}
	value, err := secrets.Get(ctx, db, keys, tenant, d.SecretName, reader,
		// Exactly this destination's secret and nothing else — the automation's
		// analogue of a plugin manifest's `secrets:` list.
		func(_ secrets.Reader, name string) bool { return name == d.SecretName })
	if err != nil {
		return "", fmt.Errorf("outbound: destination %s: read url: %w", d.ID, err)
	}
	raw := string(value)
	_, hostPort, err := Target(raw)
	if err != nil {
		return "", fmt.Errorf("outbound: destination %s: the stored url is not usable: %w", d.ID, err)
	}
	if hostPort != d.Host {
		return "", fmt.Errorf("%w: destination %s is registered for %s and its url points elsewhere",
			ErrBlocked, d.ID, d.Host)
	}
	return raw, nil
}
