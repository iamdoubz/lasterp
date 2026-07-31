//go:build integrity

// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/idgen"
)

// WP-1.7 over HTTP: a document renders in the counterparty's language, that
// language is frozen onto the document (INV-F2), and localized master data
// round-trips through generic CRUD (INV-T1/T2 unchanged — the locale is a
// rendering input, never an authorization one).

// germanInvoice creates a German-speaking customer and a posted invoice for it,
// without ever naming a locale on the invoice itself: the point is that the
// document picks the language up from the counterparty.
func germanInvoice(t *testing.T, e *env) (invoiceID, contactID string) {
	t.Helper()

	ar := e.createAccount("1100", "Accounts receivable", "asset")
	rev := e.createAccount("4000", "Revenue", "income")
	taxAcct := e.createAccount("2200", "Tax payable", "liability")

	status, body, contact := e.post("/api/v1/contact", map[string]any{
		"name": "Wile E. Coyote", "email": idgen.New() + "@acme.test",
		"kind": "customer", "locale": "de",
	})
	if status != http.StatusCreated {
		t.Fatalf("create contact = %d; body=%s", status, body)
	}
	contactID = mustField(t, contact, "id")

	if status, body, _ := e.post("/api/v1/periods", map[string]any{
		"code": "2026-07", "start_date": "2026-07-01", "end_date": "2026-07-31",
	}); status != http.StatusCreated {
		t.Fatalf("create period = %d; body=%s", status, body)
	}
	if status, body, _ := e.post("/api/v1/taxrates", map[string]any{
		"jurisdiction": "DE", "category": "standard", "rate": "0.19",
		"as_of": "2020-01-01", "name": "USt", "provider": "test",
	}); status != http.StatusCreated {
		t.Fatalf("save tax rate = %d; body=%s", status, body)
	}

	status, body, draft := e.post("/api/v1/invoices", map[string]any{
		"contact_id": contactID, "currency": "EUR", "issue_date": "2026-07-15",
		"ar_account": ar, "tax_account": taxAcct,
		"lines": []map[string]any{{
			"description":      "Rocket-powered roller skates",
			"description_i18n": map[string]any{"de": "Raketenrollschuhe"},
			"quantity":         1, "unit_price_minor": 100000,
			"revenue_account": rev, "tax_jurisdiction": "DE", "tax_category": "standard",
		}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create invoice = %d; body=%s", status, body)
	}
	invoiceID = mustField(t, draft, "ID")

	// The contact's language was copied onto the document at draft time.
	if locale, _ := draft["Locale"].(string); locale != "de" {
		t.Fatalf("draft Locale = %q, want %q (the counterparty's language)", locale, "de")
	}

	if status, body, _ := e.post("/api/v1/invoices/"+invoiceID+"/post", map[string]any{
		"period": "2026-07",
	}); status != http.StatusOK {
		t.Fatalf("post invoice = %d; body=%s", status, body)
	}
	return invoiceID, contactID
}

// pdf fetches a rendered invoice, optionally with query and headers.
func (e *env) pdf(t *testing.T, path string, headers map[string]string) []byte {
	t.Helper()
	req, err := http.NewRequest("GET", e.server.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF")) {
		t.Fatalf("GET %s did not return a PDF", path)
	}
	return buf.Bytes()
}

// TestInvoicePDFRendersInTheCounterpartysLanguage is the server half of the
// WP-1.7 acceptance criterion.
func TestInvoicePDFRendersInTheCounterpartysLanguage(t *testing.T) {
	for name, db := range bootDBs(t) {
		t.Run(name, func(t *testing.T) {
			e := seed(t, db)
			invoiceID, _ := germanInvoice(t, e)

			german := e.pdf(t, "/api/v1/invoices/"+invoiceID+"/pdf", nil)
			for _, want := range []string{"Rechnung", "Rechnungsdatum", "15.07.2026", "Raketenrollschuhe", "1.190,00"} {
				if !bytes.Contains(german, []byte(want)) {
					t.Errorf("German invoice PDF is missing %q", want)
				}
			}
			if bytes.Contains(german, []byte("Issue date")) {
				t.Error("German invoice PDF still carries an English label")
			}

			// An explicit override renders the same document for an operator who
			// wants to read it in their own language.
			english := e.pdf(t, "/api/v1/invoices/"+invoiceID+"/pdf?locale=en", nil)
			if !bytes.Contains(english, []byte("Issue date")) {
				t.Error("?locale=en did not override the document language")
			}
			if !bytes.Contains(english, []byte("Rocket-powered roller skates")) {
				t.Error("English render did not fall back to the canonical line text")
			}

			// An unsupported locale falls back rather than failing: a document
			// that will not render is worse than one in the wrong language.
			klingon := e.pdf(t, "/api/v1/invoices/"+invoiceID+"/pdf?locale=tlh", nil)
			if !bytes.Contains(klingon, []byte("Invoice")) {
				t.Error("an unsupported locale did not fall back to the source language")
			}
		})
	}
}

// INV-F2: the language a posted document renders in is part of the document.
// Editing the contact afterwards must not change what the customer's copy says.
func TestPostedInvoiceKeepsItsLanguage(t *testing.T) {
	db := sqliteBootDB(t)
	e := seed(t, db)
	invoiceID, contactID := germanInvoice(t, e)

	before := e.pdf(t, "/api/v1/invoices/"+invoiceID+"/pdf", nil)

	status, body, _ := e.call("PATCH", "/api/v1/contact/"+contactID, e.token, idgen.New(),
		map[string]any{"locale": "en"})
	if status != http.StatusOK {
		t.Fatalf("update contact locale = %d; body=%s", status, body)
	}

	after := e.pdf(t, "/api/v1/invoices/"+invoiceID+"/pdf", nil)
	if !bytes.Equal(before, after) {
		t.Error("INV-F2: a posted invoice's rendering changed after the contact was edited")
	}
	if !bytes.Contains(after, []byte("Rechnung")) {
		t.Error("INV-F2: the posted invoice stopped rendering in the language it was issued in")
	}
}

// A document with no locale of its own falls back to the reader's browser.
func TestAcceptLanguageIsTheLastResort(t *testing.T) {
	db := sqliteBootDB(t)
	e := seed(t, db)

	ar := e.createAccount("1100", "Accounts receivable", "asset")
	rev := e.createAccount("4000", "Revenue", "income")
	taxAcct := e.createAccount("2200", "Tax payable", "liability")
	status, body, contact := e.post("/api/v1/contact", map[string]any{
		"name": "No Preference GmbH", "email": idgen.New() + "@acme.test", "kind": "customer",
	})
	if status != http.StatusCreated {
		t.Fatalf("create contact = %d; body=%s", status, body)
	}
	status, body, draft := e.post("/api/v1/invoices", map[string]any{
		"contact_id": mustField(t, contact, "id"), "currency": "EUR", "issue_date": "2026-07-15",
		"ar_account": ar, "tax_account": taxAcct,
		"lines": []map[string]any{{
			"description": "Anvil", "quantity": 1, "unit_price_minor": 5000,
			"revenue_account": rev, "tax_jurisdiction": "DE", "tax_category": "standard",
		}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create invoice = %d; body=%s", status, body)
	}
	if locale, _ := draft["Locale"].(string); locale != "" {
		t.Fatalf("draft Locale = %q, want empty (the contact has no language)", locale)
	}

	id := mustField(t, draft, "ID")
	rendered := e.pdf(t, "/api/v1/invoices/"+id+"/pdf", map[string]string{
		"Accept-Language": "de-AT,de;q=0.9,en;q=0.5",
	})
	if !bytes.Contains(rendered, []byte("Rechnung")) {
		t.Error("Accept-Language was not used for a document with no language of its own")
	}
}

// An invalid locale on a draft is refused at the boundary rather than stored
// and discovered at render time.
func TestInvalidLocaleRejected(t *testing.T) {
	db := sqliteBootDB(t)
	e := seed(t, db)

	ar := e.createAccount("1100", "Accounts receivable", "asset")
	rev := e.createAccount("4000", "Revenue", "income")
	taxAcct := e.createAccount("2200", "Tax payable", "liability")
	status, body, contact := e.post("/api/v1/contact", map[string]any{
		"name": "Acme", "email": idgen.New() + "@acme.test", "kind": "customer",
	})
	if status != http.StatusCreated {
		t.Fatalf("create contact = %d; body=%s", status, body)
	}

	status, body, _ = e.post("/api/v1/invoices", map[string]any{
		"contact_id": mustField(t, contact, "id"), "currency": "EUR", "issue_date": "2026-07-15",
		"ar_account": ar, "tax_account": taxAcct, "locale": "not a locale",
		"lines": []map[string]any{{
			"description": "Anvil", "quantity": 1, "unit_price_minor": 5000,
			"revenue_account": rev, "tax_jurisdiction": "DE", "tax_category": "standard",
		}},
	})
	if status < 400 || status >= 500 {
		t.Fatalf("create invoice with a bad locale = %d, want a 4xx; body=%s", status, body)
	}
}

// Localized master data round-trips through the generic CRUD surface, and the
// metadata surface tells the renderer which fields carry translations.
func TestLocalizedAccountNameOverHTTP(t *testing.T) {
	db := sqliteBootDB(t)
	e := seed(t, db)

	status, body, account := e.post("/api/v1/account", map[string]any{
		"code": "1100", "name": "Accounts receivable", "type": "asset",
		"translations": map[string]any{
			"name": map[string]any{"de": "Forderungen aus Lieferungen und Leistungen"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create account = %d; body=%s", status, body)
	}

	status, body, got := e.get("/api/v1/account/" + mustField(t, account, "id"))
	if status != http.StatusOK {
		t.Fatalf("get account = %d; body=%s", status, body)
	}
	translations, ok := got["translations"].(map[string]any)
	if !ok {
		t.Fatalf("account carries no translations: %s", body)
	}
	name, ok := translations["name"].(map[string]any)
	if !ok {
		t.Fatalf("no translations for name: %v", translations)
	}
	if name["de"] != "Forderungen aus Lieferungen und Leistungen" {
		t.Errorf("German account name = %v", name["de"])
	}

	// Translating a field nobody declared localized is refused, not ignored.
	status, body, _ = e.post("/api/v1/account", map[string]any{
		"code": "1200", "name": "Other", "type": "asset",
		"translations": map[string]any{"code": map[string]any{"de": "1200-DE"}},
	})
	if status < 400 || status >= 500 {
		t.Fatalf("translations for a non-localized field = %d, want a 4xx; body=%s", status, body)
	}

	// And the renderer can tell which fields those are.
	status, raw, _ := e.get("/api/v1/meta/objects")
	if status != http.StatusOK {
		t.Fatalf("meta objects = %d", status)
	}
	var payload struct {
		Data []metaObject `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode meta objects: %v", err)
	}
	found := false
	for _, object := range payload.Data {
		if object.Name != "Account" {
			continue
		}
		for _, field := range object.Fields {
			if field.Name == "name" && field.Localized {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("meta/objects does not report Account.name as localized: %s", raw)
	}
}

// The pack ships every string the UI asks for; this asserts the server's own
// document strings are all present in the shipped non-English pack, so a
// "localized" invoice cannot be half English on a running server.
func TestGermanPackCoversEveryDocumentString(t *testing.T) {
	db := sqliteBootDB(t)
	e := seed(t, db)
	invoiceID, _ := germanInvoice(t, e)

	rendered := string(e.pdf(t, "/api/v1/invoices/"+invoiceID+"/pdf", nil))
	for _, englishOnly := range []string{"Issue date", "Bill to", "Currency", "Description", "Unit price"} {
		if strings.Contains(rendered, englishOnly) {
			t.Errorf("German invoice still shows the English label %q", englishOnly)
		}
	}
}
