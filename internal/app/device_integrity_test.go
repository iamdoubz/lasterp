//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-2.5: device security — registration, per-device revocation, remote wipe.
//
// The encryption half of the roadmap AC ("replica unreadable without keystore")
// is NOT here and is not silently dropped: ADR-021 records that at-rest
// encryption is a native-shell control and moves that criterion to WP-4.8. What
// these tests carry is the half that holds on the shell that exists.
//
// Invariants: **INV-D1** (a wiped device is refused on every authenticated
// path), INV-T1 (devices are tenant-scoped), INV-T2 (management is authorized),
// INV-T4 (a wipe is attributable).

// deviceEnv is a seeded tenant plus a second session on a known device id, so a
// test can wipe *that* device without disturbing the admin session it uses to
// do the wiping.
type deviceEnv struct {
	*env
	deviceID string
	token    string
	userID   identity.UserID
}

func seedDevice(t *testing.T, db *storage.DB) *deviceEnv {
	t.Helper()
	e := seed(t, db)
	deviceID := "device-" + idgen.New()
	user, token := userOnDevice(t, e, deviceID, fullGrants())
	return &deviceEnv{env: e, deviceID: deviceID, token: token, userID: user}
}

// userOnDevice creates a user and issues it a session bound to deviceID, which
// is what registers the device (registration is implicit — decisions §1).
func userOnDevice(t *testing.T, e *env, deviceID string, grants map[string][]string) (identity.UserID, string) {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := identity.CreateUser(ctx, e.db, e.tenant, idgen.New()+"@example.com", hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	grantRole(t, e.db, e.tenant, user.ID, grants)
	issued, err := identity.IssueSession(ctx, e.db, e.tenant, user.ID, deviceID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	return user.ID, issued.Token
}

func wipe(t *testing.T, e *env, deviceID string) {
	t.Helper()
	if err := identity.WipeDevice(context.Background(), e.db, e.tenant, deviceID, "admin-under-test"); err != nil {
		t.Fatalf("WipeDevice: %v", err)
	}
}

// --- INV-D1 ---

// TestWipedDeviceIsRefusedOnEveryAuthenticatedPath is INV-D1, and it is a
// property of the whole surface rather than of the handlers somebody remembered
// to check — same shape as WP-2.3b's TestNoSyncWriteEndpointExists.
//
// This is why the wipe check lives in the authenticator (decisions §2). A
// polling endpoint or a field on the sync response can be skipped by the
// client, and a control the subject can decline to receive is not a control.
// Here there is no request the device can make that both succeeds and misses
// the check — so the test enumerates the live routes and requires all of them
// to refuse.
func TestWipedDeviceIsRefusedOnEveryAuthenticatedPath(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)

			// Every authenticated GET route the action table declares. GETs
			// only: a write would need a valid body per route, and the property
			// under test is authentication, which runs before any of that.
			//
			// Public actions are excluded, and the exclusion is the invariant's
			// own wording rather than a convenience: INV-D1 is about every
			// *authenticated* path. A login route has to work without a session
			// by construction (kernel/api.Action.Public), so a wiped device
			// reaching one proves nothing — it has not authenticated. The
			// device's own credential is what this refuses, and a public route
			// never consults it.
			var probes []string
			for _, a := range allActions(t, db) {
				if a.Public || a.Method != http.MethodGet || strings.Contains(a.Path, "{") {
					continue
				}
				probes = append(probes, a.Path)
			}
			// Generic CRUD routes are registered straight onto the mux rather
			// than as Actions (gateway.go registerObject), so the scan above
			// cannot see them — the same blind spot WP-2.3b's test records. They
			// are the routes carrying the most tenant data, so they are named
			// explicitly rather than left out.
			probes = append(probes, "/api/v1/contact", "/api/v1/account")

			if len(probes) < 7 {
				t.Fatalf("only %d probe routes found — the enumeration broke and this "+
					"test would pass vacuously", len(probes))
			}

			// Before the wipe: the device works. Without this the assertions
			// below could all be failing for an unrelated reason.
			working := 0
			for _, path := range probes {
				if status, _, _ := e.call("GET", path, e.token, "", nil); status == http.StatusOK {
					working++
				}
			}
			if working == 0 {
				t.Fatal("the device could not reach any route before the wipe; " +
					"a refusal afterwards would prove nothing")
			}

			wipe(t, e.env, e.deviceID)

			for _, path := range probes {
				status, body, parsed := e.call("GET", path, e.token, "", nil)
				if status != http.StatusUnauthorized {
					t.Errorf("GET %s answered %d for a WIPED device, want 401 — INV-D1 says "+
						"there is no authenticated path a wiped device may use; body=%s",
						path, status, body)
					continue
				}
				// And it must say *why*, or the client signs the user out and
				// keeps the replica (decisions §3).
				if got, _ := parsed["type"].(string); got != "device-wiped" {
					t.Errorf("GET %s refused with type %q, want \"device-wiped\" — an "+
						"undifferentiated 401 leaves the replica on disk", path, got)
				}
			}
		})
	}
}

// TestWipedDeviceIsRefusedOnWritesToo covers the other verb class: authn runs
// before body parsing, so a write must be refused identically.
func TestWipedDeviceIsRefusedOnWritesToo(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			wipe(t, e.env, e.deviceID)

			status, body, parsed := e.call("POST", "/api/v1/contact", e.token, idgen.New(),
				map[string]any{"name": "After The Wipe", "kind": "customer"})
			if status != http.StatusUnauthorized {
				t.Fatalf("a wiped device created a contact: %d; body=%s", status, body)
			}
			if got, _ := parsed["type"].(string); got != "device-wiped" {
				t.Errorf("write refused with type %q, want \"device-wiped\"", got)
			}
		})
	}
}

// TestWipeDoesNotRevokeTheSessionBeforeDelivery is decisions §3 as a test.
//
// A wipe that also revoked the session would make ValidateSession fail with the
// deliberately-opaque ErrSessionInvalid *first*, and the device would see an
// ordinary 401 with nothing to act on — sign the user out, keep the replica.
// The session must survive precisely so it can carry one message.
func TestWipeDoesNotRevokeTheSessionBeforeDelivery(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			wipe(t, e.env, e.deviceID)

			_, _, parsed := e.call("GET", "/api/v1/meta/objects", e.token, "", nil)
			if got, _ := parsed["type"].(string); got != "device-wiped" {
				t.Fatalf("refusal type = %q, want \"device-wiped\" — if the wipe revoked the "+
					"session, this is a bare 401 and the device never learns to erase itself",
					got)
			}
		})
	}
}

// TestWipeDeliveryIsRecordedOnce: "delivered", not "confirmed" (decisions §4).
func TestWipeDeliveryIsRecordedOnce(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)

			if d := deviceRow(t, e.env, e.deviceID); d.WipeDeliveredAt != nil {
				t.Fatal("delivery recorded before any wipe")
			}
			wipe(t, e.env, e.deviceID)
			if d := deviceRow(t, e.env, e.deviceID); d.WipeDeliveredAt != nil {
				t.Fatal("delivery recorded before the device was told")
			}

			e.call("GET", "/api/v1/meta/objects", e.token, "", nil)
			first := deviceRow(t, e.env, e.deviceID)
			if first.WipeDeliveredAt == nil {
				t.Fatal("the device was refused but delivery was not recorded")
			}

			// A wiped device may keep asking; the stamp is the first time.
			e.call("GET", "/api/v1/meta/objects", e.token, "", nil)
			second := deviceRow(t, e.env, e.deviceID)
			if !second.WipeDeliveredAt.Equal(*first.WipeDeliveredAt) {
				t.Errorf("delivery timestamp moved on a second refusal: %v then %v",
					first.WipeDeliveredAt, second.WipeDeliveredAt)
			}
		})
	}
}

// TestWipedDeviceCannotGetAFreshSession: logging in again must not launder a
// device an administrator has acted on.
func TestWipedDeviceCannotGetAFreshSession(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			wipe(t, e.env, e.deviceID)

			_, err := identity.IssueSession(context.Background(), e.db, e.tenant, e.userID, e.deviceID)
			if err == nil {
				t.Fatal("a wiped device was issued a fresh session — the wipe is undone by " +
					"the most ordinary action a user takes")
			}
		})
	}
}

// TestRevokedDeviceCannotGetAFreshSession is the weaker sibling: revocation
// stops new sessions without destroying what the device holds.
func TestRevokedDeviceCannotGetAFreshSession(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			if err := identity.RevokeDevice(context.Background(), e.db, e.tenant, e.deviceID, "admin-under-test"); err != nil {
				t.Fatalf("RevokeDevice: %v", err)
			}
			if _, err := identity.IssueSession(context.Background(), e.db, e.tenant, e.userID, e.deviceID); err == nil {
				t.Fatal("a revoked device was issued a fresh session")
			}
		})
	}
}

// TestWipedDeviceCannotRefreshItsToken closes the gap INV-D1's own wording
// leaves open: refresh is a **public** route, so "every authenticated path"
// does not reach it.
//
// Without a check there, a wiped device mints a fresh access token whenever the
// old one expires — each useless for data, but each extending a session that
// should be over, forever. Found by re-reading the invariant against the route
// table rather than by a failing test, which is the honest provenance.
func TestWipedDeviceCannotRefreshItsToken(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			ctx := context.Background()

			issued, err := identity.IssueSession(ctx, e.db, e.tenant, e.userID, e.deviceID)
			if err != nil {
				t.Fatalf("IssueSession: %v", err)
			}
			// Refresh works before the wipe, or the assertion after it proves
			// nothing about the wipe.
			if _, err := identity.RefreshSession(ctx, e.db, issued.RefreshToken, e.deviceID); err != nil {
				t.Fatalf("refresh before the wipe: %v", err)
			}

			wipe(t, e.env, e.deviceID)

			_, err = identity.RefreshSession(ctx, e.db, issued.RefreshToken, e.deviceID)
			if err == nil {
				t.Fatal("a wiped device refreshed its token — it can keep a live credential " +
					"indefinitely and the session never expires out")
			}
			if !errors.Is(err, identity.ErrDeviceWiped) {
				t.Errorf("refresh refused with %v, want ErrDeviceWiped — an expiring client "+
					"reaches refresh first, so this is often where a wipe is delivered", err)
			}
		})
	}
}

// TestRevokedDeviceCannotRefreshItsToken is the same hole for the weaker
// control: revoking a device must not be undone by a routine token refresh.
func TestRevokedDeviceCannotRefreshItsToken(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			ctx := context.Background()

			issued, err := identity.IssueSession(ctx, e.db, e.tenant, e.userID, e.deviceID)
			if err != nil {
				t.Fatalf("IssueSession: %v", err)
			}
			if err := identity.RevokeDevice(ctx, e.db, e.tenant, e.deviceID, "admin-under-test"); err != nil {
				t.Fatalf("RevokeDevice: %v", err)
			}
			if _, err := identity.RefreshSession(ctx, e.db, issued.RefreshToken, e.deviceID); err == nil {
				t.Fatal("a revoked device refreshed its token")
			}
		})
	}
}

// --- registration ---

func TestSessionIssueRegistersTheDevice(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			deviceID := "device-" + idgen.New()

			before, err := identity.ListDevices(context.Background(), e.db, e.tenant)
			if err != nil {
				t.Fatalf("ListDevices: %v", err)
			}

			userOnDevice(t, e, deviceID, fullGrants())

			after, err := identity.ListDevices(context.Background(), e.db, e.tenant)
			if err != nil {
				t.Fatalf("ListDevices: %v", err)
			}
			if len(after) != len(before)+1 {
				t.Fatalf("device count %d -> %d, want exactly one new registration",
					len(before), len(after))
			}
			found := false
			for _, d := range after {
				if d.ID == deviceID {
					found = true
					if d.LastSeenAt.IsZero() {
						t.Error("registered device has no last_seen_at")
					}
				}
			}
			if !found {
				t.Errorf("device %s was not registered by issuing its session", deviceID)
			}
		})
	}
}

// --- the management surface ---

func TestDeviceListIsTenantScoped(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			mine := seedDevice(t, db)
			theirs := seedDevice(t, db)

			status, body, parsed := mine.call("GET", "/api/v1/devices", mine.env.token, "", nil)
			if status != http.StatusOK {
				t.Fatalf("GET devices = %d; body=%s", status, body)
			}
			for _, raw := range asSlice(parsed["data"]) {
				d, _ := raw.(map[string]any)
				if id, _ := d["id"].(string); id == theirs.deviceID {
					t.Fatalf("another tenant's device %s is visible — INV-T1 defeated on the "+
						"one table that says which machines hold this tenant's data", id)
				}
			}
		})
	}
}

func TestDeviceManagementRequiresAuthorization(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			// A principal with a full sync grant and no device grant: being
			// able to replicate is not being able to wipe someone's laptop.
			outsider := e.issueUser(t, map[string][]string{"sync": {"read"}})

			if status, _, _ := e.call("GET", "/api/v1/devices", outsider, "", nil); status != http.StatusForbidden {
				t.Errorf("device list without device:read = %d, want 403", status)
			}
			status, body, _ := e.call("POST", "/api/v1/devices/"+e.deviceID+"/wipe",
				outsider, idgen.New(), map[string]any{})
			if status != http.StatusForbidden {
				t.Errorf("wipe without device:manage = %d, want 403; body=%s", status, body)
			}
			if d := deviceRow(t, e.env, e.deviceID); d.Wiped() {
				t.Error("an unauthorized caller wiped a device")
			}
		})
	}
}

func TestWipeOverTheAPIMarksTheDevice(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)

			status, body, parsed := e.call("POST", "/api/v1/devices/"+e.deviceID+"/wipe",
				e.env.token, idgen.New(), map[string]any{})
			if status != http.StatusOK {
				t.Fatalf("wipe = %d; body=%s", status, body)
			}
			if got, _ := parsed["status"].(string); got != "wiped" {
				t.Errorf("wipe response status = %q, want \"wiped\"", got)
			}
			if d := deviceRow(t, e.env, e.deviceID); !d.Wiped() {
				t.Error("the API accepted the wipe but the device is not marked")
			}
		})
	}
}

// TestWipeIsAttributable is INV-T4 on the most destructive action in the
// product that does not touch the ledger.
//
// kernel/capability audits its admin mutations for exactly this reason ("a
// capability change is a mutation like any other"); a device wipe is a bigger
// one, and an unattributable wipe would mean nobody could answer "who erased
// that laptop". The audit row is written in the same transaction as the change,
// so a wipe that happened is a wipe that is attributable.
func TestWipeIsAttributable(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)

			status, body, _ := e.call("POST", "/api/v1/devices/"+e.deviceID+"/wipe",
				e.env.token, idgen.New(), map[string]any{})
			if status != http.StatusOK {
				t.Fatalf("wipe = %d; body=%s", status, body)
			}

			var actor string
			err := tenancy.WithTenant(context.Background(), e.db, e.tenant,
				func(ctx context.Context, tx *sql.Tx) error {
					return tx.QueryRowContext(ctx, e.db.Rebind(
						`SELECT actor_id FROM audit_log
						 WHERE tenant_id = ? AND object = 'device' AND record_id = ? AND action = 'wipe'`),
						string(e.tenant), e.deviceID).Scan(&actor)
				})
			if err != nil {
				t.Fatalf("no audit row for the wipe (INV-T4): %v", err)
			}
			if actor == "" {
				t.Error("the wipe was audited with an empty actor — an anonymous mutation")
			}
		})
	}
}

// TestARepeatedWipeDoesNotAuditTwice: the wipe is idempotent, and an audit
// trail of no-ops is noise that hides the entries that matter.
func TestARepeatedWipeDoesNotAuditTwice(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			for range 3 {
				e.call("POST", "/api/v1/devices/"+e.deviceID+"/wipe",
					e.env.token, idgen.New(), map[string]any{})
			}
			var n int
			err := tenancy.WithTenant(context.Background(), e.db, e.tenant,
				func(ctx context.Context, tx *sql.Tx) error {
					return tx.QueryRowContext(ctx, e.db.Rebind(
						`SELECT COUNT(*) FROM audit_log
						 WHERE tenant_id = ? AND object = 'device' AND record_id = ? AND action = 'wipe'`),
						string(e.tenant), e.deviceID).Scan(&n)
				})
			if err != nil {
				t.Fatalf("count audit rows: %v", err)
			}
			if n != 1 {
				t.Errorf("%d audit rows for three identical wipes, want 1", n)
			}
		})
	}
}

func TestWipeOfAnUnknownDeviceIs404(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			status, _, _ := e.call("POST", "/api/v1/devices/no-such-device/wipe",
				e.env.token, idgen.New(), map[string]any{})
			if status != http.StatusNotFound {
				t.Errorf("wipe of an unknown device = %d, want 404", status)
			}
		})
	}
}

// TestWipeCannotCrossATenantBoundary: the most destructive non-ledger action in
// the product must be unreachable across tenants (INV-T1).
func TestWipeCannotCrossATenantBoundary(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			mine := seedDevice(t, db)
			theirs := seedDevice(t, db)

			status, _, _ := mine.call("POST", "/api/v1/devices/"+theirs.deviceID+"/wipe",
				mine.env.token, idgen.New(), map[string]any{})
			if status != http.StatusNotFound {
				t.Errorf("cross-tenant wipe = %d, want 404", status)
			}
			if d := deviceRow(t, theirs.env, theirs.deviceID); d.Wiped() {
				t.Fatal("a tenant wiped another tenant's device")
			}
		})
	}
}

// --- the wipe, end to end through the real replica ---

// TestWipeIsHonoredOnReconnect is the roadmap AC.
//
// The shape matters as much as the assertion. A wipe test that runs against a
// replica which was never populated passes for the wrong reason — an empty
// replica is also what a driver that failed to sync produces. So: hydrate
// first and assert it holds data, wipe, sync again, assert the driver reported
// the wipe *and* that nothing survives.
func TestWipeIsHonoredOnReconnect(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			seedScopeFixture(t, e.env)

			run := newReplicaRun(t, e.env)
			run.token = e.token

			before := run.syncWithArgs()
			if len(before["Contact"]) == 0 {
				t.Fatal("fixture: the replica holds nothing, so a wipe would prove nothing")
			}

			wipe(t, e.env, e.deviceID)

			after, stderr := run.syncExpectingWipe()
			if !strings.Contains(stderr, "lasterp-device-wiped") {
				t.Fatalf("the driver did not report a wipe; the replica may have been emptied "+
					"by something else entirely. stderr: %s", stderr)
			}
			for object, rows := range after {
				if len(rows) != 0 {
					t.Errorf("%s survived the wipe with %d rows", object, len(rows))
				}
			}
		})
	}
}

// TestWipeDestroysUnsentWork is decisions §5: a wipe takes the outbox, and that
// is the deliberate opposite of WP-2.4's scope purge.
//
// Losing unsent work is the point — it is data on a device that should not have
// data. The paired test below guards the inversion.
func TestWipeDestroysUnsentWork(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			seedScopeFixture(t, e.env)

			run := newReplicaRun(t, e.env)
			run.token = e.token
			run.syncWithArgs()

			queued := run.syncWithArgs("--offline", "--enqueue=3", "--label=Doomed")
			if got := len(queued["_outbox"]); got != 3 {
				t.Fatalf("fixture: expected 3 queued commands, got %d", got)
			}

			wipe(t, e.env, e.deviceID)

			after, stderr := run.syncExpectingWipe()
			if !strings.Contains(stderr, "lasterp-device-wiped") {
				t.Fatalf("the driver did not report a wipe; stderr: %s", stderr)
			}
			if got := len(after["_outbox"]); got != 0 {
				t.Errorf("%d queued commands survived the wipe — a wiped device must keep "+
					"nothing, unsent work included (decisions §5)", got)
			}
			if got := len(after["_conflicts"]); got != 0 {
				t.Errorf("%d conflict rows survived the wipe", got)
			}
			// And the server never saw the work: the drain throws past itself
			// on a wipe rather than replaying into a tenant the device is no
			// longer trusted to write to (decisions §6).
			if n := serverCountNamed(t, e.env, "Doomed"); n != 0 {
				t.Errorf("%d queued commands were replayed to the server by a wiped device", n)
			}
		})
	}
}

// TestScopePurgeStillSparesUnsentWork is the paired regression for the rule
// above. The two live a few lines apart in the same subsystem and say opposite
// things; getting them backwards in either direction is a serious bug — a purge
// that ate the outbox loses a user's work, and a wipe that spared it leaves the
// thief a copy. WP-2.4 has its own test for this; this one exists so a change
// to *wipe.ts* that leaked into the purge path fails here.
func TestScopePurgeStillSparesUnsentWork(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seedDevice(t, db)
			seedScopeFixture(t, e.env)
			_, role, token := scopedUser(t, e.env, scopedGrants())

			run := newReplicaRun(t, e.env)
			run.token = token
			run.syncWithArgs()
			run.syncWithArgs("--offline", "--enqueue=3", "--label=Spared")

			// A revocation, not a wipe.
			revoke(t, e.db, e.tenant, role, "Contact", "read")
			revoke(t, e.db, e.tenant, role, "Contact", "create")

			after := run.syncWithArgs()
			if got := len(after["Contact"]); got != 0 {
				t.Fatalf("fixture: the purge did not run, %d rows remain", got)
			}
			if q, f := len(after["_outbox"]), len(after["_conflicts"]); q+f == 0 {
				t.Error("a scope purge destroyed queued work — that is the WIPE's rule, and " +
					"applying it to a revocation loses a user's own unsent changes " +
					"(WP-2.4-decisions.md §5)")
			}
		})
	}
}

// --- helpers ---

// syncExpectingWipe runs the driver against a wiped device. The driver exits 0
// — a wipe is a successful outcome for it, not a crash — and reports the wipe
// on stderr, which is what the caller asserts on.
func (r *replicaRun) syncExpectingWipe() (map[string][]map[string]any, string) {
	r.t.Helper()
	out, stderr, err := r.exec()
	if err != nil {
		r.t.Fatalf("replica driver failed on a wiped device: %v\nstderr: %s", err, stderr)
	}
	var dump map[string][]map[string]any
	if err := json.Unmarshal(out, &dump); err != nil {
		r.t.Fatalf("replica driver emitted unparseable output: %v\n%s", err, out)
	}
	return dump, stderr
}

func deviceRow(t *testing.T, e *env, deviceID string) identity.Device {
	t.Helper()
	d, err := identity.GetDevice(context.Background(), e.db, e.tenant, deviceID)
	if err != nil {
		t.Fatalf("GetDevice(%s): %v", deviceID, err)
	}
	return d
}
