// SPDX-License-Identifier: AGPL-3.0-only

package invoicing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
	"github.com/iamdoubz/lasterp/modules/tax"
)

// Totals is an invoice's computed money summary (minor units, invoice currency).
type Totals struct {
	NetMinor   int64
	TaxMinor   int64
	GrossMinor int64
}

// buildInvoiceJournal is the declared invoice→GL posting template (INV-F5): the
// single, pure mapping from an invoice and its resolved tax to a balanced
// journal entry. No invoice reaches the ledger except through this function.
//
//	DR  ar_account          gross (Σ line gross incl. tax)
//	CR  revenue_account(s)   net   (per line, grouped by account)
//	CR  tax_account          tax   (Σ line tax)
//
// Σgross = Σnet + Σtax, so the entry balances (INV-F1). taxLines must align 1:1
// with inv.Lines (same order) — the per-line Net/Tax/Gross split from the tax
// engine. period is the ledger period the entry posts into.
func buildInvoiceJournal(inv Invoice, taxLines []tax.LineResult, period, commandID string) (ledger.PostCmd, Totals, error) {
	if len(taxLines) != len(inv.Lines) {
		return ledger.PostCmd{}, Totals{}, fmt.Errorf("invoicing: tax result has %d lines, invoice has %d", len(taxLines), len(inv.Lines))
	}
	gross, err := money.Zero(inv.Currency)
	if err != nil {
		return ledger.PostCmd{}, Totals{}, err
	}
	netTotal := gross
	taxTotal := gross

	// Credit each revenue account its net, grouped and summed deterministically.
	revByAccount := map[string]money.Money{}
	var revOrder []string
	for i, l := range inv.Lines {
		lr := taxLines[i]
		if lr.Net.Currency() != inv.Currency || lr.Tax.Currency() != inv.Currency {
			return ledger.PostCmd{}, Totals{}, money.ErrCurrencyMismatch
		}
		if _, seen := revByAccount[l.RevenueAccount]; !seen {
			z, _ := money.Zero(inv.Currency)
			revByAccount[l.RevenueAccount] = z
			revOrder = append(revOrder, l.RevenueAccount)
		}
		sum, err := revByAccount[l.RevenueAccount].Add(lr.Net)
		if err != nil {
			return ledger.PostCmd{}, Totals{}, err
		}
		revByAccount[l.RevenueAccount] = sum

		if gross, err = gross.Add(lr.Gross); err != nil {
			return ledger.PostCmd{}, Totals{}, err
		}
		if netTotal, err = netTotal.Add(lr.Net); err != nil {
			return ledger.PostCmd{}, Totals{}, err
		}
		if taxTotal, err = taxTotal.Add(lr.Tax); err != nil {
			return ledger.PostCmd{}, Totals{}, err
		}
	}

	lines := []ledger.Line{{AccountID: inv.ARAccount, Debit: gross.Amount()}}
	sort.Strings(revOrder)
	for _, acct := range revOrder {
		lines = append(lines, ledger.Line{AccountID: acct, Credit: revByAccount[acct].Amount()})
	}
	if taxTotal.Sign() != 0 {
		lines = append(lines, ledger.Line{AccountID: inv.TaxAccount, Credit: taxTotal.Amount()})
	}

	cmd := ledger.PostCmd{
		Period:    period,
		Currency:  inv.Currency,
		Memo:      "invoice " + inv.ID,
		Lines:     lines,
		CommandID: commandID,
	}
	return cmd, Totals{NetMinor: netTotal.Amount(), TaxMinor: taxTotal.Amount(), GrossMinor: gross.Amount()}, nil
}

// resolveInvoiceTax resolves each line's rate as of the invoice's issue date and
// computes the per-line net/tax/gross split (exclusive tax — the line amount is
// the net). It returns the LineResults aligned with inv.Lines.
func resolveInvoiceTax(ctx context.Context, db *storage.DB, tenant tenancy.ID, inv Invoice) ([]tax.LineResult, error) {
	issue, err := time.Parse("2006-01-02", inv.IssueDate)
	if err != nil {
		return nil, fmt.Errorf("invoicing: invalid issue_date %q: %w", inv.IssueDate, err)
	}
	docLines := make([]tax.DocLine, len(inv.Lines))
	for i, l := range inv.Lines {
		base, err := money.New(l.netMinor(), inv.Currency)
		if err != nil {
			return nil, err
		}
		docLines[i] = tax.DocLine{
			Jurisdiction: l.TaxJurisdiction, Category: l.TaxCategory,
			Base: base, Inclusive: false,
		}
	}
	res, err := tax.ResolveAndCalculate(ctx, db, tenant, docLines, issue)
	if err != nil {
		return nil, err
	}
	return res.Lines, nil
}

// PostInvoice posts a draft invoice: resolve tax, post the GL entry through the
// declared template (INV-F5) into period, assign a gapless invoice number at
// acceptance (INV-F6), and freeze the invoice as posted (INV-F2). The posting
// principal is authorized for both Invoice "post" (INV-T2/T4) and — because
// posting an invoice writes the ledger — JournalEntry "post" (checked inside
// ledger.Post; the two are the same pipeline, no privileged side door, INV-X3).
//
// GL is posted first (idempotent on the invoice's command id); a failure before
// number assignment consumes no number (INV-F6 gaplessness). A crash between the
// GL post and the freeze leaves an orphan GL entry (no gap); a retry is
// idempotent because GetInvoice still shows the draft and the same command id
// returns the same entry.
func PostInvoice(ctx context.Context, db *storage.DB, tenant tenancy.ID, id, period string) (Invoice, error) {
	inv, err := GetInvoice(ctx, db, tenant, id)
	if err != nil {
		return Invoice{}, err
	}
	if inv.Status != StatusDraft {
		return Invoice{}, fmt.Errorf("%w: %q", ErrNotDraft, id)
	}

	actor, err := authz.Authorize(ctx, db, ObjectInvoice, "post")
	if err != nil {
		return Invoice{}, err
	}

	taxLines, err := resolveInvoiceTax(ctx, db, tenant, inv)
	if err != nil {
		return Invoice{}, err
	}
	commandID := "invoice-post-" + inv.ID
	cmd, totals, err := buildInvoiceJournal(inv, taxLines, period, commandID)
	if err != nil {
		return Invoice{}, err
	}

	// 1. Post the GL entry (idempotent). This validates balance + open period at
	// the ledger's storage-enforced pipeline; a closed period fails here, before
	// any number is allocated.
	entry, err := ledger.Post(ctx, db, tenant, cmd)
	if err != nil {
		return Invoice{}, err
	}

	// 2. Allocate the number and freeze the invoice atomically. The draft→posted
	// UPDATE passes the immutability trigger (OLD.status is still 'draft').
	now := time.Now().UTC()
	var number string
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		seq, err := allocateNumber(ctx, tx, db, tenant, numberSeries)
		if err != nil {
			return err
		}
		number = formatInvoiceNumber(seq)
		table := metadata.TableName(ObjectInvoice)
		res, err := tx.ExecContext(ctx, db.Rebind(`UPDATE `+table+`
			SET status = ?, number = ?, net_minor = ?, tax_minor = ?, gross_minor = ?,
			    gl_entry_id = ?, posted_at = ?, updated_at = ?
			WHERE tenant_id = ? AND id = ? AND status = ?`),
			StatusPosted, number, totals.NetMinor, totals.TaxMinor, totals.GrossMinor,
			entry.ID, now.Format(time.RFC3339), now, string(tenant), inv.ID, StatusDraft)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			// The draft changed out from under us (already posted / deleted).
			return fmt.Errorf("%w: %q", ErrNotDraft, inv.ID)
		}
		changes, _ := json.Marshal(map[string]any{"status": StatusPosted, "number": number, "gl_entry_id": entry.ID})
		return recordPostAudit(ctx, tx, db, tenant, inv.ID, changes, string(actor.UserID))
	})
	if err != nil {
		return Invoice{}, err
	}
	return GetInvoice(ctx, db, tenant, id)
}

// recordPostAudit writes the INV-T4 attribution row for the post transition in
// the same tx as the freeze. (metadata.recordAudit is unexported; this is the
// same append-only audit_log insert.)
func recordPostAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, invoiceID string, changes []byte, actorID string) error {
	_, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		idgen.New(), string(tenant), ObjectInvoice, invoiceID, "post", string(changes), actorID, time.Now().UTC())
	return err
}
