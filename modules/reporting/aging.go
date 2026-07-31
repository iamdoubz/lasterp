// SPDX-License-Identifier: AGPL-3.0-only

package reporting

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/invoicing"
)

// Aging buckets. Standard AR banding; the boundaries are days of age.
var agingBuckets = []struct {
	key     string
	label   string
	maxDays int // inclusive upper bound; -1 means "and older"
}{
	{"current", "Current", 0},
	{"1_30", "1–30 days", 30},
	{"31_60", "31–60 days", 60},
	{"61_90", "61–90 days", 90},
	{"90_plus", "90+ days", -1},
}

// agingBasis records what the buckets are measured from, so nobody reads this
// as due-date aging. Invoice has no due date — payment terms were deferred by
// WP-1.4 and land with the Phase-4 banking work that owns them
// (WP-1.6-decisions.md §8).
const agingBasis = "Aged by issue date. Payment terms and due-date aging land with Phase 4 banking."

// OpenItem is one invoice's unsettled balance, as AR aging sees it.
type OpenItem struct {
	InvoiceID        string
	Number           string
	ContactID        string
	Currency         string
	IssueDate        string
	OutstandingMinor int64
	AgeDays          int
}

// ARAging reports each customer's outstanding receivables banded by age.
//
// Outstanding is the invoice's gross less every posted receipt applied to it —
// the settlement position PR-A derives rather than stores (INV-F2). A fully
// settled invoice contributes nothing, which is what "a recorded receipt reduces
// AR aging" means concretely.
func ARAging(ctx context.Context, db *storage.DB, tenant tenancy.ID, currency string, asOf time.Time) (Report, error) {
	if err := authorizeRead(ctx, db, invoicing.ObjectInvoice); err != nil {
		return Report{}, err
	}
	items, err := OpenItems(ctx, db, tenant, currency, asOf)
	if err != nil {
		return Report{}, err
	}

	rep := Report{
		Name:     "ar_aging",
		Title:    "AR aging",
		Currency: currency,
		Basis:    agingBasis,
		Columns:  []string{"Customer"},
	}
	for _, b := range agingBuckets {
		rep.Columns = append(rep.Columns, b.label)
	}
	rep.Columns = append(rep.Columns, "Total")

	// One row per customer per bucket, keyed so the row order is stable and the
	// export writer needs no special cases.
	type cell struct {
		amount int64
		ids    []string
	}
	byContact := map[string]map[string]*cell{}
	bucketTotals := map[string]int64{}
	for _, it := range items {
		bucket := bucketFor(it.AgeDays)
		if byContact[it.ContactID] == nil {
			byContact[it.ContactID] = map[string]*cell{}
		}
		c := byContact[it.ContactID][bucket]
		if c == nil {
			c = &cell{}
			byContact[it.ContactID][bucket] = c
		}
		c.amount += it.OutstandingMinor
		c.ids = append(c.ids, it.InvoiceID)
		bucketTotals[bucket] += it.OutstandingMinor
	}

	contacts := make([]string, 0, len(byContact))
	for id := range byContact {
		contacts = append(contacts, id)
	}
	sort.Strings(contacts)

	for _, contactID := range contacts {
		for _, b := range agingBuckets {
			c := byContact[contactID][b.key]
			if c == nil || c.amount == 0 {
				continue
			}
			rep.Rows = append(rep.Rows, Row{
				Label:       contactID,
				Key:         contactID + ":" + b.key,
				Currency:    currency,
				AmountMinor: c.amount,
				SourceIDs:   c.ids, // drill-down: the invoices behind the number
			})
		}
	}

	var grand int64
	for _, b := range agingBuckets {
		grand += bucketTotals[b.key]
		rep.Totals = append(rep.Totals, Row{
			Label: b.label, Key: b.key, Currency: currency, AmountMinor: bucketTotals[b.key],
		})
	}
	rep.Totals = append(rep.Totals, Row{
		Label: "Total outstanding", Key: "total", Currency: currency, AmountMinor: grand,
	})
	return rep, nil
}

// bucketFor bands an age in days.
func bucketFor(ageDays int) string {
	for _, b := range agingBuckets {
		if b.maxDays >= 0 && ageDays <= b.maxDays {
			return b.key
		}
	}
	return agingBuckets[len(agingBuckets)-1].key
}

// OpenItems lists every posted invoice with an outstanding balance, oldest
// first. It is exported because it is the drill-down target for an aging cell
// and the input to the AR-outstanding metric — one definition, one number
// (docs/21 §1).
func OpenItems(ctx context.Context, db *storage.DB, tenant tenancy.ID, currency string, asOf time.Time) ([]OpenItem, error) {
	invoices, err := invoicing.ListPosted(ctx, db, tenant)
	if err != nil {
		return nil, fmt.Errorf("reporting: list posted invoices: %w", err)
	}

	var items []OpenItem
	for _, inv := range invoices {
		if inv.Currency != currency {
			continue
		}
		settlement, err := invoicing.SettlementFor(ctx, db, tenant, inv)
		if err != nil {
			return nil, err
		}
		if settlement.OutstandingMinor <= 0 {
			continue // settled invoices leave AR entirely
		}
		items = append(items, OpenItem{
			InvoiceID:        inv.ID,
			Number:           inv.Number,
			ContactID:        inv.ContactID,
			Currency:         inv.Currency,
			IssueDate:        inv.IssueDate,
			OutstandingMinor: settlement.OutstandingMinor,
			AgeDays:          ageInDays(inv.IssueDate, asOf),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].AgeDays != items[j].AgeDays {
			return items[i].AgeDays > items[j].AgeDays // oldest first
		}
		return items[i].InvoiceID < items[j].InvoiceID
	})
	return items, nil
}

// ageInDays is how old an invoice is at asOf. An unparseable or future issue
// date ages to 0 (current) rather than erroring: a report that refuses to render
// because one row has a malformed date is less useful than one that shows the
// row in the least-alarming bucket.
func ageInDays(issueDate string, asOf time.Time) int {
	issued, err := time.Parse("2006-01-02", issueDate)
	if err != nil {
		return 0
	}
	days := int(asOf.UTC().Truncate(24*time.Hour).Sub(issued.UTC()).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}
