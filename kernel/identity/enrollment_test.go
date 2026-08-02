package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// mustEnableTOTP drives the real enrollment path — pending secret, then a
// confirmation code — and returns the secret.
//
// It confirms two steps in the past on purpose. ConfirmTOTPEnrollment burns the
// counter it matched (so the confirmation code cannot double as a login code),
// which means confirming at "now" would make a current-step login code a
// replay. Every caller wants an account that can log in immediately after.
func mustEnableTOTP(t *testing.T, ctx context.Context, db *storage.DB, tenant tenancy.ID, id UserID) string {
	t.Helper()
	secret, err := StartTOTPEnrollment(ctx, db, tenant, id)
	if err != nil {
		t.Fatalf("StartTOTPEnrollment: %v", err)
	}
	past := time.Now().UTC().Add(-2 * totpStep)
	code, _, err := totpCounter(secret, past, 0)
	if err != nil {
		t.Fatalf("totpCounter: %v", err)
	}
	if _, err := ConfirmTOTPEnrollment(ctx, db, tenant, id, code, past); err != nil {
		t.Fatalf("ConfirmTOTPEnrollment: %v", err)
	}
	return secret
}

func mustUser(t *testing.T, ctx context.Context, db *storage.DB, tenant tenancy.ID, email string) *User {
	t.Helper()
	hash, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u, err := CreateUser(ctx, db, tenant, email, hash)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// A pending enrollment must not change how the account authenticates: the
// secret is stored but totp_enabled is false, so a password alone still works
// and the not-yet-confirmed code is not demanded (decisions §1).
func TestPendingEnrollmentDoesNotAffectLogin(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "pending@example.com")

			if _, err := StartTOTPEnrollment(ctx, db, tenant, u.ID); err != nil {
				t.Fatalf("StartTOTPEnrollment: %v", err)
			}

			p := &PasswordTOTPProvider{DB: db}
			if _, err := p.Authenticate(ctx, tenant, Credentials{
				Email: "pending@example.com", Password: "s3cret!",
			}); err != nil {
				t.Fatalf("password-only login during pending enrollment: %v", err)
			}

			got, err := GetUserByID(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GetUserByID: %v", err)
			}
			if got.TOTPEnabled {
				t.Error("TOTP is enabled after starting enrollment; confirmation must be required")
			}
			if got.TOTPSecret == "" {
				t.Error("no pending secret stored")
			}
		})
	}
}

// The code that confirms an enrollment must not still work as a login code:
// ConfirmTOTPEnrollment persists the matched counter, closing the step.
func TestConfirmationCodeIsNotAlsoALoginCode(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "burn@example.com")

			secret, err := StartTOTPEnrollment(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("StartTOTPEnrollment: %v", err)
			}
			now := time.Now().UTC()
			code, _, err := totpCounter(secret, now, 0)
			if err != nil {
				t.Fatalf("totpCounter: %v", err)
			}
			if _, err := ConfirmTOTPEnrollment(ctx, db, tenant, u.ID, code, now); err != nil {
				t.Fatalf("ConfirmTOTPEnrollment: %v", err)
			}

			p := &PasswordTOTPProvider{DB: db}
			if _, err := p.Authenticate(ctx, tenant, Credentials{
				Email: "burn@example.com", Password: "s3cret!", TOTPCode: code,
			}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("replayed confirmation code: err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

func TestConfirmRejectsAWrongCode(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "wrong@example.com")

			if _, err := StartTOTPEnrollment(ctx, db, tenant, u.ID); err != nil {
				t.Fatalf("StartTOTPEnrollment: %v", err)
			}
			if _, err := ConfirmTOTPEnrollment(ctx, db, tenant, u.ID, "000000", time.Now().UTC()); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("wrong code: err = %v, want ErrInvalidCredentials", err)
			}
			got, err := GetUserByID(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GetUserByID: %v", err)
			}
			if got.TOTPEnabled {
				t.Error("a wrong confirmation code enabled TOTP")
			}
		})
	}
}

// Confirming with nothing pending must not enable anything.
func TestConfirmWithoutAPendingSecret(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "nopending@example.com")

			if _, err := ConfirmTOTPEnrollment(ctx, db, tenant, u.ID, "123456", time.Now().UTC()); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("confirm with nothing pending: err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

// Re-enrolling on a live second factor is refused: rotating one is
// disable-then-enroll, so the audit trail records both halves (decisions §8).
func TestEnrollTwiceIsRefused(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "twice@example.com")
			mustEnableTOTP(t, ctx, db, tenant, u.ID)

			if _, err := StartTOTPEnrollment(ctx, db, tenant, u.ID); !errors.Is(err, ErrTOTPAlreadyEnabled) {
				t.Fatalf("second enrollment: err = %v, want ErrTOTPAlreadyEnabled", err)
			}
		})
	}
}

// TOTP is the second factor of the password path. An account with no password
// could enroll but then never re-authenticate to disable it.
func TestEnrollWithoutAPasswordIsRefused(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u, err := CreateUser(ctx, db, tenant, "idp@example.com", "")
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			if _, err := StartTOTPEnrollment(ctx, db, tenant, u.ID); !errors.Is(err, ErrPasswordRequired) {
				t.Fatalf("passwordless enrollment: err = %v, want ErrPasswordRequired", err)
			}
		})
	}
}

// Disable clears every trace, including recovery codes: a re-enrollment must
// not inherit codes minted against the previous secret.
func TestDisableClearsSecretAndRecoveryCodes(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "off@example.com")
			mustEnableTOTP(t, ctx, db, tenant, u.ID)
			codes, err := GenerateRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GenerateRecoveryCodes: %v", err)
			}

			if err := DisableTOTP(ctx, db, tenant, u.ID); err != nil {
				t.Fatalf("DisableTOTP: %v", err)
			}

			got, err := GetUserByID(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GetUserByID: %v", err)
			}
			if got.TOTPEnabled || got.TOTPSecret != "" || got.TOTPLastCounter != nil {
				t.Errorf("disable left state behind: enabled=%v secret=%q counter=%v",
					got.TOTPEnabled, got.TOTPSecret, got.TOTPLastCounter)
			}
			remaining, err := CountUnusedRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("CountUnusedRecoveryCodes: %v", err)
			}
			if remaining != 0 {
				t.Errorf("disable left %d recovery codes; a re-enrollment would inherit them", remaining)
			}
			// The old codes must be dead even though the rows are gone.
			ok, err := ConsumeRecoveryCode(ctx, db, tenant, u.ID, codes[0])
			if err != nil {
				t.Fatalf("ConsumeRecoveryCode: %v", err)
			}
			if ok {
				t.Error("a recovery code survived DisableTOTP")
			}
		})
	}
}

// Re-authentication runs the same decision the login route runs, against a
// user id. A recovery code counts as the second factor — the user who needs to
// disable is often the one whose authenticator is gone (decisions §4).
func TestReauthenticate(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "reauth@example.com")
			secret := mustEnableTOTP(t, ctx, db, tenant, u.ID)
			codes, err := GenerateRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GenerateRecoveryCodes: %v", err)
			}
			p := &PasswordTOTPProvider{DB: db}

			// Password alone is not enough: a session an attacker holds must
			// not be able to strip MFA off the account.
			if err := p.Reauthenticate(ctx, tenant, u.ID, Credentials{Password: "s3cret!"}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("password without a second factor: err = %v, want ErrInvalidCredentials", err)
			}
			// Wrong password with a valid second factor is also not enough.
			code, _, err := totpCounter(secret, time.Now().UTC(), 0)
			if err != nil {
				t.Fatalf("totpCounter: %v", err)
			}
			if err := p.Reauthenticate(ctx, tenant, u.ID, Credentials{Password: "wrong", TOTPCode: code}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("wrong password: err = %v, want ErrInvalidCredentials", err)
			}
			// Password + TOTP.
			if err := p.Reauthenticate(ctx, tenant, u.ID, Credentials{Password: "s3cret!", TOTPCode: code}); err != nil {
				t.Fatalf("password + TOTP: %v", err)
			}
			// Password + recovery code.
			if err := p.Reauthenticate(ctx, tenant, u.ID, Credentials{Password: "s3cret!", RecoveryCode: codes[0]}); err != nil {
				t.Fatalf("password + recovery code: %v", err)
			}
		})
	}
}

// The otpauth:// URI has to carry everything an authenticator needs, and the
// issuer has to distinguish two tenants for the same person.
func TestTOTPURI(t *testing.T) {
	uri := TOTPURI("acme", "alice@example.com", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{
		"otpauth://totp/",
		"secret=JBSWY3DPEHPK3PXP",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
		"LastERP+%28acme%29", // issuer query parameter, form-encoded
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI %q does not contain %q", uri, want)
		}
	}
	// Two tenants, two distinguishable authenticator entries.
	if TOTPURI("acme", "alice@example.com", "S") == TOTPURI("globex", "alice@example.com", "S") {
		t.Error("the same user in two tenants produces an identical otpauth URI")
	}
}

func TestTOTPQRDataURI(t *testing.T) {
	png, err := TOTPQRDataURI(TOTPURI("acme", "alice@example.com", "JBSWY3DPEHPK3PXP"))
	if err != nil {
		t.Fatalf("TOTPQRDataURI: %v", err)
	}
	if !strings.HasPrefix(png, "data:image/png;base64,") {
		t.Fatalf("not a PNG data URI: %.40q", png)
	}
	if len(png) < 200 {
		t.Errorf("data URI is implausibly short (%d bytes)", len(png))
	}
}
