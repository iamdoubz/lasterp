// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"net/http"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
	"github.com/iamdoubz/lasterp/modules/tax"
)

// Permission tuples for the reference-data admin writes. WP-1.1/1.3 deferred
// authz on the rate stores as "safe while no API surface exists"; the surface
// lands here, so the seam lands with it (INV-T2). See WP-1.4b-decisions.md §4.
const (
	objectTaxRate = "TaxRate"
	objectFxRate  = "FxRate"
	actionManage  = "manage"
)

// dateLayout is the YYYY-MM-DD form the rate admin endpoints accept for as_of.
const dateLayout = "2006-01-02"

// actions builds the non-CRUD HTTP surface (lifecycle verbs, event-sourced
// reads, reference-data + capability admin). Handlers run after authn with the
// actor bound into r.Context(); write actions are wrapped with idempotency by
// the gateway. See WP-1.4b-decisions.md §3 for the full table.
func actions(db *storage.DB, reg *capability.Registry) []api.Action {
	return []api.Action{
		// --- Invoice (bespoke, NOT generic CRUD: posting pipeline is the only
		// path to posted/GL — INV-F2/F5/F6, decisions §2) ---
		{Method: "POST", Path: "/api/v1/invoices", Object: invoicing.ObjectInvoice, Write: true,
			Summary: "Create a draft invoice", Handler: createInvoice(db)},
		{Method: "PATCH", Path: "/api/v1/invoices/{id}", Object: invoicing.ObjectInvoice, Write: true,
			Summary: "Update a draft invoice", Handler: updateInvoice(db)},
		{Method: "GET", Path: "/api/v1/invoices/{id}", Object: invoicing.ObjectInvoice,
			Summary: "Get an invoice", Handler: getInvoice(db)},
		{Method: "GET", Path: "/api/v1/invoices/{id}/pdf", Object: invoicing.ObjectInvoice,
			Summary: "Render an invoice as PDF", Handler: invoicePDF(db)},
		{Method: "POST", Path: "/api/v1/invoices/{id}/post", Object: invoicing.ObjectInvoice, Write: true,
			Summary: "Post an invoice to the ledger", Handler: postInvoice(db)},

		// --- Period (bespoke: status transitions go through monotonic
		// close/reopen, never a generic PATCH — INV-F3, decisions §2) ---
		{Method: "POST", Path: "/api/v1/periods", Object: ledger.ObjectPeriod, Write: true,
			Summary: "Create a fiscal period", Handler: createPeriod(db)},
		{Method: "GET", Path: "/api/v1/periods/{id}", Object: ledger.ObjectPeriod,
			Summary: "Get a fiscal period", Handler: getPeriod(db)},
		{Method: "POST", Path: "/api/v1/periods/{id}/close", Object: ledger.ObjectPeriod, Write: true,
			Summary: "Close a fiscal period", Handler: closePeriod(db)},
		{Method: "POST", Path: "/api/v1/periods/{id}/reopen", Object: ledger.ObjectPeriod, Write: true,
			Summary: "Reopen a closed fiscal period", Handler: reopenPeriod(db)},

		// --- JournalEntry (event-sourced: read + reverse only, no CRUD) ---
		{Method: "GET", Path: "/api/v1/journalentries/{id}", Object: ledger.ObjectJournalEntry,
			Summary: "Get a journal entry", Handler: getEntry(db)},
		{Method: "POST", Path: "/api/v1/journalentries/{id}/reverse", Object: ledger.ObjectJournalEntry, Write: true,
			Summary: "Reverse a journal entry", Handler: reverseEntry(db)},

		// --- Reference-data admin (authz seam lands here, decisions §4) ---
		{Method: "POST", Path: "/api/v1/taxrates", Object: "", Write: true,
			Summary: "Record a tax rate", Handler: saveTaxRate(db)},
		{Method: "POST", Path: "/api/v1/fxrates", Object: "", Write: true,
			Summary: "Record an FX rate", Handler: saveFxRate(db)},

		// --- Capability admin (already authorizes capability:manage) ---
		{Method: "GET", Path: "/api/v1/capabilities", Object: "",
			Summary: "List enabled modules", Handler: listCapabilities(db)},
		{Method: "POST", Path: "/api/v1/capabilities/{module}/enable", Object: "", Write: true,
			Summary: "Enable a module", Handler: enableCapability(db, reg)},
		{Method: "POST", Path: "/api/v1/capabilities/{module}/disable", Object: "", Write: true,
			Summary: "Disable a module", Handler: disableCapability(db, reg)},
	}
}

// --- request DTOs (snake_case JSON at the API boundary) ---

type draftReq struct {
	ContactID  string           `json:"contact_id"`
	Currency   string           `json:"currency"`
	IssueDate  string           `json:"issue_date"`
	ARAccount  string           `json:"ar_account"`
	TaxAccount string           `json:"tax_account"`
	Lines      []invoicing.Line `json:"lines"`
}

func (d draftReq) toInput() invoicing.DraftInput {
	return invoicing.DraftInput{
		ContactID: d.ContactID, Currency: d.Currency, IssueDate: d.IssueDate,
		ARAccount: d.ARAccount, TaxAccount: d.TaxAccount, Lines: d.Lines,
	}
}

type postReq struct {
	Period string `json:"period"`
}

type periodReq struct {
	Code      string `json:"code"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
}

type taxRateReq struct {
	Jurisdiction string `json:"jurisdiction"`
	Category     string `json:"category"`
	Rate         string `json:"rate"`
	Rounding     string `json:"rounding"`
	AsOf         string `json:"as_of"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
}

type fxRateReq struct {
	Base     string `json:"base"`
	Quote    string `json:"quote"`
	Rate     string `json:"rate"`
	AsOf     string `json:"as_of"`
	Provider string `json:"provider"`
}

// --- invoice handlers ---

func createInvoice(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		var req draftReq
		if !decodeJSON(w, r, &req) {
			return
		}
		inv, err := invoicing.CreateDraft(r.Context(), db, tenant, req.toInput())
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, inv)
	}
}

func updateInvoice(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		var req draftReq
		if !decodeJSON(w, r, &req) {
			return
		}
		inv, err := invoicing.UpdateDraft(r.Context(), db, tenant, r.PathValue("id"), req.toInput())
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, inv)
	}
}

func getInvoice(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		inv, err := invoicing.GetInvoice(r.Context(), db, tenant, r.PathValue("id"))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, inv)
	}
}

func invoicePDF(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		inv, err := invoicing.GetInvoice(r.Context(), db, tenant, r.PathValue("id"))
		if err != nil {
			fail(w, r, err)
			return
		}
		pdf, err := invoicing.RenderInvoicePDF(inv)
		if err != nil {
			fail(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pdf)
	}
}

func postInvoice(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		var req postReq
		if !decodeJSON(w, r, &req) {
			return
		}
		inv, err := invoicing.PostInvoice(r.Context(), db, tenant, r.PathValue("id"), req.Period)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, inv)
	}
}

// --- period handlers ---

func createPeriod(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		var req periodReq
		if !decodeJSON(w, r, &req) {
			return
		}
		rec, err := ledger.CreatePeriod(r.Context(), db, tenant, req.Code, req.StartDate, req.EndDate)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, rec)
	}
}

func getPeriod(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		rec, err := ledger.GetPeriod(r.Context(), db, tenant, r.PathValue("id"))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

func closePeriod(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if err := ledger.ClosePeriod(r.Context(), db, tenant, r.PathValue("id")); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": ledger.PeriodClosed})
	}
}

func reopenPeriod(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if err := ledger.ReopenPeriod(r.Context(), db, tenant, r.PathValue("id")); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": ledger.PeriodOpen})
	}
}

// --- journal-entry handlers ---

func getEntry(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		entry, err := ledger.LoadEntry(r.Context(), db, tenant, r.PathValue("id"))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, entry)
	}
}

func reverseEntry(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		// The HTTP Idempotency-Key doubles as the ledger command id, so a
		// replayed reverse produces exactly one reversing entry (INV-E4).
		commandID := "reverse-" + r.Header.Get("Idempotency-Key")
		entry, err := ledger.Reverse(r.Context(), db, tenant, r.PathValue("id"), commandID)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, entry)
	}
}

// --- reference-data admin handlers (authz seam, decisions §4) ---

func saveTaxRate(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectTaxRate, actionManage); err != nil {
			fail(w, r, err)
			return
		}
		var req taxRateReq
		if !decodeJSON(w, r, &req) {
			return
		}
		asOf, err := time.Parse(dateLayout, req.AsOf)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid as_of", err.Error(), r.URL.Path)
			return
		}
		rate := tax.Rate{
			Jurisdiction: req.Jurisdiction, Category: req.Category, Rate: req.Rate,
			Rounding: req.Rounding, AsOf: asOf, Name: req.Name, Provider: req.Provider,
		}
		if err := tax.SaveRate(r.Context(), db, tenant, rate); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"jurisdiction": req.Jurisdiction, "category": req.Category, "as_of": req.AsOf})
	}
}

func saveFxRate(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if _, err := authz.Authorize(r.Context(), db, objectFxRate, actionManage); err != nil {
			fail(w, r, err)
			return
		}
		var req fxRateReq
		if !decodeJSON(w, r, &req) {
			return
		}
		asOf, err := time.Parse(dateLayout, req.AsOf)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid as_of", err.Error(), r.URL.Path)
			return
		}
		rate := money.Rate{Base: req.Base, Quote: req.Quote, Rate: req.Rate, AsOf: asOf, Provider: req.Provider}
		if err := money.SaveRate(r.Context(), db, tenant, rate); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"base": req.Base, "quote": req.Quote, "as_of": req.AsOf})
	}
}

// --- capability handlers ---

func listCapabilities(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		mods, err := capability.EnabledModules(r.Context(), db, tenant)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"enabled": mods})
	}
}

func enableCapability(db *storage.DB, reg *capability.Registry) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		res, err := capability.Enable(r.Context(), db, reg, tenant, r.PathValue("module"))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

func disableCapability(db *storage.DB, reg *capability.Registry) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		if err := capability.Disable(r.Context(), db, reg, tenant, r.PathValue("module")); err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"disabled": r.PathValue("module")})
	}
}
