// SPDX-License-Identifier: AGPL-3.0-only

package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// SessionTTL is how long an issued session's access token is valid.
const SessionTTL = 24 * time.Hour

// SessionID identifies a session row.
type SessionID string

// ErrSessionInvalid covers any reason a token doesn't grant access:
// unknown, expired, or revoked. Deliberately undifferentiated so callers
// can't distinguish "wrong token" from "right token, revoked" (that
// distinction is an oracle an attacker could use).
var ErrSessionInvalid = errors.New("identity: session invalid")

// ErrDeviceMismatch is returned by RefreshSession when the refresh token
// is presented from a device other than the one it was issued to
// (08-SECURITY-MULTITENANCY.md: "refresh bound to device").
var ErrDeviceMismatch = errors.New("identity: refresh token device mismatch")

// Session is a tenant/user-bound bearer-token grant. Only token hashes are
// stored; the plaintext token exists solely in IssuedSession, returned
// once at issuance/refresh time.
type Session struct {
	ID        SessionID
	TenantID  tenancy.ID
	UserID    UserID
	DeviceID  string
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// IssuedSession carries the plaintext bearer tokens back to the caller at
// issuance time. Neither token is retrievable again once this value is
// discarded — session.go stores only their SHA-256 hashes.
type IssuedSession struct {
	ID           SessionID
	Token        string
	RefreshToken string
	ExpiresAt    time.Time
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueSession creates a new session for user on device, scoped to
// tenant. tenant, user and device must all be non-empty (INV-T4: every
// session is attributable to a principal and a device).
func IssueSession(ctx context.Context, db *storage.DB, tenant tenancy.ID, user UserID, deviceID string) (*IssuedSession, error) {
	if tenant == "" || user == "" || deviceID == "" {
		return nil, errors.New("identity: tenant, user and device are required to issue a session")
	}
	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("identity: generate token: %w", err)
	}
	refresh, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("identity: generate refresh token: %w", err)
	}

	now := time.Now().UTC()
	issued := &IssuedSession{
		ID:           SessionID(idgen.New()),
		Token:        token,
		RefreshToken: refresh,
		ExpiresAt:    now.Add(SessionTTL),
	}

	// A device an administrator has acted on cannot get a fresh session by
	// logging in again — otherwise revocation and wipe would both be undone by
	// the most ordinary action a user takes. Checked before the insert so no
	// session exists for a device that may not have one.
	switch device, err := GetDevice(ctx, db, tenant, deviceID); {
	case errors.Is(err, sql.ErrNoRows):
		// First time this device is seen: registered below.
	case err != nil:
		return nil, err
	case device.Wiped():
		return nil, ErrDeviceWiped
	case device.Revoked():
		return nil, ErrSessionInvalid
	}

	_, err = db.ExecContext(ctx, db.Rebind(`
		INSERT INTO sessions (id, tenant_id, user_id, device_id, token_hash, refresh_token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		string(issued.ID), string(tenant), string(user), deviceID,
		hashToken(token), hashToken(refresh), now, issued.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("identity: issue session: %w", err)
	}

	// Registration is implicit (decisions §1): a device that can hold a session
	// is a device worth tracking, and an enrolment step a client could skip is
	// one that leaves an untracked replica.
	if err := RegisterDevice(ctx, db, tenant, user, deviceID); err != nil {
		return nil, err
	}
	return issued, nil
}

// ValidateSession resolves a bearer token to its session, or
// ErrSessionInvalid if the token is unknown, expired, or revoked. This
// necessarily runs before tenant context exists — the token is what
// determines the tenant — which is why sessions is exempt from RLS
// (docs/notes/WP-0.3-decisions.md).
//
// A transient SQLITE_BUSY is retried rather than returned. Every other write
// path in the kernel goes through tenancy.WithTenant, which has retried busy
// for the same reason since WP-0.3; this read is outside a tenant transaction
// (it is what *determines* the tenant) and so had no retry at all. That gap is
// worse here than anywhere else: session validation runs on every single
// request, and the failure surfaces to the caller as a refused credential.
// WP-2.3b's simulation harness put four concurrent clients through it and got
// a spurious authentication failure within three rounds.
func ValidateSession(ctx context.Context, db *storage.DB, token string) (*Session, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}

	// A shorter budget than WithTenant's 30s: that one is protecting a write
	// that must not be lost, this is one read on the front of a request that
	// has a caller waiting. Past a second, failing and letting the client
	// retry is the better answer.
	const busyRetryBudget = time.Second

	deadline := time.Now().Add(busyRetryBudget)
	for attempt := 0; ; attempt++ {
		s, err := validateSessionOnce(ctx, db, token)
		if err == nil || !storage.IsBusy(err) || time.Now().After(deadline) {
			return s, err
		}
		time.Sleep(tenancy.BusyBackoff(attempt))
	}
}

func validateSessionOnce(ctx context.Context, db *storage.DB, token string) (*Session, error) {
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT id, tenant_id, user_id, device_id, expires_at, revoked_at
		FROM sessions WHERE token_hash = ?`), hashToken(token))

	s, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	if s.RevokedAt != nil || time.Now().UTC().After(s.ExpiresAt) {
		return nil, ErrSessionInvalid
	}

	// The device behind the session — INV-D1, WP-2.5. Checked here rather than
	// at any single endpoint because a wiped device must be refused on *every*
	// authenticated path; there is no request it can make that succeeds while
	// skipping this.
	//
	// **This is a second query and it must not be folded into the SELECT
	// above.** `devices` carries FORCE ROW LEVEL SECURITY, and this function
	// necessarily runs before tenant context exists — the token is what
	// determines the tenant, which is why `sessions` is RLS-exempt
	// (WP-0.3-decisions.md). A LEFT JOIN would therefore evaluate the devices
	// policy with `app.tenant_id` unset, match no rows, and produce NULL device
	// columns — i.e. every wiped device would silently pass the check while the
	// code read as though it were enforcing it. GetDevice runs inside
	// tenancy.WithTenant, using the tenant this session just resolved.
	device, err := GetDevice(ctx, db, s.TenantID, s.DeviceID)
	if errors.Is(err, sql.ErrNoRows) {
		// A session predating this table, or one whose device row was deleted.
		// Not a wipe: absence of a wipe instruction is not a wipe instruction,
		// and failing closed here would lock out every session issued before
		// WP-2.5 shipped.
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if device.Wiped() {
		// Record delivery here, where the refusal is decided, so no caller of
		// ValidateSession can forget to. Best-effort on purpose: the refusal is
		// the control and it must happen whether or not the bookkeeping write
		// lands, so a failure here is swallowed rather than turned into a 503
		// that would let the device keep operating (decisions §4).
		_ = MarkWipeDelivered(ctx, db, s.TenantID, s.DeviceID)
		return nil, ErrDeviceWiped
	}
	return s, nil
}

// RefreshSession issues a new access token for the session owning
// refreshToken, provided it is presented from the same deviceID it was
// issued to.
//
// The old access token stops working immediately: the row's token_hash is
// replaced, not appended to, so a session has exactly one live access token at
// a time. (This comment previously claimed the opposite — that the old token
// kept working until it expired — which never matched the code. The behaviour
// is the safer of the two readings and is what the tests assert; only the
// comment was wrong. Corrected in WP-1.10.)
func RefreshSession(ctx context.Context, db *storage.DB, refreshToken, deviceID string) (*IssuedSession, error) {
	if refreshToken == "" || deviceID == "" {
		return nil, ErrSessionInvalid
	}
	row := db.QueryRowContext(ctx, db.Rebind(`
		SELECT id, tenant_id, user_id, device_id, expires_at, revoked_at
		FROM sessions WHERE refresh_token_hash = ?`), hashToken(refreshToken))

	s, err := scanSession(row)
	if err != nil {
		return nil, err
	}
	if s.RevokedAt != nil {
		return nil, ErrSessionInvalid
	}
	if s.DeviceID != deviceID {
		return nil, ErrDeviceMismatch
	}

	// Refresh is a *public* route, so INV-D1's "every authenticated path" does
	// not reach it — and without this check a wiped device would keep minting
	// fresh access tokens indefinitely, each one useless for data but each one
	// extending a session that should be over. Revoked devices are stopped here
	// too, for the same reason IssueSession stops them: an administrator's
	// decision must not be undone by the most routine thing a client does.
	switch device, err := GetDevice(ctx, db, s.TenantID, s.DeviceID); {
	case errors.Is(err, sql.ErrNoRows):
		// Predates the devices table; not a wipe.
	case err != nil:
		return nil, err
	case device.Wiped():
		// Deliberately the typed error rather than a bare refusal: an expiring
		// client hits refresh before it hits anything else, so this is often
		// the first place a wipe can be delivered.
		_ = MarkWipeDelivered(ctx, db, s.TenantID, s.DeviceID)
		return nil, ErrDeviceWiped
	case device.Revoked():
		return nil, ErrSessionInvalid
	}

	newToken, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("identity: generate token: %w", err)
	}
	expiresAt := time.Now().UTC().Add(SessionTTL)
	_, err = db.ExecContext(ctx, db.Rebind(`
		UPDATE sessions SET token_hash = ?, expires_at = ? WHERE id = ?`),
		hashToken(newToken), expiresAt, string(s.ID))
	if err != nil {
		return nil, fmt.Errorf("identity: refresh session: %w", err)
	}
	return &IssuedSession{ID: s.ID, Token: newToken, RefreshToken: refreshToken, ExpiresAt: expiresAt}, nil
}

// RevokeSession invalidates a session immediately.
func RevokeSession(ctx context.Context, db *storage.DB, id SessionID) error {
	_, err := db.ExecContext(ctx, db.Rebind(`
		UPDATE sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`),
		time.Now().UTC(), string(id))
	if err != nil {
		return fmt.Errorf("identity: revoke session: %w", err)
	}
	return nil
}

func scanSession(row *sql.Row) (*Session, error) {
	var s Session
	var idStr, tenantStr, userStr string
	var expiresAt storage.Time
	var revokedAt storage.NullTime
	err := row.Scan(&idStr, &tenantStr, &userStr, &s.DeviceID, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("identity: scan session: %w", err)
	}
	s.ID = SessionID(idStr)
	s.TenantID = tenancy.ID(tenantStr)
	s.UserID = UserID(userStr)
	s.ExpiresAt = expiresAt.Time
	if revokedAt.Valid {
		s.RevokedAt = &revokedAt.Time
	}
	return &s, nil
}
