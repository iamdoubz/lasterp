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
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/ledger"
)

// buildReceiptJournal is the declared receipt→GL posting template (INV-F5): the
// single, pure mapping from a receipt and the invoices it settles to a balanced
// journal entry. No receipt reaches the ledger except through this function.
//
//	DR  bank_account        Σ applied
//	CR  ar_account(s)       per applied invoice's own AR account, grouped
//
// The AR credit uses each *invoice's* ar_account rather than one configured
// control account: the invoice debited that specific account when it posted
// (buildInvoiceJournal), so crediting anything else would leave both accounts
// permanently wrong while the entry still balanced. arByInvoice maps invoice id
// → the ar_account it was posted against.
//
// Σcredits = Σdebits by construction, so the entry balances (INV-F1).
func buildReceiptJournal(r Receipt, arByInvoice map[string]string, period, commandID string) (ledger.PostCmd, int64, error) {
	total, err := money.Zero(r.Currency)
	if err != nil {
		return ledger.PostCmd{}, 0, err
	}

	arByAccount := map[string]money.Money{}
	var arOrder []string
	for _, a := range r.Applications {
		account, ok := arByInvoice[a.InvoiceID]
		if !ok || account == "" {
			return ledger.PostCmd{}, 0, fmt.Errorf("invoicing: no AR account for invoice %q", a.InvoiceID)
		}
		amount, err := money.New(a.AmountMinor, r.Currency)
		if err != nil {
			return ledger.PostCmd{}, 0, err
		}
		if _, seen := arByAccount[account]; !seen {
			z, _ := money.Zero(r.Currency)
			arByAccount[account] = z
			arOrder = append(arOrder, account)
		}
		sum, err := arByAccount[account].Add(amount)
		if err != nil {
			return ledger.PostCmd{}, 0, err
		}
		arByAccount[account] = sum

		if total, err = total.Add(amount); err != nil {
			return ledger.PostCmd{}, 0, err
		}
	}
	if total.Sign() <= 0 {
		return ledger.PostCmd{}, 0, fmt.Errorf("invoicing: receipt total must be positive")
	}

	lines := []ledger.Line{{AccountID: r.BankAccount, Debit: total.Amount()}}
	sort.Strings(arOrder) // deterministic line order for a reproducible entry
	for _, account := range arOrder {
		lines = append(lines, ledger.Line{AccountID: account, Credit: arByAccount[account].Amount()})
	}

	return ledger.PostCmd{
		Period:    period,
		Currency:  r.Currency,
		Memo:      "receipt " + r.ID,
		Lines:     lines,
		CommandID: commandID,
	}, total.Amount(), nil
}

// PostReceipt posts a draft receipt: verify every applied invoice is postable
// and the applications fit (INV-F8), post the GL entry through the declared
// template (INV-F5), assign a gapless receipt number at acceptance (INV-F6), and
// freeze the receipt as posted (INV-F2).
//
// The invoices themselves are never touched — a posted invoice is immutable, and
// settlement state is derived from receipts rather than stamped onto the
// document (WP-1.6-decisions.md §1).
//
// Ordering mirrors PostInvoice: GL first (idempotent on the receipt's command
// id), then number allocation, so a failure before acceptance consumes no
// number.
func PostReceipt(ctx context.Context, db *storage.DB, tenant tenancy.ID, id, period string) (Receipt, error) {
	r, err := GetReceipt(ctx, db, tenant, id)
	if err != nil {
		return Receipt{}, err
	}
	if r.Status != StatusDraft {
		return Receipt{}, fmt.Errorf("%w: %q", ErrNotDraft, id)
	}

	actor, err := authz.Authorize(ctx, db, ObjectReceipt, "post")
	if err != nil {
		return Receipt{}, err
	}

	// Load every applied invoice: they must exist, be posted, share the
	// receipt's currency, and have room for the amount applied.
	arByInvoice := make(map[string]string, len(r.Applications))
	for _, a := range r.Applications {
		inv, err := GetInvoice(ctx, db, tenant, a.InvoiceID)
		if err != nil {
			return Receipt{}, err
		}
		if inv.Status != StatusPosted {
			return Receipt{}, fmt.Errorf("invoicing: invoice %q is not posted; only posted invoices can be settled", a.InvoiceID)
		}
		if inv.Currency != r.Currency {
			return Receipt{}, fmt.Errorf("%w: receipt %s, invoice %s", money.ErrCurrencyMismatch, r.Currency, inv.Currency)
		}
		arByInvoice[a.InvoiceID] = inv.ARAccount
	}

	commandID := "receipt-post-" + r.ID
	cmd, total, err := buildReceiptJournal(r, arByInvoice, period, commandID)
	if err != nil {
		return Receipt{}, err
	}

	// 1. Post the GL entry (idempotent). Balance and open-period are enforced by
	// the ledger's storage pipeline; a closed period fails here, before any
	// number is allocated.
	entry, err := ledger.Post(ctx, db, tenant, cmd)
	if err != nil {
		return Receipt{}, err
	}

	// 2. Check INV-F8, allocate the number, and freeze the receipt atomically.
	now := time.Now().UTC()
	var number string
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkNotOverApplied(ctx, tx, db, tenant, r); err != nil {
			return err
		}
		seq, err := allocateNumber(ctx, tx, db, tenant, receiptSeries)
		if err != nil {
			return err
		}
		number = formatReceiptNumber(seq)
		table := metadata.TableName(ObjectReceipt)
		res, err := tx.ExecContext(ctx, db.Rebind(`UPDATE `+table+`
			SET status = ?, number = ?, total_minor = ?, gl_entry_id = ?, posted_at = ?, updated_at = ?
			WHERE tenant_id = ? AND id = ? AND status = ?`),
			StatusPosted, number, total, entry.ID, now.Format(time.RFC3339), now,
			string(tenant), r.ID, StatusDraft)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("%w: %q", ErrNotDraft, r.ID)
		}
		changes, _ := json.Marshal(map[string]any{"status": StatusPosted, "number": number, "gl_entry_id": entry.ID})
		return recordReceiptAudit(ctx, tx, db, tenant, r.ID, changes, string(actor.UserID))
	})
	if err != nil {
		return Receipt{}, err
	}
	return GetReceipt(ctx, db, tenant, id)
}

// checkNotOverApplied enforces INV-F8 inside the acceptance transaction: for
// every invoice this receipt touches, Σ(already-posted applications) + this
// receipt's application must not exceed the invoice's gross.
//
// It must run here, not at draft time. Two receipts drafted independently can
// each look affordable against the same invoice and only collide at post time;
// re-reading inside the serialized acceptance tx (Postgres: the row locks the
// UPDATE takes; SQLite: the write lock) is what makes the check binding rather
// than advisory.
func checkNotOverApplied(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, r Receipt) error {
	table := metadata.TableName(ObjectInvoice)
	for _, a := range r.Applications {
		var gross int64
		row := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT gross_minor FROM `+table+` WHERE tenant_id = ? AND id = ?`),
			string(tenant), a.InvoiceID)
		if err := row.Scan(&gross); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: %q", ErrInvoiceNotFound, a.InvoiceID)
			}
			return err
		}
		applied, err := appliedToInvoiceTx(ctx, tx, db, tenant, a.InvoiceID, r.ID)
		if err != nil {
			return err
		}
		if applied+a.AmountMinor > gross {
			return fmt.Errorf("%w: invoice %s gross %d, already applied %d, this receipt %d",
				ErrOverApplied, a.InvoiceID, gross, applied, a.AmountMinor)
		}
	}
	return nil
}

// recordReceiptAudit writes the INV-T4 attribution row for the post transition
// in the same tx as the freeze.
func recordReceiptAudit(ctx context.Context, tx *sql.Tx, db *storage.DB, tenant tenancy.ID, receiptID string, changes []byte, actorID string) error {
	_, err := tx.ExecContext(ctx, db.Rebind(`
		INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		idgenNew(), string(tenant), ObjectReceipt, receiptID, "post", string(changes), actorID, time.Now().UTC())
	return err
}
