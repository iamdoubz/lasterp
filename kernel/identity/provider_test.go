package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPasswordTOTPProviderPasswordOnly(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			u, err := CreateUser(ctx, db, tenant, "erin@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}

			p := &PasswordTOTPProvider{DB: db}
			id, err := p.Authenticate(ctx, tenant, Credentials{Email: "erin@example.com", Password: "s3cret!"})
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if id != u.ID {
				t.Fatalf("Authenticate returned %s, want %s", id, u.ID)
			}

			if _, err := p.Authenticate(ctx, tenant, Credentials{Email: "erin@example.com", Password: "wrong"}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("wrong password: err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

func TestPasswordTOTPProviderRequiresCode(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			u, err := CreateUser(ctx, db, tenant, "frank@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			secret, err := GenerateTOTPSecret()
			if err != nil {
				t.Fatalf("GenerateTOTPSecret: %v", err)
			}
			if err := EnableTOTP(ctx, db, tenant, u.ID, secret); err != nil {
				t.Fatalf("EnableTOTP: %v", err)
			}

			p := &PasswordTOTPProvider{DB: db}
			if _, err := p.Authenticate(ctx, tenant, Credentials{Email: "frank@example.com", Password: "s3cret!", TOTPCode: "000000"}); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("wrong TOTP code: err = %v, want ErrInvalidCredentials", err)
			}

			code, _, err := totpCounter(secret, time.Now().UTC(), 0)
			if err != nil {
				t.Fatalf("totpCounter: %v", err)
			}
			if _, err := p.Authenticate(ctx, tenant, Credentials{Email: "frank@example.com", Password: "s3cret!", TOTPCode: code}); err != nil {
				t.Fatalf("correct TOTP code: %v", err)
			}
		})
	}
}
