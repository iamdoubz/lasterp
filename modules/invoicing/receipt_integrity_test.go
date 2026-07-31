//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// WP-1.6 PR-A: AR receipts. Receipts settle posted invoices, and the settlement
// position is DERIVED — a posted invoice is never touched again (INV-F2), so
// "paid" is computed from receipts rather than stamped onto the document
// (WP-1.6-decisions.md §1). Invariants: INV-F8 (new — no over-application),
// INV-F1 (receipt entry balances), INV-F2 (posted receipt immutable), INV-F5
// (GL only via the declared template), INV-F6 (gapless receipt numbers),
// INV-T1/T2/T4.

// postedInvoice drafts and posts an invoice, returning it.
func postedInvoice(t *testing.T, db *storage.DB, f fixture, qty, unitMinor int64) Invoice {
	t.Helper()
	draft, err := CreateDraft(f.ctx, db, f.tenant, f.draft(qty, unitMinor))
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	inv, err := PostInvoice(f.ctx, db, f.tenant, draft.ID, f.period)
	if err != nil {
		t.Fatalf("PostInvoice: %v", err)
	}
	return inv
}

// receiptFor drafts a receipt applying amounts to invoices.
func (f fixture) receiptFor(t *testing.T, db *storage.DB, apps ...Application) Receipt {
	t.Helper()
	r, err := CreateReceiptDraft(f.ctx, db, f.tenant, ReceiptInput{
		ContactID: f.contactID, Currency: "EUR", ReceivedDate: "2026-01-20",
		BankAccount: f.bankAccount, Applications: apps,
	})
	if err != nil {
		t.Fatalf("CreateReceiptDraft: %v", err)
	}
	return r
}

func settlementOf(t *testing.T, db *storage.DB, f fixture, invoiceID string) Settlement {
	t.Helper()
	inv, err := GetInvoice(f.ctx, db, f.tenant, invoiceID)
	if err != nil {
		t.Fatalf("GetInvoice: %v", err)
	}
	s, err := SettlementFor(f.ctx, db, f.tenant, inv)
	if err != nil {
		t.Fatalf("SettlementFor: %v", err)
	}
	return s
}

// AC: a recorded receipt flips the invoice's settlement status and reduces what
// it leaves outstanding — open → partial → paid, without the invoice row ever
// being written to after posting.
func TestReceiptLifecycleDrivesSettlement(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000) // 1000.00 net + 20% = 1200.00

			if got := settlementOf(t, db, f, inv.ID); got.Status != SettlementOpen || got.OutstandingMinor != inv.GrossMinor {
				t.Fatalf("before any receipt: %+v, want open with %d outstanding", got, inv.GrossMinor)
			}

			// Part payment.
			partial := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: 50000})
			if _, err := PostReceipt(f.ctx, db, f.tenant, partial.ID, f.period); err != nil {
				t.Fatalf("PostReceipt (partial): %v", err)
			}
			got := settlementOf(t, db, f, inv.ID)
			if got.Status != SettlementPartial {
				t.Errorf("after part payment: status %q, want %q", got.Status, SettlementPartial)
			}
			if got.SettledMinor != 50000 || got.OutstandingMinor != inv.GrossMinor-50000 {
				t.Errorf("after part payment: settled %d outstanding %d, want 50000 / %d",
					got.SettledMinor, got.OutstandingMinor, inv.GrossMinor-50000)
			}

			// Balance.
			rest := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: inv.GrossMinor - 50000})
			if _, err := PostReceipt(f.ctx, db, f.tenant, rest.ID, f.period); err != nil {
				t.Fatalf("PostReceipt (balance): %v", err)
			}
			got = settlementOf(t, db, f, inv.ID)
			if got.Status != SettlementPaid {
				t.Errorf("after full payment: status %q, want %q", got.Status, SettlementPaid)
			}
			if got.OutstandingMinor != 0 {
				t.Errorf("after full payment: outstanding %d, want 0", got.OutstandingMinor)
			}

			// The invoice document itself was never modified (INV-F2): its own
			// status is still `posted`, and settlement lives outside it.
			after, err := GetInvoice(f.ctx, db, f.tenant, inv.ID)
			if err != nil {
				t.Fatalf("GetInvoice: %v", err)
			}
			if after.Status != StatusPosted {
				t.Errorf("invoice status = %q, want it to stay %q — settlement must not edit the document",
					after.Status, StatusPosted)
			}
			if after.GrossMinor != inv.GrossMinor || after.Number != inv.Number {
				t.Error("posted invoice content changed while settling it")
			}
		})
	}
}

// INV-F1: the receipt's GL entry balances, and it moves the money the invoice
// left behind — AR down, bank up, by exactly the amount applied.
func TestReceiptPostsBalancedEntryToGL(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000)

			before := trialBalance(t, db, f)

			r := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: inv.GrossMinor})
			posted, err := PostReceipt(f.ctx, db, f.tenant, r.ID, f.period)
			if err != nil {
				t.Fatalf("PostReceipt: %v", err)
			}
			if posted.GLEntryID == "" {
				t.Fatal("posted receipt carries no GL entry id")
			}

			entry, err := ledger.LoadEntry(f.ctx, db, f.tenant, posted.GLEntryID)
			if err != nil {
				t.Fatalf("LoadEntry: %v", err)
			}
			var debits, credits int64
			for _, l := range entry.Lines {
				debits += l.Debit
				credits += l.Credit
			}
			if debits != credits {
				t.Errorf("receipt entry does not balance: %d vs %d (INV-F1)", debits, credits)
			}

			after := trialBalance(t, db, f)
			// AR is a debit-normal account: settling it moves the balance down.
			arDelta := after[f.arAccount]["EUR"] - before[f.arAccount]["EUR"]
			if arDelta != -inv.GrossMinor {
				t.Errorf("AR moved by %d, want %d", arDelta, -inv.GrossMinor)
			}
			bankDelta := after[f.bankAccount]["EUR"] - before[f.bankAccount]["EUR"]
			if bankDelta != inv.GrossMinor {
				t.Errorf("bank moved by %d, want %d", bankDelta, inv.GrossMinor)
			}
			// Fully settled: the receivable is gone.
			if after[f.arAccount]["EUR"] != 0 {
				t.Errorf("AR balance after full settlement = %d, want 0", after[f.arAccount]["EUR"])
			}
		})
	}
}

// INV-F8: a receipt may never settle more than the invoice's gross, counting
// every receipt already posted against it.
func TestReceiptCannotOverApplyInvoice(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000) // gross 120000

			// One receipt for more than the whole invoice.
			tooBig := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: inv.GrossMinor + 1})
			_, err := PostReceipt(f.ctx, db, f.tenant, tooBig.ID, f.period)
			if !errors.Is(err, ErrOverApplied) {
				t.Fatalf("single over-payment error = %v, want ErrOverApplied (INV-F8)", err)
			}

			// Pay it off in full, then try to apply one more cent.
			paid := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: inv.GrossMinor})
			if _, err := PostReceipt(f.ctx, db, f.tenant, paid.ID, f.period); err != nil {
				t.Fatalf("PostReceipt: %v", err)
			}
			extra := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: 1})
			if err := mustFailPost(t, db, f, extra.ID); !errors.Is(err, ErrOverApplied) {
				t.Fatalf("cumulative over-payment error = %v, want ErrOverApplied (INV-F8)", err)
			}

			// The refused receipts left no trace: still drafts, no GL effect.
			for _, id := range []string{tooBig.ID, extra.ID} {
				r, err := GetReceipt(f.ctx, db, f.tenant, id)
				if err != nil {
					t.Fatalf("GetReceipt: %v", err)
				}
				if r.Status != StatusDraft || r.Number != "" {
					t.Errorf("refused receipt %s is %q with number %q, want an untouched draft", id, r.Status, r.Number)
				}
			}
			// And the invoice is exactly paid, not over-settled.
			if got := settlementOf(t, db, f, inv.ID); got.OutstandingMinor != 0 || got.Status != SettlementPaid {
				t.Errorf("settlement = %+v, want exactly paid", got)
			}
		})
	}
}

// INV-F8 as a property: whatever sequence of receipts is thrown at an invoice,
// the amount settled never exceeds its gross and never goes negative.
func TestReceiptApplicationNeverExceedsGrossProperty(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000)
			gross := inv.GrossMinor

			rng := rand.New(rand.NewSource(7))
			var accepted int64
			for i := 0; i < 40; i++ {
				// Amounts deliberately straddle the remaining balance, so roughly
				// half the attempts should be refused.
				amount := rng.Int63n(gross/3) + 1
				r := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: amount})
				_, err := PostReceipt(f.ctx, db, f.tenant, r.ID, f.period)
				switch {
				case err == nil:
					accepted += amount
				case errors.Is(err, ErrOverApplied):
					// expected once the invoice fills up
				default:
					t.Fatalf("unexpected PostReceipt error: %v", err)
				}

				s := settlementOf(t, db, f, inv.ID)
				if s.SettledMinor != accepted {
					t.Fatalf("iteration %d: settled %d, expected %d", i, s.SettledMinor, accepted)
				}
				if s.SettledMinor > gross {
					t.Fatalf("iteration %d: settled %d exceeds gross %d (INV-F8)", i, s.SettledMinor, gross)
				}
				if s.OutstandingMinor < 0 {
					t.Fatalf("iteration %d: negative outstanding %d", i, s.OutstandingMinor)
				}
			}
			if accepted == 0 {
				t.Fatal("no receipt was ever accepted — the property proved nothing")
			}
		})
	}
}

// INV-F6: receipt numbers are gapless and allocated only at acceptance, from a
// series of their own.
func TestReceiptNumbersAreGaplessAndSeparate(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000)

			// A refused post must consume no number.
			doomed := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: inv.GrossMinor * 2})
			if err := mustFailPost(t, db, f, doomed.ID); !errors.Is(err, ErrOverApplied) {
				t.Fatalf("expected the over-application to be refused, got %v", err)
			}

			var numbers []string
			for i := 0; i < 3; i++ {
				r := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: 100})
				posted, err := PostReceipt(f.ctx, db, f.tenant, r.ID, f.period)
				if err != nil {
					t.Fatalf("PostReceipt %d: %v", i, err)
				}
				numbers = append(numbers, posted.Number)
			}
			want := []string{"RCT-000001", "RCT-000002", "RCT-000003"}
			for i := range want {
				if numbers[i] != want[i] {
					t.Errorf("receipt %d number = %q, want %q (gapless from 1 — a failed post must not consume one)",
						i, numbers[i], want[i])
				}
			}
			// The invoice series is untouched by receipt numbering.
			if inv.Number != "INV-000001" {
				t.Errorf("invoice number = %q, want INV-000001 — the series must be independent", inv.Number)
			}
		})
	}
}

// INV-F2: a posted receipt is frozen at the storage layer, not merely by module
// convention. Raw SQL bypassing every Go guard must still be refused.
func TestPostedReceiptImmutable(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000)
			r := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: 1000})
			posted, err := PostReceipt(f.ctx, db, f.tenant, r.ID, f.period)
			if err != nil {
				t.Fatalf("PostReceipt: %v", err)
			}

			table := metadata.TableName(ObjectReceipt)
			err = tenancy.WithTenant(f.ctx, db, f.tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, db.Rebind(
					`UPDATE `+table+` SET total_minor = ? WHERE tenant_id = ? AND id = ?`),
					999999, string(f.tenant), posted.ID)
				return err
			})
			if err == nil {
				t.Error("raw UPDATE on a posted receipt succeeded (INV-F2)")
			}

			err = tenancy.WithTenant(f.ctx, db, f.tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, db.Rebind(
					`DELETE FROM `+table+` WHERE tenant_id = ? AND id = ?`),
					string(f.tenant), posted.ID)
				return err
			})
			if err == nil {
				t.Error("raw DELETE of a posted receipt succeeded (INV-F2)")
			}

			// The module-level guard refuses too.
			if _, err := UpdateReceiptDraft(f.ctx, db, f.tenant, posted.ID, ReceiptInput{
				ContactID: f.contactID, Currency: "EUR", ReceivedDate: "2026-01-21",
				BankAccount:  f.bankAccount,
				Applications: []Application{{InvoiceID: inv.ID, AmountMinor: 1}},
			}); !errors.Is(err, ErrNotDraft) {
				t.Errorf("UpdateReceiptDraft on a posted receipt = %v, want ErrNotDraft", err)
			}
		})
	}
}

// INV-T2: posting a receipt writes the ledger, so it needs both its own post
// permission and the ledger's — no privileged side door.
func TestPostReceiptRequiresPermissions(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000)
			r := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: 1000})

			// A principal with everything except Receipt:post.
			grants := fullGrants()
			grants[ObjectReceipt] = []string{"create", "read", "update"}
			noPost := invoicingActor(t, db, f.tenant, grants)
			if _, err := PostReceipt(noPost, db, f.tenant, r.ID, f.period); err == nil {
				t.Error("posted a receipt without Receipt:post (INV-T2)")
			}

			// And one without the ledger permission the GL write needs.
			grants = fullGrants()
			grants[ledger.ObjectJournalEntry] = []string{"read"}
			noLedger := invoicingActor(t, db, f.tenant, grants)
			if _, err := PostReceipt(noLedger, db, f.tenant, r.ID, f.period); err == nil {
				t.Error("posted a receipt without JournalEntry:post (INV-T2)")
			}
		})
	}
}

// INV-T1: receipts and the settlement derived from them are tenant-scoped.
func TestCrossTenantReceiptIsolation(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000)
			r := f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: inv.GrossMinor})
			if _, err := PostReceipt(f.ctx, db, f.tenant, r.ID, f.period); err != nil {
				t.Fatalf("PostReceipt: %v", err)
			}

			// A second tenant must see neither the receipt nor its effect.
			other := setup(t, db)
			if _, err := GetReceipt(other.ctx, db, other.tenant, r.ID); err == nil {
				t.Error("another tenant read the receipt (INV-T1)")
			}
			applied, err := AppliedToInvoice(other.ctx, db, other.tenant, inv.ID)
			if err != nil {
				t.Fatalf("AppliedToInvoice: %v", err)
			}
			if applied != 0 {
				t.Errorf("another tenant saw %d applied against our invoice (INV-T1)", applied)
			}
		})
	}
}

// A receipt may only settle a posted invoice: applying to a draft would credit
// AR the invoice never debited.
func TestReceiptRejectsUnpostedInvoice(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			draft, err := CreateDraft(f.ctx, db, f.tenant, f.draft(1, 100000))
			if err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}
			r := f.receiptFor(t, db, Application{InvoiceID: draft.ID, AmountMinor: 100})
			if _, err := PostReceipt(f.ctx, db, f.tenant, r.ID, f.period); err == nil {
				t.Fatal("settled an unposted invoice")
			}
		})
	}
}

// Draft receipts have no GL effect, so they must not count toward settlement —
// otherwise an invoice reads as paid while the ledger still carries it.
func TestDraftReceiptDoesNotSettle(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			inv := postedInvoice(t, db, f, 1, 100000)
			f.receiptFor(t, db, Application{InvoiceID: inv.ID, AmountMinor: inv.GrossMinor})

			got := settlementOf(t, db, f, inv.ID)
			if got.Status != SettlementOpen || got.SettledMinor != 0 {
				t.Errorf("a draft receipt settled the invoice: %+v", got)
			}
		})
	}
}

// trialBalance refreshes and reads the ledger projection.
//
// The rebuild is not incidental: ledger_balances is only ever written by an
// explicit RebuildBalances call, and *no product code path makes one* — posting
// does not update it. So the projection is empty in a running system today.
// That is WP-1.6 PR-B's problem (reports are what make the projection matter,
// and INV-E5 is their acceptance criterion); here the rebuild is what makes the
// read meaningful, and this comment is the marker that it should not have to be.
func trialBalance(t *testing.T, db *storage.DB, f fixture) ledger.TrialBalance {
	t.Helper()
	if err := ledger.RebuildBalances(f.ctx, db, f.tenant); err != nil {
		t.Fatalf("RebuildBalances: %v", err)
	}
	tb, err := ledger.ReadTrialBalance(f.ctx, db, f.tenant)
	if err != nil {
		t.Fatalf("ReadTrialBalance: %v", err)
	}
	return tb
}

// mustFailPost posts a receipt expecting failure, returning the error.
func mustFailPost(t *testing.T, db *storage.DB, f fixture, receiptID string) error {
	t.Helper()
	_, err := PostReceipt(f.ctx, db, f.tenant, receiptID, f.period)
	if err == nil {
		t.Fatalf("PostReceipt(%s) unexpectedly succeeded", receiptID)
	}
	return err
}
