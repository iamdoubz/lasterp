//go:build integrity

// FX store suite — storage-touching, so it runs on Postgres AND SQLite
// (adapter conformance). Proves effective-dated as-of lookup, identity/inverse,
// tenant-override precedence, and INV-T1: one tenant's override rates are
// invisible to another tenant (RLS backstop on Postgres, tenant_id predicate on
// SQLite).
package money

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func mustSave(t *testing.T, db *storage.DB, tenant tenancy.ID, base, quote, rate string, asOf time.Time) {
	t.Helper()
	if err := SaveRate(context.Background(), db, tenant, Rate{Base: base, Quote: quote, Rate: rate, AsOf: asOf, Provider: "test"}); err != nil {
		t.Fatalf("SaveRate %s/%s: %v", base, quote, err)
	}
}

func TestRateAsOfEffectiveDated(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)

			mustSave(t, db, tenant, "EUR", "USD", "1.05", date(2026, 1, 1))
			mustSave(t, db, tenant, "EUR", "USD", "1.10", date(2026, 6, 1))

			// Before any rate → not found.
			if _, err := RateAsOf(ctx, db, tenant, "EUR", "USD", date(2025, 12, 31)); !errors.Is(err, ErrRateNotFound) {
				t.Fatalf("pre-history: err = %v, want ErrRateNotFound", err)
			}
			// Between the two → the earlier one.
			assertRate(t, ctx, db, tenant, "EUR", "USD", date(2026, 3, 1), "1.05")
			// On/after the later one → the later one.
			assertRate(t, ctx, db, tenant, "EUR", "USD", date(2026, 6, 1), "1.10")
			assertRate(t, ctx, db, tenant, "EUR", "USD", date(2026, 9, 1), "1.10")
		})
	}
}

func TestRateAsOfIdentityAndInverse(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			mustSave(t, db, tenant, "EUR", "USD", "1.25", date(2026, 1, 1)) // 4/5 inverse

			// Identity.
			assertRate(t, ctx, db, tenant, "USD", "USD", date(2026, 1, 1), "1")
			// Direct.
			assertRate(t, ctx, db, tenant, "EUR", "USD", date(2026, 1, 1), "5/4")
			// Inverse (only EUR/USD on file, ask USD/EUR → 1/1.25 = 4/5).
			assertRate(t, ctx, db, tenant, "USD", "EUR", date(2026, 1, 1), "4/5")
		})
	}
}

// Tenant override beats the shared global rate; INV-T1: another tenant never
// sees the override.
func TestRateAsOfTenantOverrideAndIsolation(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)

			// Shared global rate, then a tenant-A-only override.
			mustSave(t, db, GlobalTenant, "EUR", "USD", "1.10", date(2026, 1, 1))
			mustSave(t, db, tenantA, "EUR", "USD", "1.20", date(2026, 1, 1))

			// A sees its override; B sees only the global rate.
			assertRate(t, ctx, db, tenantA, "EUR", "USD", date(2026, 2, 1), "6/5")   // 1.20
			assertRate(t, ctx, db, tenantB, "EUR", "USD", date(2026, 2, 1), "11/10") // 1.10
		})
	}
}

func assertRate(t *testing.T, ctx context.Context, db *storage.DB, tenant tenancy.ID, base, quote string, on time.Time, wantRat string) {
	t.Helper()
	got, err := RateAsOf(ctx, db, tenant, base, quote, on)
	if err != nil {
		t.Fatalf("RateAsOf %s/%s: %v", base, quote, err)
	}
	want, _ := new(big.Rat).SetString(wantRat)
	if got.Cmp(want) != 0 {
		t.Fatalf("RateAsOf %s/%s = %s, want %s", base, quote, got.RatString(), want.RatString())
	}
}
