# 17 — Localization, Internationalization & Accessibility

"Take on every industry" implies every geography. i18n retrofitted is i18n done twice — these are day-one kernel concerns, not polish.

## Internationalization (kernel)
- All user-facing strings (core, modules, plugins, metadata labels) flow through a translation layer; ICU MessageFormat for plurals/genders; no concatenated sentence fragments — enforced by lint.
- Locale-aware formatting everywhere: dates, numbers, currency display (symbol position, decimal separators), name/address ordering, first-day-of-week, fiscal-year conventions. Storage stays canonical (UTC, ISO-4217, E.164); rendering localizes.
- **RTL support** (Arabic, Hebrew) in the UI kit from the first component — logical CSS properties only.
- Translations as data: core ships English; language packs are versioned packages (same distribution as plugins), community-maintained via a translation portal (Weblate-style), AI-pretranslated → human-reviewed. Tenant overlays can rename anything (already in ADR-006) — per-locale.
- Multi-language *data*: designated fields (item names, descriptions) support per-locale values; documents render in the counterparty's language (invoice to a French customer prints in French from the same record).

## Country/compliance packs (extends ADR-013)
One pack format bundles everything a jurisdiction needs: CoA template, tax rules, document numbering/retention rules (e.g., GoBD, French NF525-adjacent constraints), statutory report layouts, **e-invoicing mandate adapters** — the regulatory wave of the decade (PEPPOL, Italy SDI, France Factur-X/PPF, Germany XRechnung/ZUGFeRD, Poland KSeF, LATAM CFDI/NF-e models) — payment file formats (SEPA, NACHA, BACS), and payroll rules where available. Packs are versioned, effective-dated, community-maintainable with a certification tier.

## Accessibility (WCAG 2.2 AA as CI gate, not aspiration)
- UI kit components ship accessible by construction: full keyboard operability (we're keyboard-first anyway — docs/14 §8), visible focus, ARIA correctness, 4.5:1 contrast tokens in both themes, reduced-motion support, screen-reader-tested data tables and form patterns.
- Metadata-rendered UIs inherit accessibility from the kit — one fix heals every screen; hand-built screens (reconciliation workbench, dashboards) get explicit audit items in their WP acceptance criteria.
- CI: axe-core automated scans on every Playwright flow + quarterly manual screen-reader pass (NVDA/VoiceOver) on the top 20 flows.
- The AI surface is an accessibility feature: every workflow drivable by natural language via MCP (docs/06) is inherently an alternative input path.

## Build plan
- **WP-0.7 (added to Phase 0) — done:** i18n kernel — string layer, ICU, locale formatting, lint rules, RTL-safe UI kit foundations. AC: pseudo-locale (ÀççéñţéÐ + RTL) build renders correctly; hardcoded-string lint gate green.
  - Server string/format layer: `kernel/i18n` (message catalog + per-locale printer over `golang.org/x/text`, plural rules, locale number/currency formatting from integer minor units, pseudo-locale generation, RTL detection).
  - Web foundation: `web/src/i18n` (`I18nProvider`/`useT`, native-`Intl` ICU-subset formatter, pseudo + RTL locales driven by `?locale=pseudo|ar`; the document `dir` attribute + CSS logical properties carry direction).
  - Hardcoded-string lint gate: `scripts/i18n-lint.sh` (fixture-tested by `scripts/i18n-lint_test.sh`), wired into CI and `make lint`. Rationale and scope in `docs/notes/WP-0.7-decisions.md`.
- **WP-1.7 (Phase 1) — done:** translation-pack pipeline + first non-English pack (**German**) + per-locale data fields + localized document rendering. AC: invoice e2e fully localized incl. PDF.
  - **Packs are versioned JSON data**, one file per locale per render target: [`kernel/i18n/packs`](../kernel/i18n/packs) carries the strings the server prints on documents (plus `doc.date.pattern`, since x/text has no CLDR date formatter), `web/src/i18n/packs` carries the UI strings. Both are embedded/compiled — runtime pack *installation* rides on the plugin registry (WP-3.2). Completeness is CI-enforced in both languages: a pack must answer every source key and invent none.
  - **Per-locale data:** a metadata field may declare `localized: true`; its values travel in the record's reserved `translations` key and live in the existing `custom_fields` blob, so translating a field is not a migration. `ledger.Account.name` is the first (a localized chart of accounts is country-pack material, ADR-013). Document text (invoice line descriptions) is frozen *into* the document instead — a posted invoice's words are part of it (INV-F2).
  - **Documents render in the counterparty's language:** `Contact.locale` → `Invoice.locale` at draft time, then `?locale=` → the document's locale → `Accept-Language` → English at render time. The PDF declares `/WinAnsiEncoding` (the base-14 default encoding has no umlauts) and money rendering became exact — the old float divide lost digits past 2^53 (INV-F4).
  - **Metadata labels** are translated client-side via `schema.object.*` / `schema.field.*` keys with the humanized field name as fallback; per-tenant, per-locale relabelling waits for labels in the object schema. Decisions: [docs/notes/WP-1.7-decisions.md](notes/WP-1.7-decisions.md).
- **WP-4.12 (Phase 4):** e-invoicing adapter framework + PEPPOL + two national mandates. AC: golden-file conformance against official validators.
- Accessibility gates: axe-core in CI from WP-1.5 onward; WCAG AA audit item in every UI-touching WP's definition of done.
