package identity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecoveryCodeFormat(t *testing.T) {
	code, err := newRecoveryCode()
	if err != nil {
		t.Fatalf("newRecoveryCode: %v", err)
	}
	groups := strings.Split(code, "-")
	if len(groups) != 6 {
		t.Fatalf("code %q has %d groups, want 6", code, len(groups))
	}
	for _, g := range groups {
		if len(g) != recoveryGroupSize {
			t.Errorf("group %q is %d characters, want %d", g, len(g), recoveryGroupSize)
		}
		// No 0/1/8/9 to confuse with O/I/B/g — that is the point of base32.
		for _, r := range g {
			if (r < 'A' || r > 'Z') && (r < '2' || r > '7') {
				t.Errorf("code %q contains %q, outside the base32 alphabet", code, r)
			}
		}
	}
	// Distinctness is the whole security property; a generator returning a
	// constant would pass every other assertion here.
	seen := map[string]bool{code: true}
	for range 100 {
		next, err := newRecoveryCode()
		if err != nil {
			t.Fatalf("newRecoveryCode: %v", err)
		}
		if seen[next] {
			t.Fatalf("newRecoveryCode repeated %q within 101 draws", next)
		}
		seen[next] = true
	}
}

// A user may type a code in any of the forms they might reasonably type it in.
func TestRecoveryCodeNormalization(t *testing.T) {
	const canonical = "K7QX2M4ARSTU"
	for _, in := range []string{
		"K7QX-2M4A-RSTU",
		"k7qx-2m4a-rstu",
		" K7QX 2M4A RSTU ",
		"K7QX2M4ARSTU",
	} {
		if got := normalizeRecoveryCode(in); got != canonical {
			t.Errorf("normalizeRecoveryCode(%q) = %q, want %q", in, got, canonical)
		}
	}
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "single@example.com")
			codes, err := GenerateRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GenerateRecoveryCodes: %v", err)
			}
			if len(codes) != RecoveryCodeCount {
				t.Fatalf("got %d codes, want %d", len(codes), RecoveryCodeCount)
			}

			ok, err := ConsumeRecoveryCode(ctx, db, tenant, u.ID, codes[0])
			if err != nil {
				t.Fatalf("ConsumeRecoveryCode: %v", err)
			}
			if !ok {
				t.Fatal("a freshly minted recovery code was refused")
			}
			// The AC: a recovery code cannot be reused.
			ok, err = ConsumeRecoveryCode(ctx, db, tenant, u.ID, codes[0])
			if err != nil {
				t.Fatalf("ConsumeRecoveryCode (replay): %v", err)
			}
			if ok {
				t.Error("a recovery code was accepted twice")
			}

			remaining, err := CountUnusedRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("CountUnusedRecoveryCodes: %v", err)
			}
			if want := RecoveryCodeCount - 1; remaining != want {
				t.Errorf("remaining = %d, want %d", remaining, want)
			}
		})
	}
}

// Single-use is enforced by the database (UPDATE ... WHERE used_at IS NULL,
// RowsAffected == 1), not by a check-then-act in Go — so two concurrent logins
// presenting the same code cannot both win.
func TestRecoveryCodeConcurrentConsumeIsSingleUse(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "race@example.com")
			codes, err := GenerateRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GenerateRecoveryCodes: %v", err)
			}

			const racers = 8
			var wg sync.WaitGroup
			results := make([]bool, racers)
			errs := make([]error, racers)
			start := make(chan struct{})
			for i := range racers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results[i], errs[i] = ConsumeRecoveryCode(ctx, db, tenant, u.ID, codes[0])
				}()
			}
			close(start)
			wg.Wait()

			won := 0
			for i := range racers {
				if errs[i] != nil {
					t.Fatalf("racer %d: %v", i, errs[i])
				}
				if results[i] {
					won++
				}
			}
			if won != 1 {
				t.Errorf("%d of %d concurrent consumers accepted the same code; want exactly 1", won, racers)
			}
		})
	}
}

// INV-T1: a recovery code minted for one user must not authenticate another,
// and must not cross a tenant boundary.
func TestRecoveryCodeDoesNotCrossUserOrTenant(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)
			alice := mustUser(t, ctx, db, tenantA, "alice@example.com")
			bob := mustUser(t, ctx, db, tenantA, "bob@example.com")
			carol := mustUser(t, ctx, db, tenantB, "carol@example.com")

			codes, err := GenerateRecoveryCodes(ctx, db, tenantA, alice.ID)
			if err != nil {
				t.Fatalf("GenerateRecoveryCodes: %v", err)
			}

			ok, err := ConsumeRecoveryCode(ctx, db, tenantA, bob.ID, codes[0])
			if err != nil {
				t.Fatalf("ConsumeRecoveryCode (other user): %v", err)
			}
			if ok {
				t.Error("alice's recovery code was accepted for bob")
			}
			ok, err = ConsumeRecoveryCode(ctx, db, tenantB, carol.ID, codes[0])
			if err != nil {
				t.Fatalf("ConsumeRecoveryCode (other tenant): %v", err)
			}
			if ok {
				t.Error("a recovery code minted in tenant A was accepted in tenant B (INV-T1)")
			}
			// And it is still usable by its owner: the rejections above must be
			// rejections, not consumptions.
			ok, err = ConsumeRecoveryCode(ctx, db, tenantA, alice.ID, codes[0])
			if err != nil {
				t.Fatalf("ConsumeRecoveryCode (owner): %v", err)
			}
			if !ok {
				t.Error("a cross-user attempt consumed the owner's code")
			}
		})
	}
}

// Regenerating replaces rather than adds: a user who regenerates because they
// think a code leaked is right.
func TestGenerateRecoveryCodesReplacesTheOldSet(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "regen@example.com")

			first, err := GenerateRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GenerateRecoveryCodes: %v", err)
			}
			if _, err := GenerateRecoveryCodes(ctx, db, tenant, u.ID); err != nil {
				t.Fatalf("GenerateRecoveryCodes (second): %v", err)
			}

			ok, err := ConsumeRecoveryCode(ctx, db, tenant, u.ID, first[0])
			if err != nil {
				t.Fatalf("ConsumeRecoveryCode: %v", err)
			}
			if ok {
				t.Error("a code from the superseded set still works")
			}
			remaining, err := CountUnusedRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("CountUnusedRecoveryCodes: %v", err)
			}
			if remaining != RecoveryCodeCount {
				t.Errorf("remaining = %d, want %d", remaining, RecoveryCodeCount)
			}
		})
	}
}

// A recovery code authenticates at login, in its own field.
func TestLoginWithARecoveryCode(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "rc-login@example.com")
			mustEnableTOTP(t, ctx, db, tenant, u.ID)
			codes, err := GenerateRecoveryCodes(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GenerateRecoveryCodes: %v", err)
			}

			p := &PasswordTOTPProvider{DB: db}
			id, err := p.Authenticate(ctx, tenant, Credentials{
				Email: "rc-login@example.com", Password: "s3cret!", RecoveryCode: codes[0],
			})
			if err != nil {
				t.Fatalf("login with a recovery code: %v", err)
			}
			if id != u.ID {
				t.Fatalf("Authenticate returned %s, want %s", id, u.ID)
			}
			// Reusing it is a plain 401, indistinguishable from any other
			// failure.
			if _, err := p.Authenticate(ctx, tenant, Credentials{
				Email: "rc-login@example.com", Password: "s3cret!", RecoveryCode: codes[0],
			}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("replayed recovery code: err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

// Second-factor lockout (decisions §6): without it, a 100 req/s budget breaks a
// ±1-step TOTP window in under an hour for anyone who already has the password.
func TestSecondFactorLockout(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "lockout@example.com")
			secret := mustEnableTOTP(t, ctx, db, tenant, u.ID)
			p := &PasswordTOTPProvider{DB: db}

			for i := range totpMaxFailedAttempts {
				if _, err := p.Authenticate(ctx, tenant, Credentials{
					Email: "lockout@example.com", Password: "s3cret!", TOTPCode: "000000",
				}); !errors.Is(err, ErrInvalidCredentials) {
					t.Fatalf("attempt %d: err = %v, want ErrInvalidCredentials", i, err)
				}
			}

			got, err := GetUserByID(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GetUserByID: %v", err)
			}
			if !got.TOTPLockedAt(time.Now().UTC()) {
				t.Fatalf("account not locked after %d failures", totpMaxFailedAttempts)
			}

			// A *correct* code is now refused, and with the same
			// undifferentiated error — no "you are locked out" oracle.
			code, _, err := totpCounter(secret, time.Now().UTC(), 0)
			if err != nil {
				t.Fatalf("totpCounter: %v", err)
			}
			if _, err := p.Authenticate(ctx, tenant, Credentials{
				Email: "lockout@example.com", Password: "s3cret!", TOTPCode: code,
			}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("correct code while locked: err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

// The lockout has to survive concurrency, or it protects nothing: an attacker
// who already has the password runs attempts in parallel, and the gateway's
// budget (100 req/s, burst 200) is what makes that worth doing.
//
// Two failures are possible if the counter is read into Go and written back as
// an absolute value, and both favour the attacker: N in-flight attempts all
// read k and all write k+1, so the threshold is never reached; and any request
// holding a pre-lock snapshot writes totp_locked_until back to NULL, unlocking
// an account that had just locked. This test fails on either.
func TestSecondFactorLockoutUnderConcurrency(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "concurrent-lockout@example.com")
			secret := mustEnableTOTP(t, ctx, db, tenant, u.ID)
			p := &PasswordTOTPProvider{DB: db}

			// Comfortably past the threshold, all racing.
			const attempts = totpMaxFailedAttempts * 2
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range attempts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, _ = p.Authenticate(ctx, tenant, Credentials{
						Email: "concurrent-lockout@example.com", Password: "s3cret!", TOTPCode: "000000",
					})
				}()
			}
			close(start)
			wg.Wait()

			got, err := GetUserByID(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GetUserByID: %v", err)
			}
			if got.TOTPFailedAttempts < totpMaxFailedAttempts {
				t.Errorf("counter = %d after %d concurrent failures; a lost update is dropping increments",
					got.TOTPFailedAttempts, attempts)
			}
			if !got.TOTPLockedAt(time.Now().UTC()) {
				t.Error("account is not locked after concurrent failures past the threshold — " +
					"a stale snapshot cleared the lock")
			}
			// And the lock actually refuses a correct code.
			code, _, err := totpCounter(secret, time.Now().UTC(), 0)
			if err != nil {
				t.Fatalf("totpCounter: %v", err)
			}
			if _, err := p.Authenticate(ctx, tenant, Credentials{
				Email: "concurrent-lockout@example.com", Password: "s3cret!", TOTPCode: code,
			}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("correct code while locked: err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

// The threshold counts *consecutive* failures: a success in between resets it,
// or a user who fumbles a code once a week would eventually lock themselves out.
func TestSuccessResetsTheFailureCounter(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			u := mustUser(t, ctx, db, tenant, "reset@example.com")
			secret := mustEnableTOTP(t, ctx, db, tenant, u.ID)
			p := &PasswordTOTPProvider{DB: db}

			for range totpMaxFailedAttempts - 1 {
				if _, err := p.Authenticate(ctx, tenant, Credentials{
					Email: "reset@example.com", Password: "s3cret!", TOTPCode: "000000",
				}); !errors.Is(err, ErrInvalidCredentials) {
					t.Fatalf("err = %v, want ErrInvalidCredentials", err)
				}
			}
			code, _, err := totpCounter(secret, time.Now().UTC(), 0)
			if err != nil {
				t.Fatalf("totpCounter: %v", err)
			}
			if _, err := p.Authenticate(ctx, tenant, Credentials{
				Email: "reset@example.com", Password: "s3cret!", TOTPCode: code,
			}); err != nil {
				t.Fatalf("correct code before the threshold: %v", err)
			}

			got, err := GetUserByID(ctx, db, tenant, u.ID)
			if err != nil {
				t.Fatalf("GetUserByID: %v", err)
			}
			if got.TOTPFailedAttempts != 0 || got.TOTPLockedUntil != nil {
				t.Errorf("counter not reset after success: attempts=%d locked=%v",
					got.TOTPFailedAttempts, got.TOTPLockedUntil)
			}
		})
	}
}
