# WP-1.4 (Invoicing / AR) — interpretation & decisions

Scope: M2 Invoicing/AR minus payments/dunning (per roadmap). AC: invoice
lifecycle e2e — draft → post → GL entries correct → PDF renders. Invariants:
INV-F6 (new, this WP flips it on), INV-F5, INV-F2, INV-T1/T2/T4.

## 1. Module dependency boundaries (the load-bearing call)

`docs/10` line 3: "Modules depend on **kernel + ledger only** (no lateral
imports; events for cross-module reactions)." The M2 row also declares
`requires: [contacts, ledger.core, tax.engine]`. `CLAUDE.md` states the blunter
"modules/* may import kernel/*, never each other."

**Decision:** invoicing imports `modules/ledger` and `modules/tax` directly, and
references `contacts` by id only (no import). Rationale:

- The capability manifest `requires:` list is the *sanctioned* dependency graph.
  In a single Go binary, composing a required module = importing its public API.
- `docs/10` explicitly carves ledger out as a shared substrate ("kernel + ledger
  only"; "Ledger first because everything posts into it"). Tax is pure
  reference-data + calculation (no write coupling, no cycle: tax→kernel only).
- The AC ("post → **GL entries correct**") and INV-F5 ("posts through its
  declared **template**") require *synchronous* posting. Routing GL through async
  domain events would contradict both for v1.
- I read `CLAUDE.md`'s "never each other" as "no *undeclared* lateral imports."
  Domain events remain the mechanism for reactions between modules that do **not**
  `require` each other (e.g. inventory reacting to an invoice post — later WP).

No import cycle: `ledger→kernel`, `tax→kernel`, `invoicing→{kernel,ledger,tax}`.
Contacts referenced by id keeps that edge import-free.

*If the maintainer prefers strict event-decoupled posting, that is a larger
(async) design — flagged for the approval gate, not built unless requested.*

## 2. Object model

- **Contact** (new `modules/contacts`, capability `contacts`): CRUD object.
  Fields: `name` (req), `email`, `kind` (enum customer/vendor/both). Invoicing
  stores `contact_id` as a link; no code import.
- **Invoice** (`modules/invoicing`): CRUD object. Core fields: `contact_id`
  (link), `currency` (req), `status` (enum draft/posted), `number` (blank until
  post), `issue_date`, `ar_account`, `tax_account`, `lines` (JSON text),
  `net_minor`/`tax_minor`/`gross_minor` (int, filled at post), `gl_entry_id`,
  `posted_at`. Lines are stored as JSON (metadata `type:table` child tables are
  unsupported for CRUD DDL — WP-0.5 decision 3); the module parses them.
  ponytail: lines-as-JSON until an independent per-line query (e.g. line-level
  analytics) needs a child table.

## 3. INV-F6 — gapless document numbers (new invariant, flipped on here)

Policy: invoice numbers are **gapless per (tenant, series)**, contiguous from 1,
**assigned only at post acceptance** (drafts never carry a number). Mechanism: a
`document_number_series` table (tenant_id, series, next_value) allocated with a
row-locking `UPDATE … next_value+1` **inside the draft→posted transition tx**.

Ordering in `PostInvoice`: (1) post GL first (idempotent via command_id),
(2) then, in one tx, allocate the number **and** flip the invoice to posted.
A post that fails before step 2 (e.g. closed period) consumes no number → the
next successful post continues the sequence with no gap. Concurrent posts
serialize on the series row → no dup, no gap. A GL-succeeded / flip-failed crash
leaves an orphan GL entry (no gap); retry is idempotent. Flagged as the known
ceiling.

## 4. INV-F5 — declared posting template

All invoice→GL writes go through one pure function `buildInvoiceJournal(invoice,
taxResult)` — the "declared template" — which returns a balanced `ledger.PostCmd`:

- DR `ar_account`: gross (Σ line gross incl. tax)
- CR each line's `revenue_account`: line net (grouped by account)
- CR `tax_account`: Σ tax

`Σgross = Σnet + Σtax` ⇒ balances (INV-F1). There is no other path from an
invoice to the ledger. Test asserts the resulting GL entry / trial balance.

## 5. INV-F2 — posted invoices immutable

A storage trigger on `obj_invoice` (BEFORE UPDATE/DELETE) rejects any mutation of
a row whose **existing** status is `posted` (the draft→posted transition passes
because OLD.status='draft'). Installed by `invoicing.Register` right after
`ApplyDDL` (the metadata table only exists at runtime, so this can't be a numbered
migration — same reason ledger's triggers live on migration-created tables only).
`UpdateDraft` also refuses a non-draft at the module layer (belt-and-suspenders).
Correction path (void / credit note) reuses `ledger.Reverse` and lands with
credit notes — **deferred** (not in AC).

## 6. PDF rendering

No PDF dependency exists; adding one needs an ADR (CLAUDE.md). PDF is a simple
text container, so v1 ships a **minimal hand-rolled single-page PDF writer** in
`modules/invoicing` (pure Go, stdlib only): header, catalog, pages, one page,
one Helvetica font, a content stream with the invoice header + line table +
totals, xref + trailer. No dep, no ADR. Test asserts a valid `%PDF` header,
parseable xref/trailer, and the invoice number present. ponytail: extract to a
kernel `pdf`/template-pack service when a second document type (payslip) needs it.

## 7. Deferred (flagged, not in AC)

- **AR aging report** (`ar.aging` capability): the manifest advertises it; the
  read model (age buckets, payment-reduced balances) lands with reports WP-1.6.
  Providing the capability name here is a forward declaration, not the full impl.
- **Void / credit notes / recurring / payment terms / early-payment discount /
  PEPPOL-UBL / payment links** — all M2 breadth beyond the AC; later WPs.
- **Multi-currency invoice** — v1 is single-currency per invoice (like a ledger
  entry); FX presentation later.
