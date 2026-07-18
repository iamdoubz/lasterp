//go:build integrity

// Tax reference-data suite — storage-touching, so it runs on Postgres AND
// SQLite (adapter conformance). Proves effective-dated as-of lookup, tenant-
// override precedence, seed-pack load + end-to-end ResolveAndCalculate, and
// INV-T1: one tenant's override rates/jurisdictions are invisible to another
// tenant (RLS backstop on Postgres, tenant_id predicate on SQLite).
package tax

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func mustSaveRate(t *testing.T, db *storage.DB, tenant tenancy.ID, jur, cat, rate, rounding string, asOf time.Time) {
	t.Helper()
	if err := SaveRate(context.Background(), db, tenant, Rate{
		Jurisdiction: jur, Category: cat, Rate: rate, Rounding: rounding, AsOf: asOf, Provider: "test",
	}); err != nil {
		t.Fatalf("SaveRate %s/%s: %v", jur, cat, err)
	}
}

func assertRate(t *testing.T, db *storage.DB, tenant tenancy.ID, jur, cat string, on time.Time, wantRate string) {
	t.Helper()
	rr, err := RateAsOf(context.Background(), db, tenant, jur, cat, on)
	if err != nil {
		t.Fatalf("RateAsOf %s/%s @ %s: %v", jur, cat, on.Format(dateLayout), err)
	}
	if rr.Rate.RatString() != wantRate {
		t.Fatalf("RateAsOf %s/%s @ %s = %s, want %s", jur, cat, on.Format(dateLayout), rr.Rate.RatString(), wantRate)
	}
}

// Effective-dating: three rows for one jurisdiction+category (a rate cut and
// restoration) resolve to the row in effect on the document date.
func TestRateAsOfEffectiveDated(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenant := mustCreateTenant(t, db)
			mustSaveRate(t, db, tenant, "EU-DE", "standard", "0.19", RoundHalfEven, date(2007, 1, 1))
			mustSaveRate(t, db, tenant, "EU-DE", "standard", "0.16", RoundHalfEven, date(2020, 7, 1))
			mustSaveRate(t, db, tenant, "EU-DE", "standard", "0.19", RoundHalfEven, date(2021, 1, 1))

			// Before any rate → not found.
			if _, err := RateAsOf(context.Background(), db, tenant, "EU-DE", "standard", date(2006, 12, 31)); !errors.Is(err, ErrRateNotFound) {
				t.Fatalf("pre-history: err = %v, want ErrRateNotFound", err)
			}
			assertRate(t, db, tenant, "EU-DE", "standard", date(2010, 3, 1), "19/100") // original
			assertRate(t, db, tenant, "EU-DE", "standard", date(2020, 9, 1), "4/25")   // 0.16 temp cut
			assertRate(t, db, tenant, "EU-DE", "standard", date(2022, 1, 1), "19/100") // restored
		})
	}
}

// Tenant override beats the shared global rate; INV-T1: another tenant never
// sees the override, only the global row.
func TestRateAsOfTenantOverrideAndIsolation(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)

			mustSaveRate(t, db, GlobalTenant, "US-CA", "sales", "0.0725", RoundHalfUp, date(2017, 1, 1))
			mustSaveRate(t, db, tenantA, "US-CA", "sales", "0.08", RoundHalfUp, date(2017, 1, 1))

			assertRate(t, db, tenantA, "US-CA", "sales", date(2026, 1, 1), "2/25")   // 0.08 override
			assertRate(t, db, tenantB, "US-CA", "sales", date(2026, 1, 1), "29/400") // 0.0725 global
		})
	}
}

// INV-T1 for jurisdictions: tenant A's override jurisdiction is invisible to
// tenant B, and both see the shared global one.
func TestJurisdictionIsolation(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenantA := mustCreateTenant(t, db)
			tenantB := mustCreateTenant(t, db)

			if err := SaveJurisdiction(ctx, db, GlobalTenant, Jurisdiction{Code: "EU-DE", Name: "Germany", Country: "EU", Level: LevelCountry}); err != nil {
				t.Fatalf("save global jurisdiction: %v", err)
			}
			if err := SaveJurisdiction(ctx, db, tenantA, Jurisdiction{Code: "US-CA-SF", Name: "San Francisco", Country: "US", Level: LevelProvince}); err != nil {
				t.Fatalf("save tenant-A jurisdiction: %v", err)
			}

			a, err := ListJurisdictions(ctx, db, tenantA)
			if err != nil {
				t.Fatalf("list A: %v", err)
			}
			b, err := ListJurisdictions(ctx, db, tenantB)
			if err != nil {
				t.Fatalf("list B: %v", err)
			}
			if !hasCode(a, "EU-DE") || !hasCode(a, "US-CA-SF") {
				t.Fatalf("tenant A should see global + own, got %v", codes(a))
			}
			if !hasCode(b, "EU-DE") {
				t.Fatalf("tenant B should see the global jurisdiction, got %v", codes(b))
			}
			if hasCode(b, "US-CA-SF") {
				t.Fatalf("INV-T1 breach: tenant B sees tenant A's private jurisdiction %v", codes(b))
			}
		})
	}
}

// The embedded seed packs load under GlobalTenant and resolve end-to-end
// through ResolveAndCalculate.
func TestSeedPacksLoadAndCalculate(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			if err := LoadSeedPacks(ctx, db); err != nil {
				t.Fatalf("LoadSeedPacks: %v", err)
			}
			tenant := mustCreateTenant(t, db)

			// German temporary-cut effective-dating comes from the EU seed pack.
			assertRate(t, db, tenant, "EU-DE", "standard", date(2020, 9, 1), "4/25")   // 0.16
			assertRate(t, db, tenant, "EU-DE", "standard", date(2022, 1, 1), "19/100") // restored

			base, err := money.New(10000, "EUR")
			if err != nil {
				t.Fatalf("money.New: %v", err)
			}
			res, err := ResolveAndCalculate(ctx, db, tenant,
				[]DocLine{{Jurisdiction: "EU-DE", Category: "standard", Base: base}}, date(2026, 1, 1))
			if err != nil {
				t.Fatalf("ResolveAndCalculate: %v", err)
			}
			if res.Lines[0].Tax.Amount() != 1900 {
				t.Fatalf("seed EU-DE standard on EUR100: tax = %d, want 1900", res.Lines[0].Tax.Amount())
			}

			// A missing rate surfaces as ErrRateNotFound, not a silent zero.
			if _, err := ResolveAndCalculate(ctx, db, tenant,
				[]DocLine{{Jurisdiction: "XX-NOWHERE", Category: "sales", Base: base}}, date(2026, 1, 1)); !errors.Is(err, ErrRateNotFound) {
				t.Fatalf("unknown jurisdiction: err = %v, want ErrRateNotFound", err)
			}
		})
	}
}

func hasCode(js []Jurisdiction, code string) bool {
	for _, j := range js {
		if j.Code == code {
			return true
		}
	}
	return false
}

func codes(js []Jurisdiction) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.Code
	}
	return out
}
