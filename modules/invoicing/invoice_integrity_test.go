//go:build integrity

// Invoicing integrity suite (joins the Integrity Gauntlet, docs/19). Runs on
// Postgres AND SQLite. Proves WP-1.4's invariants at the posting pipeline and
// storage layers, and the AC lifecycle end to end:
//
//	AC     draft → post → GL entries correct → PDF renders;
//	INV-F5 an invoice reaches the ledger only through the declared template
//	       (DR AR / CR revenue / CR tax), producing a balanced entry (INV-F1);
//	INV-F6 invoice numbers are gapless, assigned only at post acceptance —
//	       drafts carry none, a failed post consumes none;
//	INV-F2 a posted invoice row is immutable (raw UPDATE/DELETE rejected by the
//	       storage trigger; UpdateDraft refuses a non-draft);
//	INV-T2 posting requires both Invoice "post" and JournalEntry "post" — no
//	       privileged side door into the ledger;
//	INV-T1 no tenant can read another tenant's invoice.
package invoicing

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// AC + INV-F5/F1: a draft posts to a balanced GL entry matching the template,
// and the invoice renders to a valid PDF carrying its assigned number.
func TestInvoiceLifecycle(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)

			// draft
			draft, err := CreateDraft(f.ctx, db, f.tenant, f.draft(1, 10000)) // net 100.00, 20% VAT
			if err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}
			if draft.Status != StatusDraft || draft.Number != "" {
				t.Fatalf("fresh draft = %+v, want status=draft number=''", draft)
			}

			// post
			posted, err := PostInvoice(f.ctx, db, f.tenant, draft.ID, f.period)
			if err != nil {
				t.Fatalf("PostInvoice: %v", err)
			}
			if posted.Status != StatusPosted || posted.Number != "INV-000001" {
				t.Fatalf("posted = %+v, want status=posted number=INV-000001", posted)
			}
			if posted.NetMinor != 10000 || posted.TaxMinor != 2000 || posted.GrossMinor != 12000 {
				t.Fatalf("posted totals = net %d tax %d gross %d, want 10000/2000/12000",
					posted.NetMinor, posted.TaxMinor, posted.GrossMinor)
			}

			// GL entries correct: load the posted journal entry and check the legs.
			entry, err := ledger.LoadEntry(f.ctx, db, f.tenant, posted.GLEntryID)
			if err != nil {
				t.Fatalf("LoadEntry: %v", err)
			}
			byAccount := map[string]ledger.Line{}
			for _, l := range entry.Lines {
				byAccount[l.AccountID] = l
			}
			if byAccount[f.arAccount].Debit != 12000 {
				t.Errorf("AR debit = %d, want 12000", byAccount[f.arAccount].Debit)
			}
			if byAccount[f.revAccount].Credit != 10000 {
				t.Errorf("revenue credit = %d, want 10000", byAccount[f.revAccount].Credit)
			}
			if byAccount[f.taxAccount].Credit != 2000 {
				t.Errorf("tax credit = %d, want 2000", byAccount[f.taxAccount].Credit)
			}

			// Trial-balance projection reconciles (net zero, per INV-F1/E5).
			if err := ledger.RebuildBalances(f.ctx, db, f.tenant); err != nil {
				t.Fatalf("RebuildBalances: %v", err)
			}
			tb, err := ledger.ReadTrialBalance(f.ctx, db, f.tenant)
			if err != nil {
				t.Fatalf("ReadTrialBalance: %v", err)
			}
			var sum int64
			for _, byCcy := range tb {
				sum += byCcy["EUR"]
			}
			if sum != 0 {
				t.Fatalf("trial balance does not net to zero: %d", sum)
			}
			if tb[f.arAccount]["EUR"] != 12000 {
				t.Errorf("AR net = %d, want 12000", tb[f.arAccount]["EUR"])
			}

			// PDF renders.
			pdf, err := RenderInvoicePDF(posted)
			if err != nil {
				t.Fatalf("RenderInvoicePDF: %v", err)
			}
			if !bytes.HasPrefix(pdf, []byte("%PDF")) || !bytes.Contains(pdf, []byte("INV-000001")) {
				t.Fatal("rendered PDF is not a valid invoice document")
			}

			// Re-posting a posted invoice is refused (idempotent lifecycle guard).
			if _, err := PostInvoice(f.ctx, db, f.tenant, draft.ID, f.period); !errors.Is(err, ErrNotDraft) {
				t.Fatalf("re-post: err = %v, want ErrNotDraft", err)
			}
		})
	}
}

// INV-F6: numbers are contiguous and assigned only at acceptance; a draft has
// none, and a post that fails before acceptance consumes none.
func TestGaplessNumbers(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)

			post := func() Invoice {
				d, err := CreateDraft(f.ctx, db, f.tenant, f.draft(1, 10000))
				if err != nil {
					t.Fatalf("CreateDraft: %v", err)
				}
				if d.Number != "" {
					t.Fatalf("draft carries a number %q before acceptance (INV-F6)", d.Number)
				}
				p, err := PostInvoice(f.ctx, db, f.tenant, d.ID, f.period)
				if err != nil {
					t.Fatalf("PostInvoice: %v", err)
				}
				return p
			}

			if got := post().Number; got != "INV-000001" {
				t.Fatalf("first number = %q, want INV-000001", got)
			}
			if got := post().Number; got != "INV-000002" {
				t.Fatalf("second number = %q, want INV-000002", got)
			}

			// A post that fails (closed period) must consume no number: close the
			// period, attempt a post (fails at the ledger), reopen, post again — the
			// next number must be contiguous (INV-000003), not skipped to 4.
			periodID := periodIDByCode(t, f.ctx, db, f.tenant, f.period)
			if err := ledger.ClosePeriod(f.ctx, db, f.tenant, periodID); err != nil {
				t.Fatalf("ClosePeriod: %v", err)
			}
			failDraft, err := CreateDraft(f.ctx, db, f.tenant, f.draft(1, 10000))
			if err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}
			if _, err := PostInvoice(f.ctx, db, f.tenant, failDraft.ID, f.period); !errors.Is(err, ledger.ErrClosedPeriod) {
				t.Fatalf("post into closed period: err = %v, want ErrClosedPeriod", err)
			}
			if err := ledger.ReopenPeriod(f.ctx, db, f.tenant, periodID); err != nil {
				t.Fatalf("ReopenPeriod: %v", err)
			}
			p, err := PostInvoice(f.ctx, db, f.tenant, failDraft.ID, f.period)
			if err != nil {
				t.Fatalf("PostInvoice after reopen: %v", err)
			}
			if p.Number != "INV-000003" {
				t.Fatalf("number after a failed post = %q, want INV-000003 (no gap consumed)", p.Number)
			}
		})
	}
}

// INV-F2: a posted invoice row is immutable — a raw UPDATE or DELETE on it is
// rejected by the storage trigger, and UpdateDraft refuses a non-draft.
func TestPostedInvoiceImmutable(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			d, err := CreateDraft(f.ctx, db, f.tenant, f.draft(1, 10000))
			if err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}
			posted, err := PostInvoice(f.ctx, db, f.tenant, d.ID, f.period)
			if err != nil {
				t.Fatalf("PostInvoice: %v", err)
			}

			table := metadata.TableName(ObjectInvoice)
			rawUpdate := tenancy.WithTenant(f.ctx, db, f.tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, db.Rebind(`UPDATE `+table+` SET number = 'TAMPER' WHERE tenant_id = ? AND id = ?`),
					string(f.tenant), posted.ID)
				return e
			})
			if rawUpdate == nil {
				t.Fatal("raw UPDATE on a posted invoice must be rejected (INV-F2)")
			}
			rawDelete := tenancy.WithTenant(f.ctx, db, f.tenant, func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, db.Rebind(`DELETE FROM `+table+` WHERE tenant_id = ? AND id = ?`),
					string(f.tenant), posted.ID)
				return e
			})
			if rawDelete == nil {
				t.Fatal("raw DELETE on a posted invoice must be rejected (INV-F2)")
			}

			// Module guard: UpdateDraft refuses the posted invoice.
			if _, err := UpdateDraft(f.ctx, db, f.tenant, posted.ID, f.draft(2, 10000)); !errors.Is(err, ErrNotDraft) {
				t.Fatalf("UpdateDraft on posted: err = %v, want ErrNotDraft", err)
			}

			// And it still reads back exactly as posted (unchanged).
			got, err := GetInvoice(f.ctx, db, f.tenant, posted.ID)
			if err != nil {
				t.Fatalf("GetInvoice: %v", err)
			}
			if got.Number != posted.Number || got.GrossMinor != posted.GrossMinor {
				t.Fatalf("posted invoice changed: %+v", got)
			}
		})
	}
}

// INV-T2: posting requires Invoice "post" AND JournalEntry "post" — there is no
// privileged path from an invoice into the ledger.
func TestPostRequiresPermissions(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			d, err := CreateDraft(f.ctx, db, f.tenant, f.draft(1, 10000))
			if err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}

			// (a) No Invoice "post": denied before the ledger is touched.
			noPost := invoicingActor(t, db, f.tenant, map[string][]string{ObjectInvoice: {"read"}})
			if _, err := PostInvoice(noPost, db, f.tenant, d.ID, f.period); !errors.Is(err, authz.ErrPermissionDenied) {
				t.Fatalf("post without Invoice post: err = %v, want ErrPermissionDenied", err)
			}

			// (b) Invoice "post" but no JournalEntry "post": denied at the ledger.
			noLedger := invoicingActor(t, db, f.tenant, map[string][]string{ObjectInvoice: {"read", "post"}})
			if _, err := PostInvoice(noLedger, db, f.tenant, d.ID, f.period); !errors.Is(err, authz.ErrPermissionDenied) {
				t.Fatalf("post without JournalEntry post: err = %v, want ErrPermissionDenied", err)
			}

			// The draft is still a draft — neither failed attempt posted or numbered it.
			got, err := GetInvoice(f.ctx, db, f.tenant, d.ID)
			if err != nil {
				t.Fatalf("GetInvoice: %v", err)
			}
			if got.Status != StatusDraft || got.Number != "" {
				t.Fatalf("failed posts left side effects: %+v", got)
			}
		})
	}
}

// INV-T1: another tenant cannot read this tenant's invoice.
func TestCrossTenantInvoiceIsolation(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			f := setup(t, db)
			d, err := CreateDraft(f.ctx, db, f.tenant, f.draft(1, 10000))
			if err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}

			// A second tenant with full invoicing grants of its own.
			other := mustCreateTenant(t, db)
			otherCtx := invoicingActor(t, db, other, map[string][]string{ObjectInvoice: {"read"}})
			if _, err := GetInvoice(otherCtx, db, other, d.ID); !errors.Is(err, ErrInvoiceNotFound) {
				t.Fatalf("cross-tenant read: err = %v, want ErrInvoiceNotFound (zero rows)", err)
			}
		})
	}
}

// periodIDByCode fetches a period's record id for the close/reopen calls.
func periodIDByCode(t *testing.T, ctx context.Context, db *storage.DB, tenant tenancy.ID, code string) string {
	t.Helper()
	var id string
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, db.Rebind(
			`SELECT id FROM `+metadata.TableName(ledger.ObjectPeriod)+` WHERE tenant_id = ? AND code = ?`),
			string(tenant), code).Scan(&id)
	})
	if err != nil {
		t.Fatalf("lookup period id: %v", err)
	}
	return id
}
