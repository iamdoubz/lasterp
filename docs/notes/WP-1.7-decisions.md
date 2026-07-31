# WP-1.7 — Translation packs + localized documents — interpretation & decisions

WP-1.7 ([docs/11-ROADMAP.md](../11-ROADMAP.md), [docs/17](../17-LOCALIZATION-ACCESSIBILITY.md)):
translation-pack pipeline + first non-English pack + per-locale data fields +
localized document rendering. **AC: invoice e2e fully localized incl. PDF in the
first non-English locale.**

WP-0.7 built the machinery (message catalog, ICU subset, locale formatting,
pseudo-locale, RTL, hardcoded-string gate) but shipped exactly one locale's
strings and nothing server-rendered. This WP makes a second locale real, end to
end, and moves document rendering onto the same layer.

## 1. First non-English locale: German (`de`)

`docs/17` says "Spanish or German". German, because the stack already speaks it:
the EU seed pack carries DE VAT (`modules/tax/seed/eu.yaml`, including the
2020–21 rate cut) and the WP-1.5 e2e already invoices in DE/EUR at 19 %. A
Spanish pack costs the same to add later — the pipeline is what this WP builds,
not the language.

## 2. Pack format: JSON, one file per locale **per render target**

A pack is `{locale, version, messages{}}` in JSON.

- **JSON, not YAML** (the repo's convention for manifests/schemas): it is read by
  Go (`encoding/json`) *and* by TypeScript (native `import`) *and* by the vitest
  parity test — YAML would need a JS parser dependency (ADR) to reach parity,
  and packs are machine-generated / AI-pretranslated artifacts, which is exactly
  what JSON is for.
- **Two files per locale**, with disjoint key namespaces and no duplicated
  string:
  - `kernel/i18n/packs/<locale>.json` — `doc.*`, the strings the **server**
    renders onto documents (the invoice PDF), plus `doc.date.pattern`.
  - `web/src/i18n/packs/<locale>.json` — the **client** UI strings, keyed
    exactly like `web/src/i18n/messages.ts` (which stays the typed English
    source of truth).

  The alternative — one file both sides read — means the web build reaching into
  `kernel/` (a `deploy/Dockerfile` web-stage COPY change and an outside-root
  Vite import) or the client fetching its catalog over HTTP, which would need a
  third `Public` route so the *login* screen could be localized. Widening the
  public-route set (fenced by `routes_integrity_test.go`, INV-T2) to ship
  translations is a bad trade. A future pack *bundle* in the plugin registry
  (WP-3.2) carries both parts under one version; today the version field is in
  both files and the completeness tests keep them honest.

- **Completeness is CI-enforced, in both languages.** `kernel/i18n`'s Go test
  asserts every non-source pack has exactly the source pack's keys; the vitest
  test asserts the same for the UI packs against `messages.ts`. A future WP that
  adds a user-facing string without translating it fails CI. That is deliberate:
  a shipped pack that silently degrades to English is how "localized" products
  end up half-English. Community packs (ADR-013's certification tier) will need a
  laxer rule; built-in packs do not get one.

## 3. Money rendering is now exact (INV-F4)

`i18n.Printer.Money` computed `float64(minorUnits) / math.Pow10(scale)` — a
float in a money path. WP-0.7's own decisions doc claims it used an exact
`*big.Rat`; the code never did. Harmless-ish while nothing rendered money
server-side; not harmless once that render is the amount printed on an invoice.

`golang.org/x/text` cannot help directly: `internal/number.Decimal.Convert`
handles ints and floats only (`big.Rat` is a TODO in the upstream source) and
its `Converter` escape hatch is in an internal package. So `Money` now:

1. splits the integer minor units into major/fraction with **integer** division
   (`uint64` magnitude, so `math.MinInt64` has no special case);
2. formats the major part with `number.Decimal(uint64)` — exact, locale-correct
   grouping;
3. takes the decimal separator, the fraction width, the symbol and its placement
   from a **sentinel render** of zero in the same locale/currency, and splices
   the exact digits into it;
4. prepends the locale's minus sign (also derived, not assumed to be `-`) ahead
   of the whole pattern, which is where CLDR puts it for `en`/`de`.

An `//go:build integrity` property test (INV-F4) renders random `int64` values —
including both extremes — across en/de × EUR/USD/JPY, parses the result back to
minor units and asserts equality. The float path is gone, not bounded.

## 4. Per-locale data fields: a kernel mechanism **and** frozen document text

Both, because they are different problems:

- **Master data** (`metadata`): a field may declare `localized: true`. Its
  per-locale values travel in a reserved `translations` key on the `Record`
  (`{field: {locale: value}}`) and are stored in the generated table's existing
  `custom_fields` blob under the reserved `i18n` namespace. No new column, no
  migration, no DDL diff (`PlanEvolution` already ignores non-storage field
  attributes), identical on both dialects. Only `text`/`long_text`/`rich_text`
  fields may be localized, and a `translations` entry naming a field that is not
  localized is rejected rather than silently stored.
  `ledger.Account.name` is the first field to use it — a localized chart of
  accounts is exactly what a country pack ships (ADR-013).
- **Document text** (`invoicing`): an invoice line carries
  `description_i18n` (`i18n.Localized`) *inside the document*. This is not the
  same as looking the text up from a master record at render time: a posted
  invoice is immutable (INV-F2), and its printed words must not change because
  someone edited an item master afterwards. Freezing the text with the document
  is the correct behaviour, and it keeps `RenderInvoicePDF` a pure function.

## 5. The document's locale is frozen on the document

`Invoice.locale` is set when the draft is created and never changes (the posted
row is immutable anyway). Resolution order when rendering:

`?locale=` (an explicit preview override) → `invoice.locale` (the counterparty's
language, frozen) → `Accept-Language` (the reader) → `en`.

`Contact.locale` is the counterparty's language, and `internal/app` fills a
draft's locale from it when the request omits one — **best effort**: a contact
that cannot be read leaves the locale empty rather than failing invoice
creation. A document's language is presentation, not a financial fact, and
refusing to create an invoice because a contact's language was unreadable would
be absurd. This also keeps `modules/invoicing` free of a `modules/contacts`
import (the manifest declares `requires: [contacts]`, so the import would be
legal — it is simply not needed, and the composition root is where cross-module
defaulting belongs, per WP-1.4b precedent 1).

## 6. Schema versions bump to 2 (first product use of WP-1.0a)

`Contact` and `Invoice` gain an optional `locale` field; `Account` gains
`localized: true` on `name`. A changed definition at the same version is
`ErrSchemaVersionConflict` by design, so each object goes to v2. On a fresh
database that is still one `CREATE TABLE`; on an existing one it is the additive
`ALTER` path from WP-1.0a — which no shipped module had exercised until now. A
tagged migration-integrity test (docs/19, "Migration integrity") seeds v1 rows,
registers v2 and asserts the column arrives with the data intact on Postgres and
SQLite.

## 7. Server-side hardcoded-string gate: a pseudo-locale test, not a linter

WP-0.7 deferred scanning Go strings ("when server-rendered user strings appear
(WP-1.7 document rendering) the gate can be extended"). Extending
`scripts/i18n-lint.sh` to Go would drown in developer-facing strings (wrapped
errors, logs) — the reason it was scoped to `.tsx` in the first place. Instead
the PDF is rendered under the **pseudo-locale** (`en-XA`) and the test asserts
every visible label comes back accented. A string that skipped the catalog is
un-accented, and the test names it. That is what pseudo-locales are for, it
costs ~15 lines, and it cannot produce a false positive on a log message.

## 8. Dates: a pattern in the pack

`x/text` has no stable CLDR date formatter (WP-0.7 decision), and dates on a
document must be unambiguous. `doc.date.pattern` (`{y}-{m}-{d}` for `en`,
`{d}.{m}.{y}` for `de`) is pack data — the same mechanism that carries the
strings, no new dependency, and a pack author can fix their own date order.
Storage stays ISO/UTC; only the render substitutes.

## 9. PDF text encoding: WinAnsi

The WP-1.4 hand-rolled PDF used Helvetica with no `/Encoding`, i.e. the font's
built-in StandardEncoding, which has no `ä`, `ö`, `ü` or `ß` — a German invoice
would have printed `W hrung`. The font dictionary now declares
`/Encoding /WinAnsiEncoding` and text is transcoded with
`golang.org/x/text/encoding/charmap.Windows1252` (already a direct dependency).

`ponytail:` this covers Latin-1/Western Europe (de, es, fr, it, nl, pt, the
Nordics). Greek, Cyrillic, Turkish and CJK need an embedded TrueType font with a
CID encoding — the upgrade path is the shared kernel PDF/template-pack service
`pdf.go` already names, and unmappable runes are replaced with `?` rather than
emitting bytes that would render as garbage.

## 10. Locale selection in the client

`?locale=` → `localStorage` → `navigator.language` → `en`, plus a labelled
`<select>` switcher in the shell (axe-scanned like every other control). The
query parameter stays the deterministic override the e2e and the pseudo-locale
build drive; `localStorage` is what makes a choice survive a reload; the browser
language is what makes a German user get German without touching anything.

## 11. Deliberately not in this WP

- **Per-tenant, per-locale relabelling of objects/fields** (docs/17's "tenant
  overlays can rename anything — per-locale"). Field *labels* do not exist in
  the object schema at all yet (`metadata.Field` has no `Label`; the client
  humanizes the field name). This WP gives labels a catalog key
  (`field.<Object>.<name>`) with the humanized name as fallback, which is the
  translation half. Server-side labels + tenant overlay relabelling is ADR-006
  overlay work and belongs with the UI-descriptor WP that introduces them.
- **Runtime pack installation** (packs as installable versioned packages).
  Built-in packs compile in, exactly like capability manifests and tax seed
  packs. Distribution belongs to the plugin/registry pipeline (WP-3.2); the pack
  format and `version` field are chosen so that lands as a loader change, not a
  format change.
- **A translations database table.** Nothing needs per-tenant message overrides
  until the relabelling story above exists.
- **A locale-discovery endpoint** (`GET /api/v1/i18n/locales`). Localization is
  reachable over the API — `?locale=` on the document route, `translations` on
  any localized field, `localized` on the metadata surface — but *which* locales
  a server has is not enumerable yet. The web client compiles its own packs so it
  already knows; the endpoint is worth having when packs become installable at
  runtime (WP-3.2) and the answer stops being a build-time constant.
- **Editing per-locale values in the UI.** Forms edit the canonical field only,
  so saving a form in German cannot overwrite the English value with a German
  one; list and detail views resolve translations for display. A control per
  locale is UI-descriptor work, and the API already carries the data.
- **Country/compliance packs** (CoA templates, statutory layouts, e-invoicing) —
  WP-4.12 and the country-pack track; `docs/17` scopes those away from WP-1.7
  explicitly.
- **Localized `Receipt` PDF** — there is no receipt PDF to localize.

## 12. Invariants

No new INV-\* is registered: translations are presentation, and presentation
carries no financial or tenancy invariant of its own. The WP touches:

| Invariant | How |
|---|---|
| **INV-F4** | `Printer.Money` becomes exact; property test at `int64` extremes (§3) |
| **INV-F2** | The rendered language is frozen on the document; test asserts a posted invoice's PDF does not change language when the contact's locale is edited afterwards |
| **INV-T1** | `translations` ride in `custom_fields` on the same tenant-scoped, RLS-backed row as the record; no new read path |
| **INV-T2** | No new `Public` route; the PDF route's authorization is unchanged, and the locale is a rendering input, not an authorization input |
| Migration integrity | §6 |
