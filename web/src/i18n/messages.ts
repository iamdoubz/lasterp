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

  // Shared status
  "status.loading": "Loading…",
  "status.error": "Something went wrong.",
  "status.capabilityDisabled": "This module is not enabled for your organisation.",
} as const;

export type MessageKey = keyof typeof messages;

export type Catalog = Record<MessageKey, string>;
