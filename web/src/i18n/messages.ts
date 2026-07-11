// SPDX-License-Identifier: AGPL-3.0-only

// The English message catalog is the source of truth for user-facing strings.
// Every string rendered in the UI must have a key here and be looked up through
// useT()/t() — the hardcoded-string lint gate (scripts/i18n-lint.sh) enforces
// this. Values use the ICU MessageFormat subset understood by formatMessage:
// {arg} interpolation and {arg, plural, one {…} other {…}} / {arg, select, …}.

export const messages = {
  "app.title": "LastERP",
  "app.tagline": "The last ERP anyone will need to build — or buy.",
  "app.status.loading": "Loading…",
  "app.status.unreachable": "Kernel API unreachable",
  "app.items": "{count, plural, one {# item} other {# items}}",
} as const;

export type MessageKey = keyof typeof messages;

export type Catalog = Record<MessageKey, string>;
