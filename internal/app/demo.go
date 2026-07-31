// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/contacts"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
	"github.com/iamdoubz/lasterp/modules/tax"
)

// Seeding a demo book (WP-1.8). A dashboard on an empty tenant is a grid of
// zeroes, so "fresh tenant shows a live role dashboard" needs a book to exist —
// and the fastest way to disbelieve a dashboard is to be shown one full of
// invented numbers that no ledger backs.
//
// Every write below goes through the ordinary authorized module entry points:
// accounts via ledger.CreateAccount, invoices via CreateDraft + PostInvoice, the
// receipt via PostReceipt. There is no bulk path and no direct insert, so the
// seeded book satisfies INV-F1 (balanced), INV-F3 (open period), INV-F5
// (declared template) and INV-F6 (gapless numbers) by construction rather than
// by inspection — which is what INV-X5 demands of a bulk path: batching, never
// bypasses.

// ErrDemoNotEmpty is returned when the tenant already has posted invoices.
// Seeding into a real book would silently inflate someone's revenue.
var ErrDemoNotEmpty = errors.New("app: tenant already has posted invoices; refusing to seed demo data")

// DemoInput names the tenant to seed and the user to attribute the writes to.
type DemoInput struct {
	Tenant string
	Email  string // an existing user in that tenant; the seed is attributed to them
	// Currency the demo book is kept in (default EUR).
	Currency string
	// Today anchors the two seeded periods. Zero means the real clock.
	Today time.Time
}

// demoAccount is one chart-of-accounts row the demo book needs.
type demoAccount struct{ code, name, kind string }

var demoAccounts = []demoAccount{
	{"1000", "Bank", ledger.AccountAsset},
	{"1100", "Accounts receivable", ledger.AccountAsset},
	{"2200", "Tax payable", ledger.AccountLiability},
	{"4000", "Consulting revenue", ledger.AccountIncome},
	{"4100", "Licence revenue", ledger.AccountIncome},
	{"6000", "Operating expenses", ledger.AccountExpense},
}

// demoCustomer is a counterparty and the invoices it is billed.
type demoCustomer struct {
	name, email, locale string
	// invoices are (period index, revenue account code, description, minor
	// units) — spread across both periods so the dashboard's comparisons have
	// something real to compare.
	invoices []demoInvoice
}

type demoInvoice struct {
	period      int
	revenue     string
	description string
	minor       int64
	// paid marks an invoice a receipt settles in full, so the seeded book has
	// both collected and outstanding receivables.
	paid bool
}

var demoCustomers = []demoCustomer{
	{
		name: "Kojote Logistik GmbH", email: "ap@kojote.example", locale: "de",
		invoices: []demoInvoice{
			{period: 0, revenue: "4000", description: "Beratung Q2", minor: 480000, paid: true},
			{period: 1, revenue: "4000", description: "Beratung Q3", minor: 520000},
		},
	},
	{
		name: "Roadrunner Freight", email: "ap@roadrunner.example",
		invoices: []demoInvoice{
			{period: 0, revenue: "4100", description: "Platform licence", minor: 250000},
			{period: 1, revenue: "4100", description: "Platform licence", minor: 250000},
			{period: 1, revenue: "4000", description: "Onboarding", minor: 90000},
		},
	},
	{
		name: "Acme Anvils", email: "ap@acme.example",
		invoices: []demoInvoice{
			{period: 1, revenue: "4000", description: "Migration workshop", minor: 160000},
		},
	},
}

// SeedDemo fills a tenant with a small but real book: a chart of accounts, two
// fiscal periods, three customers, invoices posted across both periods, and one
// receipt settling the oldest invoice.
//
// It is an operator action like Bootstrap, run under the operator's own database
// credentials and deliberately not reachable over the API — nothing in the
// product should be able to conjure revenue over HTTP.
func SeedDemo(ctx context.Context, db *storage.DB, in DemoInput) error {
	if in.Tenant == "" || in.Email == "" {
		return errors.New("app: tenant and email are required")
	}
	currency := in.Currency
	if currency == "" {
		currency = "EUR"
	}
	today := in.Today
	if today.IsZero() {
		today = time.Now().UTC()
	}

	tenant := tenancy.ID(in.Tenant)
	user, err := identity.GetUserByEmail(ctx, db, tenant, in.Email)
	if err != nil {
		return fmt.Errorf("app: demo seed: find user %s: %w", in.Email, err)
	}
	// Attributed to a real user, through the same authorization every other
	// write passes (INV-T2/T4). A seeder with its own privileged identity would
	// be the side door this codebase keeps refusing to build.
	ctx = authz.WithActor(ctx, authz.Actor{TenantID: tenant, UserID: user.ID})

	posted, err := invoicing.ListPosted(ctx, db, tenant)
	if err != nil {
		return fmt.Errorf("app: demo seed: check existing invoices: %w", err)
	}
	if len(posted) > 0 {
		return fmt.Errorf("%w (%d found)", ErrDemoNotEmpty, len(posted))
	}

	accounts, err := seedAccounts(ctx, db, tenant)
	if err != nil {
		return err
	}
	periods, err := seedPeriods(ctx, db, tenant, today)
	if err != nil {
		return err
	}
	if err := seedTaxRate(ctx, db, tenant); err != nil {
		return err
	}
	return seedInvoices(ctx, db, tenant, currency, accounts, periods)
}

func seedAccounts(ctx context.Context, db *storage.DB, tenant tenancy.ID) (map[string]string, error) {
	out := make(map[string]string, len(demoAccounts))
	for _, a := range demoAccounts {
		rec, err := ledger.CreateAccount(ctx, db, tenant, a.code, a.name, a.kind, "", "")
		if err != nil {
			return nil, fmt.Errorf("app: demo seed: account %s: %w", a.code, err)
		}
		id, _ := rec["id"].(string)
		out[a.code] = id
	}
	return out, nil
}

// seedPeriods creates the previous and current calendar months. Two periods is
// the minimum that makes a comparison real: with one, every card would render
// without a prior value and the "mandatory comparison" would be vacuous.
func seedPeriods(ctx context.Context, db *storage.DB, tenant tenancy.ID, today time.Time) ([]string, error) {
	thisMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastMonth := thisMonth.AddDate(0, -1, 0)

	var codes []string
	for _, start := range []time.Time{lastMonth, thisMonth} {
		end := start.AddDate(0, 1, -1)
		code := start.Format("2006-01")
		if _, err := ledger.CreatePeriod(ctx, db, tenant, code,
			start.Format(dateLayout), end.Format(dateLayout)); err != nil {
			return nil, fmt.Errorf("app: demo seed: period %s: %w", code, err)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// seedTaxRate records a tenant-level VAT rate so invoices post tax through the
// real engine rather than at zero.
func seedTaxRate(ctx context.Context, db *storage.DB, tenant tenancy.ID) error {
	asOf, err := time.Parse(dateLayout, "2020-01-01")
	if err != nil {
		return err
	}
	rate := tax.Rate{
		Jurisdiction: "DE", Category: "standard", Rate: "0.19",
		AsOf: asOf, Name: "USt", Provider: "demo",
	}
	if err := tax.SaveRate(ctx, db, tenant, rate); err != nil {
		return fmt.Errorf("app: demo seed: tax rate: %w", err)
	}
	return nil
}

func seedInvoices(ctx context.Context, db *storage.DB, tenant tenancy.ID, currency string, accounts map[string]string, periods []string) error {
	for _, c := range demoCustomers {
		contact, err := contacts.CreateContact(ctx, db, tenant, c.name, c.email, contacts.KindCustomer)
		if err != nil {
			return fmt.Errorf("app: demo seed: contact %s: %w", c.name, err)
		}
		contactID, _ := contact["id"].(string)
		if c.locale != "" {
			if err := contacts.SetLocale(ctx, db, tenant, contactID, c.locale); err != nil {
				return fmt.Errorf("app: demo seed: contact locale: %w", err)
			}
		}

		for _, inv := range c.invoices {
			issue := periods[inv.period] + "-15"
			draft, err := invoicing.CreateDraft(ctx, db, tenant, invoicing.DraftInput{
				ContactID: contactID, Currency: currency, IssueDate: issue,
				ARAccount: accounts["1100"], TaxAccount: accounts["2200"],
				Locale: c.locale,
				Lines: []invoicing.Line{{
					Description: inv.description, Quantity: 1, UnitPriceMinor: inv.minor,
					RevenueAccount:  accounts[inv.revenue],
					TaxJurisdiction: "DE", TaxCategory: "standard",
				}},
			})
			if err != nil {
				return fmt.Errorf("app: demo seed: draft for %s: %w", c.name, err)
			}
			postedInvoice, err := invoicing.PostInvoice(ctx, db, tenant, draft.ID, periods[inv.period])
			if err != nil {
				return fmt.Errorf("app: demo seed: post invoice for %s: %w", c.name, err)
			}
			if !inv.paid {
				continue
			}
			if err := seedReceipt(ctx, db, tenant, currency, accounts, periods[inv.period],
				contactID, issue, postedInvoice.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedReceipt settles an invoice through the real receipt pipeline, so AR
// outstanding and the aging report reflect a part-collected book rather than one
// where nobody has ever paid.
func seedReceipt(ctx context.Context, db *storage.DB, tenant tenancy.ID, currency string,
	accounts map[string]string, period, contactID, receivedDate, invoiceID string,
) error {
	// The receipt settles the invoice's gross, which includes tax — the amount
	// the customer actually transferred.
	inv, err := invoicing.GetInvoice(ctx, db, tenant, invoiceID)
	if err != nil {
		return fmt.Errorf("app: demo seed: reload invoice: %w", err)
	}
	draft, err := invoicing.CreateReceiptDraft(ctx, db, tenant, invoicing.ReceiptInput{
		ContactID: contactID, Currency: currency, ReceivedDate: receivedDate,
		BankAccount:  accounts["1000"],
		Applications: []invoicing.Application{{InvoiceID: invoiceID, AmountMinor: inv.GrossMinor}},
	})
	if err != nil {
		return fmt.Errorf("app: demo seed: receipt draft: %w", err)
	}
	if _, err := invoicing.PostReceipt(ctx, db, tenant, draft.ID, period); err != nil {
		return fmt.Errorf("app: demo seed: post receipt: %w", err)
	}
	return nil
}
