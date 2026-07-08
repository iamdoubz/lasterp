package identity

import (
	"context"
	"errors"
	"testing"
)

func TestSessionIssueAndValidateRoundTrip(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			u, err := CreateUser(ctx, db, tenant, "bob@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}

			issued, err := IssueSession(ctx, db, tenant, u.ID, "device-1")
			if err != nil {
				t.Fatalf("IssueSession: %v", err)
			}

			s, err := ValidateSession(ctx, db, issued.Token)
			if err != nil {
				t.Fatalf("ValidateSession: %v", err)
			}
			if s.UserID != u.ID || s.TenantID != tenant {
				t.Fatalf("session = %+v, want user %s tenant %s", s, u.ID, tenant)
			}

			if _, err := ValidateSession(ctx, db, "not-a-real-token"); !errors.Is(err, ErrSessionInvalid) {
				t.Fatalf("bogus token: err = %v, want ErrSessionInvalid", err)
			}
		})
	}
}

func TestSessionRevoked(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			u, err := CreateUser(ctx, db, tenant, "carol@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			issued, err := IssueSession(ctx, db, tenant, u.ID, "device-1")
			if err != nil {
				t.Fatalf("IssueSession: %v", err)
			}

			if err := RevokeSession(ctx, db, issued.ID); err != nil {
				t.Fatalf("RevokeSession: %v", err)
			}

			if _, err := ValidateSession(ctx, db, issued.Token); !errors.Is(err, ErrSessionInvalid) {
				t.Fatalf("revoked session: err = %v, want ErrSessionInvalid", err)
			}
		})
	}
}

func TestRefreshSessionDeviceBinding(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			hash, _ := HashPassword("s3cret!")
			u, err := CreateUser(ctx, db, tenant, "dave@example.com", hash)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			issued, err := IssueSession(ctx, db, tenant, u.ID, "device-1")
			if err != nil {
				t.Fatalf("IssueSession: %v", err)
			}

			if _, err := RefreshSession(ctx, db, issued.RefreshToken, "device-2"); !errors.Is(err, ErrDeviceMismatch) {
				t.Fatalf("refresh from wrong device: err = %v, want ErrDeviceMismatch", err)
			}

			refreshed, err := RefreshSession(ctx, db, issued.RefreshToken, "device-1")
			if err != nil {
				t.Fatalf("RefreshSession from correct device: %v", err)
			}
			if _, err := ValidateSession(ctx, db, refreshed.Token); err != nil {
				t.Fatalf("ValidateSession on refreshed token: %v", err)
			}
		})
	}
}
