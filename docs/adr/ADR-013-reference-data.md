# ADR-013: Tax, currency & compliance data — adapter pattern, no vendor lock

**Status:** Accepted · 2026-07-06

## Context
"Read tax data, local tax laws, currency rates" spans wildly different regimes (US sales tax: 4,200+ jurisdictions; EU VAT; payroll withholding tables). No open dataset covers it all; commercial vendors (Avalara, Vertex, Thomson Reuters, PayrollTax API, Gusto Embedded) do, at a price.

## Decision
Kernel defines **provider interfaces**; implementations are connectors/plugins:

- `FxRateProvider` — built-in free providers: ECB reference rates, national central banks; manual rate entry always available. Rates stored historically (rate-as-of-transaction-date is an accounting requirement).
- `TaxRateProvider` — v1 ships **local editable tax tables** (jurisdictions, rates, effective-date ranges, rules as data) with community-maintained seed packs (US state-level, EU VAT, CA GST/PST…). Commercial adapters (Avalara AvaTax, Vertex, Numeral, PayrollTax API) as optional certified connectors for district-level US precision and filing.
- `PayrollRuleProvider` — payroll calculation rules are **per-country plugin packs** (see 10-MODULES.md); withholding tables are versioned data with effective dates.
- All reference data is: versioned, effective-dated, auditable (which rate applied to this document and why), overridable per tenant with warning.

## Rationale
- A free product cannot hard-depend on paid data; a serious product cannot pretend free data is always enough. Adapters give both.
- Effective-dating everything is what lets an offline client price an invoice correctly and lets the server revalidate it on sync.

## Consequences
- Tax engine evaluates against the document's date and jurisdiction resolution chain (ship-from/ship-to/place-of-supply rules as data).
- Community seed-pack repo with review process; packs carry disclaimers (not legal advice).
