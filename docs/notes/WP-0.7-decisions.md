# WP-0.7 — i18n kernel — interpretation & decisions

WP-0.7 (docs/11-ROADMAP.md:37, docs/17): i18n kernel — string layer, ICU,
locale formatting, RTL-safe UI-kit foundations. AC: pseudo-locale (ÀççéñţéÐ +
RTL) build renders correctly; hardcoded-string lint gate green.

This is the "parallel, low-dependency" Phase-0 WP; it does **not** depend on
WP-0.6. Where the design left choices open, this note records what was decided.

## Dependencies

- **`golang.org/x/text`** — already present as an *indirect* dependency
  (go.mod, v0.37.0). Promoted to a direct dependency by using it in
  `kernel/i18n`. This follows the WP-0.3 `google/uuid` precedent of promoting an
  existing indirect dep rather than adding a genuinely new one — no ADR needed
  and no new module enters the graph. x/text is the Go standard choice for
  CLDR/ICU plural rules, number and currency formatting; we do not reinvent it.
- **Web:** no new dependency. Locale number/date/currency formatting uses the
  platform-native `Intl` API (Intl.NumberFormat, Intl.DateTimeFormat,
  Intl.PluralRules), already in every target browser. ICU MessageFormat plural
  and interpolation is a ~60-line native parser subset — an `intl-messageformat`
  dependency is not warranted for the foundation.

## String layer

- **Go (`kernel/i18n`):** a `Translator` wraps an `x/text/message` catalog and
  hands out per-locale `Printer`s. `Printer.T(key, args…)` looks a message up by
  key with automatic fallback to the key itself when a locale/key is missing
  (x/text default). Plurals use `x/text/feature/plural`; a table-driven test
  demonstrates one/other selection.
- **Web (`web/src/i18n`):** an `I18nProvider` context + `useT()` hook. Messages
  are a typed `en` record (source of truth). `t(key, vars)` runs the message
  through a minimal ICU MessageFormat subset supporting `{arg}` interpolation
  and `{arg, plural, one {…} other {…}}` / `{arg, select, …}`, backed by native
  `Intl.PluralRules`.

## Locale formatting

- Storage stays canonical (UTC, ISO-4217, integer minor units) — rendering
  localizes. `Printer.Number` and `Printer.Money(minorUnits, iso4217)` render
  via x/text. `Money` converts integer minor units → an exact `*big.Rat`
  (no float, per the money hard rule) using the currency's CLDR fraction
  digits, then formats with locale-correct symbol placement.
- Date localization on the server is intentionally **out of scope** for the
  foundation: x/text has no stable CLDR date formatter, and dates are rendered
  client-side where `Intl.DateTimeFormat` is native and complete. Storage
  remains UTC.

## Pseudo-locale & RTL

- Pseudo-localization follows the Chrome/Android convention: **en-XA** =
  accented Latin (ÀççéñţéÐ), used to surface un-externalized strings and,
  because the transform expands length ~40%, truncation bugs. The transform
  preserves `%verbs` and `{…}` placeholders so formatting still works.
- To satisfy the AC's "ÀççéñţéÐ **+ RTL**" in a single build, the web
  pseudo-locale renders accented text **and** sets `dir="rtl"`, exercising both
  the accent path and the RTL/logical-CSS path at once. A real RTL locale
  (`ar`) is also wired for direction detection.
- `i18n.IsRTL(tag)` / `DirectionOf(tag)` detect RTL from the language subtag set
  (ar, he, fa, ur, ps, sd, ug, yi, dv, …). The UI kit foundation uses CSS
  logical properties and a single `dir` attribute on the document root — no
  hardcoded left/right.

## Hardcoded-string lint gate

- Implemented as a scripted gate, `scripts/i18n-lint.sh`, matching the repo's
  existing `spdx-lint.sh` / `dco-check.sh` pattern (bash + a fixture test),
  rather than a bespoke Go analyzer. It scans `web/src/**/*.tsx` — the layer
  where user-facing strings actually live — and fails on hardcoded JSX text
  nodes and user-facing attributes (`placeholder`, `title`, `aria-label`,
  `alt`) that don't go through `t()`.
- Escapes: a `i18n-ignore` line comment, and a small brand allowlist
  (`LastERP`). Go backend strings are developer-facing (wrapped errors, logs)
  and are deliberately **not** scanned to avoid overwhelming false positives;
  when server-rendered user strings appear (WP-1.7 document rendering) the gate
  can be extended.
- Wired into CI (`.github/workflows/ci.yml` lint job) and `make lint`; the
  gate's own pass/fail behaviour is covered by `scripts/i18n-lint_test.sh`
  (run from `make test`).
</content>
