//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package automations

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/outbound"
	"github.com/iamdoubz/lasterp/kernel/secrets"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-3.3c's authority half. Invariants: **INV-T3** (an automation's outbound
// authority is bounded by its creator's own, and the registry is a ceiling no
// automation escapes) and INV-T4 in the shape the grant takes.
//
// The AC's first clause — "approved against the creating administrator's own
// authority" — is *two* refusals, and they catch different mistakes. Without
// `Webhook:send`, anyone who can write an automation can make the server call
// out. Without the registry check, whoever holds `Webhook:send` chooses the
// host. Each test below is paired with the mirrored success, or a refusal that
// means "nothing works" would read as green.

const webhookDefinition = `
id: notify-ops
name: Notify ops
trigger:
  object: Invoice
actions:
  - type: webhook
    destination: ops-slack
`

func webhookTestKeys(t *testing.T) secrets.KeySource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lasterp.keys")
	if err := secrets.NewKeyFile(path, "automation-test-key"); err != nil {
		t.Fatalf("NewKeyFile: %v", err)
	}
	t.Setenv(secrets.EnvKeyFile, path)
	src, err := secrets.LoadKeySource()
	if err != nil {
		t.Fatalf("LoadKeySource: %v", err)
	}
	return src
}

// registerDestination stores the vaulted URL and the row that points at it —
// the two steps an administrator holding `Webhook:manage` takes.
func registerDestination(t *testing.T, db *storage.DB, keys secrets.KeySource, tenant tenancy.ID, id, url string) {
	t.Helper()
	ctx := context.Background()
	if err := secrets.Put(ctx, db, keys, tenant, id+"-url", "webhook url", []byte(url), "admin"); err != nil {
		t.Fatalf("Put secret: %v", err)
	}
	host := strings.TrimPrefix(url, "https://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if _, err := outbound.Register(ctx, db, keys, tenant, outbound.Destination{
		ID: id, Host: host, SecretName: id + "-url",
	}, "admin"); err != nil {
		t.Fatalf("Register destination: %v", err)
	}
}

// webhookApprover is automationApprover plus `Webhook:send` — the creator this
// WP's happy path needs.
func webhookApprover(t *testing.T, db *storage.DB, tenant tenancy.ID) authz.Actor {
	t.Helper()
	ctx := context.Background()
	actor := automationApprover(t, db, tenant)
	role, err := authz.CreateRole(ctx, db, tenant, "webhook-sender-"+string(actor.UserID), false)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := authz.GrantPermission(ctx, db, tenant, role, outbound.ObjectWebhook, outbound.ActionSend, ""); err != nil {
		t.Fatalf("GrantPermission: %v", err)
	}
	if err := authz.AssignRole(ctx, db, tenant, actor.UserID, role); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return actor
}

// INV-T3: an automation may not hold `Webhook:send` when the person creating it
// does not. Refused, never narrowed — an automation that "saved fine" and then
// fails every firing is one whose reason nobody can see.
func TestSaveRefusesAWebhookTheCreatorMayNotSend(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			keys := webhookTestKeys(t)
			registerDestination(t, db, keys, tenant, "ops-slack", "https://hooks.example.com/services/T0/B0/xyz")

			// automationApprover holds Invoice and plugin grants — everything
			// this definition needs *except* the outbound one.
			_, err := Save(ctx, db, tenant, []byte(webhookDefinition), automationApprover(t, db, tenant))
			if !errors.Is(err, ErrCapabilityNotHeld) {
				t.Fatalf("Save without Webhook:send: err = %v, want ErrCapabilityNotHeld", err)
			}
			if !strings.Contains(err.Error(), outbound.ObjectWebhook+":"+outbound.ActionSend) {
				t.Fatalf("the refusal does not name the missing tuple: %v", err)
			}

			// Mirrored: the identical definition saves for a creator who holds
			// it. Without this half the test above proves only that Save
			// refuses everything.
			if _, err := Save(ctx, db, tenant, []byte(webhookDefinition), webhookApprover(t, db, tenant)); err != nil {
				t.Fatalf("Save with Webhook:send: %v", err)
			}
		})
	}
}

// The other key. `Webhook:send` says this creator may make the server call out;
// a registered destination says *where*. Whoever writes the automation does not
// choose the host — that is `Webhook:manage`, and it is a different power
// (docs/notes/WP-3.3c-decisions.md §1).
func TestSaveRefusesAnUnregisteredDestination(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			keys := webhookTestKeys(t)
			approver := webhookApprover(t, db, tenant)

			_, err := Save(ctx, db, tenant, []byte(webhookDefinition), approver)
			if !errors.Is(err, ErrUnknownDestination) {
				t.Fatalf("Save naming an unregistered destination: err = %v, want ErrUnknownDestination", err)
			}

			// Mirrored: registering it is the only thing that changes.
			registerDestination(t, db, keys, tenant, "ops-slack", "https://hooks.example.com/services/T0/B0/xyz")
			if _, err := Save(ctx, db, tenant, []byte(webhookDefinition), approver); err != nil {
				t.Fatalf("Save after the destination was registered: %v", err)
			}
		})
	}
}

// A destination is one tenant's. INV-T1 through the new table: registering
// `ops-slack` in one tenant does not let another tenant's automation name it.
func TestADestinationDoesNotCrossTenants(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			keys := webhookTestKeys(t)
			a, b := mustCreateTenant(t, db), mustCreateTenant(t, db)
			registerDestination(t, db, keys, a, "ops-slack", "https://hooks.example.com/services/T0/B0/xyz")

			if _, err := outbound.GetDestination(ctx, db, b, "ops-slack"); !errors.Is(err, outbound.ErrNoDestination) {
				t.Fatalf("tenant B could see tenant A's destination: %v", err)
			}
			if _, err := Save(ctx, db, b, []byte(webhookDefinition), webhookApprover(t, db, b)); !errors.Is(err, ErrUnknownDestination) {
				t.Fatalf("tenant B saved an automation naming tenant A's destination: %v", err)
			}
			// Non-vacuity: the same call in tenant A works.
			if _, err := Save(ctx, db, a, []byte(webhookDefinition), webhookApprover(t, db, a)); err != nil {
				t.Fatalf("tenant A could not save its own: %v", err)
			}
		})
	}
}

// The grant an automation receives is exactly what its definition needs and
// nothing more. In particular a webhook action does not carry `Webhook:manage`
// with it: an automation may use a destination somebody approved, never
// register one.
func TestWebhookAutomationHoldsOnlySend(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			keys := webhookTestKeys(t)
			registerDestination(t, db, keys, tenant, "ops-slack", "https://hooks.example.com/services/T0/B0/xyz")
			d, err := Save(ctx, db, tenant, []byte(webhookDefinition), webhookApprover(t, db, tenant))
			if err != nil {
				t.Fatalf("Save: %v", err)
			}

			principal := authz.Actor{TenantID: tenant, UserID: identity.UserID(d.Principal())}
			can := func(object, action string) bool {
				ok, err := authz.Can(ctx, db, principal, object, action)
				if err != nil {
					t.Fatalf("Can(%s:%s): %v", object, action, err)
				}
				return ok
			}
			if !can(outbound.ObjectWebhook, outbound.ActionSend) {
				t.Fatal("the automation cannot send — it would be inert")
			}
			if can(outbound.ObjectWebhook, outbound.ActionManage) {
				t.Fatal("the automation may register destinations: it can choose where the server calls out")
			}
			// And nothing it never asked for. `Contact:update` is in the
			// approver's own set, so this is the widening a role built from the
			// creator rather than the definition would produce.
			if can("Contact", "update") {
				t.Fatal("the automation inherited a grant its definition never asked for")
			}

			// Deleting the automation takes its outbound authority with it: a
			// role left behind is authority a later automation under the same
			// id would silently inherit.
			if err := Delete(ctx, db, tenant, d.ID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if can(outbound.ObjectWebhook, outbound.ActionSend) {
				t.Fatal("the deleted automation kept Webhook:send")
			}
		})
	}
}

// A destination's URL is a credential (WP-3.2b: the path *is* the secret), so
// it lives in the vault and the row holds only the reviewable host. INV-K1
// through the registry itself: nothing this package returns carries the URL.
func TestDestinationRowNeverCarriesTheURL(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			keys := webhookTestKeys(t)
			const url = "https://hooks.example.com/services/T0/B0/hunter2-the-token"
			registerDestination(t, db, keys, tenant, "ops-slack", url)

			d, err := outbound.GetDestination(ctx, db, tenant, "ops-slack")
			if err != nil {
				t.Fatalf("GetDestination: %v", err)
			}
			// Non-vacuity: the row really is the one holding the secret's name,
			// or the sweep below is searching an empty struct.
			if d.SecretName != "ops-slack-url" || d.Host != "hooks.example.com:443" {
				t.Fatalf("destination = %+v", d)
			}
			if strings.Contains(fmtDestination(*d), "hunter2-the-token") {
				t.Fatalf("the destination row carries the credential: %+v", d)
			}
			list, err := outbound.ListDestinations(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListDestinations: %v", err)
			}
			for _, row := range list {
				if strings.Contains(fmtDestination(row), "hunter2-the-token") {
					t.Fatalf("the destination list carries the credential: %+v", row)
				}
			}
		})
	}
}

// A destination whose vaulted URL points at a host other than the registered
// one is refused. The name an administrator reviewed is what binds; a secret
// rotated elsewhere is a redirect nobody approved.
func TestDestinationRefusesAURLThatLeftItsHost(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			keys := webhookTestKeys(t)
			if err := secrets.Put(ctx, db, keys, tenant, "elsewhere-url", "", []byte("https://evil.example.net/x"), "admin"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			_, err := outbound.Register(ctx, db, keys, tenant, outbound.Destination{
				ID: "elsewhere", Host: "hooks.example.com", SecretName: "elsewhere-url",
			}, "admin")
			if err == nil {
				t.Fatal("a destination whose url points at another host was registered")
			}
			if strings.Contains(err.Error(), "evil.example.net") {
				t.Fatalf("the refusal quoted the url, which is a credential: %v", err)
			}
			// Mirrored: the same registration with a matching url succeeds.
			if err := secrets.Put(ctx, db, keys, tenant, "matching-url", "", []byte("https://hooks.example.com/x"), "admin"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if _, err := outbound.Register(ctx, db, keys, tenant, outbound.Destination{
				ID: "matching", Host: "hooks.example.com", SecretName: "matching-url",
			}, "admin"); err != nil {
				t.Fatalf("a matching destination was refused: %v", err)
			}
			// And a destination whose secret does not exist at all is refused
			// where an administrator sees it, not at 2am inside a job retry.
			if _, err := outbound.Register(ctx, db, keys, tenant, outbound.Destination{
				ID: "missing", Host: "hooks.example.com", SecretName: "nothing-stored",
			}, "admin"); err == nil {
				t.Fatal("a destination pointing at a secret nobody stored was registered")
			}
		})
	}
}

// fmtDestination renders every field of a destination for a credential sweep.
// %+v rather than a field list on purpose: a URL column added to the struct
// later is caught rather than silently skipped.
func fmtDestination(d outbound.Destination) string { return fmt.Sprintf("%+v", d) }
