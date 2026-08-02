//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// WP-1.12. The AC is one flow: enroll → confirm → sign in with a code →
// consume a recovery code → disable, driven over HTTP; a recovery code cannot
// be reused; enrollment and disablement appear in audit_log.
//
// Invariants: INV-T1 (the new tenant-scoped table), INV-T2 (the routes are
// authenticated — the Public allowlist does not grow, and no route accepts a
// user id, so nobody can act on another account), INV-T4 (every transition is
// audited).

const totpTestPassword = "c0rrect-horse-battery"

// totpUser creates a password user, grants it nothing in particular (the /me
// routes are ungated by design) and logs in over HTTP for a real session token.
func (e *env) totpUser(t *testing.T) (identity.UserID, string, string) {
	t.Helper()
	id, email := e.createLoginUser(t, totpTestPassword)
	token := e.loginToken(t, map[string]any{
		"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
	})
	return id, email, token
}

// loginToken posts credentials to the session route and returns the session
// cookie's value, so every step of this test goes through the real front door.
func (e *env) loginToken(t *testing.T, creds map[string]any) string {
	t.Helper()
	req := newJSONRequest(t, "POST", e.server.URL+"/api/v1/sessions", creds)
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieSession {
			return c.Value
		}
	}
	t.Fatal("login set no session cookie")
	return ""
}

// loginStatus posts credentials and returns only the status, for the failure
// cases that must not be distinguishable from one another.
func (e *env) loginStatus(t *testing.T, creds map[string]any) int {
	t.Helper()
	req := newJSONRequest(t, "POST", e.server.URL+"/api/v1/sessions", creds)
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func newJSONRequest(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, jsonBody(t, body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// codeFor computes the current TOTP code from the secret the enroll response
// handed back — the same thing an authenticator app does with the QR.
func codeFor(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := identity.CurrentTOTPCode(secret, at)
	if err != nil {
		t.Fatalf("CurrentTOTPCode: %v", err)
	}
	return code
}

// TestTOTPEnrollmentLifecycle is the acceptance criterion, end to end.
func TestTOTPEnrollmentLifecycle(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			userID, email, token := e.totpUser(t)
			e.token = token

			// --- status: nothing enabled, nothing pending ---
			status, _, body := e.get("/api/v1/me/totp")
			if status != http.StatusOK {
				t.Fatalf("GET status = %d, want 200", status)
			}
			if body["enabled"] != false || body["pending"] != false {
				t.Fatalf("fresh account reports enabled=%v pending=%v", body["enabled"], body["pending"])
			}

			// --- enroll ---
			// A session alone is not enough. Without the password, a stolen
			// session could enroll an authenticator the real owner does not
			// hold and then never be undone (decisions §4, corrected).
			if s, _, _ := e.post("/api/v1/me/totp/enroll", map[string]any{}); s != http.StatusUnauthorized {
				t.Errorf("enroll with no password = %d, want 401", s)
			}
			if s, _, _ := e.post("/api/v1/me/totp/enroll", map[string]any{"password": "wrong"}); s != http.StatusUnauthorized {
				t.Errorf("enroll with a wrong password = %d, want 401", s)
			}

			status, _, enroll := e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword})
			if status != http.StatusOK {
				t.Fatalf("enroll status = %d, want 200", status)
			}
			secret := mustField(t, enroll, "secret")
			uri := mustField(t, enroll, "otpauth_uri")
			qr := mustField(t, enroll, "qr_png")
			if !strings.HasPrefix(uri, "otpauth://totp/") || !strings.Contains(uri, "secret="+secret) {
				t.Errorf("otpauth URI %q does not carry the secret", uri)
			}
			if !strings.HasPrefix(qr, "data:image/png;base64,") {
				t.Errorf("qr_png is not a PNG data URI: %.40q", qr)
			}

			// A pending enrollment must not yet demand a code: password-only
			// login still works, and the factor is not on.
			if _, _, s := e.get("/api/v1/me/totp"); s["pending"] != true || s["enabled"] != false {
				t.Errorf("after enroll: pending=%v enabled=%v, want true/false", s["pending"], s["enabled"])
			}
			if got := e.loginStatus(t, map[string]any{
				"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
			}); got != http.StatusOK {
				t.Errorf("password login during pending enrollment = %d, want 200", got)
			}

			// --- confirm ---
			now := time.Now().UTC()
			status, _, confirm := e.post("/api/v1/me/totp/confirm", map[string]any{"code": codeFor(t, secret, now)})
			if status != http.StatusOK {
				t.Fatalf("confirm status = %d, want 200", status)
			}
			codes := stringSlice(t, confirm, "recovery_codes")
			if len(codes) != identity.RecoveryCodeCount {
				t.Fatalf("got %d recovery codes, want %d", len(codes), identity.RecoveryCodeCount)
			}
			if confirm["enabled"] != true {
				t.Error("confirm did not report the factor enabled")
			}

			// --- the factor is now required ---
			if got := e.loginStatus(t, map[string]any{
				"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
			}); got != http.StatusUnauthorized {
				t.Errorf("password-only login after enabling TOTP = %d, want 401", got)
			}

			// --- sign in with a code (a later step, since confirm burned this one) ---
			next := now.Add(totpStepForTest)
			sessionToken := e.loginToken(t, map[string]any{
				"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
				"totp": codeFor(t, secret, next),
			})
			if sessionToken == "" {
				t.Fatal("TOTP login issued no session")
			}

			// --- consume a recovery code ---
			e.token = e.loginToken(t, map[string]any{
				"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
				"recovery_code": codes[0],
			})
			// AC: a recovery code cannot be reused.
			if got := e.loginStatus(t, map[string]any{
				"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
				"recovery_code": codes[0],
			}); got != http.StatusUnauthorized {
				t.Errorf("replayed recovery code = %d, want 401", got)
			}
			if _, _, s := e.get("/api/v1/me/totp"); s["recovery_codes_remaining"] != float64(identity.RecoveryCodeCount-1) {
				t.Errorf("remaining = %v, want %d", s["recovery_codes_remaining"], identity.RecoveryCodeCount-1)
			}

			// --- disable requires the password AND a second factor ---
			if status, _, _ := e.post("/api/v1/me/totp/disable", map[string]any{
				"password": totpTestPassword,
			}); status != http.StatusUnauthorized {
				t.Errorf("disable with a session and password only = %d, want 401 (a stolen session must not strip MFA)", status)
			}
			if status, _, _ := e.post("/api/v1/me/totp/disable", map[string]any{
				"password": "wrong", "recovery_code": codes[1],
			}); status != http.StatusUnauthorized {
				t.Errorf("disable with a wrong password = %d, want 401", status)
			}
			// A recovery code counts as the second factor — the user who needs
			// to disable is usually the one who lost their authenticator.
			if status, _, _ := e.post("/api/v1/me/totp/disable", map[string]any{
				"password": totpTestPassword, "recovery_code": codes[1],
			}); status != http.StatusNoContent {
				t.Fatalf("disable = %d, want 204", status)
			}

			if _, _, s := e.get("/api/v1/me/totp"); s["enabled"] != false || s["recovery_codes_remaining"] != float64(0) {
				t.Errorf("after disable: enabled=%v remaining=%v, want false/0", s["enabled"], s["recovery_codes_remaining"])
			}
			// Password alone works again.
			if got := e.loginStatus(t, map[string]any{
				"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
			}); got != http.StatusOK {
				t.Errorf("password login after disable = %d, want 200", got)
			}

			// --- AC: enrollment and disablement appear in audit_log (INV-T4) ---
			for _, action := range []string{
				identity.AuditTOTPEnrollStarted,
				identity.AuditTOTPEnabled,
				identity.AuditTOTPDisabled,
				identity.AuditRecoveryUsed,
			} {
				if n := accountAuditCount(t, e, userID, action); n == 0 {
					t.Errorf("no audit_log row for %q (INV-T4)", action)
				}
			}
		})
	}
}

// AC (decisions §9): the show-once credentials must not end up in plaintext in
// idempotency_keys.response_body, which has no TTL and no cleanup — nor in
// audit_log.changes. Same shape as the WP-3.0 criterion.
func TestTOTPSecretsAreNotStoredInPlaintextAnywhere(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, _, token := e.totpUser(t)
			e.token = token

			_, _, enroll := e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword})
			secret := mustField(t, enroll, "secret")
			_, _, confirm := e.post("/api/v1/me/totp/confirm",
				map[string]any{"code": codeFor(t, secret, time.Now().UTC())})
			codes := stringSlice(t, confirm, "recovery_codes")

			needles := append([]string{secret}, codes...)
			for _, table := range []struct{ name, col string }{
				{"idempotency_keys", "response_body"},
				{"audit_log", "changes"},
			} {
				for _, needle := range needles {
					if n := countLike(t, e, table.name, table.col, needle); n != 0 {
						t.Errorf("%s.%s contains a credential in plaintext (%d rows)", table.name, table.col, n)
					}
				}
			}
		})
	}
}

// The fingerprint is the other half of the same problem. idempotency_keys
// stores SHA-256(method ‖ path ‖ body), unsalted and forever — and the disable
// body is {"password":"…","totp":"123456"}. Method and path are constants and a
// TOTP code is ~20 bits, so that row would be an offline password oracle orders
// of magnitude cheaper than the bcrypt in users.password_hash. Hashing it is
// not storing it in plaintext, which is why the grep above would not catch it.
func TestCredentialWritesDoNotFingerprintTheirBody(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, _, token := e.totpUser(t)
			e.token = token

			_, _, enroll := e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword})
			secret := mustField(t, enroll, "secret")
			e.post("/api/v1/me/totp/confirm", map[string]any{"code": codeFor(t, secret, time.Now().UTC())})
			// A wrong second factor, so the row survives: a failed write
			// discards its reservation, and only a 2xx keeps one.
			e.post("/api/v1/me/totp/disable", map[string]any{
				"password": totpTestPassword, "totp": "000000",
			})

			// What the fingerprint would be if the body were included.
			for _, body := range []string{
				`{"password":"` + totpTestPassword + `","totp":"000000","recovery_code":""}`,
				`{"password":"` + totpTestPassword + `"}`,
			} {
				leaked := fingerprintOf("POST", "/api/v1/me/totp/disable", body)
				if n := countLike(t, e, "idempotency_keys", "request_fingerprint", leaked); n != 0 {
					t.Errorf("idempotency_keys.request_fingerprint is a hash of the password-bearing body")
				}
			}

			// The enroll/confirm rows exist (idempotency still works), they
			// just do not commit to a body.
			bodyless := fingerprintOf("POST", "/api/v1/me/totp/enroll", "")
			if n := countLike(t, e, "idempotency_keys", "request_fingerprint", bodyless); n != 1 {
				t.Errorf("expected exactly 1 body-free enroll fingerprint, got %d", n)
			}
		})
	}
}

// fingerprintOf mirrors kernel/api's fingerprint (method ‖ 0 ‖ path ‖ 0 ‖ body).
// Duplicated rather than exported: a test that computes the value independently
// is what makes it evidence.
func fingerprintOf(method, path, body string) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// A false audit row is worse than none: an incident reviewer reads
// `totp.recovery.used` as "this user lost their authenticator". It must be
// written when a code was actually spent, and not when one was merely offered
// to an account that has no second factor to check it against.
func TestAuditRowsDescribeTransitionsThatHappened(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			userID, email, token := e.totpUser(t)
			e.token = token

			// TOTP is off. A login carrying a recovery_code succeeds on the
			// password — the field is never read — so nothing was consumed.
			if got := e.loginStatus(t, map[string]any{
				"tenant": string(e.tenant), "email": email, "password": totpTestPassword,
				"recovery_code": "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF",
			}); got != http.StatusOK {
				t.Fatalf("login = %d, want 200", got)
			}
			if n := accountAuditCount(t, e, userID, identity.AuditRecoveryUsed); n != 0 {
				t.Errorf("%d totp.recovery.used rows for a login that spent no code", n)
			}

			// Disabling a factor that is not on is a 409, and writes nothing.
			if s, _, _ := e.post("/api/v1/me/totp/disable", map[string]any{
				"password": totpTestPassword,
			}); s != http.StatusConflict {
				t.Errorf("disable with TOTP off = %d, want 409", s)
			}
			if n := accountAuditCount(t, e, userID, identity.AuditTOTPDisabled); n != 0 {
				t.Errorf("%d totp.disabled rows for an account that had no second factor", n)
			}

			// And a pending enrollment survives it, rather than being quietly
			// discarded by anyone holding a session and the password.
			e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword})
			e.post("/api/v1/me/totp/disable", map[string]any{"password": totpTestPassword})
			if _, _, s := e.get("/api/v1/me/totp"); s["pending"] != true {
				t.Error("a disable call discarded a pending enrollment")
			}
		})
	}
}

// NoStoreResponse must drop the body without dropping idempotency: a replayed
// key still returns the original status and Idempotent-Replayed, and must not
// re-reveal the codes or run the write twice.
func TestCredentialWriteReplayReturnsNoBody(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, _, token := e.totpUser(t)
			e.token = token

			_, _, enroll := e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword})
			secret := mustField(t, enroll, "secret")

			key := idgen.New()
			body := map[string]any{"code": codeFor(t, secret, time.Now().UTC())}
			status, first, parsed := e.call("POST", "/api/v1/me/totp/confirm", e.token, key, body)
			if status != http.StatusOK {
				t.Fatalf("confirm status = %d, want 200", status)
			}
			codes := stringSlice(t, parsed, "recovery_codes")

			status, replayed, _ := e.call("POST", "/api/v1/me/totp/confirm", e.token, key, body)
			if status != http.StatusOK {
				t.Errorf("replay status = %d, want 200", status)
			}
			if len(replayed) != 0 {
				t.Errorf("replay returned a body (%q); a show-once credential must not be re-revealed", replayed)
			}
			if strings.Contains(string(replayed), codes[0]) || len(first) == len(replayed) {
				t.Error("replay re-revealed the recovery codes")
			}
			// The write ran once: still exactly one live set of codes.
			if _, _, s := e.get("/api/v1/me/totp"); s["recovery_codes_remaining"] != float64(identity.RecoveryCodeCount) {
				t.Errorf("remaining = %v, want %d", s["recovery_codes_remaining"], identity.RecoveryCodeCount)
			}
		})
	}
}

// INV-T2: the routes act on the caller and nobody else. There is no id to pass,
// and an unauthenticated caller gets nothing — the Public allowlist did not
// grow to accommodate enrollment (INV-T2; routes_integrity_test.go pins the
// allowlist itself).
func TestTOTPRoutesAreSelfServiceAndAuthenticated(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, _, token := e.totpUser(t)

			for _, route := range []struct{ method, path string }{
				{"GET", "/api/v1/me/totp"},
				{"POST", "/api/v1/me/totp/enroll"},
				{"POST", "/api/v1/me/totp/confirm"},
				{"POST", "/api/v1/me/totp/disable"},
			} {
				status, _, _ := e.call(route.method, route.path, "", idgen.New(), map[string]any{})
				if status != http.StatusUnauthorized {
					t.Errorf("unauthenticated %s %s = %d, want 401", route.method, route.path, status)
				}
			}

			// Two users, two independent enrollments: alice's enrollment must
			// not touch bob's account. The routes take no id, so this is a
			// property of the shape rather than of a check — which is the point.
			e.token = token
			e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword})
			_, _, bobToken := e.totpUser(t)
			e.token = bobToken
			if _, _, s := e.get("/api/v1/me/totp"); s["pending"] != false {
				t.Error("one user's enrollment showed up as another user's pending state")
			}
		})
	}
}

// Rotating a live factor is disable-then-enroll, so both halves are audited.
func TestEnrollWhileEnabledIsAConflict(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			_, _, token := e.totpUser(t)
			e.token = token

			_, _, enroll := e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword})
			secret := mustField(t, enroll, "secret")
			if status, _, _ := e.post("/api/v1/me/totp/confirm",
				map[string]any{"code": codeFor(t, secret, time.Now().UTC())}); status != http.StatusOK {
				t.Fatalf("confirm failed")
			}
			if status, _, _ := e.post("/api/v1/me/totp/enroll", map[string]any{"password": totpTestPassword}); status != http.StatusConflict {
				t.Errorf("re-enroll while enabled = %d, want 409", status)
			}
		})
	}
}

// totpStepForTest mirrors the RFC 6238 step so the test can ask for a code from
// the *next* window — the one confirm did not burn.
const totpStepForTest = 30 * time.Second

func accountAuditCount(t *testing.T, e *env, user identity.UserID, action string) int {
	t.Helper()
	var n int
	err := tenancy.WithTenant(context.Background(), e.db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, e.db.Rebind(`
			SELECT COUNT(*) FROM audit_log
			WHERE tenant_id = ? AND actor_id = ? AND action = ?`),
			string(e.tenant), string(user), action).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// countLike greps a column for a literal needle. The parameter carries the
// wildcards, so the needle is never concatenated into SQL.
func countLike(t *testing.T, e *env, table, column, needle string) int {
	t.Helper()
	var n int
	// #nosec G202 -- table and column are literals from this test's own list,
	// never request data; the needle is a bound parameter.
	query := `SELECT COUNT(*) FROM ` + table + ` WHERE ` + column + ` LIKE ?`
	err := tenancy.WithTenant(context.Background(), e.db, e.tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, e.db.Rebind(query), "%"+needle+"%").Scan(&n)
	})
	if err != nil {
		t.Fatalf("scan %s.%s: %v", table, column, err)
	}
	return n
}

func stringSlice(t *testing.T, m map[string]any, key string) []string {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		t.Fatalf("expected array field %q in %v", key, m)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("non-string element in %q", key)
		}
		out = append(out, s)
	}
	return out
}
