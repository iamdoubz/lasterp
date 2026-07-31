//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"testing"
	"time"
)

// WP-1.6 AC: "recorded receipt reduces AR aging". PR-A proved the settlement
// position moves; this proves the report that consumes it does.

func agingTotal(rep Report) int64 {
	for _, row := range rep.Totals {
		if row.Key == "total" {
			return row.AmountMinor
		}
	}
	return 0
}

func bucketTotal(rep Report, bucket string) int64 {
	for _, row := range rep.Totals {
		if row.Key == bucket {
			return row.AmountMinor
		}
	}
	return 0
}

// A receipt reduces the aging total by exactly what it settled, and a fully
// settled invoice leaves the report entirely.
func TestReceiptReducesARAging(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv := b.postInvoice(t, db, "2026-01-05", 1, 100000) // gross 120000

			before, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			if got := agingTotal(before); got != inv.GrossMinor {
				t.Fatalf("aging total before payment = %d, want %d", got, inv.GrossMinor)
			}

			b.receive(t, db, inv.ID, 45000)
			partial, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			if got, want := agingTotal(partial), inv.GrossMinor-45000; got != want {
				t.Errorf("aging total after part payment = %d, want %d", got, want)
			}

			b.receive(t, db, inv.ID, inv.GrossMinor-45000)
			settled, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			if got := agingTotal(settled); got != 0 {
				t.Errorf("aging total after full payment = %d, want 0", got)
			}
			if len(settled.Rows) != 0 {
				t.Errorf("a fully settled invoice is still on the aging report: %+v", settled.Rows)
			}
		})
	}
}

// Invoices land in the bucket matching their age, measured from issue date.
func TestARAgingBucketsByAge(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			// asOf is 2026-01-31.
			b.postInvoice(t, db, "2026-01-31", 1, 10000) // 0 days  → current
			b.postInvoice(t, db, "2026-01-20", 1, 20000) // 11 days → 1–30
			b.postInvoice(t, db, "2025-12-15", 1, 30000) // 47 days → 31–60
			b.postInvoice(t, db, "2025-10-01", 1, 40000) // 122     → 90+

			rep, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			// Each invoice is net + 20% VAT.
			want := map[string]int64{
				"current": 12000, "1_30": 24000, "31_60": 36000, "61_90": 0, "90_plus": 48000,
			}
			for bucket, expected := range want {
				if got := bucketTotal(rep, bucket); got != expected {
					t.Errorf("bucket %s = %d, want %d", bucket, got, expected)
				}
			}
			if got, want := agingTotal(rep), int64(12000+24000+36000+48000); got != want {
				t.Errorf("aging total = %d, want %d", got, want)
			}
		})
	}
}

// The report states its basis, so nobody reads issue-date aging as due-date
// aging — a materially different conclusion (WP-1.6-decisions.md §8).
func TestARAgingDeclaresItsBasis(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			b.postInvoice(t, db, "2026-01-05", 1, 10000)

			rep, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			if rep.Basis == "" {
				t.Error("AR aging does not declare what it ages from")
			}
		})
	}
}

// Aging rows carry the invoices behind them, which is what drill-down needs.
func TestARAgingRowsCarrySourceInvoices(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv1 := b.postInvoice(t, db, "2026-01-05", 1, 10000)
			inv2 := b.postInvoice(t, db, "2026-01-06", 1, 20000)

			rep, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			found := map[string]bool{}
			for _, row := range rep.Rows {
				if len(row.SourceIDs) == 0 {
					t.Errorf("aging row %q carries no invoice ids — nothing to drill into", row.Key)
				}
				for _, id := range row.SourceIDs {
					found[id] = true
				}
			}
			for _, id := range []string{inv1.ID, inv2.ID} {
				if !found[id] {
					t.Errorf("invoice %s is outstanding but absent from the aging drill-down", id)
				}
			}
		})
	}
}

// Draft invoices carry no receivable — nothing has reached the ledger — so they
// must not appear in AR.
func TestARAgingExcludesDrafts(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			b.postInvoice(t, db, "2026-01-05", 1, 10000)
			// A draft that never posts.
			if _, err := draftOnly(t, db, b); err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}

			rep, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			if got, want := agingTotal(rep), int64(12000); got != want {
				t.Errorf("aging total = %d, want %d — a draft invoice leaked into AR", got, want)
			}
		})
	}
}

// The AR-outstanding metric and the aging report must agree: one definition, one
// number (docs/21 §1).
func TestARMetricAgreesWithAgingReport(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv1 := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			b.postInvoice(t, db, "2026-01-12", 2, 15000)
			b.receive(t, db, inv1.ID, 33333)

			rep, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			metric, err := Evaluate(b.ctx, db, b.tenant, "ar_outstanding", Scope{Currency: "EUR", AsOf: asOf})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if metric.Value != agingTotal(rep) {
				t.Errorf("ar_outstanding metric = %d but the aging report totals %d — "+
					"the CFO's number and the CEO's number disagree", metric.Value, agingTotal(rep))
			}
		})
	}
}

// AR outstanding must also tie to the ledger's AR control account: the aging
// report and the balance sheet are two views of the same receivable.
func TestARAgingTiesToLedgerARBalance(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			b := seedBooks(t, db)
			inv1 := b.postInvoice(t, db, "2026-01-05", 1, 100000)
			inv2 := b.postInvoice(t, db, "2026-01-09", 3, 7000)
			b.receive(t, db, inv1.ID, 40000)
			b.receive(t, db, inv2.ID, 1000)

			rep, err := ARAging(b.ctx, db, b.tenant, "EUR", asOf)
			if err != nil {
				t.Fatalf("ARAging: %v", err)
			}
			d, err := Load(b.ctx, db, b.tenant, "EUR")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			arBalance := d.netFor(b.arAccount)
			if agingTotal(rep) != arBalance {
				t.Errorf("AR aging totals %d but the ledger's AR control account holds %d — "+
					"the subledger does not tie to the GL", agingTotal(rep), arBalance)
			}
		})
	}
}

func TestAgeInDays(t *testing.T) {
	at := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		issue string
		want  int
	}{
		{"2026-01-31", 0},
		{"2026-01-30", 1},
		{"2026-01-01", 30},
		{"2025-12-31", 31},
		{"2026-02-15", 0}, // future-dated: least-alarming bucket, not negative
		{"not-a-date", 0}, // malformed: render the row rather than fail the report
	}
	for _, tc := range tests {
		if got := ageInDays(tc.issue, at); got != tc.want {
			t.Errorf("ageInDays(%q) = %d, want %d", tc.issue, got, tc.want)
		}
	}
}
