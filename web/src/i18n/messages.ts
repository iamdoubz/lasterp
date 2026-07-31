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

  // Navigation shell
  "nav.label": "Main",
  "nav.signOut": "Sign out",
  "nav.skipToContent": "Skip to content",
  "nav.home": "Home",
  "nav.language": "Language",

  // Authentication
  "login.title": "Sign in",
  "login.tenant": "Tenant",
  "login.email": "Email",
  "login.password": "Password",
  "login.totp": "Authentication code",
  "login.totpHint": "Only required if two-factor authentication is enabled.",
  "login.submit": "Sign in",
  "login.pending": "Signing in…",
  "login.failed": "Sign-in failed. Check your details and try again.",

  // Generic object screens
  "object.list.title": "{object}",
  "object.list.empty": "Nothing here yet.",
  "object.list.new": "New {object}",
  "object.list.open": "Open",
  "object.detail.title": "{object} detail",
  "object.detail.edit": "Edit",
  "object.detail.back": "Back to list",
  "object.form.newTitle": "New {object}",
  "object.form.editTitle": "Edit {object}",
  "object.form.save": "Save",
  "object.form.saving": "Saving…",
  "object.form.cancel": "Cancel",
  "object.column.actions": "Actions",

  // Invoicing
  "invoice.title": "Invoice",
  "invoice.status": "Status",
  "invoice.number": "Number",
  "invoice.total": "Total",
  "invoice.post": "Post to ledger",
  "invoice.posting": "Posting…",
  "invoice.period": "Period",
  "invoice.pdf": "Download PDF",
  "invoice.posted": "Posted. Journal entry {entry} created.",
  "invoice.journalEntry": "Journal entry",

  // Schema labels. Objects and fields arrive from /api/v1/meta/objects with
  // machine names; these are what a person should read. A schema key that is
  // absent (an overlay's custom field, a module shipped after this bundle)
  // falls back to the humanized field name, so the renderer stays total —
  // see meta/render.tsx labelFor.
  "schema.object.Account": "Account",
  "schema.object.Contact": "Contact",
  "schema.field.Account.code": "Code",
  "schema.field.Account.name": "Name",
  "schema.field.Account.type": "Account type",
  "schema.field.Account.parent": "Parent account",
  "schema.field.Account.currency": "Currency",
  "schema.field.Contact.name": "Name",
  "schema.field.Contact.email": "Email",
  "schema.field.Contact.kind": "Kind",
  "schema.field.Contact.locale": "Language",

  // Dashboards (docs/21 role packs). Pack titles and metric labels arrive
  // from the server in English; these keys translate them, falling back to
  // the server's own label for anything a pack adds later.
  "nav.dashboard": "Dashboard",
  "dashboard.period": "Period {period}",
  "dashboard.comparison": "vs {period}",
  "dashboard.noComparison": "No earlier period to compare with",
  "dashboard.omitted": "{count, plural, one {# tile is hidden because you cannot access its data} other {# tiles are hidden because you cannot access their data}}",
  "dashboard.none": "No dashboard is available for your permissions yet.",
  "dashboard.needsPeriods": "Add a fiscal period and post something to see live figures here.",
  "dashboard.pack.ceo": "Executive overview",
  "dashboard.pack.cfo": "Finance overview",
  "dashboard.pack.ar": "Accounts receivable",
  "dashboard.pack.ap": "Accounts payable",
  "metric.revenue": "Revenue",
  "metric.expenses": "Expenses",
  "metric.net_income": "Net income",
  "metric.total_assets": "Total assets",
  "metric.total_liabilities": "Total liabilities",
  "metric.cash_position": "Cash position",
  "metric.ar_outstanding": "Accounts receivable outstanding",
  "metric.ar_overdue": "Accounts receivable overdue",
  "metric.open_invoice_count": "Open invoices",

  // Shared status
  "status.loading": "Loading…",
  "status.error": "Something went wrong.",
  "status.capabilityDisabled": "This module is not enabled for your organisation.",
} as const;

export type MessageKey = keyof typeof messages;

export type Catalog = Record<MessageKey, string>;
